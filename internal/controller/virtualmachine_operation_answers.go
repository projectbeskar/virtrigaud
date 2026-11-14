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
	"crypto/subtle"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Outcomes of operation attempts from signed host answers only (development
// builds; a Provider whose type requires operation identity). Every attempt
// in flight is moved by asking its host about its op_id, with a fresh nonce,
// and reading the answer only once it verified (hostidentity.VerifyAnswer),
// its store continuity holds, its agent is READY and it binds to the
// attempt's MAC-verified intent (op_id, enrolment, kind, attempt, VM,
// binding, intent digest). Everything else is no answer: the attempt stays
// as it is and the host is asked again; nothing is concluded from the
// Provider's unsigned fields (a send's answer or refusal, a task status, a
// Describe's exists). In particular:
//
//   - a not-performed outcome (FAILED_CLEAN, RETIRED) counts only signed and
//     bound, so a lying Provider cannot make the manager mint a second
//     attempt;
//   - a re-send of the same op_id follows only a signed NO_RECORD under the
//     host's current enrolment, never a predecessor's;
//   - a Delete releases its VM's finalizer only on a signed COMPLETED of the
//     Delete's op_id or a signed DELETED of the VM's placement;
//   - a regression of the host's store holds the host.

// msgHostAnswersUnavailable is the hold of a manager that cannot verify host
// answers.
var msgHostAnswersUnavailable = "the Provider requires operation identity, and this manager cannot verify signed host answers " +
	"(a development build needs per-host agent enrolment, --" + DevHostEnrolmentNamespaceFlagName + "), or the Provider's client " +
	"cannot ask for them: nothing is sent"

// msgCreateNotPerformedUnconfirmed is the hold of a VM being deleted whose
// latest Create is recorded not performed but not signed again by its host.
var msgCreateNotPerformedUnconfirmed = "the VM's latest Create is recorded not performed, but its host does not sign that again " +
	"(its stored intent is gone, or the host has no record of it under its current enrolment), so the finalizer is kept; " +
	operationHoldRecovery

// msgHostStoreRegressed is the hold of a VM whose host's store regressed.
var msgHostStoreRegressed = "the host's operation store was replaced, rolled back or forked since the manager last verified it; " +
	"nothing it answers is acted on until it is re-enrolled; " + operationHoldRecovery

// grantEnrolment returns the enrolment a new attempt's grants name on host:
// the host's current enrolment from its trust. It holds vm (ok false) when
// the host has none, its store regressed, or it has no recent verified host
// answer from a READY agent that implements the manager's grant version.
func (r *VirtualMachineReconciler) grantEnrolment(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	host string) (operation.Enrolment, attemptStep, ctrl.Result, bool) {
	hr, err := r.HostAnswers.begin(ctx, ia.providerCR, host)
	switch {
	case errors.Is(err, errHostRegressed):
		return operation.Enrolment{}, stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationHostStoreRegressed, msgHostStoreRegressed,
			operationHoldRecheckInterval), false
	case err != nil:
		log.FromContext(ctx).Info("No operation-identity enrolment for the host; nothing is sent", "host", host, "error", err.Error())
		return operation.Enrolment{}, stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationEnrolmentUnavailable,
			fmt.Sprintf("no operation-identity enrolment is configured for host %s; nothing is sent", host), operationHoldRecheckInterval), false
	}
	if err := r.HostAnswers.grantReady(ia.providerCR, host, hr.trust.Enrolment.EnrolmentID); err != nil {
		return operation.Enrolment{}, stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationHostAnswerUnavailable,
			fmt.Sprintf("host %s has no recent verified signed host answer (%s); nothing new is signed for it", host, err.Error()),
			operationHoldRecheckInterval), false
	}
	return hr.trust.Enrolment, stepWaiting, ctrl.Result{}, true
}

// opPoll is one signed poll of an attempt.
type opPoll struct {
	verdict answerVerdict
	v       hostidentity.VerifiedAnswer
	read    contracts.SignedRead
}

// state is the answer's operation state.
func (p opPoll) state() providerv1.OperationRecordState { return p.v.Operation.GetState() }

// predecessor reports whether the answer is about an op_id of an enrolment
// other than the host's current one.
func (p opPoll) predecessor() bool {
	return !bytes.Equal(p.v.Operation.GetOpEnrolmentId(), p.v.Context.GetEnrolmentId())
}

