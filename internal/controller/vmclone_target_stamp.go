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
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Single-host clone targets are stamped with their owner (rc2 R4).
//
// A single-host VMClone creates its target VirtualMachine BEFORE the Clone
// RPC, as a clustered one does (ensureCloneTarget), and records the target's
// UID (status.targetUID). The Clone carries that UID (CloneRequest.target_vm),
// and the provider stamps the clone with it, as a Create stamps its VM with
// CreateRequest.owner. A Clone sent again for the same target — the manager
// restarted, or the answer or its record was lost — is therefore answered with
// the clone the first attempt made (the provider re-binds the VM stamped with
// that UID) instead of a second clone.
//
// Before a Clone is sent to a provider that re-binds — one that reports
// GetCapabilitiesResponse.supports_owner_rebind (providerRebindsOwnedVM) — the
// VMClone records that a Clone was requested (the CloneRequested condition,
// written with a resourceVersion precondition; nothing is sent if it cannot
// be written). It is replaced by the outcome once the provider accepts the
// Clone. Like the create marker of a single-host VirtualMachine, it tells a
// re-send from a first attempt:
//
//   - a definite refusal of a FIRST attempt (a Conflict, an invalid spec, a
//     missing source, ...) fails the clone: nothing was made. A Conflict —
//     the target name is taken by a VM the target does not own — fails it
//     with the constant cloneTargetConflictMessage (reason ProviderConflict);
//   - any other failure, and any failure answering a re-send, leaves the
//     outcome unknown: the clone waits (CloneOutcomeUnknown, the constant
//     cloneOutcomeUnknownMessage) and the Clone is sent again with the
//     blocked-VM backoff. The re-send re-binds what the first attempt made.
//
// A provider that does not report the capability (any image that predates it,
// such as a Proxmox image that ignores target_vm) keeps the historical
// behaviour: an unknown outcome fails the clone, and a Clone is never sent to
// it again, because that could make a second VM. A re-send is paced by
// cloneAttemptPacer, whatever triggers the reconcile.

const (
	// cloneConditionRequested is the VMClone condition recording that a Clone
	// was requested from a provider that re-binds stamped clones and its
	// outcome is not recorded yet. Its presence makes the next Clone a re-send.
	cloneConditionRequested = "CloneRequested"
	// cloneReasonRequested is the reason of cloneConditionRequested.
	cloneReasonRequested = "CloneRequested"
	// cloneReasonOutcomeUnknown is the Ready reason (and Warning event reason)
	// of a clone whose Clone's outcome is unknown and is sent again.
	cloneReasonOutcomeUnknown = "CloneOutcomeUnknown"

	// errReasonCloneOutcomeUnknown is the metrics reason of a clone held for
	// an unknown outcome.
	errReasonCloneOutcomeUnknown = "clone-outcome-unknown"
	// errReasonProviderCloneRejected is the metrics reason of a clone the
	// provider refused because its target name is taken.
	errReasonProviderCloneRejected = "provider-clone-rejected"
)

// cloneRequestedMessage is the message of cloneConditionRequested.
const cloneRequestedMessage = "A provider Clone was requested for this VMClone, stamped with its target VirtualMachine's UID; " +
	"its outcome is recorded here once the provider answers. A Clone sent again returns the clone already made"

// cloneOutcomeUnknownMessage is the Ready message of a clone whose Clone's
// outcome is unknown. It is constant; the provider's answer is in the
// CloneOutcomeUnknown Warning event and the manager log.
var cloneOutcomeUnknownMessage = fmt.Sprintf("The clone's outcome is unknown: the provider's answer was lost, or it could not "+
	"tell whether it made the clone. The Clone is sent again with a backoff of up to %s; the provider returns the clone an "+
	"earlier attempt made (it is stamped with the target VirtualMachine's UID) instead of making a second one. The "+
	"provider's answer is in the %s event", blockedRetryMax, cloneReasonOutcomeUnknown)

