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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// maxDiscardedEnrolmentIDs bounds the predecessors kept as discarded per Host
// (oldest dropped first): an attempt still open under one is held as a
// discarded predecessor's, never as tampered.
const maxDiscardedEnrolmentIDs = 32

// predecessorQuietPeriod is how long after a re-key a predecessor is never
// confirmed, so that an attempt minted under it just before the re-key, whose
// marker may not be visible yet, is seen by the check. The first send of an
// attempt also re-reads the Host's enrolment after its marker is written
// (sendFresh), so the period is a margin, not the only guard.
const predecessorQuietPeriod = 2 * time.Minute

// predecessorClockSkew is added to the quiet period for the clock difference
// between the manager that stamped a predecessor and the one that checks it
// (a leader change).
const predecessorClockSkew = 30 * time.Second

// Recheck of a Host with an unconfirmed predecessor: from
// freezeRecheckMin, doubling while the predecessor stays blocked, up to
// freezeRecheckMax (open attempts end without an event on the Host).
const (
	freezeRecheckMin = time.Minute
	freezeRecheckMax = 10 * time.Minute
)

// freezeListPage bounds one page of the live VirtualMachine list that
// confirms no attempt blocks a predecessor.
const freezeListPage = 500

// Event reasons of freeze confirmation.
const (
	eventReasonAgentFreezeConfirmed      = "AgentFreezeConfirmed"
	eventReasonAgentPredecessorDiscarded = "AgentPredecessorDiscarded"
)

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

// blockingAttempts returns vm's attempts that keep a predecessor of host
// from being confirmed: every attempt on host that is not Terminal, and, for a
// VM being deleted that still holds its finalizer, a Terminal Delete attempt
// on host (its release may still need to ask the host).
func blockingAttempts(vm *infravirtrigaudiov1beta1.VirtualMachine, host string) []infravirtrigaudiov1beta1.OperationAttempt {
	var out []infravirtrigaudiov1beta1.OperationAttempt
	deleting := !vm.DeletionTimestamp.IsZero() && len(vm.Finalizers) > 0
	for _, a := range vm.Status.OperationAttempts {
		if a.HostID != host {
			continue
		}
		if a.Phase != infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal ||
			(deleting && a.Kind == infravirtrigaudiov1beta1.OperationAttemptKindDelete) {
			out = append(out, a)
		}
	}
	return out
}

// freezeBlockers is which predecessors the attempts seen block: blocked
// holds the enrolment ids their authenticated intents name; all is set when
// an attempt's enrolment could not be learnt (no intent authenticator, or an
// intent that is missing or does not authenticate), which blocks every
// predecessor.
type freezeBlockers struct {
	blocked map[string]bool
	all     bool
	open    int
}

// blocks reports whether predecessor p is blocked.
func (b freezeBlockers) blocks(p string) bool { return b.all || b.blocked[p] }

// addVMs adds the blocking attempts of vms on host (VMs of host's Provider
// only), learning each attempt's enrolment from its authenticated intent.
func (r *HostEnrolmentReconciler) addVMs(ctx context.Context, b *freezeBlockers, host *infravirtrigaudiov1beta1.Host,
	vms []infravirtrigaudiov1beta1.VirtualMachine) {
	for i := range vms {
		vm := &vms[i]
		if !vmOfProvider(vm, host.Namespace, host.Spec.ProviderRef.Name) {
			continue
		}
		for _, a := range blockingAttempts(vm, host.Name) {
			b.open++
			if id, ok := r.attemptEnrolment(ctx, vm, a, host.Namespace); ok {
				b.blocked[id] = true
			} else {
				b.all = true
			}
		}
	}
}

// attemptEnrolment returns the enrolment (hex) attempt a of vm was issued
// under, from its intent (full or redacted) in namespace ns, authenticated
// with the manager's intent key; ok is false when it cannot be learnt.
func (r *HostEnrolmentReconciler) attemptEnrolment(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	a infravirtrigaudiov1beta1.OperationAttempt, ns string) (string, bool) {
	if r.IntentAuth == nil {
		return "", false
	}
	vr := &VirtualMachineReconciler{Client: r.Client, APIReader: r.liveReader(), OperationIntentAuth: r.IntentAuth}
	ref, found, err := vr.loadPollIntent(ctx, vm, a.OpID, ns)
	if err != nil || !found || len(ref.Identity.EnrolmentID) != operation.IDLen {
		return "", false
	}
	return hex.EncodeToString(ref.Identity.EnrolmentID), true
}