// definiteOpState reports whether s decides an attempt.
func definiteOpState(s providerv1.OperationRecordState) bool {
	switch s {
	case providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED, providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_CLEAN,
		providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_DIRTY, providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED:
		return true
	}
	return false
}

// bindsTo reports whether a verified OperationAnswer is about the attempt p:
// its op_id and enrolment, and, for an answer from a record, its kind,
// attempt, VM, binding and the digest of p's exact request bytes.
func bindsTo(a *providerv1.OperationAnswer, p *contracts.PreparedOperation) bool {
	id := p.Identity
	if !bytes.Equal(a.GetOpId(), id.OpID) || !bytes.Equal(a.GetOpEnrolmentId(), id.EnrolmentID) {
		return false
	}
	switch a.GetState() {
	case providerv1.OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD,
		providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNKNOWN_TO_AGENT:
		return true
	}
	wire, ok := wireKinds[p.Kind]
	return ok && a.GetKind() == int32(wire) && a.GetAttempt() == id.Attempt && a.GetVmUid() == id.VMUID &&
		bytes.Equal(a.GetBindingId(), id.BindingID) &&
		subtle.ConstantTimeCompare(a.GetIntentDigest(), operation.IntentDigest(p.DigestSalt, p.RequestBytes)) == 1
}

// pollSigned asks the host of attempt p about its op_id and returns the
// verified, bound answer, or a verdict of no answer or a regressed host. A
// verified answer that does not bind to p is no answer (and logged).
func (r *VirtualMachineReconciler) pollSigned(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	p *contracts.PreparedOperation) opPoll {
	logger := log.FromContext(ctx)
	host := p.Identity.HostID
	hr, err := r.HostAnswers.begin(ctx, ia.providerCR, host)
	if errors.Is(err, errHostRegressed) {
		return opPoll{verdict: answerRegressed}
	}
	if err != nil {
		logger.Info("Cannot ask the host about the attempt", "host", host, "error", err.Error())
		return opPoll{verdict: answerNone}
	}
	_, read, rerr := ia.reader.OperationAnswer(ctx, contracts.OperationAnswerQuery{OpID: p.Identity.OpID,
		OpEnrolmentID: p.Identity.EnrolmentID, HostID: host, ContinuitySeq: hr.snapshot.ContinuitySeq()})
	if rerr != nil && read.Answer == nil {
		logger.Info("The host was not asked about the attempt, or did not answer", "host", host, "error", providerFailureDetail(rerr))
		return opPoll{verdict: answerNone, read: read}
	}
	definite := func(v hostidentity.VerifiedAnswer) bool {
		return v.Ready() && bindsTo(v.Operation, p) && definiteOpState(v.Operation.GetState())
	}
	v, verdict := r.HostAnswers.finish(ctx, ia.providerCR, host, hr, hostidentity.BodyTypeOperationAnswer, read, definite)
	if verdict == answerVerified && !bindsTo(v.Operation, p) {
		logger.Info("The host's signed answer is about another operation than the attempt; nothing is concluded", "host", host)
		return opPoll{verdict: answerNone, read: read}
	}
	return opPoll{verdict: verdict, v: v, read: read}
}

