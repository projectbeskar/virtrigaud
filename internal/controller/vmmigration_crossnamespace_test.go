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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the cross-namespace target rule for VMMigration (the own
// namespace is unchanged; another namespace needs its grant, re-checked at
// Validating, Importing, Creating, Validating-Target and deletion) and the
// source-provider rule (spec.source.providerRef must be the Provider the source
// VM runs on).

// migrationSpy counts every provider call that has a side effect.
type migrationSpy struct {
	stubProvider
	mu                                 sync.Mutex
	imports, exports, snapshots, power int
}

func (p *migrationSpy) ImportDisk(_ context.Context, _ contracts.ImportDiskRequest) (contracts.ImportDiskResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.imports++
	return contracts.ImportDiskResponse{DiskId: "team-b.web-migrated", Path: "/pool/team-b.web-migrated.qcow2"}, nil
}

func (p *migrationSpy) ExportDisk(_ context.Context, _ contracts.ExportDiskRequest) (contracts.ExportDiskResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exports++
	return contracts.ExportDiskResponse{ExportId: "exp-1"}, nil
}

func (p *migrationSpy) SnapshotCreate(_ context.Context, _ contracts.SnapshotCreateRequest) (contracts.SnapshotCreateResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshots++
	return contracts.SnapshotCreateResponse{SnapshotId: "snap-1"}, nil
}

func (p *migrationSpy) Power(_ context.Context, _ contracts.VMRef, _ contracts.PowerOp) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.power++
	return "", nil
}

func (p *migrationSpy) Describe(_ context.Context, _ contracts.VMRef) (contracts.DescribeResponse, error) {
	return contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOn)}, nil
}

func (p *migrationSpy) calls() (imports, exports, snapshots, power int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.imports, p.exports, p.snapshots, p.power
}

var _ contracts.Provider = (*migrationSpy)(nil)

// xnsMigration builds a migration in team-a (with its source VM and two ready
// Providers there) whose target VM "web" lives in targetNamespace.
func xnsMigration(targetNamespace string) (*infrav1beta1.VMMigration, []client.Object) {
	srcVM := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "src-vm", Namespace: xnsSource},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "src-prov"},
			PowerState:  infrav1beta1.PowerStateOn,
		},
		Status: infrav1beta1.VirtualMachineStatus{ID: "vm-1"},
	}
	srcProv := readyProvider(xnsSource, "src-prov")
	srcProv.Spec.Type = infrav1beta1.ProviderTypeVSphere
	tgtProv := readyProvider(xnsSource, "tgt-prov")
	tgtProv.Spec.Type = infrav1beta1.ProviderTypeLibvirt
	// The target VM references the target Provider and class from the target
	// namespace, so both are shared (spec.consumerNamespaceSelector: {}).
	tgtProv.Spec.ConsumerNamespaceSelector = &metav1.LabelSelector{}
	migration := &infrav1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: xnsSource, UID: "uid-xns-1", Generation: 1},
		Spec: infrav1beta1.VMMigrationSpec{
			Source: infrav1beta1.MigrationSource{VMRef: infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infrav1beta1.MigrationTarget{
				Name:        "web",
				Namespace:   targetNamespace,
				ProviderRef: infrav1beta1.ObjectRef{Name: "tgt-prov"},
				ClassRef:    &infrav1beta1.LocalObjectReference{Name: "cls"},
				Labels:      map[string]string{"tenant": "team-a"},
			},
		},
	}
	return migration, []client.Object{srcVM, srcProv, tgtProv, grantedClass(xnsSource, "cls", &metav1.LabelSelector{})}
}

func newXNSMigrationReconciler(t *testing.T, scheme *runtime.Scheme, prov contracts.Provider, objs ...client.Object) (*VMMigrationReconciler, *record.FakeRecorder) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&infrav1beta1.VMMigration{}, &infrav1beta1.VirtualMachine{}).
		Build()
	rec := record.NewFakeRecorder(100)
	return &VMMigrationReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: rec,
		providerInstanceFn: func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
			return prov, nil
		},
	}, rec
}

func getXNSMigration(t *testing.T, r *VMMigrationReconciler, m *infrav1beta1.VMMigration) *infrav1beta1.VMMigration {
	t.Helper()
	got := &infrav1beta1.VMMigration{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(m), got))
	return got
}

