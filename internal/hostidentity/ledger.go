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

package hostidentity

// The manager's rules for the host ledger fields of a signed answer
// (development builds): the ledger state and attestation session of a host
// answer, the acceptance position of an operation answer, the challenge and
// authority answers, and operation receipts. Each rule mirrors what the
// agent applies before it signs (internal/agent/answer), so a field the
// agent would never sign is "no answer" here.

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// LedgerVersion is the highest host ledger version this verifier
// implements (HostLedgerState.version, and the ledger_version a manager
// sends).
const LedgerVersion = StoreAttestedLedgerVersion

// MaxChallengeValiditySeconds bounds a challenge's valid_for_seconds.
const MaxChallengeValiditySeconds = 300

// The reasons a challenge is not issued (ChallengeAnswer.not_issued_reason).
const (
	// ChallengeNotIssuedAlreadyAttested: ATTEST asked of an attested agent.
	ChallengeNotIssuedAlreadyAttested = "already_attested"
	// ChallengeNotIssuedNotAttested: REATTEST asked of an agent awaiting its
	// first attestation.
	ChallengeNotIssuedNotAttested = "not_attested"
	// ChallengeNotIssuedQuarantined: the agent's store is quarantined.
	ChallengeNotIssuedQuarantined = "quarantined"
	// ChallengeNotIssuedNotEnrolled: the agent holds no active enrolment.
	ChallengeNotIssuedNotEnrolled = "not_enrolled"
)

// challengeNotIssued is the closed set of not_issued_reason values.
var challengeNotIssued = []string{ChallengeNotIssuedAlreadyAttested, ChallengeNotIssuedNotAttested,
	ChallengeNotIssuedQuarantined, ChallengeNotIssuedNotEnrolled}

// Outcome reasons of an authority answer the manager acts on.
const (
	// AuthorityReasonChallengeUnknown: the grant names no challenge the
	// agent holds.
	AuthorityReasonChallengeUnknown = "challenge_unknown"
	// AuthorityReasonChallengeExpired: the grant names an expired challenge.
	AuthorityReasonChallengeExpired = "challenge_expired"
	// AuthorityReasonStaleAttestation: a ledger mark or a retirement named
	// another session.
	AuthorityReasonStaleAttestation = "stale_attestation"
	// AuthorityReasonOpTraceFound: a retirement found a trace of the
	// operation that no record explains; the agent quarantined its store.
	AuthorityReasonOpTraceFound = "op_trace_found"
	// AuthorityReasonOpKnown: a retirement named an op_id the store holds
	// with other identity fields.
	AuthorityReasonOpKnown = "op_known"
	// AuthorityReasonEnrolmentNotActive: a retirement named an enrolment
	// that is not the store's active one.
	AuthorityReasonEnrolmentNotActive = "enrolment_not_active"
	// AuthorityReasonTraceScanUnavailable: the agent's executor cannot scan
	// its objects for traces, so it retires nothing.
	AuthorityReasonTraceScanUnavailable = "trace_scan_unavailable"
)

// AuthorityReasons is the closed set of AuthorityAnswer.outcome_reason a
// manager accepts; it equals the set an agent signs.
var AuthorityReasons = []string{"grant_invalid", AuthorityReasonStaleAttestation, AuthorityReasonChallengeUnknown,
	AuthorityReasonChallengeExpired, "not_attested", "already_attested", "session_replaced", AuthorityReasonOpKnown, AuthorityReasonOpTraceFound,
	AuthorityReasonEnrolmentNotActive, AuthorityReasonTraceScanUnavailable, "ledger_mismatch", "ledger_ahead", "ledger_unverifiable", "ledger_store_mismatch", "store_busy"}

// authorityGrantKinds is the closed set of AuthorityAnswer.grant_kind.
var authorityGrantKinds = []providerv1.GrantKind{providerv1.GrantKind_GRANT_KIND_ATTEST,
	providerv1.GrantKind_GRANT_KIND_LEDGER_MARK, providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN}

// ledgerHeadLen is the length of a chain head.
const ledgerHeadLen = sha256.Size

