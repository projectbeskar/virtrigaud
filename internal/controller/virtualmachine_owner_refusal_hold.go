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

// A single-host provider that checks the owner of a per-VM call (Proxmox,
// whose VMIDs are reused) refuses a Describe, power change or reconfigure of
// a VM that is not the VirtualMachine's with VM_NOT_OWNED (a NotFound), and of
// a VM whose owner stamp cannot be read with OWNER_STAMP_UNREADABLE (a
// non-retryable Conflict). Neither is a transient failure, and a VM_NOT_OWNED
// Describe is NOT "the VM is gone": the provider could not prove the
// VirtualMachine's own VM gone, so nothing is created in its place. The VM
// controller holds the VirtualMachine on a constant condition and the
// blocked-VM backoff instead of the 5 s provider-error loop.

// vmNotOwnedHoldMessage is the Ready message of a VirtualMachine held because
// the provider answered VM_NOT_OWNED. It is constant (no provider text, no
// other owner or VM), so a repeated hold leaves the status unchanged.
var vmNotOwnedHoldMessage = fmt.Sprintf("The provider reports that the hypervisor VM at this VirtualMachine's id is "+
	"not this VirtualMachine's: it carries another owner's stamp, or it is unstamped and named differently (its id "+
	"was most likely reused after this VirtualMachine's VM was removed outside VirtRigaud). That VM is neither read "+
	"nor changed, and no VM is created in its place (re-checked with a backoff of up to %s). Deleting this "+
	"VirtualMachine leaves that VM untouched; create the VirtualMachine again to get a new VM. If that VM is this "+
	"VirtualMachine's (renamed on the hypervisor, or adopted while carrying an earlier owner's stamp), restore its "+
	"name, or delete this VirtualMachine (the VM is left in place) and adopt the VM again", blockedRetryMax)

// ownerStampUnreadableHoldMessage is the Ready message of a VirtualMachine
// held because the provider answered a Describe, power change or reconfigure
// with OWNER_STAMP_UNREADABLE. It is constant.
var ownerStampUnreadableHoldMessage = fmt.Sprintf("The provider cannot tell who owns the hypervisor VM at this "+
	"VirtualMachine's id (its owner stamp cannot be read or records more than one owner), so it is neither read nor "+
	"changed (re-checked with a backoff of up to %s). An administrator fixes the stamp or removes the VM by hand. %s",
	blockedRetryMax, imageArtifactDetachHint)

// ownerRefusalHold is one per-VM refusal holdOwnerRefusal holds a
// VirtualMachine on: its Ready reason, constant message and metrics reason.
type ownerRefusalHold struct {
	reason, message, metricsReason string
}

// ownerRefusalHoldFor returns the hold for err, a failed Describe, Power or
// Reconfigure, and false when err is not an owner refusal.
func ownerRefusalHoldFor(err error) (ownerRefusalHold, bool) {
	switch {
	case contracts.IsVMNotOwned(err):
		return ownerRefusalHold{k8s.ReasonVMNotOwned, vmNotOwnedHoldMessage, errReasonVMNotOwned}, true
	case contracts.IsOwnerStampUnreadable(err):
		return ownerRefusalHold{k8s.ReasonOwnerStampUnreadable, ownerStampUnreadableHoldMessage, errReasonOwnerStampUnreadable}, true
	}
	return ownerRefusalHold{}, false
}

// holdOwnerRefusal holds vm when err, the answer of its Describe, Power or
// Reconfigure (op names it for the log), is an owner refusal
// (ownerRefusalHoldFor), and reports whether it did: Ready=False/<reason> with
// the constant message, one Warning event when the hold begins, the status
// written only when it changes, and a re-check with the blocked-VM backoff
// counted from when the hold began (15 s doubling to 5 min). status.id is
// kept: the VM is never created again on this answer. A clustered VM's
// NotFound is handled before this (handleRoutedOpError).
func (r *VirtualMachineReconciler) holdOwnerRefusal(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	ref contracts.VMRef,
	err error,
	op string,
) (ctrl.Result, bool) {
	hold, ok := ownerRefusalHoldFor(err)
	if !ok {
		return ctrl.Result{}, false
	}
	metrics.RecordError(hold.metricsReason, metrics.ComponentManager)
	persisted := vm.Status.DeepCopy()
	since := r.now()
	if prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady); prev != nil &&
		prev.Status == metav1.ConditionFalse && prev.Reason == hold.reason {
		since = prev.LastTransitionTime.Time
	}
	transition := setHeldReady(vm, hold.reason, hold.message)
	retry := blockedRetryBackoff(since)
	log.FromContext(ctx).Info("The provider refused the call: the VM at this id is not this VirtualMachine's, or its "+
		"owner cannot be told; holding", "op", op, "id", ref.ID, "reason", hold.reason, "retryAfter", retry.String(),
		"error", err.Error())
	if !equality.Semantic.DeepEqual(persisted, &vm.Status) {
		r.updateStatus(ctx, vm)
	}
	if transition {
		r.recordEvent(vm, corev1.EventTypeWarning, hold.reason, hold.message)
	}
	return ctrl.Result{RequeueAfter: retry}, true
}
