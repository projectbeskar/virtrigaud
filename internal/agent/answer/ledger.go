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

package answer

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/attest"
	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// The reasons a challenge is not issued (ChallengeAnswer.not_issued_reason).
// An agent whose store has no active enrolment matching its certificate
// signs no answer at all, so it never says "not_enrolled".
const (
	// NotIssuedAlreadyAttested: an ATTEST challenge was asked of an attested
	// agent.
	NotIssuedAlreadyAttested = "already_attested"
	// NotIssuedNotAttested: a REATTEST challenge was asked of an agent that
	// awaits its first attestation.
	NotIssuedNotAttested = "not_attested"
	// NotIssuedQuarantined: the store is quarantined.
	NotIssuedQuarantined = "quarantined"
)

// notIssuedReasons is the closed set of not_issued_reason values this agent
// signs.
var notIssuedReasons = []string{NotIssuedAlreadyAttested, NotIssuedNotAttested, NotIssuedQuarantined}

// The outcome reasons of an authority answer this agent signs
// (AuthorityAnswer.outcome_reason), and the ledger loss reasons of a
// quarantine an attestation records.
const (
	// ReasonGrantInvalid: the grant did not verify.
	ReasonGrantInvalid = "grant_invalid"
	// ReasonChallengeUnknown: the grant names no challenge this agent holds.
	ReasonChallengeUnknown = "challenge_unknown"
	// ReasonChallengeExpired: the grant names a challenge whose validity
	// passed.
	ReasonChallengeExpired = "challenge_expired"
	// ReasonNotAttested: a re-attestation reached an agent that awaits its
	// first attestation.
	ReasonNotAttested = "not_attested"
	// ReasonAlreadyAttested: a first attestation reached an attested agent.
	ReasonAlreadyAttested = "already_attested"
	// ReasonSessionReplaced: an applied re-attestation replaced the
	// session.
	ReasonSessionReplaced = "session_replaced"
	// ReasonStoreBusy: the store could not be read; nothing changed.
	ReasonStoreBusy = "store_busy"
	// ReasonStaleAttestation: a ledger mark or a retirement named another
	// attestation session than the agent's current one; nothing changed.
	ReasonStaleAttestation = "stale_attestation"
	// ReasonOpKnown: a retirement named an op_id the store holds a record
	// or tombstone of with other identity fields; nothing changed.
	ReasonOpKnown = "op_known"
	// ReasonOpTraceFound: a retirement found a trace of the operation that
	// no record explains; the store is quarantined (unknown_op_trace).
	ReasonOpTraceFound = "op_trace_found"
	// ReasonEnrolmentNotActive: a retirement named an enrolment that is
	// not the store's active one; nothing changed.
	ReasonEnrolmentNotActive = "enrolment_not_active"
	// ReasonTraceScanUnavailable: the agent's executor cannot scan its
	// objects for a trace of the operation, so it retires nothing; asking
	// again does not change that.
	ReasonTraceScanUnavailable = "trace_scan_unavailable"
)

// outcomeReasons is the closed set of outcome_reason values this agent signs.
var outcomeReasons = []string{ReasonGrantInvalid, ReasonChallengeUnknown, ReasonChallengeExpired, ReasonNotAttested,
	ReasonAlreadyAttested, ReasonSessionReplaced, ReasonStoreBusy, ReasonStaleAttestation, ReasonOpKnown, ReasonOpTraceFound,
	ReasonEnrolmentNotActive, ReasonTraceScanUnavailable, string(journal.LostLedgerStoreMismatch),
	string(journal.LostLedgerAhead), string(journal.LostLedgerMismatch), string(journal.LostLedgerUnverifiable)}

// ledgerAware reports whether h answers a request of the host ledger
// protocol: one that asked for a ledger version, or a host authority
// request.
func ledgerAware(h Host) bool { return h.LedgerRequested >= 1 || h.Authority }

// attested reports whether the answer may name an attestation session: the
// agent is attested and its store is not quarantined (an attestation does
// not survive a loss).
func attested(h Host) bool {
	return h.Attested && len(h.Attestation.Session) == idLen && h.State != providerv1.AgentState_AGENT_STATE_QUARANTINED
}

// stateReason is the state reason the answer for h carries: a quarantine an
// attestation recorded keeps its own reason only for a ledger-aware request;
// any other request sees rolled_back, a reason every manager knows.
func stateReason(h Host) string {
	if journal.IsLedgerReason(journal.LostReason(h.StateReason)) && !ledgerAware(h) && !guard.Off(guard.LedgerReasonsGated) {
		return string(journal.LostRolledBack)
	}
	return h.StateReason
}

