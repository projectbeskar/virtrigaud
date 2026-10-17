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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// providerDeletingRetryInterval re-checks a VirtualMachine held because its
// Provider is being deleted. The VM controller does not watch Providers'
// deletion, and the hold ends only when the VM's owner points it at another
// Provider or deletes it (or a Provider of that name exists again), so the
// re-check is unhurried.
const providerDeletingRetryInterval = time.Minute

// errReasonProviderDeleting is the metrics reason of a create held because the
// VM's Provider is being deleted.
const errReasonProviderDeleting = "provider-deleting"

// providerDeletingMessage is the Ready and Provisioning message of a VM held
// by holdForDeletingProvider. It is constant, so a repeated hold leaves the
// status unchanged.
const providerDeletingMessage = "The Provider this VirtualMachine references is being deleted, and a VM is created only " +
	"through a Provider that is not: nothing is created. Point spec.providerRef at another Provider, or delete this " +
	"VirtualMachine; it is created only if a Provider of that name exists again and is not being deleted"

// providerDeleting reports whether provider is being deleted (a non-zero
// deletionTimestamp; its finalizers may hold it for as long as VMs and their
// snapshots, clones and migrations still need it).
func providerDeleting(provider *infravirtrigaudiov1beta1.Provider) bool {
	return provider != nil && !provider.DeletionTimestamp.IsZero()
}

// holdForDeletingProvider keeps vm from being created (or re-created) on the
// hypervisor while its Provider is being deleted: a VM is created only
// through a Provider that is not. The held VM records no Create (it never
// gets the create marker), so it does not keep the Provider in use. No
// provider call is made:
// Ready=False and Provisioning=False with reason ProviderDeleting and a
// constant message, a Warning event when the hold begins, and a re-check
// every providerDeletingRetryInterval. persisted is the status as read; the
// status is written only when it changes. A VM that is already created keeps
// being managed (described, powered, deleted) as usual.
func (r *VirtualMachineReconciler) holdForDeletingProvider(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	persisted *infravirtrigaudiov1beta1.VirtualMachineStatus,
	provider *infravirtrigaudiov1beta1.Provider,
) ctrl.Result {
	prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
	isNew := prev == nil || prev.Reason != k8s.ReasonProviderDeleting
	condTypes := []string{k8s.ConditionReady, k8s.ConditionProvisioning}
	if createRequested(vm) {
		// The create marker (a Create may already have made the VM) is kept:
		// only Ready says why it waits. This hold never writes the marker.
		condTypes = condTypes[:1]
	}
	for _, condType := range condTypes {
		meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionFalse,
			Reason:             k8s.ReasonProviderDeleting,
			Message:            providerDeletingMessage,
			ObservedGeneration: vm.Generation,
		})
	}
	log.FromContext(ctx).Info("Not creating the VM: its Provider is being deleted",
		"provider", provider.Namespace+"/"+provider.Name, "retryAfter", providerDeletingRetryInterval.String())
	metrics.RecordError(errReasonProviderDeleting, metrics.ComponentManager)
	if statusDiffers(persisted, vm) {
		r.updateStatus(ctx, vm)
	}
	if isNew {
		r.recordEvent(vm, corev1.EventTypeWarning, k8s.ReasonProviderDeleting, providerDeletingMessage)
	}
	return ctrl.Result{RequeueAfter: providerDeletingRetryInterval}
}