// assertMigrationRefused checks Ready=False / TargetNamespaceNotAllowed at the
// current generation with the standard message, and that the migration was
// NOT failed.
func assertMigrationRefused(t *testing.T, got *infrav1beta1.VMMigration, targetNamespace string) {
	t.Helper()
	ready := readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, ReasonTargetNamespaceNotAllowed, ready.Reason)
	assert.Equal(t, got.Generation, ready.ObservedGeneration)
	assert.Equal(t, got.Generation, got.Status.ObservedGeneration)
	assert.Equal(t, targetNamespaceNotAllowedMessage(got.Namespace, targetNamespace), ready.Message)
	assert.Equal(t, ready.Message, got.Status.Message)
	assert.NotEqual(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase, "a refusal is recoverable, not a failure")
	assert.Zero(t, got.Status.RetryCount)
}

// TestVMMigrationXNS_RefusedBeforeAnySideEffect drives a fresh migration
// through Reconcile: without a grant it stops at Validating before powering
// off the source or creating the staging PVC, and nothing appears in the
// target namespace — whatever form the missing grant takes.
func TestVMMigrationXNS_RefusedBeforeAnySideEffect(t *testing.T) {
	cases := map[string][]client.Object{
		"namespace does not exist": nil,
		"no annotation":            {grantNamespace(xnsTarget, nil)},
		"other namespaces only":    {grantNamespace(xnsTarget, strPtr(" team-c , team-d "))},
		"wildcard is not a grant":  {grantNamespace(xnsTarget, strPtr("*"))},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			migration, objs := xnsMigration(xnsTarget)
			migration.Spec.Source.PowerOffBeforeMigration = true
			migration.Spec.Storage = &infrav1beta1.MigrationStorage{
				Type: "pvc",
				PVC:  &infrav1beta1.PVCStorageConfig{StorageClassName: "std", Size: "10Gi"},
			}
			spy := &migrationSpy{}
			r, rec := newXNSMigrationReconciler(t, mountTestScheme(t), spy, append(append(objs, migration), extra...)...)

			var res reconcile.Result
			for i := 0; i < 6; i++ {
				var err error
				res, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(migration)})
				require.NoError(t, err)
			}

			assert.Equal(t, crossNamespaceRecheckInterval, res.RequeueAfter, "a refusal rechecks slowly")
			got := getXNSMigration(t, r, migration)
			assert.Equal(t, infrav1beta1.MigrationPhaseValidating, got.Status.Phase)
			assertMigrationRefused(t, got, xnsTarget)
			validating := readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionValidating)
			require.NotNil(t, validating)
			assert.Equal(t, metav1.ConditionFalse, validating.Status)
			assert.Equal(t, ReasonTargetNamespaceNotAllowed, validating.Reason)

			imports, exports, snapshots, power := spy.calls()
			assert.Zero(t, imports+exports+snapshots+power, "no provider side effect for a refused migration")
			src := &infrav1beta1.VirtualMachine{}
			require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: xnsSource, Name: "src-vm"}, src))
			assert.Equal(t, infrav1beta1.PowerStateOn, src.Spec.PowerState, "the source is not powered off")
			assertNothingInNamespace(t, r.Client, xnsTarget)
			pvcs := &corev1.PersistentVolumeClaimList{}
			require.NoError(t, r.List(ctx, pvcs, client.InNamespace(xnsSource)))
			assert.Empty(t, pvcs.Items, "no staging PVC is created for a refused migration")
			assert.Equal(t, 1, countEvents(rec, ReasonTargetNamespaceNotAllowed), "one Warning event, on the transition")
		})
	}
}

// TestVMMigrationXNS_SameNamespaceUnchanged: an empty target namespace and
// the migration's own pass Validating with no grant and no Namespace read (the
// scheme has no corev1 types, so a Namespace read would fail).
func TestVMMigrationXNS_SameNamespaceUnchanged(t *testing.T) {
	for _, target := range []string{"", xnsSource} {
		t.Run("target="+target, func(t *testing.T) {
			migration, objs := xnsMigration(target)
			migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
			r, _ := newXNSMigrationReconciler(t, capGatingScheme(t), &migrationSpy{}, append(objs, migration)...)

			_, err := r.handleValidatingPhase(context.Background(), migration)
			require.NoError(t, err)
			assert.Equal(t, infrav1beta1.MigrationPhaseExporting, getXNSMigration(t, r, migration).Status.Phase)
		})
	}
}

// TestVMMigrationXNS_ValidatingAllowedWithGrant: a grant listing the
// migration's namespace (whitespace tolerated) lets Validating proceed.
func TestVMMigrationXNS_ValidatingAllowedWithGrant(t *testing.T) {
	migration, objs := xnsMigration(xnsTarget)
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
	r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), &migrationSpy{},
		append(objs, migration, grantNamespace(xnsTarget, strPtr("team-x ,  team-a")))...)

	_, err := r.handleValidatingPhase(context.Background(), migration)
	require.NoError(t, err)
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseExporting, got.Status.Phase)
	assert.Nil(t, readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionReady))
}