// cloneTargetConflictMessage is the Ready message of a clone the provider
// refused because a VM of the target's name exists that the target
// VirtualMachine does not own. It is constant; the provider's answer is in the
// ProviderConflict Warning event.
var cloneTargetConflictMessage = fmt.Sprintf("The provider refused the clone: a VM with the target's name already exists "+
	"on the provider and is not owned by the target VirtualMachine (it does not carry its owner stamp), so nothing was "+
	"cloned. The provider's answer is in the %s event. Remove or rename that VM, or adopt it, then recreate the VMClone "+
	"(or clone to another target name)", k8s.ReasonProviderConflict)

// cloneTargetDeletedMessageFormat is the Failed message of a single-host clone
// whose own target VirtualMachine (namespace, name) was deleted after a Clone
// was sent for it whose outcome is not recorded.
const cloneTargetDeletedMessageFormat = "the target VirtualMachine %s/%s this clone created was deleted while a Clone sent for " +
	"it had no recorded outcome; the clone is not sent again for another VirtualMachine, because the provider may hold a " +
	"clone stamped with the deleted one's UID. Check the provider for that VM and remove or adopt it, then recreate the VMClone"

// cloneResendNotSupportedMessage is the Failed message of a clone whose
// requested Clone's outcome is unknown when its provider no longer reports
// supports_owner_rebind (e.g. it was rolled back): the Clone is never sent to
// it again. The target VirtualMachine is kept (removeFailedCloneTarget).
const cloneResendNotSupportedMessage = "A Clone was sent for this VMClone and its outcome was never recorded, and the " +
	"provider does not report that it re-binds the clone it made (supports_owner_rebind), so the Clone is not sent again: " +
	"it could make a second clone. The target VirtualMachine is kept. Check the provider for a VM stamped with this " +
	"VMClone's status.targetUID: record its id as the target's status.id, or remove it, then recreate the VMClone"

// cloneCapabilitiesRebindWaitMessage is the Pending message of a single-host
// clone whose provider did not answer GetCapabilities: nothing is sent until
// it tells whether it re-binds stamped clones. Constant; the error is in the
// manager log.
const cloneCapabilitiesRebindWaitMessage = "waiting for the provider's capabilities before the Clone " +
	"(GetCapabilities failed; the cause is in the manager log)"

// cloneTargetGoneMessageFormat is the Failed message of a clone whose own
// target VirtualMachine (namespace, name; recorded as status.targetUID) no
// longer exists when the cloned VM (id) is to be bound: no new target is
// created for it.
const cloneTargetGoneMessageFormat = "the target VirtualMachine %s/%s this clone created no longer exists; the cloned VM %q " +
	"is not bound to a new one and is left on the provider: adopt or remove it"

// cloneAttemptPacer paces the single-host Clone attempts of a held clone
// (CloneRetrying, CloneOutcomeUnknown) in this manager process: the next
// attempt is due blockedRetryBackoff after the last one, the backoff counted
// from when the failing streak began — whatever reconciles the clone
// meanwhile (a spec edit, a grant or source power change) and whichever of the
// two hold reasons the attempts alternate between. It is keyed by the VMClone
// UID and forgotten when the clone is accepted or ends. A manager restart
// forgets it: the next attempt is then due at once. The zero value is ready.
type cloneAttemptPacer struct {
	mu sync.Mutex
	m  map[types.UID]cloneAttemptRecord
}

// cloneAttemptRecord is one clone's failing streak: when it began and when
// its last attempt failed.
type cloneAttemptRecord struct {
	streakSince, last time.Time
}

// failed records that uid's attempt failed at now and returns how long the
// next one waits. since, when not zero, is when the clone's hold began (its
// Ready condition's LastTransitionTime): the streak counts from the earlier
// of it and the recorded start.
func (p *cloneAttemptPacer) failed(uid types.UID, since, now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[types.UID]cloneAttemptRecord{}
	}
	rec, ok := p.m[uid]
	if !ok || rec.streakSince.IsZero() {
		rec.streakSince = now
	}
	if !since.IsZero() && since.Before(rec.streakSince) {
		rec.streakSince = since
	}
	rec.last = now
	p.m[uid] = rec
	return cloneAttemptBackoff(rec)
}

