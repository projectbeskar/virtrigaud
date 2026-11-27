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
	"math"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// hostLedgerMirrorInterval is the least time between two refreshes of one
// Host's ledger mirrors (Host.status.agentLedger and agentLastStore).
const hostLedgerMirrorInterval = 30 * time.Second

// mirrorLedger refreshes, in host's status (written by the caller), the
// display mirrors of the host's last verified store position
// (agentLastStore) and of its ledger (agentLedger, only with a host ledger
// that verified or wrote the host's ledger in this process), at most every
// hostLedgerMirrorInterval per host; between refreshes the mirrors keep
// their values. The mirrors carry a store id prefix and counters only, and
// nothing reads them to decide. A sequence number above the int64 range of
// the status fields is never mirrored (that mirror is left out). With a host
// ledger it also sets the AgentLedgerHeld condition.
func (h *HostAnswers) mirrorLedger(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, host *infravirtrigaudiov1beta1.Host) {
	key := hostAnswerKey(provider, host.Name)
	now := h.now()
	h.mu.Lock()
	s := h.hosts[key]
	if s == nil || (!s.mirroredAt.IsZero() && now.Sub(s.mirroredAt) < hostLedgerMirrorInterval) {
		h.mu.Unlock()
		return
	}
	s.mirroredAt = now
	last := s.store.Current
	h.mu.Unlock()

	stamp := metav1.NewTime(now.UTC().Truncate(time.Second))
	fits := func(v uint64) bool { return v <= math.MaxInt64 }
	switch {
	case last.Empty():
		host.Status.AgentLastStore = nil
	case !fits(last.Seq) || !fits(last.CheckpointSeq):
		host.Status.AgentLastStore = nil
		log.FromContext(ctx).Info("The host's store position is above the status mirror's range; not mirrored", "host", host.Name)
	default:
		host.Status.AgentLastStore = &infravirtrigaudiov1beta1.HostAgentLastStoreStatus{StoreIDPrefix: storeIDPrefix(last.StoreID),
			Seq: int64(last.Seq), CheckpointSeq: int64(last.CheckpointSeq), UpdatedAt: &stamp} // #nosec G115 -- checked above
	}
	if h.Ledger == nil {
		return
	}
	m, ok := h.Ledger.mirrorOf(key)
	prev := meta.FindStatusCondition(host.Status.Conditions, HostConditionAgentLedgerHeld)
	switch reason := h.Ledger.HeldReason(provider, host.Name); {
	case reason != "":
		setHostCondition(host, HostConditionAgentLedgerHeld, metav1.ConditionTrue, reason,
			"the host's ledger holds the host; nothing new is signed for it from the ledger")
	case ok || (prev != nil && prev.Status == metav1.ConditionTrue):
		setHostCondition(host, HostConditionAgentLedgerHeld, metav1.ConditionFalse, reasonLedgerNotHeld, "the host's ledger holds nothing")
	}
	if !ok {
		return
	}
	if !fits(m.seq) || !fits(m.checkpoint) || !fits(m.generation) {
		host.Status.AgentLedger = nil
		log.FromContext(ctx).Info("The host's ledger is above the status mirror's range; not mirrored", "host", host.Name)
		return
	}
	l := &infravirtrigaudiov1beta1.HostAgentLedgerStatus{StoreIDPrefix: m.storePrefix, Seq: int64(m.seq), // #nosec G115 -- checked above
		CheckpointSeq: int64(m.checkpoint), AttestationState: m.state, LedgerGeneration: int64(m.generation), // #nosec G115 -- checked above
		UpdatedAt: &stamp}
	if !m.attestedAt.IsZero() {
		at := metav1.NewTime(m.attestedAt.UTC().Truncate(time.Second))
		l.AttestedAt = &at
	}
	host.Status.AgentLedger = l
}
