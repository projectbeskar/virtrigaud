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
	"crypto/tls"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/attest"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	pAttest   = providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST
	pReattest = providerv1.ChallengePurpose_CHALLENGE_PURPOSE_REATTEST
)

// authProxy is the server's Authority, bound to the environment's store
// once it exists.
type authProxy struct {
	mu sync.Mutex
	st *opstore.Store
}

func (a *authProxy) store() *opstore.Store {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st
}

func (a *authProxy) Attestation() (opstore.Attestation, bool) {
	if st := a.store(); st != nil {
		return st.Attestation()
	}
	return opstore.Attestation{}, false
}

func (a *authProxy) Attest(ctx context.Context, r opstore.AttestRequest) (opstore.AttestResult, error) {
	return a.store().Attest(ctx, r)
}

func (a *authProxy) LedgerMark(ctx context.Context, r opstore.LedgerMarkRequest) (opstore.LedgerMarkResult, error) {
	return a.store().LedgerMark(ctx, r)
}

func (a *authProxy) Backlog() opstore.LedgerBacklog {
	if st := a.store(); st != nil {
		return st.Backlog()
	}
	return opstore.LedgerBacklog{}
}

// rawClock is a settable challenge clock.
type rawClock struct{ now atomic.Int64 }

func (c *rawClock) Now() time.Duration { return time.Duration(c.now.Load()) }

// authEnv is an operation environment whose server serves HostAuthority.
type authEnv struct {
	*opsEnv
	clock *rawClock
	ha    providerv1.HostAuthorityClient
}

func newAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	return newAuthEnvWith(t, nil)
}

// newAuthEnvWith is newAuthEnv with storeMod adjusting the store's options
// (the ledger gate is always on) and mods the server's.
func newAuthEnvWith(t *testing.T, storeMod func(*opstore.Options), mods ...withOptions) *authEnv {
	t.Helper()
	proxy := &authProxy{}
	clock := &rawClock{}
	clock.now.Store(int64(time.Hour))
	all := append([]withOptions{func(o *server.Options) { o.Authority, o.ChallengeClock = proxy, clock }}, mods...)
	e := newOpsEnvStore(t, nil, func(o *opstore.Options) {
		o.LedgerGate = true
		if storeMod != nil {
			storeMod(o)
		}
	}, all...)
	proxy.mu.Lock()
	proxy.st = e.store
	proxy.mu.Unlock()
	a := &authEnv{opsEnv: e, clock: clock, ha: providerv1.NewHostAuthorityClient(e.cc())}
	a.syncView()
	return a
}

// syncView makes the fake agent's host view the store's.
func (e *authEnv) syncView() {
	e.t.Helper()
	v, err := e.store.HostView(context.Background(), 0)
	require.NoError(e.t, err)
	e.agent.mu.Lock()
	e.agent.view = v
	e.agent.mu.Unlock()
}

func (e *authEnv) challenge(t *testing.T, purpose providerv1.ChallengePurpose) *providerv1.ChallengeAnswer {
	t.Helper()
	resp, err := e.ha.Challenge(ctx(t), &providerv1.ChallengeRequest{TargetHostId: trusttest.HostName, AnswerNonce: nonce(0x31), Purpose: purpose})
	require.NoError(t, err)
	a := &providerv1.ChallengeAnswer{}
	verifyBlob(t, e.opsEnv, resp.GetHostAnswer(), answer.BodyTypeChallengeAnswer, a)
	require.Equal(t, nonce(0x31), a.GetContext().GetNonce())
	return a
}

// attestGrant mints an attestation grant for the challenge nonce c at ledger
// position l (with checkpoint cp when set).
func (e *authEnv) attestGrant(t *testing.T, c []byte, l journal.Position, edit func(*operation.AttestMintRequest)) *providerv1.OperationGrant {
	t.Helper()
	r := operation.AttestMintRequest{ProviderUID: e.pki.Record.ProviderUID, HostID: trusttest.HostName,
		EnrolmentID: e.pki.Record.EnrolmentID, Epoch: e.epoch, ChallengeNonce: c,
		Ledger:   operation.LedgerPosition{StoreID: l.StoreID[:], Seq: l.Seq, Head: l.Head[:]},
		IssuedAt: time.Now()}
	if edit != nil {
		edit(&r)
	}
	g, err := operation.MintAttestGrant(context.Background(), e.grants, r)
	require.NoError(t, err)
	return g
}

