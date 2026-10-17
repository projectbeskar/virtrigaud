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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// orphanRefusedRetryInterval re-checks a refused orphan-on-delete: it clears
// when the Provider's administrator allows it or the VM's owner removes the
// annotation, both human actions.
const orphanRefusedRetryInterval = 2 * time.Minute

// errReasonOrphanNotAllowed is the metrics reason of a refused orphan-on-delete.
const errReasonOrphanNotAllowed = "orphan-not-allowed"

// orphanRefusal decides whether the orphan-on-delete of vm must be refused
// (review M4). An orphaned VM keeps running on its host but no longer counts
// toward the committed capacity of a clustered Provider, so a consumer in
// another namespace could otherwise occupy capacity nobody sees. It is refused
// when all of these hold:
//
//   - the VM is bound (a provider id or a pending create) — an unbound VM has
//     nothing to leave behind;
//   - its placement Provider exists and has topology: cluster (single-host
//     Providers are unchanged);
//   - the VM is in another namespace than that Provider;
//   - the Provider does not carry
//     infra.virtrigaud.io/allow-consumer-orphan-on-delete: "true".
//
// A refusal keeps the finalizer and records Ready=False/
// OrphanOnDeleteNotAllowed plus a Warning event (on the transition) telling
// the owner to ask the Provider's administrator, or to remove the annotation
// for a normal delete. A failed Provider read is returned as an error (the
// finalizer stays).
func (r *VirtualMachineReconciler) orphanRefusal(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) (ctrl.Result, bool, error) {
	if !vmIsBound(vm) {
		return ctrl.Result{}, false, nil
	}
	key, denied, err := r.consumerDetachDenied(ctx, vm, infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation)
	if err != nil || !denied {
		return ctrl.Result{}, false, err
	}

	msg := fmt.Sprintf("%s=true is not honoured: VirtualMachines in namespace %s may not detach VMs from the clustered Provider %s "+
		"(an orphaned VM keeps running on its host outside the capacity accounting). Ask the Provider's administrator to set %s=\"true\" "+
		"on it, or remove the annotation to delete the VM normally; the finalizer is kept until then",
		infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation, vm.Namespace, key,
		infravirtrigaudiov1beta1.ProviderAllowConsumerOrphanOnDeleteAnnotation)
	prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
	isNew := prev == nil || prev.Reason != k8s.ReasonOrphanOnDeleteNotAllowed
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             k8s.ReasonOrphanOnDeleteNotAllowed,
		Message:            msg,
		ObservedGeneration: vm.Generation,
	})
	log.FromContext(ctx).Info("Refusing orphan-on-delete of a consumer VM on a clustered Provider; keeping the finalizer",
		"provider", key.String())
	metrics.RecordError(errReasonOrphanNotAllowed, metrics.ComponentManager)
	if isNew {
		r.recordEvent(vm, corev1.EventTypeWarning, k8s.ReasonOrphanOnDeleteNotAllowed, msg)
	}
	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: orphanRefusedRetryInterval}, true, nil
}

// consumerDetachDenied reports whether vm may NOT leave its hypervisor VM
// behind (orphan-on-delete, or force-delete without a confirmed provider
// delete; annotation names which, for the error) because it is a consumer of
// a clustered Provider that has not allowed it (review M4): its placement
// Provider exists, has topology: cluster, is in another namespace than vm, and
// does not carry ProviderAllowConsumerOrphanOnDeleteAnnotation: "true". It
// returns the Provider's key for the caller's message. A Provider that does
// not exist denies nothing (no accounting to leave); a failed read is
// returned as an error. The caller decides whether vm has anything to leave.
func (r *VirtualMachineReconciler) consumerDetachDenied(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	annotation string,
) (types.NamespacedName, bool, error) {
	key := placementProviderKey(vm)
	denied, err := consumerDetachDeniedFor(ctx, r.Client, key, vm.Namespace)
	if err != nil {
		return key, false, fmt.Errorf("decide %s of VirtualMachine %s/%s: %w", annotation, vm.Namespace, vm.Name, err)
	}
	return key, denied, nil
}