// checkLedgerAnswer applies the host ledger rules to a verified answer whose
// context and position are already checked, and records its ledger state.
func checkLedgerAnswer(out *VerifiedAnswer, exp AnswerExpectation) error {
	c := out.Context
	session := c.GetAttestationId()
	if len(session) != 0 && len(session) != IDLen {
		return refuse("the attestation id is not %d bytes", IDLen)
	}
	if len(session) != 0 && c.GetAgentState() == providerv1.AgentState_AGENT_STATE_QUARANTINED {
		return refuse("a quarantined agent names an attestation session")
	}
	switch {
	case out.Challenge != nil, out.Authority != nil:
		if c.GetLedgerVersionEcho() != 0 {
			return refuse("a host authority answer echoes a ledger version")
		}
	case c.GetLedgerVersionEcho() != exp.LedgerVersion:
		return refuse("the answer does not echo the request's ledger version (stripped or altered in transit)")
	}
	switch {
	case out.Host != nil:
		l := out.Host.GetLedger()
		if l == nil {
			return refuse("a host answer to a request that asked for the ledger state carries none")
		}
		if l.GetRequestedVersion() != exp.LedgerVersion {
			return refuse("the ledger state does not echo the requested ledger version")
		}
		if err := checkLedgerState(l, c, out.Position); err != nil {
			return err
		}
		out.Ledger = l
	case out.Operation != nil:
		if err := checkOperationLedger(out.Operation, out.Position); err != nil {
			return err
		}
	case out.Challenge != nil:
		if err := checkChallengeAnswer(out.Challenge, exp, out.Position); err != nil {
			return err
		}
		out.Ledger = out.Challenge.GetLedger()
	case out.Authority != nil:
		if err := checkAuthorityAnswer(out.Authority, exp, out.Position); err != nil {
			return err
		}
	}
	if out.Ledger != nil {
		a, err := StoreAttestationOf(out.Ledger)
		if err != nil {
			return err
		}
		out.Attestation = a
	}
	return nil
}

// checkLedgerPosition checks a ledger position of the answer's store, at or
// below its chain position.
func checkLedgerPosition(p *providerv1.LedgerPosition, pos ChainPosition) error {
	switch {
	case !bytes.Equal(p.GetStoreId(), pos.StoreID[:]):
		return refuse("a ledger position names another store than the answer's")
	case p.GetSeq() == 0 || p.GetSeq() > pos.Seq:
		return refuse("a ledger position is 0 or above the answer's chain position")
	case len(p.GetHead()) != ledgerHeadLen:
		return refuse("a ledger position's head is not %d bytes", ledgerHeadLen)
	}
	return nil
}

// checkLedgerState checks a HostLedgerState against its answer's context.
func checkLedgerState(l *providerv1.HostLedgerState, c *providerv1.AnswerContext, pos ChainPosition) error {
	attested := l.GetState() == providerv1.LedgerSessionState_LEDGER_SESSION_STATE_ATTESTED
	switch {
	case l.GetRequestedVersion() == 0:
		return refuse("the ledger state echoes no requested version")
	case l.GetVersion() == 0 || l.GetVersion() > l.GetRequestedVersion():
		return refuse("the ledger state's version is 0 or above the requested one")
	case l.GetState() == providerv1.LedgerSessionState_LEDGER_SESSION_STATE_UNSPECIFIED:
		return refuse("the ledger session state is unspecified")
	case attested != (len(l.GetAttestationId()) == IDLen):
		return refuse("the ledger state names an attestation session exactly when attested")
	case !attested && len(l.GetAttestationId()) != 0:
		return refuse("an unattested ledger state names a session")
	case !bytes.Equal(l.GetAttestationId(), c.GetAttestationId()):
		return refuse("the ledger state's session is not the context's")
	case attested != (l.GetMark() != nil):
		return refuse("the ledger state carries a mark exactly when attested")
	case len(l.GetStoreOriginEnrolmentId()) != 0 && len(l.GetStoreOriginEnrolmentId()) != IDLen:
		return refuse("the store's origin enrolment is not %d bytes", IDLen)
	}
	if attested {
		if err := checkLedgerPosition(l.GetMark(), pos); err != nil {
			return err
		}
	}
	return nil
}