func (e *authEnv) attest(t *testing.T, g *providerv1.OperationGrant) *providerv1.AuthorityAnswer {
	t.Helper()
	resp, err := e.ha.Attest(ctx(t), &providerv1.AttestRequest{TargetHostId: trusttest.HostName, AnswerNonce: nonce(0x32), Grant: g})
	require.NoError(t, err)
	a := &providerv1.AuthorityAnswer{}
	verifyBlob(t, e.opsEnv, resp.GetHostAnswer(), answer.BodyTypeAuthorityAnswer, a)
	return a
}

func wantOutcome(t *testing.T, a *providerv1.AuthorityAnswer, o providerv1.AuthorityOutcome, reason string) {
	t.Helper()
	if a.GetOutcome() != o || a.GetOutcomeReason() != reason {
		t.Fatalf("outcome %v (%q), want %v (%q)", a.GetOutcome(), a.GetOutcomeReason(), o, reason)
	}
}

// TestAuthority_AttestThenOperate: an agent with an Authority refuses every
// mutation until attested; a challenge and an attestation grant at the
// store's position attest it; its answers then name the session to
// ledger-aware requests only.
func TestAuthority_AttestThenOperate(t *testing.T) {
	e := newAuthEnv(t)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	_, err := e.send(o)
	require.Error(t, err)
	assert.Equal(t, opstore.ReasonUnattested, reasonOf(err), "a mutation before the attestation: %v", err)
	assert.Contains(t, err.Error(), server.CauseAwaitingAttestation)

	pc := e.client()
	info, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(5), LedgerVersion: 1})
	require.NoError(t, err)
	ha := verifyAnswer(t, e.env, info.GetHostAnswer())
	l := ha.GetLedger()
	require.NotNil(t, l)
	assert.Equal(t, providerv1.LedgerSessionState_LEDGER_SESSION_STATE_AWAITING_ATTESTATION, l.GetState())
	assert.Equal(t, e.pki.Record.EnrolmentID, l.GetStoreOriginEnrolmentId(), "a store enrolled fresh records its origin")
	assert.Equal(t, uint32(1), l.GetStoreEnrolmentCount())
	assert.Equal(t, uint32(1), ha.GetContext().GetLedgerVersionEcho())

	ch := e.challenge(t, pAttest)
	require.Len(t, ch.GetChallengeNonce(), attest.NonceLen)
	assert.Equal(t, uint32(300), ch.GetValidForSeconds())
	assert.Equal(t, providerv1.LedgerSessionState_LEDGER_SESSION_STATE_AWAITING_ATTESTATION, ch.GetLedger().GetState())

	pos := e.store.Position()
	g := e.attestGrant(t, ch.GetChallengeNonce(), pos, nil)
	a := e.attest(t, g)
	wantOutcome(t, a, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, "")
	assert.Equal(t, ch.GetChallengeNonce(), a.GetContext().GetAttestationId(), "the answer names the new session")
	assert.Equal(t, pos.Seq, a.GetMark().GetSeq())
	assert.Equal(t, int32(providerv1.GrantKind_GRANT_KIND_ATTEST), a.GetGrantKind())
	assert.Len(t, a.GetGrantDigest(), 32)

	task, err := e.send(o)
	require.NoError(t, err, "a mutation after the attestation")
	pos = e.markTip(t)
	e.waitDone(task)

	info, err = pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(6), LedgerVersion: 1})
	require.NoError(t, err)
	ha = verifyAnswer(t, e.env, info.GetHostAnswer())
	assert.Equal(t, providerv1.LedgerSessionState_LEDGER_SESSION_STATE_ATTESTED, ha.GetLedger().GetState())
	assert.Equal(t, ch.GetChallengeNonce(), ha.GetLedger().GetAttestationId())
	assert.Equal(t, ch.GetChallengeNonce(), ha.GetContext().GetAttestationId())
	assert.Equal(t, pos.Seq, ha.GetLedger().GetMark().GetSeq())

	// Not asked: no ledger field.
	info, err = pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(7)})
	require.NoError(t, err)
	ha = verifyAnswer(t, e.env, info.GetHostAnswer())
	assert.Nil(t, ha.GetLedger())
	assert.Empty(t, ha.GetContext().GetAttestationId())
	assert.Zero(t, ha.GetContext().GetLedgerVersionEcho())

	// TaskStatus and Describe echo the version and name the session when
	// asked.
	ts, err := pc.TaskStatus(ctx(t), &providerv1.TaskStatusRequest{AnswerNonce: nonce(8), OpId: o.id.OpID,
		OpEnrolmentId: o.id.EnrolmentID, TargetHostId: trusttest.HostName, LedgerVersion: 3})
	require.NoError(t, err)
	oa := &providerv1.OperationAnswer{}
	verifyBlob(t, e.opsEnv, ts.GetHostAnswer(), answer.BodyTypeOperationAnswer, oa)
	assert.Equal(t, uint32(3), oa.GetContext().GetLedgerVersionEcho())
	assert.Equal(t, ch.GetChallengeNonce(), oa.GetContext().GetAttestationId())
	assert.NotZero(t, oa.GetAcceptedSeq(), "the acceptance position, from the record")
	assert.Empty(t, oa.GetAcceptedHead(), "no head once the operation left ACCEPTED")
	d, err := pc.Describe(ctx(t), &providerv1.DescribeRequest{Owner: ownerOf("vm-1"), TargetHostId: trusttest.HostName,
		AnswerNonce: nonce(9), BindingId: o.id.BindingID, LedgerVersion: 1})
	require.NoError(t, err)
	va := &providerv1.VMAnswer{}
	verifyBlob(t, e.opsEnv, d.GetHostAnswer(), answer.BodyTypeVMAnswer, va)
	assert.Equal(t, ch.GetChallengeNonce(), va.GetContext().GetAttestationId())
	d, err = pc.Describe(ctx(t), &providerv1.DescribeRequest{Owner: ownerOf("vm-1"), TargetHostId: trusttest.HostName,
		AnswerNonce: nonce(9), BindingId: o.id.BindingID})
	require.NoError(t, err)
	va = &providerv1.VMAnswer{}
	verifyBlob(t, e.opsEnv, d.GetHostAnswer(), answer.BodyTypeVMAnswer, va)
	assert.Empty(t, va.GetContext().GetAttestationId())

	created := &providerv1.CreateResponse{}
	o2 := e.newOp(kCreate, "vm-2", randID(), 1, createReq("vm-2", nil))
	req := &providerv1.CreateRequest{OperationEnvelope: &providerv1.OperationEnvelope{Request: o2.request, Grant: e.grant(o2, time.Now(), 0)}}
	require.NoError(t, e.cc().Invoke(ctx(t), providerv1.Provider_Create_FullMethodName, req, created))
	assert.NotNil(t, created.GetOperationReceipt(), "an accept response carries a receipt")
}

