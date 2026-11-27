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

package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/attest"
	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// authorityService serves provider.v1.HostAuthority for this host
// (development builds): Challenge, Attest, LedgerMark and RetireIfUnknown.
//
// Every request is on the strict path: a field or an enum value the agent
// does not know, a target that is not this host, or an answer_nonce that is
// not 16 bytes is refused INVALID_ARGUMENT before anything else. Every
// decision after that is answered with a host-signed answer (refusals
// included), under the per-Provider answer limit; a call above it gets no
// signed answer, but what it applied stays applied.
type authorityService struct {
	providerv1.UnimplementedHostAuthorityServer
	s *Server
}

// checkAuthorityRequest applies the request rules of a host authority call.
func (s *Server) checkAuthorityRequest(req proto.Message, target string, nonce []byte) error {
	if err := grantverify.CheckStrictRequest(req); err != nil {
		return status.Error(codes.InvalidArgument, "the request carries a field or a value this agent does not know")
	}
	if target == "" || len(target) > maxHostIDLen || target != s.opts.Credentials.Load().Identity.HostID() {
		return status.Error(codes.InvalidArgument, "target_host_id must name this host")
	}
	return checkNonce(nonce, true)
}

// Challenge issues an attestation challenge: a fresh single-use nonce of
// the purpose asked, valid for attest.Validity, which an attestation grant
// must name. ATTEST is issued only while the agent awaits its first
// attestation, REATTEST only while it is attested, neither while the store
// is quarantined; otherwise the answer says why none was issued. The answer
// always carries the ledger state, with the store's origin evidence.
func (a *authorityService) Challenge(ctx context.Context, req *providerv1.ChallengeRequest) (*providerv1.ChallengeResponse, error) {
	s := a.s
	if err := s.checkAuthorityRequest(req, req.GetTargetHostId(), req.GetAnswerNonce()); err != nil {
		return nil, err
	}
	purpose := req.GetPurpose()
	if purpose != providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST && purpose != providerv1.ChallengePurpose_CHALLENGE_PURPOSE_REATTEST {
		return nil, status.Error(codes.InvalidArgument, "purpose must be ATTEST or REATTEST")
	}
	_, attested := s.opts.Authority.Attestation()
	state, _ := s.opts.Agent.TrustState()
	var notIssued string
	switch {
	case state == providerv1.AgentState_AGENT_STATE_QUARANTINED:
		notIssued = answer.NotIssuedQuarantined
	case purpose == providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST && attested:
		notIssued = answer.NotIssuedAlreadyAttested
	case purpose == providerv1.ChallengePurpose_CHALLENGE_PURPOSE_REATTEST && !attested:
		notIssued = answer.NotIssuedNotAttested
	}
	var nonce []byte
	if notIssued == "" {
		nonce = make([]byte, attest.NonceLen)
		if _, err := rand.Read(nonce); err != nil {
			return nil, status.Error(codes.Unavailable, "no challenge can be drawn")
		}
	}
	blob := s.signAnswer(ctx, req.GetAnswerNonce(), 0, func(h answer.Host) (proto.Message, error) {
		view, err := s.opts.Agent.HostView(ctx, 0)
		if err != nil {
			return nil, err
		}
		h.View = view
		return answer.ChallengeAnswer(answer.Challenge{Host: h, Purpose: purpose, Nonce: nonce, NotIssued: notIssued})
	})
	// Only a challenge the manager can see is held: an unsigned answer
	// issues nothing.
	if blob != nil && nonce != nil {
		s.challenges.Issue(purpose, nonce)
	}
	return &providerv1.ChallengeResponse{HostAnswer: blob}, nil
}