// checkOperationLedger applies the rules of an operation answer's ledger
// fields (12 to 14), as the agent does before it signs.
func checkOperationLedger(a *providerv1.OperationAnswer, pos ChainPosition) error {
	accepted := a.GetState() == providerv1.OperationRecordState_OPERATION_RECORD_STATE_ACCEPTED
	switch {
	case len(a.GetAcceptedHead()) > 0 && (len(a.GetAcceptedHead()) != ledgerHeadLen || a.GetAcceptedSeq() == 0 || !accepted):
		return refuse("an accepted head without an accepted operation and its sequence number")
	case a.GetAwaitingRequest() && (a.GetAcceptedSeq() == 0 || !accepted):
		return refuse("awaiting_request for an operation that is not accepted")
	case a.GetAcceptedSeq() > pos.Seq:
		return refuse("the accepted sequence number is above the answer's chain position")
	case a.GetAcceptedSeq() != 0 && !showsRecordState(a.GetState()):
		return refuse("an accepted sequence number without a record")
	}
	return nil
}

// showsRecordState reports whether s is the state of a record.
func showsRecordState(s providerv1.OperationRecordState) bool {
	switch s {
	case providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNSPECIFIED, providerv1.OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD,
		providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNKNOWN_TO_AGENT:
		return false
	}
	return true
}

// checkChallengeAnswer applies the rules of a ChallengeAnswer.
func checkChallengeAnswer(a *providerv1.ChallengeAnswer, exp AnswerExpectation, pos ChainPosition) error {
	nonce, reason := a.GetChallengeNonce(), a.GetNotIssuedReason()
	switch {
	case a.GetPurpose() != exp.ChallengePurpose || exp.ChallengePurpose == providerv1.ChallengePurpose_CHALLENGE_PURPOSE_UNSPECIFIED:
		return refuse("the challenge answer's purpose is not the request's")
	case (len(nonce) == 0) == (reason == ""):
		return refuse("a challenge answer carries a nonce or a reason, exactly one")
	case len(nonce) != 0 && len(nonce) != IDLen:
		return refuse("the challenge nonce is not %d bytes", IDLen)
	case reason != "" && !slices.Contains(challengeNotIssued, reason):
		return refuse("the not-issued reason is outside the closed set")
	case len(nonce) != 0 && (a.GetValidForSeconds() == 0 || a.GetValidForSeconds() > MaxChallengeValiditySeconds):
		return refuse("a challenge's validity is outside 1..%d seconds", MaxChallengeValiditySeconds)
	case len(nonce) == 0 && a.GetValidForSeconds() != 0:
		return refuse("a validity without a challenge")
	case a.GetLedger() == nil:
		return refuse("a challenge answer carries no ledger state")
	}
	return checkLedgerState(a.GetLedger(), a.GetContext(), pos)
}

// checkAuthorityAnswer applies the rules of an AuthorityAnswer.
func checkAuthorityAnswer(a *providerv1.AuthorityAnswer, exp AnswerExpectation, pos ChainPosition) error {
	kind := providerv1.GrantKind(a.GetGrantKind())
	switch {
	case !slices.Contains(authorityGrantKinds, kind) || kind != exp.GrantKind:
		return refuse("the authority answer names another grant kind than the request's")
	case a.GetOutcome() == providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_UNSPECIFIED:
		return refuse("the authority outcome is unspecified")
	case a.GetOutcomeReason() != "" && !slices.Contains(AuthorityReasons, a.GetOutcomeReason()):
		return refuse("the outcome reason is outside the closed set")
	case len(a.GetGrantDigest()) == 0 && a.GetOutcome() != providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED:
		return refuse("an authority answer that names no grant is not a refusal")
	case len(a.GetGrantDigest()) != 0 && (len(exp.GrantDigest) == 0 ||
		subtle.ConstantTimeCompare(a.GetGrantDigest(), exp.GrantDigest) != 1):
		return refuse("the authority answer names another grant than the one sent")
	case kind != providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN && (len(a.GetOpId()) != 0 ||
		a.GetOpState() != providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNSPECIFIED):
		return refuse("an operation in an authority answer that is not a retirement")
	case kind == providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN && a.GetMark() != nil:
		return refuse("a mark in a retirement answer")
	case kind == providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN && len(a.GetOpId()) != 0 && len(a.GetOpId()) != IDLen:
		return refuse("the retirement answer's op_id is not %d bytes", IDLen)
	case kind == providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN && retireApplied(a.GetOutcome()) &&
		(len(a.GetOpId()) != IDLen || a.GetOpState() != providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED):
		return refuse("an applied retirement answer names no operation, or a state other than RETIRED")
	case kind == providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN &&
		a.GetOutcome() == providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_NOT_APPLICABLE &&
		(len(a.GetOpId()) != IDLen || a.GetOpState() == providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNSPECIFIED):
		return refuse("a retirement answer that is not applicable names no operation or no state")
	}
	if a.GetMark() != nil {
		return checkLedgerPosition(a.GetMark(), pos)
	}
	return nil
}

