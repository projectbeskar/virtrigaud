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

// Retire-if-unknown and the frozen-predecessor closure (development builds
// only: a manager with host ledger keys, a host its ledger drives).
//
// A host that signs, in its ledger's adopted session, that it holds no
// record of an attempt's operation under its active enrolment is asked to
// retire it: the manager signs a retire-if-unknown grant naming the
// operation's identity and that session, and the agent commits a RETIRED
// tombstone unless it holds a record or a trace of the operation. Once the
// agent answers that it retired it, the attempt closes Retired
// (never_received) and a later delivery of the op_id can never run; a new
// attempt follows as after any definite not-performed outcome.
//
// The manager asks only when every precondition holds; any doubt holds the
// attempt or asks the host again, never closes it:
//
//  1. the attempt is in flight (Prepared, Sent or Accepted) and its marker
//     holds no receipt that counts as ledgered; a recorded receipt whose
//     tag does not verify holds the attempt;
//  2. a verified answer from the host's READY agent, in the ledger's
//     adopted session (the ledger Attested in it, bound, not archived, of
//     the store the answer names), signs NO_RECORD for the operation under
//     the active enrolment;
//  3. the attempt is continuous with that ledger: the store and generation
//     covered by its authenticated intent are the ledger's, and its
//     enrolment is one of the generation's. An attempt prepared without a
//     ledger (generation 0), or under another store or generation, is never
//     retired.
//
// The grant is signed in one place only (sendRetire). Its answer must name
// the grant sent; APPLIED, or ALREADY_APPLIED with the operation RETIRED,
// closes the attempt; NOT_APPLICABLE (the host holds a record now) asks
// again; a stale session asks the host again from its next answer; a
// quarantine holds the host and the VM; any other refusal asks again.
//
// A re-send and a retirement never both follow one answer: a marker whose
// receipt counts as ledgered is re-sent with expect_known (the agent then
// refuses an op_id it has no record of instead of running it), and is never
// retired; an attempt with no ledgered receipt that is continuous with the
// ledger is retired, never re-sent; an attempt that is not continuous with
// the ledger is never retired, and keeps the re-send rules it had before.
//
// The frozen-predecessor closure: a verified answer in the adopted session
// that the store, which still holds the records of the attempt's earlier
// (frozen) enrolment, has no record of the operation, closes the attempt
// Retired (unknown_to_frozen_enrolment) without a grant, under the same
// receipt and continuity preconditions, the predecessor being one of the
// generation's enrolments. A frozen enrolment admits no new operation, and
// a lost record whose acceptance the ledger covered would have failed the
// attestation. A predecessor's RETIRED stays non-definite: a freeze may
// retire a record whose step was lost.
//
// Nothing here logs or records a key, a tag, a MAC or a session id.

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Outcome reasons of attempts the host ledger closes Retired.
const (
	// retiredNeverReceived: the host retired the operation it never
	// received (a retire-if-unknown grant).
	retiredNeverReceived = "never_received"
	// retiredUnknownToFrozenEnrolment: the host holds the records of the
	// attempt's frozen enrolment and none of the operation.
	retiredUnknownToFrozenEnrolment = "unknown_to_frozen_enrolment"
	// outcomeStoreLost is the reason of the outcome-unknown hold of an
	// attempt prepared under a store whose ledger a new store archived.
	outcomeStoreLost = "store_lost"
)

// Hold messages of the host ledger's retirement.
const (
	// msgReceiptUnbound: the attempt's recorded receipt does not verify.
	msgReceiptUnbound = "the attempt's recorded receipt does not verify against the attempt (its status was edited or restored), " +
		"so it is neither retired nor re-sent; "
	// msgRetireQuarantined: the agent quarantined its store on the
	// retirement.
	msgRetireQuarantined = "the host's agent quarantined its store when asked to retire the attempt's operation (it found a " +
		"trace or a witness of an operation it holds no record of): nothing it answers is definite; "
)

// retireVerdict is what the retirement preconditions say about an attempt.
type retireVerdict int

const (
	// retireResend: the attempt is not one the ledger retires (its receipt
	// counts as ledgered, or it is not continuous with the ledger); the
	// re-send rules apply.
	retireResend retireVerdict = iota
	// retireHoldReceipt: the attempt's recorded receipt does not verify;
	// held, never retired nor re-sent.
	retireHoldReceipt
	// retireAskAgain: the answer is not evidence the ledger accepts (not
	// READY, not in the adopted session, the ledger not readable or not
	// Attested); the host is asked again.
	retireAskAgain
	// retireAllowed: every precondition holds.
	retireAllowed
)

