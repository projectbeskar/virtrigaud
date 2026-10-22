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
	"errors"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	storagemigration "github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

// routedMigrationProvider is the provider of a migration from a single-host
// source into a clustered target (ADR-0007 Addendum B), shared by both
// Providers of a spec and by the VirtualMachine and VMMigration controllers.
// It records every call; its answers are scripted under mu.
type routedMigrationProvider struct {
	stubProvider

	mu sync.Mutex
	// routedImport is reported as supportsRoutedImport.
	routedImport bool
	// diskBytes is the source disk's virtual size (GetDiskInfo).
	diskBytes int64
	// importErrs answer the next ImportDisk calls, in order (nil: success).
	importErrs []error
	// createErr answers every Create while set.
	createErr error
	// deleteErr answers every Delete while set.
	deleteErr error
	// exportTarget names the VM whose pending host each ExportDisk records
	// in pendingAtExport (read outside mu).
	exportTarget    types.NamespacedName
	pendingAtExport []string

	exports       int
	imports       []contracts.ImportDiskRequest
	creates       []contracts.CreateRequest
	deletes       []contracts.VMRef
	landedDeletes []contracts.LandedDiskDeleteRequest
	stagedDeletes []contracts.StagedDiskDeleteRequest
}

func newRoutedMigrationProvider() *routedMigrationProvider {
	return &routedMigrationProvider{routedImport: true, diskBytes: 10 << 30}
}

func (p *routedMigrationProvider) GetCapabilities(context.Context) (contracts.Capabilities, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return contracts.Capabilities{
		SupportsClustering: true, SupportsListOwnerFilter: true, SupportsDiskExport: true, SupportsDiskImport: true,
		SupportsRoutedImport: p.routedImport, SupportsLandedDiskDelete: true,
		SupportedStagedDiskDeleteBackends: []string{storagemigration.BackendS3, storagemigration.BackendNFS},
		ResendSafeImportBackends:          []string{storagemigration.BackendS3, storagemigration.BackendNFS},
	}, nil
}

func (p *routedMigrationProvider) ListVMsForOwner(context.Context, contracts.OwnerFilter) (contracts.VMList, error) {
	return contracts.VMList{OwnerFilterApplied: true}, nil
}

func (p *routedMigrationProvider) GetDiskInfo(context.Context, contracts.GetDiskInfoRequest) (contracts.GetDiskInfoResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return contracts.GetDiskInfoResponse{VirtualSizeBytes: p.diskBytes}, nil
}

func (p *routedMigrationProvider) ExportDisk(context.Context, contracts.ExportDiskRequest) (contracts.ExportDiskResponse, error) {
	p.mu.Lock()
	p.exports++
	key := p.exportTarget
	p.mu.Unlock()
	if key.Name != "" {
		host := "missing"
		vm := &infravirtrigaudiov1beta1.VirtualMachine{}
		if err := k8sClient.Get(ctx, key, vm); err == nil {
			host = pendingHost(vm)
		}
		p.mu.Lock()
		p.pendingAtExport = append(p.pendingAtExport, host)
		p.mu.Unlock()
	}
	return contracts.ExportDiskResponse{ExportId: "exp-1"}, nil
}

func (p *routedMigrationProvider) ImportDisk(_ context.Context, req contracts.ImportDiskRequest) (contracts.ImportDiskResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.imports = append(p.imports, req)
	if len(p.importErrs) > 0 {
		err := p.importErrs[0]
		p.importErrs = p.importErrs[1:]
		if err != nil {
			return contracts.ImportDiskResponse{}, err
		}
	}
	name := req.TargetVM.Namespace + "." + req.TargetVM.Name + contracts.ImportedDiskNameSuffix
	return contracts.ImportDiskResponse{DiskId: name, Path: "/var/lib/libvirt/images/" + name + ".qcow2"}, nil
}

