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

package server_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// attestAt attests the agent at the store's position and returns the
// session.
func (e *authEnv) attestAt(t *testing.T) []byte {
	t.Helper()
	c := e.challenge(t, pAttest).GetChallengeNonce()
	wantOutcome(t, e.attest(t, e.attestGrant(t, c, e.store.Position(), nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, "")
	return c
}

// ledgerMarkGrant mints a ledger mark grant for session c at l.
func (e *authEnv) ledgerMarkGrant(t *testing.T, c []byte, l journal.Position, edit func(*operation.LedgerMarkMintRequest)) *providerv1.OperationGrant {
	t.Helper()
	r := operation.LedgerMarkMintRequest{ProviderUID: e.pki.Record.ProviderUID, HostID: trusttest.HostName,
		EnrolmentID: e.pki.Record.EnrolmentID, Epoch: e.epoch, AttestationID: c,
		Ledger:   operation.LedgerPosition{StoreID: l.StoreID[:], Seq: l.Seq, Head: l.Head[:]},
		IssuedAt: time.Now()}
	if edit != nil {
		edit(&r)
	}
	g, err := operation.MintLedgerMarkGrant(context.Background(), e.grants, r)
	require.NoError(t, err)
	return g
}

// ledgerMark delivers g on LedgerMark and returns the verified answer.
func (e *authEnv) ledgerMark(t *testing.T, g *providerv1.OperationGrant) *providerv1.AuthorityAnswer {
	t.Helper()
	resp, err := e.ha.LedgerMark(ctx(t), &providerv1.LedgerMarkRequest{TargetHostId: trusttest.HostName, AnswerNonce: nonce(0x33), Grant: g})
	require.NoError(t, err)
	a := &providerv1.AuthorityAnswer{}
	verifyBlob(t, e.opsEnv, resp.GetHostAnswer(), answer.BodyTypeAuthorityAnswer, a)
	require.Equal(t, nonce(0x33), a.GetContext().GetNonce())
	return a
}

// markTip raises the ledger mark of the current session to the store's
// position, as a manager's heartbeat raise does, and returns the position.
func (e *authEnv) markTip(t *testing.T) journal.Position {
	t.Helper()
	a, ok := e.store.Attestation()
	require.True(t, ok, "not attested")
	pos := e.store.Position()
	got := e.ledgerMark(t, e.ledgerMarkGrant(t, a.Session, pos, nil))
	require.Contains(t, []providerv1.AuthorityOutcome{providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED,
		providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_ALREADY_APPLIED}, got.GetOutcome(), "ledger mark: %v", got)
	return pos
}

// create sends a Create of o with a fresh grant (expect_known when set) and
// returns the response and the grant.
func (e *authEnv) create(t *testing.T, o *op, expectKnown bool) (*providerv1.CreateResponse, *providerv1.OperationGrant, error) {
	t.Helper()
	g, err := operation.MintOperationGrant(context.Background(), e.grants, operation.MintRequest{
		Identity: o.id, DigestSalt: o.salt, Request: o.request, IssuedAt: time.Now(), GrantVersion: operation.LedgerGrantVersion,
		ExpectKnown: expectKnown})
	require.NoError(t, err)
	resp := &providerv1.CreateResponse{}
	err = e.cc().Invoke(ctx(t), providerv1.Provider_Create_FullMethodName,
		&providerv1.CreateRequest{OperationEnvelope: &providerv1.OperationEnvelope{Request: o.request, Grant: g}}, resp)
	return resp, g, err
}

// receiptOf verifies the receipt of resp.
func (e *authEnv) receiptOf(t *testing.T, resp *providerv1.CreateResponse) *providerv1.OperationReceipt {
	t.Helper()
	r := &providerv1.OperationReceipt{}
	verifyBlob(t, e.opsEnv, resp.GetOperationReceipt(), answer.BodyTypeOperationReceipt, r)
	return r
}

// ledgerStatus is the ledger-aware signed TaskStatus answer about o.
func (e *authEnv) ledgerStatus(t *testing.T, o *op) *providerv1.OperationAnswer {
	t.Helper()
	ts, err := e.client().TaskStatus(ctx(t), &providerv1.TaskStatusRequest{AnswerNonce: nonce(0x34), OpId: o.id.OpID,
		OpEnrolmentId: o.id.EnrolmentID, TargetHostId: trusttest.HostName, LedgerVersion: 1})
	require.NoError(t, err)
	oa := &providerv1.OperationAnswer{}
	verifyBlob(t, e.opsEnv, ts.GetHostAnswer(), answer.BodyTypeOperationAnswer, oa)
	return oa
}

// hostLedger is the ledger state of a ledger-aware GetHostInfo.
func (e *authEnv) hostLedger(t *testing.T) *providerv1.HostLedgerState {
	t.Helper()
	info, err := e.client().GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(0x35), LedgerVersion: 1})
	require.NoError(t, err)
	return verifyAnswer(t, e.env, info.GetHostAnswer()).GetLedger()
}