// TestVMMigrationXNS_RecoversWhenGranted: a migration refused at Validating
// proceeds once the grant is added, and the refusal conditions are cleared.
func TestVMMigrationXNS_RecoversWhenGranted(t *testing.T) {
	ctx := context.Background()
	migration, objs := xnsMigration(xnsTarget)
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
	ns := grantNamespace(xnsTarget, nil)
	r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), &migrationSpy{}, append(objs, migration, ns)...)

	_, err := r.handleValidatingPhase(ctx, migration)
	require.NoError(t, err)
	assertMigrationRefused(t, getXNSMigration(t, r, migration), xnsTarget)

	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(ns), ns))
	ns.Annotations = map[string]string{AllowedSourceNamespacesAnnotation: xnsSource}
	require.NoError(t, r.Update(ctx, ns))

	latest := getXNSMigration(t, r, migration)
	_, err = r.handleValidatingPhase(ctx, latest)
	require.NoError(t, err)
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseExporting, got.Status.Phase)
	assert.Nil(t, readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionReady), "the refusal is cleared")
	validating := readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionValidating)
	require.NotNil(t, validating)
	assert.Equal(t, metav1.ConditionTrue, validating.Status)
}

// TestVMMigrationXNS_ImportRefusedWithoutGrant: a grant revoked after
// Validating stops the import — no disk named for a VM in the target namespace
// is landed — and the import runs once the grant returns.
func TestVMMigrationXNS_ImportRefusedWithoutGrant(t *testing.T) {
	ctx := context.Background()
	prov := &capturingMigrationProvider{importResp: contracts.ImportDiskResponse{
		DiskId: "team-b.target-vm-migrated", Path: "/var/lib/libvirt/images/team-b.target-vm-migrated.qcow2",
	}}
	sourceVM, sourceProvider, targetProvider, migration := directionFixture(
		infrav1beta1.ProviderTypeVSphere, infrav1beta1.ProviderTypeLibvirt)
	migration.Spec.Target.Namespace = xnsTarget
	ns := grantNamespace(xnsTarget, strPtr("team-c"))
	r, _ := directionReconciler(t, prov, sourceVM, sourceProvider, targetProvider, migration, s3CredsSecret(), ns)

	res, err := r.handleImportingPhase(ctx, migration)
	require.NoError(t, err)
	assert.Equal(t, crossNamespaceRecheckInterval, res.RequeueAfter)
	assert.Zero(t, prov.importCalls, "no import into a namespace that does not grant the migration's")
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseImporting, got.Status.Phase, "the phase is kept to resume later")
	assertMigrationRefused(t, got, xnsTarget)
	assert.Empty(t, got.Status.ImportID)

	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(ns), ns))
	ns.Annotations[AllowedSourceNamespacesAnnotation] = "team-c,default"
	require.NoError(t, r.Update(ctx, ns))

	_, err = r.handleImportingPhase(ctx, getXNSMigration(t, r, migration))
	require.NoError(t, err)
	assert.Equal(t, 1, prov.importCalls)
	assert.Equal(t, contracts.ObjectIdentity{Namespace: xnsTarget, Name: "target-vm"}, prov.lastImportReq.TargetVM)
}

// creatingFixture stages a directionFixture migration in the Creating phase
// with its imported disk recorded, targeting targetNamespace.
func creatingFixture(targetNamespace string) (*infrav1beta1.VirtualMachine, *infrav1beta1.Provider, *infrav1beta1.Provider, *infrav1beta1.VMMigration) {
	sourceVM, sourceProvider, targetProvider, migration := directionFixture(
		infrav1beta1.ProviderTypeVSphere, infrav1beta1.ProviderTypeLibvirt)
	migration.Spec.Target.Namespace = targetNamespace
	migration.Spec.Target.ClassRef = &infrav1beta1.LocalObjectReference{Name: "cls"}
	migration.Status.Phase = infrav1beta1.MigrationPhaseCreating
	migration.Status.ImportID = "team-b.target-vm-migrated"
	migration.Status.DiskInfo = &infrav1beta1.MigrationDiskInfo{
		TargetPath: "/var/lib/libvirt/images/team-b.target-vm-migrated.qcow2", TargetFormat: "qcow2",
	}
	return sourceVM, sourceProvider, targetProvider, migration
}

