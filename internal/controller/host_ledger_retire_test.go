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
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

var (
	retireStore     = [16]byte{0x51}
	retireSession   = [16]byte{0x5e}
	retireEnrolment = bytes.Repeat([]byte{0xe1}, 16)
	retirePrevEnrol = bytes.Repeat([]byte{0xe0}, 16)
	retireOp        = bytes.Repeat([]byte{0x0a}, 16)
)

// retireFixture is evidence on which every retirement precondition holds.
func retireFixture() retireEvidence {
	return retireEvidence{
		attempt: &infravirtrigaudiov1beta1.OperationAttempt{Phase: infravirtrigaudiov1beta1.OperationAttemptPhaseSent},
		prepared: contracts.PreparedOperation{Identity: contracts.OperationAttemptIdentity{OpID: retireOp, EnrolmentID: retireEnrolment},
			LedgerStoreID: retireStore[:], LedgerGeneration: 7, RequestBytes: []byte("request"), DigestSalt: bytes.Repeat([]byte{5}, 32)},
		answer: hostidentity.VerifiedAnswer{
			Context: &providerv1.AnswerContext{AgentState: providerv1.AgentState_AGENT_STATE_READY, EnrolmentId: retireEnrolment,
				AttestationId: retireSession[:]},
			Operation: &providerv1.OperationAnswer{OpId: retireOp, OpEnrolmentId: retireEnrolment,
				State: providerv1.OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD},
			Position: hostidentity.ChainPosition{StoreID: retireStore, Seq: 120},
		},
		ledger: &HostLedgerRecord{StoreID: retireStore, AttestationState: infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested,
			AttestationID: retireSession, Generation: 7, GenerationEnrolments: [][]byte{retirePrevEnrol, retireEnrolment},
			Position: HostLedgerPosition{Seq: 100}, SessionHistory: []HostLedgerFencedSession{{ID: [16]byte{0x01}, FencedAt: 40}}},
	}
}

// frozenFixture is evidence on which every precondition of the
// frozen-predecessor closure holds: the attempt was prepared under the
// predecessor enrolment, which the host's active enrolment replaced.
func frozenFixture() retireEvidence {
	e := retireFixture()
	e.frozen = true
	e.prepared.Identity.EnrolmentID = retirePrevEnrol
	e.answer.Operation.OpEnrolmentId = retirePrevEnrol
	return e
}

// receiptAt is a bound marker receipt of the store in session at seq.
func receiptAt(session [16]byte, seq int64) *infravirtrigaudiov1beta1.OperationAttemptReceipt {
	return &infravirtrigaudiov1beta1.OperationAttemptReceipt{StoreID: hex.EncodeToString(retireStore[:]), Seq: seq,
		AttestationID: hex.EncodeToString(session[:])}
}