// driveSigned moves the attempt a (its verified intent prepared) on its
// host's signed answer about its op_id. predecessor is true for an intent
// under a predecessor enrolment (asked about, never re-sent); sent is true
// when this reconcile already delivered the attempt (no second delivery).
func (r *VirtualMachineReconciler) driveSigned(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	a *infravirtrigaudiov1beta1.OperationAttempt, prepared contracts.PreparedOperation, predecessor, sent bool) (attemptStep, ctrl.Result, error) {
	poll := r.pollSigned(ctx, vm, ia, &prepared)
	isDelete := a.Kind == infravirtrigaudiov1beta1.OperationAttemptKindDelete
	switch poll.verdict {
	case answerRegressed:
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationHostStoreRegressed, msgHostStoreRegressed, operationHoldRecheckInterval), nil
	case answerNone:
		if isDelete {
			if step, res, done := r.deleteByVMAnswer(ctx, vm, ia, a, &prepared); done {
				return step, res, nil
			}
		}
		return r.noSignedAnswer(ctx, vm, a, "the host gave no verified signed answer about it")
	}
	ctxState := poll.v.Context.GetAgentState()
	if !poll.v.Ready() {
		if ctxState == providerv1.AgentState_AGENT_STATE_QUARANTINED {
			return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationOutcomeUnknown, fmt.Sprintf(
				"the host's agent is quarantined (%s): nothing it answers is definite; %s", poll.v.Context.GetStateReason(),
				operationHoldRecovery), operationHoldRecheckInterval), nil
		}
		return r.noSignedAnswer(ctx, vm, a, "the host's agent has not yet proved the continuity of its store")
	}
	pre := predecessor || poll.predecessor()
	switch st := poll.state(); st {
	case providerv1.OperationRecordState_OPERATION_RECORD_STATE_ACCEPTED, providerv1.OperationRecordState_OPERATION_RECORD_STATE_EXECUTING:
		cur := a
		if a.Phase != infravirtrigaudiov1beta1.OperationAttemptPhaseSent {
			m, err := r.markSent(ctx, vm, a, nil)
			if err != nil {
				return stepWaiting, ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
			}
			cur = m
		}
		if r.runningPastDeadline(cur) {
			return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationOutcomeUnknown, fmt.Sprintf(
				"%s attempt %d has been reported running for more than %s; its outcome is treated as unknown (it is still asked "+
					"about, and a Delete of the VM supersedes it); %s", a.Kind, a.Attempt, sentAttemptDeadline, operationHoldRecovery),
				operationHoldRecheckInterval), nil
		}
		return stepWaiting, r.operationInProgress(ctx, vm, cur, operationPollInterval), nil
	case providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED:
		if a.Kind == infravirtrigaudiov1beta1.OperationAttemptKindCreate && a.ID == "" {
			return r.learnCreateID(ctx, vm, ia, a, prepared, sent)
		}
		return r.closeSigned(ctx, vm, ia, a, poll, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded)
	case providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_CLEAN:
		return r.closeSigned(ctx, vm, ia, a, poll, infravirtrigaudiov1beta1.OperationAttemptOutcomeFailedClean)
	case providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_DIRTY:
		return r.closeSigned(ctx, vm, ia, a, poll, infravirtrigaudiov1beta1.OperationAttemptOutcomeFailedDirty)
	case providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED:
		if pre {
			return r.holdPredecessor(ctx, vm, a, "RETIRED")
		}
		return r.closeSigned(ctx, vm, ia, a, poll, infravirtrigaudiov1beta1.OperationAttemptOutcomeRetired)
	case providerv1.OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD:
		if isDelete {
			if step, res, done := r.deleteByVMAnswer(ctx, vm, ia, a, &prepared); done {
				return step, res, nil
			}
		}
		if pre {
			return r.holdPredecessor(ctx, vm, a, "NO_RECORD")
		}
		if sent {
			return r.noSignedAnswer(ctx, vm, a, "the host has no record of it after it was delivered; it is delivered again later")
		}
		// The host signed that it has no record under its current
		// enrolment: a delivery may be lost, so the same op_id is
		// delivered again under a fresh grant.
		return r.sendAttempt(ctx, vm, ia, a, prepared, false)
	case providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNKNOWN_TO_AGENT:
		if isDelete {
			if step, res, done := r.deleteByVMAnswer(ctx, vm, ia, a, &prepared); done {
				return step, res, nil
			}
		}
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationUnknownToAgent, msgOperationUnknownToAgent, operationHoldRecheckInterval), nil
	default: // OUTCOME_UNKNOWN
		if isDelete {
			if step, res, done := r.deleteByVMAnswer(ctx, vm, ia, a, &prepared); done {
				return step, res, nil
			}
		}
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationOutcomeUnknown,
			fmt.Sprintf("the host signed that it cannot tell what %s attempt %d did (%s); it is asked again periodically; %s", a.Kind,
				a.Attempt, poll.v.Operation.GetOutcomeReason(), operationHoldRecovery), operationHoldRecheckInterval), nil
	}
}

// learnCreateID handles a Create the host signed COMPLETED whose provider id
// the manager never received (the send's answer was lost): the same op_id is
// delivered once more (the host answers a known op_id from its record, and
// executes nothing again), and the id it returns is recorded on the marker;
// the Create closes on the next signed COMPLETED.
func (r *VirtualMachineReconciler) learnCreateID(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	a *infravirtrigaudiov1beta1.OperationAttempt, prepared contracts.PreparedOperation, sent bool) (attemptStep, ctrl.Result, error) {
	if !sent {
		answer, err := ia.sender.SendOperation(ctx, prepared)
		if err == nil && answer.ID != "" {
			if _, werr := r.markSent(ctx, vm, a, &answer); werr == nil {
				return stepWaiting, ctrl.Result{RequeueAfter: operationPollInterval}, nil
			}
		}
	}
	return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationIntentLost, msgOperationIntentLost, operationHoldRecheckInterval), nil
}