// TestVMMigrationXNS_CreatingRefusedWithoutGrant: no VirtualMachine is created
// in a namespace that revoked the grant; with the grant it is created and its
// provider / class references are pinned to the migration's namespace (where
// they were resolved for the import).
func TestVMMigrationXNS_CreatingRefusedWithoutGrant(t *testing.T) {
	ctx := context.Background()
	sourceVM, sourceProvider, targetProvider, migration := creatingFixture(xnsTarget)
	ns := grantNamespace(xnsTarget, nil)
	r, _ := directionReconciler(t, &capturingMigrationProvider{}, sourceVM, sourceProvider, targetProvider, migration, ns,
		grantedClass("default", "cls", &metav1.LabelSelector{}))

	res, err := r.handleCreatingPhase(ctx, migration)
	require.NoError(t, err)
	assert.Equal(t, crossNamespaceRecheckInterval, res.RequeueAfter)
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseCreating, got.Status.Phase)
	assertMigrationRefused(t, got, xnsTarget)
	assertNothingInNamespace(t, r.Client, xnsTarget)

	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(ns), ns))
	ns.Annotations = map[string]string{AllowedSourceNamespacesAnnotation: "default"}
	require.NoError(t, r.Update(ctx, ns))

	_, err = r.handleCreatingPhase(ctx, getXNSMigration(t, r, migration))
	require.NoError(t, err)
	vm := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: xnsTarget, Name: "target-vm"}, vm))
	assert.Equal(t, infrav1beta1.ObjectRef{Name: "target-provider", Namespace: "default"}, vm.Spec.ProviderRef,
		"the VM must reference the Provider the disk was imported through")
	assert.Equal(t, infrav1beta1.ObjectRef{Name: "cls", Namespace: "default"}, vm.Spec.ClassRef)
	assert.Nil(t, readyCondition(getXNSMigration(t, r, migration).Status.Conditions, infrav1beta1.VMMigrationConditionReady),
		"the refusal is cleared once granted")
}

// TestVMMigrationXNS_CreatingSameNamespaceKeepsImplicitProvider: a same-
// namespace target keeps its provider reference exactly as specified.
func TestVMMigrationXNS_CreatingSameNamespaceKeepsImplicitProvider(t *testing.T) {
	ctx := context.Background()
	sourceVM, sourceProvider, targetProvider, migration := creatingFixture("")
	r, _ := directionReconciler(t, &capturingMigrationProvider{}, sourceVM, sourceProvider, targetProvider, migration)

	_, err := r.handleCreatingPhase(ctx, migration)
	require.NoError(t, err)
	vm := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "target-vm"}, vm))
	assert.Equal(t, infrav1beta1.ObjectRef{Name: "target-provider"}, vm.Spec.ProviderRef)
}

// readyTargetVM is a target VM in ns that reports Ready, as the Creating phase
// would have left it while the migration was still granted.
func readyTargetVM(ns string) *infrav1beta1.VirtualMachine {
	vm := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "target-vm", Namespace: ns,
			Annotations: map[string]string{"virtrigaud.io/migration": "default/dir-migration"},
		},
		Spec: infrav1beta1.VirtualMachineSpec{ProviderRef: infrav1beta1.ObjectRef{Name: "target-provider", Namespace: "default"}},
	}
	vm.Status.ID = "default.target-vm"
	vm.Status.Conditions = []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Now(),
	}}
	return vm
}

// TestVMMigrationXNS_ValidatingTargetRefusedWithoutGrant: after a revocation
// the migration neither reads nor annotates the (already created) target VM;
// the VM itself is kept as it is.
func TestVMMigrationXNS_ValidatingTargetRefusedWithoutGrant(t *testing.T) {
	ctx := context.Background()
	sourceVM, sourceProvider, targetProvider, migration := creatingFixture(xnsTarget)
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidatingTarget
	target := readyTargetVM(xnsTarget)
	ns := grantNamespace(xnsTarget, nil)
	r, _ := directionReconciler(t, &capturingMigrationProvider{}, sourceVM, sourceProvider, targetProvider, migration, ns, target)

	_, err := r.handleValidatingTargetPhase(ctx, migration)
	require.NoError(t, err)
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseValidatingTarget, got.Status.Phase)
	assertMigrationRefused(t, got, xnsTarget)
	vm := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(target), vm))
	assert.NotContains(t, vm.Annotations, "virtrigaud.io/migration-completed", "no write to the target VM")

	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(ns), ns))
	ns.Annotations = map[string]string{AllowedSourceNamespacesAnnotation: "default"}
	require.NoError(t, r.Update(ctx, ns))
	_, err = r.handleValidatingTargetPhase(ctx, getXNSMigration(t, r, migration))
	require.NoError(t, err)
	assert.Equal(t, infrav1beta1.MigrationPhaseReady, getXNSMigration(t, r, migration).Status.Phase)
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(target), vm))
	assert.Equal(t, "true", vm.Annotations["virtrigaud.io/migration-completed"])
}

