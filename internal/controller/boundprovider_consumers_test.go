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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the VirtualMachine provider binding (status.boundProvider)
// in the controllers that bind a VM other than through VirtualMachine create —
// VMClone (target bind) and adoption — and in those that act on a VM by its id
// through a Provider they resolve from it: VMClone (source), VMMigration
// (source) and VMSnapshot.

// ─── VMClone ──────────────────────────────────────────────────────────────────

func bpClone(ns string) *infrav1beta1.VMClone {
	return &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-1", Namespace: ns},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infrav1beta1.VMCloneTarget{Name: "clone-target"},
		},
	}
}

func TestVMClone_BindRecordsBoundProvider(t *testing.T) {
	ns := "default"
	prov := runningProvider(ns, "prov-1")
	prov.UID = "uid-prov-1"
	src := sourceVMWithID(ns, "src-vm", "prov-1", "vm-source-123")
	src.Status.BoundProvider = &infrav1beta1.BoundProviderRef{Namespace: ns, Name: "prov-1", UID: "uid-prov-1"}
	clone := bpClone(ns)
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-999"}}
	r := newCloneReconciler(cloneTestScheme(t), &stubResolver{provider: cp}, prov, src, clone)

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	target := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "clone-target"}, target))
	assert.Equal(t, "vm-clone-999", target.Status.ID)
	assert.Equal(t, &infrav1beta1.BoundProviderRef{Namespace: ns, Name: "prov-1", UID: "uid-prov-1"},
		target.Status.BoundProvider, "the clone's target is bound through the Provider the clone ran on, in the same write as its id")
}

func TestVMClone_RefusesToBindATargetThatReferencesAnotherProvider(t *testing.T) {
	// The clone already ran on prov-1 (TargetVMID recorded), but a
	// VirtualMachine with the target name — referencing prov-other — appeared
	// meanwhile. The cloned VM's id is never bound to it.
	ns := "default"
	prov := runningProvider(ns, "prov-1")
	other := runningProvider(ns, "prov-other")
	src := sourceVMWithID(ns, "src-vm", "prov-1", "vm-source-123")
	clone := bpClone(ns)
	clone.Finalizers = []string{vmCloneFinalizer}
	clone.Status.Phase = infrav1beta1.ClonePhaseCloning
	clone.Status.TargetVMID = "vm-clone-999"
	squatter := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-target", Namespace: ns},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "prov-other"},
			ClassRef:    infrav1beta1.ObjectRef{Name: "c"},
		},
	}
	cp := &clonerProvider{}
	r := newCloneReconciler(cloneTestScheme(t), &stubResolver{provider: cp}, prov, other, src, clone, squatter)

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	got := &infrav1beta1.VMClone{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(clone), got))
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "references Provider default/prov-other, not the Provider the clone ran on (default/prov-1)")
	target := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "clone-target"}, target))
	assert.Empty(t, target.Status.ID, "the cloned id is not bound to a VM that references another Provider")
	assert.Nil(t, target.Status.BoundProvider)
	assert.Zero(t, cp.cloneCnt, "and no second clone is issued")
}

func TestVMClone_SourceBoundThroughAnotherProvider_NoClone(t *testing.T) {
	ns := "default"
	prov := runningProvider(ns, "prov-1")
	src := sourceVMWithID(ns, "src-vm", "prov-1", "vm-source-123")
	src.Status.BoundProvider = &infrav1beta1.BoundProviderRef{Namespace: ns, Name: "prov-0"}
	clone := bpClone(ns)
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-999"}}
	r := newCloneReconciler(cloneTestScheme(t), &stubResolver{provider: cp}, prov, src, clone)

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	assert.Zero(t, cp.cloneCnt, "the source id is never cloned through a Provider the source is not bound to")
	got := &infrav1beta1.VMClone{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(clone), got))
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	c := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	require.NotNil(t, c)
	assert.Equal(t, k8s.ReasonProviderRefMismatch, c.Reason)
}

// ─── adoption ─────────────────────────────────────────────────────────────────

