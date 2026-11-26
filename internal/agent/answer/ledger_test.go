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

package answer_test

import (
	"bytes"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

var (
	testSession = bytes.Repeat([]byte{0xA5}, 16)
	testOrigin  = bytes.Repeat([]byte{0x0E}, 16)
)

// attestedHost is testHost with an attestation session and origin evidence.
func attestedHost(p *trusttest.PKI) answer.Host {
	h := testHost(p)
	h.View.OriginEnrolmentID, h.View.EnrolmentActivations = testOrigin, 2
	h.Attested = true
	h.Attestation = opstore.Attestation{Session: testSession, Mark: h.View.Position}
	return h
}

func TestHostAnswer_LedgerState(t *testing.T) {
	p := trusttest.New(t)

	// Not asked: no ledger state, no echo, no attestation id, even when
	// attested.
	h := attestedHost(p)
	a, err := answer.HostAnswer(h)
	if err != nil {
		t.Fatal(err)
	}
	if a.GetLedger() != nil || a.GetContext().GetLedgerVersionEcho() != 0 || len(a.GetContext().GetAttestationId()) != 0 {
		t.Fatalf("an answer that was not asked carries ledger fields: %v", a)
	}

	// Asked, attested.
	h.LedgerRequested = 7
	a, err = answer.HostAnswer(h)
	if err != nil {
		t.Fatal(err)
	}
	l, c := a.GetLedger(), a.GetContext()
	if c.GetLedgerVersionEcho() != 7 || !bytes.Equal(c.GetAttestationId(), testSession) {
		t.Fatalf("context %v", c)
	}
	if l.GetRequestedVersion() != 7 || l.GetVersion() != answer.LedgerVersion ||
		l.GetState() != providerv1.LedgerSessionState_LEDGER_SESSION_STATE_ATTESTED || !bytes.Equal(l.GetAttestationId(), testSession) ||
		l.GetMark().GetSeq() != h.View.Position.Seq || !bytes.Equal(l.GetMark().GetHead(), h.View.Position.Head[:]) ||
		!bytes.Equal(l.GetMark().GetStoreId(), h.View.Position.StoreID[:]) ||
		!bytes.Equal(l.GetStoreOriginEnrolmentId(), testOrigin) || l.GetStoreEnrolmentCount() != 2 {
		t.Fatalf("ledger %v", l)
	}
	blob, err := answer.Sign(p.HostKey, leafDER(t, p), a)
	if err != nil {
		t.Fatal(err)
	}
	verify(t, blob, &providerv1.HostAnswer{})

	// Asked, awaiting: no session, no mark; the origin evidence is there.
	h.Attested, h.Attestation = false, opstore.Attestation{}
	h.LedgerRequested = 1
	a, err = answer.HostAnswer(h)
	if err != nil {
		t.Fatal(err)
	}
	l = a.GetLedger()
	if l.GetState() != providerv1.LedgerSessionState_LEDGER_SESSION_STATE_AWAITING_ATTESTATION || l.GetMark() != nil ||
		len(l.GetAttestationId()) != 0 || len(a.GetContext().GetAttestationId()) != 0 || !bytes.Equal(l.GetStoreOriginEnrolmentId(), testOrigin) {
		t.Fatalf("awaiting: %v", a)
	}

	// A quarantined store names no session even when it had one.
	h = attestedHost(p)
	h.LedgerRequested = 1
	h.State, h.StateReason = providerv1.AgentState_AGENT_STATE_QUARANTINED, string(journal.LostLedgerMismatch)
	a, err = answer.HostAnswer(h)
	if err != nil {
		t.Fatal(err)
	}
	if a.GetLedger().GetState() != providerv1.LedgerSessionState_LEDGER_SESSION_STATE_AWAITING_ATTESTATION ||
		len(a.GetContext().GetAttestationId()) != 0 {
		t.Fatalf("quarantined: %v", a)
	}
}

// TestStateReason_LedgerReasonsGated: a quarantine an attestation recorded
// keeps its reason only for a ledger-aware request.
func TestStateReason_LedgerReasonsGated(t *testing.T) {
	p := trusttest.New(t)
	for _, r := range journal.LedgerReasons {
		h := testHost(p)
		h.State, h.StateReason = providerv1.AgentState_AGENT_STATE_QUARANTINED, string(r)
		a, err := answer.HostAnswer(h)
		if err != nil {
			t.Fatal(err)
		}
		if got := a.GetContext().GetStateReason(); got != string(journal.LostRolledBack) {
			t.Fatalf("%s to a request that did not ask: %q, want rolled_back", r, got)
		}
		if _, err := answer.Sign(p.HostKey, leafDER(t, p), a); err != nil {
			t.Fatal(err)
		}
		h.LedgerRequested = 1
		if a, err = answer.HostAnswer(h); err != nil || a.GetContext().GetStateReason() != string(r) {
			t.Fatalf("%s to a ledger-aware request: %v %v", r, a.GetContext().GetStateReason(), err)
		}
		if _, err := answer.Sign(p.HostKey, leafDER(t, p), a); err != nil {
			t.Fatal(err)
		}
		h.LedgerRequested = 0
		ch, err := answer.ChallengeAnswer(answer.Challenge{Host: h, Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST,
			NotIssued: answer.NotIssuedQuarantined})
		if err != nil || ch.GetContext().GetStateReason() != string(r) {
			t.Fatalf("%s in a challenge answer: %v %v", r, ch.GetContext().GetStateReason(), err)
		}
	}
}

func TestChallengeAnswer(t *testing.T) {
	p := trusttest.New(t)
	h := testHost(p)
	h.View.OriginEnrolmentID, h.View.EnrolmentActivations = p.Record.EnrolmentID, 1
	nonce := bytes.Repeat([]byte{0xC7}, 16)
	a, err := answer.ChallengeAnswer(answer.Challenge{Host: h, Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST, Nonce: nonce})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.GetChallengeNonce(), nonce) || a.GetValidForSeconds() != 300 || a.GetNotIssuedReason() != "" ||
		a.GetLedger().GetVersion() != 1 || !bytes.Equal(a.GetLedger().GetStoreOriginEnrolmentId(), p.Record.EnrolmentID) ||
		a.GetLedger().GetStoreEnrolmentCount() != 1 || a.GetContext().GetLedgerVersionEcho() != 0 {
		t.Fatalf("challenge answer %v", a)
	}
	blob, err := answer.Sign(p.HostKey, leafDER(t, p), a)
	if err != nil {
		t.Fatal(err)
	}
	if blob.GetBodyType() != answer.BodyTypeChallengeAnswer {
		t.Fatalf("body type %q", blob.GetBodyType())
	}
	verify(t, blob, &providerv1.ChallengeAnswer{})

	// An attested agent's challenge answer names its session (an authority
	// answer is ledger-aware by definition).
	ah := attestedHost(p)
	a, err = answer.ChallengeAnswer(answer.Challenge{Host: ah, Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_REATTEST, Nonce: nonce})
	if err != nil || !bytes.Equal(a.GetContext().GetAttestationId(), testSession) ||
		a.GetLedger().GetState() != providerv1.LedgerSessionState_LEDGER_SESSION_STATE_ATTESTED {
		t.Fatalf("attested: %v %v", a, err)
	}
	if _, err := answer.Sign(p.HostKey, leafDER(t, p), a); err != nil {
		t.Fatal(err)
	}

	a, err = answer.ChallengeAnswer(answer.Challenge{Host: h, Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST,
		NotIssued: answer.NotIssuedAlreadyAttested})
	if err != nil || len(a.GetChallengeNonce()) != 0 || a.GetValidForSeconds() != 0 {
		t.Fatalf("not issued: %v %v", a, err)
	}

	for name, c := range map[string]answer.Challenge{
		"no purpose":        {Host: h, Nonce: nonce},
		"unknown purpose":   {Host: h, Purpose: 9, Nonce: nonce},
		"neither":           {Host: h, Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST},
		"both":              {Host: h, Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST, Nonce: nonce, NotIssued: answer.NotIssuedQuarantined},
		"short nonce":       {Host: h, Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST, Nonce: nonce[:8]},
		"reason not in set": {Host: h, Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST, NotIssued: "not_enrolled"},
	} {
		if _, err := answer.ChallengeAnswer(c); !errors.Is(err, answer.ErrSign) {
			t.Fatalf("%s: %v, want ErrSign", name, err)
		}
	}
	bad := h
	bad.Nonce = nil
	if _, err := answer.ChallengeAnswer(answer.Challenge{Host: bad, Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST, Nonce: nonce}); !errors.Is(err, answer.ErrNoAnswer) {
		t.Fatalf("no request nonce: %v", err)
	}
}

