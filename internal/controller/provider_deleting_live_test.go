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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// The informer cache can show a Provider that is not being deleted after its
// deletion began (ADR-0010). Right before a Create, Clone or ImportDisk RPC
// the Provider is re-read live, and nothing is sent when it is being deleted
// or gone.

// liveProviderReader is an uncached reader that shows the Provider named name
// as being deleted (or, with gone, as not existing); every other read goes to
// Reader unchanged.
type liveProviderReader struct {
	client.Reader
	name string
	gone bool
}

func (l *liveProviderReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	p, isProvider := obj.(*infravirtrigaudiov1beta1.Provider)
	if !isProvider || key.Name != l.name {
		return l.Reader.Get(ctx, key, obj, opts...)
	}
	if l.gone {
		return apierrors.NewNotFound(schema.GroupResource{Group: "infra.virtrigaud.io", Resource: "providers"}, key.Name)
	}
	if err := l.Reader.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	markDeleting(p)
	return nil
}

func TestProviderDeletingLive(t *testing.T) {
	ctx := context.Background()
	prov, _ := providerAndClass("default")
	prov.UID = "uid-prov"
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{}, prov)

	deleting, err := providerDeletingLive(ctx, r.Client, prov)
	require.NoError(t, err)
	assert.False(t, deleting)

	deleting, err = providerDeletingLive(ctx, &liveProviderReader{Reader: r.Client, name: prov.Name}, prov)
	require.NoError(t, err)
	assert.True(t, deleting, "a deletionTimestamp")

	deleting, err = providerDeletingLive(ctx, &liveProviderReader{Reader: r.Client, name: prov.Name, gone: true}, prov)
	require.NoError(t, err)
	assert.True(t, deleting, "gone")

	replaced := prov.DeepCopy()
	replaced.UID = "uid-earlier-object"
	deleting, err = providerDeletingLive(ctx, r.Client, replaced)
	require.NoError(t, err)
	assert.True(t, deleting, "the object resolved earlier was replaced")
}

func TestReconcileVM_SingleHost_NoCreateWhenProviderDeletingLive(t *testing.T) {
	for _, gone := range []bool{false, true} {
		ctx := context.Background()
		prov, class := providerAndClass("default")
		vm := creatableVM("default")
		rp := &routingProvider{}
		r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: rp}, prov, class, vm)
		r.APIReader = &liveProviderReader{Reader: r.Client, name: prov.Name, gone: gone}
		rec := record.NewFakeRecorder(16)
		r.Recorder = rec

		stored, _ := storedVM(t, r)
		res, err := r.reconcileVM(ctx, stored)
		require.NoError(t, err)
		assert.Equal(t, providerDeletingRetryInterval, res.RequeueAfter)
		assert.Empty(t, rp.createReqs, "no Create through a Provider the API server shows deleting (gone=%v)", gone)

		held, _ := storedVM(t, r)
		assert.Empty(t, held.Status.ID)
		assert.False(t, createRequested(held),
			"the marker written for this attempt is replaced: no Create was sent, so it never reads as an unknown outcome")
		for _, condType := range []string{k8s.ConditionReady, k8s.ConditionProvisioning} {
			c := meta.FindStatusCondition(held.Status.Conditions, condType)
			require.NotNil(t, c, condType)
			assert.Equal(t, k8s.ReasonProviderDeleting, c.Reason, condType)
			assert.Equal(t, providerDeletingMessage, c.Message, condType)
			assert.Equal(t, held.Generation, c.ObservedGeneration, condType)
		}
		assert.Equal(t, 1, providerDeletingEvents(rec))
	}
}

// Only a marker that stands for the unsent attempt alone is dropped; a
// re-send keeps the earlier attempt's marker (its outcome is unknown).
func TestMarkerDroppableUnsent(t *testing.T) {
	assert.True(t, markerDroppableUnsent(createAttempt{}), "a fresh create")
	assert.True(t, markerDroppableUnsent(createAttempt{priorID: "vm-1"}), "a re-create of a vanished VM")
	assert.True(t, markerDroppableUnsent(createAttempt{markerID: "2026-10-12T10:00:00Z", confirmed: true}),
		"an operator confirmed the earlier attempt made nothing")
	assert.False(t, markerDroppableUnsent(createAttempt{markerID: "2026-10-12T10:00:00Z", resend: true}), "a re-send")
	assert.False(t, markerDroppableUnsent(createAttempt{markerID: "2026-10-12T10:00:00Z"}),
		"any other attempt over an earlier marker")
}