// ledgerState is the HostLedgerState of h for a request that asked for
// ledger version requested (a host authority answer passes 1).
func ledgerState(h Host, requested uint32) *providerv1.HostLedgerState {
	st := &providerv1.HostLedgerState{
		RequestedVersion:         requested,
		Version:                  max(min(requested, LedgerVersion), 1),
		State:                    providerv1.LedgerSessionState_LEDGER_SESSION_STATE_AWAITING_ATTESTATION,
		StoreOriginEnrolmentId:   bytes.Clone(h.View.OriginEnrolmentID),
		StoreEnrolmentCount:      h.View.EnrolmentActivations,
		PendingReceipts:          min(h.Backlog.PendingReceipts, opstore.MaxReportedPendingReceipts),
		ParkedRequestBytes:       h.Backlog.ParkedRequestBytes,
		ParkedRequestBudgetBytes: h.Backlog.ParkedRequestBudgetBytes,
	}
	if attested(h) {
		st.State = providerv1.LedgerSessionState_LEDGER_SESSION_STATE_ATTESTED
		st.AttestationId = bytes.Clone(h.Attestation.Session)
		st.Mark = ledgerPosition(h.Attestation.Mark)
	}
	if rec := h.View.Attested; st.Version >= hostidentity.StoreAttestedLedgerVersion && rec.EverAttested &&
		!guard.Off(guard.AttestRecordReported) {
		st.StoreEverAttested = true
		st.StoreFirstAttestedSeq = rec.FirstSeq
		st.StoreFirstAttestedEnrolmentId = bytes.Clone(rec.FirstEnrolmentID)
		st.StoreAttestedRecordSeq = rec.RecordSeq
	}
	return st
}

// hostLedger is the ledger state of a host answer: set exactly when the
// request asked for a ledger version.
func hostLedger(h Host) *providerv1.HostLedgerState {
	if h.LedgerRequested == 0 {
		return nil
	}
	return ledgerState(h, h.LedgerRequested)
}

// ledgerPosition is the wire form of a chain position.
func ledgerPosition(p journal.Position) *providerv1.LedgerPosition {
	return &providerv1.LedgerPosition{StoreId: bytes.Clone(p.StoreID[:]), Seq: p.Seq, Head: bytes.Clone(p.Head[:])}
}

// Challenge is what an answer to an attestation challenge request is made
// from.
type Challenge struct {
	// Host carries the request nonce, the host's identity, trust state,
	// attestation and the store's view.
	Host Host
	// Purpose is the purpose the request asked for.
	Purpose providerv1.ChallengePurpose
	// Nonce is the challenge nonce issued; nil when NotIssued is set.
	Nonce []byte
	// NotIssued says why no challenge is issued (NotIssued* constants).
	NotIssued string
}

// ChallengeAnswer builds the ChallengeAnswer for c. It returns ErrNoAnswer
// when no answer may be made (see HostAnswer), and an error wrapping ErrSign
// when c breaks a field rule: a purpose that is not ATTEST or REATTEST,
// both or neither of a nonce and a reason, a nonce that is not 16 bytes, or
// a reason outside the closed set.
func ChallengeAnswer(c Challenge) (*providerv1.ChallengeAnswer, error) {
	h := c.Host
	h.Authority = true
	ctx, err := answerContext(h)
	if err != nil {
		return nil, err
	}
	switch {
	case c.Purpose != providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST && c.Purpose != providerv1.ChallengePurpose_CHALLENGE_PURPOSE_REATTEST:
		return nil, fmt.Errorf("%w: challenge purpose %v", ErrSign, c.Purpose)
	case (len(c.Nonce) == 0) == (c.NotIssued == ""):
		return nil, fmt.Errorf("%w: a challenge answer carries a nonce or a reason, exactly one", ErrSign)
	case len(c.Nonce) != 0 && len(c.Nonce) != attest.NonceLen:
		return nil, fmt.Errorf("%w: challenge nonce of %d bytes", ErrSign, len(c.Nonce))
	case c.NotIssued != "" && !slices.Contains(notIssuedReasons, c.NotIssued):
		return nil, fmt.Errorf("%w: not-issued reason %q is not in the closed set", ErrSign, c.NotIssued)
	}
	a := &providerv1.ChallengeAnswer{
		Context:         ctx,
		Purpose:         c.Purpose,
		ChallengeNonce:  bytes.Clone(c.Nonce),
		NotIssuedReason: c.NotIssued,
		Ledger:          ledgerState(h, LedgerVersion),
	}
	if len(c.Nonce) != 0 {
		a.ValidForSeconds = uint32(attest.Validity.Seconds())
	}
	return a, nil
}

