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
	stderrors "errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// force-delete on a clustered Provider, like orphan-on-delete (review M4):
// only a VM in the Provider's own namespace, or a consumer of a Provider that
// allows it, may be released without a confirmed provider delete — a released
// clustered VM's domain keeps running on the shared hosts outside the
// committed-capacity accounting. A refused force-delete is ignored: the
// delete is still retried, and goes through once the provider can delete.

// forceFixture seeds provider (shared with every namespace) and a bound VM in
// vmNS that references it and carries force-delete, marked for deletion; it
// returns the reconciler, the routing provider stub, the resolver and the
// event recorder.
func forceFixture(t *testing.T, provider *infravirtrigaudiov1beta1.Provider, vmNS string) (
	*VirtualMachineReconciler, *routingProvider, *switchableResolver, *record.FakeRecorder,
) {
	t.Helper()
	provider.Spec.ConsumerNamespaceSelector = sharedWithAll()
	vm := clusterVM("leaving", vmNS, provider.Name)
	vm.UID = "uid-leaving"
	vm.Spec.ProviderRef.Namespace = provider.Namespace
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	vm.Annotations = map[string]string{forceDeleteAnnotation: "true"}
	vm.Status.ID = "leaving"
	vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: provider.Namespace, Name: provider.Name}
	if isClusterTopology(provider) {
		vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-alpha", Pool: "pool-a"}
	}
	prov := &routingProvider{}
	resolver := &switchableResolver{provider: prov, err: errValidateFailed}
	r := newTestReconciler(coverageTestScheme(t), resolver, provider, vm,
		labeledNamespace(orphanInfraNS, nil), labeledNamespace(orphanTenantNS, nil))
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec
	markForDeletion(t, r, vm)
	return r, prov, resolver, rec
}

// getLeaving reads the fixture's VM; ok is false once it is gone.
func getLeaving(t *testing.T, r *VirtualMachineReconciler, ns string) (*infravirtrigaudiov1beta1.VirtualMachine, bool) {
	t.Helper()
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "leaving"}, vm)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	require.NoError(t, err)
	return vm, true
}

// forceRefusedEvents counts the Warning ForceDeleteNotAllowed events in rec.
func forceRefusedEvents(rec *record.FakeRecorder) int {
	n := 0
	for _, e := range drainEvents(rec) {
		if strings.HasPrefix(e, "Warning "+k8s.ReasonForceDeleteNotAllowed) {
			n++
		}
	}
	return n
}

func TestForceDelete_ClusteredConsumer_ProviderUnavailable_IsRefused(t *testing.T) {
	ctx := context.Background()
	r, prov, resolver, rec := forceFixture(t, withRuntime(clusteredProviderCR("shared", orphanInfraNS)), orphanTenantNS)

	vm, _ := getLeaving(t, r, orphanTenantNS)
	res, err := r.handleDeletion(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, blockedRetryMin, res.RequeueAfter, "the delete is held on the blocked-VM backoff, as without force-delete")
	assert.Empty(t, prov.deleteRefs)

	held, exists := getLeaving(t, r, orphanTenantNS)
	require.True(t, exists, "force-delete must not release a consumer VM of a clustered Provider")
	refused := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionForceDeleteRefused)
	require.NotNil(t, refused)
	assert.Equal(t, metav1.ConditionTrue, refused.Status)
	assert.Equal(t, k8s.ReasonForceDeleteNotAllowed, refused.Reason)
	assert.Equal(t, held.Generation, refused.ObservedGeneration)
	assert.Equal(t, forceDeleteRefusedMessage(orphanTenantNS, types.NamespacedName{Namespace: orphanInfraNS, Name: "shared"}), refused.Message)
	assert.Contains(t, refused.Message, infravirtrigaudiov1beta1.ProviderAllowConsumerOrphanOnDeleteAnnotation)
	assert.NotContains(t, refused.Message, "host-alpha", "no host is named")
	blocked := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionDeleteBlocked)
	require.NotNil(t, blocked, "the hold says why the delete waits")
	assert.Equal(t, k8s.ReasonProviderUnavailable, blocked.Reason)
	assert.Equal(t, 1, forceRefusedEvents(rec))

	// A repeated refusal writes nothing and records no event.
	res, err = r.handleDeletion(ctx, held.DeepCopy())
	require.NoError(t, err)
	assert.Equal(t, blockedRetryMin, res.RequeueAfter)
	again, _ := getLeaving(t, r, orphanTenantNS)
	assert.Equal(t, held.ResourceVersion, again.ResourceVersion, "a repeated refusal does not write the VM")
	assert.True(t, equality.Semantic.DeepEqual(held.Status, again.Status))
	assert.Zero(t, forceRefusedEvents(rec))

	// The provider answers again: the owner-checked delete goes through.
	resolver.err = nil
	_, err = r.handleDeletion(ctx, again.DeepCopy())
	require.NoError(t, err)
	require.Len(t, prov.deleteRefs, 1)
	assert.Equal(t, "host-alpha", prov.deleteRefs[0].HostID)
	_, exists = getLeaving(t, r, orphanTenantNS)
	assert.False(t, exists, "released once the provider deleted the VM")
}

