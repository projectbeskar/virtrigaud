/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/devgate"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// notFrontedErr is a clustered Provider's answer for a host it does not
// front, wrapped as the manager's transport wraps it.
func notFrontedErr(t *testing.T) error {
	t.Helper()
	st, err := status.New(codes.Unavailable, "the host is not fronted by this Provider").WithDetails(&errdetails.ErrorInfo{
		Reason: contracts.HostUnavailableReason, Domain: contracts.HostUnavailableErrorDomain,
		Metadata: map[string]string{contracts.HostUnavailableCauseKey: contracts.HostUnavailableCauseNotFronted}})
	require.NoError(t, err)
	return contracts.NewHostUnavailableError("getHostInfo", st.Err())
}

// lineCounter counts the log lines that contain a substring.
type lineCounter struct {
	mu    sync.Mutex
	lines []string
}

func (c *lineCounter) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.lines = append(c.lines, prefix+" "+args)
	}, funcr.Options{})
}

func (c *lineCounter) count(sub string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, l := range c.lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func TestHostUnavailableCause(t *testing.T) {
	assert.Equal(t, contracts.HostUnavailableCauseNotFronted, contracts.HostUnavailableCause(notFrontedErr(t)))
	assert.Empty(t, contracts.HostUnavailableCause(contracts.NewRetryableError("down", nil)))
	assert.Empty(t, contracts.HostUnavailableCause(status.Error(codes.Unavailable, "no details")))
}

// A per-host agent Host its Provider does not front yet (no pinned
// certificate) is polled with an exponential backoff up to the heartbeat,
// logged at ERROR on the first miss and then at most once per heartbeat
// interval; a successful poll ends the streak.
func TestHostReconciler_NotFrontedBacksOff(t *testing.T) {
	if !devgate.Compiled {
		t.Skip("per-host agents run in development builds only")
	}
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-a")
	prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
	host := hostCR("host-nf", "prov-a", nil)
	fronted := false
	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, id string) (contracts.HostInfo, error) {
		if fronted {
			return healthyHostInfo(id), nil
		}
		return contracts.HostInfo{}, notFrontedErr(t)
	}}
	r := newHostReconciler(s, &stubResolver{provider: stub}, prov, host)
	var logs lineCounter
	ctx := ctrllog.IntoContext(context.Background(), logs.logger())
	const errLine = "the host is not fronted by its Provider yet"

	var waits []time.Duration
	for range 5 {
		res, err := r.Reconcile(ctx, hostReq("host-nf"))
		require.NoError(t, err)
		waits = append(waits, res.RequeueAfter)
	}
	assert.Equal(t, []time.Duration{hostSyncBackoffInterval, 2 * hostSyncBackoffInterval, hostHeartbeatInterval,
		hostHeartbeatInterval, hostHeartbeatInterval}, waits)
	assert.Equal(t, 1, logs.count(errLine), "ERROR on the first miss only, within a heartbeat interval")
	ready := hostReadyCondition(t, getHost(t, r.Client, "host-nf"))
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, reasonProviderUnavailable, ready.Reason)

	// A heartbeat interval later, the next miss is logged at ERROR again.
	key := types.NamespacedName{Namespace: "default", Name: "host-nf"}
	v, ok := r.notFronted.Load(key)
	require.True(t, ok)
	v.(*notFrontedStreak).lastError = time.Now().Add(-hostHeartbeatInterval)
	_, err := r.Reconcile(ctx, hostReq("host-nf"))
	require.NoError(t, err)
	assert.Equal(t, 2, logs.count(errLine))

	// Fronted: polled normally, the streak ends.
	fronted = true
	res, err := r.Reconcile(ctx, hostReq("host-nf"))
	require.NoError(t, err)
	assert.Equal(t, hostHeartbeatInterval, res.RequeueAfter)
	_, ok = r.notFronted.Load(key)
	assert.False(t, ok, "a successful poll ends the streak")
	fronted = false
	res, err = r.Reconcile(ctx, hostReq("host-nf"))
	require.NoError(t, err)
	assert.Equal(t, hostSyncBackoffInterval, res.RequeueAfter, "a new streak starts from the shortest wait")
	assert.Equal(t, 3, logs.count(errLine), "and logs its first miss at ERROR")
}

