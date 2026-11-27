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

package hostidentity_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

var (
	testSession = bytes.Repeat([]byte{0x5e}, 16)
	testStore   = bytes.Repeat([]byte{0xab}, 16)
	testHead    = bytes.Repeat([]byte{0xcd}, 32)
)

// ledgerHostAnswer is a host answer to a request with ledger version 2 from
// an attested agent.
func (f *answerFixture) ledgerHostAnswer() *providerv1.HostAnswer {
	a := f.hostAnswer()
	a.AgentApiVersion = 6
	a.GrantVersions = []uint32{2}
	a.Context.LedgerVersionEcho = hostidentity.LedgerVersion
	a.Context.AttestationId = testSession
	a.Ledger = &providerv1.HostLedgerState{RequestedVersion: hostidentity.LedgerVersion, Version: hostidentity.LedgerVersion,
		State: providerv1.LedgerSessionState_LEDGER_SESSION_STATE_ATTESTED, AttestationId: testSession,
		Mark:                   &providerv1.LedgerPosition{StoreId: testStore, Seq: 5, Head: testHead},
		StoreOriginEnrolmentId: f.pki.Record.EnrolmentID, StoreEnrolmentCount: 1}
	return a
}

func (f *answerFixture) ledgerExp(bodyType string) hostidentity.AnswerExpectation {
	exp := f.exp(bodyType)
	exp.LedgerVersion = hostidentity.LedgerVersion
	return exp
}

func TestVerifyAnswer_LedgerHostAnswer(t *testing.T) {
	f := newAnswerFixture(t)
	v, err := hostidentity.VerifyAnswer(f.signBody(t, f.ledgerHostAnswer()), f.ledgerExp(hostidentity.BodyTypeHostAnswer))
	if err != nil {
		t.Fatalf("VerifyAnswer = %v", err)
	}
	if v.Ledger == nil || !v.Attestation.Reported || v.Attestation.EverAttested {
		t.Fatalf("ledger state not recorded: %+v", v.Attestation)
	}
	// The same answer to a request that asked for no ledger state is
	// refused: the ledger fields are only for a request that asked.
	if _, err := hostidentity.VerifyAnswer(f.signBody(t, f.ledgerHostAnswer()), f.exp(hostidentity.BodyTypeHostAnswer)); err == nil {
		t.Fatal("a ledger answer to a request without a ledger version verified")
	}
}

func TestVerifyAnswer_LedgerHostAnswerRules(t *testing.T) {
	f := newAnswerFixture(t)
	cases := map[string]func(a *providerv1.HostAnswer){
		"echo stripped":        func(a *providerv1.HostAnswer) { a.Context.LedgerVersionEcho = 0 },
		"no ledger state":      func(a *providerv1.HostAnswer) { a.Ledger = nil },
		"requested not echoed": func(a *providerv1.HostAnswer) { a.Ledger.RequestedVersion = 1 },
		"version above request": func(a *providerv1.HostAnswer) {
			a.Ledger.Version = hostidentity.LedgerVersion + 1
		},
		"session differs from context": func(a *providerv1.HostAnswer) { a.Context.AttestationId = bytes.Repeat([]byte{1}, 16) },
		"attested without mark":        func(a *providerv1.HostAnswer) { a.Ledger.Mark = nil },
		"mark of another store": func(a *providerv1.HostAnswer) {
			a.Ledger.Mark.StoreId = bytes.Repeat([]byte{9}, 16)
		},
		"mark above position": func(a *providerv1.HostAnswer) { a.Ledger.Mark.Seq = 8 },
		"awaiting with session": func(a *providerv1.HostAnswer) {
			a.Ledger.State = providerv1.LedgerSessionState_LEDGER_SESSION_STATE_AWAITING_ATTESTATION
		},
		"short session": func(a *providerv1.HostAnswer) {
			a.Context.AttestationId, a.Ledger.AttestationId = []byte{1}, []byte{1}
		},
		"bad origin": func(a *providerv1.HostAnswer) { a.Ledger.StoreOriginEnrolmentId = []byte{1, 2} },
		"bad attestation record": func(a *providerv1.HostAnswer) {
			a.Ledger.StoreEverAttested, a.Ledger.StoreFirstAttestedSeq = true, 0
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			a := f.ledgerHostAnswer()
			edit(a)
			if _, err := hostidentity.VerifyAnswer(f.signBody(t, a), f.ledgerExp(hostidentity.BodyTypeHostAnswer)); !errors.Is(err, hostidentity.ErrAnswer) {
				t.Fatalf("VerifyAnswer = %v, want ErrAnswer", err)
			}
		})
	}
}

