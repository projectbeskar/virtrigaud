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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Freeze confirmation of predecessor enrolments (development builds only).
//
// After a re-key, the replaced enrolment stays listed as a predecessor so
// that attempts sent under it are still asked about and their VMs held. The
// manager confirms its freeze once no VM has an open attempt on the Host,
// and at least predecessorQuietPeriod after the re-key; the confirmation is
// rendered into the Provider's host inventory, the Provider relays it to the
// agent, and the agent discards the predecessor's records. The predecessor
// is removed from the pins Secret only once a signed answer of the host
// shows it discarded (HostAnswers.checkFreezeDiscards).

// predecessorQuietPeriod is how long after a re-key a predecessor is never
// confirmed, so that an attempt minted under it just before the re-key, whose
// marker may not be visible yet, is seen by the check.
const predecessorQuietPeriod = 2 * time.Minute

// freezeRecheckInterval is how often a Host with an unconfirmed predecessor is
// checked again (open attempts end without an event on the Host).
const freezeRecheckInterval = time.Minute

// Event reasons of freeze confirmation.
const (
	eventReasonAgentFreezeConfirmed      = "AgentFreezeConfirmed"
	eventReasonAgentPredecessorDiscarded = "AgentPredecessorDiscarded"
)

// openAttemptsOnHost counts the attempts that are not Terminal, on any
// VirtualMachine, that name host (its id) and belong to host's Provider. The
// list is read from the API server (not a cache), so an attempt written just
// before is seen. An attempt does not name its enrolment, so every open
// attempt on the Host counts: the check is a superset of "an attempt under the
// predecessor".
func openAttemptsOnHost(ctx context.Context, r *HostEnrolmentReconciler, host *infravirtrigaudiov1beta1.Host) (int, error) {
	vms := &infravirtrigaudiov1beta1.VirtualMachineList{}
	if err := r.liveReader().List(ctx, vms); err != nil {
		return 0, fmt.Errorf("list VirtualMachines for Host %s/%s: %w", host.Namespace, host.Name, err)
	}
	n := 0
	for i := range vms.Items {
		vm := &vms.Items[i]
		if !vmOfProvider(vm, host.Namespace, host.Spec.ProviderRef.Name) {
			continue
		}
		for _, a := range vm.Status.OperationAttempts {
			if a.HostID == host.Name && a.Phase != infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal {
				n++
			}
		}
	}
	return n, nil
}

// vmOfProvider reports whether vm names, or is bound to, the Provider
// namespace/name.
func vmOfProvider(vm *infravirtrigaudiov1beta1.VirtualMachine, namespace, name string) bool {
	ns := vm.Spec.ProviderRef.Namespace
	if ns == "" {
		ns = vm.Namespace
	}
	if ns == namespace && vm.Spec.ProviderRef.Name == name {
		return true
	}
	b := vm.Status.BoundProvider
	return b != nil && b.Namespace == namespace && b.Name == name
}

// confirmFreezes confirms the freeze of the Host's predecessors that are past
// their quiet period, once no open attempt names the Host. It reports whether
// the state changed and whether a predecessor is still waiting (to requeue).
func (r *HostEnrolmentReconciler) confirmFreezes(ctx context.Context, host *infravirtrigaudiov1beta1.Host,
	st *hostEnrolmentState) (changed, waiting bool, err error) {
	if st.Current == nil || host.Spec.Revoked {
		return false, false, nil
	}
	now := r.now()
	var due []string
	for _, p := range st.PredecessorEnrolmentIDs {
		if slices.Contains(st.ConfirmedFrozenEnrolmentIDs, p) {
			continue
		}
		since, ok := st.PredecessorSince[p]
		if ok && now.Sub(since.Time) < predecessorQuietPeriod {
			waiting = true
			continue
		}
		due = append(due, p)
	}
	if len(due) == 0 {
		return false, waiting, nil
	}
	open, err := openAttemptsOnHost(ctx, r, host)
	if err != nil {
		return false, true, err
	}
	if open > 0 {
		log.FromContext(ctx).V(1).Info("Predecessor freeze not confirmed yet: VMs have open attempts on the host",
			"host", host.Namespace+"/"+host.Name, "openAttempts", open, "predecessors", predecessorSummary(st))
		return false, true, nil
	}
	for _, p := range due {
		if len(st.ConfirmedFrozenEnrolmentIDs) >= hostsecret.MaxFreezeConfirmations {
			waiting = true
			break
		}
		st.ConfirmedFrozenEnrolmentIDs = append(st.ConfirmedFrozenEnrolmentIDs, p)
		changed = true
		log.FromContext(ctx).Info("Confirmed the freeze of a predecessor enrolment: no open attempt names the host",
			"host", host.Namespace+"/"+host.Name, "enrolmentID", p)
		r.event(host, corev1.EventTypeNormal, eventReasonAgentFreezeConfirmed, "the freeze of predecessor enrolment "+p+
			" is confirmed (no open operation on the host); the host is asked to discard its records")
	}
	return changed, waiting, nil
}

