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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// Orphan-on-delete on a clustered Provider (review M4): only a VM in the
// Provider's own namespace, or a consumer of a Provider that allows it, may be
// detached — an orphaned clustered VM keeps running outside the capacity
// accounting.

const (
	orphanInfraNS  = "infra"
	orphanTenantNS = "tenant"
)

// orphanFixture seeds provider (in its own namespace) and a bound VM in vmNS
// that references it and carries orphan-on-delete; it returns the reconciler,
// the routing provider stub and the event recorder.
func orphanFixture(t *testing.T, provider *infravirtrigaudiov1beta1.Provider, vmNS string, bound bool) (*VirtualMachineReconciler, *routingProvider, *record.FakeRecorder) {
	t.Helper()
	vm := clusterVM("leaving", vmNS, provider.Name)
	vm.Spec.ProviderRef.Namespace = provider.Namespace
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	vm.Annotations = map[string]string{infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation: "true"}
	if bound {
		vm.Status.ID = "leaving"
		vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: provider.Namespace, Name: provider.Name}
		if isClusterTopology(provider) {
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-alpha", Pool: "pool-a"}
		}
	}
	prov := &routingProvider{}
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, provider, vm)
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	return r, prov, rec
}

// deleteOrphan marks the VM for deletion and runs the finalizer; it reports
// whether the object is gone.
func deleteOrphan(t *testing.T, r *VirtualMachineReconciler, ns string) bool {
	t.Helper()
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "leaving"}, vm))
	_, err := r.handleDeletion(context.Background(), markForDeletion(t, r, vm))
	require.NoError(t, err)
	err = r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "leaving"}, &infravirtrigaudiov1beta1.VirtualMachine{})
	return apierrors.IsNotFound(err)
}

func TestOrphanOnDelete_ClusteredConsumerIsRefused(t *testing.T) {
	r, prov, rec := orphanFixture(t, clusteredProviderCR("shared", orphanInfraNS), orphanTenantNS, true)
	assert.False(t, deleteOrphan(t, r, orphanTenantNS), "the finalizer is kept")
	assert.Empty(t, prov.deleteRefs, "and the VM is not deleted instead")

	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: orphanTenantNS, Name: "leaving"}, vm))
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, c)
	assert.Equal(t, k8s.ReasonOrphanOnDeleteNotAllowed, c.Reason)
	assert.Contains(t, c.Message, infravirtrigaudiov1beta1.ProviderAllowConsumerOrphanOnDeleteAnnotation)
	assert.Contains(t, c.Message, "remove the annotation to delete the VM normally")
	events := drainEvents(rec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "Warning "+k8s.ReasonOrphanOnDeleteNotAllowed)

	// A re-check of the same refusal records no second event.
	_, err := r.handleDeletion(context.Background(), vm)
	require.NoError(t, err)
	assert.Empty(t, drainEvents(rec))
}

func TestOrphanOnDelete_AllowedCases(t *testing.T) {
	allowing := clusteredProviderCR("shared", orphanInfraNS)
	allowing.Annotations = map[string]string{infravirtrigaudiov1beta1.ProviderAllowConsumerOrphanOnDeleteAnnotation: "true"}
	notTrue := clusteredProviderCR("shared", orphanInfraNS)
	notTrue.Annotations = map[string]string{infravirtrigaudiov1beta1.ProviderAllowConsumerOrphanOnDeleteAnnotation: "yes"}

	cases := []struct {
		name     string
		provider *infravirtrigaudiov1beta1.Provider
		vmNS     string
		bound    bool
		orphaned bool
	}{
		{name: "VM in the clustered Provider's own namespace", provider: clusteredProviderCR("shared", orphanInfraNS), vmNS: orphanInfraNS, bound: true, orphaned: true},
		{name: "consumer of a clustered Provider that allows it", provider: allowing, vmNS: orphanTenantNS, bound: true, orphaned: true},
		{name: "only the value \"true\" allows it", provider: notTrue, vmNS: orphanTenantNS, bound: true, orphaned: false},
		{name: "consumer of a single-host Provider (unchanged)", provider: singleProviderCR("shared", orphanInfraNS), vmNS: orphanTenantNS, bound: true, orphaned: true},
		{name: "an unbound clustered consumer has nothing to leave behind", provider: clusteredProviderCR("shared", orphanInfraNS), vmNS: orphanTenantNS, bound: false, orphaned: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, prov, _ := orphanFixture(t, tc.provider, tc.vmNS, tc.bound)
			assert.Equal(t, tc.orphaned, deleteOrphan(t, r, tc.vmNS))
			assert.Empty(t, prov.deleteRefs, "orphan-on-delete never deletes the hypervisor VM")
		})
	}
}