// wait reports how long uid's next attempt must still wait at now (0: due).
func (p *cloneAttemptPacer) wait(uid types.UID, now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, ok := p.m[uid]
	if !ok {
		return 0
	}
	if w := rec.last.Add(cloneAttemptBackoff(rec)).Sub(now); w > 0 {
		return w
	}
	return 0
}

// forget drops uid's record: its clone was accepted or ended.
func (p *cloneAttemptPacer) forget(uid types.UID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, uid)
}

// cloneAttemptBackoff is the wait after rec's last attempt: as long as the
// streak had lasted then, between blockedRetryMin and blockedRetryMax (15 s
// doubling to 5 min).
func cloneAttemptBackoff(rec cloneAttemptRecord) time.Duration {
	return min(max(rec.last.Sub(rec.streakSince), blockedRetryMin), blockedRetryMax)
}

// cloneHoldSince is when clone's current hold began: its Ready condition's
// LastTransitionTime when it shows one of the single-host hold reasons, else
// the zero time.
func cloneHoldSince(clone *infrav1beta1.VMClone) time.Time {
	prev := meta.FindStatusCondition(clone.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	if prev == nil || (prev.Reason != cloneReasonRetrying && prev.Reason != cloneReasonOutcomeUnknown) {
		return time.Time{}
	}
	return prev.LastTransitionTime.Time
}

// clearCloneRequested drops clone's CloneRequested record (in memory; the
// caller's status write persists it) when the provider's answer proves that
// a FIRST attempt made nothing, so a later answer is not mistaken for one to
// a re-send.
func clearCloneRequested(clone *infrav1beta1.VMClone) {
	meta.RemoveStatusCondition(&clone.Status.Conditions, cloneConditionRequested)
}

// isCloneTarget reports whether vm is a VMClone's target VirtualMachine: it
// carries the clone-uid marker buildTargetVM sets (CloneAnnotationCloneUID).
// Such a VM is created before its Clone and waits, unbound, for that clone's
// bind; adoption never binds it to a listed VM or treats it as an adopter.
func isCloneTarget(vm *infrav1beta1.VirtualMachine) bool {
	return vm.Annotations[CloneAnnotationCloneUID] != ""
}

// cloneRequested reports whether clone records a Clone requested from a
// provider that re-binds, whose outcome is not recorded (the next Clone is a
// re-send).
func cloneRequested(clone *infrav1beta1.VMClone) bool {
	return meta.IsStatusConditionTrue(clone.Status.Conditions, cloneConditionRequested)
}

// markCloneRequested durably records, before a stamped Clone is sent, that a
// Clone was requested (cloneConditionRequested). It is written over clone as
// read — which confirmCloneNotStarted has just confirmed is the live object —
// with the optimistic lock of its resourceVersion; a clone that already
// records it writes nothing. sent=false means the record could not be written
// and nothing may be sent: res is the result to return.
func (r *VMCloneReconciler) markCloneRequested(ctx context.Context, clone *infrav1beta1.VMClone) (ctrl.Result, bool) {
	if cloneRequested(clone) {
		return ctrl.Result{}, true
	}
	meta.SetStatusCondition(&clone.Status.Conditions, metav1.Condition{
		Type:               cloneConditionRequested,
		Status:             metav1.ConditionTrue,
		Reason:             cloneReasonRequested,
		Message:            cloneRequestedMessage,
		ObservedGeneration: clone.Generation,
	})
	if err := r.Status().Update(ctx, clone); err != nil {
		logging.FromContext(ctx).Info("Cannot record the requested Clone on the VMClone; not cloning", "error", err.Error())
		return ctrl.Result{RequeueAfter: cloneStaleReadRetryInterval}, false
	}
	return ctrl.Result{}, true
}

// handleSingleHostCloneError decides a failed single-host Clone RPC. rebinds
// reports that the provider re-binds a clone stamped with the target's UID
// (providerRebindsOwnedVM; the request carried it), resend that an earlier
// attempt may have made the clone (cloneRequested before this attempt). An
// answer that proves a first attempt made nothing — a running source, nothing
// done, a definite refusal — also drops the CloneRequested record, in the
// same status write:
//
//   - a source that is not powered off waits for it (holdCloneForRunningSource);
//   - an owner refusal of the source (VM_NOT_OWNED, OWNER_STAMP_UNREADABLE)
//     fails a first attempt with the constant cloneSourceNotOwnedMessage; a
//     re-send's is held like any failure answering a re-send;
//   - an answer that nothing was done (singleHostCloneRetryable, or
//     CREATE_NOT_PERFORMED) is held and retried (holdCloneForTransientError);
//   - a definite refusal of a first attempt fails the clone — a name Conflict
//     with the constant cloneTargetConflictMessage;
//   - anything else from a provider that re-binds waits with an unknown
//     outcome and is sent again (holdCloneOutcomeUnknown);
//   - anything else fails the clone, as before: a provider that does not
//     re-bind could make a second VM.
//
// An answer that drops the CloneRequested record must not be lost to a
// status write that conflicts with an edit of the VMClone made while the
// provider ran the Clone: the record would survive, and the next answer would
// be taken for one to a re-send (a first attempt's Conflict held as an
// unknown outcome for ever, a deleted target failing the clone as one that
// may hold a clone). recordCloneAnswer re-applies it.
func (r *VMCloneReconciler) handleSingleHostCloneError(ctx context.Context, clone *infrav1beta1.VMClone, err error,
	rebinds, resend bool) ctrl.Result {
	nothingMade := contracts.IsVMSourceRunning(err) || singleHostCloneRetryable(err) || contracts.IsCreateNotPerformed(err) ||
		contracts.IsConflict(err) || createDefinitelyRefused(err) || isOwnerRefusal(err)
	var requested *metav1.Condition
	if !resend && nothingMade {
		if c := meta.FindStatusCondition(clone.Status.Conditions, cloneConditionRequested); c != nil {
			requested = c.DeepCopy()
		}
		clearCloneRequested(clone)
	}
	res := r.decideSingleHostCloneError(ctx, clone, err, rebinds, resend)
	if requested != nil {
		r.recordCloneAnswer(ctx, clone, requested)
	}
	return res
}

// decideSingleHostCloneError is handleSingleHostCloneError's decision; each
// branch writes clone's status as it records the answer.
func (r *VMCloneReconciler) decideSingleHostCloneError(ctx context.Context, clone *infrav1beta1.VMClone, err error,
	rebinds, resend bool) ctrl.Result {
	logger := logging.FromContext(ctx)
	switch {
	case contracts.IsVMSourceRunning(err):
		// A libvirt full clone requires a powered-off source: the clone waits
		// (Pending) until it is off, and is never failed for it.
		return r.holdCloneForRunningSource(ctx, clone, err)
	case !resend && isOwnerRefusal(err):
		// The source VM is not the source VirtualMachine's (a reused id), or
		// its owner cannot be told: a first attempt is refused before
		// anything was cloned, and failed with a constant message
		// (copy_source_owner_refusal.go). Answering a re-send it says nothing
		// about the clone an earlier attempt may have made (the provider
		// re-binds before it checks the source), so it is held below.
		logger.Info("The provider refused the source VM as not the source VirtualMachine's", "error", err.Error())
		return r.markFailed(ctx, clone, ownerRefusalReason(err), cloneSourceNotOwnedMessage)
	case singleHostCloneRetryable(err), contracts.IsCreateNotPerformed(err):
		// A transient failure (the provider or its host briefly unreachable,
		// a check that could not run, a copy still in progress) is held and
		// retried with a backoff, never failed: one SSH or libvirtd blip must
		// not end the clone. So is a provider's CREATE_NOT_PERFORMED (the
		// Proxmox provider: nothing was made, e.g. its cluster list could not
		// be read).
		return r.holdCloneForTransientError(ctx, clone, err)
	case !resend && contracts.IsConflict(err):
		logger.Info("Provider refused the clone: its target name is taken by a VM the target does not own",
			"error", sanitizeProviderDetail(err))
		metrics.RecordError(errReasonProviderCloneRejected, metrics.ComponentManager)
		r.Recorder.Event(clone, "Warning", k8s.ReasonProviderConflict, "Clone refused: "+rpcFailureDetail(err))
		return r.markFailed(ctx, clone, k8s.ReasonProviderConflict, cloneTargetConflictMessage)
	case rebinds && (resend || !createDefinitelyRefused(err)):
		return r.holdCloneOutcomeUnknown(ctx, clone, err)
	}
	logger.Error(err, "Clone RPC failed")
	detail := rpcFailureDetail(err)
	r.Recorder.Event(clone, "Warning", infrav1beta1.VMCloneReasonProviderError, "Clone failed: "+detail)
	return r.markFailed(ctx, clone, infrav1beta1.VMCloneReasonProviderError, "clone failed: "+detail)
}

// recordCloneAnswer makes sure the answer to a first single-host Clone
// attempt that dropped the CloneRequested record (requested, as it was before
// the answer) is on record. The branch that decided the answer wrote clone's
// status under the resourceVersion it read; an edit of the VMClone made while
// the provider ran the Clone makes that write fail with a Conflict. So the
// VMClone is read live and, while it is the same object and still carries
// that very record — no answer of this attempt landed, and nothing newer did
// — clone's status is written over it (retry.RetryOnConflict). Only this
// controller writes a VMClone's status and a VMClone is never reconciled
// concurrently (persistCloneStatus), so the stored status is the one this
// reconcile read and replacing it loses nothing. Any failure is logged: the
// record then stands, and the next answer is read as one to a re-send.
func (r *VMCloneReconciler) recordCloneAnswer(ctx context.Context, clone *infrav1beta1.VMClone, requested *metav1.Condition) {
	logger := logging.FromContext(ctx)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live := &infrav1beta1.VMClone{}
		if err := r.liveReader().Get(ctx, client.ObjectKeyFromObject(clone), live); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("read VMClone %s/%s: %w", clone.Namespace, clone.Name, err)
		}
		stored := meta.FindStatusCondition(live.Status.Conditions, cloneConditionRequested)
		if live.UID != clone.UID || stored == nil || stored.Status != requested.Status ||
			!stored.LastTransitionTime.Equal(&requested.LastTransitionTime) {
			return nil // recorded already, or something newer is
		}
		logger.Info("Recording the Clone's answer on the VMClone as stored (its first write conflicted)")
		live.Status = *clone.Status.DeepCopy()
		if err := r.Status().Update(ctx, live); err != nil {
			return err
		}
		clone.ResourceVersion = live.ResourceVersion
		return nil
	})
	if err != nil {
		logger.Error(err, "Failed to record the provider's answer to the Clone")
	}
}