func TestAuthorityAnswer(t *testing.T) {
	p := trusttest.New(t)
	h := attestedHost(p)
	digest := bytes.Repeat([]byte{0xD1}, 32)
	a, err := answer.AuthorityAnswer(answer.Authority{Host: h, GrantKind: providerv1.GrantKind_GRANT_KIND_ATTEST, GrantDigest: digest,
		Outcome: providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, Reason: answer.ReasonSessionReplaced})
	if err != nil {
		t.Fatal(err)
	}
	if a.GetGrantKind() != int32(providerv1.GrantKind_GRANT_KIND_ATTEST) || !bytes.Equal(a.GetGrantDigest(), digest) ||
		a.GetOutcomeReason() != answer.ReasonSessionReplaced || a.GetMark().GetSeq() != h.View.Position.Seq ||
		!bytes.Equal(a.GetContext().GetAttestationId(), testSession) {
		t.Fatalf("authority answer %v", a)
	}
	blob, err := answer.Sign(p.HostKey, leafDER(t, p), a)
	if err != nil {
		t.Fatal(err)
	}
	if blob.GetBodyType() != answer.BodyTypeAuthorityAnswer {
		t.Fatalf("body type %q", blob.GetBodyType())
	}
	verify(t, blob, &providerv1.AuthorityAnswer{})

	// A refusal of an agent that awaits attestation carries no mark.
	w := testHost(p)
	a, err = answer.AuthorityAnswer(answer.Authority{Host: w, GrantKind: providerv1.GrantKind_GRANT_KIND_ATTEST,
		Outcome: providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED, Reason: answer.ReasonGrantInvalid})
	if err != nil || a.GetMark() != nil || len(a.GetGrantDigest()) != 0 {
		t.Fatalf("refusal: %v %v", a, err)
	}
	for name, c := range map[string]answer.Authority{
		"operation kind":  {Host: h, GrantKind: providerv1.GrantKind_GRANT_KIND_OPERATION, Outcome: providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED},
		"no outcome":      {Host: h, GrantKind: providerv1.GrantKind_GRANT_KIND_ATTEST},
		"unknown outcome": {Host: h, GrantKind: providerv1.GrantKind_GRANT_KIND_ATTEST, Outcome: 99},
		"reason":          {Host: h, GrantKind: providerv1.GrantKind_GRANT_KIND_ATTEST, Outcome: providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED, Reason: "rolled_back"},
		"short digest":    {Host: h, GrantKind: providerv1.GrantKind_GRANT_KIND_ATTEST, Outcome: providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED, GrantDigest: digest[:8]},
	} {
		if _, err := answer.AuthorityAnswer(c); !errors.Is(err, answer.ErrSign) {
			t.Fatalf("%s: %v, want ErrSign", name, err)
		}
	}
}