// TestVMMigrationXNS_DeletionCleanupNeedsGrant: deleting a migration removes
// its partially created target VM in another namespace only while that
// namespace grants the migration's; otherwise the VM is left for its owners.
func TestVMMigrationXNS_DeletionCleanupNeedsGrant(t *testing.T) {
	for _, tc := range []struct {
		name        string
		grant       *string
		wantDeleted bool
	}{
		{"no grant: target VM kept", nil, false},
		{"other namespace granted: target VM kept", strPtr("team-c"), false},
		{"granted: target VM deleted", strPtr("default"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sourceVM, sourceProvider, targetProvider, migration := creatingFixture(xnsTarget)
			migration.Spec.Storage = nil
			migration.Finalizers = []string{"vmmigration.infra.virtrigaud.io/finalizer"}
			target := readyTargetVM(xnsTarget)
			r, _ := directionReconciler(t, &capturingMigrationProvider{}, sourceVM, sourceProvider, targetProvider, migration,
				grantNamespace(xnsTarget, tc.grant), target)
			require.NoError(t, r.Delete(ctx, migration))
			deleting := getXNSMigration(t, r, migration)
			require.False(t, deleting.DeletionTimestamp.IsZero())

			_, err := r.handleDeletion(ctx, deleting)
			require.NoError(t, err)

			err = r.Get(ctx, client.ObjectKeyFromObject(target), &infrav1beta1.VirtualMachine{})
			if tc.wantDeleted {
				assert.True(t, apierrors.IsNotFound(err), "the partial target VM is cleaned up")
			} else {
				assert.NoError(t, err, "the target VM is not touched without the grant")
			}
			err = r.Get(ctx, client.ObjectKeyFromObject(migration), &infrav1beta1.VMMigration{})
			assert.True(t, apierrors.IsNotFound(err), "the finalizer is released either way")
		})
	}
}

// TestVMMigrationXNS_NamespaceWatchMapsOnlyCrossNamespaceMigrations: a grant
// change re-drives exactly the unfinished migrations in OTHER namespaces that
// target the namespace.
func TestVMMigrationXNS_NamespaceWatchMapsOnlyCrossNamespaceMigrations(t *testing.T) {
	mk := func(ns, name, target string, phase infrav1beta1.MigrationPhase) *infrav1beta1.VMMigration {
		m, _ := xnsMigration(target)
		m.Namespace, m.Name = ns, name
		m.Status.Phase = phase
		return m
	}
	objs := []client.Object{
		mk(xnsSource, "validating", xnsTarget, infrav1beta1.MigrationPhaseValidating),
		mk(xnsSource, "importing", xnsTarget, infrav1beta1.MigrationPhaseImporting),
		mk(xnsSource, "failed", xnsTarget, infrav1beta1.MigrationPhaseFailed),
		mk(xnsSource, "ready", xnsTarget, infrav1beta1.MigrationPhaseReady),
		mk(xnsSource, "own-ns", "", infrav1beta1.MigrationPhaseValidating),
		mk(xnsSource, "elsewhere", "team-c", infrav1beta1.MigrationPhaseValidating),
		mk(xnsTarget, "inside-target", xnsTarget, infrav1beta1.MigrationPhaseValidating),
	}
	r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), &migrationSpy{}, objs...)

	var names []string
	for _, rq := range r.migrationsTargetingNamespace(context.Background(), grantNamespace(xnsTarget, strPtr(xnsSource))) {
		names = append(names, rq.Namespace+"/"+rq.Name)
	}
	assert.ElementsMatch(t, []string{"team-a/validating", "team-a/importing", "team-a/failed"}, names)
}

// TestVMMigrationXNS_ReadErrorFailsClosed: when the target Namespace cannot be
// read, the phase returns the error (backoff) and does nothing.
func TestVMMigrationXNS_ReadErrorFailsClosed(t *testing.T) {
	migration, objs := xnsMigration(xnsTarget)
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
	spy := &migrationSpy{}
	r, _ := newXNSMigrationReconciler(t, capGatingScheme(t), spy, append(objs, migration)...) // no corev1: reads fail

	_, err := r.handleValidatingPhase(context.Background(), migration)
	require.Error(t, err)
	assert.Equal(t, infrav1beta1.MigrationPhaseValidating, getXNSMigration(t, r, migration).Status.Phase)
	imports, exports, snapshots, power := spy.calls()
	assert.Zero(t, imports+exports+snapshots+power)
}
