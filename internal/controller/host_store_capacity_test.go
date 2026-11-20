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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/devgate"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// capacityOf is a store capacity of live bytes against a 1000-byte threshold,
// with the level the contract's rule gives.
func capacityOf(live uint64) *providerv1.StoreCapacity {
	return &providerv1.StoreCapacity{LiveBytes: live, ThresholdBytes: 1000, Level: hostidentity.StoreLevelOf(live, 1000), RequestedVersion: 1, Version: 1}
}

// storeEvents returns the recorded events of the AgentStorePressure reasons.
func storeEvents(r *record.FakeRecorder) []string {
	var out []string
	for _, e := range drainEvents(r) {
		if strings.Contains(e, " "+reasonStoreNearlyFull+" ") || strings.Contains(e, " "+reasonStoreFull+" ") {
			out = append(out, e)
		}
	}
	return out
}

// The heartbeat asks for the store capacity, and each verified answer sets
// AgentStorePressure: False at OK, True at WARNING and FULL, with the sizes
// and the recovery in the message. Once True it is False again only below the
// 70% recovery bound. A Warning event marks each rise in severity, and none is
// emitted while it holds or falls.
func TestHostStoreCapacity_Transitions(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	live := uint64(100)
	f.signed.storeCapacity = func() *providerv1.StoreCapacity { return capacityOf(live) }

	h := f.reconcile(t)
	assert.EqualValues(t, hostidentity.StoreCapacityVersion, f.lastQuery(t).StoreCapacityVersion)
	c := requireCond(t, h, HostConditionAgentStorePressure, metav1.ConditionFalse, reasonStoreCapacityOK)
	assert.Contains(t, c.Message, "100 bytes")
	assert.Empty(t, storeEvents(f.events))

	steps := []struct {
		live   uint64
		status metav1.ConditionStatus
		reason string
		event  bool
	}{
		{749, metav1.ConditionFalse, reasonStoreCapacityOK, false},
		{750, metav1.ConditionTrue, reasonStoreNearlyFull, true},
		{900, metav1.ConditionTrue, reasonStoreNearlyFull, false}, // still WARNING: no event
		{1000, metav1.ConditionTrue, reasonStoreNearlyFull, false},
		{1001, metav1.ConditionTrue, reasonStoreFull, true},
		{5000, metav1.ConditionTrue, reasonStoreFull, false},       // still FULL: no event
		{800, metav1.ConditionTrue, reasonStoreNearlyFull, false},  // severity falls: no event
		{720, metav1.ConditionTrue, reasonStoreNearlyFull, false},  // agent OK, above the 70% bound: kept
		{700, metav1.ConditionTrue, reasonStoreNearlyFull, false},  // at the bound: kept
		{699, metav1.ConditionFalse, reasonStoreCapacityOK, false}, // below the bound
		{720, metav1.ConditionFalse, reasonStoreCapacityOK, false}, // no hysteresis on the way up
		{760, metav1.ConditionTrue, reasonStoreNearlyFull, true},
		{10, metav1.ConditionFalse, reasonStoreCapacityOK, false},
		{1001, metav1.ConditionTrue, reasonStoreFull, true}, // OK straight to FULL
	}
	for _, s := range steps {
		live = s.live
		f.clock.advance(time.Second)
		h = f.reconcile(t)
		c := requireCond(t, h, HostConditionAgentStorePressure, s.status, s.reason)
		assert.Contains(t, c.Message, "1000 bytes", "live %d", s.live)
		if s.status == metav1.ConditionTrue {
			assert.Contains(t, c.Message, "re-key", "live %d: the message names the recovery", s.live)
			assert.Contains(t, c.Message, storeRecoveryDoc)
		}
		ev := storeEvents(f.events)
		if s.event {
			require.Len(t, ev, 1, "live %d: one event on the transition", s.live)
			assert.True(t, strings.HasPrefix(ev[0], corev1.EventTypeWarning+" "+s.reason+" "), ev[0])
		} else {
			assert.Empty(t, ev, "live %d: no event without a rise in severity", s.live)
		}
	}
}

// An agent older than the report answers without a capacity: the condition is
// never set from such an answer while none was ever reported, and goes to
// Unknown, without an event, once a reported capacity stops being reported.
func TestHostStoreCapacity_OlderAgent(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	h := f.reconcile(t)
	requireCond(t, h, HostConditionAgentAnswer, metav1.ConditionTrue, reasonHostAnswerVerified)
	assert.Nil(t, meta.FindStatusCondition(h.Status.Conditions, HostConditionAgentStorePressure))

	f.signed.storeCapacity = func() *providerv1.StoreCapacity { return capacityOf(1001) }
	f.clock.advance(time.Second)
	h = f.reconcile(t)
	requireCond(t, h, HostConditionAgentStorePressure, metav1.ConditionTrue, reasonStoreFull)
	require.Len(t, storeEvents(f.events), 1)

	// The agent is rolled back to a version without the report.
	f.signed.storeCapacity = nil
	f.clock.advance(time.Second)
	h = f.reconcile(t)
	requireCond(t, h, HostConditionAgentAnswer, metav1.ConditionTrue, reasonHostAnswerVerified)
	requireCond(t, h, HostConditionAgentStorePressure, metav1.ConditionUnknown, reasonStoreCapacityNotReported)
	assert.Empty(t, storeEvents(f.events))
}

