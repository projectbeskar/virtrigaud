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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// force-delete of a VMSnapshot or a VMMigration whose VM is a consumer of a
// clustered Provider in another namespace is honoured only when the Provider
// allows consumers to leave state behind (as for a VirtualMachine, #381).

// sharedClusteredProvider is a clustered Provider in orphanInfraNS shared
// with every namespace.
func sharedClusteredProvider() *infravirtrigaudiov1beta1.Provider {
	p := withRuntime(clusteredProviderCR("shared", orphanInfraNS))
	p.Spec.ConsumerNamespaceSelector = sharedWithAll()
	return p
}

// consumerVM is the bound VM "web" in orphanTenantNS on sharedClusteredProvider.
func consumerVM() *infravirtrigaudiov1beta1.VirtualMachine {
	vm := clusterVM("web", orphanTenantNS, "shared")
	vm.UID = "uid-web"
	vm.Spec.ProviderRef.Namespace = orphanInfraNS
	vm.Status.ID = "web"
	vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: orphanInfraNS, Name: "shared"}
	vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-alpha", Pool: "pool-a"}
	return vm
}

// allowConsumerOrphan sets the Provider's allow-consumer-orphan-on-delete.
func allowConsumerOrphan(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	p := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: orphanInfraNS, Name: "shared"}, p))
	p.Annotations = map[string]string{infravirtrigaudiov1beta1.ProviderAllowConsumerOrphanOnDeleteAnnotation: "true"}
	require.NoError(t, c.Update(ctx, p))
}

func countPrefixed(events []string, prefix string) int {
	n := 0
	for _, e := range events {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

func TestVMSnapshot_ForceDelete_ClusteredConsumer_IsRefused(t *testing.T) {
	ctx := context.Background()
	snap := pendingSnapshot()
	snap.Namespace = orphanTenantNS
	snap.Annotations = map[string]string{forceDeleteAnnotation: "true"}
	snap.Status.SnapshotID = "snap-1"
	snap.Status.Phase = infravirtrigaudiov1beta1.SnapshotPhaseReady
	r, rec := newSnapPhaseReconciler(t, consumerVM(), sharedClusteredProvider(), snap,
		labeledNamespace(orphanInfraNS, nil), labeledNamespace(orphanTenantNS, nil))
	spy := &snapshotDeleteSpy{}
	spy.answer(contracts.NewRetryableError("snapshotDelete: host unreachable", nil))
	r.providerInstanceFn = func(context.Context, *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) { return spy, nil }
	require.NoError(t, r.Delete(ctx, snap))
	get := func() *infravirtrigaudiov1beta1.VMSnapshot {
		cur := &infravirtrigaudiov1beta1.VMSnapshot{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(snap), cur); err != nil {
			require.True(t, apierrors.IsNotFound(err), "%v", err)
			return nil
		}
		return cur
	}

	_, err := r.handleDeletion(ctx, get())
	require.NoError(t, err)
	held := get()
	require.NotNil(t, held, "force-delete must not release a consumer's snapshot of a clustered Provider")
	assert.EqualValues(t, 1, spy.calls.Load(), "the delete is still sent, as without force-delete")
	refused := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionForceDeleteRefused)
	require.NotNil(t, refused)
	assert.Equal(t, metav1.ConditionTrue, refused.Status)
	assert.Equal(t, k8s.ReasonForceDeleteNotAllowed, refused.Reason)
	assert.Equal(t, snapshotForceDeleteRefusedMessage(orphanTenantNS, types.NamespacedName{Namespace: orphanInfraNS, Name: "shared"}),
		refused.Message)
	assert.Equal(t, held.Generation, refused.ObservedGeneration)
	assert.Equal(t, 1, countPrefixed(eventsOf(rec.Events), "Warning "+k8s.ReasonForceDeleteNotAllowed))

	// A repeated refusal records no further event.
	_, err = r.handleDeletion(ctx, get())
	require.NoError(t, err)
	require.NotNil(t, get())
	assert.Zero(t, countPrefixed(eventsOf(rec.Events), "Warning "+k8s.ReasonForceDeleteNotAllowed))

	// The Provider allows it: the force-delete releases the snapshot.
	allowConsumerOrphan(t, r.Client)
	_, err = r.handleDeletion(ctx, get())
	require.NoError(t, err)
	assert.Nil(t, get(), "honoured once the Provider allows consumers to leave state behind")
}

func TestVMMigration_ForceDelete_ClusteredConsumer_IsRefused(t *testing.T) {
	ctx := context.Background()
	migration := &infravirtrigaudiov1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: orphanTenantNS, Generation: 1,
			Annotations: map[string]string{forceDeleteAnnotation: "true"}},
		Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
			Source: infravirtrigaudiov1beta1.MigrationSource{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "web"}},
			Target: infravirtrigaudiov1beta1.MigrationTarget{Name: "web-2", ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "target"}},
		},
	}
	migration.Status.SnapshotID = "snap-src"
	spy := &snapshotDeleteSpy{}
	spy.answer(contracts.NewRetryableError("snapshotDelete: host unreachable", nil))
	r, c := directionReconciler(t, spy, consumerVM(), sharedClusteredProvider(), migration,
		labeledNamespace(orphanInfraNS, nil), labeledNamespace(orphanTenantNS, nil))

	hold, _, err := r.releaseSourceSnapshotForDeletion(ctx, migration)
	require.NoError(t, err)
	assert.True(t, hold, "force-delete must not release a consumer's migration while its source snapshot may remain")
	assert.EqualValues(t, 1, spy.calls.Load())
	got := getXNSMigration(t, r, migration)
	refused := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionForceDeleteRefused)
	require.NotNil(t, refused)
	assert.Equal(t, k8s.ReasonForceDeleteNotAllowed, refused.Reason)
	assert.Equal(t, migrationForceDeleteRefusedMessage(orphanTenantNS, types.NamespacedName{Namespace: orphanInfraNS, Name: "shared"}),
		refused.Message)

	// The Provider allows it: the force-delete is honoured, the condition is gone.
	allowConsumerOrphan(t, c)
	hold, _, err = r.releaseSourceSnapshotForDeletion(ctx, getXNSMigration(t, r, migration))
	require.NoError(t, err)
	assert.False(t, hold)
	assert.Nil(t, meta.FindStatusCondition(getXNSMigration(t, r, migration).Status.Conditions, k8s.ConditionForceDeleteRefused))
}