// cachedBlockers reads the Host's Provider's VirtualMachines from the cache
// through the provider indexes; ok is false when the cache cannot serve
// them (no index), and the caller goes to the API server.
func (r *HostEnrolmentReconciler) cachedBlockers(ctx context.Context, host *infravirtrigaudiov1beta1.Host) (freezeBlockers, bool) {
	b := freezeBlockers{blocked: map[string]bool{}}
	key := types.NamespacedName{Namespace: host.Namespace, Name: host.Spec.ProviderRef.Name}.String()
	seen := map[types.UID]bool{}
	var vms []infravirtrigaudiov1beta1.VirtualMachine
	for _, index := range []string{specProviderIndex, placementProviderIndex} {
		var list infravirtrigaudiov1beta1.VirtualMachineList
		if err := r.List(ctx, &list, client.MatchingFields{index: key}); err != nil {
			return b, false
		}
		for i := range list.Items {
			if !seen[list.Items[i].UID] {
				seen[list.Items[i].UID] = true
				vms = append(vms, list.Items[i])
			}
		}
	}
	r.addVMs(ctx, &b, host, vms)
	return b, true
}

// liveBlockers reads every VirtualMachine from the API server, page by page
// (freezeListPage), so that an attempt written a moment ago is seen.
func (r *HostEnrolmentReconciler) liveBlockers(ctx context.Context, host *infravirtrigaudiov1beta1.Host) (freezeBlockers, error) {
	b := freezeBlockers{blocked: map[string]bool{}}
	cont := ""
	for {
		var list infravirtrigaudiov1beta1.VirtualMachineList
		if err := r.liveReader().List(ctx, &list, client.Limit(freezeListPage), client.Continue(cont)); err != nil {
			return b, fmt.Errorf("list VirtualMachines for Host %s/%s: %w", host.Namespace, host.Name, err)
		}
		r.addVMs(ctx, &b, host, list.Items)
		if cont = list.Continue; cont == "" {
			return b, nil
		}
	}
}

// freezeRecheck returns the recheck delay of a Host after n consecutive
// blocked checks.
func freezeRecheck(n int) time.Duration {
	d := freezeRecheckMin << min(n, 4)
	return min(d, freezeRecheckMax)
}

