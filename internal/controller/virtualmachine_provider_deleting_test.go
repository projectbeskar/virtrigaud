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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A VirtualMachine is never created (or re-created) on the hypervisor through
// a Provider that is being deleted: the new VM would keep the Provider's
// deletion waiting indefinitely. A VM that is already created is managed as
// usual.

// markDeleting gives provider a deletionTimestamp (with a finalizer, so the
// fake client accepts it).
func markDeleting(provider *infravirtrigaudiov1beta1.Provider) *infravirtrigaudiov1beta1.Provider {
	now := metav1.Now()
	provider.DeletionTimestamp = &now
	provider.Finalizers = append(provider.Finalizers, "infra.virtrigaud.io/in-use")
	return provider
}

// providerDeletingEvents counts the Warning ProviderDeleting events in rec.
func providerDeletingEvents(rec *record.FakeRecorder) int {
	n := 0
	for _, e := range drainEvents(rec) {
		if strings.HasPrefix(e, "Warning "+k8s.ReasonProviderDeleting) {
			n++
		}
	}
	return n
}

func TestReconcileVM_SingleHost_NoCreateOnDeletingProvider(t *testing.T) {
	ctx := context.Background()
	prov, class := providerAndClass("default")
	vm := creatableVM("default")
	rp := &routingProvider{}
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: rp}, markDeleting(prov), class, vm)
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec

	stored, _ := storedVM(t, r)
	res, err := r.reconcileVM(ctx, stored)
	require.NoError(t, err)
	assert.Equal(t, providerDeletingRetryInterval, res.RequeueAfter)
	assert.Empty(t, rp.createReqs, "no VM is created through a Provider that is being deleted")

	held, _ := storedVM(t, r)
	assert.Empty(t, held.Status.ID)
	assert.Nil(t, held.Status.BoundProvider, "nor is the VM bound to it")
	for _, condType := range []string{k8s.ConditionReady, k8s.ConditionProvisioning} {
		c := meta.FindStatusCondition(held.Status.Conditions, condType)
		require.NotNil(t, c, condType)
		assert.Equal(t, metav1.ConditionFalse, c.Status, condType)
		assert.Equal(t, k8s.ReasonProviderDeleting, c.Reason, condType)
		assert.Equal(t, providerDeletingMessage, c.Message, condType)
		assert.Equal(t, held.Generation, c.ObservedGeneration, condType)
	}
	assert.Equal(t, 1, providerDeletingEvents(rec))

	// A repeated hold writes nothing and records no event.
	_, err = r.reconcileVM(ctx, held.DeepCopy())
	require.NoError(t, err)
	again, _ := storedVM(t, r)
	assert.Equal(t, held.ResourceVersion, again.ResourceVersion, "a repeated hold does not write the VM")
	assert.True(t, equality.Semantic.DeepEqual(held.Status, again.Status))
	assert.Zero(t, providerDeletingEvents(rec))
	assert.Empty(t, rp.createReqs)
}

func TestReconcileVM_Clustered_NoPlacementOnDeletingProvider(t *testing.T) {
	ctx := context.Background()
	rp := &routingProvider{}
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	r := clusteredFixture(t, rp, vm)
	providerCR := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: clusteredNS, Name: "prov-cluster"}, providerCR))
	providerCR.Finalizers = []string{"infra.virtrigaud.io/in-use"}
	require.NoError(t, r.Update(ctx, providerCR))
	require.NoError(t, r.Delete(ctx, providerCR))

	res, err := r.reconcileVM(ctx, getVM(t, r, "web"))
	require.NoError(t, err)
	assert.Equal(t, providerDeletingRetryInterval, res.RequeueAfter)
	assert.Empty(t, rp.createReqs)
	held := getVM(t, r, "web")
	assert.Nil(t, held.Status.Placement, "no host is chosen on a Provider that is being deleted")
	assert.Equal(t, k8s.ReasonProviderDeleting, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady).Reason)
}

func TestReconcileVM_CreatedVMIsManagedOnDeletingProvider(t *testing.T) {
	ctx := context.Background()
	prov, class := providerAndClass("default")
	vm := baseVM("default")
	vm.Status.ID = "vm-100"
	vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "default", Name: prov.Name}
	describe := &offGracefulProvider{describe: contracts.DescribeResponse{Exists: true, PowerState: "On", IPs: []string{"10.0.0.5"}}}
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: describe}, markDeleting(prov), class, vm)

	stored := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), stored))
	_, err := r.reconcileVM(ctx, stored)
	require.NoError(t, err)
	got := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), got))
	assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, k8s.ConditionReady), "a created VM keeps being managed")
}

func TestReconcileVM_NoRecreateOnDeletingProvider(t *testing.T) {
	ctx := context.Background()
	prov, class := providerAndClass("default")
	vm := creatableVM("default")
	vm.Status.ID = "vm-100"
	vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "default", Name: prov.Name}
	rp := &routingProvider{describeResp: contracts.DescribeResponse{Exists: false}}
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: rp}, markDeleting(prov), class, vm)

	stored, _ := storedVM(t, r)
	res, err := r.reconcileVM(ctx, stored)
	require.NoError(t, err)
	assert.Equal(t, providerDeletingRetryInterval, res.RequeueAfter)
	assert.Empty(t, rp.createReqs, "a vanished VM is not re-created through a Provider that is being deleted")
	held, _ := storedVM(t, r)
	assert.Equal(t, "vm-100", held.Status.ID, "its id is kept")
	assert.Equal(t, k8s.ReasonProviderDeleting, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady).Reason)
}

func TestReconcileVM_UnsavedCreateIsRecordedOnDeletingProvider(t *testing.T) {
	ctx := context.Background()
	r, rp, _ := createRecordFixture(t, "", nil)
	stored, _ := storedVM(t, r)
	r.unsavedCreates.Store(stored.UID, unsavedCreate{id: "vm-web",
		provider: infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "default", Name: "test-prov", UID: "uid-provider"}})
	providerCR := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-prov"}, providerCR))
	providerCR.Finalizers = []string{"infra.virtrigaud.io/in-use"}
	require.NoError(t, r.Update(ctx, providerCR))
	require.NoError(t, r.Delete(ctx, providerCR))

	_, err := r.reconcileVM(ctx, stored)
	require.NoError(t, err)
	assert.Empty(t, rp.createReqs)
	got, _ := storedVM(t, r)
	assert.Equal(t, "vm-web", got.Status.ID, "a VM the provider already created is recorded, so its delete can reach it")
}

// TestProviderDeletingMessage (review M2): the hold tells the owner what to
// do, and never that the VM waits for the deleting Provider.
func TestProviderDeletingMessage(t *testing.T) {
	assert.Contains(t, providerDeletingMessage, "Point spec.providerRef at another Provider, or delete this VirtualMachine")
	assert.NotContains(t, providerDeletingMessage, "waits")
}
