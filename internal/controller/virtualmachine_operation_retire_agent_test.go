//go:build virtrigaud_dev

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
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered/agenttarget"
)

// readyMessage is the message of vm's Ready condition.
func readyMessage(vm *infravirtrigaudiov1beta1.VirtualMachine) string {
	if c := k8s.GetCondition(vm.Status.Conditions, k8s.ConditionReady); c != nil {
		return c.Message
	}
	return ""
}

// A Create lost before it reached the agent: the agent signs, in the
// ledger's adopted session, that it holds no record of it; the manager has
// it retired, the attempt closes Retired (never_received) without the
// operation ever running, and the next attempt creates the VM exactly once.
func TestAgentIdentity_RetireNeverDeliveredOp(t *testing.T) {
	e := newAgentEnv(t)
	e.lie.set(func(l *lyingProvider) { l.dropCreate = true })
	var first *infravirtrigaudiov1beta1.OperationAttempt
	for i := 0; i < 10 && first == nil; i++ {
		e.step(t, 1)
		if a := findAttempt(e.vm(t), contracts.OperationKindCreate); a != nil && a.Attempt == 1 &&
			a.Phase == infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal {
			first = a
		}
	}
	if first == nil {
		vm := e.vm(t)
		t.Fatalf("the lost Create did not close: %+v (ready %s: %s)", vm.Status.OperationAttempts, readyReason(vm), readyMessage(vm))
	}
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeRetired, first.Outcome)
	assert.Equal(t, retiredNeverReceived, first.OutcomeReason)
	// A new attempt follows after the retry backoff of a not-performed
	// attempt.
	now := time.Now()
	e.r.clock = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		now = now.Add(blockedRetryMax)
		e.step(t, 1)
		if vm := e.vm(t); vm != nil && vm.Status.ID != "" {
			break
		}
	}
	e.r.clock = nil
	vm := e.waitCreated(t)
	c := findAttempt(vm, contracts.OperationKindCreate)
	assert.Equal(t, int64(2), c.Attempt, "a new attempt followed the retirement")
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded, c.Outcome)
	assert.Equal(t, 1, e.createOps(t), "exactly one Create executed")
	effects, err := e.host.Effects()
	require.NoError(t, err)
	for _, ef := range effects {
		assert.NotEqual(t, first.OpID, ef.OpID, "the retired operation never ran")
	}
}

// A Create lost before it reached the agent, whose host then re-enrolled in
// place (its store keeps the earlier, now frozen, enrolment's records): the
// agent signs, in the ledger's adopted session, that it holds no record of
// the operation under the frozen enrolment, and the attempt closes Retired
// (unknown_to_frozen_enrolment) without any grant; the operation never
// ran.
func TestAgentIdentity_FrozenPredecessorClosure(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	e.lie.set(func(l *lyingProvider) { l.dropCreate = true })
	_, err := e.r.Reconcile(ctx, e.req)
	require.NoError(t, err)
	a := findAttempt(e.vm(t), contracts.OperationKindCreate)
	require.NotNil(t, a)
	require.NotEqual(t, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, a.Phase, "the lost Create is in flight")
	newEnrolment, err := e.tg.ReenrolHost(agentHost, agenttarget.StartEpoch+1)
	require.NoError(t, err)
	e.trust.mu.Lock()
	tr := e.trust.trusts[agentHost]
	tr.Predecessors = append(tr.Predecessors, tr.Enrolment.EnrolmentID)
	tr.Enrolment.EnrolmentID = newEnrolment
	e.trust.trusts[agentHost] = tr
	e.trust.mu.Unlock()
	var closed *infravirtrigaudiov1beta1.OperationAttempt
	ok := assert.Eventually(t, func() bool {
		if _, err := e.r.Reconcile(ctx, e.req); err != nil {
			return false
		}
		c := findAttempt(e.vm(t), contracts.OperationKindCreate)
		if c != nil && c.OpID == a.OpID && c.Phase == infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal {
			closed = c
			return true
		}
		return false
	}, 30*time.Second, 300*time.Millisecond, "the frozen enrolment's attempt closes")
	if !ok {
		vm := e.vm(t)
		t.Fatalf("not closed: %s: %s", readyReason(vm), readyMessage(vm))
	}
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeRetired, closed.Outcome)
	assert.Equal(t, retiredUnknownToFrozenEnrolment, closed.OutcomeReason)
	effects, err := e.host.Effects()
	require.NoError(t, err)
	for _, ef := range effects {
		assert.NotEqual(t, a.OpID, ef.OpID, "the operation never ran")
	}
}

