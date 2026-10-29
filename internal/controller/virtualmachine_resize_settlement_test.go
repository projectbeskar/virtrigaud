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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A durable resize reservation is released only on positive evidence that
// the last reconfigure was applied (Reconfiguring=False/ReconcileSuccess) and
// a Describe of the VM's size; every other condition, including one this
// check has never heard of, keeps it.

// syntheticUnknownReason is a Reconfiguring reason no version of the manager
// sets: one added later must keep a reservation until it is classified.
const syntheticUnknownReason = "SomeFutureReconfigureOutcome"

func TestResizeSettledCondition_AllowListOnly(t *testing.T) {
	cond := func(status metav1.ConditionStatus, reason string) *metav1.Condition {
		return &metav1.Condition{Type: k8s.ConditionReconfiguring, Status: status, Reason: reason}
	}
	cases := []struct {
		name string
		cond *metav1.Condition
		want bool
	}{
		{"applied", cond(metav1.ConditionFalse, k8s.ReasonReconcileSuccess), true},
		{"no condition: a Reconfigure sent before a restart was never answered", nil, false},
		{"a synthetic, unrecognised reason", cond(metav1.ConditionFalse, syntheticUnknownReason), false},
		{"a synthetic, unrecognised reason in progress", cond(metav1.ConditionTrue, syntheticUnknownReason), false},
		{"ReconcileSuccess while still True is not a settled outcome", cond(metav1.ConditionTrue, k8s.ReasonReconcileSuccess), false},
		{"failed or unanswered", cond(metav1.ConditionFalse, k8s.ReasonProviderError), false},
		{"pending a restart", cond(metav1.ConditionTrue, k8s.ReasonRestartRequired), false},
		{"pending a power-off", cond(metav1.ConditionFalse, k8s.ReasonResizePendingPowerOff), false},
		{"task in progress", cond(metav1.ConditionTrue, k8s.ReasonTaskInProgress), false},
		{"updating", cond(metav1.ConditionTrue, k8s.ReasonUpdating), false},
		{"no honest result", cond(metav1.ConditionFalse, k8s.ReasonProviderLacksHonestReconfigure), false},
		{"refused before sending: inherited over capacity", cond(metav1.ConditionFalse, k8s.ReasonInheritedAdmissionOverCapacity), false},
		{"refused before sending: placement error", cond(metav1.ConditionFalse, k8s.ReasonPlacementError), false},
		{"refused before sending: does not fit", cond(metav1.ConditionFalse, k8s.ReasonUnschedulable), false},
		{"refused before sending: shrink waits", cond(metav1.ConditionFalse, k8s.ReasonShrinkPendingPowerOff), false},
		{"invalid override", cond(metav1.ConditionFalse, k8s.ReasonValidationError), false},
		{"failed power task", cond(metav1.ConditionFalse, k8s.ReasonPowerTaskFailed), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resizeSettledCondition(tc.cond))
		})
	}
}

func TestResizeOutcomeEstablished_NeedsDescribeAndNoTask(t *testing.T) {
	vm := sized("app", 2)
	k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonReconcileSuccess, "VM reconfigured successfully", vm.Generation)
	described := contracts.DescribeResponse{VCPUs: 2, MaxMemoryMiB: 4096}
	assert.True(t, resizeOutcomeEstablished(vm, described))
	assert.False(t, resizeOutcomeEstablished(vm, contracts.DescribeResponse{MaxMemoryMiB: 4096}), "no vCPUs reported")
	assert.False(t, resizeOutcomeEstablished(vm, contracts.DescribeResponse{VCPUs: 2}), "no memory maximum reported")
	vm.Status.ReconfigureTaskRef = "task-1"
	assert.False(t, resizeOutcomeEstablished(vm, described), "a task is outstanding")
}

// reservedSteadyVM is a bound clustered VM whose spec matches its recorded
// size (no reconfigure due), holding a resize reservation, with its
// Reconfiguring condition set to reason.
func reservedSteadyVM(status metav1.ConditionStatus, reason string) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := sized("app", 2)
	vm.Status.Placement.MemoryCeilingMiB = i64p(0)
	vm.Status.Placement.ResizeReservation = &infravirtrigaudiov1beta1.PlacementResources{CPU: 4, MemoryMiB: 4096}
	if reason != "" {
		k8s.SetReconfiguringCondition(&vm.Status.Conditions, status, reason, "test", vm.Generation)
	}
	return vm
}

// TestResizeReservation_UnrecognisedReasonKeepsIt: a reconcile of a VM in its
// steady state does not release the reservation on a reason it does not
// recognise, on a missing condition, or on a refusal that may have replaced an
// earlier attempt's unresolved outcome; it does on ReconcileSuccess.
func TestResizeReservation_UnrecognisedReasonKeepsIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  metav1.ConditionStatus
		reason  string
		release bool
	}{
		{"synthetic unrecognised reason", metav1.ConditionFalse, syntheticUnknownReason, false},
		{"no Reconfiguring condition", "", "", false},
		{"a refusal replaced the outcome", metav1.ConditionFalse, k8s.ReasonInheritedAdmissionOverCapacity, false},
		{"applied and confirmed", metav1.ConditionFalse, k8s.ReasonReconcileSuccess, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := resizeFixture(t, reportingProvider(2, 4096), reservedSteadyVM(tc.status, tc.reason), capVM("newvm"))
			for range 2 {
				_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
				require.NoError(t, err)
			}
			if tc.release {
				assert.Nil(t, reservationOf(t, r, "app"), "released on positive evidence")
				return
			}
			assert.NotNil(t, reservationOf(t, r, "app"), "kept while the outcome is not positively established")
			assert.False(t, newVMFits(t, r), "the held reservation keeps counting")
		})
	}
}

// TestResizeReservation_UncertainOutcomeKeepsIt: a Reconfigure whose answer
// was lost (the provider did not answer in time) leaves the outcome
// uncertain; the reservation is kept through later reconciles, including one
// in which the provider describes the VM at its old size.
func TestResizeReservation_UncertainOutcomeKeepsIt(t *testing.T) {
	prov := reportingProvider(2, 4096)
	prov.reconfigureErr = contracts.NewRetryableError("reconfigure: context deadline exceeded", nil)
	r := resizeFixture(t, prov, wantsCPU(sized("app", 2), 4), capVM("newvm"))
	for range 3 {
		_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
		require.NoError(t, err)
	}
	got := getVM(t, r, "app")
	assert.False(t, resizeSettledCondition(reconfiguringCondition(got)))
	assert.NotNil(t, got.Status.Placement.ResizeReservation, "an unanswered Reconfigure keeps its reservation")
}