func TestVerifyAnswer_LedgerOperationAnswer(t *testing.T) {
	f := newAnswerFixture(t)
	a := f.opAnswer()
	a.Tombstoned = false
	a.State = providerv1.OperationRecordState_OPERATION_RECORD_STATE_ACCEPTED
	a.AcceptedSeq, a.AcceptedHead = 4, testHead
	a.Context.LedgerVersionEcho = hostidentity.LedgerVersion
	if _, err := hostidentity.VerifyAnswer(f.signBody(t, a), f.ledgerExp(hostidentity.BodyTypeOperationAnswer)); err != nil {
		t.Fatalf("VerifyAnswer = %v", err)
	}
	a.State = providerv1.OperationRecordState_OPERATION_RECORD_STATE_EXECUTING
	if _, err := hostidentity.VerifyAnswer(f.signBody(t, a), f.ledgerExp(hostidentity.BodyTypeOperationAnswer)); err == nil {
		t.Fatal("an accepted head of an operation that is not accepted verified")
	}
	a.State = providerv1.OperationRecordState_OPERATION_RECORD_STATE_ACCEPTED
	a.AcceptedSeq = 8 // the chain position is 7
	if _, err := hostidentity.VerifyAnswer(f.signBody(t, a), f.ledgerExp(hostidentity.BodyTypeOperationAnswer)); err == nil {
		t.Fatal("an accepted sequence number above the chain position verified")
	}
}

func (f *answerFixture) challengeAnswer() *providerv1.ChallengeAnswer {
	c := f.ctx()
	return &providerv1.ChallengeAnswer{Context: c, Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST,
		ChallengeNonce: bytes.Repeat([]byte{7}, 16), ValidForSeconds: 300,
		Ledger: &providerv1.HostLedgerState{RequestedVersion: 2, Version: 2,
			State: providerv1.LedgerSessionState_LEDGER_SESSION_STATE_AWAITING_ATTESTATION}}
}

func (f *answerFixture) challengeExp() hostidentity.AnswerExpectation {
	exp := f.exp(hostidentity.BodyTypeChallengeAnswer)
	exp.ChallengePurpose = providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST
	return exp
}

func TestVerifyAnswer_ChallengeAnswer(t *testing.T) {
	f := newAnswerFixture(t)
	v, err := hostidentity.VerifyAnswer(f.signBody(t, f.challengeAnswer()), f.challengeExp())
	if err != nil {
		t.Fatalf("VerifyAnswer = %v", err)
	}
	if v.Challenge == nil || v.Ledger == nil || !v.Attestation.Reported {
		t.Fatal("challenge answer not decoded")
	}
	cases := map[string]func(a *providerv1.ChallengeAnswer){
		"other purpose": func(a *providerv1.ChallengeAnswer) {
			a.Purpose = providerv1.ChallengePurpose_CHALLENGE_PURPOSE_REATTEST
		},
		"nonce and reason": func(a *providerv1.ChallengeAnswer) { a.NotIssuedReason = "quarantined" },
		"neither":          func(a *providerv1.ChallengeAnswer) { a.ChallengeNonce, a.ValidForSeconds = nil, 0 },
		"unknown reason": func(a *providerv1.ChallengeAnswer) {
			a.ChallengeNonce, a.ValidForSeconds, a.NotIssuedReason = nil, 0, "whatever"
		},
		"validity too long": func(a *providerv1.ChallengeAnswer) { a.ValidForSeconds = 301 },
		"no ledger":         func(a *providerv1.ChallengeAnswer) { a.Ledger = nil },
		"echo":              func(a *providerv1.ChallengeAnswer) { a.Context.LedgerVersionEcho = 2 },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			a := f.challengeAnswer()
			edit(a)
			if _, err := hostidentity.VerifyAnswer(f.signBody(t, a), f.challengeExp()); !errors.Is(err, hostidentity.ErrAnswer) {
				t.Fatalf("VerifyAnswer = %v, want ErrAnswer", err)
			}
		})
	}
}