// closeSigned closes a on the verified answer of poll with outcome, after an
// audit record of the signed decision.
func (r *VirtualMachineReconciler) closeSigned(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	a *infravirtrigaudiov1beta1.OperationAttempt, poll opPoll, outcome infravirtrigaudiov1beta1.OperationAttemptOutcome) (attemptStep, ctrl.Result, error) {
	if r.operationCrashed(ctx, crashAfterReply) {
		return stepWaiting, ctrl.Result{}, errOperationCrashed
	}
	summary := auditSignedDecision(ctx, poll.read, poll.v, fmt.Sprintf("%s attempt %d closed %s", a.Kind, a.Attempt, outcome), a.OpID)
	r.recordEvent(vm, corev1.EventTypeNormal, eventReasonSignedOutcome, summary)
	if a.Kind == infravirtrigaudiov1beta1.OperationAttemptKindReconfigure {
		// The flags of the answer that accepted the Reconfigure, as recorded
		// on the Sent marker under its task tag (verified before the poll).
		ia.noteResult(nil, &contracts.OperationAnswer{TaskRef: a.TaskRef, Honest: a.Honest, RestartRequired: a.RestartRequired})
	}
	return r.closeAttempt(ctx, vm, ia, a, outcome, poll.v.Operation.GetOutcomeReason(), a.ID)
}

// eventReasonSignedOutcome is the event of a decision a signed host answer
// drove.
const eventReasonSignedOutcome = "OperationSignedOutcome"

// noSignedAnswer leaves a as it is: nothing is concluded, and the host is
// asked again.
func (r *VirtualMachineReconciler) noSignedAnswer(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	a *infravirtrigaudiov1beta1.OperationAttempt, why string) (attemptStep, ctrl.Result, error) {
	msg := fmt.Sprintf("%s attempt %d: %s; nothing is concluded and the host is asked again", a.Kind, a.Attempt, why)
	k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonOperationAnswerUnverified, msg, vm.Generation)
	r.updateStatus(ctx, vm)
	return stepHeld, ctrl.Result{RequeueAfter: blockedRetryMin}, nil
}

// holdPredecessor holds a, an attempt under a predecessor enrolment that the
// host answered state about: never definite, never re-sent.
func (r *VirtualMachineReconciler) holdPredecessor(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	a *infravirtrigaudiov1beta1.OperationAttempt, state string) (attemptStep, ctrl.Result, error) {
	return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationOutcomeUnknown, fmt.Sprintf(
		"%s attempt %d was sent under an earlier enrolment of its host, which answers %s about it: that is not definite after a "+
			"re-enrolment, and the attempt is never re-sent; %s", a.Kind, a.Attempt, state, operationHoldRecovery), operationHoldRecheckInterval), nil
}

// authenticatedHostDeleted asks the host of the Delete p (its verified
// intent) whether the VM's placement it names is deleted. deleted reports a
// verified, READY answer, for a request the manager made now, that the
// placement is DELETED, bound to the intent's VM UID and binding and signed
// by the host the Delete was sent to under its current enrolment; sameOp
// that it names p's op_id as the Delete that deleted it. NO_RECORD, another
// binding, an unsigned exists=false or NotFound are never "deleted".
func (r *VirtualMachineReconciler) authenticatedHostDeleted(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	p *contracts.PreparedOperation) (deleted, sameOp bool, read contracts.SignedRead, v hostidentity.VerifiedAnswer) {
	id := p.Identity
	hr, err := r.HostAnswers.begin(ctx, ia.providerCR, id.HostID)
	if err != nil {
		return false, false, read, v
	}
	ref := contracts.VMRef{ID: vm.Status.ID, HostID: id.HostID, Owner: vmOwnerIdentity(vm)}
	ref.Owner.UID = id.VMUID
	_, read, rerr := ia.reader.VMAnswer(ctx, contracts.VMAnswerQuery{VM: ref, BindingID: id.BindingID, ContinuitySeq: hr.snapshot.ContinuitySeq()})
	if rerr != nil && read.Answer == nil {
		return false, false, read, v
	}
	bound := func(v hostidentity.VerifiedAnswer) bool {
		return v.Ready() && v.VM.GetPresence() == providerv1.VMPresence_VM_PRESENCE_DELETED && v.VM.GetVmUid() == id.VMUID &&
			bytes.Equal(v.VM.GetBindingId(), id.BindingID)
	}
	v, verdict := r.HostAnswers.finish(ctx, ia.providerCR, id.HostID, hr, hostidentity.BodyTypeVMAnswer, read, bound)
	if verdict != answerVerified || !bound(v) {
		return false, false, read, v
	}
	return true, bytes.Equal(v.VM.GetDeleteOpId(), id.OpID), read, v
}