// A retirement of an operation the agent received is refused by the agent
// (NOT_APPLICABLE): the attempt stays in flight and ends on the agent's own
// answers, with exactly one Create executed.
func TestAgentIdentity_RetireRefusedWhenReceived(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	_, err := e.r.Reconcile(ctx, e.req)
	require.NoError(t, err)
	vm := e.vm(t)
	a := findAttempt(vm, contracts.OperationKindCreate)
	require.NotNil(t, a)
	p, found, err := e.r.loadIntent(ctx, vm, a.OpID, e.provider.Namespace)
	require.NoError(t, err)
	require.True(t, found)
	rec, err := e.ledger.LoadBound(ctx, e.provider, agentHost, [16]byte(p.LedgerStoreID))
	require.NoError(t, err)
	read, err := e.r.HostAnswers.sendRetire(ctx, e.cli, rec, p, rec.AttestationID)
	require.NoError(t, err)
	require.NotNil(t, read.Answer, "the agent signed its answer")
	ia := identityAttempt{providerCR: e.provider, reader: e.cli}
	d := e.r.retireAnswer(ctx, vm, ia, a, p, read, rec.AttestationID)
	require.NoError(t, d.err)
	assert.Equal(t, stepWaiting, d.step, "a received operation is not retired")
	cur := findAttempt(e.vm(t), contracts.OperationKindCreate)
	assert.NotEqual(t, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, cur.Phase)
	vm = e.waitCreated(t)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded, findAttempt(vm, contracts.OperationKindCreate).Outcome)
	assert.Equal(t, int64(1), findAttempt(vm, contracts.OperationKindCreate).Attempt)
	assert.Equal(t, 1, e.createOps(t))
}

// A retirement that finds a trace of an operation the agent holds no record
// of quarantines the agent's store: the host is held, and an attempt in
// flight reaches the quarantine hold with the agent's own reason (a
// verified quarantined answer is not "no answer").
func TestAgentIdentity_RetireTraceQuarantinesAndHolds(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	_, err := e.r.Reconcile(ctx, e.req)
	require.NoError(t, err)
	st := e.r.HostAnswers.ledgerOf(e.provider, agentHost)
	rec, err := e.ledger.LoadBound(ctx, e.provider, agentHost, st.store)
	require.NoError(t, err)
	// An operation the store has no record of, whose effect the executor
	// holds (its record was lost).
	opID, err := operation.NewRandomID()
	require.NoError(t, err)
	binding, err := operation.NewRandomID()
	require.NoError(t, err)
	tr := e.trust.trusts[agentHost]
	p := contracts.PreparedOperation{Kind: contracts.OperationKindPowerOn, RequestBytes: []byte("x"), DigestSalt: make([]byte, operation.SaltLen),
		LedgerStoreID: st.store[:], LedgerGeneration: st.generation,
		Identity: contracts.OperationAttemptIdentity{ProviderUID: string(e.provider.UID), HostID: agentHost, EnrolmentID: e.enrol,
			Epoch: contracts.OperationEpoch{Counter: tr.Enrolment.Epoch.Counter, Holder: tr.Enrolment.Epoch.Holder}, VMUID: "vm-lost",
			BindingID: binding, OpID: opID, Attempt: 1,
			MintEpoch: contracts.OperationEpoch{Counter: tr.Enrolment.Epoch.Counter, Holder: tr.Enrolment.Epoch.Holder}}}
	require.NoError(t, os.WriteFile(filepath.Join(e.host.FakeDir(), "effects", hex.EncodeToString(opID)+".power"), nil, 0o600))
	read, err := e.r.HostAnswers.sendRetire(ctx, e.cli, rec, p, rec.AttestationID)
	require.NoError(t, err)
	require.NotNil(t, read.Answer)
	vm := e.vm(t)
	a := findAttempt(vm, contracts.OperationKindCreate)
	ia := identityAttempt{providerCR: e.provider, reader: e.cli}
	d := e.r.retireAnswer(ctx, vm, ia, a, p, read, rec.AttestationID)
	assert.Equal(t, stepHeld, d.step)
	assert.Equal(t, LedgerReasonAgentQuarantined, e.ledger.HeldReason(e.provider, agentHost), "the host is held")

	// The attempt in flight: the agent's verified answer says its store is
	// quarantined, and the VM is held with that reason.
	_, err = e.r.Reconcile(ctx, e.req)
	require.NoError(t, err)
	vm = e.vm(t)
	assert.Equal(t, k8s.ReasonOperationOutcomeUnknown, readyReason(vm))
	assert.Contains(t, readyMessage(vm), "unknown_op_trace")
	assert.LessOrEqual(t, e.createOps(t), 1)
}