// Authority is what an answer to a host authority grant is made from.
type Authority struct {
	// Host carries the request nonce, the host's identity, trust state,
	// attestation (after the grant was applied) and the store's view.
	Host Host
	// GrantKind is the grant kind the method carries.
	GrantKind providerv1.GrantKind
	// GrantDigest is grantverify.GrantDigest of the delivered grant; nil
	// when no grant was decoded.
	GrantDigest []byte
	// Outcome is what the agent did.
	Outcome providerv1.AuthorityOutcome
	// Reason qualifies the outcome (Reason* constants or a ledger loss
	// reason); empty when there is nothing to add.
	Reason string
	// OpID is, for a retirement whose grant verified, the op_id it named
	// (16 bytes); nil otherwise.
	OpID []byte
	// OpState is, for a retirement, the operation's state after the grant
	// (RETIRED when it was retired; the record's state when it was not
	// applicable); unspecified otherwise.
	OpState providerv1.OperationRecordState
}

// authorityKinds is the closed set of grant kinds an authority answer names.
var authorityKinds = []providerv1.GrantKind{providerv1.GrantKind_GRANT_KIND_ATTEST,
	providerv1.GrantKind_GRANT_KIND_LEDGER_MARK, providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN}

// AuthorityAnswer builds the AuthorityAnswer for a. It returns ErrNoAnswer
// when no answer may be made (see HostAnswer), and an error wrapping ErrSign
// when a breaks a field rule: a grant kind outside the authority kinds, an
// unspecified or unknown outcome, a reason outside the closed set, or a
// grant digest that is not 32 bytes. The mark is the attestation's for an
// attestation or ledger mark answer of an attested agent.
func AuthorityAnswer(a Authority) (*providerv1.AuthorityAnswer, error) {
	h := a.Host
	h.Authority = true
	ctx, err := answerContext(h)
	if err != nil {
		return nil, err
	}
	switch {
	case !slices.Contains(authorityKinds, a.GrantKind):
		return nil, fmt.Errorf("%w: grant kind %v is not an authority grant", ErrSign, a.GrantKind)
	case a.Outcome == providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_UNSPECIFIED || providerv1.AuthorityOutcome_name[int32(a.Outcome)] == "":
		return nil, fmt.Errorf("%w: authority outcome %v", ErrSign, a.Outcome)
	case a.Reason != "" && !slices.Contains(outcomeReasons, a.Reason):
		return nil, fmt.Errorf("%w: outcome reason %q is not in the closed set", ErrSign, a.Reason)
	case len(a.GrantDigest) != 0 && len(a.GrantDigest) != grantverify.DigestLen:
		return nil, fmt.Errorf("%w: grant digest of %d bytes", ErrSign, len(a.GrantDigest))
	case a.GrantKind != providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN &&
		(len(a.OpID) != 0 || a.OpState != providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNSPECIFIED):
		return nil, fmt.Errorf("%w: an operation in an authority answer that is not a retirement", ErrSign)
	case len(a.OpID) != 0 && len(a.OpID) != idLen:
		return nil, fmt.Errorf("%w: op_id of %d bytes", ErrSign, len(a.OpID))
	case retireTookEffect(a.Outcome) && a.GrantKind == providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN &&
		(len(a.OpID) == 0 || a.OpState != providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED):
		return nil, fmt.Errorf("%w: an applied retirement names no operation or a state other than RETIRED", ErrSign)
	case a.Outcome == providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_NOT_APPLICABLE &&
		(len(a.OpID) == 0 || a.OpState == providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNSPECIFIED):
		return nil, fmt.Errorf("%w: a retirement that is not applicable names no operation or no state", ErrSign)
	}
	out := &providerv1.AuthorityAnswer{
		Context:       ctx,
		GrantKind:     int32(a.GrantKind),
		GrantDigest:   bytes.Clone(a.GrantDigest),
		Outcome:       a.Outcome,
		OutcomeReason: a.Reason,
		OpId:          bytes.Clone(a.OpID),
		OpState:       a.OpState,
	}
	if attested(h) && a.GrantKind != providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN {
		out.Mark = ledgerPosition(h.Attestation.Mark)
	}
	return out, nil
}

// retireTookEffect reports whether o is an outcome by which a retirement
// took effect (APPLIED, or ALREADY_APPLIED by an earlier delivery).
func retireTookEffect(o providerv1.AuthorityOutcome) bool {
	return o == providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED || o == providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_ALREADY_APPLIED
}