// retireEvidence is what a retirement decision reads: the attempt, whether
// its recorded receipt verifies, the attempt's authenticated intent, the
// verified answer about its operation and the store's ledger read for it.
type retireEvidence struct {
	attempt      *infravirtrigaudiov1beta1.OperationAttempt
	receiptBound bool
	prepared     contracts.PreparedOperation
	answer       hostidentity.VerifiedAnswer
	ledger       *HostLedgerRecord
	// frozen is true for the frozen-predecessor closure (the answer is
	// about an enrolment other than the host's active one).
	frozen bool
}

// attemptInFlight reports whether p is a phase a retirement may close.
func attemptInFlight(p infravirtrigaudiov1beta1.OperationAttemptPhase) bool {
	switch p {
	case infravirtrigaudiov1beta1.OperationAttemptPhasePrepared, infravirtrigaudiov1beta1.OperationAttemptPhaseSent,
		infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted:
		return true
	}
	return false
}

// decideRetire applies the retirement preconditions (see the file comment)
// to e, in order, and returns the verdict and, for any verdict but
// retireAllowed, why.
func decideRetire(e retireEvidence) (retireVerdict, string) {
	a, v, rec := e.attempt, e.answer, e.ledger
	op := v.Operation
	switch {
	case a == nil || !attemptInFlight(a.Phase):
		return retireAskAgain, "the attempt is not in flight"
	case a.Receipt != nil && !e.receiptBound:
		return retireHoldReceipt, "the recorded receipt does not verify"
	case !e.frozen && (len(e.prepared.RequestBytes) == 0 || len(e.prepared.DigestSalt) == 0):
		return retireAskAgain, "the attempt's intent does not carry its request (its digest cannot be named)"
	case op == nil || !v.Ready() || op.GetState() != providerv1.OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD:
		return retireAskAgain, "no verified NO_RECORD from a READY agent"
	case !bytes.Equal(op.GetOpId(), e.prepared.Identity.OpID) || !bytes.Equal(op.GetOpEnrolmentId(), e.prepared.Identity.EnrolmentID):
		return retireAskAgain, "the answer is about another operation"
	case e.frozen == bytes.Equal(op.GetOpEnrolmentId(), v.Context.GetEnrolmentId()):
		return retireAskAgain, "the answer's enrolment is not the one the decision is for"
	case rec == nil || rec.Archived || rec.AttestationState != infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested ||
		rec.StoreID != v.Position.StoreID:
		return retireAskAgain, "the store's ledger is not Attested, bound and current"
	}
	if s, ok := sessionOf(v.Context); !ok || s != rec.AttestationID {
		return retireAskAgain, "the answer is not from the ledger's adopted session"
	}
	if a.Receipt != nil && receiptLedgered(rec, a.Receipt) {
		return retireResend, "the recorded receipt counts as ledgered"
	}
	if !ledgerContinuous(e.prepared, rec) {
		return retireResend, "the attempt is not continuous with the store's ledger"
	}
	return retireAllowed, ""
}

// attemptDecision is the step, requeue and error a decision on an attempt
// returns to the attempt driver.
type attemptDecision struct {
	step attemptStep
	res  ctrl.Result
	err  error
}

// decided wraps the results of an attempt driver step.
func decided(step attemptStep, res ctrl.Result, err error) attemptDecision {
	return attemptDecision{step: step, res: res, err: err}
}

// held is a hold of the attempt with requeue res.
func held(res ctrl.Result) attemptDecision { return attemptDecision{step: stepHeld, res: res} }