// Attest applies an attestation grant, in this order:
//
//  1. the request rules (strict path, this host, a 16-byte answer_nonce);
//  2. the grant verifies at grant version 2 as an attestation grant for this
//     host, Provider, installation and the certificate's enrolment, at the
//     clock floor (else REFUSED grant_invalid);
//  3. a quarantined store is QUARANTINED, unchanged;
//  4. the grant's challenge nonce names an outstanding challenge (else
//     REFUSED challenge_unknown or challenge_expired) whose purpose the
//     agent's state allows: ATTEST while awaiting the first attestation,
//     REATTEST while attested (else REFUSED already_attested or
//     not_attested); only then is it consumed;
//  5. the store checks the ledger position against its chain
//     (opstore.Store.Attest): it is APPLIED, the challenge nonce becoming the
//     attestation session (session_replaced on a re-attestation), or the
//     store is QUARANTINED with the ledger loss reason.
//
// The answer names the session and mark after the grant, so a manager can
// adopt the session from it.
func (a *authorityService) Attest(ctx context.Context, req *providerv1.AttestRequest) (*providerv1.AuthorityResponse, error) {
	s := a.s
	if err := s.checkAuthorityRequest(req, req.GetTargetHostId(), req.GetAnswerNonce()); err != nil {
		return nil, err
	}
	s.attestMu.Lock()
	defer s.attestMu.Unlock()
	outcome, reason := s.attest(ctx, req)
	digest := decodedGrantDigest(req.GetGrant())
	blob := s.signAnswer(ctx, req.GetAnswerNonce(), 0, func(h answer.Host) (proto.Message, error) {
		view, err := s.opts.Agent.HostView(ctx, 0)
		if err != nil {
			return nil, err
		}
		h.View = view
		return answer.AuthorityAnswer(answer.Authority{Host: h, GrantKind: providerv1.GrantKind_GRANT_KIND_ATTEST,
			GrantDigest: digest, Outcome: outcome, Reason: reason})
	})
	s.opts.Log.Info("attestation", "outcome", outcome.String(), "reason", reason)
	return &providerv1.AuthorityResponse{HostAnswer: blob}, nil
}

// attest decides on one attestation grant (Attest steps 2 to 5) and returns
// the outcome and its reason.
func (s *Server) attest(ctx context.Context, req *providerv1.AttestRequest) (providerv1.AuthorityOutcome, string) {
	refused := providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED
	v, err := s.verifier()
	if err != nil {
		return refused, answer.ReasonGrantInvalid
	}
	clock, _ := s.grantClock()
	got, err := v.VerifyAuthority(providerv1.HostAuthority_Attest_FullMethodName, req, clock)
	if err != nil {
		if r, ok := grantverify.AsRefusal(err); ok {
			s.opts.Log.Info("attestation grant refused", "rule", string(r.Rule))
		}
		return refused, answer.ReasonGrantInvalid
	}
	if state, _ := s.opts.Agent.TrustState(); state == providerv1.AgentState_AGENT_STATE_QUARANTINED {
		return providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_QUARANTINED, ""
	}
	nonce := got.Body.GetSingleUseNonce()
	purpose, found := s.challenges.Find(nonce)
	switch found {
	case attest.Unknown:
		return refused, answer.ReasonChallengeUnknown
	case attest.Expired:
		return refused, answer.ReasonChallengeExpired
	}
	_, attested := s.opts.Authority.Attestation()
	reattest := purpose == providerv1.ChallengePurpose_CHALLENGE_PURPOSE_REATTEST
	if !guard.Off(guard.ChallengePurpose) {
		switch {
		case !reattest && attested:
			return refused, answer.ReasonAlreadyAttested
		case reattest && !attested:
			return refused, answer.ReasonNotAttested
		}
	} else {
		reattest = attested
	}
	if !s.challenges.Consume(purpose, nonce) {
		return refused, answer.ReasonChallengeUnknown
	}
	l, cp := got.Body.GetLedger(), got.Body.GetLedgerCheckpoint()
	r := opstore.AttestRequest{Session: bytes.Clone(nonce), Ledger: position(l), Reattest: reattest}
	if cp != nil {
		p := position(cp)
		r.Checkpoint = &p
	}
	res, err := s.opts.Authority.Attest(ctx, r)
	switch {
	case errors.Is(err, journal.ErrLost):
		return providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_QUARANTINED, ""
	case errors.Is(err, opstore.ErrAttestState) && reattest:
		return refused, answer.ReasonNotAttested
	case errors.Is(err, opstore.ErrAttestState):
		return refused, answer.ReasonAlreadyAttested
	case err != nil:
		s.logNoAnswer(err, "the attestation could not be checked against the store")
		return refused, answer.ReasonStoreBusy
	case res.Quarantined != "":
		s.opts.Log.Error(nil, "an attestation quarantined the store", "reason", string(res.Quarantined))
		return providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_QUARANTINED, string(res.Quarantined)
	case reattest:
		return providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, answer.ReasonSessionReplaced
	}
	return providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, ""
}

