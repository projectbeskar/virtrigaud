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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// The reserved Accepted phase is driven as Sent; every other phase keeps its
// meaning.
func TestAttemptDelivered(t *testing.T) {
	for phase, want := range map[infravirtrigaudiov1beta1.OperationAttemptPhase]bool{
		infravirtrigaudiov1beta1.OperationAttemptPhasePrepared: false,
		infravirtrigaudiov1beta1.OperationAttemptPhaseSent:     true,
		infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted: true,
		infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal: false,
		"":        false,
		"Unknown": false,
	} {
		assert.Equal(t, want, attemptDelivered(phase), "phase %q", phase)
	}
}

// An Accepted attempt is in flight, like Sent: it serialises later attempts
// and is never swept.
func TestAcceptedAttemptIsInFlight(t *testing.T) {
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	vm.Status.OperationAttempts = []infravirtrigaudiov1beta1.OperationAttempt{{
		Kind: infravirtrigaudiov1beta1.OperationAttemptKindPowerOn, Attempt: 1, OpID: strings.Repeat("a", 32),
		Phase: infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted,
	}}
	a := inFlightAttempt(vm)
	require.NotNil(t, a)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted, a.Phase)
}

// An Accepted power attempt counts against the running deadline exactly like
// a Sent one; a Create or Delete never does.
func TestAcceptedAttemptRunningDeadline(t *testing.T) {
	r := &VirtualMachineReconciler{}
	for _, phase := range []infravirtrigaudiov1beta1.OperationAttemptPhase{
		infravirtrigaudiov1beta1.OperationAttemptPhaseSent, infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted,
	} {
		a := &infravirtrigaudiov1beta1.OperationAttempt{Kind: infravirtrigaudiov1beta1.OperationAttemptKindPowerOn,
			Phase: phase, LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * sentAttemptDeadline))}
		assert.True(t, r.runningPastDeadline(a), "%s past the deadline", phase)
		a.LastTransitionTime = metav1.NewTime(time.Now())
		assert.False(t, r.runningPastDeadline(a), "%s within the deadline", phase)
		a.LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * sentAttemptDeadline))
		a.Kind = infravirtrigaudiov1beta1.OperationAttemptKindDelete
		assert.False(t, r.runningPastDeadline(a), "%s Delete never", phase)
	}
	a := &infravirtrigaudiov1beta1.OperationAttempt{Kind: infravirtrigaudiov1beta1.OperationAttemptKindPowerOn,
		Phase: infravirtrigaudiov1beta1.OperationAttemptPhasePrepared, LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * sentAttemptDeadline))}
	assert.False(t, r.runningPastDeadline(a), "Prepared is unchanged")
}

