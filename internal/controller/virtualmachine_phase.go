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
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// status.phase of a VirtualMachine is a pure function of the rest of its
// state, so it is the same whichever code path writes the status and is never
// left empty on a healthy VM (the PHASE printer column). derivePhase is that
// function; applyDerivedPhase stores its result. Every VirtualMachine status
// write goes through applyDerivedPhase just before the write, so the phase is
// written in the same request as the facts it summarizes and the status
// changes only when one of those facts does.

// observedPowerStateOffGraceful is the legacy observed power state a status
// written by an older manager may still carry (see ObservedPowerState).
const observedPowerStateOffGraceful infravirtrigaudiov1beta1.ObservedPowerState = "OffGraceful"

// holdPhaseReasons are the Ready=False reasons that put a VirtualMachine in
// phase Failed whether or not it has an id: each is a hold that only an
// operator (or the removal of its cause) lifts, so the VM is not progressing.
var holdPhaseReasons = map[string]bool{
	k8s.ReasonVMNotOwned:                true,
	k8s.ReasonOwnerStampUnreadable:      true,
	k8s.ReasonProviderUnavailable:       true,
	k8s.ReasonVMIsImageArtifact:         true,
	k8s.ReasonCreateOutcomeUnknown:      true,
	k8s.ReasonDeleteBlocked:             true,
	k8s.ReasonProviderRefMismatch:       true,
	k8s.ReasonPlacementTopologyMismatch: true,
	k8s.ReasonConsumerNotAllowed:        true,
	k8s.ReasonVMMissingOnHost:           true,
}

// restoreHoldPhaseReasons are the reasons of the clustered restore guards'
// holds (ADR-0007 A6, R1 and R4: holdBeforePlacement,
// holdForPreviousIncarnation, holdRestoredBinding). The VM is held, not
// failed: it proceeds by itself once the cause is gone — the runbook
// re-attached or removed the previous domain, the marker names the VM's own
// UID, the pre-schedule check can run — and nothing about it is lost or
// refused, so derivePhase reports Pending (V3b; earlier releases reported
// Failed). They are written to the Placed and Provisioning conditions of a
// VM that was never placed, and RestorePending to Ready of a bound VM whose
// restore marker names another UID; derivePhase looks at both.
var restoreHoldPhaseReasons = map[string]bool{
	k8s.ReasonRestorePending:               true,
	k8s.ReasonOwnDomainOnAnotherHost:       true,
	k8s.ReasonUniquenessCheckFailed:        true,
	k8s.ReasonProviderLacksListOwnerFilter: true,
}

// placementHoldPhaseReasons are the Placed/Provisioning reasons of a
// clustered VM that was never placed and whose hold needs an operator to
// resolve a conflict on the host (ImportedDiskPinned: a migration target whose
// Create on the host its disk landed on was refused with a name conflict), so
// the VM is Failed.
var placementHoldPhaseReasons = map[string]bool{
	k8s.ReasonImportedDiskPinned: true,
}

// createFailurePhaseReasons are the Ready=False reasons that put a
// VirtualMachine WITHOUT an id in phase Failed: its Create was refused, or
// failed and is retried with a backoff. On a VM that exists the same reasons
// describe a failed change to it, not the VM, so they do not change its phase.
var createFailurePhaseReasons = map[string]bool{
	k8s.ReasonCreateBackoff:    true,
	k8s.ReasonCreateRefused:    true,
	k8s.ReasonProviderConflict: true,
	k8s.ReasonValidationError:  true,
}

// derivePhase returns the status.phase for vm, from its deletion state, its
// conditions, its power state as the provider last reported it, and whether it
// has been created:
//
//   - being deleted: Deleting (also while the delete is held; the hold is on
//     the Ready/DeleteBlocked conditions).
//   - Ready=False with a hold reason (holdPhaseReasons); with no id, also a
//     failed-Create reason (createFailurePhaseReasons) on Ready, or an
//     operator hold (placementHoldPhaseReasons) on Placed or Provisioning:
//     Failed.
//   - a clustered restore guard's hold (restoreHoldPhaseReasons) on Ready, or,
//     with no id, on Placed or Provisioning: Pending — the VM waits, and
//     proceeds once the cause is gone.
//   - no id: Provisioning once a Create was requested, a host was chosen for
//     it, a task is recorded or the Provisioning condition is true; else
//     Pending (waiting for dependencies, placement, an adoption).
//   - an id and a resize in flight (Reconfiguring=True other than
//     RestartRequired, or a reconfigure task recorded): Reconfiguring. A resize
//     applied to the persistent definition only (RestartRequired) leaves the
//     VM's phase to its power state.
//   - an id: by power state. On and Suspended (still active) are Running; Off
//     and the legacy OffGraceful are Stopped; Unknown (a state the provider
//     cannot classify) is Failed for as long as the provider reports it (it
//     is the current observation, not a sticky failure: the next reconcile
//     that sees a classifiable state clears it). With no power state observed yet the VM is
//     Running if Ready, else Provisioning.
//
// The function reads vm only.
func derivePhase(vm *infravirtrigaudiov1beta1.VirtualMachine) infravirtrigaudiov1beta1.VirtualMachinePhase {
	if !vm.DeletionTimestamp.IsZero() {
		return infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting
	}
	bound := vm.Status.ID != ""
	ready := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)

	if ready != nil && ready.Status == metav1.ConditionFalse {
		if holdPhaseReasons[ready.Reason] || (!bound && createFailurePhaseReasons[ready.Reason]) {
			return infravirtrigaudiov1beta1.VirtualMachinePhaseFailed
		}
		if restoreHoldPhaseReasons[ready.Reason] {
			return infravirtrigaudiov1beta1.VirtualMachinePhasePending
		}
	}

	if !bound {
		if placementHeld(vm, placementHoldPhaseReasons) {
			return infravirtrigaudiov1beta1.VirtualMachinePhaseFailed
		}
		if placementHeld(vm, restoreHoldPhaseReasons) {
			return infravirtrigaudiov1beta1.VirtualMachinePhasePending
		}
		if createRequested(vm) || pendingHost(vm) != "" || vm.Status.LastTaskRef != "" || provisioningInFlight(vm) {
			return infravirtrigaudiov1beta1.VirtualMachinePhaseProvisioning
		}
		return infravirtrigaudiov1beta1.VirtualMachinePhasePending
	}

	if reconfigureInFlight(vm) {
		return infravirtrigaudiov1beta1.VirtualMachinePhaseReconfiguring
	}

	switch vm.Status.PowerState {
	case infravirtrigaudiov1beta1.ObservedPowerStateOn, infravirtrigaudiov1beta1.ObservedPowerStateSuspended:
		return infravirtrigaudiov1beta1.VirtualMachinePhaseRunning
	case infravirtrigaudiov1beta1.ObservedPowerStateOff, observedPowerStateOffGraceful:
		return infravirtrigaudiov1beta1.VirtualMachinePhaseStopped
	case infravirtrigaudiov1beta1.ObservedPowerStateUnknown:
		return infravirtrigaudiov1beta1.VirtualMachinePhaseFailed
	}
	// Created, but not described yet.
	if ready != nil && ready.Status == metav1.ConditionTrue {
		return infravirtrigaudiov1beta1.VirtualMachinePhaseRunning
	}
	return infravirtrigaudiov1beta1.VirtualMachinePhaseProvisioning
}

