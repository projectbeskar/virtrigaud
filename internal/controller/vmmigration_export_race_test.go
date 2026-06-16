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
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/util/k8s"
)

// countingMigrationProvider embeds stubProvider (defined in
// virtualmachine_controller_test.go) and counts ExportDisk / ImportDisk calls.
// Each call returns a DISTINCT checksum to mimic the real vSphere
// streamOptimized VMDK, which is not byte-deterministic: two exports of the
// same disk produce different SHA256 sums. The whole point of the race fix is
// to guarantee exactly one such call drives the staged object + recorded
// checksum, so a second call's distinct checksum can never desynchronize them.
type countingMigrationProvider struct {
	stubProvider
	exportCalls atomic.Int32
	importCalls atomic.Int32
}

// ExportDisk records the call and returns a per-call-unique checksum.
func (p *countingMigrationProvider) ExportDisk(_ context.Context, _ contracts.ExportDiskRequest) (contracts.ExportDiskResponse, error) {
	n := p.exportCalls.Add(1)
	return contracts.ExportDiskResponse{
		ExportId:           fmt.Sprintf("export-%d", n),
		EstimatedSizeBytes: 1024,
		// Distinct per call: export #1 -> checksum-1, export #2 -> checksum-2.
		Checksum: fmt.Sprintf("checksum-%d", n),
		// No TaskRef: the export completes synchronously (the bug's setting).
	}, nil
}

// ImportDisk records the call and returns a per-call-unique disk id.
func (p *countingMigrationProvider) ImportDisk(_ context.Context, _ contracts.ImportDiskRequest) (contracts.ImportDiskResponse, error) {
	n := p.importCalls.Add(1)
	return contracts.ImportDiskResponse{
		DiskId:          fmt.Sprintf("disk-%d", n),
		ActualSizeBytes: 1024,
		Checksum:        fmt.Sprintf("imported-%d", n),
		// No TaskRef: synchronous import.
	}, nil
}

var _ contracts.Provider = (*countingMigrationProvider)(nil)

// newRaceReconciler builds a VMMigrationReconciler whose provider resolution is
// pinned to the supplied fake, with a fake client seeded with the supplied
// objects and status subresource support.
func newRaceReconciler(t *testing.T, prov contracts.Provider, objs ...client.Object) (*VMMigrationReconciler, client.Client) {
	t.Helper()
	scheme := capGatingScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(objs...).
		Build()

	r := &VMMigrationReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(50),
		providerInstanceFn: func(_ context.Context, _ *infrav1beta1.Provider) (contracts.Provider, error) {
			return prov, nil
		},
	}
	return r, c
}

// raceMigrationFixture returns a source VM, a source Provider, and a VMMigration
// staged in the Exporting phase with a PVC backend already provisioned (so
// generateStorageURL succeeds). The migration is ready to drive
// handleExportingPhase straight into the ExportDisk RPC.
func raceMigrationFixture() (*infrav1beta1.VirtualMachine, *infrav1beta1.Provider, *infrav1beta1.VMMigration) {
	sourceVM := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "source-vm", Namespace: "default"},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "source-provider"},
		},
	}
	sourceVM.Status.ID = "vm-123"

	sourceProvider := &infrav1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "source-provider", Namespace: "default"},
	}

	migration := &infrav1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "race-migration",
			Namespace:  "default",
			UID:        "uid-race-1",
			Generation: 1,
		},
		Spec: infrav1beta1.VMMigrationSpec{
			Source: infrav1beta1.MigrationSource{
				VMRef: infrav1beta1.LocalObjectReference{Name: "source-vm"},
			},
			Target: infrav1beta1.MigrationTarget{
				Name:        "target-vm",
				ProviderRef: infrav1beta1.ObjectRef{Name: "target-provider"},
			},
			Storage: &infrav1beta1.MigrationStorage{
				Type: "pvc",
				PVC:  &infrav1beta1.PVCStorageConfig{Name: "mig-pvc"},
			},
		},
	}
	migration.Status.Phase = infrav1beta1.MigrationPhaseExporting
	migration.Status.StoragePVCName = "mig-pvc"
	return sourceVM, sourceProvider, migration
}