// TestAuthority_ChallengeRules: ATTEST only while awaiting, REATTEST only
// while attested, neither while quarantined; a challenge is single-use.
func TestAuthority_ChallengeRules(t *testing.T) {
	e := newAuthEnv(t)
	re := e.challenge(t, pReattest)
	assert.Equal(t, answer.NotIssuedNotAttested, re.GetNotIssuedReason())
	assert.Empty(t, re.GetChallengeNonce())
	assert.Zero(t, re.GetValidForSeconds())

	c1 := e.challenge(t, pAttest).GetChallengeNonce()
	c2 := e.challenge(t, pAttest).GetChallengeNonce()
	require.NotEqual(t, c1, c2)
	pos := e.store.Position()
	g1 := e.attestGrant(t, c1, pos, nil)
	wantOutcome(t, e.attest(t, g1), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, "")

	// The same grant again: its challenge was consumed.
	wantOutcome(t, e.attest(t, g1), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED, answer.ReasonChallengeUnknown)
	// Another ATTEST challenge, issued while awaiting, cannot be used once
	// attested: a first attestation needs an agent that awaits one.
	wantOutcome(t, e.attest(t, e.attestGrant(t, c2, pos, nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED,
		answer.ReasonAlreadyAttested)
	if a, _ := e.store.Attestation(); !bytes.Equal(a.Session, c1) {
		t.Fatal("a refused attestation changed the session")
	}

	at := e.challenge(t, pAttest)
	assert.Equal(t, answer.NotIssuedAlreadyAttested, at.GetNotIssuedReason())
	assert.Equal(t, c1, at.GetContext().GetAttestationId())

	e.agent.set(providerv1.AgentState_AGENT_STATE_QUARANTINED, string(journal.LostRolledBack))
	q := e.challenge(t, pReattest)
	assert.Equal(t, answer.NotIssuedQuarantined, q.GetNotIssuedReason())
}

// TestAuthority_Reattest: a re-attestation replaces the session and keeps
// the higher mark.
func TestAuthority_Reattest(t *testing.T) {
	e := newAuthEnv(t)
	older := e.store.Position()
	task, err := func() (string, error) {
		c := e.challenge(t, pAttest).GetChallengeNonce()
		wantOutcome(t, e.attest(t, e.attestGrant(t, c, older, nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, "")
		return e.send(e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil)))
	}()
	require.NoError(t, err)
	e.markTip(t)
	e.waitDone(task)
	newer := e.store.Position()
	require.Greater(t, newer.Seq, older.Seq)

	c2 := e.challenge(t, pReattest).GetChallengeNonce()
	require.Len(t, c2, 16)
	a := e.attest(t, e.attestGrant(t, c2, newer, nil))
	wantOutcome(t, a, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, answer.ReasonSessionReplaced)
	assert.Equal(t, c2, a.GetContext().GetAttestationId())
	assert.Equal(t, newer.Seq, a.GetMark().GetSeq())

	c3 := e.challenge(t, pReattest).GetChallengeNonce()
	a = e.attest(t, e.attestGrant(t, c3, older, nil))
	wantOutcome(t, a, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, answer.ReasonSessionReplaced)
	assert.Equal(t, c3, a.GetContext().GetAttestationId())
	assert.Equal(t, newer.Seq, a.GetMark().GetSeq(), "a lower re-attestation keeps the higher mark")
}

// TestAuthority_Refusals: a grant that does not verify leaves the challenge
// usable; an unknown or expired challenge is refused.
func TestAuthority_Refusals(t *testing.T) {
	e := newAuthEnv(t)
	pos := e.store.Position()
	c := e.challenge(t, pAttest).GetChallengeNonce()

	for name, edit := range map[string]func(*operation.AttestMintRequest){
		"another enrolment": func(r *operation.AttestMintRequest) { r.EnrolmentID = randID() },
		"another host":      func(r *operation.AttestMintRequest) { r.HostID = "host-b" },
		"expired": func(r *operation.AttestMintRequest) {
			r.IssuedAt = time.Now().Add(-time.Hour)
			r.Lifetime = time.Minute
		},
	} {
		a := e.attest(t, e.attestGrant(t, c, pos, edit))
		wantOutcome(t, a, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED, answer.ReasonGrantInvalid)
		assert.Empty(t, a.GetContext().GetAttestationId(), name)
	}
	// An operation grant on Attest is refused on its kind.
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	a := e.attest(t, e.grant(o, time.Now(), 0))
	wantOutcome(t, a, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED, answer.ReasonGrantInvalid)
	// No grant at all.
	resp, err := e.ha.Attest(ctx(t), &providerv1.AttestRequest{TargetHostId: trusttest.HostName, AnswerNonce: nonce(0x32)})
	require.NoError(t, err)
	none := &providerv1.AuthorityAnswer{}
	verifyBlob(t, e.opsEnv, resp.GetHostAnswer(), answer.BodyTypeAuthorityAnswer, none)
	wantOutcome(t, none, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED, answer.ReasonGrantInvalid)
	assert.Empty(t, none.GetGrantDigest())

	// A nonce the agent never issued.
	wantOutcome(t, e.attest(t, e.attestGrant(t, randID(), pos, nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED,
		answer.ReasonChallengeUnknown)

	// The challenge survived every refusal above.
	wantOutcome(t, e.attest(t, e.attestGrant(t, c, pos, nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, "")
}

func TestAuthority_ChallengeExpires(t *testing.T) {
	e := newAuthEnv(t)
	c := e.challenge(t, pAttest).GetChallengeNonce()
	e.clock.now.Add(int64(attest.Validity))
	wantOutcome(t, e.attest(t, e.attestGrant(t, c, e.store.Position(), nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED,
		answer.ReasonChallengeExpired)
	if _, ok := e.store.Attestation(); ok {
		t.Fatal("an expired challenge attested the agent")
	}
}

// TestAuthority_QuarantineOnLedgerAhead: an attestation beyond the store's
// chain quarantines it, and the answer says why.
func TestAuthority_QuarantineOnLedgerAhead(t *testing.T) {
	e := newAuthEnv(t)
	c := e.challenge(t, pAttest).GetChallengeNonce()
	pos := e.store.Position()
	pos.Seq += 5
	a := e.attest(t, e.attestGrant(t, c, pos, nil))
	wantOutcome(t, a, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_QUARANTINED, string(journal.LostLedgerAhead))
	st, r := e.store.Journal().State()
	require.Equal(t, journal.StateLost, st)
	require.Equal(t, journal.LostLedgerAhead, r)

	// The fake agent reports the store's state: the reason reaches only a
	// ledger-aware request.
	e.agent.set(providerv1.AgentState_AGENT_STATE_QUARANTINED, string(r))
	pc := e.client()
	plain, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(5)})
	require.NoError(t, err)
	assert.Equal(t, string(journal.LostRolledBack), verifyAnswer(t, e.env, plain.GetHostAnswer()).GetContext().GetStateReason())
	aware, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(5), LedgerVersion: 1})
	require.NoError(t, err)
	assert.Equal(t, string(journal.LostLedgerAhead), verifyAnswer(t, e.env, aware.GetHostAnswer()).GetContext().GetStateReason())

	// A quarantined agent answers every later attestation QUARANTINED.
	wantOutcome(t, e.attest(t, e.attestGrant(t, c, e.store.Position(), nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_QUARANTINED, "")
}

// TestAuthority_RequestRules: the strict path refuses before anything else.
func TestAuthority_RequestRules(t *testing.T) {
	e := newAuthEnv(t)
	good := &providerv1.ChallengeRequest{TargetHostId: trusttest.HostName, AnswerNonce: nonce(1), Purpose: pAttest}
	for name, edit := range map[string]func(*providerv1.ChallengeRequest){
		"another host": func(r *providerv1.ChallengeRequest) { r.TargetHostId = "host-b" },
		"no host":      func(r *providerv1.ChallengeRequest) { r.TargetHostId = "" },
		"short nonce":  func(r *providerv1.ChallengeRequest) { r.AnswerNonce = nonce(1)[:8] },
		"no nonce":     func(r *providerv1.ChallengeRequest) { r.AnswerNonce = nil },
		"no purpose":   func(r *providerv1.ChallengeRequest) { r.Purpose = 0 },
		"bad purpose":  func(r *providerv1.ChallengeRequest) { r.Purpose = 9 },
		"unknown field": func(r *providerv1.ChallengeRequest) {
			r.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
		},
	} {
		r := proto.CloneOf(good)
		edit(r)
		_, err := e.ha.Challenge(ctx(t), r)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "%s: %v", name, err)
	}
	_, err := e.ha.Attest(ctx(t), &providerv1.AttestRequest{TargetHostId: "host-b", AnswerNonce: nonce(2)})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = e.ha.LedgerMark(ctx(t), &providerv1.LedgerMarkRequest{TargetHostId: "host-b", AnswerNonce: nonce(3)})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = e.ha.LedgerMark(ctx(t), &providerv1.LedgerMarkRequest{TargetHostId: trusttest.HostName, AnswerNonce: nonce(3)[:8]})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = e.ha.RetireIfUnknown(ctx(t), &providerv1.RetireIfUnknownRequest{TargetHostId: trusttest.HostName, AnswerNonce: nonce(4)})
	assert.Equal(t, codes.Unimplemented, status.Code(err), "RetireIfUnknown is not implemented yet")
	// A caller that is not the enrolled Provider is refused before the
	// service.
	other := providerv1.NewHostAuthorityClient(e.dial(otherProvider(e.env), 0))
	_, err = other.Challenge(ctx(t), good)
	assert.Contains(t, []codes.Code{codes.PermissionDenied, codes.FailedPrecondition, codes.Unavailable}, status.Code(err), "%v", err)
}

// otherProvider is a client certificate of an allowed-looking Provider that
// is not the one the host is enrolled with.
func otherProvider(e *env) *tls.Certificate {
	c := e.pki.ProviderCert(e.t, nil, trusttest.ProviderURI("00000000-0000-4000-8000-000000000000"))
	return &c
}
