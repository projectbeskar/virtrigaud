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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// capacityOf is a store capacity of live bytes against a 1000-byte threshold,
// with the level the contract's rule gives.
func capacityOf(live uint64) *providerv1.StoreCapacity {
	return &providerv1.StoreCapacity{LiveBytes: live, ThresholdBytes: 1000, Level: hostidentity.StoreLevelOf(live, 1000)}
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
// and the recovery in the message; a Warning event marks each transition
// into WARNING or FULL, and none is emitted while the level holds.
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
		{5000, metav1.ConditionTrue, reasonStoreFull, false}, // still FULL: no event
		{800, metav1.ConditionTrue, reasonStoreNearlyFull, true},
		{10, metav1.ConditionFalse, reasonStoreCapacityOK, false},
		{760, metav1.ConditionTrue, reasonStoreNearlyFull, true},
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
			assert.Empty(t, ev, "live %d: no event without a transition into WARNING or FULL", s.live)
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
// contradicts its sizes) leaves it as it was.
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
	} {
		t.Run(name, func(t *testing.T) {
			edit()
			f.clock.advance(time.Second)
			h := f.reconcile(t)
			requireCond(t, h, HostConditionAgentAnswer, metav1.ConditionFalse, reasonHostAnswerMissing)
			requireCond(t, h, HostConditionAgentStorePressure, metav1.ConditionTrue, reasonStoreFull)
			assert.Empty(t, storeEvents(f.events))
			f.signed.unsigned = false
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
		status, reason, msg := storeCapacityCondition(capacityOf(tc.live))
		assert.Equal(t, tc.status, status, "live %d", tc.live)
		assert.Equal(t, tc.reason, reason, "live %d", tc.live)
		assert.NotEmpty(t, msg)
	}
	_, _, msg := storeCapacityCondition(&providerv1.StoreCapacity{LiveBytes: 200 << 20, ThresholdBytes: 256 << 20,
		Level: providerv1.StoreLevel_STORE_LEVEL_WARNING})
	assert.Contains(t, msg, "200.0 MiB")
	assert.Contains(t, msg, "256.0 MiB")
	assert.Contains(t, msg, "78%")
}