// position is the store form of a verified ledger position (its lengths are
// checked by the grant rules).
func position(l *providerv1.LedgerPosition) journal.Position {
	var p journal.Position
	copy(p.StoreID[:], l.GetStoreId())
	copy(p.Head[:], l.GetHead())
	p.Seq = l.GetSeq()
	return p
}

// decodedGrantDigest is the grant digest of g when the call carries a grant
// within the size caps; nil otherwise ("no grant decoded"). The digest is a
// hash of bytes the caller sent, so it names the delivery even for a grant
// that did not verify.
func decodedGrantDigest(g *providerv1.OperationGrant) []byte {
	if g == nil || len(g.GetBody()) == 0 || len(g.GetBody()) > grantverify.MaxGrantBodyBytes ||
		len(g.GetSignature()) > grantverify.MaxSignatureBytes {
		return nil
	}
	return grantverify.GrantDigest(g)
}

// LedgerMark applies a ledger mark grant, in this order:
//
//  1. the request rules (strict path, this host, a 16-byte answer_nonce);
//  2. the grant verifies at grant version 2 as a ledger mark grant for this
//     host, Provider, installation and the certificate's enrolment, at the
//     clock floor (else REFUSED grant_invalid);
//  3. a quarantined store is QUARANTINED, unchanged;
//  4. the store applies it (opstore.Store.LedgerMark): an agent that awaits
//     its first attestation is REFUSED not_attested, a mark for another
//     session REFUSED stale_attestation (never a quarantine); a position
//     that is not on the store's chain QUARANTINES it with the ledger loss
//     reason; a position above the mark raises it (APPLIED) and wakes the
//     operations it now covers; one at or below it is ALREADY_APPLIED.
//
// The answer names the session and the mark after the grant.
func (a *authorityService) LedgerMark(ctx context.Context, req *providerv1.LedgerMarkRequest) (*providerv1.AuthorityResponse, error) {
	s := a.s
	if err := s.checkAuthorityRequest(req, req.GetTargetHostId(), req.GetAnswerNonce()); err != nil {
		return nil, err
	}
	s.attestMu.Lock()
	defer s.attestMu.Unlock()
	outcome, reason := s.ledgerMark(ctx, req)
	digest := decodedGrantDigest(req.GetGrant())
	blob := s.signAnswer(ctx, req.GetAnswerNonce(), 0, func(h answer.Host) (proto.Message, error) {
		view, err := s.opts.Agent.HostView(ctx, 0)
		if err != nil {
			return nil, err
		}
		h.View = view
		return answer.AuthorityAnswer(answer.Authority{Host: h, GrantKind: providerv1.GrantKind_GRANT_KIND_LEDGER_MARK,
			GrantDigest: digest, Outcome: outcome, Reason: reason})
	})
	s.opts.Log.V(1).Info("ledger mark", "outcome", outcome.String(), "reason", reason)
	return &providerv1.AuthorityResponse{HostAnswer: blob}, nil
}

// ledgerMark decides on one ledger mark grant (LedgerMark steps 2 to 4) and
// returns the outcome and its reason.
func (s *Server) ledgerMark(ctx context.Context, req *providerv1.LedgerMarkRequest) (providerv1.AuthorityOutcome, string) {
	refused := providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED
	v, err := s.verifier()
	if err != nil {
		return refused, answer.ReasonGrantInvalid
	}
	clock, _ := s.grantClock()
	got, err := v.VerifyAuthority(providerv1.HostAuthority_LedgerMark_FullMethodName, req, clock)
	if err != nil {
		if r, ok := grantverify.AsRefusal(err); ok {
			s.opts.Log.Info("ledger mark grant refused", "rule", string(r.Rule))
		}
		return refused, answer.ReasonGrantInvalid
	}
	if state, _ := s.opts.Agent.TrustState(); state == providerv1.AgentState_AGENT_STATE_QUARANTINED {
		return providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_QUARANTINED, ""
	}
	res, err := s.opts.Authority.LedgerMark(ctx, opstore.LedgerMarkRequest{Session: bytes.Clone(got.Body.GetAttestationId()),
		Ledger: position(got.Body.GetLedger())})
	switch {
	case errors.Is(err, journal.ErrLost):
		return providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_QUARANTINED, ""
	case errors.Is(err, opstore.ErrAttestState):
		return refused, answer.ReasonNotAttested
	case errors.Is(err, opstore.ErrStaleAttestation):
		return refused, answer.ReasonStaleAttestation
	case err != nil:
		s.logNoAnswer(err, "the ledger mark could not be checked against the store")
		return refused, answer.ReasonStoreBusy
	case res.Outcome == opstore.LedgerMarkQuarantined:
		s.opts.Log.Error(nil, "a ledger mark quarantined the store", "reason", string(res.Quarantined))
		return providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_QUARANTINED, string(res.Quarantined)
	case res.Outcome == opstore.LedgerMarkAlreadyApplied:
		return providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_ALREADY_APPLIED, ""
	}
	return providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, ""
}