func newBPAdoptionReconciler(t *testing.T, objs ...client.Object) *VMAdoptionReconciler {
	t.Helper()
	s := coverageTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&infrav1beta1.VirtualMachine{}).Build()
	return &VMAdoptionReconciler{Client: c, Scheme: s}
}

func TestAdoptVM_RecordsBoundProvider(t *testing.T) {
	ctx := context.Background()
	prov := singleProviderCR("prov-a", "infra")
	prov.UID = "uid-a"
	r := newBPAdoptionReconciler(t, prov)

	require.NoError(t, r.adoptVM(ctx, prov, contracts.VMInfo{ID: "101", Name: "legacy-db", CPU: 2, MemoryMiB: 2048}))

	vm := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "infra", Name: "legacy-db"}, vm))
	assert.Equal(t, "101", vm.Status.ID)
	assert.Equal(t, &infrav1beta1.BoundProviderRef{Namespace: "infra", Name: "prov-a", UID: "uid-a"}, vm.Status.BoundProvider)
}

func TestAdoptVM_ExistingAdoptedVMIsBoundOnlyToItsOwnProvider(t *testing.T) {
	ctx := context.Background()
	prov := singleProviderCR("prov-a", "infra")
	prov.UID = "uid-a"
	adopted := func(ref infrav1beta1.ObjectRef) *infrav1beta1.VirtualMachine {
		return &infrav1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy-db", Namespace: "infra",
				Labels: map[string]string{AdoptedLabel: AdoptedLabelValue}},
			Spec: infrav1beta1.VirtualMachineSpec{ProviderRef: ref, ClassRef: infrav1beta1.ObjectRef{Name: "c"}},
		}
	}

	t.Run("references another Provider: not bound", func(t *testing.T) {
		r := newBPAdoptionReconciler(t, prov, adopted(infrav1beta1.ObjectRef{Name: "prov-b"}))
		require.NoError(t, r.adoptVM(ctx, prov, contracts.VMInfo{ID: "101", Name: "legacy-db"}))
		vm := &infrav1beta1.VirtualMachine{}
		require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "infra", Name: "legacy-db"}, vm))
		assert.Empty(t, vm.Status.ID, "prov-a's id is never bound to a VM that references prov-b")
		assert.Nil(t, vm.Status.BoundProvider)
	})

	t.Run("references this Provider: bound through it", func(t *testing.T) {
		r := newBPAdoptionReconciler(t, prov, adopted(infrav1beta1.ObjectRef{Name: "prov-a"}))
		require.NoError(t, r.adoptVM(ctx, prov, contracts.VMInfo{ID: "101", Name: "legacy-db"}))
		vm := &infrav1beta1.VirtualMachine{}
		require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "infra", Name: "legacy-db"}, vm))
		assert.Equal(t, "101", vm.Status.ID)
		assert.Equal(t, &infrav1beta1.BoundProviderRef{Namespace: "infra", Name: "prov-a", UID: "uid-a"}, vm.Status.BoundProvider)
	})
}

func TestUnmanagedProviderVMs_CountsVMsBoundThroughTheProvider(t *testing.T) {
	prov := singleProviderCR("prov-a", "infra")
	repointed := infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a"},
		Spec:       infrav1beta1.VirtualMachineSpec{ProviderRef: infrav1beta1.ObjectRef{Name: "elsewhere"}},
		Status: infrav1beta1.VirtualMachineStatus{ID: "101",
			BoundProvider: &infrav1beta1.BoundProviderRef{Namespace: "infra", Name: "prov-a"}},
	}
	got := unmanagedProviderVMs(prov, []contracts.VMInfo{{ID: "101"}, {ID: "102"}}, []infrav1beta1.VirtualMachine{repointed})
	require.Len(t, got, 1)
	assert.Equal(t, "102", got[0].ID, "a VM bound through the Provider still owns its hypervisor VM (no second adoption)")
}

// ─── VMMigration ──────────────────────────────────────────────────────────────