// TestExportingPhase_SingleExportAcrossStaleReReconcile is the core regression
// test for the duplicate-ExportDisk race. The real bug: handleExportingPhase
// runs a ~16-minute synchronous ExportDisk, advances Status to Importing, then
// returned ctrl.Result{Requeue: true}. The immediate requeue re-entered before
// the status write propagated to the informer cache, so the guard read stale
// Phase=Exporting/ExportID="" and called ExportDisk a SECOND time, overwriting
// the staged (non-deterministic) object while its checksum was lost to a
// conflicting status update -> import checksum mismatch -> migration fails.
//
// This test proves: (1) the first pass returns WITHOUT an immediate requeue,
// and (2) even when the next reconcile sees a stale cache (Phase reset to
// Exporting, ExportID cleared), the in-memory guard suppresses the duplicate
// RPC, so ExportDisk is invoked exactly once and the migration still advances
// to Importing with the original export's checksum intact.
func TestExportingPhase_SingleExportAcrossStaleReReconcile(t *testing.T) {
	ctx := context.Background()
	prov := &countingMigrationProvider{}
	sourceVM, sourceProvider, migration := raceMigrationFixture()
	r, _ := newRaceReconciler(t, prov, sourceVM, sourceProvider, migration)

	// First reconcile of the Exporting phase: issues ExportDisk synchronously,
	// records the checksum, advances to Importing.
	res, err := r.handleExportingPhase(ctx, migration)
	require.NoError(t, err)
	require.EqualValues(t, 1, prov.exportCalls.Load(), "first pass must issue exactly one ExportDisk")

	// The fix: no immediate requeue after the status write. The status-update
	// watch event re-drives reconcile with a fresh cache instead.
	assert.False(t, res.Requeue, "must NOT immediate-requeue after a long synchronous export")
	assert.Zero(t, res.RequeueAfter, "must not schedule a timed requeue either")

	// Status advanced correctly and the recorded checksum is export #1's.
	assert.Equal(t, infrav1beta1.MigrationPhaseImporting, migration.Status.Phase)
	assert.Equal(t, "export-1", migration.Status.ExportID)
	require.NotNil(t, migration.Status.DiskInfo)
	assert.Equal(t, "checksum-1", migration.Status.DiskInfo.SourceChecksum)

	// Simulate the racing re-reconcile reading a STALE informer cache: as the
	// real bug observed, the next reconcile still saw Phase=Exporting and an
	// empty ExportID even though the persisted object had advanced. Clear both
	// to reproduce that exact stale view. The in-memory guard (set during the
	// first pass) and/or the durable Exporting condition must prevent a second
	// ExportDisk.
	stale := migration.DeepCopy()
	stale.Status.Phase = infrav1beta1.MigrationPhaseExporting
	stale.Status.ExportID = ""

	res2, err := r.handleExportingPhase(ctx, stale)
	require.NoError(t, err)

	// The decisive assertion: NO second export.
	assert.EqualValues(t, 1, prov.exportCalls.Load(),
		"stale-cache re-reconcile must NOT issue a duplicate ExportDisk")

	// And the stale re-entry still advances to Importing (it took the
	// already-exported branch) rather than getting stuck in Exporting.
	assert.Equal(t, infrav1beta1.MigrationPhaseImporting, stale.Status.Phase)
	assert.False(t, res2.Requeue, "already-exported branch must not immediate-requeue")
}

// TestExportingPhase_GuardSurvivesConditionOnlyStaleView covers the case where
// the in-memory guard is the ONLY thing standing between a stale cache and a
// duplicate export: the durable ExportID AND the Exporting condition both read
// empty/absent (e.g. the status write that set them has not yet landed in the
// cache), but the export RPC already ran this process-lifetime.
func TestExportingPhase_GuardSurvivesConditionOnlyStaleView(t *testing.T) {
	ctx := context.Background()
	prov := &countingMigrationProvider{}
	sourceVM, sourceProvider, migration := raceMigrationFixture()
	r, _ := newRaceReconciler(t, prov, sourceVM, sourceProvider, migration)

	// First pass exports once.
	_, err := r.handleExportingPhase(ctx, migration)
	require.NoError(t, err)
	require.EqualValues(t, 1, prov.exportCalls.Load())

	// Worst-case stale view: ExportID empty, Exporting condition absent, phase
	// still Exporting. Only the in-memory guard remains. Same UID+generation,
	// so the guard key matches.
	stale := migration.DeepCopy()
	stale.Status.Phase = infrav1beta1.MigrationPhaseExporting
	stale.Status.ExportID = ""
	stale.Status.Conditions = nil
	require.False(t, k8s.IsConditionTrue(stale.Status.Conditions, infrav1beta1.VMMigrationConditionExporting))

	_, err = r.handleExportingPhase(ctx, stale)
	require.NoError(t, err)
	assert.EqualValues(t, 1, prov.exportCalls.Load(),
		"in-memory guard alone must suppress the duplicate ExportDisk")
}

// TestExportingPhase_DurableConditionGuardWithoutInMemoryState models a manager
// RESTART: the in-memory guard is empty (fresh process), and the cache is
// briefly stale on ExportID, but the durable Exporting=True condition persisted
// from before the restart. That condition alone must route the reconcile to the
// already-exported branch instead of re-issuing ExportDisk.
func TestExportingPhase_DurableConditionGuardWithoutInMemoryState(t *testing.T) {
	ctx := context.Background()
	prov := &countingMigrationProvider{}
	sourceVM, sourceProvider, migration := raceMigrationFixture()
	// Fresh reconciler => empty longOpInFlight (as after a manager restart).
	r, _ := newRaceReconciler(t, prov, sourceVM, sourceProvider, migration)

	// Persisted state that survived the restart: Exporting condition True, but
	// ExportID not yet visible in the (stale) cache.
	migration.Status.ExportID = ""
	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionExporting,
		metav1.ConditionTrue, "ExportComplete", "Source VM disk exported")

	res, err := r.handleExportingPhase(ctx, migration)
	require.NoError(t, err)
	assert.EqualValues(t, 0, prov.exportCalls.Load(),
		"durable Exporting=True condition must prevent ExportDisk after a restart")
	assert.Equal(t, infrav1beta1.MigrationPhaseImporting, migration.Status.Phase)
	assert.False(t, res.Requeue)
}

