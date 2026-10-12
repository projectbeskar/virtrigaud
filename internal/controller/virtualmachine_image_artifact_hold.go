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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A provider refuses a per-VM operation with VM_IS_IMAGE_ARTIFACT
// (contracts.IsVMIsImageArtifact: a non-retryable Conflict, nothing changed)
// when the hypervisor object a VirtualMachine is bound to is a template or a
// VirtRigaud image artifact, not an ordinary VM — today vSphere does, e.g. for
// a VM adopted from a template or bound to an image VM. The refusal holds
// until an operator detaches the VirtualMachine, so the VM controller holds it
// on a constant condition and a slow backoff instead of retrying it as an
// ordinary failure every few seconds.

// imageArtifactDetachHint is the part of every VMIsImageArtifact message that
// says how the owner ends the hold.
var imageArtifactDetachHint = fmt.Sprintf("To release this VirtualMachine without touching that object, set the "+
	"annotation %s: \"true\" and delete the VirtualMachine (the object is then left on the hypervisor)",
	infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation)

// imageArtifactDeleteMessage is the Ready (and, clustered, DeleteBlocked)
// message of a VirtualMachine whose provider Delete was refused with
// VM_IS_IMAGE_ARTIFACT. It is constant (no provider text), so a repeated hold
// leaves the status unchanged.
var imageArtifactDeleteMessage = fmt.Sprintf("Delete refused: the provider reports that the hypervisor object this "+
	"VirtualMachine is bound to is a template or a VirtRigaud image artifact, not an ordinary VM; deleting it would "+
	"destroy the image for every VM cloned from it, so nothing was deleted and the finalizer is kept (re-checked "+
	"with a backoff of up to %s). %s", blockedRetryMax, imageArtifactDetachHint)

// imageArtifactPowerMessage is the Ready message of a VirtualMachine whose
// provider Power was refused with VM_IS_IMAGE_ARTIFACT. It is constant.
var imageArtifactPowerMessage = fmt.Sprintf("Power change refused: the provider reports that the hypervisor object "+
	"this VirtualMachine is bound to is a template or a VirtRigaud image artifact, not an ordinary VM, so its power "+
	"state is not changed (re-checked with a backoff of up to %s). %s", blockedRetryMax, imageArtifactDetachHint)

// errReasonVMIsImageArtifact is the metrics reason of a delete or power change
// held because the provider reports the object is an image artifact.
const errReasonVMIsImageArtifact = "vm-is-image-artifact"

// retainForImageArtifactDelete keeps the finalizer of a VirtualMachine whose
// provider Delete was refused with VM_IS_IMAGE_ARTIFACT
// (contracts.IsVMIsImageArtifact). Nothing was deleted, and retrying cannot
// change the answer, so the delete is held — never retried as an ordinary
// failure:
//
//   - a single-host VM gets Ready=False/VMIsImageArtifact with the constant
//     imageArtifactDeleteMessage (it never carries DeleteBlocked), and is
//     re-checked with the blocked-VM backoff counted from its
//     deletionTimestamp;
//   - a clustered (routed) VM is held like every other clustered delete:
//     DeleteBlocked=True/VMIsImageArtifact and Ready=False/DeleteBlocked
//     (recordDeleteHold).
//
// The status is written, and one Warning event recorded, only when the hold
// begins; the cause goes to the log only. A force-delete or orphan-on-delete
// set meanwhile is acted on at once (an annotation change re-triggers a
// deleting VM, vmUpdateNeedsReconcile).
func (r *VirtualMachineReconciler) retainForImageArtifactDelete(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	routed bool,
	err error,
) ctrl.Result {
	metrics.RecordError(errReasonVMIsImageArtifact, metrics.ComponentManager)
	if routed {
		return r.recordDeleteHold(ctx, vm, k8s.ReasonVMIsImageArtifact, imageArtifactDeleteMessage, deleteHeldLogMessage, err)
	}
	var since time.Time
	if vm.DeletionTimestamp != nil {
		since = vm.DeletionTimestamp.Time
	}
	retry := blockedRetryBackoff(since)
	log.FromContext(ctx).Info("Provider refused to delete the VM: the object is a template or an image artifact; "+
		"retaining the finalizer", "id", vm.Status.ID, "retryAfter", retry.String(), "error", err.Error())
	persisted := vm.Status.DeepCopy()
	transition := r.setImageArtifactReady(vm, imageArtifactDeleteMessage)
	meta.RemoveStatusCondition(&vm.Status.Conditions, k8s.ConditionDeleteBlocked)
	if !equality.Semantic.DeepEqual(persisted, &vm.Status) {
		r.updateStatus(ctx, vm)
	}
	if transition {
		r.recordEvent(vm, corev1.EventTypeWarning, k8s.ReasonVMIsImageArtifact, imageArtifactDeleteMessage)
	}
	return ctrl.Result{RequeueAfter: retry}
}