func TestConsumerDetachDeniedFor(t *testing.T) {
	ctx := context.Background()
	single := withRuntime(singleProviderCR("single", orphanInfraNS))
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{}, sharedClusteredProvider(), single)
	clustered := types.NamespacedName{Namespace: orphanInfraNS, Name: "shared"}

	denied, err := consumerDetachDeniedFor(ctx, r.Client, clustered, orphanTenantNS)
	require.NoError(t, err)
	assert.True(t, denied, "a consumer of a clustered Provider")
	denied, err = consumerDetachDeniedFor(ctx, r.Client, clustered, orphanInfraNS)
	require.NoError(t, err)
	assert.False(t, denied, "the Provider's own namespace")
	denied, err = consumerDetachDeniedFor(ctx, r.Client, types.NamespacedName{Namespace: orphanInfraNS, Name: "single"}, orphanTenantNS)
	require.NoError(t, err)
	assert.False(t, denied, "a single-host Provider")
	denied, err = consumerDetachDeniedFor(ctx, r.Client, types.NamespacedName{Namespace: orphanInfraNS, Name: "gone"}, orphanTenantNS)
	require.NoError(t, err)
	assert.False(t, denied, "a Provider that does not exist")
	allowConsumerOrphan(t, r.Client)
	denied, err = consumerDetachDeniedFor(ctx, r.Client, clustered, orphanTenantNS)
	require.NoError(t, err)
	assert.False(t, denied, "a Provider that allows it")
}