// freezeConfirmationOf returns the freeze confirmation to render for the
// Host's agent: the confirmed predecessors still listed, for its current
// enrolment; nil when there is none.
func freezeConfirmationOf(st *hostEnrolmentState) *hostsecret.FreezeConfirmation {
	if st.Current == nil {
		return nil
	}
	var ids []string
	for _, p := range st.ConfirmedFrozenEnrolmentIDs {
		if slices.Contains(st.PredecessorEnrolmentIDs, p) && p != st.Current.Record.EnrolmentID {
			ids = append(ids, p)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > hostsecret.MaxFreezeConfirmations {
		ids = ids[:hostsecret.MaxFreezeConfirmations]
	}
	return &hostsecret.FreezeConfirmation{ForEnrolmentID: st.Current.Record.EnrolmentID, EnrolmentIDs: ids}
}

// discardPredecessor removes predecessor p from the state once the host has
// shown, by a verified signed answer, that it discarded p's records; it
// reports whether p was listed.
func (st *hostEnrolmentState) discardPredecessor(p string) bool {
	if !slices.Contains(st.PredecessorEnrolmentIDs, p) {
		return false
	}
	st.PredecessorEnrolmentIDs = slices.DeleteFunc(slices.Clone(st.PredecessorEnrolmentIDs), func(s string) bool { return s == p })
	st.ConfirmedFrozenEnrolmentIDs = slices.DeleteFunc(slices.Clone(st.ConfirmedFrozenEnrolmentIDs), func(s string) bool { return s == p })
	delete(st.PredecessorSince, p)
	return true
}

// predecessorSummary is the operators' view of a Host's predecessors, for a
// log line: "id (since <time>, confirmed)".
func predecessorSummary(st *hostEnrolmentState) string {
	parts := make([]string, 0, len(st.PredecessorEnrolmentIDs))
	for _, p := range st.PredecessorEnrolmentIDs {
		s := p
		if t, ok := st.PredecessorSince[p]; ok {
			s += " (since " + t.UTC().Format(time.RFC3339)
		} else {
			s += " ("
		}
		if slices.Contains(st.ConfirmedFrozenEnrolmentIDs, p) {
			s += ", confirmed"
		}
		parts = append(parts, s+")")
	}
	return strings.Join(parts, "; ")
}

// predecessorStamp returns the time a predecessor is recorded at.
func predecessorStamp(now time.Time) metav1.Time {
	return metav1.NewTime(now.UTC().Truncate(time.Second))
}

// freezeTrust is the part of a HostTrustSource that holds the freeze
// confirmations of a Host's predecessors (PinsHostTrust).
type freezeTrust interface {
	// ConfirmedFrozen returns the predecessors whose freeze the manager
	// confirmed and that are still listed.
	ConfirmedFrozen(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string) ([][]byte, error)
	// DiscardPredecessor removes predecessor id from the Host's state (with
	// the read resourceVersion as precondition); it reports whether it was
	// listed.
	DiscardPredecessor(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, id []byte) (bool, error)
}

// ConfirmedFrozen implements freezeTrust.
func (p *PinsHostTrust) ConfirmedFrozen(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string) ([][]byte, error) {
	h, err := p.host(ctx, provider, hostID)
	if err != nil {
		return nil, err
	}
	l, err := loadHostEnrolment(ctx, p.secretReader(), p.Config, h)
	if err != nil || l.reset {
		return nil, err
	}
	var out [][]byte
	for _, s := range l.state.ConfirmedFrozenEnrolmentIDs {
		if !slices.Contains(l.state.PredecessorEnrolmentIDs, s) {
			continue
		}
		if id, err := hostidentity.DecodeID(s); err == nil {
			out = append(out, id)
		}
	}
	return out, nil
}

// DiscardPredecessor implements freezeTrust.
func (p *PinsHostTrust) DiscardPredecessor(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, id []byte) (bool, error) {
	h, err := p.host(ctx, provider, hostID)
	if err != nil {
		return false, err
	}
	l, err := loadHostEnrolment(ctx, p.secretReader(), p.Config, h)
	if err != nil || l.reset {
		return false, err
	}
	if !l.state.discardPredecessor(hex.EncodeToString(id)) {
		return false, nil
	}
	if err := saveHostEnrolment(ctx, p.Client, p.Config, h, &l); err != nil {
		return false, err
	}
	return true, nil
}

// checkFreezeDiscards asks the host, for each confirmed predecessor, about an
// op_id it never saw under that enrolment, and removes the predecessor from
// the Host's state only on a verified signed answer from a READY agent that
// the enrolment is UNKNOWN_TO_AGENT (its records are discarded); a NO_RECORD
// says it is still held, and anything else concludes nothing. It returns the
// removed predecessors' ids.
func (h *HostAnswers) checkFreezeDiscards(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	reader contracts.HostAnswerReader, hostID string) []string {
	ft, ok := h.Trust.(freezeTrust)
	if !ok {
		return nil
	}
	confirmed, err := ft.ConfirmedFrozen(ctx, provider, hostID)
	if err != nil || len(confirmed) == 0 {
		return nil
	}
	logger := log.FromContext(ctx)
	var removed []string
	for _, id := range confirmed {
		probe := make([]byte, operation.IDLen)
		if _, err := rand.Read(probe); err != nil {
			return removed
		}
		hr, err := h.begin(ctx, provider, hostID)
		if err != nil {
			return removed
		}
		_, read, rerr := reader.OperationAnswer(ctx, contracts.OperationAnswerQuery{OpID: probe, OpEnrolmentID: id, HostID: hostID,
			ContinuitySeq: hr.snapshot.ContinuitySeq()})
		if rerr != nil && read.Answer == nil {
			logger.Info("The host was not asked whether it discarded a predecessor enrolment", "host", hostID, "error", providerFailureDetail(rerr))
			return removed
		}
		discarded := func(v hostidentity.VerifiedAnswer) bool {
			return v.Ready() && v.Operation != nil &&
				v.Operation.GetState() == providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNKNOWN_TO_AGENT &&
				bytes.Equal(v.Operation.GetOpEnrolmentId(), id) && bytes.Equal(v.Operation.GetOpId(), probe) &&
				!bytes.Equal(v.Context.GetEnrolmentId(), id)
		}
		v, verdict := h.finish(ctx, provider, hostID, hr, hostidentity.BodyTypeOperationAnswer, read, discarded)
		if verdict != answerVerified || !discarded(v) {
			continue
		}
		ok, err := ft.DiscardPredecessor(ctx, provider, hostID, id)
		if err != nil {
			logger.Info("A discarded predecessor enrolment could not be removed; it is checked again", "host", hostID, "error", err.Error())
			continue
		}
		if ok {
			auditSignedDecision(ctx, read, v, "predecessor enrolment discarded by the host", hex.EncodeToString(probe))
			removed = append(removed, hex.EncodeToString(id))
		}
	}
	return removed
}
