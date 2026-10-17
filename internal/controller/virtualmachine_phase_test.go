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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
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

		// Restore holds (ADR-0007 A6).
		{"restored binding held (Ready)", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn),
			ready(metav1.ConditionFalse, k8s.ReasonRestorePending)), fa},
		{"never-placed VM held before placement (Placed/Provisioning)", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			setPhaseCond(vm, k8s.ConditionPlaced, metav1.ConditionFalse, k8s.ReasonRestorePending)
			setPhaseCond(vm, k8s.ConditionProvisioning, metav1.ConditionFalse, k8s.ReasonRestorePending)
		}, fa},
		{"a Placed hold reason is ignored once the VM exists", both(power(infravirtrigaudiov1beta1.ObservedPowerStateOn),
			func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
				setPhaseCond(vm, k8s.ConditionPlaced, metav1.ConditionFalse, k8s.ReasonRestorePending)
			}), run},
		{"unschedulable is not a restore hold", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			setPhaseCond(vm, k8s.ConditionPlaced, metav1.ConditionFalse, k8s.ReasonUnschedulable)
		}, pend},

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

// TestRestoreHoldWritersGiveFailed uses the real hold writers (not synthetic
// conditions): each of them leaves the stored VM in phase Failed.
func TestRestoreHoldWritersGiveFailed(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []string{k8s.ReasonRestorePending, k8s.ReasonOwnDomainOnAnotherHost,
		k8s.ReasonUniquenessCheckFailed, k8s.ReasonProviderLacksListOwnerFilter} {
		t.Run("holdBeforePlacement/"+reason, func(t *testing.T) {
			vm := phaseVM(nil)
			r := newTestReconciler(coverageTestScheme(t), nil, vm)
			r.holdBeforePlacement(ctx, vm, reason, "held", errReasonRestorePending)
			got := &infravirtrigaudiov1beta1.VirtualMachine{}
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), got))
			assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseFailed, got.Status.Phase)
		})
	}

	t.Run("holdRestoredBinding", func(t *testing.T) {
		vm := phaseVM(func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Status.ID = "vm-1"
			vm.Status.PowerState = infravirtrigaudiov1beta1.ObservedPowerStateOn
		})
		r := newTestReconciler(coverageTestScheme(t), nil, vm)
		r.holdRestoredBinding(ctx, vm, contracts.VMRef{ID: "vm-1", HostID: "host-a"})
		got := &infravirtrigaudiov1beta1.VirtualMachine{}
		require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), got))
		assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseFailed, got.Status.Phase)
	})

	t.Run("holdForPreviousIncarnation", func(t *testing.T) {
		vm := phaseVM(func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-a"}
		})
		r := newTestReconciler(coverageTestScheme(t), nil, vm)
		_, err := r.holdForPreviousIncarnation(ctx, vm, "host-a", errors.New("exists"))
		require.NoError(t, err)
		got := &infravirtrigaudiov1beta1.VirtualMachine{}
		require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), got))
		assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseFailed, got.Status.Phase,
			"a hold on a pending host is a restore hold, not a create in flight")
	})
}

// TestStatusDiffersDerivesThePhaseFirst pins the upgrade case: a VM already
// held before the phase was derived has an empty stored phase, and the hold
// path's "did the status change" comparison must see the phase as a change so
// it is written once; with the phase stored, the same state is no change.
func TestStatusDiffersDerivesThePhaseFirst(t *testing.T) {
	vm := phaseVM(func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Status.ID = "vm-1"
		vm.Status.PowerState = infravirtrigaudiov1beta1.ObservedPowerStateOn
		setPhaseCond(vm, k8s.ConditionReady, metav1.ConditionFalse, k8s.ReasonVMNotOwned)
	})
	persisted := vm.Status.DeepCopy() // phase empty, as stored by an older manager
	assert.True(t, statusDiffers(persisted, vm), "the derived phase is a change")
	assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseFailed, vm.Status.Phase)

	persisted = vm.Status.DeepCopy()
	assert.False(t, statusDiffers(persisted, vm), "an unchanged held VM writes nothing")
	assert.True(t, statusDiffers(nil, vm), "an unknown baseline always differs")
}

// TestMarkDeleting_StaleObjectIsNotPatched pins the optimistic lock: when the
// object handled is not the stored one any more, nothing is written, the
// caller's object (phase and resourceVersion) is untouched, and the conflict
// is reported so handleDeletion retries on a fresh read.
func TestMarkDeleting_StaleObjectIsNotPatched(t *testing.T) {
	ctx := context.Background()
	stored := phaseVM(func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Status.ID = "vm-1"
		vm.Status.PowerState = infravirtrigaudiov1beta1.ObservedPowerStateOn
		vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseRunning
	})
	r := newTestReconciler(coverageTestScheme(t), nil, stored)

	stale := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(stored), stale))
	staleRV := stale.ResourceVersion

	// Someone else changes the object after stale was read.
	fresh := stale.DeepCopy()
	fresh.Labels = map[string]string{"edited": "meanwhile"}
	require.NoError(t, r.Update(ctx, fresh))

	assert.True(t, r.markDeleting(ctx, stale), "a stale object reports the conflict")
	assert.Equal(t, staleRV, stale.ResourceVersion, "the caller's resourceVersion is not refreshed over unseen content")
	assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseRunning, stale.Status.Phase)
	got := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(stored), got))
	assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseRunning, got.Status.Phase, "nothing was written")

	// A current object is patched, and takes over only the phase and the new resourceVersion.
	assert.False(t, r.markDeleting(ctx, got))
	assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting, got.Status.Phase)
	after := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(stored), after))
	assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting, after.Status.Phase)
	assert.Equal(t, after.ResourceVersion, got.ResourceVersion)
	assert.Equal(t, "meanwhile", after.Labels["edited"])
}