// errReasonForceDeleteNotAllowed is the metrics reason of a refused
// force-delete.
const errReasonForceDeleteNotAllowed = "force-delete-not-allowed"

// forceDeleteRefusedMessage is the ConditionForceDeleteRefused message (and
// Warning event) of a refused force-delete of a VM in namespace ns whose
// clustered Provider is provider. It is constant for a given VM.
func forceDeleteRefusedMessage(ns string, provider types.NamespacedName) string {
	return fmt.Sprintf("%s=true is not honoured: VirtualMachines in namespace %s may not be released from the clustered "+
		"Provider %s without a confirmed provider delete (the hypervisor VM would keep running on its host outside the "+
		"capacity accounting). The delete keeps being retried as if the annotation were not set; ask the Provider's "+
		"administrator to set %s=\"true\" on it, or to remove the hypervisor VM",
		forceDeleteAnnotation, ns, provider, infravirtrigaudiov1beta1.ProviderAllowConsumerOrphanOnDeleteAnnotation)
}

// forceDeleteHonoured decides, for a VirtualMachine being deleted, whether
// its force-delete escape hatch may release it without a confirmed provider
// delete. It is false when the annotation is not set, and — the cross-namespace
// gate, as for orphan-on-delete (orphanRefusal) — when vm has something to
// leave behind (it is bound, or its own domain was found on another host)
// and is a consumer of a clustered Provider that has not allowed it
// (consumerDetachDenied). A refused force-delete is ignored: the delete is
// retried as if it were not set, and ConditionForceDeleteRefused=True (with a
// Warning event when it is set) says why it does not release the VM.
// Same-namespace VMs and single-host Providers are unchanged.
//
// The condition is removed once the force-delete is honoured or the
// annotation is gone. Its status is written only when that changes something,
// so a repeated refusal writes nothing. A failed Provider read is returned as
// an error (the finalizer stays).
func (r *VirtualMachineReconciler) forceDeleteHonoured(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) (bool, error) {
	if !hasForceDeleteAnnotation(vm) {
		r.clearForceDeleteRefused(ctx, vm)
		return false, nil
	}
	if !vmIsBound(vm) && !heldForOwnDomainElsewhere(vm) {
		r.clearForceDeleteRefused(ctx, vm)
		return true, nil
	}
	key, denied, err := r.consumerDetachDenied(ctx, vm, forceDeleteAnnotation)
	if err != nil {
		return false, err
	}
	if !denied {
		r.clearForceDeleteRefused(ctx, vm)
		return true, nil
	}

	msg := forceDeleteRefusedMessage(vm.Namespace, key)
	persisted := vm.Status.DeepCopy()
	isNew := setForceDeleteRefused(&vm.Status.Conditions, vm.Generation, msg)
	log.FromContext(ctx).Info("Not honouring force-delete of a consumer VM on a clustered Provider; the delete is retried as if it were not set",
		"provider", key.String(), "annotation", forceDeleteAnnotation)
	metrics.RecordError(errReasonForceDeleteNotAllowed, metrics.ComponentManager)
	if statusDiffers(persisted, vm) {
		r.updateStatus(ctx, vm)
	}
	if isNew {
		r.recordEvent(vm, corev1.EventTypeWarning, k8s.ReasonForceDeleteNotAllowed, msg)
	}
	return false, nil
}

// clearForceDeleteRefused removes ConditionForceDeleteRefused from vm and
// writes the status when it was there.
func (r *VirtualMachineReconciler) clearForceDeleteRefused(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) {
	if meta.RemoveStatusCondition(&vm.Status.Conditions, k8s.ConditionForceDeleteRefused) {
		r.updateStatus(ctx, vm)
	}
}