// placementHeld reports whether vm is held by one of reasons on its Placed or
// Provisioning condition (False).
func placementHeld(vm *infravirtrigaudiov1beta1.VirtualMachine, reasons map[string]bool) bool {
	for _, condType := range []string{k8s.ConditionPlaced, k8s.ConditionProvisioning} {
		c := meta.FindStatusCondition(vm.Status.Conditions, condType)
		if c != nil && c.Status == metav1.ConditionFalse && reasons[c.Reason] {
			return true
		}
	}
	return false
}

// provisioningInFlight reports whether vm's Provisioning condition is true for
// a reason that means a Create is under way (not WaitingForDependencies, which
// is a VM that waits for another controller to bind it).
func provisioningInFlight(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionProvisioning)
	return c != nil && c.Status == metav1.ConditionTrue && c.Reason != k8s.ReasonWaitingForDependencies
}

// reconfigureInFlight reports whether a resize of vm is under way: a recorded
// reconfigure task, or Reconfiguring=True for any reason but RestartRequired
// (a change that takes effect at the next power cycle is not in flight).
func reconfigureInFlight(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	if vm.Status.ReconfigureTaskRef != "" {
		return true
	}
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring)
	return c != nil && c.Status == metav1.ConditionTrue && c.Reason != k8s.ReasonRestartRequired
}

// applyDerivedPhase stores derivePhase(vm) in vm.Status.Phase. Call it on the
// object about to be written, after its conditions and power state are final.
func applyDerivedPhase(vm *infravirtrigaudiov1beta1.VirtualMachine) {
	vm.Status.Phase = derivePhase(vm)
}

// statusDiffers applies the derived phase to vm and reports whether vm's status
// differs from persisted, the status as read (nil: unknown, so it differs).
// The hold paths write the status only when a hold changed it; deriving the
// phase first makes a VM that was already held before the phase was derived
// (an upgrade) get its phase on the first reconcile instead of never.
func statusDiffers(persisted *infravirtrigaudiov1beta1.VirtualMachineStatus, vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	applyDerivedPhase(vm)
	return persisted == nil || !equality.Semantic.DeepEqual(persisted, &vm.Status)
}

// deletingPhaseConflictRetry is how long handleDeletion waits before it
// reconciles again after markDeleting lost a resourceVersion race (the cached
// object was stale): long enough for the cache to catch up.
const deletingPhaseConflictRetry = time.Second

// markDeleting records phase Deleting on a VirtualMachine whose deletion has
// begun, once: a VM whose delete takes a while (a held or retried delete, a
// provider call) then shows Deleting instead of its last running phase. It is
// a merge patch of the phase alone with an optimistic lock: the patch is
// refused if vm is not the stored object any more, so the caller's resourceVersion
// is never refreshed over content it has not seen (its later status and
// finalizer updates keep their own check). It writes only when the phase
// differs. It reports true when the write lost that race, and the caller
// retries after deletingPhaseConflictRetry; any other failure is logged and
// not retried (the delete does not wait on it, and the next reconcile tries
// again).
func (r *VirtualMachineReconciler) markDeleting(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) (conflict bool) {
	if vm.Status.Phase == infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting {
		return false
	}
	patched := vm.DeepCopy()
	patched.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting
	if err := r.Status().Patch(ctx, patched, client.MergeFromWithOptions(vm, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			log.FromContext(ctx).V(1).Info("The VirtualMachine changed before phase Deleting was recorded; retrying")
			return true
		}
		log.FromContext(ctx).V(1).Info("Cannot record phase Deleting on the VirtualMachine; the delete goes on", "error", err.Error())
		return false
	}
	// Take over only what the patch changed: the stored object is the one vm
	// was read from (the lock), so its resourceVersion is vm's again.
	vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting
	vm.ResourceVersion = patched.ResourceVersion
	return false
}
