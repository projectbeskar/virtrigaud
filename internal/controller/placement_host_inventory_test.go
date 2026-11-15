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
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// placeOn runs resolveClusterPlacement for a fresh VM of providerCR against
// hosts and returns the chosen host ("" when none).
func placeOn(t *testing.T, providerCR *infravirtrigaudiov1beta1.Provider, answers *HostAnswers, hosts ...*infravirtrigaudiov1beta1.Host) string {
	t.Helper()
	objs := capBase()
	objs[0] = providerCR
	for _, h := range hosts {
		objs = append(objs, h)
	}
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &stubProvider{}}, objs...)
	r.HostAnswers = answers
	vm := capVM("vm-a")
	p, _, err := r.resolveClusterPlacement(context.Background(), vm, providerCR, capCreateReq(), nil)
	require.NoError(t, err)
	if p == nil {
		return ""
	}
	return p.hostID
}

// The scheduler skips a host of a Provider whose type runs a per-host agent
// when its inventory made it NotReady, or when its last sync is older than
// the maximum inventory age; a libvirt Provider's hosts are unaffected.
func TestPlacement_AgentProviderSkipsNotReadyAndStaleHosts(t *testing.T) {
	ch := clusteredProviderCR("prov-cluster", capNS)
	ch.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
	lv := clusteredProviderCR("prov-cluster", capNS)

	big := func(name string) *infravirtrigaudiov1beta1.Host { return capHost(name, 64) }
	notReady := big("host-not-ready")
	notReady.Status.Health = infravirtrigaudiov1beta1.HostHealthNotReady
	stale := big("host-stale")
	old := metav1.NewTime(time.Now().Add(-DefaultHostInventoryMaxAge - time.Minute))
	stale.Status.LastHeartbeatTime = &old
	small := capHost("host-small", 4)

	assert.Equal(t, "host-small", placeOn(t, ch, nil, notReady, stale, small),
		"only the ready host with a recent sync is a candidate")
	assert.Empty(t, placeOn(t, ch, nil, notReady, stale), "no candidate: not placed")

	// The maximum age comes from the policy.
	longer := DefaultHostInventoryPolicy()
	longer.MaxAge = time.Hour
	assert.Equal(t, "host-stale", placeOn(t, ch, &HostAnswers{Inventory: longer}, stale, small))

	// libvirt: the stale host is a candidate as before (and wins on size).
	never := big("host-never-synced")
	never.Status.LastHeartbeatTime = nil
	assert.Equal(t, "host-stale", placeOn(t, lv, nil, stale, small))
	assert.Equal(t, "host-never-synced", placeOn(t, lv, nil, never, small))
}

// A host flipping to NotReady (and back) never re-places, re-binds or draws a
// new binding for a VM with an open identity Create attempt on it: the
// attempt keeps being asked about by op_id on that host.
func TestPlacement_FlipNeverReplacesAnOpenAttempt(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{ID: "vm-1", TaskRef: "task-1"}, nil
	}
	stub.onTask = func(string) (contracts.TaskStatus, error) {
		return contracts.TaskStatus{Outcome: contracts.TaskOutcomeUnknown}, nil
	}
	// A second ready host the VM could be moved to.
	other := readyHost("host-beta", clusteredNS, "pool-a", "prov-cluster")
	other.Status.AllocatableCPU = i32p(4) // smaller: the first placement picks host-alpha
	require.NoError(t, r.Create(context.Background(), other))

	reconcileN(t, r, req, 3)
	vm := idVM(t, r)
	first := findAttempt(vm, contracts.OperationKindCreate)
	require.NotNil(t, first, "the Create attempt is open")
	require.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseSent, first.Phase)
	require.NotNil(t, vm.Status.Placement)
	require.Equal(t, "host-alpha", vm.Status.Placement.PendingHost)
	require.Len(t, stub.sent(), 1)

	setHealth := func(h infravirtrigaudiov1beta1.HostHealth) {
		host := &infravirtrigaudiov1beta1.Host{}
		require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: clusteredNS, Name: "host-alpha"}, host))
		host.Status.Health = h
		require.NoError(t, r.Update(context.Background(), host))
	}
	for i := 0; i < 4; i++ {
		setHealth(infravirtrigaudiov1beta1.HostHealthNotReady)
		reconcileN(t, r, req, 2)
		setHealth(infravirtrigaudiov1beta1.HostHealthReady)
		reconcileN(t, r, req, 1)
		vm = idVM(t, r)
		a := findAttempt(vm, contracts.OperationKindCreate)
		require.NotNil(t, a)
		assert.Equal(t, first.OpID, a.OpID, "the same attempt")
		assert.Equal(t, first.BindingID, a.BindingID, "no new binding")
		assert.Equal(t, "host-alpha", a.HostID)
		assert.Equal(t, "host-alpha", vm.Status.Placement.PendingHost, "never re-placed")
		assert.Empty(t, vm.Status.Placement.Host, "never bound elsewhere")
	}
	assert.Len(t, stub.sent(), 1, "nothing new is sent")
	stub.signed.amu.Lock()
	queries := append([]contracts.OperationAnswerQuery(nil), stub.signed.opQueries...)
	stub.signed.amu.Unlock()
	require.NotEmpty(t, queries)
	for _, q := range queries {
		assert.Equal(t, "host-alpha", q.HostID, "the attempt is asked about on its host")
		assert.Equal(t, first.OpID, hex.EncodeToString(q.OpID), "by op_id")
	}
}