// holdCloneOutcomeUnknown keeps a single-host clone whose Clone's outcome is
// unknown Pending with Ready=False/CloneOutcomeUnknown (the constant
// cloneOutcomeUnknownMessage), records the provider's answer (made safe by
// rpcFailureDetail) in a Warning event when the hold begins, and sends the
// Clone again after the blocked-VM backoff (blockedRetryBackoff: 15 s doubling
// to 5 min, from when the failing streak began; cloneAttemptPacer). A
// repeated hold writes nothing. The CloneRequested record is kept, so the
// next attempt is a re-send.
func (r *VMCloneReconciler) holdCloneOutcomeUnknown(ctx context.Context, clone *infrav1beta1.VMClone, err error) ctrl.Result {
	retry := r.cloneAttempts.failed(clone.UID, cloneHoldSince(clone), time.Now())
	logging.FromContext(ctx).Info("The Clone's outcome is unknown; holding the clone and sending it again (the provider "+
		"re-binds the clone stamped with the target's UID)", "retryAfter", retry.String(), "error", err.Error())
	metrics.RecordError(errReasonCloneOutcomeUnknown, metrics.ComponentManager)
	r.announceCloneWait(clone, cloneReasonOutcomeUnknown, "", err)
	res := r.markPending(ctx, clone, cloneReasonOutcomeUnknown, cloneOutcomeUnknownMessage)
	res.RequeueAfter = retry
	return res
}