// providerRecordingReconciler builds a migration reconciler whose provider
// instances are per-Provider counting fakes, recording every Provider object a
// client was resolved for.
func providerRecordingReconciler(t *testing.T, objs ...client.Object) (*VMMigrationReconciler, map[string]*countingMigrationProvider, func() []string) {
	t.Helper()
	r, _ := newRaceReconciler(t, nil, objs...)
	var mu sync.Mutex
	var resolved []string
	spies := map[string]*countingMigrationProvider{}
	r.providerInstanceFn = func(_ context.Context, p *infrav1beta1.Provider) (contracts.Provider, error) {
		mu.Lock()
		defer mu.Unlock()
		resolved = append(resolved, p.Namespace+"/"+p.Name)
		if spies[p.Name] == nil {
			spies[p.Name] = &countingMigrationProvider{}
		}
		return spies[p.Name], nil
	}
	return r, spies, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), resolved...)
	}
}

func totalExports(spies map[string]*countingMigrationProvider) int32 {
	var n int32
	for _, s := range spies {
		n += s.exportCalls.Load()
	}
	return n
}

func TestMigrationExport_UsesTheBoundProvider(t *testing.T) {
	ctx := context.Background()
	sourceVM, sourceProvider, migration := raceMigrationFixture()
	sourceProvider.UID = "uid-src"
	sourceVM.Status.BoundProvider = &infrav1beta1.BoundProviderRef{Namespace: "default", Name: "source-provider", UID: "uid-src"}
	r, spies, resolved := providerRecordingReconciler(t, sourceVM, sourceProvider, migration)

	_, err := r.handleExportingPhase(ctx, migration)
	require.NoError(t, err)

	require.NotNil(t, spies["source-provider"])
	assert.EqualValues(t, 1, spies["source-provider"].exportCalls.Load(), "the disk is exported through the bound Provider")
	assert.EqualValues(t, 1, totalExports(spies))
	assert.Equal(t, []string{"default/source-provider"}, resolved())
	assert.Equal(t, infrav1beta1.MigrationPhaseImporting, migration.Status.Phase)
}

func TestMigrationExport_RefusesASourceNotBoundThroughItsProvider(t *testing.T) {
	t.Run("spec.providerRef re-pointed at another Provider", func(t *testing.T) {
		ctx := context.Background()
		sourceVM, sourceProvider, migration := raceMigrationFixture()
		sourceVM.Spec.ProviderRef = infrav1beta1.ObjectRef{Name: "decoy"}
		sourceVM.Status.BoundProvider = &infrav1beta1.BoundProviderRef{Namespace: "default", Name: "source-provider"}
		decoy := &infrav1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "decoy", Namespace: "default"}}
		r, spies, resolved := providerRecordingReconciler(t, sourceVM, sourceProvider, decoy, migration)

		_, err := r.handleExportingPhase(ctx, migration)
		require.NoError(t, err)

		assert.Zero(t, totalExports(spies))
		assert.Empty(t, resolved(), "no Provider client is even resolved")
		assert.Equal(t, infrav1beta1.MigrationPhaseFailed, migration.Status.Phase)
		assert.Contains(t, migration.Status.Message, "is bound through Provider default/source-provider")
	})

	t.Run("the bound Provider was deleted and re-created", func(t *testing.T) {
		ctx := context.Background()
		sourceVM, sourceProvider, migration := raceMigrationFixture()
		sourceProvider.UID = "uid-recreated"
		sourceVM.Status.BoundProvider = &infrav1beta1.BoundProviderRef{Namespace: "default", Name: "source-provider", UID: "uid-src"}
		r, spies, _ := providerRecordingReconciler(t, sourceVM, sourceProvider, migration)

		res, err := r.handleExportingPhase(ctx, migration)
		require.NoError(t, err)

		assert.Zero(t, totalExports(spies), "no ExportDisk through a re-created Provider object")
		assert.Equal(t, infrav1beta1.MigrationPhaseExporting, migration.Status.Phase, "the phase does not advance")
		assert.Greater(t, res.RequeueAfter, time.Duration(0))
		assert.Contains(t, migration.Status.Message, "does not match the Provider it is bound through")
	})
}

