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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// A VirtualMachine being deleted is reconciled when its deletion is requested
// and then only on a change of its generation, annotations or finalizers —
// never on its own status writes or another metadata-only update, which used
// to re-run a held delete (and its provider calls) at once instead of on its
// backoff.

func TestVMUpdateNeedsReconcile_DeletingVM(t *testing.T) {
	live := clusterVM("web", clusteredNS, "prov-cluster")
	live.Generation = 3
	live.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}

	requested := live.DeepCopy()
	now := metav1.Now()
	requested.DeletionTimestamp = &now
	assert.True(t, vmUpdateNeedsReconcile(live, requested), "the deletion request is reconciled")

	statusOnly := requested.DeepCopy()
	statusOnly.ResourceVersion = "42"
	meta.SetStatusCondition(&statusOnly.Status.Conditions, metav1.Condition{Type: k8s.ConditionReady,
		Status: metav1.ConditionFalse, Reason: k8s.ReasonProviderUnavailable, Message: "held"})
	assert.False(t, vmUpdateNeedsReconcile(requested, statusOnly), "the controller's own status write is not reconciled")

	labelled := statusOnly.DeepCopy()
	labelled.Labels = map[string]string{"team": "a"}
	labelled.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl"}}
	assert.False(t, vmUpdateNeedsReconcile(statusOnly, labelled), "labels are not read by the delete path")

	forced := labelled.DeepCopy()
	forced.Annotations = map[string]string{forceDeleteAnnotation: "true"}
	assert.True(t, vmUpdateNeedsReconcile(labelled, forced), "force-delete set on a held delete is acted on at once")

	orphaned := labelled.DeepCopy()
	orphaned.Annotations = map[string]string{infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation: "true"}
	assert.True(t, vmUpdateNeedsReconcile(labelled, orphaned), "orphan-on-delete set on a held delete is acted on at once")

	finalizers := labelled.DeepCopy()
	finalizers.Finalizers = append(finalizers.Finalizers, "example.com/other")
	assert.True(t, vmUpdateNeedsReconcile(labelled, finalizers), "a finalizer change is reconciled")

	specChange := labelled.DeepCopy()
	specChange.Generation = 4
	assert.True(t, vmUpdateNeedsReconcile(labelled, specChange))
}

// TestHeldDelete_StatusWritesDoNotRetrigger runs a held single-host delete
// (its Provider cannot be reached) as the controller would: its own status
// write, and a label edit, pass no update to the reconciler, so no provider
// is resolved and no Delete is sent until the backoff; a force-delete
// annotation passes and releases the VM without a provider call.
func TestHeldDelete_StatusWritesDoNotRetrigger(t *testing.T) {
	ctx := context.Background()
	prov := &deleteStubProvider{}
	resolver := &switchableResolver{provider: prov, err: errRuntimeNotReady}
	vm := deletionVM("vm-held")
	r := newTestReconciler(coverageTestScheme(t), resolver, vm, deletionProviderCR())
	marked := markForDeletion(t, r, vm)

	// reconcileOnUpdate runs handleDeletion when the update filter passes the
	// update, as the controller's event filter does.
	reconcileOnUpdate := func(oldVM, newVM *infravirtrigaudiov1beta1.VirtualMachine) bool {
		if !vmUpdateNeedsReconcile(oldVM, newVM) {
			return false
		}
		_, err := r.handleDeletion(ctx, newVM.DeepCopy())
		require.NoError(t, err)
		return true
	}

	_, err := r.handleDeletion(ctx, marked.DeepCopy())
	require.NoError(t, err)
	assert.EqualValues(t, 1, resolver.calls.Load())
	held := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), held))
	require.NotEqual(t, marked.ResourceVersion, held.ResourceVersion, "the hold wrote the VM's status")

	assert.False(t, reconcileOnUpdate(marked, held), "the hold's own status write does not re-run the delete")

	labelled := held.DeepCopy()
	labelled.Labels = map[string]string{"team": "a"}
	require.NoError(t, r.Update(ctx, labelled))
	assert.False(t, reconcileOnUpdate(held, labelled), "a label edit does not re-run the delete")
	assert.EqualValues(t, 1, resolver.calls.Load(), "no provider was resolved again")
	assert.Zero(t, prov.calls.Load(), "and no Delete sent")

	forced := labelled.DeepCopy()
	forced.Annotations = map[string]string{forceDeleteAnnotation: "true"}
	require.NoError(t, r.Update(ctx, forced))
	assert.True(t, reconcileOnUpdate(labelled, forced), "force-delete is acted on at once")
	assert.Zero(t, prov.calls.Load(), "force-delete releases without a provider Delete")
	assert.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(vm), &infravirtrigaudiov1beta1.VirtualMachine{})))
}