func TestVMClone_NoCloneWhenProviderDeletingLive(t *testing.T) {
	clone := xnsClone("")
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
	r, rec := newXNSCloneReconciler(t, cp, clone)
	r.APIReader = &liveProviderReader{Reader: r.Client, name: "prov-1"}

	reconcileClone(t, r, clone, 4)

	assert.Zero(t, cp.cloneCnt, "no Clone RPC through a Provider the API server shows deleting")
	got := getClone(t, r, clone)
	assert.Equal(t, infravirtrigaudiov1beta1.ClonePhasePending, got.Status.Phase)
	ready := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMCloneConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, k8s.ReasonProviderDeleting, ready.Reason)
	assert.Equal(t, cloneProviderDeletingMessage, ready.Message)
	assert.Equal(t, got.Generation, ready.ObservedGeneration)
	assert.Equal(t, 1, providerDeletingEvents(rec), "one event, not one per reconcile")

	// Once the API server shows the Provider usable, the clone runs.
	r.APIReader = r.Client
	reconcileClone(t, r, clone, 4)
	assert.Equal(t, 1, cp.cloneCnt)
}

func TestVMMigration_NoImportWhenTargetProviderDeletingLive(t *testing.T) {
	ctx := context.Background()
	prov := &capturingMigrationProvider{importResp: contracts.ImportDiskResponse{DiskId: "d", Path: "/var/lib/libvirt/images/default.target-vm-migrated.qcow2"}}
	sourceVM, sourceProvider, targetProvider, migration := directionFixture(
		infravirtrigaudiov1beta1.ProviderTypeVSphere, infravirtrigaudiov1beta1.ProviderTypeLibvirt)
	r, _ := directionReconciler(t, prov, sourceVM, sourceProvider, targetProvider, migration, s3CredsSecret())
	r.APIReader = &liveProviderReader{Reader: r.Client, name: targetProvider.Name}

	res, err := r.handleImportingPhase(ctx, migration)
	require.NoError(t, err)
	assert.Equal(t, providerDeletingRetryInterval, res.RequeueAfter)
	assert.Zero(t, prov.importCalls, "no ImportDisk through a Provider the API server shows deleting")
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseImporting, got.Status.Phase, "held, not failed")
	ready := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, k8s.ReasonProviderDeleting, ready.Reason)
	assert.Equal(t, migrationProviderDeletingMessage, ready.Message)
	assert.False(t, r.longOpAlreadyStarted(got, longOpImport), "the hold does not claim the import guard")

	// A repeated hold writes nothing.
	rv := got.ResourceVersion
	_, err = r.handleImportingPhase(ctx, got.DeepCopy())
	require.NoError(t, err)
	assert.Equal(t, rv, getXNSMigration(t, r, migration).ResourceVersion)

	// Usable again: the import runs, and the hold is gone.
	r.APIReader = r.Client
	_, err = r.handleImportingPhase(ctx, getXNSMigration(t, r, migration))
	require.NoError(t, err)
	assert.Equal(t, 1, prov.importCalls)
	ready = meta.FindStatusCondition(getXNSMigration(t, r, migration).Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionReady)
	assert.True(t, ready == nil || ready.Reason != k8s.ReasonProviderDeleting)
}

// staleProviderClient is a client whose Provider reads show no deletion: the
// informer cache before it observed the Provider's deletionTimestamp.
type staleProviderClient struct {
	client.Client
}

func (s *staleProviderClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := s.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if p, ok := obj.(*infravirtrigaudiov1beta1.Provider); ok {
		p.DeletionTimestamp = nil
	}
	return nil
}

var _ = Describe("Live Provider check before Create (envtest)", func() {
	It("sends no Create when the cache shows the Provider usable but the API server shows it deleting", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "prov-deleting-live-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		prov := holdTestProvider(ns.Name)
		prov.Finalizers = []string{"example.com/test-hold"}
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		prov.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
		Expect(k8sClient.Status().Update(ctx, prov)).To(Succeed())
		Expect(k8sClient.Create(ctx, envtestClass(ns.Name, "small", nil))).To(Succeed())
		vm := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef:  infravirtrigaudiov1beta1.ObjectRef{Name: prov.Name},
				ClassRef:     infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
				ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "disk-1", Format: "qcow2", Source: "manual"},
			},
		}
		Expect(k8sClient.Create(ctx, vm)).To(Succeed())
		DeferCleanup(func() {
			for _, obj := range []client.Object{vm, prov} {
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
		// The Provider's deletion begins; the reconciler's (cache) client does
		// not show it yet, the API server does.
		Expect(k8sClient.Delete(ctx, prov)).To(Succeed())

		rp := &routingProvider{}
		r := &VirtualMachineReconciler{Client: &staleProviderClient{Client: k8sClient}, Scheme: k8sClient.Scheme(),
			APIReader: k8sClient, RemoteResolver: &stubResolver{provider: rp}, Recorder: record.NewFakeRecorder(100)}
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}
		_, err := r.Reconcile(ctx, req) // finalizer
		Expect(err).NotTo(HaveOccurred())
		res, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(providerDeletingRetryInterval))
		Expect(rp.createReqs).To(BeEmpty(), "no Create through a Provider being deleted")

		got := &infravirtrigaudiov1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, got)).To(Succeed())
		Expect(got.Status.ID).To(BeEmpty())
		ready := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal(k8s.ReasonProviderDeleting))
		Expect(ready.ObservedGeneration).To(Equal(got.Generation))
	})
})