// TestSign_LedgerFieldRules: whatever built the answer, a host ledger field
// is signed only where the request asked for the ledger protocol, and the
// fields this agent does not implement are never signed.
func TestSign_LedgerFieldRules(t *testing.T) {
	p := trusttest.New(t)
	h := testHost(p)
	host := func() *providerv1.HostAnswer {
		a, err := answer.HostAnswer(h)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	op := func() *providerv1.OperationAnswer {
		return &providerv1.OperationAnswer{Context: host().GetContext(), OpId: bytes.Repeat([]byte{1}, 16),
			OpEnrolmentId: p.Record.EnrolmentID, State: providerv1.OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD}
	}
	vm := func() *providerv1.VMAnswer {
		return &providerv1.VMAnswer{Context: host().GetContext(), VmUid: "vm-uid-1", BindingId: bytes.Repeat([]byte{2}, 16),
			Presence: providerv1.VMPresence_VM_PRESENCE_NO_RECORD}
	}
	ch := func() *providerv1.ChallengeAnswer {
		a, err := answer.ChallengeAnswer(answer.Challenge{Host: h, Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST,
			Nonce: bytes.Repeat([]byte{3}, 16)})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	au := func() *providerv1.AuthorityAnswer {
		a, err := answer.AuthorityAnswer(answer.Authority{Host: h, GrantKind: providerv1.GrantKind_GRANT_KIND_ATTEST,
			Outcome: providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED, Reason: answer.ReasonChallengeUnknown})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	refused := map[string]func() proto.Message{
		"host attestation id unasked": func() proto.Message { a := host(); a.Context.AttestationId = testSession; return a },
		"host ledger unasked": func() proto.Message {
			a := host()
			a.Ledger = &providerv1.HostLedgerState{RequestedVersion: 1}
			return a
		},
		"host ledger echo differs": func() proto.Message {
			a := host()
			a.Context.LedgerVersionEcho, a.Ledger = 2, &providerv1.HostLedgerState{RequestedVersion: 1}
			return a
		},
		"operation attestation id unasked": func() proto.Message { a := op(); a.Context.AttestationId = testSession; return a },
		"vm attestation id unasked":        func() proto.Message { a := vm(); a.Context.AttestationId = testSession; return a },
		"operation accepted_seq":           func() proto.Message { a := op(); a.AcceptedSeq = 1; return a },
		"operation accepted_head":          func() proto.Message { a := op(); a.AcceptedHead = bytes.Repeat([]byte{4}, 32); return a },
		"operation awaiting_request":       func() proto.Message { a := op(); a.AwaitingRequest = true; return a },
		"ledger reason unasked": func() proto.Message {
			a := host()
			a.Context.AgentState, a.Context.StateReason = providerv1.AgentState_AGENT_STATE_QUARANTINED, string(journal.LostLedgerAhead)
			return a
		},
		"challenge answer echo":  func() proto.Message { a := ch(); a.Context.LedgerVersionEcho = 1; return a },
		"authority answer echo":  func() proto.Message { a := au(); a.Context.LedgerVersionEcho = 1; return a },
		"receipt body (not yet)": func() proto.Message { return &providerv1.OperationReceipt{ReceiptVersion: 1} },
	}
	for name, body := range refused {
		t.Run(name, func(t *testing.T) {
			if _, err := answer.Sign(p.HostKey, leafDER(t, p), body()); !errors.Is(err, answer.ErrSign) {
				t.Fatalf("Sign = %v, want ErrSign", err)
			}
		})
	}
	allowed := map[string]func() proto.Message{
		"host":         func() proto.Message { return host() },
		"operation":    func() proto.Message { return op() },
		"vm":           func() proto.Message { return vm() },
		"vm echo":      func() proto.Message { a := vm(); a.Context.LedgerVersionEcho = 2; return a },
		"challenge":    func() proto.Message { return ch() },
		"authority":    func() proto.Message { return au() },
		"challenge+id": func() proto.Message { a := ch(); a.Context.AttestationId = testSession; return a },
		"op asked + id": func() proto.Message {
			a := op()
			a.Context.LedgerVersionEcho, a.Context.AttestationId = 1, testSession
			return a
		},
		"host asked all": func() proto.Message {
			hh := attestedHost(p)
			hh.LedgerRequested = 1
			a, _ := answer.HostAnswer(hh)
			return a
		},
	}
	for name, body := range allowed {
		t.Run(name, func(t *testing.T) {
			if _, err := answer.Sign(p.HostKey, leafDER(t, p), body()); err != nil {
				t.Fatalf("Sign = %v", err)
			}
		})
	}
}

func TestVersions(t *testing.T) {
	if answer.AgentAPIVersion != 5 || answer.GrantVersion != 2 || answer.LedgerVersion != 1 {
		t.Fatalf("api %d, grant %d, ledger %d", answer.AgentAPIVersion, answer.GrantVersion, answer.LedgerVersion)
	}
}