// RetireIfUnknown applies a retire-if-unknown grant, in this order:
//
//  1. the request rules (strict path, this host, a 16-byte answer_nonce),
//     then the per-Provider retirement limit (RESOURCE_EXHAUSTED above it,
//     before any verification or scan);
//  2. the grant verifies at grant version 2 as a retirement grant for this
//     host, Provider, installation and the certificate's enrolment, at the
//     clock floor (else REFUSED grant_invalid);
//  3. a quarantined store is QUARANTINED, unchanged; an agent that is not
//     READY, or awaits its first attestation, is REFUSED not_attested;
//  4. the store applies it (opstore.Store.RetireIfUnknown): another session
//     is REFUSED stale_attestation, another enrolment REFUSED
//     enrolment_not_active (neither changes anything); the trace scan runs
//     first, without the store lock, and a trace no record explains
//     quarantines the store and is REFUSED op_trace_found; then, under the
//     store lock, a known operation is NOT_APPLICABLE with its state (with
//     other identity fields REFUSED op_known), a witness or a busy VM of an
//     operation with no record QUARANTINES the store (inconsistent_store),
//     and otherwise the RETIRED tombstone (never_received) is committed and
//     the answer is APPLIED with state RETIRED (ALREADY_APPLIED for a
//     retirement already committed). A store whose executor has no trace
//     scanner is REFUSED trace_scan_unavailable; a scan that failed, a full
//     store or a store that cannot be read is REFUSED store_busy: nothing
//     is retired.
//
// The answer echoes the grant's op_id once the grant verified.
func (a *authorityService) RetireIfUnknown(ctx context.Context, req *providerv1.RetireIfUnknownRequest) (*providerv1.AuthorityResponse, error) {
	s := a.s
	if err := s.checkAuthorityRequest(req, req.GetTargetHostId(), req.GetAnswerNonce()); err != nil {
		return nil, err
	}
	// The per-Provider retirement limit comes before anything costly (the
	// grant's verification, the store reads and the executor's trace scan):
	// a retirement grant can be replayed within its session and lifetime.
	if id := peerIdentity(ctx); id == "" || !s.retires.allow(id, s.opts.Now()) {
		return nil, status.Error(codes.ResourceExhausted, "above this Provider's retire-if-unknown rate; nothing was decided")
	}
	d := s.retire(ctx, req)
	digest := decodedGrantDigest(req.GetGrant())
	blob := s.signAnswer(ctx, req.GetAnswerNonce(), 0, func(h answer.Host) (proto.Message, error) {
		view, err := s.opts.Agent.HostView(ctx, 0)
		if err != nil {
			return nil, err
		}
		h.View = view
		return answer.AuthorityAnswer(answer.Authority{Host: h, GrantKind: providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN,
			GrantDigest: digest, Outcome: d.outcome, Reason: d.reason, OpID: d.opID, OpState: d.state})
	})
	s.opts.Log.Info("retire-if-unknown", "outcome", d.outcome.String(), "reason", d.reason)
	return &providerv1.AuthorityResponse{HostAnswer: blob}, nil
}

// retireDecision is what the agent decided on one retirement grant.
type retireDecision struct {
	outcome providerv1.AuthorityOutcome
	reason  string
	// opID is the grant's op_id once the grant verified.
	opID []byte
	// state is the operation's state after the grant, when known.
	state providerv1.OperationRecordState
}

