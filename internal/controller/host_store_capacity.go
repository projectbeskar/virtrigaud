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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// HostConditionAgentStorePressure is True while the host's last verified
// answer reported its operation store at or above three quarters of its size
// threshold (reason StoreNearlyFull) or above it (reason StoreFull: the agent
// refuses new operations), False below three quarters (reason
// StoreCapacityOK). It is absent until an agent reports its store capacity
// (an agent older than the report never does), and Unknown (reason
// StoreCapacityNotReported) when a later verified answer carries no report
// (an older agent, or a store the agent cannot measure, such as a lost one).
// Only a verified answer sets it (development builds). It is removed when the
// Host is no longer polled for signed answers: its Provider does not run
// per-host agents (or no longer requires operation identity), is gone or is
// not clustered, or the Host is revoked or its agent certificate expired
// (clearStorePressure). A heartbeat that only failed to reach a polled host
// keeps it, beside AgentAnswerVerified=False.
const HostConditionAgentStorePressure = "AgentStorePressure"

// Reasons of the AgentStorePressure condition. The two True reasons are also
// the reasons of the Warning event emitted on the transition into them.
const (
	reasonStoreCapacityOK          = "StoreCapacityOK"
	reasonStoreNearlyFull          = "StoreNearlyFull"
	reasonStoreFull                = "StoreFull"
	reasonStoreCapacityNotReported = "StoreCapacityNotReported"
)

// storeRecoveryDoc is where the store's recovery is documented.
const storeRecoveryDoc = "docs/per-host-agent-development.md#store-size"

// storeCapacityRecovery is the recovery an AgentStorePressure message names.
const storeCapacityRecovery = "compaction never drops op_id tombstones, VM tombstones or the records of unresolved " +
	"operations; resolve unresolved operations, then re-key the Host so that the store discards its predecessor " +
	"enrolment's tombstones once the freeze is confirmed; VM tombstones stay across re-keys: a store they fill is " +
	"recovered only once no VM is left on the host, with a fresh agent state and a re-key with a new key (see " +
	storeRecoveryDoc + ")"

// mib formats a byte count in MiB with one decimal.
func mib(b uint64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }

// storeCapacityCondition is the AgentStorePressure status, reason and message
// of a verified store capacity c (non-nil).
func storeCapacityCondition(c *providerv1.StoreCapacity) (metav1.ConditionStatus, string, string) {
	sizes := fmt.Sprintf("live size %s (%d bytes) of a %s (%d bytes) threshold, %.0f%%", mib(c.GetLiveBytes()),
		c.GetLiveBytes(), mib(c.GetThresholdBytes()), c.GetThresholdBytes(),
		100*float64(c.GetLiveBytes())/float64(c.GetThresholdBytes()))
	switch c.GetLevel() {
	case providerv1.StoreLevel_STORE_LEVEL_FULL:
		return metav1.ConditionTrue, reasonStoreFull, "the host's operation store is above its size threshold (" + sizes +
			"): the agent refuses new operations (STORE_FULL, definitely not performed); " + storeCapacityRecovery
	case providerv1.StoreLevel_STORE_LEVEL_WARNING:
		return metav1.ConditionTrue, reasonStoreNearlyFull, "the host's operation store is at or above three quarters of " +
			"its size threshold (" + sizes + "); above the threshold the agent refuses new operations; " + storeCapacityRecovery
	}
	return metav1.ConditionFalse, reasonStoreCapacityOK, "the host's operation store is below three quarters of its size threshold (" +
		sizes + ")"
}

// applyStoreCapacity sets the AgentStorePressure condition from the store
// capacity c of a verified host answer (nil when the answer carried none),
// and emits a Warning event when the condition moves into StoreNearlyFull or
// StoreFull, never on an answer that leaves it unchanged. An answer without a
// capacity (an agent older than the report) never sets it to True or False:
// the condition stays absent when no capacity was ever reported, and goes to
// Unknown when one was.
func (r *HostReconciler) applyStoreCapacity(host *infravirtrigaudiov1beta1.Host, c *providerv1.StoreCapacity) {
	// The previous reason is copied: setting the condition updates it in
	// place.
	prev := meta.FindStatusCondition(host.Status.Conditions, HostConditionAgentStorePressure)
	prevReason := ""
	if prev != nil {
		prevReason = prev.Reason
	}
	if c == nil {
		if prev != nil {
			setHostCondition(host, HostConditionAgentStorePressure, metav1.ConditionUnknown, reasonStoreCapacityNotReported,
				"the host's last verified answer reports no store capacity (its agent is older than the report, or "+
					"cannot measure its store); the store's size is not known")
		}
		return
	}
	status, reason, msg := storeCapacityCondition(c)
	setHostCondition(host, HostConditionAgentStorePressure, status, reason, msg)
	if status != metav1.ConditionTrue || prevReason == reason || r.Recorder == nil {
		return
	}
	r.Recorder.Event(host, corev1.EventTypeWarning, reason, msg)
}

// clearStorePressure removes the AgentStorePressure condition of a Host that
// is no longer polled for signed answers, so a stale verdict (StoreFull, say)
// is never shown for a store nobody reads. A later verified report sets it
// again, with its event.
func clearStorePressure(host *infravirtrigaudiov1beta1.Host) {
	meta.RemoveStatusCondition(&host.Status.Conditions, HostConditionAgentStorePressure)
}