// Every retirement precondition is needed: with any one removed, the
// attempt is not retired (asked again, held, or left to the re-send rules).
func TestDecideRetire_EveryPrecondition(t *testing.T) {
	v, why := decideRetire(retireFixture())
	require.Equal(t, retireAllowed, v, why)
	cases := map[string]struct {
		edit func(e *retireEvidence)
		want retireVerdict
	}{
		"attempt terminal":       {func(e *retireEvidence) { e.attempt.Phase = infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal }, retireAskAgain},
		"no attempt":             {func(e *retireEvidence) { e.attempt = nil }, retireAskAgain},
		"intent without request": {func(e *retireEvidence) { e.prepared.RequestBytes = nil }, retireAskAgain},
		"receipt tag fails":      {func(e *retireEvidence) { e.attempt.Receipt = receiptAt(retireSession, 200) }, retireHoldReceipt},
		"receipt ledgered":       {func(e *retireEvidence) { e.attempt.Receipt, e.receiptBound = receiptAt(retireSession, 90), true }, retireResend},
		"older session receipt":  {func(e *retireEvidence) { e.attempt.Receipt, e.receiptBound = receiptAt([16]byte{9}, 1), true }, retireResend},
		"fenced receipt below":   {func(e *retireEvidence) { e.attempt.Receipt, e.receiptBound = receiptAt([16]byte{0x01}, 40), true }, retireResend},
		"agent not READY": {func(e *retireEvidence) {
			e.answer.Context.AgentState = providerv1.AgentState_AGENT_STATE_UNATTESTED
		}, retireAskAgain},
		"not NO_RECORD": {func(e *retireEvidence) {
			e.answer.Operation.State = providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNKNOWN_TO_AGENT
		}, retireAskAgain},
		"no operation answer":    {func(e *retireEvidence) { e.answer.Operation = nil }, retireAskAgain},
		"another operation":      {func(e *retireEvidence) { e.answer.Operation.OpId = bytes.Repeat([]byte{1}, 16) }, retireAskAgain},
		"a predecessor's answer": {func(e *retireEvidence) { e.answer.Context.EnrolmentId = retirePrevEnrol }, retireAskAgain},
		"no ledger":              {func(e *retireEvidence) { e.ledger = nil }, retireAskAgain},
		"ledger archived":        {func(e *retireEvidence) { e.ledger.Archived = true }, retireAskAgain},
		"ledger pending": {func(e *retireEvidence) {
			e.ledger.AttestationState = infravirtrigaudiov1beta1.HostAgentLedgerAttestationPending
		}, retireAskAgain},
		"ledger of another store":  {func(e *retireEvidence) { e.answer.Position.StoreID = [16]byte{0x52} }, retireAskAgain},
		"not the adopted session":  {func(e *retireEvidence) { e.answer.Context.AttestationId = bytes.Repeat([]byte{2}, 16) }, retireAskAgain},
		"no session":               {func(e *retireEvidence) { e.answer.Context.AttestationId = nil }, retireAskAgain},
		"intent without a ledger":  {func(e *retireEvidence) { e.prepared.LedgerGeneration, e.prepared.LedgerStoreID = 0, nil }, retireResend},
		"intent of another gen":    {func(e *retireEvidence) { e.prepared.LedgerGeneration = 8 }, retireResend},
		"intent of another store":  {func(e *retireEvidence) { e.prepared.LedgerStoreID = bytes.Repeat([]byte{0x52}, 16) }, retireResend},
		"enrolment not in the gen": {func(e *retireEvidence) { e.ledger.GenerationEnrolments = [][]byte{retirePrevEnrol} }, retireResend},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := retireFixture()
			c.edit(&e)
			got, why := decideRetire(e)
			assert.Equal(t, c.want, got, why)
			assert.NotEmpty(t, why)
		})
	}
	// A receipt of a fenced session above where it was fenced provably never
	// had a mark: it does not stop the retirement.
	e := retireFixture()
	e.attempt.Receipt, e.receiptBound = receiptAt([16]byte{0x01}, 41), true
	v, why = decideRetire(e)
	assert.Equal(t, retireAllowed, v, why)
	// A receipt above the ledger in the current session was never covered.
	e = retireFixture()
	e.attempt.Receipt, e.receiptBound = receiptAt(retireSession, 101), true
	v, why = decideRetire(e)
	assert.Equal(t, retireAllowed, v, why)
}

// The frozen-predecessor closure needs every precondition too; the
// predecessor must be one of the generation's enrolments, and the intent's
// store and generation the ledger's.
func TestDecideRetire_FrozenPredecessor(t *testing.T) {
	v, why := decideRetire(frozenFixture())
	require.Equal(t, retireAllowed, v, why)
	cases := map[string]func(e *retireEvidence){
		"predecessor not in the gen": func(e *retireEvidence) { e.ledger.GenerationEnrolments = [][]byte{retireEnrolment} },
		"intent of another gen":      func(e *retireEvidence) { e.prepared.LedgerGeneration = 6 },
		"intent without a ledger":    func(e *retireEvidence) { e.prepared.LedgerGeneration = 0 },
		"receipt ledgered":           func(e *retireEvidence) { e.attempt.Receipt, e.receiptBound = receiptAt(retireSession, 10), true },
		"receipt tag fails":          func(e *retireEvidence) { e.attempt.Receipt = receiptAt(retireSession, 10) },
		"not the adopted session":    func(e *retireEvidence) { e.answer.Context.AttestationId = bytes.Repeat([]byte{2}, 16) },
		"agent not READY":            func(e *retireEvidence) { e.answer.Context.AgentState = providerv1.AgentState_AGENT_STATE_UNATTESTED },
		"an active enrolment answer": func(e *retireEvidence) { e.answer.Operation.OpEnrolmentId = retireEnrolment },
		"a RETIRED predecessor": func(e *retireEvidence) {
			e.answer.Operation.State = providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			e := frozenFixture()
			edit(&e)
			got, why := decideRetire(e)
			assert.NotEqual(t, retireAllowed, got, why)
		})
	}
}

