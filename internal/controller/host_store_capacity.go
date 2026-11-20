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
// refuses new operations), False below (reason StoreCapacityOK). Once True,
// it returns to False only when the store is below the recovery bound
// (storeRecoveryNum/storeRecoveryDen of the threshold, 70%): between that
// bound and three quarters it stays True/StoreNearlyFull (the agent's signed
// level is OK there; the hysteresis is the manager's, on the reason only).
//
// It is Unknown with reason StoreCapacityNotMeasured when the agent says it
// cannot measure its store, StoreCapacityNotReported when a verified answer
// from an older agent carries no report after one did, and
// StoreCapacityStale when a heartbeat got no verified answer while it was
// False (an old "OK" is never shown as current; a True is kept). It is
// absent until an agent reports its store capacity. Only a verified answer
// sets True or False (development builds). It is removed when the Host is no
// longer polled for, or cannot be checked with, signed answers
// (clearStorePressure): its Provider does not run per-host agents (or no
// longer requires operation identity), is gone, is not clustered or does not
// implement GetHostInfo; the manager has no host answer verification or the
// Provider's client cannot ask for signed answers; the manager holds no trust
// state for the host (no Host or no current enrolment: ErrNoEnrolment; a trust
// state that cannot be read is a failed heartbeat, markStorePressureStale);
// the host's store regressed; the Host is revoked or its agent certificate
// expired. An Unknown or absent condition resets the hysteresis: the next
// report is judged by the agent's level alone.
const HostConditionAgentStorePressure = "AgentStorePressure"

// Reasons of the AgentStorePressure condition. The two True reasons are also
// the reasons of the Warning event emitted when the severity rises to them.
const (
	reasonStoreCapacityOK          = "StoreCapacityOK"
	reasonStoreNearlyFull          = "StoreNearlyFull"
	reasonStoreFull                = "StoreFull"
	reasonStoreCapacityNotReported = "StoreCapacityNotReported"
	reasonStoreCapacityNotMeasured = "StoreCapacityNotMeasured"
	reasonStoreCapacityStale       = "StoreCapacityStale"
)

// The recovery bound of the condition's hysteresis: once StoreNearlyFull or
// StoreFull, the condition is False again only below
// storeRecoveryNum/storeRecoveryDen of the threshold, in integer arithmetic
// (threshold / 10 * 7).
const (
	storeRecoveryNum = 7
	storeRecoveryDen = 10
)

// storeSeverity orders the reasons for the Warning event: it is emitted only
// when the severity rises (OK, absent or Unknown to StoreNearlyFull,
// StoreNearlyFull to StoreFull, OK straight to StoreFull).
func storeSeverity(reason string) int {
	switch reason {
	case reasonStoreNearlyFull:
		return 1
	case reasonStoreFull:
		return 2
	}
	return 0
}

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
// of a verified store capacity c (non-nil), given the condition's previous
// reason (for the hysteresis).
func storeCapacityCondition(c *providerv1.StoreCapacity, prevReason string) (metav1.ConditionStatus, string, string) {
	if c.GetLevel() == providerv1.StoreLevel_STORE_LEVEL_NOT_MEASURED {
		return metav1.ConditionUnknown, reasonStoreCapacityNotMeasured, "the host's agent cannot measure its operation store " +
			"(it lost its persistence guarantee or is closed, or the agent's build does not measure it); the store's size is not known"
	}
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
	if storeSeverity(prevReason) > 0 && c.GetLiveBytes() >= c.GetThresholdBytes()/storeRecoveryDen*storeRecoveryNum {
		return metav1.ConditionTrue, reasonStoreNearlyFull, "the host's operation store is below three quarters of its size " +
			"threshold but not yet below the 70% recovery bound (" + sizes + "); " + storeCapacityRecovery
	}
	return metav1.ConditionFalse, reasonStoreCapacityOK, "the host's operation store is below three quarters of its size threshold (" +
		sizes + ")"
}

// applyStoreCapacity sets the AgentStorePressure condition from the store
// capacity c of a verified host answer (nil when the answer carried none),
// and emits a Warning event only when the severity rises to StoreNearlyFull
// or StoreFull (storeSeverity), never when it holds or falls. An answer
// without a capacity (an agent older than the report) never sets it to True
// or False: the condition stays absent when no capacity was ever reported,
// and goes to Unknown when one was.
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
				"the host's last verified answer reports no store capacity (its agent is older than the report); "+
					"the store's size is not known")
		}
		return
	}
	status, reason, msg := storeCapacityCondition(c, prevReason)
	setHostCondition(host, HostConditionAgentStorePressure, status, reason, msg)
	if storeSeverity(reason) <= storeSeverity(prevReason) || r.Recorder == nil {
		return
	}
	r.Recorder.Event(host, corev1.EventTypeWarning, reason, msg)
}

// markStorePressureStale is a heartbeat that got no verified answer from a
// host it still polls: a False AgentStorePressure becomes
// Unknown/StoreCapacityStale (no event), so an old "OK" is never presented as
// current; a True one is kept (the warning stands until a verified answer
// lifts it); any other is left as it is.
func markStorePressureStale(host *infravirtrigaudiov1beta1.Host) {
	c := meta.FindStatusCondition(host.Status.Conditions, HostConditionAgentStorePressure)
	if c == nil || c.Status != metav1.ConditionFalse {
		return
	}
	setHostCondition(host, HostConditionAgentStorePressure, metav1.ConditionUnknown, reasonStoreCapacityStale,
		"the last heartbeat got no verified answer from the host, so its last report (below three quarters of the store's "+
			"size threshold) is not current")
}

// clearStorePressure removes the AgentStorePressure condition of a Host that
// is no longer polled for, or cannot be checked with, signed answers, so a stale verdict (StoreFull, say)
// is never shown for a store nobody reads. A later verified report sets it
// again, with its event.
func clearStorePressure(host *infravirtrigaudiov1beta1.Host) {
	meta.RemoveStatusCondition(&host.Status.Conditions, HostConditionAgentStorePressure)
}
