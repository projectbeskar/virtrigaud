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
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// The host ledger fields are reserved: generated code knows them, so the
// unknown-field rule no longer refuses them, and nothing implements them.
// These tests pin that each one, when set, makes the answer "no answer", and
// that an answer without them verifies exactly as before.

func (f *answerFixture) vmAnswer() *providerv1.VMAnswer {
	return &providerv1.VMAnswer{Context: f.ctx(), VmUid: "vm-uid-1", BindingId: bytes.Repeat([]byte{2}, 16),
		Presence: providerv1.VMPresence_VM_PRESENCE_PRESENT}
}

func (f *answerFixture) hostAnswer() *providerv1.HostAnswer {
	return &providerv1.HostAnswer{Context: f.ctx(), AgentApiVersion: 4, GrantVersions: []uint32{1},
		Mode: providerv1.AgentMode_AGENT_MODE_IDENTITY}
}

// signBody signs the deterministic encoding of body as a host would, without
// the agent's own refusal of reserved fields.
func (f *answerFixture) signBody(t *testing.T, body proto.Message) *providerv1.SignedBlob {
	t.Helper()
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return signRaw(t, f.pki.HostKey, f.cert, string(body.ProtoReflect().Descriptor().FullName()), raw)
}

// reservedLedgerCase sets one reserved field on a fresh, otherwise valid
// answer.
type reservedLedgerCase struct {
	name string
	body func(f *answerFixture) proto.Message
}

func reservedLedgerCases() []reservedLedgerCase {
	ctxEdits := []struct {
		name string
		set  func(*providerv1.AnswerContext)
	}{
		{"context.attestation_id", func(c *providerv1.AnswerContext) { c.AttestationId = bytes.Repeat([]byte{9}, 16) }},
		{"context.attestation_id one byte", func(c *providerv1.AnswerContext) { c.AttestationId = []byte{0} }},
		{"context.ledger_version_echo", func(c *providerv1.AnswerContext) { c.LedgerVersionEcho = 1 }},
	}
	var out []reservedLedgerCase
	for _, e := range ctxEdits {
		out = append(out,
			reservedLedgerCase{"OperationAnswer " + e.name, func(f *answerFixture) proto.Message { a := f.opAnswer(); e.set(a.Context); return a }},
			reservedLedgerCase{"VMAnswer " + e.name, func(f *answerFixture) proto.Message { a := f.vmAnswer(); e.set(a.Context); return a }},
			reservedLedgerCase{"HostAnswer " + e.name, func(f *answerFixture) proto.Message { a := f.hostAnswer(); e.set(a.Context); return a }},
		)
	}
	return append(out,
		reservedLedgerCase{"OperationAnswer accepted_seq", func(f *answerFixture) proto.Message { a := f.opAnswer(); a.AcceptedSeq = 7; return a }},
		reservedLedgerCase{"OperationAnswer accepted_head", func(f *answerFixture) proto.Message {
			a := f.opAnswer()
			a.AcceptedHead = bytes.Repeat([]byte{0xcd}, 32)
			return a
		}},
		reservedLedgerCase{"OperationAnswer awaiting_request", func(f *answerFixture) proto.Message { a := f.opAnswer(); a.AwaitingRequest = true; return a }},
		reservedLedgerCase{"HostAnswer ledger", func(f *answerFixture) proto.Message {
			a := f.hostAnswer()
			a.Ledger = &providerv1.HostLedgerState{RequestedVersion: 1, Version: 1,
				State: providerv1.LedgerSessionState_LEDGER_SESSION_STATE_AWAITING_ATTESTATION}
			return a
		}},
		reservedLedgerCase{"HostAnswer ledger empty", func(f *answerFixture) proto.Message {
			a := f.hostAnswer()
			a.Ledger = &providerv1.HostLedgerState{}
			return a
		}},
	)
}