// A power attempt whose marker reads Accepted (with a receipt and ledger
// generation) is driven as a Sent one: it is not demoted to Sent, its receipt
// and transition time are kept, it is reported in progress, held once past
// the deadline, and superseded by a Delete, which is then sent.
func TestIdentityPower_AcceptedDrivenAsSent(t *testing.T) {
	stub := newIdentityStub()
	r, req := createdIdentityVM(t, stub)
	stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{TaskRef: "task-" + string(p.Kind)}, nil
	}
	stub.onTask = func(string) (contracts.TaskStatus, error) {
		return contracts.TaskStatus{Outcome: contracts.TaskOutcomeRunning}, nil
	}
	stub.describeResp.PowerState = "Off"
	setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
	})
	reconcileN(t, r, req, 2)
	require.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseSent, findAttempt(idVM(t, r), contracts.OperationKindPowerOn).Phase)

	receipt := &infravirtrigaudiov1beta1.OperationAttemptReceipt{
		StoreID: strings.Repeat("1", 32), Seq: 7, Head: strings.Repeat("2", 64),
		AttestationID: strings.Repeat("3", 32), ReceiptTag: strings.Repeat("4", 64),
	}
	vm := idVM(t, r)
	var since metav1.Time
	for i := range vm.Status.OperationAttempts {
		if vm.Status.OperationAttempts[i].Kind == infravirtrigaudiov1beta1.OperationAttemptKindPowerOn {
			vm.Status.OperationAttempts[i].Phase = infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted
			vm.Status.OperationAttempts[i].Receipt = receipt.DeepCopy()
			vm.Status.OperationAttempts[i].LedgerGeneration = 3
			since = vm.Status.OperationAttempts[i].LastTransitionTime
		}
	}
	require.NoError(t, r.Status().Update(context.Background(), vm))

	reconcileN(t, r, req, 2)
	vm = idVM(t, r)
	a := findAttempt(vm, contracts.OperationKindPowerOn)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted, a.Phase, "not demoted to Sent")
	assert.Equal(t, receipt, a.Receipt, "the receipt is kept")
	assert.Equal(t, int64(3), a.LedgerGeneration)
	assert.True(t, since.Equal(&a.LastTransitionTime), "the transition time is kept")
	assert.Equal(t, k8s.ReasonOperationInProgress, readyReason(vm))
	assert.Equal(t, []contracts.OperationKind{contracts.OperationKindCreate, contracts.OperationKindPowerOn}, sentKinds(stub),
		"nothing is re-sent")

	r.clock = func() time.Time { return time.Now().Add(sentAttemptDeadline + time.Minute) }
	reconcileN(t, r, req, 1)
	assert.Equal(t, k8s.ReasonOperationOutcomeUnknown, readyReason(idVM(t, r)), "held past the deadline, as Sent")

	markDeleted(t, r, nil)
	reconcileN(t, r, req, 3)
	vm = idVM(t, r)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSuperseded, findAttempt(vm, contracts.OperationKindPowerOn).Outcome)
	assert.Equal(t, []contracts.OperationKind{contracts.OperationKindCreate, contracts.OperationKindPowerOn,
		contracts.OperationKindDelete}, sentKinds(stub), "the signed Delete is sent")
}

// markSent keeps an Accepted marker Accepted, with its receipt and transition
// time: a re-send never demotes it to Sent.
func TestMarkSent_KeepsAccepted(t *testing.T) {
	_, r, _, _ := asyncDeleteInFlight(t)
	vm := idVM(t, r)
	d := findAttempt(vm, contracts.OperationKindDelete)
	require.NotNil(t, d)
	accepted := *d
	accepted.Phase = infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted
	accepted.Receipt = &infravirtrigaudiov1beta1.OperationAttemptReceipt{
		StoreID: strings.Repeat("1", 32), Seq: 1, Head: strings.Repeat("2", 64),
		AttestationID: strings.Repeat("3", 32), ReceiptTag: strings.Repeat("4", 64),
	}
	vm.Status.OperationAttempts = withAttempt(vm, accepted)
	require.NoError(t, r.Status().Update(context.Background(), vm))

	vm = idVM(t, r)
	got, err := r.markSent(context.Background(), vm, findAttempt(vm, contracts.OperationKindDelete),
		&contracts.OperationAnswer{TaskRef: accepted.TaskRef})
	require.NoError(t, err)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted, got.Phase)
	assert.Equal(t, accepted.Receipt, got.Receipt)
	assert.True(t, accepted.LastTransitionTime.Equal(&got.LastTransitionTime))
	stored := findAttempt(idVM(t, r), contracts.OperationKindDelete)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted, stored.Phase)
}

// An Accepted Delete whose task reference is not bound to it is held while in
// flight, exactly like a Sent one.
func TestIdentityDelete_AcceptedUnboundTaskRefHolds(t *testing.T) {
	_, r, req, _ := asyncDeleteInFlight(t)
	vm := idVM(t, r)
	for i := range vm.Status.OperationAttempts {
		a := &vm.Status.OperationAttempts[i]
		if a.Kind == infravirtrigaudiov1beta1.OperationAttemptKindDelete {
			a.Phase = infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted
			a.TaskRef = "some-other-task"
		}
	}
	require.NoError(t, r.Status().Update(context.Background(), vm))
	reconcileN(t, r, req, 2)
	assert.False(t, vmGone(t, r), "the finalizer is kept")
	assert.Equal(t, k8s.ReasonOperationStateLost, readyReason(idVM(t, r)))
}