// retireUnknown runs the retirement of attempt a (prepared as p) on the
// verified NO_RECORD of poll, from a host the ledger drives. done is false
// when the attempt is not one the ledger retires, and the re-send rules
// apply.
func (r *VirtualMachineReconciler) retireUnknown(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	a *infravirtrigaudiov1beta1.OperationAttempt, p contracts.PreparedOperation, poll opPoll) (d attemptDecision, done bool) {
	logger := log.FromContext(ctx).WithValues("host", a.HostID)
	rec, err := r.HostAnswers.Ledger.LoadBound(ctx, ia.providerCR, a.HostID, poll.v.Position.StoreID)
	if err != nil {
		logger.Info("The store's ledger cannot be read; the attempt is not retired now", "error", err.Error())
		return decided(r.noSignedAnswer(ctx, vm, a, "the host's ledger cannot be read to decide a retirement")), true
	}
	verdict, why := decideRetire(retireEvidence{attempt: a, receiptBound: r.receiptBound(vm, a), prepared: p, answer: poll.v, ledger: rec})
	switch verdict {
	case retireResend:
		logger.V(1).Info("The attempt is not one the ledger retires; the re-send rules apply", "why", why)
		return attemptDecision{}, false
	case retireHoldReceipt:
		return held(r.holdOperation(ctx, vm, k8s.ReasonOperationStateLost, msgReceiptUnbound+operationHoldRecovery,
			operationHoldRecheckInterval)), true
	case retireAskAgain:
		return decided(r.noSignedAnswer(ctx, vm, a, "a retirement is not decided now ("+why+")")), true
	}
	auth, ok := ia.reader.(contracts.HostAuthorityCaller)
	if !ok {
		return decided(r.noSignedAnswer(ctx, vm, a, "the Provider's client cannot ask the host to retire the operation")), true
	}
	session, _ := sessionOf(poll.v.Context)
	read, err := r.HostAnswers.sendRetire(ctx, auth, rec, p, session)
	if read.Answer == nil {
		logger.Info("No signed answer to the retirement", "error", errString(err))
		return decided(r.noSignedAnswer(ctx, vm, a, "the host gave no signed answer to its retirement")), true
	}
	return r.retireAnswer(ctx, vm, ia, a, p, read, session), true
}

// errNotRetirable: sendRetire's refusal of a ledger record or an attempt it
// may not sign a retirement for.
var errNotRetirable = errors.New("the attempt is not continuous with an Attested, current ledger in this session")

// sendRetire is the only place the manager has a retire-if-unknown grant
// signed. It signs only for an attempt prepared as p that is continuous
// with rec, a ledger record read bound and tag-verified, Attested in
// session, not archived; the grant names p's identity and intent digest and
// the session.
func (h *HostAnswers) sendRetire(ctx context.Context, auth contracts.HostAuthorityCaller, rec *HostLedgerRecord,
	p contracts.PreparedOperation, session [16]byte) (contracts.AuthorityRead, error) {
	if h.Ledger == nil || rec == nil || rec.Archived || rec.AttestationState != infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested ||
		rec.AttestationID != session || session == ([16]byte{}) || !ledgerContinuous(p, rec) || len(p.RequestBytes) == 0 ||
		len(p.DigestSalt) == 0 {
		return contracts.AuthorityRead{}, errNotRetirable
	}
	wire, ok := wireKinds[p.Kind]
	if !ok {
		return contracts.AuthorityRead{}, fmt.Errorf("%w: kind %s", errNotRetirable, p.Kind)
	}
	return auth.RetireIfUnknown(ctx, contracts.RetireOrder{Identity: p.Identity, Kind: wire,
		IntentDigest: operation.IntentDigest(p.DigestSalt, p.RequestBytes), AttestationID: bytes.Clone(session[:])})
}