// sendRetire signs only for an attempt continuous with an Attested,
// current ledger in the session named: an intent recreated after a ledger
// restart (another generation in its authenticated intent), or one
// authenticated under the first intent version (generation 0), is never
// retired.
func TestSendRetire_OnlyContinuous(t *testing.T) {
	h := &HostAnswers{Ledger: &HostLedger{}}
	auth := &countingAuthority{}
	good := retireFixture()
	good.prepared.Kind = contracts.OperationKindCreate
	_, err := h.sendRetire(context.Background(), auth, good.ledger, good.prepared, retireSession)
	require.NoError(t, err)
	require.Equal(t, 1, auth.retires)
	cases := map[string]func(e *retireEvidence, s *[16]byte){
		"ledger restarted":     func(e *retireEvidence, _ *[16]byte) { e.ledger.Generation = 8 },
		"first intent version": func(e *retireEvidence, _ *[16]byte) { e.prepared.LedgerGeneration = 0 },
		"another store":        func(e *retireEvidence, _ *[16]byte) { e.prepared.LedgerStoreID = bytes.Repeat([]byte{3}, 16) },
		"enrolment not in gen": func(e *retireEvidence, _ *[16]byte) { e.ledger.GenerationEnrolments = [][]byte{retirePrevEnrol} },
		"archived":             func(e *retireEvidence, _ *[16]byte) { e.ledger.Archived = true },
		"pending": func(e *retireEvidence, _ *[16]byte) {
			e.ledger.AttestationState = infravirtrigaudiov1beta1.HostAgentLedgerAttestationPending
		},
		"another session":        func(_ *retireEvidence, s *[16]byte) { *s = [16]byte{7} },
		"no session":             func(e *retireEvidence, s *[16]byte) { *s, e.ledger.AttestationID = [16]byte{}, [16]byte{} },
		"kind without wire form": func(e *retireEvidence, _ *[16]byte) { e.prepared.Kind = "Nope" },
		"no request bytes":       func(e *retireEvidence, _ *[16]byte) { e.prepared.RequestBytes = nil },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			e := retireFixture()
			e.prepared.Kind = contracts.OperationKindCreate
			s := retireSession
			edit(&e, &s)
			_, err := h.sendRetire(context.Background(), auth, e.ledger, e.prepared, s)
			require.ErrorIs(t, err, errNotRetirable)
		})
	}
	assert.Equal(t, 1, auth.retires, "nothing else was signed")
	_, err = (&HostAnswers{}).sendRetire(context.Background(), auth, good.ledger, good.prepared, retireSession)
	require.ErrorIs(t, err, errNotRetirable, "no ledger")
}

// A retirement refusal that asking again does not change is held at the
// hold re-check interval; any other refusal is asked again soon.
func TestRetireRefusalHold(t *testing.T) {
	for _, r := range []string{hostidentity.AuthorityReasonOpKnown, hostidentity.AuthorityReasonEnrolmentNotActive,
		hostidentity.AuthorityReasonTraceScanUnavailable} {
		msg, hold := retireRefusalHold(r)
		assert.True(t, hold, r)
		assert.NotEmpty(t, msg, r)
	}
	for _, r := range []string{"store_busy", "grant_invalid", "not_attested", ""} {
		_, hold := retireRefusalHold(r)
		assert.False(t, hold, r)
	}
}
