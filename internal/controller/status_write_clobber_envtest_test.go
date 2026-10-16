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
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// staleReadClient serves, for the one object set with serveStale, the copy
// held there instead of the stored object — an informer cache that has not
// caught up with a write yet. Every other read and every write goes to the
// API server.
type staleReadClient struct {
	client.Client
	mu    sync.Mutex
	stale client.Object
}

// serveStale makes Get of obj's key answer obj (nil: read through again).
func (c *staleReadClient) serveStale(obj client.Object) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stale = obj
}

// Get answers the stale copy for its key, else reads the API server.
func (c *staleReadClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.mu.Lock()
	stale := c.stale
	c.mu.Unlock()
	if stale != nil && key == client.ObjectKeyFromObject(stale) {
		switch dst := obj.(type) {
		case *infravirtrigaudiov1beta1.VMMigration:
			if src, ok := stale.(*infravirtrigaudiov1beta1.VMMigration); ok {
				src.DeepCopyInto(dst)
				return nil
			}
		case *infravirtrigaudiov1beta1.Provider:
			if src, ok := stale.(*infravirtrigaudiov1beta1.Provider); ok {
				src.DeepCopyInto(dst)
				return nil
			}
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// pathImportProvider is countingMigrationProvider whose ImportDisk also
// returns the landed disk's provider-native path, as the vSphere provider does.
type pathImportProvider struct {
	countingMigrationProvider
}

// pathImportTargetPath is the landed-disk path pathImportProvider returns.
const pathImportTargetPath = "[datastore1] web-migrated/web-migrated.vmdk"

// ImportDisk records the call and answers a synchronous import with a path.
func (p *pathImportProvider) ImportDisk(ctx context.Context, req contracts.ImportDiskRequest) (contracts.ImportDiskResponse, error) {
	resp, err := p.countingMigrationProvider.ImportDisk(ctx, req)
	resp.Path = pathImportTargetPath
	return resp, err
}

var _ = Describe("Status writes never replace a newer status with a stale copy (envtest)", func() {
	newNamespace := func(prefix string) string {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		return ns.Name
	}

	// The rc1 lab run: libvirt -> S3 -> vSphere. Reconcile A imported the disk,
	// recorded status.diskInfo and moved to Creating; reconcile B, from a
	// cache that predated A's write, took the already-imported branch and its
	// write replaced the live status, dropping diskInfo — Creating then failed
	// with "import did not record disk info".
	It("keeps the imported disk recorded by one reconcile when a reconcile from a stale cache writes after it", func() {
		ns := newNamespace("clobber-mig-")
		Expect(k8sClient.Create(ctx, holdTestProvider(ns))).To(Succeed())
		holdTestVM(ns, "src-vm", infravirtrigaudiov1beta1.ObservedPowerStateOff)

		m := &infravirtrigaudiov1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: ns, Finalizers: []string{infravirtrigaudiov1beta1.VMMigrationFinalizer}},
			Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
				Source: infravirtrigaudiov1beta1.MigrationSource{VMRef: infrav1LocalRef("src-vm")},
				Target: infravirtrigaudiov1beta1.MigrationTarget{
					Name:        "web",
					ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-1"},
					ClassRef:    &infravirtrigaudiov1beta1.LocalObjectReference{Name: "small"},
				},
				Storage: &infravirtrigaudiov1beta1.MigrationStorage{
					Type: "pvc",
					PVC:  &infravirtrigaudiov1beta1.PVCStorageConfig{Name: "mig-pvc"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		m.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseImporting
		m.Status.StoragePVCName = "mig-pvc"
		m.Status.DiskInfo = &infravirtrigaudiov1beta1.MigrationDiskInfo{SourceChecksum: "checksum-1"}
		Expect(k8sClient.Status().Update(ctx, m)).To(Succeed())

		prov := &pathImportProvider{}
		c := &staleReadClient{Client: k8sClient}
		r := NewVMMigrationReconciler(c, k8sClient, k8sClient.Scheme(), nil, record.NewFakeRecorder(100), false)
		r.providerInstanceFn = onlyProviderIn(ns, prov)
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}

		// What B's cache will still hold: the migration before A's write.
		beforeA := &infravirtrigaudiov1beta1.VMMigration{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, beforeA)).To(Succeed())

		By("reconcile A imports the disk and records it")
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(prov.importCalls.Load()).To(BeEquivalentTo(1))
		afterA := &infravirtrigaudiov1beta1.VMMigration{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, afterA)).To(Succeed())
		Expect(afterA.Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseCreating))
		Expect(afterA.Status.DiskInfo).NotTo(BeNil())
		Expect(afterA.Status.DiskInfo.TargetPath).To(Equal(pathImportTargetPath))

		By("reconcile B, from a cache that predates A's write, takes the already-imported branch")
		c.serveStale(beforeA)
		res, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred(), "a lost status write is requeued, not an error")
		Expect(prov.importCalls.Load()).To(BeEquivalentTo(1), "the import is never sent twice")

		afterB := &infravirtrigaudiov1beta1.VMMigration{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, afterB)).To(Succeed())
		Expect(afterB.Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseCreating))
		Expect(afterB.Status.DiskInfo).NotTo(BeNil(), "B's stale write did not drop A's diskInfo")
		Expect(afterB.Status.DiskInfo.TargetPath).To(Equal(pathImportTargetPath))
		Expect(afterB.Status.DiskInfo.SourceChecksum).To(Equal("checksum-1"))
		Expect(afterB.Status.ImportID).To(Equal("disk-1"))
		Expect(afterB.ResourceVersion).To(Equal(afterA.ResourceVersion), "B wrote nothing")
		Expect(res.RequeueAfter).To(Equal(migrationStatusConflictRequeue), "B runs again, bounded, from the live state")

		By("the next reconcile, from the caught-up cache, creates the target VM on the imported disk")
		c.serveStale(nil)
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		target := &infravirtrigaudiov1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "web"}, target)).To(Succeed())
		Expect(target.Spec.ImportedDisk).NotTo(BeNil())
		Expect(target.Spec.ImportedDisk.Path).To(Equal(pathImportTargetPath))
		Expect(target.Spec.ImportedDisk.DiskID).To(Equal("disk-1"))
		final := &infravirtrigaudiov1beta1.VMMigration{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, final)).To(Succeed())
		Expect(final.Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseCreating))
	})

	It("records an ImportDisk result on the live VMMigration when another write landed while the import ran", func() {
		ns := newNamespace("clobber-rpc-")
		Expect(k8sClient.Create(ctx, holdTestProvider(ns))).To(Succeed())
		holdTestVM(ns, "src-vm", infravirtrigaudiov1beta1.ObservedPowerStateOff)
		m := &infravirtrigaudiov1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: ns, Finalizers: []string{infravirtrigaudiov1beta1.VMMigrationFinalizer}},
			Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
				Source: infravirtrigaudiov1beta1.MigrationSource{VMRef: infrav1LocalRef("src-vm")},
				Target: infravirtrigaudiov1beta1.MigrationTarget{Name: "web", ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-1"}},
				Storage: &infravirtrigaudiov1beta1.MigrationStorage{
					Type: "pvc",
					PVC:  &infravirtrigaudiov1beta1.PVCStorageConfig{Name: "mig-pvc"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		m.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseImporting
		m.Status.StoragePVCName = "mig-pvc"
		Expect(k8sClient.Status().Update(ctx, m)).To(Succeed())

		// While the import runs, a label is added to the VMMigration: its
		// resourceVersion moves, so the reconcile's write conflicts.
		prov := &labelDuringImportProvider{key: client.ObjectKeyFromObject(m)}
		r := NewVMMigrationReconciler(k8sClient, k8sClient, k8sClient.Scheme(), nil, record.NewFakeRecorder(100), false)
		r.providerInstanceFn = onlyProviderIn(ns, prov)

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)})
		Expect(err).NotTo(HaveOccurred())
		Expect(prov.importCalls.Load()).To(BeEquivalentTo(1))
		got := &infravirtrigaudiov1beta1.VMMigration{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), got)).To(Succeed())
		Expect(got.Labels).To(HaveKeyWithValue("touched", "during-import"))
		Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseCreating))
		Expect(got.Status.ImportID).To(Equal("disk-1"))
		Expect(got.Status.DiskInfo).NotTo(BeNil())
		Expect(got.Status.DiskInfo.TargetDiskID).To(Equal("disk-1"))
	})

	// In the lab the Provider controller (every health check) and the
	// VMAdoption controller wrote the same Provider's status and conflicted;
	// the Provider controller's whole-status copy dropped status.adoption.
	It("keeps an adoption-status write when the Provider controller writes the Provider's status from an older read", func() {
		ns := newNamespace("clobber-prov-")
		Expect(k8sClient.Create(ctx, holdTestProvider(ns))).To(Succeed())
		key := client.ObjectKey{Namespace: ns, Name: "prov-1"}
		c := &staleReadClient{Client: k8sClient}
		pr := &ProviderReconciler{Client: c, APIReader: k8sClient, Scheme: k8sClient.Scheme()}

		// A first reconcile adds the in-use finalizer and writes the status.
		// The stored status then lacks observedGeneration, so the Provider
		// controller's next write has something to change.
		_, _ = pr.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		providerRead := &infravirtrigaudiov1beta1.Provider{}
		Expect(k8sClient.Get(ctx, key, providerRead)).To(Succeed())
		Expect(providerRead.Status.Runtime).NotTo(BeNil())
		providerRead.Status.ObservedGeneration = 0
		Expect(k8sClient.Status().Update(ctx, providerRead)).To(Succeed())

		By("the VMAdoption controller records a discovery")
		ar := &VMAdoptionReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		adoptionRead := providerRead.DeepCopy()
		now := metav1.Now()
		Expect(ar.writeAdoptionResult(ctx, adoptionRead, &infravirtrigaudiov1beta1.ProviderAdoptionStatus{
			LastDiscoveryTime: &now, DiscoveredVMs: 3, AdoptedVMs: 2, FailedAdoptions: 1, Message: "Adopted 2 VMs, 1 failed",
		})).To(Succeed())

		By("the Provider controller reconciles from a read that predates it")
		c.serveStale(providerRead)
		_, _ = pr.Reconcile(ctx, ctrl.Request{NamespacedName: key})

		got := &infravirtrigaudiov1beta1.Provider{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.Adoption).NotTo(BeNil(), "the Provider controller's write did not drop status.adoption")
		Expect(got.Status.Adoption.AdoptedVMs).To(BeEquivalentTo(2))
		Expect(got.Status.Adoption.Message).To(Equal("Adopted 2 VMs, 1 failed"))
		Expect(got.Status.Runtime).NotTo(BeNil())
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation), "the Provider controller's write landed")
		Expect(got.ResourceVersion).NotTo(Equal(adoptionRead.ResourceVersion))

		By("an adoption-status write from an older read keeps the Provider controller's fields")
		Expect(ar.writeAdoptionMessage(ctx, providerRead.DeepCopy(), adoptionProviderNotReadyMessage)).To(Succeed())
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.Adoption.Message).To(Equal(adoptionProviderNotReadyMessage))
		Expect(got.Status.Adoption.AdoptedVMs).To(BeEquivalentTo(2), "a message write keeps the discovery's counts")
		Expect(got.Status.Runtime).NotTo(BeNil())
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
	})

	It("does not write the Provider controller's status computed from a read its own fields have moved past", func() {
		ns := newNamespace("clobber-prov-own-")
		Expect(k8sClient.Create(ctx, holdTestProvider(ns))).To(Succeed())
		key := client.ObjectKey{Namespace: ns, Name: "prov-1"}
		pr := &ProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, _ = pr.Reconcile(ctx, ctrl.Request{NamespacedName: key})

		read := &infravirtrigaudiov1beta1.Provider{}
		Expect(k8sClient.Get(ctx, key, read)).To(Succeed())
		base := read.Status.DeepCopy()

		// A newer write of the Provider controller's own fields.
		newer := read.DeepCopy()
		newer.Status.Version = "v-newer"
		Expect(k8sClient.Status().Update(ctx, newer)).To(Succeed())

		read.Status.Version = "v-stale"
		err := pr.writeProviderStatus(ctx, base, read)
		Expect(err).To(MatchError(ContainSubstring(errProviderStatusMovedOn.Error())))
		got := infrav1ProviderGet(key)
		Expect(got.Status.Version).To(Equal("v-newer"))

		By("a reconcile from that older read requeues shortly instead of failing")
		c := &staleReadClient{Client: k8sClient}
		c.serveStale(read)
		stalePR := &ProviderReconciler{Client: c, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		res, err := stalePR.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred(), "a refused stale write is not a reconcile error")
		Expect(res.RequeueAfter).To(Equal(providerStatusConflictRequeue))
		got = infrav1ProviderGet(key)
		Expect(got.Status.Version).To(Equal("v-newer"), "nothing was written over the newer status")
	})
})

