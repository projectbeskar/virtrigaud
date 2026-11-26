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
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestSign_RefusesReservedLedgerFields: the agent implements none of the
// fields reserved for the host ledger protocol, so it never signs an answer
// that sets one, whatever built the answer; nor a body of a reserved type.
func TestSign_RefusesReservedLedgerFields(t *testing.T) {
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
	for name, body := range map[string]func() proto.Message{
		"host context.attestation_id":      func() proto.Message { a := host(); a.Context.AttestationId = bytes.Repeat([]byte{3}, 16); return a },
		"host context.ledger_version_echo": func() proto.Message { a := host(); a.Context.LedgerVersionEcho = 1; return a },
		"host ledger":                      func() proto.Message { a := host(); a.Ledger = &providerv1.HostLedgerState{}; return a },
		"operation context.attestation_id": func() proto.Message { a := op(); a.Context.AttestationId = []byte{3}; return a },
		"operation accepted_seq":           func() proto.Message { a := op(); a.AcceptedSeq = 1; return a },
		"operation accepted_head":          func() proto.Message { a := op(); a.AcceptedHead = bytes.Repeat([]byte{4}, 32); return a },
		"operation awaiting_request":       func() proto.Message { a := op(); a.AwaitingRequest = true; return a },
		"vm context.ledger_version_echo":   func() proto.Message { a := vm(); a.Context.LedgerVersionEcho = 2; return a },
		"reserved body OperationReceipt":   func() proto.Message { return &providerv1.OperationReceipt{ReceiptVersion: 1} },
		"reserved body ChallengeAnswer":    func() proto.Message { return &providerv1.ChallengeAnswer{Context: host().GetContext()} },
		"reserved body AuthorityAnswer":    func() proto.Message { return &providerv1.AuthorityAnswer{Context: host().GetContext()} },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := answer.Sign(p.HostKey, leafDER(t, p), body()); !errors.Is(err, answer.ErrSign) {
				t.Fatalf("Sign = %v, want ErrSign", err)
			}
		})
	}
	// Without them every answer type is signed as before.
	for _, body := range []proto.Message{host(), op(), vm()} {
		if _, err := answer.Sign(p.HostKey, leafDER(t, p), body); err != nil {
			t.Fatalf("Sign(%T) = %v", body, err)
		}
	}
}

// TestAnswerBuilders_SetNoReservedLedgerField: the answers this agent builds
// leave every reserved ledger field unset (the agent answers as an agent that
// does not implement the host ledger protocol).
func TestAnswerBuilders_SetNoReservedLedgerField(t *testing.T) {
	p := trusttest.New(t)
	h := testHost(p)
	h.StoreCapacityRequested = hostidentity.StoreCapacityVersion
	a, err := answer.HostAnswer(h)
	if err != nil {
		t.Fatal(err)
	}
	if err := hostidentity.CheckNoLedgerFields(a); err != nil {
		t.Fatalf("HostAnswer: %v", err)
	}
}