// retireApplied reports whether o is an outcome by which a retirement grant
// took effect (APPLIED, or ALREADY_APPLIED by an earlier delivery).
func retireApplied(o providerv1.AuthorityOutcome) bool {
	return o == providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED || o == providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_ALREADY_APPLIED
}

// ReceiptExpectation is what the manager expects of one operation receipt:
// the host trust of the attempt's host, the attempt's MAC-verified identity
// and the digest of the grant of the delivery the receipt answered.
type ReceiptExpectation struct {
	// Anchors, HostURI, HostName, HostID and Pins are the host's trust (as
	// in AnswerExpectation).
	Anchors  *x509.CertPool
	HostURI  string
	HostName string
	HostID   string
	Pins     []string
	// ProviderUID, InstallID and EnrolmentID are the Provider, the
	// installation and the enrolment the grant named.
	ProviderUID string
	InstallID   []byte
	EnrolmentID []byte
	// OpID, Kind, Attempt, VMUID, BindingID and IntentDigest are the
	// attempt's identity, from its authenticated intent.
	OpID         []byte
	Kind         providerv1.OperationKind
	Attempt      uint64
	VMUID        string
	BindingID    []byte
	IntentDigest []byte
	// GrantDigest is grantverify.GrantDigest of the grant sent on the call
	// the receipt came back from.
	GrantDigest []byte
	// Now is the manager's clock.
	Now time.Time
}

// VerifiedReceipt is an operation receipt that passed VerifyReceipt.
type VerifiedReceipt struct {
	// StoreID is the agent's store.
	StoreID [16]byte
	// Seq and Head are the position at which the agent accepted the
	// operation.
	Seq  uint64
	Head [32]byte
	// AttestationID is the session the agent accepted it in.
	AttestationID [16]byte
}