// TestReceipt_ThenLedgerMarkRuns: an accepted operation is answered with a
// receipt that names where it was accepted, the session and the delivery;
// it does not run until a ledger mark covers it; a re-delivery while it is
// still accepted gets a receipt bound to its own grant.
func TestReceipt_ThenLedgerMarkRuns(t *testing.T) {
	e := newAuthEnv(t)
	c := e.attestAt(t)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	resp, g, err := e.create(t, o, false)
	require.NoError(t, err)
	r := e.receiptOf(t, resp)
	tip := e.store.Position()
	assert.Equal(t, answer.ReceiptVersion, r.GetReceiptVersion())
	assert.Equal(t, c, r.GetAttestationId())
	assert.Equal(t, tip.StoreID[:], r.GetStoreId())
	assert.Equal(t, tip.Seq, r.GetAcceptedSeq())
	assert.Equal(t, tip.Head[:], r.GetAcceptedHead())
	assert.Equal(t, grantverify.GrantDigest(g), r.GetGrantDigest())
	assert.Equal(t, o.id.OpID, r.GetOpId())
	assert.Equal(t, int32(kCreate), r.GetKind())
	assert.Equal(t, o.id.Attempt, r.GetAttempt())
	assert.Equal(t, "vm-1", r.GetVmUid())
	assert.Equal(t, o.id.BindingID, r.GetBindingId())
	assert.Equal(t, e.pki.Record.EnrolmentID, r.GetEnrolmentId())
	assert.Equal(t, e.pki.Record.InstallID, r.GetInstallId())
	assert.Equal(t, e.pki.Record.ProviderUID, r.GetProviderUid())
	assert.Equal(t, trusttest.HostName, r.GetHostId())
	assert.Equal(t, operation.IntentDigest(o.salt, o.request), r.GetIntentDigest())

	// Parked: nothing ran, the answers say where it was accepted.
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, tip, e.store.Position(), "a parked operation committed")
	oa := e.ledgerStatus(t, o)
	assert.Equal(t, providerv1.OperationRecordState_OPERATION_RECORD_STATE_ACCEPTED, oa.GetState())
	assert.Equal(t, tip.Seq, oa.GetAcceptedSeq())
	assert.Equal(t, tip.Head[:], oa.GetAcceptedHead())
	assert.False(t, oa.GetAwaitingRequest(), "the agent holds the request")
	l := e.hostLedger(t)
	assert.Equal(t, uint32(1), l.GetPendingReceipts())
	assert.Equal(t, uint64(len(o.request)), l.GetParkedRequestBytes())
	assert.Equal(t, uint64(opstore.DefaultParkedRequestBudget), l.GetParkedRequestBudgetBytes())

	// A re-delivery while accepted: the same acceptance, its own grant.
	again, g2, err := e.create(t, o, false)
	require.NoError(t, err)
	r2 := e.receiptOf(t, again)
	assert.Equal(t, tip.Seq, r2.GetAcceptedSeq())
	assert.Equal(t, grantverify.GrantDigest(g2), r2.GetGrantDigest())
	assert.NotEqual(t, r.GetGrantDigest(), r2.GetGrantDigest())

	a := e.ledgerMark(t, e.ledgerMarkGrant(t, c, tip, nil))
	wantOutcome(t, a, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, "")
	assert.Equal(t, tip.Seq, a.GetMark().GetSeq())
	assert.Equal(t, int32(providerv1.GrantKind_GRANT_KIND_LEDGER_MARK), a.GetGrantKind())
	assert.Equal(t, c, a.GetContext().GetAttestationId())
	e.waitDone(resp.GetTask().GetId())
	assert.Zero(t, e.hostLedger(t).GetPendingReceipts())

	// Over: no receipt.
	done, _, err := e.create(t, o, false)
	require.NoError(t, err)
	assert.Nil(t, done.GetOperationReceipt(), "a receipt for an operation that is no longer accepted")
}