// retire decides on one retirement grant (RetireIfUnknown steps 2 to 4).
func (s *Server) retire(ctx context.Context, req *providerv1.RetireIfUnknownRequest) retireDecision {
	refused := func(reason string) retireDecision {
		return retireDecision{outcome: providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED, reason: reason}
	}
	v, err := s.verifier()
	if err != nil {
		return refused(answer.ReasonGrantInvalid)
	}
	clock, _ := s.grantClock()
	got, err := v.VerifyAuthority(providerv1.HostAuthority_RetireIfUnknown_FullMethodName, req, clock)
	if err != nil {
		if r, ok := grantverify.AsRefusal(err); ok {
			s.opts.Log.Info("retirement grant refused", "rule", string(r.Rule))
		}
		return refused(answer.ReasonGrantInvalid)
	}
	b := got.Body
	d := s.retireVerified(ctx, b)
	d.opID = bytes.Clone(b.GetOpId())
	return d
}

// retireVerified applies the verified retirement grant body b.
func (s *Server) retireVerified(ctx context.Context, b *providerv1.GrantBody) retireDecision {
	refused := func(reason string) retireDecision {
		return retireDecision{outcome: providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED, reason: reason}
	}
	quarantined := retireDecision{outcome: providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_QUARANTINED}
	switch state, _ := s.opts.Agent.TrustState(); state {
	case providerv1.AgentState_AGENT_STATE_QUARANTINED:
		return quarantined
	case providerv1.AgentState_AGENT_STATE_READY:
	default:
		return refused(answer.ReasonNotAttested)
	}
	if _, ok := s.opts.Authority.Attestation(); !ok {
		return refused(answer.ReasonNotAttested)
	}
	res, err := s.opts.Authority.RetireIfUnknown(ctx, opstore.RetireRequest{Session: bytes.Clone(b.GetAttestationId()),
		Op: opstore.Request{ProviderUID: b.GetProviderUid(), EnrolmentID: b.GetEnrolmentId(), OpID: b.GetOpId(), VMUID: b.GetVmUid(),
			BindingID: b.GetBindingId(), Kind: b.GetKind(), Attempt: b.GetAttempt(), IntentDigest: b.GetIntentDigest(),
			MintEpoch: opstore.Epoch{Counter: b.GetMintEpoch().GetCounter(), Holder: b.GetMintEpoch().GetHolder()}}})
	switch {
	case errors.Is(err, journal.ErrLost):
		return quarantined
	case errors.Is(err, opstore.ErrStaleAttestation):
		return refused(answer.ReasonStaleAttestation)
	case errors.Is(err, opstore.ErrAttestState):
		return refused(answer.ReasonNotAttested)
	case errors.Is(err, opstore.ErrEnrolmentNotActive):
		return refused(answer.ReasonEnrolmentNotActive)
	case errors.Is(err, opstore.ErrNoTraceScanner):
		s.opts.Log.Info("a retirement was refused: the executor cannot scan for traces")
		return refused(answer.ReasonTraceScanUnavailable)
	case errors.Is(err, opstore.ErrTraceScanFailed):
		s.logNoAnswer(err, "a retirement's trace scan failed")
		return refused(answer.ReasonStoreBusy)
	case err != nil:
		s.logNoAnswer(err, "the retirement could not be decided")
		return refused(answer.ReasonStoreBusy)
	}
	state := answer.RecordState(opstore.OpAnswerView{Found: true, Op: opstore.OpView{State: res.State}})
	switch res.Outcome {
	case opstore.RetireApplied:
		return retireDecision{outcome: providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, state: state}
	case opstore.RetireAlreadyApplied:
		return retireDecision{outcome: providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_ALREADY_APPLIED, state: state}
	case opstore.RetireNotApplicable:
		return retireDecision{outcome: providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_NOT_APPLICABLE, state: state}
	case opstore.RetireOpKnown:
		return refused(answer.ReasonOpKnown)
	case opstore.RetireTraceFound:
		s.opts.Log.Error(nil, "a retirement found a trace of an unknown operation and quarantined the store",
			"reason", string(res.Quarantined))
		return refused(answer.ReasonOpTraceFound)
	case opstore.RetireQuarantined:
		s.opts.Log.Error(nil, "a retirement found evidence of a lost record and quarantined the store", "reason", string(res.Quarantined))
		return quarantined
	}
	return refused(answer.ReasonStoreBusy)
}
