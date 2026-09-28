/*
Copyright 2025.

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
	stderrors "errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// deleteStubProvider embeds stubProvider and makes Delete return a configurable
// error, recording how many times it was called.
type deleteStubProvider struct {
	stubProvider
	err   error
	calls atomic.Int32
}

func (p *deleteStubProvider) Delete(_ context.Context, _ contracts.VMRef) (string, error) {
	p.calls.Add(1)
	return "", p.err
}

// deletionVM builds a VM that already carries the finalizer and a provider VM ID,
// so handleDeletion attempts the provider-side delete.
func deletionVM(name string) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			Finalizers: []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer},
		},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "test-provider"},
		},
	}
	vm.Status.ID = "100"
	return vm
}

func deletionProviderCR() *infravirtrigaudiov1beta1.Provider {
	return &infravirtrigaudiov1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "test-provider", Namespace: "default"},
	}
}

// markForDeletion deletes the VM through the fake client (which sets a
// DeletionTimestamp because the finalizer is present) and returns the refreshed,
// deletion-marked object.
func markForDeletion(t *testing.T, r *VirtualMachineReconciler, vm *infravirtrigaudiov1beta1.VirtualMachine) *infravirtrigaudiov1beta1.VirtualMachine {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, r.Delete(ctx, vm))
	var marked infravirtrigaudiov1beta1.VirtualMachine
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), &marked))
	require.False(t, marked.DeletionTimestamp.IsZero(), "VM should be marked for deletion")
	return &marked
}

// TestHandleDeletion_RetainsFinalizerOnDeleteFailure is the core #261 P0-2 fix: a
// failed provider Delete must NOT drop the finalizer (which would orphan the
// hypervisor VM). It requeues and the VM stays present.
func TestHandleDeletion_RetainsFinalizerOnDeleteFailure(t *testing.T) {
	ctx := context.Background()
	s := coverageTestScheme(t)
	prov := &deleteStubProvider{err: stderrors.New("VM 100 is running - destroy failed")}
	vm := deletionVM("vm-keep")
	r := newTestReconciler(s, &stubResolver{provider: prov}, vm, deletionProviderCR())
	marked := markForDeletion(t, r, vm)

	res, err := r.handleDeletion(ctx, marked)
	require.NoError(t, err)
	assert.Greater(t, res.RequeueAfter, time.Duration(0), "must requeue to retry the delete")
	require.EqualValues(t, 1, prov.calls.Load(), "must have attempted the provider delete")

	var after infravirtrigaudiov1beta1.VirtualMachine
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), &after), "VM must still exist (finalizer retained)")
	assert.Contains(t, after.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer,
		"finalizer must be retained so the hypervisor VM is not orphaned")
}

// TestHandleDeletion_ProceedsWhenAlreadyGone verifies an already-absent VM
// (provider returns NotFound) is treated as success: the finalizer is removed.
func TestHandleDeletion_ProceedsWhenAlreadyGone(t *testing.T) {
	ctx := context.Background()
	s := coverageTestScheme(t)
	prov := &deleteStubProvider{err: contracts.NewNotFoundError("delete: VM not found", nil)}
	vm := deletionVM("vm-gone")
	r := newTestReconciler(s, &stubResolver{provider: prov}, vm, deletionProviderCR())
	marked := markForDeletion(t, r, vm)

	_, err := r.handleDeletion(ctx, marked)
	require.NoError(t, err)

	var after infravirtrigaudiov1beta1.VirtualMachine
	getErr := r.Get(ctx, client.ObjectKeyFromObject(vm), &after)
	assert.True(t, apierrors.IsNotFound(getErr), "finalizer removed → VM gone when the provider VM is already absent")
}

// TestHandleDeletion_ForceDeleteAnnotationRemovesFinalizer verifies the escape
// hatch: with the force-delete annotation, a persistently-failing Delete still
// removes the finalizer (operator accepts a possibly-orphaned provider VM).
func TestHandleDeletion_ForceDeleteAnnotationRemovesFinalizer(t *testing.T) {
	ctx := context.Background()
	s := coverageTestScheme(t)
	prov := &deleteStubProvider{err: stderrors.New("provider permanently unreachable")}
	vm := deletionVM("vm-force")
	vm.Annotations = map[string]string{forceDeleteAnnotation: "true"}
	r := newTestReconciler(s, &stubResolver{provider: prov}, vm, deletionProviderCR())
	marked := markForDeletion(t, r, vm)

	_, err := r.handleDeletion(ctx, marked)
	require.NoError(t, err)

	var after infravirtrigaudiov1beta1.VirtualMachine
	getErr := r.Get(ctx, client.ObjectKeyFromObject(vm), &after)
	assert.True(t, apierrors.IsNotFound(getErr), "force-delete annotation must remove the finalizer despite the failure")
}

// TestHandleDeletion_ConflictKeepsFinalizerWithCondition verifies a provider
// Delete refused because other VMs depend on this one (a Conflict — e.g. a
// libvirt linked clone backed by its disk) keeps the finalizer, sets
// Ready=False/DeleteBlocked with the provider's categorized message and the
// way out, and re-checks at the slower blocked cadence; force-delete still
// removes the finalizer (the provider changed nothing, so the VM is left on
// the hypervisor, detached).
func TestHandleDeletion_ConflictKeepsFinalizerWithCondition(t *testing.T) {
	ctx := context.Background()
	s := coverageTestScheme(t)
	refusal := contracts.NewConflictError(`delete: failed to delete VM: delete of libvirt domain "default.vm-src" refused: `+
		`its disk is the backing file (or a disk) of 1 other domain(s) on this host, such as a linked clone of this VM; `+
		`delete the linked clones first`, fmt.Errorf("%w: rpc error: code = FailedPrecondition", contracts.ErrVMDiskInUse))
	prov := &deleteStubProvider{err: refusal}
	vm := deletionVM("vm-src")
	r := newTestReconciler(s, &stubResolver{provider: prov}, vm, deletionProviderCR())
	marked := markForDeletion(t, r, vm)

	res, err := r.handleDeletion(ctx, marked)
	require.NoError(t, err)
	assert.Equal(t, vmDeleteBlockedRetryInterval, res.RequeueAfter)
	require.EqualValues(t, 1, prov.calls.Load())

	var after infravirtrigaudiov1beta1.VirtualMachine
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), &after), "VM must still exist (finalizer retained)")
	assert.Contains(t, after.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
	ready := meta.FindStatusCondition(after.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready, "the owner is told why the delete does not complete")
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, k8s.ReasonDeleteBlocked, ready.Reason)
	assert.Contains(t, ready.Message, "delete the linked clones first")
	assert.Contains(t, ready.Message, infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation)
	assert.NotContains(t, ready.Message, "rpc error", "the raw gRPC chain stays out of the condition")
	assert.Nil(t, meta.FindStatusCondition(after.Status.Conditions, k8s.ConditionDeleteBlocked),
		"a single-host VM never carries the clustered DeleteBlocked condition")

	// force-delete still wins.
	after.Annotations = map[string]string{forceDeleteAnnotation: "true"}
	require.NoError(t, r.Update(ctx, &after))
	_, err = r.handleDeletion(ctx, &after)
	require.NoError(t, err)
	var gone infravirtrigaudiov1beta1.VirtualMachine
	assert.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(vm), &gone)))
}

// TestHandleDeletion_OtherConflictIsAnOrdinaryFailure pins that only the
// VM_DISK_IN_USE refusal is a blocked delete: any other Conflict keeps the
// finalizer and retries at the ordinary cadence, with no DeleteBlocked
// condition claiming another VM depends on the disk.
func TestHandleDeletion_OtherConflictIsAnOrdinaryFailure(t *testing.T) {
	ctx := context.Background()
	s := coverageTestScheme(t)
	prov := &deleteStubProvider{err: contracts.NewConflictError("delete: something else conflicts", nil)}
	vm := deletionVM("vm-other")
	r := newTestReconciler(s, &stubResolver{provider: prov}, vm, deletionProviderCR())
	marked := markForDeletion(t, r, vm)

	res, err := r.handleDeletion(ctx, marked)
	require.NoError(t, err)
	assert.Equal(t, vmDeleteRetryInterval, res.RequeueAfter)
	var after infravirtrigaudiov1beta1.VirtualMachine
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), &after))
	assert.Contains(t, after.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
	if c := meta.FindStatusCondition(after.Status.Conditions, k8s.ConditionReady); c != nil {
		assert.NotEqual(t, k8s.ReasonDeleteBlocked, c.Reason)
	}
}