// confirmFreezes confirms the freeze of the Host's predecessors that are past
// their quiet period and that no attempt blocks: the cache is asked first,
// and only when it shows a predecessor free is the API server read to confirm
// it. It reports whether the state changed and, when a predecessor still
// waits, after how long to look again (zero: none waits).
func (r *HostEnrolmentReconciler) confirmFreezes(ctx context.Context, host *infravirtrigaudiov1beta1.Host,
	st *hostEnrolmentState) (changed bool, recheck time.Duration, err error) {
	key := host.Namespace + "/" + host.Name
	if st.Current == nil || host.Spec.Revoked {
		r.freezeWaits.Delete(key)
		return false, 0, nil
	}
	now := r.now()
	var due []string
	waiting := false
	for _, p := range st.PredecessorEnrolmentIDs {
		if slices.Contains(st.ConfirmedFrozenEnrolmentIDs, p) {
			continue
		}
		since, ok := st.PredecessorSince[p]
		if !ok {
			// A predecessor recorded without a time (before times were
			// kept) starts its quiet period now.
			if st.PredecessorSince == nil {
				st.PredecessorSince = map[string]metav1.Time{}
			}
			st.PredecessorSince[p] = predecessorStamp(now)
			changed, waiting = true, true
			continue
		}
		if now.Sub(since.Time) < predecessorQuietPeriod+predecessorClockSkew {
			waiting = true
			continue
		}
		due = append(due, p)
	}
	n := 0
	if v, ok := r.freezeWaits.Load(key); ok {
		n, _ = v.(int)
	}
	wait := func() (bool, time.Duration, error) {
		r.freezeWaits.Store(key, n+1)
		return changed, freezeRecheck(n), nil
	}
	if len(due) == 0 {
		if waiting {
			return changed, freezeRecheckMin, nil
		}
		r.freezeWaits.Delete(key)
		return changed, 0, nil
	}
	free := func(b freezeBlockers) []string {
		var out []string
		for _, p := range due {
			if !b.blocks(p) {
				out = append(out, p)
			}
		}
		return out
	}
	if cached, ok := r.cachedBlockers(ctx, host); ok && len(free(cached)) == 0 {
		log.FromContext(ctx).V(1).Info("Predecessor freeze not confirmed yet: attempts on the host block it (cache)",
			"host", key, "openAttempts", cached.open, "predecessors", predecessorSummary(st))
		return wait()
	}
	live, err := r.liveBlockers(ctx, host)
	if err != nil {
		return changed, freezeRecheck(n), err
	}
	ready := free(live)
	for _, p := range ready {
		if len(st.ConfirmedFrozenEnrolmentIDs) >= hostsecret.MaxFreezeConfirmations {
			break
		}
		st.ConfirmedFrozenEnrolmentIDs = append(st.ConfirmedFrozenEnrolmentIDs, p)
		changed = true
		log.FromContext(ctx).Info("Confirmed the freeze of a predecessor enrolment: no attempt under it is open on the host",
			"host", key, "enrolmentID", p)
		r.event(host, corev1.EventTypeNormal, eventReasonAgentFreezeConfirmed, "the freeze of predecessor enrolment "+p+
			" is confirmed (no open operation under it on the host); the host is asked to discard its records")
	}
	if len(ready) < len(due) {
		log.FromContext(ctx).V(1).Info("Predecessor freeze not confirmed yet: attempts on the host block it",
			"host", key, "openAttempts", live.open, "predecessors", predecessorSummary(st))
		return wait()
	}
	r.freezeWaits.Delete(key)
	if waiting {
		return changed, freezeRecheckMin, nil
	}
	return changed, 0, nil
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
	if !slices.Contains(st.DiscardedEnrolmentIDs, p) {
		st.DiscardedEnrolmentIDs = append(st.DiscardedEnrolmentIDs, p)
		if n := len(st.DiscardedEnrolmentIDs); n > maxDiscardedEnrolmentIDs {
			st.DiscardedEnrolmentIDs = slices.Clone(st.DiscardedEnrolmentIDs[n-maxDiscardedEnrolmentIDs:])
		}
	}
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
		key := hostAnswerKey(provider, hostID) + "/" + hex.EncodeToString(id)
		if !h.freezeProbeDue(key) {
			continue
		}
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
			h.freezeProbeMissed(key)
			continue
		}
		h.freezeProbes.Delete(key)
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

// Probe backoff of a confirmed predecessor the host has not shown discarded
// yet (an agent or Provider that never applies the confirmation): from
// freezeProbeMin, doubling, up to freezeProbeMax.
const (
	freezeProbeMin = time.Minute
	freezeProbeMax = 30 * time.Minute
)

// freezeProbe is the probe backoff state of one predecessor.
type freezeProbe struct {
	missed int
	next   time.Time
}

// freezeProbeDue reports whether the predecessor of key may be probed now.
func (h *HostAnswers) freezeProbeDue(key string) bool {
	v, ok := h.freezeProbes.Load(key)
	if !ok {
		return true
	}
	p, _ := v.(freezeProbe)
	return !h.now().Before(p.next)
}

// freezeProbeMissed records a probe that did not show the predecessor
// discarded, and backs off the next one.
func (h *HostAnswers) freezeProbeMissed(key string) {
	var p freezeProbe
	if v, ok := h.freezeProbes.Load(key); ok {
		p, _ = v.(freezeProbe)
	}
	wait := min(freezeProbeMin<<min(p.missed, 5), freezeProbeMax)
	p.missed++
	p.next = h.now().Add(wait)
	h.freezeProbes.Store(key, p)
}
