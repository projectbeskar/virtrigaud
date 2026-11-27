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
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
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