// Only a verified answer drives the condition: an answer that fails
// verification (unsigned, a capacity the request did not ask for, a level that
// contradicts its sizes, an echo that differs from the request, or a current
// agent's answer to a request stripped of store_capacity_version) leaves a
// True as it was.
func TestHostStoreCapacity_OnlyVerifiedAnswers(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	f.signed.storeCapacity = func() *providerv1.StoreCapacity { return capacityOf(1001) }
	h := f.reconcile(t)
	requireCond(t, h, HostConditionAgentStorePressure, metav1.ConditionTrue, reasonStoreFull)
	drainEvents(f.events)

	for name, edit := range map[string]func(){
		"unsigned": func() { f.signed.unsigned = true },
		"level contradicts the sizes": func() {
			f.signed.storeCapacity = func() *providerv1.StoreCapacity {
				return &providerv1.StoreCapacity{LiveBytes: 1001, ThresholdBytes: 1000, Level: providerv1.StoreLevel_STORE_LEVEL_OK}
			}
		},
		// The host saw no store_capacity_version and still put a capacity in
		// (echoing 0): a capacity the request, as the host got it, did not ask
		// for. The manager, which asked for version 1, refuses the echo.
		"capacity the request did not ask for": func() {
			f.signed.stripCapacityRequest, f.signed.capacityUnasked = true, true
		},
		// A relay strips store_capacity_version: a current agent answers as
		// asked for none; the manager, which asked, refuses the answer.
		"request stripped, current agent": func() {
			f.signed.stripCapacityRequest = true
			f.signed.apiVersion = hostidentity.StoreCapacityAgentAPIVersion
		},
	} {
		t.Run(name, func(t *testing.T) {
			edit()
			f.clock.advance(time.Second)
			h := f.reconcile(t)
			requireCond(t, h, HostConditionAgentAnswer, metav1.ConditionFalse, reasonHostAnswerMissing)
			requireCond(t, h, HostConditionAgentStorePressure, metav1.ConditionTrue, reasonStoreFull)
			assert.Empty(t, storeEvents(f.events))
			f.signed.unsigned, f.signed.stripCapacityRequest, f.signed.capacityUnasked, f.signed.apiVersion = false, false, false, 0
			f.signed.storeCapacity = func() *providerv1.StoreCapacity { return capacityOf(1001) }
		})
	}
}

// applyStoreCapacity without a capacity on a Host that never had one leaves
// the condition absent and emits nothing.
func TestHostStoreCapacity_NeverReportedStaysAbsent(t *testing.T) {
	events := record.NewFakeRecorder(4)
	h := &infravirtrigaudiov1beta1.Host{}
	(&HostReconciler{Recorder: events}).applyStoreCapacity(h, nil)
	assert.Empty(t, h.Status.Conditions)
	assert.Empty(t, drainEvents(events))
}

func TestStoreCapacityCondition(t *testing.T) {
	for _, tc := range []struct {
		live   uint64
		status metav1.ConditionStatus
		reason string
	}{
		{0, metav1.ConditionFalse, reasonStoreCapacityOK},
		{749, metav1.ConditionFalse, reasonStoreCapacityOK},
		{750, metav1.ConditionTrue, reasonStoreNearlyFull},
		{1000, metav1.ConditionTrue, reasonStoreNearlyFull},
		{1001, metav1.ConditionTrue, reasonStoreFull},
	} {
		status, reason, msg := storeCapacityCondition(capacityOf(tc.live), "")
		assert.Equal(t, tc.status, status, "live %d", tc.live)
		assert.Equal(t, tc.reason, reason, "live %d", tc.live)
		assert.NotEmpty(t, msg)
	}
	// The hysteresis: below three quarters, at or above 70%, after a True.
	for prev, want := range map[string]string{
		"": reasonStoreCapacityOK, reasonStoreCapacityOK: reasonStoreCapacityOK, reasonStoreCapacityStale: reasonStoreCapacityOK,
		reasonStoreNearlyFull: reasonStoreNearlyFull, reasonStoreFull: reasonStoreNearlyFull,
	} {
		_, reason, _ := storeCapacityCondition(capacityOf(700), prev)
		assert.Equal(t, want, reason, "700 after %q", prev)
		_, reason, _ = storeCapacityCondition(capacityOf(699), prev)
		assert.Equal(t, reasonStoreCapacityOK, reason, "699 after %q", prev)
	}
	status, reason, _ := storeCapacityCondition(&providerv1.StoreCapacity{Level: providerv1.StoreLevel_STORE_LEVEL_NOT_MEASURED}, reasonStoreFull)
	assert.Equal(t, metav1.ConditionUnknown, status)
	assert.Equal(t, reasonStoreCapacityNotMeasured, reason)
	_, _, msg := storeCapacityCondition(&providerv1.StoreCapacity{LiveBytes: 200 << 20, ThresholdBytes: 256 << 20,
		Level: providerv1.StoreLevel_STORE_LEVEL_WARNING}, "")
	assert.Contains(t, msg, "200.0 MiB")
	assert.Contains(t, msg, "256.0 MiB")
	assert.Contains(t, msg, "78%")
}