func TestVerifyAnswer_AuthorityAnswer(t *testing.T) {
	f := newAnswerFixture(t)
	digest := bytes.Repeat([]byte{4}, 32)
	mk := func() *providerv1.AuthorityAnswer {
		c := f.ctx()
		c.AttestationId = testSession
		return &providerv1.AuthorityAnswer{Context: c, GrantKind: int32(providerv1.GrantKind_GRANT_KIND_ATTEST), GrantDigest: digest,
			Outcome: providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED,
			Mark:    &providerv1.LedgerPosition{StoreId: testStore, Seq: 3, Head: testHead}}
	}
	exp := f.exp(hostidentity.BodyTypeAuthorityAnswer)
	exp.GrantKind, exp.GrantDigest = providerv1.GrantKind_GRANT_KIND_ATTEST, digest
	if _, err := hostidentity.VerifyAnswer(f.signBody(t, mk()), exp); err != nil {
		t.Fatalf("VerifyAnswer = %v", err)
	}
	cases := map[string]func(a *providerv1.AuthorityAnswer){
		"kind swapped":      func(a *providerv1.AuthorityAnswer) { a.GrantKind = int32(providerv1.GrantKind_GRANT_KIND_LEDGER_MARK) },
		"kind out of set":   func(a *providerv1.AuthorityAnswer) { a.GrantKind = 99 },
		"other grant":       func(a *providerv1.AuthorityAnswer) { a.GrantDigest = bytes.Repeat([]byte{5}, 32) },
		"no grant, applied": func(a *providerv1.AuthorityAnswer) { a.GrantDigest = nil },
		"unknown reason":    func(a *providerv1.AuthorityAnswer) { a.OutcomeReason = "nope" },
		"op in attest":      func(a *providerv1.AuthorityAnswer) { a.OpId = bytes.Repeat([]byte{1}, 16) },
		"mark other store":  func(a *providerv1.AuthorityAnswer) { a.Mark.StoreId = bytes.Repeat([]byte{1}, 16) },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			a := mk()
			edit(a)
			if _, err := hostidentity.VerifyAnswer(f.signBody(t, a), exp); !errors.Is(err, hostidentity.ErrAnswer) {
				t.Fatalf("VerifyAnswer = %v, want ErrAnswer", err)
			}
		})
	}
}