func (p *routedMigrationProvider) Create(_ context.Context, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates = append(p.creates, req)
	if p.createErr != nil {
		return contracts.CreateResponse{}, p.createErr
	}
	return contracts.CreateResponse{ID: req.Name}, nil
}

func (p *routedMigrationProvider) Delete(_ context.Context, ref contracts.VMRef) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deletes = append(p.deletes, ref)
	return "", p.deleteErr
}

func (p *routedMigrationProvider) Describe(context.Context, contracts.VMRef) (contracts.DescribeResponse, error) {
	return contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOn)}, nil
}

func (p *routedMigrationProvider) DeleteLandedDisk(_ context.Context, req contracts.LandedDiskDeleteRequest) (contracts.LandedDiskDeleteOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.landedDeletes = append(p.landedDeletes, req)
	return contracts.LandedDiskDeleted, nil
}

func (p *routedMigrationProvider) DeleteStagedDisk(_ context.Context, req contracts.StagedDiskDeleteRequest) (contracts.StagedDiskDeleteOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stagedDeletes = append(p.stagedDeletes, req)
	return contracts.StagedDiskDeleted, nil
}

// set runs f under mu (scripting answers from a spec).
func (p *routedMigrationProvider) set(f func(p *routedMigrationProvider)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
}

// exportPendingHosts returns the target's pending host at each ExportDisk.
func (p *routedMigrationProvider) exportPendingHosts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.pendingAtExport...)
}

// snapshot returns copies of the recorded calls.
func (p *routedMigrationProvider) snapshot() (exports int, imports []contracts.ImportDiskRequest, creates []contracts.CreateRequest,
	landed []contracts.LandedDiskDeleteRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exports, append([]contracts.ImportDiskRequest(nil), p.imports...),
		append([]contracts.CreateRequest(nil), p.creates...), append([]contracts.LandedDiskDeleteRequest(nil), p.landedDeletes...)
}

// routedHost is one Host of the clustered target Provider.
type routedHost struct {
	name         string
	cpu          int32
	storageBytes *int64
}

// routedFixture is one spec's namespace, objects and running controllers.
type routedFixture struct {
	ns        string
	prov      *routedMigrationProvider
	migration *infravirtrigaudiov1beta1.VMMigration
	rec       *record.FakeRecorder
}

// routedFixtureOptions shape a fixture.
type routedFixtureOptions struct {
	hosts []routedHost
	// retry gives the migration a retry policy (one retry after 1 s).
	retry bool
	// loseStatus, when set, makes the VirtualMachine controller's status
	// writes it returns true for fail without reaching the apiserver.
	loseStatus func(client.Object) bool
}

// lossyStatusClient is a client whose status writes selected by lose are
// lost (they never reach the apiserver).
type lossyStatusClient struct {
	client.Client
	lose func(client.Object) bool
}

func (c *lossyStatusClient) Status() client.SubResourceWriter {
	return &lossyStatusWriter{SubResourceWriter: c.Client.Status(), lose: c.lose}
}

type lossyStatusWriter struct {
	client.SubResourceWriter
	lose func(client.Object) bool
}