// retireAnswer acts on the host's answer to the retirement of attempt a
// (prepared as p) in session.
func (r *VirtualMachineReconciler) retireAnswer(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	a *infravirtrigaudiov1beta1.OperationAttempt, p contracts.PreparedOperation, read contracts.AuthorityRead, session [16]byte) attemptDecision {
	logger := log.FromContext(ctx).WithValues("host", a.HostID)
	h := r.HostAnswers
	hr, err := h.begin(ctx, ia.providerCR, a.HostID)
	if err != nil {
		return decided(r.noSignedAnswer(ctx, vm, a, "the host's trust cannot be read to verify its answer to the retirement"))
	}
	exp := hr.trust.expectation(ia.providerCR, a.HostID, hostidentity.BodyTypeAuthorityAnswer, read.Nonce, r.now())
	exp.GrantKind, exp.GrantDigest = providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN, read.GrantDigest
	va, err := hostidentity.VerifyAnswer(blobOf(read.Answer), exp)
	if err != nil {
		logger.Info("The host's answer to the retirement did not verify; nothing is concluded", "check", err.Error())
		return decided(r.noSignedAnswer(ctx, vm, a, "the host's answer to its retirement did not verify"))
	}
	au := va.Authority
	switch au.GetOutcome() {
	case providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_ALREADY_APPLIED:
		s, ok := sessionOf(va.Context)
		if !ok || s != session || !va.Ready() || !bytes.Equal(au.GetOpId(), p.Identity.OpID) ||
			au.GetOpState() != providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED {
			return decided(r.noSignedAnswer(ctx, vm, a, "the host's answer to its retirement is not about this operation in this session"))
		}
		// The answer decides the attempt. Its position (at or above the
		// tombstone's commit) is first covered by the ledger, so a store
		// later rolled back below the tombstone fails its next attestation
		// instead of letting a late delivery of the op_id run. An answer
		// below the position the manager holds for the store is no answer.
		// (An authority request names no continuity position, so its answer
		// is not put through the store continuity check.)
		if (va.Position.StoreID != hr.snapshot.StoreID || va.Position.Seq < hr.snapshot.Seq) && !mutationOff(guardRetireAnswerPosition) {
			return decided(r.noSignedAnswer(ctx, vm, a, "the host's answer to its retirement is behind its store's known position"))
		}
		if r.operationCrashed(ctx, crashBeforeRetireCover) {
			return attemptDecision{step: stepWaiting, err: errOperationCrashed}
		}
		if err := h.coverRetirement(ctx, ia.providerCR, a.HostID, session, va.Position); err != nil {
			logger.Info("The host's ledger does not cover the retirement; it is asked again", "error", err.Error())
			return decided(r.noSignedAnswer(ctx, vm, a, "the host's ledger does not cover its retirement yet"))
		}
		return r.closeRetired(ctx, vm, ia, a, read.SignedRead, va, retiredNeverReceived, "retired by the host it was never delivered to")
	case providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_NOT_APPLICABLE:
		// The host holds a record of the operation now (a delivery won the
		// race): its own answers decide the attempt.
		return attemptDecision{step: stepWaiting, res: r.operationInProgress(ctx, vm, a, operationPollInterval)}
	case providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_QUARANTINED:
		h.holdQuarantined(ctx, ia.providerCR, a.HostID, va.Position.StoreID)
		return held(r.holdOperation(ctx, vm, k8s.ReasonOperationOutcomeUnknown, msgRetireQuarantined+operationHoldRecovery,
			operationHoldRecheckInterval))
	}
	switch reason := au.GetOutcomeReason(); reason {
	case hostidentity.AuthorityReasonOpTraceFound:
		h.holdQuarantined(ctx, ia.providerCR, a.HostID, va.Position.StoreID)
		return held(r.holdOperation(ctx, vm, k8s.ReasonOperationOutcomeUnknown, msgRetireQuarantined+operationHoldRecovery,
			operationHoldRecheckInterval))
	case hostidentity.AuthorityReasonStaleAttestation:
		// The agent holds another session: the ledger driver attests it
		// again, and the next answer decides.
		h.DriveHostLedger(ctx, ia.providerCR, ia.reader, a.HostID)
		return decided(r.noSignedAnswer(ctx, vm, a, "the host's agent is in another attestation session than the one the retirement named"))
	default:
		return decided(r.noSignedAnswer(ctx, vm, a, fmt.Sprintf("the host refused its retirement (%s)", reason)))
	}
}

// errRetirementNotCovered: the host ledger was not raised to the position
// of a retirement answer.
var errRetirementNotCovered = errors.New("the host ledger does not cover the retirement answer's position")

// coverRetirement raises the host's ledger to pos, the position of an
// applied retirement answer in session (at or above the tombstone's
// commit), and returns an error unless the ledger, in that session, now
// covers it. A store later rolled back below the tombstone then fails its
// next attestation, so a late delivery of the op_id cannot run while the
// manager records the attempt Retired.
func (h *HostAnswers) coverRetirement(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, session [16]byte,
	pos hostidentity.ChainPosition) error {
	if mutationOff(guardRetireLedgerRaise) {
		return nil
	}
	raised, err := h.Ledger.Raise(ctx, provider, hostID, pos.StoreID, []HostLedgerRaise{{Session: session,
		Position: HostLedgerPosition{Seq: pos.Seq, Head: pos.Head}}})
	switch {
	case err != nil:
		return fmt.Errorf("%w: %w", errRetirementNotCovered, err)
	case raised == nil || raised.AttestationID != session || raised.Position.Seq < pos.Seq:
		return errRetirementNotCovered
	}
	return nil
}