// importRaceFixture stages a migration in the Importing phase with the source
// checksum already recorded (as it would be after a successful export), plus a
// target Provider. Ready to drive handleImportingPhase into ImportDisk.
func importRaceFixture() (*infrav1beta1.Provider, *infrav1beta1.VMMigration) {
	targetProvider := &infrav1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "target-provider", Namespace: "default"},
	}
	migration := &infrav1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "race-migration-import",
			Namespace:  "default",
			UID:        "uid-race-2",
			Generation: 1,
		},
		Spec: infrav1beta1.VMMigrationSpec{
			Source: infrav1beta1.MigrationSource{
				VMRef: infrav1beta1.LocalObjectReference{Name: "source-vm"},
			},
			Target: infrav1beta1.MigrationTarget{
				Name:        "target-vm",
				ProviderRef: infrav1beta1.ObjectRef{Name: "target-provider"},
			},
			Storage: &infrav1beta1.MigrationStorage{
				Type: "pvc",
				PVC:  &infrav1beta1.PVCStorageConfig{Name: "mig-pvc"},
			},
		},
	}
	migration.Status.Phase = infrav1beta1.MigrationPhaseImporting
	migration.Status.StoragePVCName = "mig-pvc"
	migration.Status.DiskInfo = &infrav1beta1.MigrationDiskInfo{SourceChecksum: "checksum-1"}
	return targetProvider, migration
}

// TestImportingPhase_SingleImportAcrossStaleReReconcile is the import-side
// counterpart of the export race test. ImportDisk is also synchronous and
// long-running and it WRITES the target qcow2; a duplicate driven by a stale
// cache would overwrite it. The same guard + no-immediate-requeue fix applies.
func TestImportingPhase_SingleImportAcrossStaleReReconcile(t *testing.T) {
	ctx := context.Background()
	prov := &countingMigrationProvider{}
	targetProvider, migration := importRaceFixture()
	r, _ := newRaceReconciler(t, prov, targetProvider, migration)

	res, err := r.handleImportingPhase(ctx, migration)
	require.NoError(t, err)
	require.EqualValues(t, 1, prov.importCalls.Load(), "first pass must issue exactly one ImportDisk")
	assert.False(t, res.Requeue, "must NOT immediate-requeue after a long synchronous import")
	assert.Zero(t, res.RequeueAfter)
	assert.Equal(t, infrav1beta1.MigrationPhaseCreating, migration.Status.Phase)
	assert.Equal(t, "disk-1", migration.Status.ImportID)

	// Stale re-entry: cache still shows Phase=Importing, ImportID="".
	stale := migration.DeepCopy()
	stale.Status.Phase = infrav1beta1.MigrationPhaseImporting
	stale.Status.ImportID = ""

	res2, err := r.handleImportingPhase(ctx, stale)
	require.NoError(t, err)
	assert.EqualValues(t, 1, prov.importCalls.Load(),
		"stale-cache re-reconcile must NOT issue a duplicate ImportDisk")
	assert.Equal(t, infrav1beta1.MigrationPhaseCreating, stale.Status.Phase)
	assert.False(t, res2.Requeue)
}

// TestReconcile_ExportingPhase_NoImmediateRequeue drives the full Reconcile
// entry point (not just the phase handler) to confirm the synchronous-export
// path returns a non-requeueing result end-to-end, so the watch — not an
// immediate requeue — re-drives the next phase.
func TestReconcile_ExportingPhase_NoImmediateRequeue(t *testing.T) {
	ctx := context.Background()
	prov := &countingMigrationProvider{}
	sourceVM, sourceProvider, migration := raceMigrationFixture()
	// Reconcile requires the finalizer to already be present, else it short
	// circuits on finalizer-add. Seed it.
	migration.Finalizers = []string{"vmmigration.infra.virtrigaud.io/finalizer"}
	r, c := newRaceReconciler(t, prov, sourceVM, sourceProvider, migration)

	req := ctrl.Request{NamespacedName: types.NamespacedName{
		Name:      migration.Name,
		Namespace: migration.Namespace,
	}}

	res, err := r.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.EqualValues(t, 1, prov.exportCalls.Load())
	assert.False(t, res.Requeue, "Reconcile must not immediate-requeue after the synchronous export")

	persisted := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(ctx, req.NamespacedName, persisted))
	assert.Equal(t, infrav1beta1.MigrationPhaseImporting, persisted.Status.Phase)
	assert.Equal(t, "checksum-1", persisted.Status.DiskInfo.SourceChecksum)
}
