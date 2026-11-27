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
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

var testFirstEnrolment = bytes.Repeat([]byte{0x1F}, 16)

// recordedHost is attestedHost whose store records its first attestation.
func recordedHost(p *trusttest.PKI) answer.Host {
	h := attestedHost(p)
	h.View.Attested = journal.StoreAttestation{EverAttested: true, FirstSeq: 5, FirstEnrolmentID: testFirstEnrolment, RecordSeq: 6}
	return h
}

// decodeSigned signs body, checks the signature as a verifier does, decodes
// it into out and returns the store's attestation record of its ledger
// state as the manager's decoder reads it.
func decodeSigned(t *testing.T, p *trusttest.PKI, body proto.Message, out proto.Message, ledger func() *providerv1.HostLedgerState) hostidentity.StoreAttestation {
	t.Helper()
	blob, err := answer.Sign(p.HostKey, leafDER(t, p), body)
	if err != nil {
		t.Fatal(err)
	}
	verify(t, blob, out)
	if err := proto.Unmarshal(blob.GetBody(), out); err != nil {
		t.Fatal(err)
	}
	rec, err := hostidentity.StoreAttestationOf(ledger())
	if err != nil {
		t.Fatalf("the manager refuses the signed ledger state: %v", err)
	}
	return rec
}

func wantReported(t *testing.T, got hostidentity.StoreAttestation) {
	t.Helper()
	if !got.Reported || !got.EverAttested || got.FirstSeq != 5 || got.RecordSeq != 6 || !bytes.Equal(got.FirstEnrolmentID, testFirstEnrolment) {
		t.Fatalf("store attestation %+v", got)
	}
}

// Every signed ledger state of version 2 carries the store's attestation
// record: the challenge answer and a host answer that asked for version 2
// or higher. A host answer that asked for version 1 carries none.
func TestLedgerState_ReportsStoreAttestation(t *testing.T) {
	p := trusttest.New(t)
	h := recordedHost(p)

	ch, err := answer.ChallengeAnswer(answer.Challenge{Host: h, Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_REATTEST,
		Nonce: bytes.Repeat([]byte{3}, 16)})
	if err != nil {
		t.Fatal(err)
	}
	gotCh := &providerv1.ChallengeAnswer{}
	wantReported(t, decodeSigned(t, p, ch, gotCh, gotCh.GetLedger))

	for _, v := range []uint32{2, 3, 1 << 31} {
		h.LedgerRequested = v
		a, err := answer.HostAnswer(h)
		if err != nil {
			t.Fatal(err)
		}
		got := &providerv1.HostAnswer{}
		wantReported(t, decodeSigned(t, p, a, got, got.GetLedger))
	}

	h.LedgerRequested = 1
	a, err := answer.HostAnswer(h)
	if err != nil {
		t.Fatal(err)
	}
	got := &providerv1.HostAnswer{}
	if rec := decodeSigned(t, p, a, got, got.GetLedger); rec.Reported || rec.EverAttested {
		t.Fatalf("a version 1 ledger state reports the record: %+v", rec)
	}
	if l := got.GetLedger(); l.GetStoreEverAttested() || l.GetStoreFirstAttestedSeq() != 0 || l.GetStoreAttestedRecordSeq() != 0 ||
		len(l.GetStoreFirstAttestedEnrolmentId()) != 0 {
		t.Fatalf("version 1 ledger state %v", l)
	}
}

// A store that records no attestation reports false, explicitly, at
// version 2.
func TestLedgerState_NeverAttestedReported(t *testing.T) {
	p := trusttest.New(t)
	h := attestedHost(p)
	h.Attested = false
	ch, err := answer.ChallengeAnswer(answer.Challenge{Host: h, Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST,
		Nonce: bytes.Repeat([]byte{3}, 16)})
	if err != nil {
		t.Fatal(err)
	}
	got := &providerv1.ChallengeAnswer{}
	if rec := decodeSigned(t, p, ch, got, got.GetLedger); !rec.Reported || rec.EverAttested {
		t.Fatalf("never attested: %+v", rec)
	}
}

// The agent never signs a ledger state whose attestation record breaks the
// manager's field rules.
func TestSign_StoreAttestationRules(t *testing.T) {
	p := trusttest.New(t)
	h := recordedHost(p)
	ch := func() *providerv1.ChallengeAnswer {
		a, err := answer.ChallengeAnswer(answer.Challenge{Host: h, Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST,
			Nonce: bytes.Repeat([]byte{3}, 16)})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	host := func() *providerv1.HostAnswer {
		hh := h
		hh.LedgerRequested = 2
		a, err := answer.HostAnswer(hh)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	for name, body := range map[string]func() proto.Message{
		"challenge: facts without the flag": func() proto.Message { a := ch(); a.Ledger.StoreEverAttested = false; return a },
		"challenge: flag without a position": func() proto.Message {
			a := ch()
			a.Ledger.StoreFirstAttestedSeq = 0
			return a
		},
		"challenge: record not above the position": func() proto.Message {
			a := ch()
			a.Ledger.StoreAttestedRecordSeq = a.Ledger.StoreFirstAttestedSeq
			return a
		},
		"challenge: short enrolment": func() proto.Message { a := ch(); a.Ledger.StoreFirstAttestedEnrolmentId = []byte{1}; return a },
		"challenge: record at version 1": func() proto.Message {
			a := ch()
			a.Ledger.Version = 1
			return a
		},
		"host: record at version 1": func() proto.Message {
			a := host()
			a.Ledger.Version = 1
			return a
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := answer.Sign(p.HostKey, leafDER(t, p), body()); !errors.Is(err, answer.ErrSign) {
				t.Fatalf("Sign = %v, want ErrSign", err)
			}
		})
	}
}