// closeRetired closes attempt a Retired with reason on the verified answer
// v of read, after an audit record of the signed decision.
func (r *VirtualMachineReconciler) closeRetired(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	a *infravirtrigaudiov1beta1.OperationAttempt, read contracts.SignedRead, v hostidentity.VerifiedAnswer, reason, decision string) attemptDecision {
	if r.operationCrashed(ctx, crashAfterReply) {
		return attemptDecision{step: stepWaiting, err: errOperationCrashed}
	}
	r.HostAnswers.setRecordAnchored(ia.providerCR, a.HostID, a.OpID, false)
	summary := auditSignedDecision(ctx, read, v, fmt.Sprintf("%s attempt %d closed Retired (%s): %s", a.Kind, a.Attempt, reason, decision), a.OpID)
	r.recordEvent(vm, corev1.EventTypeNormal, eventReasonSignedOutcome, summary)
	return decided(r.closeAttempt(ctx, vm, ia, a, infravirtrigaudiov1beta1.OperationAttemptOutcomeRetired, reason, a.ID))
}

// frozenClosure closes attempt a (prepared as p, under an enrolment other
// than the host's active one) Retired (unknown_to_frozen_enrolment) on the
// verified NO_RECORD of poll, without a grant, when the closure's
// preconditions hold (see the file comment); done is false otherwise, and
// the attempt is held as every predecessor attempt is.
func (r *VirtualMachineReconciler) frozenClosure(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	a *infravirtrigaudiov1beta1.OperationAttempt, p contracts.PreparedOperation, poll opPoll) (d attemptDecision, done bool) {
	rec, err := r.HostAnswers.Ledger.LoadBound(ctx, ia.providerCR, a.HostID, poll.v.Position.StoreID)
	if err != nil {
		return attemptDecision{}, false
	}
	verdict, why := decideRetire(retireEvidence{attempt: a, receiptBound: r.receiptBound(vm, a), prepared: p, answer: poll.v, ledger: rec,
		frozen: true})
	if verdict == retireHoldReceipt {
		return held(r.holdOperation(ctx, vm, k8s.ReasonOperationStateLost, msgReceiptUnbound+operationHoldRecovery,
			operationHoldRecheckInterval)), true
	}
	if verdict != retireAllowed {
		log.FromContext(ctx).V(1).Info("The frozen enrolment's answer does not close the attempt", "host", a.HostID, "why", why)
		return attemptDecision{}, false
	}
	return r.closeRetired(ctx, vm, ia, a, poll.read, poll.v, retiredUnknownToFrozenEnrolment,
		"the host holds the records of the attempt's frozen enrolment and none of the operation"), true
}

// storeLost holds attempt a (prepared as p) as outcome unknown when the
// store its authenticated intent was prepared under is one whose ledger a
// new store archived (the host's store was replaced): what the operation
// did in the lost store can never be learned, and it is neither re-sent nor
// retired. done is false when that is not the case.
func (r *VirtualMachineReconciler) storeLost(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	a *infravirtrigaudiov1beta1.OperationAttempt, p contracts.PreparedOperation) (d attemptDecision, done bool) {
	l := r.HostAnswers.Ledger
	if l == nil || len(p.LedgerStoreID) != len([16]byte{}) || p.LedgerGeneration == 0 {
		return attemptDecision{}, false
	}
	rec, err := l.Load(ctx, ia.providerCR, a.HostID, [16]byte(p.LedgerStoreID))
	if err != nil || !rec.Archived {
		return attemptDecision{}, false
	}
	return held(r.holdOperation(ctx, vm, k8s.ReasonOperationOutcomeUnknown, fmt.Sprintf(
		"%s attempt %d was prepared under a store of host %s that a new store replaced (%s): what it did there can never be "+
			"learned, so it is neither re-sent nor retired; %s", a.Kind, a.Attempt, a.HostID, outcomeStoreLost, operationHoldRecovery),
		operationHoldRecheckInterval)), true
}