// deleteByVMAnswer settles the Delete attempt a when its host signs that the
// VM's placement is DELETED: by this Delete (the attempt closes Succeeded and
// the finalizer follows), or by another Delete of the same placement on that
// host (the finalizer is released; the attempt is left as it is). done is
// false when the host does not sign that.
func (r *VirtualMachineReconciler) deleteByVMAnswer(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	a *infravirtrigaudiov1beta1.OperationAttempt, p *contracts.PreparedOperation) (attemptStep, ctrl.Result, bool) {
	deleted, sameOp, read, v := r.authenticatedHostDeleted(ctx, vm, ia, p)
	if !deleted {
		return stepWaiting, ctrl.Result{}, false
	}
	summary := auditSignedDecision(ctx, read, v, "Delete confirmed by a deleted placement", a.OpID, fmtID(v.VM.GetDeleteOpId()))
	r.recordEvent(vm, corev1.EventTypeNormal, eventReasonSignedOutcome, summary)
	if !sameOp {
		return stepSucceeded, ctrl.Result{}, true
	}
	step, res, err := r.closeAttempt(ctx, vm, ia, a, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded, "", "")
	if err != nil {
		return stepWaiting, ctrl.Result{RequeueAfter: providerErrorRetryInterval}, true
	}
	return step, res, true
}

// fmtID is the hex form of an identifier.
func fmtID(b []byte) string { return fmt.Sprintf("%x", b) }

// signedDeleteConfirmed reports whether the host of the Delete d (verified
// intent p) confirms it with a fresh signed answer: the Delete's op_id
// COMPLETED, or the VM's placement DELETED.
func (r *VirtualMachineReconciler) signedDeleteConfirmed(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	d *infravirtrigaudiov1beta1.OperationAttempt, p contracts.PreparedOperation) bool {
	poll := r.pollSigned(ctx, vm, ia, &p)
	if poll.verdict == answerVerified && poll.v.Ready() &&
		poll.state() == providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED &&
		poll.v.Operation.GetKind() == int32(providerv1.OperationKind_OPERATION_KIND_DELETE) {
		auditSignedDecision(ctx, poll.read, poll.v, "Delete release confirmed", d.OpID)
		return true
	}
	if poll.verdict == answerRegressed {
		return false
	}
	deleted, _, read, v := r.authenticatedHostDeleted(ctx, vm, ia, &p)
	if deleted {
		auditSignedDecision(ctx, read, v, "Delete release confirmed by a deleted placement", d.OpID, fmtID(v.VM.GetDeleteOpId()))
	}
	return deleted
}

// signedNotPerformed reports whether the host of the latest Create c signs
// again, for a request the manager makes now, that c was not performed:
// FAILED_CLEAN, or RETIRED under the host's current enrolment, from c's
// retained intent. A missing or failing intent, or any other answer, is not.
func (r *VirtualMachineReconciler) signedNotPerformed(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	c *infravirtrigaudiov1beta1.OperationAttempt) bool {
	if c.OpID == "" {
		return false
	}
	p, found, err := r.loadIntent(ctx, vm, c.OpID, ia.providerCR.Namespace)
	if err != nil || !found {
		return false
	}
	saved := ia.deleteRef
	ia.deleteRef = nil // a Create's intent names no Delete target
	cerr := r.checkIntent(ctx, vm, ia, c, p)
	ia.deleteRef = saved
	if cerr != nil {
		return false
	}
	poll := r.pollSigned(ctx, vm, ia, &p)
	if poll.verdict != answerVerified || !poll.v.Ready() {
		return false
	}
	switch poll.state() {
	case providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_CLEAN:
	case providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED:
		if poll.predecessor() {
			return false
		}
	default:
		return false
	}
	auditSignedDecision(ctx, poll.read, poll.v, "release without a Delete: the latest Create was not performed", c.OpID)
	return true
}
