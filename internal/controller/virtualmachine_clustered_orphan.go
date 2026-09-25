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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	key := placementProviderKey(vm)
	provider := &infravirtrigaudiov1beta1.Provider{}
	if err := r.Get(ctx, key, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, false, nil // no Provider, no accounting to leave
		}
		return ctrl.Result{}, false, fmt.Errorf("get Provider %s to decide orphan-on-delete of VirtualMachine %s/%s: %w",
			key, vm.Namespace, vm.Name, err)
	}
	if !isClusterTopology(provider) || vm.Namespace == provider.Namespace ||
		provider.Annotations[infravirtrigaudiov1beta1.ProviderAllowConsumerOrphanOnDeleteAnnotation] == "true" {
		return ctrl.Result{}, false, nil
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