// TestVerifyAnswer_RefusesReservedLedgerFields: a validly signed answer that
// sets any reserved ledger field is refused, and CheckNoLedgerFields says so
// on its own.
func TestVerifyAnswer_RefusesReservedLedgerFields(t *testing.T) {
	f := newAnswerFixture(t)
	for _, c := range reservedLedgerCases() {
		t.Run(c.name, func(t *testing.T) {
			body := c.body(f)
			bodyType := string(body.ProtoReflect().Descriptor().FullName())
			_, err := hostidentity.VerifyAnswer(f.signBody(t, body), f.exp(bodyType))
			if !errors.Is(err, hostidentity.ErrAnswer) || !strings.Contains(err.Error(), "reserved ledger field") {
				t.Fatalf("VerifyAnswer = %v, want ErrAnswer for a reserved ledger field", err)
			}
			if err := hostidentity.CheckNoLedgerFields(body); !errors.Is(err, hostidentity.ErrAnswer) {
				t.Fatalf("CheckNoLedgerFields = %v, want ErrAnswer", err)
			}
		})
	}
}

// TestVerifyAnswer_WithoutReservedLedgerFieldsAsBefore: the same answers
// without the reserved fields verify, as they did before the fields existed.
func TestVerifyAnswer_WithoutReservedLedgerFieldsAsBefore(t *testing.T) {
	f := newAnswerFixture(t)
	for _, body := range []proto.Message{f.opAnswer(), f.vmAnswer(), f.hostAnswer()} {
		bodyType := string(body.ProtoReflect().Descriptor().FullName())
		t.Run(bodyType, func(t *testing.T) {
			if err := hostidentity.CheckNoLedgerFields(body); err != nil {
				t.Fatalf("CheckNoLedgerFields = %v", err)
			}
			if _, err := hostidentity.VerifyAnswer(f.signBody(t, body), f.exp(bodyType)); err != nil {
				t.Fatalf("VerifyAnswer = %v", err)
			}
		})
	}
}

// TestVerifyAnswer_RefusesReservedBodyTypes: a receipt is never an answer,
// and a challenge or authority answer is refused when the request expected
// another body type or the body breaks its rules.
func TestVerifyAnswer_RefusesReservedBodyTypes(t *testing.T) {
	f := newAnswerFixture(t)
	bodies := map[string]proto.Message{
		hostidentity.BodyTypeOperationReceipt: &providerv1.OperationReceipt{ReceiptVersion: 1},
		hostidentity.BodyTypeChallengeAnswer:  &providerv1.ChallengeAnswer{Context: f.ctx()},
		hostidentity.BodyTypeAuthorityAnswer:  &providerv1.AuthorityAnswer{Context: f.ctx()},
	}
	if len(hostidentity.ReservedBodyTypes) != len(bodies) {
		t.Fatalf("%d reserved body types, want %d", len(hostidentity.ReservedBodyTypes), len(bodies))
	}
	for _, bt := range hostidentity.ReservedBodyTypes {
		body, ok := bodies[bt]
		if !ok {
			t.Fatalf("no fixture for reserved body type %q", bt)
		}
		if got := string(body.ProtoReflect().Descriptor().FullName()); got != bt {
			t.Fatalf("reserved body type %q is not the full name %q of its message", bt, got)
		}
		for _, expected := range []string{bt, hostidentity.BodyTypeOperationAnswer, hostidentity.BodyTypeHostAnswer} {
			t.Run(bt+" expecting "+expected, func(t *testing.T) {
				if _, err := hostidentity.VerifyAnswer(f.signBody(t, body), f.exp(expected)); !errors.Is(err, hostidentity.ErrAnswer) {
					t.Fatalf("VerifyAnswer = %v, want ErrAnswer", err)
				}
			})
		}
		if err := hostidentity.CheckNoLedgerFields(body); !errors.Is(err, hostidentity.ErrAnswer) {
			t.Errorf("CheckNoLedgerFields(%s) = %v, want ErrAnswer", bt, err)
		}
	}
	for _, bt := range []string{hostidentity.BodyTypeOperationAnswer, hostidentity.BodyTypeVMAnswer, hostidentity.BodyTypeHostAnswer} {
		if slices.Contains(hostidentity.ReservedBodyTypes, bt) {
			t.Errorf("accepted body type %q is listed as reserved", bt)
		}
	}
}
