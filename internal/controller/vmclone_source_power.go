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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	utilk8s "github.com/projectbeskar/virtrigaud/internal/util/k8s"
)

// A libvirt full clone requires a powered-off source (ADR-0007 Slice 5 lab,
// B2): the provider refuses a clone whose source VM is not shut off, before
// anything is copied, with FailedPrecondition + VM_SOURCE_RUNNING, which the
// transport client maps to a retryable error marked
// contracts.ErrVMSourceRunning (never counted toward the circuit breaker).
// The VMClone controller keeps such a clone Pending — Ready=False with reason
// SourceMustBePoweredOff — and retries it with the blocked-VM backoff; it is
// never failed for it, and a clustered clone keeps the target VirtualMachine
// it created (and its pending host). A change of the source VM's observed
// power state re-drives the waiting clones at once (clonesWaitingOnSource), so
// the clone proceeds as soon as the source is off. vSphere clones running VMs
// and never answers this way.

// cloneReasonSourceMustBePoweredOff is the VMClone Ready (and Cloning)
// condition reason, and the event reason, of a clone waiting for its source VM
// to be powered off.
const cloneReasonSourceMustBePoweredOff = "SourceMustBePoweredOff"

// holdCloneForRunningSource records that the provider refused clone because
// its source VM is not powered off (err, a contracts.IsVMSourceRunning
// answer): the clone is Pending with Ready=False/SourceMustBePoweredOff and
// Cloning=False, a Warning event is emitted on the transition only, and the
// clone is re-checked with the blocked-VM backoff (15 s doubling to
// blockedRetryMax) counted from when this hold began — the Ready condition's
// last transition, which a repeated hold with the same message keeps. Nothing
// the clone created is touched.
func (r *VMCloneReconciler) holdCloneForRunningSource(ctx context.Context, clone *infrav1beta1.VMClone, err error) ctrl.Result {
	source := ""
	if clone.Spec.Source.VMRef != nil {
		source = clone.Spec.Source.VMRef.Name
	}
	msg := fmt.Sprintf("waiting for the source VM %q to be powered off (%s); the clone proceeds once the source is off. "+
		"It is re-checked when the source's power state changes, and with a backoff of up to %s",
		source, providerErrorMessage(err), blockedRetryMax)

	var since time.Time
	prev := meta.FindStatusCondition(clone.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	if prev != nil && prev.Reason == cloneReasonSourceMustBePoweredOff {
		since = prev.LastTransitionTime.Time
	}
	if prev == nil || prev.Reason != cloneReasonSourceMustBePoweredOff {
		logging.FromContext(ctx).Info("Clone waits for its source VM to be powered off", "source", source, "error", err.Error())
		r.Recorder.Event(clone, corev1.EventTypeWarning, cloneReasonSourceMustBePoweredOff, msg)
	}
	utilk8s.SetCondition(&clone.Status.Conditions, infrav1beta1.VMCloneConditionCloning, metav1.ConditionFalse,
		cloneReasonSourceMustBePoweredOff, "waiting for the source VM to be powered off")
	res := r.markPending(ctx, clone, cloneReasonSourceMustBePoweredOff, msg)
	res.RequeueAfter = blockedRetryBackoff(since)
	return res
}

// waitsForSourcePowerOff reports whether clone is an unfinished clone of the
// VirtualMachine named source that waits for it to be powered off.
func waitsForSourcePowerOff(clone *infrav1beta1.VMClone, source string) bool {
	if clone.Spec.Source.VMRef == nil || clone.Spec.Source.VMRef.Name != source {
		return false
	}
	if clone.Status.Phase == infrav1beta1.ClonePhaseReady || clone.Status.Phase == infrav1beta1.ClonePhaseFailed {
		return false
	}
	ready := meta.FindStatusCondition(clone.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	return ready != nil && ready.Reason == cloneReasonSourceMustBePoweredOff
}

// clonesWaitingOnSource maps a VirtualMachine whose observed power state
// changed to the VMClones in its namespace (spec.source.vmRef is
// namespace-local) that clone it and wait for it to be powered off, so a clone
// proceeds as soon as its source is off instead of at its next backoff.
func (r *VMCloneReconciler) clonesWaitingOnSource(ctx context.Context, obj client.Object) []reconcile.Request {
	clones := &infrav1beta1.VMCloneList{}
	if err := r.List(ctx, clones, client.InNamespace(obj.GetNamespace())); err != nil {
		logging.FromContext(ctx).Error(err, "Failed to list VMClones for a source VM's power change",
			"namespace", obj.GetNamespace(), "vm", obj.GetName())
		return nil
	}
	var reqs []reconcile.Request
	for i := range clones.Items {
		if waitsForSourcePowerOff(&clones.Items[i], obj.GetName()) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&clones.Items[i])})
		}
	}
	return reqs
}

// sourcePowerStateChanged passes only VirtualMachine updates that change its
// observed power state (status.powerState).
func sourcePowerStateChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldVM, ok := e.ObjectOld.(*infrav1beta1.VirtualMachine)
			if !ok {
				return false
			}
			newVM, ok := e.ObjectNew.(*infrav1beta1.VirtualMachine)
			if !ok {
				return false
			}
			return oldVM.Status.PowerState != newVM.Status.PowerState
		},
	}
}
