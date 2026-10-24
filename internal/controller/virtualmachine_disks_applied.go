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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// disksNotAppliedFmt is the DisksApplied=False message (and the Warning
// event's): the number of spec.disks entries and the Provider's type. It names
// neither a host nor the Provider's endpoint.
const disksNotAppliedFmt = "spec.disks lists %d disk(s), but this VirtualMachine's Provider (type %s) does not attach " +
	"additional disks: they are not created or attached at create, and later changes to spec.disks are not applied " +
	"either. The VM itself runs without them. Remove spec.disks to clear this condition"

// noteUnattachedDisks records on vm whether its spec.disks can be honoured by
// provider's type (V4): DisksApplied=False/NotSupportedByProvider while
// spec.disks is not empty and the type does not attach additional disks
// (infravirtrigaudiov1beta1.ProviderIgnoresAdditionalDisks), no DisksApplied
// condition otherwise. It sets the condition on vm only; the reconcile's
// status write persists it. A Warning event of the same reason is recorded
// when the condition is first set and again once per new generation (a spec
// change), never per reconcile: the condition's observedGeneration records
// the generation already warned about. Ready is never touched.
func (r *VirtualMachineReconciler) noteUnattachedDisks(vm *infravirtrigaudiov1beta1.VirtualMachine,
	provider *infravirtrigaudiov1beta1.Provider) {
	if provider == nil || len(vm.Spec.Disks) == 0 || !infravirtrigaudiov1beta1.ProviderIgnoresAdditionalDisks(provider.Spec.Type) {
		meta.RemoveStatusCondition(&vm.Status.Conditions, k8s.ConditionDisksApplied)
		return
	}
	msg := fmt.Sprintf(disksNotAppliedFmt, len(vm.Spec.Disks), provider.Spec.Type)
	prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionDisksApplied)
	warned := prev != nil && prev.Status == metav1.ConditionFalse &&
		prev.Reason == k8s.ReasonNotSupportedByProvider && prev.ObservedGeneration == vm.Generation
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionDisksApplied,
		Status:             metav1.ConditionFalse,
		Reason:             k8s.ReasonNotSupportedByProvider,
		Message:            msg,
		ObservedGeneration: vm.Generation,
	})
	if !warned {
		r.recordEvent(vm, corev1.EventTypeWarning, k8s.ReasonNotSupportedByProvider, msg)
	}
}

// carryDisksApplied copies the DisksApplied condition this reconcile decided
// for vm (noteUnattachedDisks) onto toWrite, a status built from the live
// object (the pre-Create marker write), so the condition is written with the
// create — and the Warning event recorded with it is not repeated by the next
// reconcile of the same generation.
func carryDisksApplied(vm, toWrite *infravirtrigaudiov1beta1.VirtualMachine) {
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionDisksApplied)
	if c == nil {
		meta.RemoveStatusCondition(&toWrite.Status.Conditions, k8s.ConditionDisksApplied)
		return
	}
	meta.SetStatusCondition(&toWrite.Status.Conditions, *c)
}