// storeFullCondition is a stale AgentStorePressure=True/StoreFull from an
// earlier heartbeat.
func storeFullCondition() metav1.Condition {
	return metav1.Condition{Type: HostConditionAgentStorePressure, Status: metav1.ConditionTrue, Reason: reasonStoreFull,
		Message: "an earlier answer", LastTransitionTime: metav1.Now()}
}

// A Host whose Provider stops requiring operation identity is no longer asked
// for signed answers: its AgentStorePressure is removed with the inventory
// conditions, not left at a stale StoreFull.
func TestHostStoreCapacity_ClearedWhenProviderStopsRequiringIdentity(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	f.signed.storeCapacity = func() *providerv1.StoreCapacity { return capacityOf(1001) }
	h := f.reconcile(t)
	requireCond(t, h, HostConditionAgentStorePressure, metav1.ConditionTrue, reasonStoreFull)

	p := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, f.r.Get(context.Background(), client.ObjectKeyFromObject(f.provider), p))
	p.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeLibvirt
	require.NoError(t, f.r.Update(context.Background(), p))
	f.clock.advance(time.Second)
	h = f.reconcile(t)
	assert.Nil(t, meta.FindStatusCondition(h.Status.Conditions, HostConditionAgentStorePressure))
	assert.Nil(t, meta.FindStatusCondition(h.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified))
}

// A heartbeat that fails to reach a host still polled keeps its last verdict
// (AgentAnswerVerified and the inventory conditions say the answer is
// missing); it is not a reason to drop the warning.
func TestHostStoreCapacity_KeptOnFailedHeartbeat(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	f.signed.storeCapacity = func() *providerv1.StoreCapacity { return capacityOf(1001) }
	h := f.reconcile(t)
	requireCond(t, h, HostConditionAgentStorePressure, metav1.ConditionTrue, reasonStoreFull)
	f.signed.hostErr = contracts.NewRetryableError("getHostInfo: connection refused", nil)
	f.clock.advance(time.Second)
	h = f.reconcile(t)
	requireCond(t, h, HostConditionAgentStorePressure, metav1.ConditionTrue, reasonStoreFull)
}

// A Host that is not polled (revoked, or its agent certificate expired), or
// whose Provider is gone or not clustered, has no AgentStorePressure: nobody
// reads its store.
func TestHostStoreCapacity_ClearedWhenNotPolled(t *testing.T) {
	cases := map[string]func(t *testing.T) (*HostReconciler, string){
		"provider gone": func(t *testing.T) (*HostReconciler, string) {
			host := hostCR("host-gone", "prov-missing", nil)
			host.Status.Conditions = []metav1.Condition{storeFullCondition()}
			return newHostReconciler(coverageTestScheme(t), &stubResolver{provider: &stubProvider{}}, host), "host-gone"
		},
		"provider not clustered": func(t *testing.T) (*HostReconciler, string) {
			prov := clusterProvider("prov-a")
			prov.Spec.Topology = ""
			host := hostCR("host-single", "prov-a", nil)
			host.Status.Conditions = []metav1.Condition{storeFullCondition()}
			return newHostReconciler(coverageTestScheme(t), &stubResolver{provider: &stubProvider{}}, prov, host), "host-single"
		},
	}
	if devgate.Compiled {
		cases["revoked"] = func(t *testing.T) (*HostReconciler, string) {
			prov := clusterProvider("prov-a")
			prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
			host := hostCR("host-rv", "prov-a", nil)
			host.Spec.Revoked = true
			host.Status.Conditions = []metav1.Condition{storeFullCondition()}
			return newHostReconciler(coverageTestScheme(t), &stubResolver{provider: &stubProvider{}}, prov, host), "host-rv"
		}
		cases["certificate expired"] = func(t *testing.T) (*HostReconciler, string) {
			prov := clusterProvider("prov-a")
			prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
			host := agentHostWithExpiry("host-np", time.Now().Add(-time.Minute))
			host.Status.Conditions = []metav1.Condition{storeFullCondition()}
			return newHostReconciler(coverageTestScheme(t), &stubResolver{provider: &stubProvider{}}, prov, host), "host-np"
		}
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			r, hostName := setup(t)
			_, err := r.Reconcile(context.Background(), hostReq(hostName))
			require.NoError(t, err)
			got := getHost(t, r.Client, hostName)
			assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, HostConditionAgentStorePressure))
		})
	}
}