// TestLedgerMark_Refusals: a ledger mark is refused before attestation, for
// another session or with a grant that does not verify; a head that is not
// the store's quarantines it.
func TestLedgerMark_Refusals(t *testing.T) {
	e := newAuthEnv(t)
	pos := e.store.Position()
	wantOutcome(t, e.ledgerMark(t, e.ledgerMarkGrant(t, randID(), pos, nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED,
		answer.ReasonNotAttested)
	c := e.attestAt(t)
	stale := e.ledgerMark(t, e.ledgerMarkGrant(t, randID(), pos, nil))
	wantOutcome(t, stale, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED, answer.ReasonStaleAttestation)
	assert.Equal(t, c, stale.GetContext().GetAttestationId())
	for name, edit := range map[string]func(*operation.LedgerMarkMintRequest){
		"another enrolment": func(r *operation.LedgerMarkMintRequest) { r.EnrolmentID = randID() },
		"another host":      func(r *operation.LedgerMarkMintRequest) { r.HostID = "host-b" },
		"expired": func(r *operation.LedgerMarkMintRequest) {
			r.IssuedAt = time.Now().Add(-time.Hour)
			r.Lifetime = time.Minute
		},
	} {
		wantOutcome(t, e.ledgerMark(t, e.ledgerMarkGrant(t, c, pos, edit)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED,
			answer.ReasonGrantInvalid)
		_ = name
	}
	// An attestation grant on LedgerMark is refused on its kind, and so is
	// an operation grant.
	ch := e.challenge(t, pReattest).GetChallengeNonce()
	wantOutcome(t, e.ledgerMark(t, e.attestGrant(t, ch, pos, nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED,
		answer.ReasonGrantInvalid)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	wantOutcome(t, e.ledgerMark(t, e.grant(o, time.Now(), 0)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED,
		answer.ReasonGrantInvalid)
	// The same mark twice: already applied.
	wantOutcome(t, e.ledgerMark(t, e.ledgerMarkGrant(t, c, pos, nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_ALREADY_APPLIED, "")
	if a, _ := e.store.Attestation(); !bytes.Equal(a.Session, c) || a.Mark != pos {
		t.Fatalf("a refused mark changed the session: %+v", a)
	}

	bad := pos
	bad.Head[0] ^= 1
	q := e.ledgerMark(t, e.ledgerMarkGrant(t, c, bad, nil))
	wantOutcome(t, q, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_QUARANTINED, string(journal.LostLedgerMismatch))
	st, r := e.store.Journal().State()
	require.Equal(t, journal.StateLost, st)
	require.Equal(t, journal.LostLedgerMismatch, r)
}

// TestLedgerBehind_Refused: past the bound of accepted operations above the
// mark, a new operation is refused RESOURCE_EXHAUSTED LEDGER_BEHIND
// (definitely not performed); a mark lifts it.
func TestLedgerBehind_Refused(t *testing.T) {
	e := newAuthEnvWith(t, func(o *opstore.Options) { o.MaxPendingReceipts = 1 })
	e.attestAt(t)
	_, err := e.send(e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil)))
	require.NoError(t, err)
	o2 := e.newOp(kCreate, "vm-2", randID(), 1, createReq("vm-2", nil))
	_, err = e.send(o2)
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
	assert.Equal(t, server.ReasonLedgerBehind, reasonOf(err))
	e.markTip(t)
	_, err = e.send(o2)
	require.NoError(t, err)
}

// TestExpectKnown_Refused: a grant that says the manager holds a ledgered
// receipt for an op_id the host knows nothing of is refused
// UNKNOWN_TO_AGENT, never accepted; for a known op_id it is answered as
// usual.
func TestExpectKnown_Refused(t *testing.T) {
	e := newAuthEnv(t)
	e.attestAt(t)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	before := e.store.Position()
	_, _, err := e.create(t, o, true)
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
	assert.Equal(t, opstore.ReasonUnknownToAgent, reasonOf(err))
	assert.Equal(t, before, e.store.Position(), "an expect_known refusal wrote")
	assert.Contains(t, strings.Join(e.logs.all(), "\n"), opstore.AuditExpectKnownUnknown)

	resp, _, err := e.create(t, o, false)
	require.NoError(t, err)
	again, _, err := e.create(t, o, true)
	require.NoError(t, err)
	assert.Equal(t, resp.GetTask().GetId(), again.GetTask().GetId())
}

// TestReceipt_RateLimited: a delivery above the per-identity receipt limit
// is answered without a receipt; the operation is accepted all the same.
func TestReceipt_RateLimited(t *testing.T) {
	e := newAuthEnvWith(t, nil, func(o *server.Options) { o.ReceiptsPerSecond, o.ReceiptBurst = 0.001, 1 })
	e.attestAt(t)
	first, _, err := e.create(t, e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil)), false)
	require.NoError(t, err)
	assert.NotNil(t, first.GetOperationReceipt())
	second, _, err := e.create(t, e.newOp(kCreate, "vm-2", randID(), 1, createReq("vm-2", nil)), false)
	require.NoError(t, err)
	assert.Nil(t, second.GetOperationReceipt())
	assert.NotEmpty(t, second.GetTask().GetId())
}

// TestReceipt_NoneWithoutAuthority: an agent that does not take part in the
// ledger protocol signs no receipt.
func TestReceipt_NoneWithoutAuthority(t *testing.T) {
	e := newOpsEnv(t)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	resp := &providerv1.CreateResponse{}
	require.NoError(t, e.cc().Invoke(ctx(t), providerv1.Provider_Create_FullMethodName,
		&providerv1.CreateRequest{OperationEnvelope: &providerv1.OperationEnvelope{Request: o.request, Grant: e.grant(o, time.Now(), 0)}}, resp))
	assert.Nil(t, resp.GetOperationReceipt())
}