// Any other GetHostInfo failure keeps the short backoff and its ERROR line.
func TestHostReconciler_OtherFailuresKeepTheShortBackoff(t *testing.T) {
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-a")
	prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
	host := hostCR("host-down", "prov-a", nil)
	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, _ string) (contracts.HostInfo, error) {
		return contracts.HostInfo{}, contracts.NewRetryableError("down", nil)
	}}
	r := newHostReconciler(s, &stubResolver{provider: stub}, prov, host)
	var logs lineCounter
	ctx := ctrllog.IntoContext(context.Background(), logs.logger())
	for range 3 {
		res, err := r.Reconcile(ctx, hostReq("host-down"))
		require.NoError(t, err)
		assert.Equal(t, hostSyncBackoffInterval, res.RequeueAfter)
	}
	assert.Equal(t, 3, logs.count("GetHostInfo failed"))
}

// A Host with a pinned certificate its Provider has not applied yet is
// retried at the short backoff.
func TestNotFrontedBackoff(t *testing.T) {
	assert.Equal(t, hostSyncBackoffInterval, notFrontedBackoff(1, false))
	assert.Equal(t, 2*hostSyncBackoffInterval, notFrontedBackoff(2, false))
	assert.Equal(t, hostHeartbeatInterval, notFrontedBackoff(3, false))
	assert.Equal(t, hostHeartbeatInterval, notFrontedBackoff(40, false))
	assert.Equal(t, hostSyncBackoffInterval, notFrontedBackoff(5, true))
}

// While a renewed certificate is issued and not pinned, the Host is polled
// again within seconds, not a heartbeat later: the Provider stages it on its
// next call to the host and the manager pins it on the host's next answer.
func TestHostReconciler_PollsSoonWhileARenewalAwaitsStaging(t *testing.T) {
	if !devgate.Compiled {
		t.Skip("per-host agents run in development builds only")
	}
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-a")
	prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
	host := hostCR("host-ren", "prov-a", nil)
	host.UID = "host-ren-uid"
	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, id string) (contracts.HostInfo, error) {
		return healthyHostInfo(id), nil
	}}
	r := newHostReconciler(s, &stubResolver{provider: stub}, prov, host)
	cfg := &HostEnrolmentConfig{Namespace: "host-agent-pki", TrustDomain: "cluster.test"}
	r.HostAnswers = NewHostAnswers(r.Client, r.Client, cfg, bytes16(0x42))
	setRenewal := func(f func(*renewalState)) {
		t.Helper()
		h := getHost(t, r.Client, "host-ren")
		l, err := loadHostEnrolment(context.Background(), r.Client, cfg, h)
		require.NoError(t, err)
		l.state.Current = enrolmentFor(strings.Repeat("c3", 16), "uid-prov-a", 2, strings.Repeat("aa", 32))
		l.state.Current.NotAfter = metav1.NewTime(time.Now().Add(24 * time.Hour))
		l.state.Renewal = nil
		if f != nil {
			l.state.Renewal = &renewalState{KeyPin: strings.Repeat("bb", 32), Since: metav1.Now()}
			f(l.state.Renewal)
		}
		require.NoError(t, saveHostEnrolment(context.Background(), r.Client, cfg, h, &l))
	}
	reconcile := func() time.Duration {
		t.Helper()
		res, err := r.Reconcile(context.Background(), hostReq("host-ren"))
		require.NoError(t, err)
		return res.RequeueAfter
	}
	setRenewal(nil)
	assert.Equal(t, hostHeartbeatInterval, reconcile(), "no renewal: the heartbeat")
	setRenewal(func(*renewalState) {})
	assert.Equal(t, hostHeartbeatInterval, reconcile(), "a renewal not issued yet: the heartbeat")
	setRenewal(func(ren *renewalState) { ren.Certificate = []byte("der") })
	assert.Equal(t, renewalStagePoll, reconcile(), "issued, not pinned: soon")
	setRenewal(func(ren *renewalState) {
		ren.Certificate = []byte("der")
		at := metav1.Now()
		ren.PinnedAt = &at
	})
	assert.Equal(t, hostHeartbeatInterval, reconcile(), "pinned: the heartbeat")
}