// An attempt prepared under a store whose ledger a new store archived is
// held as outcome unknown (store_lost): it is neither re-sent nor retired.
func TestAgentIdentity_ArchivedStoreAttemptsOutcomeUnknown(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	_, err := e.r.Reconcile(ctx, e.req)
	require.NoError(t, err)
	a := findAttempt(e.vm(t), contracts.OperationKindCreate)
	require.NotNil(t, a)
	store := e.r.HostAnswers.ledgerOf(e.provider, agentHost).store
	lh, err := e.ledger.resolve(ctx, e.provider, agentHost)
	require.NoError(t, err)
	_, err = e.ledger.archive(ctx, lh, store, [16]byte{0xff})
	require.NoError(t, err)
	e.step(t, 1)
	vm := e.vm(t)
	assert.Equal(t, k8s.ReasonOperationOutcomeUnknown, readyReason(vm))
	assert.Contains(t, readyMessage(vm), outcomeStoreLost)
	cur := findAttempt(vm, contracts.OperationKindCreate)
	assert.Equal(t, a.OpID, cur.OpID, "no new attempt")
	assert.NotEqual(t, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, cur.Phase)
}

// A host the ledger drives: its signed answer that a placement is deleted
// counts only from the ledger's adopted session, as every answer does; once
// the restarted agent is attested again, it counts.
func TestAgentIdentity_VMAnswerOnlyInAdoptedSession(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	_, err := e.r.Reconcile(ctx, e.req)
	require.NoError(t, err)
	c := findAttempt(e.vm(t), contracts.OperationKindCreate)
	require.NotNil(t, c)
	p, found, err := e.r.loadIntent(ctx, e.vm(t), c.OpID, e.provider.Namespace)
	require.NoError(t, err)
	require.True(t, found, "the Create's intent")
	ref := refOf(p)
	vm := e.waitCreated(t)
	e.markDeleted(t)
	for i := 0; i < 20 && e.vm(t) != nil; i++ {
		e.step(t, 1)
	}
	require.Nil(t, e.vm(t), "the VM was deleted on the agent's signed answers")
	ia := identityAttempt{providerCR: e.provider, reader: e.cli}
	deleted, _, _, _ := e.r.authenticatedHostDeleted(ctx, vm, ia, ref)
	require.True(t, deleted, "the placement is deleted, signed in the adopted session")
	require.NoError(t, e.host.Stop())
	_, _ = e.host.Wait(10 * time.Second)
	require.NoError(t, e.host.Restart())
	deleted, _, _, _ = e.r.authenticatedHostDeleted(ctx, vm, ia, ref)
	assert.False(t, deleted, "an answer outside the adopted session decides nothing")
	require.Eventually(t, func() bool {
		e.r.HostAnswers.DriveHostLedger(ctx, e.provider, e.cli, agentHost)
		deleted, _, _, _ := e.r.authenticatedHostDeleted(ctx, vm, ia, ref)
		return deleted
	}, 20*time.Second, 300*time.Millisecond, "it counts once the agent is attested again")
}

// lostCreate makes the VM's first Create get lost before it reaches the
// agent and returns the attempt in flight, its authenticated intent and the
// store's ledger.
func (e *agentEnv) lostCreate(t *testing.T) (*infravirtrigaudiov1beta1.VirtualMachine, *infravirtrigaudiov1beta1.OperationAttempt,
	contracts.PreparedOperation, *HostLedgerRecord) {
	t.Helper()
	ctx := context.Background()
	e.lie.set(func(l *lyingProvider) { l.dropCreate = true })
	_, err := e.r.Reconcile(ctx, e.req)
	require.NoError(t, err)
	vm := e.vm(t)
	a := findAttempt(vm, contracts.OperationKindCreate)
	require.NotNil(t, a)
	require.NotEqual(t, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, a.Phase)
	p, found, err := e.r.loadIntent(ctx, vm, a.OpID, e.provider.Namespace)
	require.NoError(t, err)
	require.True(t, found)
	rec, err := e.ledger.LoadBound(ctx, e.provider, agentHost, [16]byte(p.LedgerStoreID))
	require.NoError(t, err)
	return vm, a, p, rec
}

// retireDirect has the lost Create retired by the agent and returns the
// read and the answer's verified chain position.
func (e *agentEnv) retireDirect(t *testing.T, p contracts.PreparedOperation, rec *HostLedgerRecord) (contracts.AuthorityRead,
	hostidentity.ChainPosition) {
	t.Helper()
	read, err := e.r.HostAnswers.sendRetire(context.Background(), e.cli, rec, p, rec.AttestationID)
	require.NoError(t, err)
	require.NotNil(t, read.Answer)
	e.trust.mu.Lock()
	tr := e.trust.trusts[agentHost]
	e.trust.mu.Unlock()
	exp := tr.expectation(e.provider, agentHost, hostidentity.BodyTypeAuthorityAnswer, read.Nonce, time.Now())
	exp.GrantKind, exp.GrantDigest = providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN, read.GrantDigest
	va, err := hostidentity.VerifyAnswer(blobOf(read.Answer), exp)
	require.NoError(t, err)
	require.Equal(t, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, va.Authority.GetOutcome())
	return read, va.Position
}