func TestForceDelete_ClusteredConsumer_DeleteFailure_IsRefused(t *testing.T) {
	ctx := context.Background()
	r, prov, resolver, _ := forceFixture(t, withRuntime(clusteredProviderCR("shared", orphanInfraNS)), orphanTenantNS)
	resolver.err = nil
	prov.deleteErr = stderrors.New("domain is busy")

	vm, _ := getLeaving(t, r, orphanTenantNS)
	res, err := r.handleDeletion(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, vmDeleteRetryInterval, res.RequeueAfter, "retried like an ordinary failed delete")
	assert.Len(t, prov.deleteRefs, 1)
	held, exists := getLeaving(t, r, orphanTenantNS)
	require.True(t, exists, "a failed delete does not release a consumer VM even with force-delete")
	assert.True(t, meta.IsStatusConditionTrue(held.Status.Conditions, k8s.ConditionForceDeleteRefused))

	// The annotation removed: the refusal is withdrawn.
	held.Annotations = nil
	require.NoError(t, r.Update(ctx, held))
	_, err = r.handleDeletion(ctx, held.DeepCopy())
	require.NoError(t, err)
	cleared, _ := getLeaving(t, r, orphanTenantNS)
	assert.Nil(t, meta.FindStatusCondition(cleared.Status.Conditions, k8s.ConditionForceDeleteRefused))
}

func TestForceDelete_AllowedCases(t *testing.T) {
	allowing := clusteredProviderCR("shared", orphanInfraNS)
	allowing.Annotations = map[string]string{infravirtrigaudiov1beta1.ProviderAllowConsumerOrphanOnDeleteAnnotation: "true"}

	cases := []struct {
		name     string
		provider *infravirtrigaudiov1beta1.Provider
		vmNS     string
	}{
		{name: "VM in the clustered Provider's own namespace", provider: clusteredProviderCR("shared", orphanInfraNS), vmNS: orphanInfraNS},
		{name: "consumer of a clustered Provider that allows it", provider: allowing, vmNS: orphanTenantNS},
		{name: "consumer of a single-host Provider (unchanged)", provider: singleProviderCR("shared", orphanInfraNS), vmNS: orphanTenantNS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, prov, _, rec := forceFixture(t, withRuntime(tc.provider), tc.vmNS)
			vm, _ := getLeaving(t, r, tc.vmNS)
			_, err := r.handleDeletion(context.Background(), vm)
			require.NoError(t, err)
			_, exists := getLeaving(t, r, tc.vmNS)
			assert.False(t, exists, "force-delete releases the VM")
			assert.Empty(t, prov.deleteRefs, "without a provider call: the Provider cannot be reached")
			assert.Zero(t, forceRefusedEvents(rec))
		})
	}
}