func TestMigrationPhases_RefuseASourceBoundElsewhere(t *testing.T) {
	for _, phase := range []infrav1beta1.MigrationPhase{
		infrav1beta1.MigrationPhaseValidating,
		infrav1beta1.MigrationPhaseSnapshotting,
		infrav1beta1.MigrationPhaseExporting,
	} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			migration, objs := xnsMigration("")
			migration.Status.Phase = phase
			migration.Spec.Source.PowerOffBeforeMigration = true
			migration.Spec.Source.CreateSnapshot = true
			src, ok := objs[0].(*infrav1beta1.VirtualMachine)
			require.True(t, ok, "xnsMigration returns the source VM first")
			src.Spec.ProviderRef = infrav1beta1.ObjectRef{Name: "tgt-prov"} // re-pointed at another ready Provider
			src.Status.BoundProvider = &infrav1beta1.BoundProviderRef{Namespace: xnsSource, Name: "src-prov"}
			spy := &migrationSpy{}
			r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), spy, append(objs, migration)...)

			var err error
			switch phase {
			case infrav1beta1.MigrationPhaseValidating:
				_, err = r.handleValidatingPhase(ctx, migration)
			case infrav1beta1.MigrationPhaseSnapshotting:
				_, err = r.handleSnapshottingPhase(ctx, migration)
			case infrav1beta1.MigrationPhaseExporting:
				_, err = r.handleExportingPhase(ctx, migration)
			}
			require.NoError(t, err)

			got := getXNSMigration(t, r, migration)
			assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
			assert.Contains(t, got.Status.Message, "is bound through Provider "+xnsSource+"/src-prov")
			imports, exports, snapshots, power := spy.calls()
			assert.Zero(t, imports+exports+snapshots+power, "no provider acts on the source VM's id")
		})
	}
}

// ─── VMSnapshot ───────────────────────────────────────────────────────────────

func TestVMSnapshot_RefusesAVMBoundElsewhere(t *testing.T) {
	ns := "default"
	vm := sourceVMWithID(ns, "web", "prov-b", "vm-100")
	vm.Status.BoundProvider = &infrav1beta1.BoundProviderRef{Namespace: ns, Name: "prov-a"}
	provB := runningProvider(ns, "prov-b")
	snapshot := &infrav1beta1.VMSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns},
		Spec:       infrav1beta1.VMSnapshotSpec{VMRef: infrav1beta1.LocalObjectReference{Name: "web"}},
	}
	newR := func(objs ...client.Object) *VMSnapshotReconciler {
		s := cloneTestScheme(t)
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
			WithStatusSubresource(&infrav1beta1.VMSnapshot{}).Build()
		// No RemoteResolver: resolving a client for any Provider would fail
		// the reconcile with a ProviderError instead of the reason below.
		return &VMSnapshotReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}
	}

	t.Run("create", func(t *testing.T) {
		snap := snapshot.DeepCopy()
		r := newR(vm, provB, snap)
		res, err := r.createSnapshot(context.Background(), snap, vm)
		require.NoError(t, err)
		assert.Greater(t, res.RequeueAfter, time.Duration(0))
		c := meta.FindStatusCondition(snap.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating)
		require.NotNil(t, c)
		assert.Equal(t, k8s.ReasonProviderRefMismatch, c.Reason)
		assert.Empty(t, snap.Status.Phase, "the create is retried unchanged once resolved")
	})

	t.Run("task poll", func(t *testing.T) {
		snap := snapshot.DeepCopy()
		snap.Status.Phase = infrav1beta1.SnapshotPhaseCreating
		snap.Status.TaskRef = "task-1"
		r := newR(vm, provB, snap)
		res, err := r.checkSnapshotCreation(context.Background(), snap, vm)
		require.NoError(t, err)
		assert.Equal(t, providerRefMismatchRetryInterval, res.RequeueAfter)
		c := meta.FindStatusCondition(snap.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating)
		require.NotNil(t, c)
		assert.Equal(t, k8s.ReasonProviderRefMismatch, c.Reason)
		assert.Equal(t, infrav1beta1.SnapshotPhaseCreating, snap.Status.Phase)
	})
}
