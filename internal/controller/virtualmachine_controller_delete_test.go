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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// deleteStubProvider embeds stubProvider and makes Delete return a configurable
// error, recording how many times it was called.
type deleteStubProvider struct {
	stubProvider
	err   error
	calls atomic.Int32
}

func (p *deleteStubProvider) Delete(_ context.Context, _ string) (string, error) {
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