// VerifyReceipt verifies an operation receipt blob, as VerifyAnswer
// verifies an answer (sizes, no unknown fields, the body type, the signer
// certificate, the signature, a canonical body with no unknown field), and
// then the receipt's rules: version 1; the Provider, host, installation and
// enrolment are exp's and the certificate's record's; the operation is
// exp's attempt (op_id, kind, attempt, VM, binding, intent digest); the
// grant digest is the one of the grant sent on this call; identifiers,
// position and head are well formed. Any failure is "no receipt".
func VerifyReceipt(blob *providerv1.SignedBlob, exp ReceiptExpectation) (VerifiedReceipt, error) {
	if blob == nil {
		return VerifiedReceipt{}, refuse("no receipt")
	}
	switch {
	case blob.GetBodyType() != BodyTypeOperationReceipt:
		return VerifiedReceipt{}, refuse("the receipt's body type is not an operation receipt")
	case len(blob.GetBody()) > MaxAnswerBodyBytes, len(blob.GetSignature()) == 0, len(blob.GetSignature()) > MaxAnswerSignatureBytes,
		len(blob.GetSignerCertificate()) == 0, len(blob.GetSignerCertificate()) > MaxAnswerCertBytes, proto.Size(blob) > MaxAnswerBlobBytes:
		return VerifiedReceipt{}, refuse("receipt size")
	case len(blob.ProtoReflect().GetUnknown()) != 0:
		return VerifiedReceipt{}, refuse("the receipt carries unknown fields")
	}
	ae := AnswerExpectation{Anchors: exp.Anchors, HostURI: exp.HostURI, HostName: exp.HostName, HostID: exp.HostID,
		Pins: exp.Pins, Now: exp.Now}
	pub, rec, _, err := verifySigner(blob.GetSignerCertificate(), ae)
	if err != nil {
		return VerifiedReceipt{}, err
	}
	if err := verifyAnswerSignature(pub, blob); err != nil {
		return VerifiedReceipt{}, err
	}
	r := &providerv1.OperationReceipt{}
	if err := proto.Unmarshal(blob.GetBody(), r); err != nil {
		return VerifiedReceipt{}, refuse("the receipt does not decode")
	}
	if err := checkMessage(r.ProtoReflect()); err != nil {
		return VerifiedReceipt{}, err
	}
	if canon, err := (proto.MarshalOptions{Deterministic: true}).Marshal(r); err != nil || !bytes.Equal(canon, blob.GetBody()) {
		return VerifiedReceipt{}, refuse("the receipt is not canonically encoded")
	}
	eq := func(a, b []byte) bool { return len(a) == len(b) && subtle.ConstantTimeCompare(a, b) == 1 }
	switch {
	case r.GetReceiptVersion() != 1:
		return VerifiedReceipt{}, refuse("receipt version %d", r.GetReceiptVersion())
	case r.GetProviderUid() != exp.ProviderUID || rec.ProviderUID != exp.ProviderUID:
		return VerifiedReceipt{}, refuse("the receipt names another Provider")
	case r.GetHostId() != exp.HostID:
		return VerifiedReceipt{}, refuse("the receipt names another host")
	case !eq(r.GetInstallId(), exp.InstallID) || !eq(rec.InstallID, exp.InstallID):
		return VerifiedReceipt{}, refuse("the receipt names another installation")
	case !eq(r.GetEnrolmentId(), exp.EnrolmentID) || !eq(rec.EnrolmentID, exp.EnrolmentID):
		return VerifiedReceipt{}, refuse("the receipt names another enrolment")
	case len(exp.GrantDigest) == 0 || !eq(r.GetGrantDigest(), exp.GrantDigest):
		return VerifiedReceipt{}, refuse("the receipt is for another grant than the one sent")
	case !eq(r.GetOpId(), exp.OpID) || providerv1.OperationKind(r.GetKind()) != exp.Kind || r.GetAttempt() != exp.Attempt ||
		r.GetVmUid() != exp.VMUID || !eq(r.GetBindingId(), exp.BindingID) || !eq(r.GetIntentDigest(), exp.IntentDigest):
		return VerifiedReceipt{}, refuse("the receipt is about another operation than the attempt")
	case exp.Kind == providerv1.OperationKind_OPERATION_KIND_UNSPECIFIED:
		return VerifiedReceipt{}, refuse("no operation kind expected")
	case len(r.GetAttestationId()) != IDLen || len(r.GetStoreId()) != IDLen || len(r.GetAcceptedHead()) != ledgerHeadLen ||
		r.GetAcceptedSeq() == 0 || bytes.Equal(r.GetStoreId(), make([]byte, IDLen)):
		return VerifiedReceipt{}, refuse("the receipt's session, store or position is malformed")
	}
	out := VerifiedReceipt{Seq: r.GetAcceptedSeq()}
	copy(out.StoreID[:], r.GetStoreId())
	copy(out.Head[:], r.GetAcceptedHead())
	copy(out.AttestationID[:], r.GetAttestationId())
	return out, nil
}

// LedgerEvidence is what one verified, nonce-bound answer says about the
// host's store, for the manager's host ledger: the store, the position, the
// store's origin and attestation record, the agent's attestation session,
// and the host's confirmation of a chain head the request named. Only
// VerifiedAnswer.LedgerEvidence builds a valid one, from an answer that
// VerifyAnswer verified; the zero value is invalid. It carries the request
// nonce, the Provider, host and enrolment it is bound to, and when it
// verified, so a consumer can refuse evidence of another store or request.
type LedgerEvidence struct {
	valid          bool
	bodyType       string
	nonce          []byte
	providerUID    string
	hostID         string
	enrolmentID    []byte
	verifiedAt     time.Time
	position       ChainPosition
	checkpointSeq  uint64
	checkpointHead [32]byte
	state          providerv1.LedgerSessionState
	session        [16]byte
	markSeq        uint64
	markHead       [32]byte
	originEnrol    []byte
	enrolCount     uint32
	attestation    StoreAttestation
	continuitySeq  uint64
	continuityHead [32]byte
	hasContinuity  bool
	agentState     providerv1.AgentState
}