func (w *lossyStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if w.lose(obj) {
		return errors.New("connection reset by peer")
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func storageGiB(n int64) *int64 { v := n << 30; return &v }

// newRoutedFixture creates, in a fresh namespace, a single-host source
// Provider and VM, a clustered target Provider with its pool and hosts, the
// s3 credentials and a VMMigration into the clustered Provider, and runs the
// VirtualMachine and VMMigration controllers on prov.
func newRoutedFixture(prov *routedMigrationProvider, opts routedFixtureOptions) *routedFixture {
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "routed-mig-"}}
	Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
	ns := nsObj.Name
	// Objects with finalizers are released once the spec's manager stopped
	// (DeferCleanup is LIFO; the manager's stop is registered later).
	DeferCleanup(func() { releaseRoutedNamespace(ns) })

	ready := func(p *infravirtrigaudiov1beta1.Provider, caps *infravirtrigaudiov1beta1.ReportedCapabilities) {
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		p.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
		p.Status.Conditions = []metav1.Condition{{Type: "ProviderAvailable", Status: metav1.ConditionTrue, Reason: "RemoteAvailable",
			Message: "ready", LastTransitionTime: metav1.Now()}}
		p.Status.ReportedCapabilities = caps
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
	}
	ready(envtestProvider(ns, "src", nil), &infravirtrigaudiov1beta1.ReportedCapabilities{
		SupportedExportBackends: []string{"pvc", "s3", "nfs"}, SupportedTransferModes: []string{"relay"},
	})
	target := envtestProvider(ns, "clustered", nil)
	target.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
	ready(target, &infravirtrigaudiov1beta1.ReportedCapabilities{
		SupportedImportBackends: []string{"s3", "nfs"}, SupportedTransferModes: []string{"relay"},
	})

	Expect(k8sClient.Create(ctx, &infravirtrigaudiov1beta1.HostPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: ns},
		Spec: infravirtrigaudiov1beta1.HostPoolSpec{
			ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "clustered"},
			Strategy:    infravirtrigaudiov1beta1.PoolStrategySpread,
		},
	})).To(Succeed())
	for _, h := range opts.hosts {
		host := &infravirtrigaudiov1beta1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: h.name, Namespace: ns},
			Spec: infravirtrigaudiov1beta1.HostSpec{
				ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "clustered"},
				PoolRef:     infravirtrigaudiov1beta1.LocalObjectReference{Name: "pool"},
				Endpoint:    "qemu+ssh://virt@" + h.name + "/system",
				Schedulable: true,
			},
		}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		cpu, mem := h.cpu, int64(1<<20)
		host.Status = infravirtrigaudiov1beta1.HostStatus{
			Health: infravirtrigaudiov1beta1.HostHealthReady, AllocatableCPU: &cpu, AllocatableMemoryMiB: &mem,
			AllocatableStorageBytes: h.storageBytes,
		}
		Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
	}
	Expect(k8sClient.Create(ctx, envtestClass(ns, "small", nil))).To(Succeed())
	Expect(k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: stagingTestSecret, Namespace: ns},
		Data: map[string][]byte{
			storagemigration.CredKeyAccessKeyID:     []byte("AKID"),
			storagemigration.CredKeySecretAccessKey: []byte(stagingTestKeyValue),
		},
	})).To(Succeed())

	src := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "src-vm", Namespace: ns},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef:  infravirtrigaudiov1beta1.ObjectRef{Name: "src"},
			ClassRef:     infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
			ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "src-disk", Source: "manual"},
		},
	}
	Expect(k8sClient.Create(ctx, src)).To(Succeed())
	src.Status.ID = "src-vm"
	Expect(k8sClient.Status().Update(ctx, src)).To(Succeed())

	m := &infravirtrigaudiov1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: ns},
		Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
			Source: infravirtrigaudiov1beta1.MigrationSource{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infravirtrigaudiov1beta1.MigrationTarget{
				Name: "web", ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "clustered"},
				ClassRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "small"}, PowerOn: true,
			},
			Storage: &infravirtrigaudiov1beta1.MigrationStorage{
				Type: storagemigration.BackendS3,
				S3: &infravirtrigaudiov1beta1.S3StorageConfig{
					Bucket: stagingTestBucket, Endpoint: stagingTestEndpoint, UsePathStyle: true,
					CredentialsSecretRef: infravirtrigaudiov1beta1.ObjectRef{Name: stagingTestSecret},
				},
			},
		},
	}
	if opts.retry {
		maxRetries := int32(1)
		m.Spec.Options = &infravirtrigaudiov1beta1.MigrationOptions{RetryPolicy: &infravirtrigaudiov1beta1.MigrationRetryPolicy{
			MaxRetries: &maxRetries, RetryDelay: &metav1.Duration{Duration: time.Second},
		}}
	}

	rec := record.NewFakeRecorder(1000)
	mgrCtx, stop := context.WithCancel(ctx)
	mgrOpts := ctrl.Options{
		Scheme:                 k8sClient.Scheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
	}
	mgr, err := ctrl.NewManager(cfg, mgrOpts)
	Expect(err).NotTo(HaveOccurred())
	vmClient := mgr.GetClient()
	if opts.loseStatus != nil {
		vmClient = &lossyStatusClient{Client: mgr.GetClient(), lose: opts.loseStatus}
	}
	Expect((&VirtualMachineReconciler{
		Client: vmClient, Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader(),
		RemoteResolver: &atomicResolver{provider: prov, namespace: ns}, Recorder: rec,
	}).SetupWithManager(mgr)).To(Succeed())
	r := NewVMMigrationReconciler(mgr.GetClient(), mgr.GetAPIReader(), mgr.GetScheme(), nil, rec, false)
	r.providerInstanceFn = onlyProviderIn(ns, prov)
	Expect(r.SetupWithManager(mgr)).To(Succeed())
	done := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(done)
		Expect(mgr.Start(mgrCtx)).To(Succeed())
	}()
	DeferCleanup(func() { stop(); <-done })

	Expect(k8sClient.Create(ctx, m)).To(Succeed())
	return &routedFixture{ns: ns, prov: prov, migration: m, rec: rec}
}