// An applied retirement closes the attempt only once the host ledger
// covers the answer's position (at or above the tombstone's commit).
func TestAgentIdentity_RetireCoveredByLedger(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	vm, a, p, rec := e.lostCreate(t)
	before := rec.Position.Seq
	read, pos := e.retireDirect(t, p, rec)
	require.Greater(t, pos.Seq, before, "the tombstone was committed above the ledger")
	d := e.r.retireAnswer(ctx, vm, identityAttempt{providerCR: e.provider, reader: e.cli}, a, p, read, rec.AttestationID)
	require.NoError(t, d.err)
	closed := findAttempt(e.vm(t), contracts.OperationKindCreate)
	require.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeRetired, closed.Outcome)
	after, err := e.ledger.LoadBound(ctx, e.provider, agentHost, rec.StoreID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, after.Position.Seq, pos.Seq, "the ledger covers the retirement answer")
}

// A ledger that cannot be raised over the retirement answer never lets the
// attempt close.
func TestAgentIdentity_RetireNotCoveredNotClosed(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	vm, a, p, rec := e.lostCreate(t)
	read, _ := e.retireDirect(t, p, rec)
	lh, err := e.ledger.resolve(ctx, e.provider, agentHost)
	require.NoError(t, err)
	_, err = e.ledger.archive(ctx, lh, rec.StoreID, [16]byte{0xff})
	require.NoError(t, err)
	d := e.r.retireAnswer(ctx, vm, identityAttempt{providerCR: e.provider, reader: e.cli}, a, p, read, rec.AttestationID)
	require.NoError(t, d.err)
	cur := findAttempt(e.vm(t), contracts.OperationKindCreate)
	assert.NotEqual(t, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, cur.Phase, "not closed without the ledger's cover")
	assert.Equal(t, k8s.ReasonOperationAnswerUnverified, readyReason(e.vm(t)))
}

// A retirement answer behind the store position the manager holds never
// closes the attempt.
func TestAgentIdentity_RetireAnswerBehindNotClosed(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	vm, a, p, rec := e.lostCreate(t)
	read, pos := e.retireDirect(t, p, rec)
	h := e.r.HostAnswers
	h.mu.Lock()
	s := h.hosts[hostAnswerKey(e.provider, agentHost)]
	require.NotNil(t, s)
	s.store.Current.Seq = pos.Seq + 10
	h.mu.Unlock()
	d := e.r.retireAnswer(ctx, vm, identityAttempt{providerCR: e.provider, reader: e.cli}, a, p, read, rec.AttestationID)
	require.NoError(t, d.err)
	cur := findAttempt(e.vm(t), contracts.OperationKindCreate)
	assert.NotEqual(t, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, cur.Phase, "an answer behind the known position decides nothing")
}

// A manager that dies between the agent's retirement answer and the ledger
// raise: the next reconcile reads the tombstone through a signed RETIRED
// answer, whose position is persisted before the attempt closes Retired
// (never_received), so a rollback below it fails the store's continuity.
func TestAgentIdentity_RetireCrashBeforeCover(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	vm, a, p, rec := e.lostCreate(t)
	read, pos := e.retireDirect(t, p, rec)
	e.r.operationCrashHook = func(_ context.Context, point operationCrashPoint) bool { return point == crashBeforeRetireCover }
	d := e.r.retireAnswer(ctx, vm, identityAttempt{providerCR: e.provider, reader: e.cli}, a, p, read, rec.AttestationID)
	require.ErrorIs(t, d.err, errOperationCrashed)
	e.r.operationCrashHook = nil
	for i := 0; i < 5; i++ {
		_, err := e.r.Reconcile(ctx, e.req)
		require.NoError(t, err)
		if c := findAttempt(e.vm(t), contracts.OperationKindCreate); c.OpID == a.OpID && c.Phase == infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal {
			break
		}
	}
	closed := findAttempt(e.vm(t), contracts.OperationKindCreate)
	require.Equal(t, a.OpID, closed.OpID)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeRetired, closed.Outcome)
	assert.Equal(t, retiredNeverReceived, closed.OutcomeReason)
	h := e.r.HostAnswers
	h.mu.Lock()
	persisted := h.hosts[hostAnswerKey(e.provider, agentHost)].persistedSeq
	h.mu.Unlock()
	assert.GreaterOrEqual(t, persisted, pos.Seq, "the RETIRED answer's position is persisted")
}