func TestLedgerEvidence_OnlyFromVerifiedAnswers(t *testing.T) {
	f := newAnswerFixture(t)
	if _, err := (hostidentity.VerifiedAnswer{Host: f.ledgerHostAnswer(), Ledger: f.ledgerHostAnswer().Ledger,
		Context: f.ctx()}).LedgerEvidence(); err == nil {
		t.Fatal("a hand-built VerifiedAnswer yielded ledger evidence")
	}
	if (hostidentity.LedgerEvidence{}).Valid() {
		t.Fatal("the zero evidence is valid")
	}
	exp := f.ledgerExp(hostidentity.BodyTypeHostAnswer)
	exp.ContinuitySeq = 3
	a := f.ledgerHostAnswer()
	a.Context.ContinuityHead = testHead
	v, err := hostidentity.VerifyAnswer(f.signBody(t, a), exp)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := v.LedgerEvidence()
	if err != nil || !ev.Valid() {
		t.Fatalf("LedgerEvidence = %v", err)
	}
	if !bytes.Equal(ev.Nonce(), f.nonce) || ev.StoreID() != [16]byte(testStore) || ev.HostID() != trusttest.HostName {
		t.Fatal("evidence does not carry its request binding")
	}
	if seq, head, ok := ev.Continuity(); !ok || seq != 3 || head != [32]byte(testHead) {
		t.Fatal("evidence does not carry the continuity confirmation")
	}
	// An operation answer is no ledger evidence.
	op := f.opAnswer()
	op.Context.LedgerVersionEcho = hostidentity.LedgerVersion
	vo, err := hostidentity.VerifyAnswer(f.signBody(t, op), f.ledgerExp(hostidentity.BodyTypeOperationAnswer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vo.LedgerEvidence(); err == nil {
		t.Fatal("an operation answer yielded ledger evidence")
	}
}

func TestVerifyReceipt(t *testing.T) {
	f := newAnswerFixture(t)
	digest := bytes.Repeat([]byte{4}, 32)
	mk := func() *providerv1.OperationReceipt {
		return &providerv1.OperationReceipt{ReceiptVersion: 1, ProviderUid: trusttest.ProviderUID, HostId: trusttest.HostName,
			InstallId: f.pki.Record.InstallID, EnrolmentId: f.pki.Record.EnrolmentID, AttestationId: testSession, StoreId: testStore,
			OpId: bytes.Repeat([]byte{1}, 16), Kind: int32(providerv1.OperationKind_OPERATION_KIND_CREATE), Attempt: 1, VmUid: "vm-uid-1",
			BindingId: bytes.Repeat([]byte{2}, 16), IntentDigest: bytes.Repeat([]byte{3}, 32), AcceptedSeq: 9, AcceptedHead: testHead,
			GrantDigest: digest}
	}
	exp := hostidentity.ReceiptExpectation{Anchors: f.pki.HostCAPool(), HostURI: f.pki.HostURI, HostName: trusttest.HostName,
		HostID: trusttest.HostName, Pins: []string{f.pin}, ProviderUID: trusttest.ProviderUID, InstallID: f.pki.Record.InstallID,
		EnrolmentID: f.pki.Record.EnrolmentID, OpID: bytes.Repeat([]byte{1}, 16), Kind: providerv1.OperationKind_OPERATION_KIND_CREATE,
		Attempt: 1, VMUID: "vm-uid-1", BindingID: bytes.Repeat([]byte{2}, 16), IntentDigest: bytes.Repeat([]byte{3}, 32),
		GrantDigest: digest, Now: time.Now()}
	r, err := hostidentity.VerifyReceipt(f.signBody(t, mk()), exp)
	if err != nil {
		t.Fatalf("VerifyReceipt = %v", err)
	}
	if r.Seq != 9 || r.StoreID != [16]byte(testStore) || r.AttestationID != [16]byte(testSession) {
		t.Fatalf("receipt not decoded: %+v", r)
	}
	cases := map[string]func(r *providerv1.OperationReceipt){
		"other grant":   func(r *providerv1.OperationReceipt) { r.GrantDigest = bytes.Repeat([]byte{6}, 32) },
		"other op":      func(r *providerv1.OperationReceipt) { r.OpId = bytes.Repeat([]byte{9}, 16) },
		"kind swapped":  func(r *providerv1.OperationReceipt) { r.Kind = int32(providerv1.OperationKind_OPERATION_KIND_DELETE) },
		"kind unknown":  func(r *providerv1.OperationReceipt) { r.Kind = 999 },
		"other attempt": func(r *providerv1.OperationReceipt) { r.Attempt = 2 },
		"version":       func(r *providerv1.OperationReceipt) { r.ReceiptVersion = 2 },
		"other host":    func(r *providerv1.OperationReceipt) { r.HostId = "host-z" },
		"no position":   func(r *providerv1.OperationReceipt) { r.AcceptedSeq = 0 },
		"short session": func(r *providerv1.OperationReceipt) { r.AttestationId = []byte{1} },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			rc := mk()
			edit(rc)
			if _, err := hostidentity.VerifyReceipt(f.signBody(t, rc), exp); !errors.Is(err, hostidentity.ErrAnswer) {
				t.Fatalf("VerifyReceipt = %v, want ErrAnswer", err)
			}
		})
	}
	// A receipt is never an answer.
	if _, err := hostidentity.VerifyAnswer(f.signBody(t, mk()), f.exp(hostidentity.BodyTypeOperationReceipt)); err == nil {
		t.Fatal("VerifyAnswer accepted a receipt")
	}
}
