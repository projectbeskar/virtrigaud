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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// phaseVM builds a VirtualMachine with the given status, for derivePhase.
func phaseVM(mutate func(vm *infravirtrigaudiov1beta1.VirtualMachine)) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := &infravirtrigaudiov1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns", Generation: 3}}
	if mutate != nil {
		mutate(vm)
	}
	return vm
}

func setPhaseCond(vm *infravirtrigaudiov1beta1.VirtualMachine, condType string, status metav1.ConditionStatus, reason string) {
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type: condType, Status: status, Reason: reason, Message: "m", ObservedGeneration: vm.Generation,
	})
}

func TestDerivePhase(t *testing.T) {
	const (
		fa   = infravirtrigaudiov1beta1.VirtualMachinePhaseFailed
		pend = infravirtrigaudiov1beta1.VirtualMachinePhasePending
		prov = infravirtrigaudiov1beta1.VirtualMachinePhaseProvisioning
		run  = infravirtrigaudiov1beta1.VirtualMachinePhaseRunning
		stop = infravirtrigaudiov1beta1.VirtualMachinePhaseStopped
		reco = infravirtrigaudiov1beta1.VirtualMachinePhaseReconfiguring
		del  = infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting
	)
	withID := func(vm *infravirtrigaudiov1beta1.VirtualMachine) { vm.Status.ID = "vm-1" }
	power := func(p infravirtrigaudiov1beta1.ObservedPowerState) func(*infravirtrigaudiov1beta1.VirtualMachine) {
		return func(vm *infravirtrigaudiov1beta1.VirtualMachine) { withID(vm); vm.Status.PowerState = p }
	}
	ready := func(s metav1.ConditionStatus, reason string) func(*infravirtrigaudiov1beta1.VirtualMachine) {
		return func(vm *infravirtrigaudiov1beta1.VirtualMachine) { setPhaseCond(vm, k8s.ConditionReady, s, reason) }
	}
	both := func(fs ...func(*infravirtrigaudiov1beta1.VirtualMachine)) func(*infravirtrigaudiov1beta1.VirtualMachine) {
		return func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			for _, f := range fs {
				f(vm)
			}
		}
	}
	now := metav1.NewTime(time.Now())

	tests := []struct {
		name   string
		mutate func(vm *infravirtrigaudiov1beta1.VirtualMachine)
		want   infravirtrigaudiov1beta1.VirtualMachinePhase
	}{
		// Not created yet.
		{"fresh VM with no status", nil, pend},
		{"waiting for the provider to exist", ready(metav1.ConditionFalse, k8s.ReasonWaitingForDependencies), pend},
		{"provider client not resolvable", ready(metav1.ConditionFalse, k8s.ReasonProviderError), pend},
		{"clustered VM the scheduler cannot place", both(ready(metav1.ConditionFalse, k8s.ReasonUnschedulable),
			func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
				setPhaseCond(vm, k8s.ConditionProvisioning, metav1.ConditionFalse, k8s.ReasonUnschedulable)
			}), pend},
		{"adopted VM waiting for its id", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			setPhaseCond(vm, k8s.ConditionProvisioning, metav1.ConditionTrue, k8s.ReasonWaitingForDependencies)
		}, pend},
		{"held for a deleting provider", ready(metav1.ConditionFalse, k8s.ReasonProviderDeleting), pend},

		// Create in flight or outcome unknown.
		{"create requested (marker written)", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			setPhaseCond(vm, k8s.ConditionProvisioning, metav1.ConditionTrue, k8s.ReasonCreateRequested)
		}, prov},
		{"create requested and the provider is failing", both(
			func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
				setPhaseCond(vm, k8s.ConditionProvisioning, metav1.ConditionTrue, k8s.ReasonCreateRequested)
			}, ready(metav1.ConditionFalse, k8s.ReasonProviderError)), prov},
		{"creating", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			setPhaseCond(vm, k8s.ConditionProvisioning, metav1.ConditionTrue, k8s.ReasonCreating)
		}, prov},
		{"create task in flight", func(vm *infravirtrigaudiov1beta1.VirtualMachine) { vm.Status.LastTaskRef = "task-1" }, prov},
		{"clustered create pending on a host", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-a"}
		}, prov},
		{"created, not described yet", both(withID, ready(metav1.ConditionFalse, k8s.ReasonCreating)), prov},

		// Created: by power state.
		{"created and on", power(infravirtrigaudiov1beta1.ObservedPowerStateOn), run},
		{"on and ready", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn), ready(metav1.ConditionTrue, k8s.ReasonReconcileSuccess)), run},
		{"off", power(infravirtrigaudiov1beta1.ObservedPowerStateOff), stop},
		{"legacy OffGraceful", power("OffGraceful"), stop},
		{"suspended is still active", both(power(infravirtrigaudiov1beta1.ObservedPowerStateSuspended),
			ready(metav1.ConditionFalse, k8s.ReasonPowerStateUnmanaged)), run},
		{"unknown power state", both(power(infravirtrigaudiov1beta1.ObservedPowerStateUnknown),
			ready(metav1.ConditionFalse, k8s.ReasonPowerStateUnmanaged)), fa},
		{"ready with no power state recorded", both(withID, ready(metav1.ConditionTrue, k8s.ReasonReconcileSuccess)), run},
		{"transient provider error keeps the last power state", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn),
			ready(metav1.ConditionFalse, k8s.ReasonProviderError)), run},
		{"transient provider error on a stopped VM", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOff),
			ready(metav1.ConditionFalse, k8s.ReasonProviderError)), stop},

		// Reconfiguring.
		{"reconfigure task in flight", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn),
			func(vm *infravirtrigaudiov1beta1.VirtualMachine) { vm.Status.ReconfigureTaskRef = "task-2" }), reco},
		{"reconfiguring condition true", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn),
			func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
				setPhaseCond(vm, k8s.ConditionReconfiguring, metav1.ConditionTrue, k8s.ReasonUpdating)
			}), reco},
		{"change pending a restart is not in flight", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn),
			func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
				setPhaseCond(vm, k8s.ConditionReconfiguring, metav1.ConditionTrue, k8s.ReasonRestartRequired)
			}), run},
		{"reconfigure finished", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn),
			func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
				setPhaseCond(vm, k8s.ConditionReconfiguring, metav1.ConditionFalse, k8s.ReasonReconcileSuccess)
			}), run},

		// Holds.
		{"VMNotOwned", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn), ready(metav1.ConditionFalse, k8s.ReasonVMNotOwned)), fa},
		{"OwnerStampUnreadable", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn),
			ready(metav1.ConditionFalse, k8s.ReasonOwnerStampUnreadable)), fa},
		{"ProviderUnavailable", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn),
			ready(metav1.ConditionFalse, k8s.ReasonProviderUnavailable)), fa},
		{"VMIsImageArtifact", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOff),
			ready(metav1.ConditionFalse, k8s.ReasonVMIsImageArtifact)), fa},
		{"VMMissingOnHost", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn),
			ready(metav1.ConditionFalse, k8s.ReasonVMMissingOnHost)), fa},
		{"ProviderRefMismatch", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn),
			ready(metav1.ConditionFalse, k8s.ReasonProviderRefMismatch)), fa},
		{"ConsumerNotAllowed on a VM that was never created", ready(metav1.ConditionFalse, k8s.ReasonConsumerNotAllowed), fa},
		{"CreateOutcomeUnknown", both(ready(metav1.ConditionFalse, k8s.ReasonCreateOutcomeUnknown),
			func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
				setPhaseCond(vm, k8s.ConditionProvisioning, metav1.ConditionTrue, k8s.ReasonCreateRequested)
			}), fa},
		{"create refused", ready(metav1.ConditionFalse, k8s.ReasonCreateRefused), fa},
		{"create backing off", ready(metav1.ConditionFalse, k8s.ReasonCreateBackoff), fa},
		{"create name conflict", ready(metav1.ConditionFalse, k8s.ReasonProviderConflict), fa},
		{"create validation error", ready(metav1.ConditionFalse, k8s.ReasonValidationError), fa},
		{"a validation error on a VM that exists is a failed change, not a failed VM",
			both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn), ready(metav1.ConditionFalse, k8s.ReasonValidationError)), run},
		{"hold wins over a resize in flight", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn),
			func(vm *infravirtrigaudiov1beta1.VirtualMachine) { vm.Status.ReconfigureTaskRef = "task-2" },
			ready(metav1.ConditionFalse, k8s.ReasonVMNotOwned)), fa},

		// Deletion wins over everything.
		{"deleting a running VM", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn),
			func(vm *infravirtrigaudiov1beta1.VirtualMachine) { vm.DeletionTimestamp = &now }), del},
		{"deleting, delete held", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn), ready(metav1.ConditionFalse, k8s.ReasonDeleteBlocked),
			func(vm *infravirtrigaudiov1beta1.VirtualMachine) { vm.DeletionTimestamp = &now }), del},
		{"deleting a VM that was never created", func(vm *infravirtrigaudiov1beta1.VirtualMachine) { vm.DeletionTimestamp = &now }, del},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vm := phaseVM(tc.mutate)
			assert.Equal(t, tc.want, derivePhase(vm))
			// Deterministic: applying it stores the same value, and twice is the same as once.
			applyDerivedPhase(vm)
			assert.Equal(t, tc.want, vm.Status.Phase)
			applyDerivedPhase(vm)
			assert.Equal(t, tc.want, vm.Status.Phase)
		})
	}
}