// holdImageArtifactPower holds a VirtualMachine whose provider Power was
// refused with VM_IS_IMAGE_ARTIFACT: Ready=False/VMIsImageArtifact with the
// constant imageArtifactPowerMessage, one Warning event when the hold begins,
// the status written only when it changes, and a re-check with the blocked-VM
// backoff counted from when the hold began (15 s doubling to 5 min) instead
// of the 5 s provider-error cadence. A spec change (e.g. a powerState the VM
// already has) is acted on at once, as for any VM.
func (r *VirtualMachineReconciler) holdImageArtifactPower(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	err error,
) ctrl.Result {
	metrics.RecordError(errReasonVMIsImageArtifact, metrics.ComponentManager)
	persisted := vm.Status.DeepCopy()
	since := r.now()
	if prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady); prev != nil &&
		prev.Status == metav1.ConditionFalse && prev.Reason == k8s.ReasonVMIsImageArtifact {
		since = prev.LastTransitionTime.Time
	}
	transition := r.setImageArtifactReady(vm, imageArtifactPowerMessage)
	retry := blockedRetryBackoff(since)
	log.FromContext(ctx).Info("Provider refused the power change: the object is a template or an image artifact; holding",
		"id", vm.Status.ID, "retryAfter", retry.String(), "error", err.Error())
	if !equality.Semantic.DeepEqual(persisted, &vm.Status) {
		r.updateStatus(ctx, vm)
	}
	if transition {
		r.recordEvent(vm, corev1.EventTypeWarning, k8s.ReasonVMIsImageArtifact, imageArtifactPowerMessage)
	}
	return ctrl.Result{RequeueAfter: retry}
}

// vmNotOwnedLeftMessage is the VMLeftOnHypervisor Warning recorded when a
// VirtualMachine is released because its provider Delete answered
// VM_NOT_OWNED. It is constant: nothing the provider found is put in it.
const vmNotOwnedLeftMessage = "The VirtualMachine was removed without deleting a hypervisor VM: the provider " +
	"reports that the VM at its id is not this VirtualMachine's (another owner's stamp, or another name), and left " +
	"it in place. Its own VM is treated as gone. If this VirtualMachine was re-created under a new UID (for example " +
	"by a backup restore), that VM may be its own and is now unmanaged: check the hypervisor and remove or adopt it"

// errReasonVMNotOwned is the metrics reason of a delete released on a
// VM_NOT_OWNED answer.
const errReasonVMNotOwned = "vm-not-owned"

// recordVMNotOwnedRelease records that vm's provider Delete was answered
// VM_NOT_OWNED (contracts.IsVMNotOwned): the caller releases the finalizer —
// the VM at the id is not this VirtualMachine's (a reused id) — and this logs
// it at Info, counts it, and records the constant VMLeftOnHypervisor Warning.
func (r *VirtualMachineReconciler) recordVMNotOwnedRelease(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	ref contracts.VMRef,
) {
	log.FromContext(ctx).Info("The provider reports that the VM at this id is not this VirtualMachine's; releasing the "+
		"finalizer without deleting it", "id", ref.ID, "host", ref.HostID)
	metrics.RecordError(errReasonVMNotOwned, metrics.ComponentManager)
	r.recordEvent(vm, corev1.EventTypeWarning, eventReasonVMLeftOnHypervisor, vmNotOwnedLeftMessage)
}

// setImageArtifactReady sets Ready=False/VMIsImageArtifact with msg on vm and
// reports whether that begins a hold (Ready was not already False for that
// reason with that message).
func (r *VirtualMachineReconciler) setImageArtifactReady(vm *infravirtrigaudiov1beta1.VirtualMachine, msg string) bool {
	// Read before the condition is set: FindStatusCondition returns a pointer
	// into the slice that SetStatusCondition updates in place.
	prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
	transition := prev == nil || prev.Status != metav1.ConditionFalse || prev.Reason != k8s.ReasonVMIsImageArtifact ||
		prev.Message != msg
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             k8s.ReasonVMIsImageArtifact,
		Message:            msg,
		ObservedGeneration: vm.Generation,
	})
	return transition
}