// releaseRoutedNamespace strips the finalizers of a spec's VMs and
// VMMigrations and deletes them, so a later spec's manager never reconciles
// them.
func releaseRoutedNamespace(ns string) {
	strip := func(list client.ObjectList, items func() []client.Object) {
		Expect(k8sClient.List(ctx, list, client.InNamespace(ns))).To(Succeed())
		for _, o := range items() {
			key := client.ObjectKeyFromObject(o)
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := k8sClient.Get(ctx, key, o); err != nil {
					return client.IgnoreNotFound(err)
				}
				o.SetFinalizers(nil)
				return k8sClient.Update(ctx, o)
			})).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
	}
	vms := &infravirtrigaudiov1beta1.VirtualMachineList{}
	strip(vms, func() []client.Object {
		out := make([]client.Object, 0, len(vms.Items))
		for i := range vms.Items {
			out = append(out, &vms.Items[i])
		}
		return out
	})
	migs := &infravirtrigaudiov1beta1.VMMigrationList{}
	strip(migs, func() []client.Object {
		out := make([]client.Object, 0, len(migs.Items))
		for i := range migs.Items {
			out = append(out, &migs.Items[i])
		}
		return out
	})
}

func (f *routedFixture) getMigration(g Gomega) *infravirtrigaudiov1beta1.VMMigration {
	m := &infravirtrigaudiov1beta1.VMMigration{}
	g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(f.migration), m)).To(Succeed())
	return m
}

func (f *routedFixture) getTarget(g Gomega) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: "web"}, vm)).To(Succeed())
	return vm
}

func condReason(conds []metav1.Condition, condType string) string {
	if c := meta.FindStatusCondition(conds, condType); c != nil {
		return c.Reason
	}
	return ""
}