// TestDerivePhaseNeverEmpty pins that no combination of the inputs that decide
// the phase yields an empty one.
func TestDerivePhaseNeverEmpty(t *testing.T) {
	ids := []string{"", "vm-1"}
	powers := []infravirtrigaudiov1beta1.ObservedPowerState{"", infravirtrigaudiov1beta1.ObservedPowerStateOn,
		infravirtrigaudiov1beta1.ObservedPowerStateOff, infravirtrigaudiov1beta1.ObservedPowerStateSuspended,
		infravirtrigaudiov1beta1.ObservedPowerStateUnknown}
	readyStates := []metav1.ConditionStatus{"", metav1.ConditionTrue, metav1.ConditionFalse}
	reasons := []string{"", k8s.ReasonProviderError, k8s.ReasonVMNotOwned, k8s.ReasonCreateBackoff, k8s.ReasonRestartRequired}
	for _, id := range ids {
		for _, p := range powers {
			for _, rs := range readyStates {
				for _, reason := range reasons {
					vm := phaseVM(nil)
					vm.Status.ID = id
					vm.Status.PowerState = p
					if rs != "" {
						setPhaseCond(vm, k8s.ConditionReady, rs, reason)
					}
					assert.NotEmpty(t, derivePhase(vm), "id=%q power=%q ready=%q/%q", id, p, rs, reason)
				}
			}
		}
	}
}

// TestUpdateStatusStoresDerivedPhase pins that the central status write
// derives the phase from the state it writes, over a stale one.
func TestUpdateStatusStoresDerivedPhase(t *testing.T) {
	vm := phaseVM(func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Status.ID = "vm-1"
		vm.Status.PowerState = infravirtrigaudiov1beta1.ObservedPowerStateOff
		vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseRunning // stale
	})
	r := newTestReconciler(coverageTestScheme(t), nil, vm)
	r.updateStatus(context.Background(), vm)

	got := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(vm), got))
	assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseStopped, got.Status.Phase)
}