// LedgerEvidence returns the ledger evidence of v. It refuses (ErrAnswer) an
// answer VerifyAnswer did not produce, and one that is not a host answer to
// a request that asked for the ledger state, nor a challenge answer.
func (v VerifiedAnswer) LedgerEvidence() (LedgerEvidence, error) {
	if !v.verified || v.Context == nil {
		return LedgerEvidence{}, refuse("ledger evidence comes only from a verified answer")
	}
	bodyType := ""
	switch {
	case v.Host != nil && v.Ledger != nil:
		bodyType = BodyTypeHostAnswer
	case v.Challenge != nil && v.Ledger != nil:
		bodyType = BodyTypeChallengeAnswer
	default:
		return LedgerEvidence{}, refuse("ledger evidence comes only from a host answer with its ledger state or a challenge answer")
	}
	c := v.Context
	e := LedgerEvidence{valid: true, bodyType: bodyType, nonce: bytes.Clone(v.nonce), providerUID: c.GetProviderUid(),
		hostID: c.GetHostId(), enrolmentID: bytes.Clone(c.GetEnrolmentId()), verifiedAt: v.VerifiedAt, position: v.Position,
		checkpointSeq: c.GetCheckpointSeq(), state: v.Ledger.GetState(),
		originEnrol: bytes.Clone(v.Ledger.GetStoreOriginEnrolmentId()), enrolCount: v.Ledger.GetStoreEnrolmentCount(),
		attestation: v.Attestation, agentState: c.GetAgentState()}
	copy(e.checkpointHead[:], c.GetCheckpointHead())
	copy(e.session[:], v.Ledger.GetAttestationId())
	if m := v.Ledger.GetMark(); m != nil {
		e.markSeq = m.GetSeq()
		copy(e.markHead[:], m.GetHead())
	}
	if v.ContinuitySeq > 0 && len(c.GetContinuityHead()) == ledgerHeadLen {
		e.continuitySeq, e.hasContinuity = v.ContinuitySeq, true
		copy(e.continuityHead[:], c.GetContinuityHead())
	}
	return e, nil
}

// Valid reports whether e was built from a verified answer.
func (e LedgerEvidence) Valid() bool { return e.valid }

// BodyType is the body type of the answer e came from.
func (e LedgerEvidence) BodyType() string { return e.bodyType }

// Nonce is the request nonce of the answer e came from (a copy).
func (e LedgerEvidence) Nonce() []byte { return bytes.Clone(e.nonce) }

// ProviderUID is the Provider the answer was bound to.
func (e LedgerEvidence) ProviderUID() string { return e.providerUID }

// HostID is the host the answer named.
func (e LedgerEvidence) HostID() string { return e.hostID }

// EnrolmentID is the enrolment the answer named (a copy).
func (e LedgerEvidence) EnrolmentID() []byte { return bytes.Clone(e.enrolmentID) }

// VerifiedAt is when the answer verified.
func (e LedgerEvidence) VerifiedAt() time.Time { return e.verifiedAt }

// StoreID is the store the answer's chain position names.
func (e LedgerEvidence) StoreID() [16]byte { return e.position.StoreID }

// Position is the answer's chain position.
func (e LedgerEvidence) Position() ChainPosition { return e.position }

// Checkpoint is the answer's checkpoint (sequence number 0: none).
func (e LedgerEvidence) Checkpoint() (uint64, [32]byte) { return e.checkpointSeq, e.checkpointHead }

// SessionState is the agent's ledger session state.
func (e LedgerEvidence) SessionState() providerv1.LedgerSessionState { return e.state }

// Session is the agent's attestation session (zero when not attested).
func (e LedgerEvidence) Session() [16]byte { return e.session }

// Mark is the agent's ledger mark (sequence number 0 when not attested).
func (e LedgerEvidence) Mark() (uint64, [32]byte) { return e.markSeq, e.markHead }

// Origin is the store-origin evidence: the store's origin enrolment (a copy,
// empty for a store that predates the record) and how many enrolments it
// ever held.
func (e LedgerEvidence) Origin() ([]byte, uint32) { return bytes.Clone(e.originEnrol), e.enrolCount }

// Attestation is the store's attestation record.
func (e LedgerEvidence) Attestation() StoreAttestation { return e.attestation }

// Continuity is the host's confirmation of the chain head at the request's
// continuity_seq; ok is false when the request named none or the host held
// no head there.
func (e LedgerEvidence) Continuity() (seq uint64, head [32]byte, ok bool) {
	return e.continuitySeq, e.continuityHead, e.hasContinuity
}

// AgentState is the agent state the answer carried.
func (e LedgerEvidence) AgentState() providerv1.AgentState { return e.agentState }