var _ = Describe("Cross-namespace force-delete of a VMSnapshot (envtest)", func() {
	It("keeps the finalizer and records ForceDeleteRefused once, then honours it when the Provider allows it", func() {
		infra := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "fd-infra-"}}
		Expect(k8sClient.Create(ctx, infra)).To(Succeed())
		tenant := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "fd-tenant-"}}
		Expect(k8sClient.Create(ctx, tenant)).To(Succeed())

		prov := envtestProvider(infra.Name, "shared", &metav1.LabelSelector{})
		prov.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		prov.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
		Expect(k8sClient.Status().Update(ctx, prov)).To(Succeed())

		vm := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: tenant.Name},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef:  infravirtrigaudiov1beta1.ObjectRef{Name: "shared", Namespace: infra.Name},
				ClassRef:     infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
				ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "disk-1", Format: "qcow2", Source: "manual"},
			},
		}
		Expect(k8sClient.Create(ctx, vm)).To(Succeed())
		vm.Status.ID = "web"
		vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: infra.Name, Name: "shared"}
		vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-alpha", Pool: "pool-a"}
		Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())

		snap := &infravirtrigaudiov1beta1.VMSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: tenant.Name,
				Finalizers:  []string{vmSnapshotFinalizerName},
				Annotations: map[string]string{forceDeleteAnnotation: "true"}},
			Spec: infrav1beta1VMSnapshotSpec("web"),
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		snap.Status.SnapshotID = "snap-1"
		snap.Status.Phase = infravirtrigaudiov1beta1.SnapshotPhaseReady
		Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())
		DeferCleanup(func() {
			for _, obj := range []client.Object{snap, vm} {
				Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
					if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
						return client.IgnoreNotFound(err)
					}
					obj.SetFinalizers(nil)
					return k8sClient.Update(ctx, obj)
				})).To(Succeed())
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
		})
		Expect(k8sClient.Delete(ctx, snap)).To(Succeed())

		spy := &snapshotDeleteSpy{}
		spy.answer(contracts.NewRetryableError("snapshotDelete: host unreachable", nil))
		rec := record.NewFakeRecorder(100)
		r := &VMSnapshotReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: rec,
			providerInstanceFn: func(context.Context, *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) { return spy, nil }}
		get := func() *infravirtrigaudiov1beta1.VMSnapshot {
			cur := &infravirtrigaudiov1beta1.VMSnapshot{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(snap), cur); err != nil {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
				return nil
			}
			return cur
		}

		_, err := r.handleDeletion(ctx, get())
		Expect(err).NotTo(HaveOccurred())
		held := get()
		Expect(held).NotTo(BeNil())
		refused := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionForceDeleteRefused)
		Expect(refused).NotTo(BeNil())
		Expect(refused.Reason).To(Equal(k8s.ReasonForceDeleteNotAllowed))
		Expect(refused.ObservedGeneration).To(Equal(held.Generation))
		Expect(countPrefixed(eventsOf(rec.Events), "Warning "+k8s.ReasonForceDeleteNotAllowed)).To(Equal(1))

		_, err = r.handleDeletion(ctx, get())
		Expect(err).NotTo(HaveOccurred())
		Expect(get()).NotTo(BeNil())
		Expect(countPrefixed(eventsOf(rec.Events), "Warning "+k8s.ReasonForceDeleteNotAllowed)).To(BeZero())

		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			p := &infravirtrigaudiov1beta1.Provider{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(prov), p); err != nil {
				return err
			}
			p.Annotations = map[string]string{infravirtrigaudiov1beta1.ProviderAllowConsumerOrphanOnDeleteAnnotation: "true"}
			return k8sClient.Update(ctx, p)
		})).To(Succeed())
		_, err = r.handleDeletion(ctx, get())
		Expect(err).NotTo(HaveOccurred())
		Expect(get()).To(BeNil(), "honoured once the Provider allows it")
	})
})

// infrav1beta1VMSnapshotSpec is a VMSnapshot spec of the VM vmName.
func infrav1beta1VMSnapshotSpec(vmName string) infravirtrigaudiov1beta1.VMSnapshotSpec {
	return infravirtrigaudiov1beta1.VMSnapshotSpec{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: vmName}}
}