var _ = Describe("Migration into a clustered provider (ADR-0007 Addendum B, envtest)", func() {
	It("places the target before the export, on a host with room for the disk, and lands the disk there", func() {
		prov := newRoutedMigrationProvider()
		// kvm-big would win Spread on CPU; it cannot hold the 10 GiB source disk.
		f := newRoutedFixture(prov, routedFixtureOptions{hosts: []routedHost{
			{name: "kvm-big", cpu: 64, storageBytes: storageGiB(5)},
			{name: "kvm-01", cpu: 8, storageBytes: storageGiB(100)},
		}})
		prov.set(func(p *routedMigrationProvider) { p.exportTarget = types.NamespacedName{Namespace: f.ns, Name: "web"} })

		Eventually(func(g Gomega) {
			m := f.getMigration(g)
			g.Expect(m.Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseReady), m.Status.Message)
		}, "60s", "250ms").Should(Succeed())

		m := f.getMigration(Default)
		target := f.getTarget(Default)
		exports, imports, creates, _ := prov.snapshot()
		Expect(exports).To(Equal(1))
		Expect(prov.exportPendingHosts()).To(Equal([]string{"kvm-01"}), "placed (on the host with room) before the export")
		Expect(target.Spec.ImportedDisk.SizeGiB).To(Equal(int32(10)), "the source disk's size drives placement")
		Expect(imports).To(HaveLen(1))
		Expect(imports[0].TargetHostID).To(Equal("kvm-01"))
		Expect(imports[0].TargetVM.UID).To(Equal(string(target.UID)))
		Expect(m.Status.StorageInfo.ImportTarget.Host).To(Equal("kvm-01"))
		Expect(m.Status.TargetVMUID).To(Equal(string(target.UID)), "the target created at Validating is the one completed")
		Expect(condReason(m.Status.Conditions, migrationConditionTargetPlaced)).To(Equal(migrationReasonTargetPlaced))
		Expect(creates).To(HaveLen(1), "one Create, after the import")
		Expect(creates[0].TargetHostID).To(Equal("kvm-01"))
		Expect(creates[0].Image.ImportedDisk).To(BeTrue(), "the landed disk is attached in place")
		Expect(creates[0].Image.Path).To(Equal(m.Status.DiskInfo.TargetPath))
		Expect(target.Status.Placement.Host).To(Equal("kvm-01"))
	})

	It("waits, without touching the source, for a target that cannot be placed", func() {
		prov := newRoutedMigrationProvider()
		f := newRoutedFixture(prov, routedFixtureOptions{hosts: []routedHost{{name: "kvm-01", cpu: 8, storageBytes: storageGiB(5)}}})

		Eventually(func(g Gomega) {
			m := f.getMigration(g)
			g.Expect(m.Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseValidating))
			g.Expect(condReason(m.Status.Conditions, migrationConditionTargetPlaced)).To(Equal(k8s.ReasonUnschedulable))
		}, "40s", "250ms").Should(Succeed())
		Consistently(func(g Gomega) {
			exports, imports, creates, _ := prov.snapshot()
			g.Expect(exports).To(BeZero())
			g.Expect(imports).To(BeEmpty())
			g.Expect(creates).To(BeEmpty())
			g.Expect(f.getMigration(g).Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseValidating), "never failed for it")
		}, "3s", "250ms").Should(Succeed())
	})

	It("does not export while the target's pending-host write was lost, and places it on the next attempt", func() {
		prov := newRoutedMigrationProvider()
		var mu sync.Mutex
		lost := 0
		lose := func(obj client.Object) bool {
			vm, ok := obj.(*infravirtrigaudiov1beta1.VirtualMachine)
			if !ok || vm.Name != "web" || pendingHost(vm) == "" {
				return false
			}
			mu.Lock()
			defer mu.Unlock()
			if lost < 2 {
				lost++
				return true
			}
			return false
		}
		f := newRoutedFixture(prov, routedFixtureOptions{hosts: []routedHost{{name: "kvm-01", cpu: 8, storageBytes: storageGiB(100)}}, loseStatus: lose})
		prov.set(func(p *routedMigrationProvider) { p.exportTarget = types.NamespacedName{Namespace: f.ns, Name: "web"} })

		Eventually(func(g Gomega) {
			g.Expect(f.getMigration(g).Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseReady))
		}, "90s", "250ms").Should(Succeed())
		mu.Lock()
		Expect(lost).To(Equal(2))
		mu.Unlock()
		Expect(prov.exportPendingHosts()).To(Equal([]string{"kvm-01"}), "the export waited for a durable pending host")
		exports, imports, creates, _ := prov.snapshot()
		Expect(exports).To(Equal(1))
		Expect(imports).To(HaveLen(1))
		Expect(creates).To(HaveLen(1))
	})

	It("releases the pending host on a definite name conflict at import and follows the VM to its next host", func() {
		prov := newRoutedMigrationProvider()
		prov.set(func(p *routedMigrationProvider) {
			p.importErrs = []error{contracts.NewConflictError("importDisk: a libvirt domain with the target's name exists", nil)}
		})
		f := newRoutedFixture(prov, routedFixtureOptions{hosts: []routedHost{
			{name: "kvm-01", cpu: 16, storageBytes: storageGiB(100)},
			{name: "kvm-02", cpu: 8, storageBytes: storageGiB(100)},
		}})

		Eventually(func(g Gomega) {
			g.Expect(f.getMigration(g).Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseReady))
		}, "90s", "250ms").Should(Succeed())
		_, imports, creates, _ := prov.snapshot()
		Expect(imports).To(HaveLen(2))
		Expect(imports[0].TargetHostID).To(Equal("kvm-01"))
		Expect(imports[1].TargetHostID).To(Equal("kvm-02"), "the import follows the re-placed VM")
		Expect(creates).To(HaveLen(1))
		Expect(creates[0].TargetHostID).To(Equal("kvm-02"))
		Expect(f.getMigration(Default).Status.StorageInfo.ImportTarget.Host).To(Equal("kvm-02"))
	})

	It("pins the target to the host its disk landed on, and deletes the unbound target before the landed disk", func() {
		prov := newRoutedMigrationProvider()
		prov.set(func(p *routedMigrationProvider) {
			p.createErr = contracts.NewConflictError("create: a domain with this name exists", nil)
			p.deleteErr = contracts.NewHostUnavailableError("delete: host kvm-01 unreachable", nil)
		})
		f := newRoutedFixture(prov, routedFixtureOptions{hosts: []routedHost{
			{name: "kvm-01", cpu: 16, storageBytes: storageGiB(100)},
			{name: "kvm-02", cpu: 8, storageBytes: storageGiB(100)},
		}})

		By("a Create conflict after the import holds the VM on its host")
		Eventually(func(g Gomega) {
			vm := f.getTarget(g)
			g.Expect(condReason(vm.Status.Conditions, k8s.ConditionPlaced)).To(Equal(k8s.ReasonImportedDiskPinned))
		}, "60s", "250ms").Should(Succeed())
		Consistently(func(g Gomega) {
			vm := f.getTarget(g)
			g.Expect(pendingHost(vm)).To(Equal("kvm-01"))
			g.Expect(vm.Status.Placement.ExcludedHosts).To(BeEmpty())
			g.Expect(vm.Status.Phase).To(Equal(infravirtrigaudiov1beta1.VirtualMachinePhaseFailed))
		}, "3s", "250ms").Should(Succeed())
		_, _, creates, _ := prov.snapshot()
		for _, c := range creates {
			Expect(c.TargetHostID).To(Equal("kvm-01"), "never re-scheduled")
		}

		By("deleting the migration deletes its unbound target first and holds the landed disk while that delete is held")
		Expect(k8sClient.Delete(ctx, f.migration)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(f.getTarget(g).DeletionTimestamp.IsZero()).To(BeFalse())
		}, "20s", "250ms").Should(Succeed())
		Consistently(func(g Gomega) {
			_, _, _, landed := prov.snapshot()
			g.Expect(landed).To(BeEmpty(), "no landed-disk delete while the target VM exists")
			g.Expect(f.getMigration(g).Finalizers).To(ContainElement(infravirtrigaudiov1beta1.VMMigrationFinalizer))
		}, "3s", "250ms").Should(Succeed())

		By("once the target VM is gone, the landed disk is deleted through the recorded host")
		prov.set(func(p *routedMigrationProvider) { p.deleteErr = nil })
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(f.migration), &infravirtrigaudiov1beta1.VMMigration{}))
		}, "60s", "250ms").Should(BeTrue())
		_, _, _, landed := prov.snapshot()
		Expect(landed).To(HaveLen(1))
		Expect(landed[0].TargetHostID).To(Equal("kvm-01"))
		Expect(landed[0].TargetVM.Name).To(Equal("web"))
	})

	It("deletes the unbound target before a retry, and the retry places a fresh one", func() {
		prov := newRoutedMigrationProvider()
		prov.set(func(p *routedMigrationProvider) {
			p.importErrs = []error{contracts.NewInvalidSpecError("importDisk: refused", nil)}
		})
		f := newRoutedFixture(prov, routedFixtureOptions{retry: true, hosts: []routedHost{{name: "kvm-01", cpu: 8, storageBytes: storageGiB(100)}}})

		var firstUID types.UID
		Eventually(func(g Gomega) {
			_, imports, _, _ := prov.snapshot()
			g.Expect(imports).NotTo(BeEmpty())
			firstUID = types.UID(imports[0].TargetVM.UID)
		}, "60s", "250ms").Should(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(f.getMigration(g).Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseReady))
		}, "90s", "250ms").Should(Succeed())
		_, imports, creates, landed := prov.snapshot()
		Expect(imports).To(HaveLen(2))
		Expect(types.UID(imports[1].TargetVM.UID)).NotTo(Equal(firstUID), "a fresh target VM")
		Expect(f.getTarget(Default).UID).To(Equal(types.UID(imports[1].TargetVM.UID)))
		Expect(creates).To(HaveLen(1))
		Expect(landed).To(HaveLen(1), "the first attempt's landed disk is cleaned up before the retry")
		Expect(landed[0].TargetHostID).To(Equal("kvm-01"))
		prov.mu.Lock()
		Expect(prov.deletes).NotTo(BeEmpty(), "the unbound target's finalizer ran its owner-checked delete")
		Expect(prov.deletes[0].HostID).To(Equal("kvm-01"))
		prov.mu.Unlock()
	})

	It("re-sends an import of unknown outcome only to the host it was recorded for", func() {
		prov := newRoutedMigrationProvider()
		prov.set(func(p *routedMigrationProvider) {
			p.importErrs = []error{contracts.NewRetryableError("importDisk: connection dropped", nil)}
		})
		f := newRoutedFixture(prov, routedFixtureOptions{hosts: []routedHost{
			{name: "kvm-01", cpu: 16, storageBytes: storageGiB(100)},
			{name: "kvm-02", cpu: 8, storageBytes: storageGiB(100)},
		}})

		Eventually(func(g Gomega) {
			m := f.getMigration(g)
			g.Expect(condReason(m.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionReady)).
				To(Equal(migrationReasonImportOutcomeUnknown))
		}, "60s", "250ms").Should(Succeed())

		By("an administrator moves the target's pending host: the re-send is held")
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			vm := f.getTarget(Default)
			vm.Status.Placement.PendingHost = "kvm-02"
			return k8sClient.Status().Update(ctx, vm)
		})).To(Succeed())
		confirm := func() {
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				m := f.getMigration(Default)
				if m.Annotations == nil {
					m.Annotations = map[string]string{}
				}
				m.Annotations[operationOutcomeAnnotation] = operationOutcomeNotPerformedPrefix +
					requestMarkerAttemptID(m.Status.Conditions, migrationConditionImportRequested)
				return k8sClient.Update(ctx, m)
			})).To(Succeed())
		}
		confirm()
		Eventually(func(g Gomega) {
			m := f.getMigration(g)
			c := meta.FindStatusCondition(m.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Message).To(Equal(routedImportHostMovedMessage))
		}, "30s", "250ms").Should(Succeed())
		_, imports, _, _ := prov.snapshot()
		Expect(imports).To(HaveLen(1), "nothing sent to the moved host")

		By("the pending host is set back: the re-send goes to the recorded host")
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			vm := f.getTarget(Default)
			vm.Status.Placement.PendingHost = "kvm-01"
			return k8sClient.Status().Update(ctx, vm)
		})).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(f.getMigration(g).Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseReady))
		}, "60s", "250ms").Should(Succeed())
		_, imports, creates, _ := prov.snapshot()
		Expect(imports).To(HaveLen(2))
		Expect(imports[1].TargetHostID).To(Equal("kvm-01"))
		Expect(creates[len(creates)-1].TargetHostID).To(Equal("kvm-01"))
	})

	It("refuses a clustered target whose provider does not route the import, before any side effect", func() {
		prov := newRoutedMigrationProvider()
		prov.set(func(p *routedMigrationProvider) { p.routedImport = false })
		f := newRoutedFixture(prov, routedFixtureOptions{hosts: []routedHost{{name: "kvm-01", cpu: 8, storageBytes: storageGiB(100)}}})
		Eventually(func(g Gomega) {
			m := f.getMigration(g)
			g.Expect(m.Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseFailed))
			g.Expect(m.Status.Message).To(Equal(clusteredTargetUnsupportedMessage))
		}, "30s", "250ms").Should(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: "web"},
			&infravirtrigaudiov1beta1.VirtualMachine{}))).To(BeTrue(), "no target VM")
		exports, _, _, _ := prov.snapshot()
		Expect(exports).To(BeZero())
	})

	It("holds a VM waiting for an import whose migration does not exist or did not create it (MigrationNotFound)", func() {
		prov := newRoutedMigrationProvider()
		f := newRoutedFixture(prov, routedFixtureOptions{hosts: []routedHost{{name: "kvm-01", cpu: 8, storageBytes: storageGiB(100)}}})
		for _, ref := range []string{"no-such-migration", f.migration.Name} {
			name := "forged-" + strings.ReplaceAll(ref, "-", "")
			Expect(k8sClient.Create(ctx, &infravirtrigaudiov1beta1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
				Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
					ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "clustered"},
					ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
					ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{
						Source: "migration", MigrationRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: ref},
					},
				},
			})).To(Succeed())
			Eventually(func(g Gomega) {
				vm := &infravirtrigaudiov1beta1.VirtualMachine{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: name}, vm)).To(Succeed())
				g.Expect(condReason(vm.Status.Conditions, k8s.ConditionProvisioning)).To(Equal(k8s.ReasonMigrationNotFound), ref)
				g.Expect(pendingHost(vm)).To(BeEmpty(), "never placed")
			}, "30s", "250ms").Should(Succeed())
		}
		_, _, creates, _ := prov.snapshot()
		for _, c := range creates {
			Expect(c.Name).NotTo(HavePrefix("forged"))
		}
	})
})

var _ = Describe("ImportedDisk diskID validation (envtest)", func() {
	It("accepts an empty diskID only while migrationRef names a VMMigration", func() {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "diskid-cel-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		vm := func(name string, disk *infravirtrigaudiov1beta1.ImportedDiskRef) *infravirtrigaudiov1beta1.VirtualMachine {
			return &infravirtrigaudiov1beta1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsObj.Name},
				Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
					ProviderRef:  infravirtrigaudiov1beta1.ObjectRef{Name: "p"},
					ClassRef:     infravirtrigaudiov1beta1.ObjectRef{Name: "c"},
					ImportedDisk: disk,
				},
			}
		}
		err := k8sClient.Create(ctx, vm("no-ref", &infravirtrigaudiov1beta1.ImportedDiskRef{Source: "manual"}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("importedDisk.diskID may be empty only while importedDisk.migrationRef"))
		Expect(k8sClient.Create(ctx, vm("with-ref", &infravirtrigaudiov1beta1.ImportedDiskRef{
			Source: "migration", MigrationRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "mig"},
		}))).To(Succeed())
		Expect(k8sClient.Create(ctx, vm("with-id", &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "d1"}))).To(Succeed())
	})
})