// errLedgerField refuses an answer that sets a host ledger field where it
// may not.
var errLedgerField = errors.New("host ledger field")

// checkLedgerFields refuses, before signing, an answer body that sets a host
// ledger field where it may not:
//
//   - the operation answer's ledger fields (12 to 14) only with a ledger
//     version echo of at least 1; the accepted head (32 bytes) and
//     awaiting_request only for an ACCEPTED operation with its accepted
//     sequence number;
//   - in an operation, VM or host answer, the context's attestation id and
//     a host answer's ledger state only with a ledger version echo of at
//     least 1 (the request asked), and the ledger state's requested version
//     equal to the echo;
//   - in a challenge or authority answer, the echo is 0 (those requests
//     carry no ledger version).
func checkLedgerFields(body proto.Message) error {
	if oa, ok := body.(*providerv1.OperationAnswer); ok && !guard.Off(guard.OperationLedgerFields) {
		if err := checkOperationLedgerFields(oa); err != nil {
			return err
		}
	}
	if guard.Off(guard.LedgerFieldsOnlyWhenAsked) {
		return nil
	}
	var ctx *providerv1.AnswerContext
	switch b := body.(type) {
	case *providerv1.ChallengeAnswer:
		if b.GetContext().GetLedgerVersionEcho() != 0 {
			return fmt.Errorf("%w: a challenge answer echoes a ledger version", errLedgerField)
		}
		return checkStoreAttestation(b.GetLedger())
	case *providerv1.AuthorityAnswer:
		if b.GetContext().GetLedgerVersionEcho() != 0 {
			return fmt.Errorf("%w: an authority answer echoes a ledger version", errLedgerField)
		}
		return nil
	case *providerv1.HostAnswer:
		ctx = b.GetContext()
		if l := b.GetLedger(); l != nil && (ctx.GetLedgerVersionEcho() == 0 || l.GetRequestedVersion() != ctx.GetLedgerVersionEcho()) {
			return fmt.Errorf("%w: a host answer carries a ledger state the request did not ask for", errLedgerField)
		}
		if l := b.GetLedger(); l != nil {
			if err := checkStoreAttestation(l); err != nil {
				return err
			}
		}
	case *providerv1.OperationAnswer:
		ctx = b.GetContext()
	case *providerv1.VMAnswer:
		ctx = b.GetContext()
	}
	if len(ctx.GetAttestationId()) > 0 && ctx.GetLedgerVersionEcho() == 0 {
		return fmt.Errorf("%w: an attestation id in an answer to a request that did not ask for it", errLedgerField)
	}
	if journal.IsLedgerReason(journal.LostReason(ctx.GetStateReason())) && ctx.GetLedgerVersionEcho() == 0 &&
		!guard.Off(guard.LedgerReasonsGated) {
		return fmt.Errorf("%w: a ledger loss reason in an answer to a request that did not ask for it", errLedgerField)
	}
	return nil
}

// checkStoreAttestation applies the field rules of the store's attestation
// record in a ledger state (hostidentity.StoreAttestationOf, the manager's
// rules), so that the agent never signs a state the manager refuses.
func checkStoreAttestation(l *providerv1.HostLedgerState) error {
	if _, err := hostidentity.StoreAttestationOf(l); err != nil {
		return fmt.Errorf("%w: %w", errLedgerField, err)
	}
	return nil
}

// checkOperationLedgerFields applies the rules of an operation answer's
// ledger fields (12 to 14).
func checkOperationLedgerFields(oa *providerv1.OperationAnswer) error {
	set := oa.GetAcceptedSeq() != 0 || len(oa.GetAcceptedHead()) > 0 || oa.GetAwaitingRequest()
	accepted := oa.GetState() == providerv1.OperationRecordState_OPERATION_RECORD_STATE_ACCEPTED
	switch {
	case !set:
		return nil
	case oa.GetContext().GetLedgerVersionEcho() == 0:
		return fmt.Errorf("%w: an operation answer's ledger fields in an answer to a request that did not ask for them", errLedgerField)
	case len(oa.GetAcceptedHead()) > 0 && (len(oa.GetAcceptedHead()) != headLen || oa.GetAcceptedSeq() == 0 || !accepted):
		return fmt.Errorf("%w: an accepted head without an accepted operation and its sequence number", errLedgerField)
	case oa.GetAwaitingRequest() && (oa.GetAcceptedSeq() == 0 || !accepted):
		return fmt.Errorf("%w: awaiting_request for an operation that is not accepted", errLedgerField)
	}
	return nil
}

// headLen is the length of a chain head.
const headLen = 32
