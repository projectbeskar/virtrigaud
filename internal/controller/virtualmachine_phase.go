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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
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
	k8s.ReasonVMNotOwned:                   true,
	k8s.ReasonOwnerStampUnreadable:         true,
	k8s.ReasonProviderUnavailable:          true,
	k8s.ReasonVMIsImageArtifact:            true,
	k8s.ReasonCreateOutcomeUnknown:         true,
	k8s.ReasonDeleteBlocked:                true,
	k8s.ReasonProviderRefMismatch:          true,
	k8s.ReasonPlacementTopologyMismatch:    true,
	k8s.ReasonConsumerNotAllowed:           true,
	k8s.ReasonVMMissingOnHost:              true,
	k8s.ReasonOwnDomainOnAnotherHost:       true,
	k8s.ReasonUniquenessCheckFailed:        true,
	k8s.ReasonProviderLacksListOwnerFilter: true,
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
//   - Ready=False with a hold reason (holdPhaseReasons), or, with no id, a
//     failed-Create reason (createFailurePhaseReasons): Failed.
//   - no id: Provisioning once a Create was requested, a host was chosen for
//     it, a task is recorded or the Provisioning condition is true; else
//     Pending (waiting for dependencies, placement, an adoption).
//   - an id and a resize in flight (Reconfiguring=True other than
//     RestartRequired, or a reconfigure task recorded): Reconfiguring. A resize
//     applied to the persistent definition only (RestartRequired) leaves the
//     VM's phase to its power state.
//   - an id: by power state. On and Suspended (still active) are Running; Off
//     and the legacy OffGraceful are Stopped; Unknown (a state the provider
//     cannot classify) is Failed. With no power state observed yet the VM is
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
	}

	if !bound {
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

// markDeleting records phase Deleting on a VirtualMachine whose deletion has
// begun, once: a VM whose delete takes a while (a held or retried delete, a
// provider call) then shows Deleting instead of its last running phase. It is
// a merge patch of the phase alone, so it needs no fresh resourceVersion and
// changes nothing else; it writes only when the phase differs, and a failed
// write is logged and not retried (the next reconcile tries again, and the
// delete itself does not wait on it).
func (r *VirtualMachineReconciler) markDeleting(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) {
	if vm.Status.Phase == infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting {
		return
	}
	patched := vm.DeepCopy()
	patched.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting
	if err := r.Status().Patch(ctx, patched, client.MergeFrom(vm)); err != nil {
		log.FromContext(ctx).V(1).Info("Cannot record phase Deleting on the VirtualMachine; the delete goes on", "error", err.Error())
		return
	}
	// Take over only what the patch changed, so the caller's object keeps
	// everything else it holds, and its resourceVersion is the stored one.
	vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting
	vm.ResourceVersion = patched.ResourceVersion
}