// infrav1ProviderGet reads the Provider key from the API server.
func infrav1ProviderGet(key client.ObjectKey) *infravirtrigaudiov1beta1.Provider {
	got := &infravirtrigaudiov1beta1.Provider{}
	Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
	return got
}

// labelDuringImportProvider is countingMigrationProvider whose ImportDisk
// adds a label to the VMMigration key while it runs, as a user or another
// controller editing the object during a long import would.
type labelDuringImportProvider struct {
	countingMigrationProvider
	key client.ObjectKey
}

// ImportDisk labels the VMMigration, then answers like countingMigrationProvider.
func (p *labelDuringImportProvider) ImportDisk(ctx context.Context, req contracts.ImportDiskRequest) (contracts.ImportDiskResponse, error) {
	m := &infravirtrigaudiov1beta1.VMMigration{}
	if err := k8sClient.Get(ctx, p.key, m); err != nil {
		return contracts.ImportDiskResponse{}, err
	}
	if m.Labels == nil {
		m.Labels = map[string]string{}
	}
	m.Labels["touched"] = "during-import"
	if err := k8sClient.Update(ctx, m); err != nil {
		return contracts.ImportDiskResponse{}, err
	}
	return p.countingMigrationProvider.ImportDisk(ctx, req)
}

// infrav1LocalRef is a LocalObjectReference to name.
func infrav1LocalRef(name string) infravirtrigaudiov1beta1.LocalObjectReference {
	return infravirtrigaudiov1beta1.LocalObjectReference{Name: name}
}