// A heartbeat that gets no verified answer from a host it still polls turns a
// False AgentStorePressure into Unknown/StoreCapacityStale, without an event,
// and keeps a True; the next verified answer sets it again.
func TestHostStoreCapacity_StaleOK(t *testing.T) {
	for name, fail := range map[string]func(f *invFixture){
		"GetHostInfo failed": func(f *invFixture) {
			f.signed.hostErr = contracts.NewRetryableError("getHostInfo: connection refused", nil)
		},
		"answer not verified": func(f *invFixture) { f.signed.unsigned = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newInvFixture(t, testPolicy(t))
			f.signed.storeCapacity = func() *providerv1.StoreCapacity { return capacityOf(10) }
			h := f.reconcile(t)
			requireCond(t, h, HostConditionAgentStorePressure, metav1.ConditionFalse, reasonStoreCapacityOK)
			fail(f)
			f.clock.advance(time.Second)
			h = f.reconcile(t)
			requireCond(t, h, HostConditionAgentStorePressure, metav1.ConditionUnknown, reasonStoreCapacityStale)
			f.signed.hostErr, f.signed.unsigned = nil, false
			f.clock.advance(time.Second)
			h = f.reconcile(t)
			requireCond(t, h, HostConditionAgentStorePressure, metav1.ConditionFalse, reasonStoreCapacityOK)
			assert.Empty(t, storeEvents(f.events))
		})
	}
}

// An agent that cannot measure its store says so: Unknown/StoreCapacityNotMeasured,
// without an event.
func TestHostStoreCapacity_NotMeasured(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	f.signed.storeCapacity = func() *providerv1.StoreCapacity { return capacityOf(1001) }
	h := f.reconcile(t)
	requireCond(t, h, HostConditionAgentStorePressure, metav1.ConditionTrue, reasonStoreFull)
	drainEvents(f.events)
	f.signed.storeCapacity = func() *providerv1.StoreCapacity {
		return &providerv1.StoreCapacity{Level: providerv1.StoreLevel_STORE_LEVEL_NOT_MEASURED}
	}
	f.clock.advance(time.Second)
	h = f.reconcile(t)
	requireCond(t, h, HostConditionAgentStorePressure, metav1.ConditionUnknown, reasonStoreCapacityNotMeasured)
	assert.Empty(t, storeEvents(f.events))
}

// The heartbeat paths that cannot check the host with signed answers remove
// AgentStorePressure: no host answer verification on the manager, a Provider
// client that cannot ask for signed answers, no trust state for the host, a
// regressed store, and a Provider that does not implement GetHostInfo.
func TestHostStoreCapacity_ClearedWhenUnverifiable(t *testing.T) {
	for name, edit := range map[string]func(t *testing.T, f *invFixture){
		"no host answer verification": func(_ *testing.T, f *invFixture) { f.r.HostAnswers = nil },
		"client cannot ask for signed answers": func(_ *testing.T, f *invFixture) {
			f.r.RemoteResolver = &stubResolver{provider: &stubProvider{GetHostInfoFn: func(_ context.Context, id string) (contracts.HostInfo, error) {
				return healthyHostInfo(id), nil
			}}}
		},
		"no trust state": func(_ *testing.T, f *invFixture) {
			f.r.HostAnswers.Trust = &staticTrust{hosts: map[string]*testHost{}}
		},
		"store regressed": func(t *testing.T, f *invFixture) {
			for range 4 {
				f.clock.advance(time.Second)
				f.reconcile(t)
			}
			f.signed.host.rollBack(3)
		},
		"GetHostInfo not implemented": func(_ *testing.T, f *invFixture) {
			f.signed.hostErr = contracts.NewNotSupportedError("getHostInfo: not a clustered provider")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newInvFixture(t, testPolicy(t))
			f.signed.storeCapacity = func() *providerv1.StoreCapacity { return capacityOf(1001) }
			h := f.reconcile(t)
			requireCond(t, h, HostConditionAgentStorePressure, metav1.ConditionTrue, reasonStoreFull)
			edit(t, f)
			f.clock.advance(time.Second)
			h = f.reconcile(t)
			assert.Nil(t, meta.FindStatusCondition(h.Status.Conditions, HostConditionAgentStorePressure))
		})
	}
}
