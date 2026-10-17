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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// conflictingStatusClient builds a fake client seeded with m whose first
// `conflicts` status updates answer Conflict; *calls counts every status
// update.
func conflictingStatusClient(t *testing.T, m *infrav1beta1.VMMigration, conflicts int, calls *int) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(capGatingScheme(t)).
		WithObjects(m).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				*calls++
				if *calls <= conflicts {
					return apierrors.NewConflict(schema.GroupResource{Group: "infra.virtrigaud.io", Resource: "vmmigrations"},
						obj.GetName(), nil)
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
}

// writeConcurrently stands in for the reconcile that wrote m's status after
// the caller read it: it records the imported disk and moves m to Creating.
func writeConcurrently(t *testing.T, c client.Client, key client.ObjectKey) {
	t.Helper()
	live := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), key, live))
	live.Status.Phase = infrav1beta1.MigrationPhaseCreating
	live.Status.ImportID = "disk-1"
	live.Status.DiskInfo = &infrav1beta1.MigrationDiskInfo{TargetDiskID: "disk-1", TargetPath: "[ds1] disk-1/disk-1.vmdk"}
	require.NoError(t, c.Status().Update(context.Background(), live))
}

// TestVMMigrationUpdateStatus_NeverOverwritesAConcurrentWrite is the rc1 lab
// race at the unit level: a reconcile that read the VMMigration before
// another reconcile recorded the imported disk must not replace that status
// with its own. Its write conflicts, the conflict is reported as status
// contention (Reconcile requeues) and the stored diskInfo survives.
func TestVMMigrationUpdateStatus_NeverOverwritesAConcurrentWrite(t *testing.T) {
	_, _, m := snapshotFixture()
	m.Status.Phase = infrav1beta1.MigrationPhaseImporting
	c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(m.DeepCopy()).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
	r := &VMMigrationReconciler{Client: c, Recorder: record.NewFakeRecorder(5)}
	key := client.ObjectKeyFromObject(m)

	stale := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), key, stale))
	writeConcurrently(t, c, key)

	markImportComplete(stale) // what the stale reconcile decided
	err := r.updateStatus(context.Background(), stale)
	require.Error(t, err)
	assert.True(t, isMigrationStatusContention(err), "a lost status write is contention, requeued: %v", err)

	got := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), key, got))
	require.NotNil(t, got.Status.DiskInfo, "the concurrent writer's diskInfo is not dropped")
	assert.Equal(t, "[ds1] disk-1/disk-1.vmdk", got.Status.DiskInfo.TargetPath)
	assert.Equal(t, "disk-1", got.Status.ImportID)
}

// TestVMMigrationUpdateStatus_NeverWritesARecreatedObject pins that a
// conflict caused by the VMMigration being re-created (another UID) is not
// retried onto the new object.
func TestVMMigrationUpdateStatus_NeverWritesARecreatedObject(t *testing.T) {
	_, _, m := snapshotFixture()
	calls := 0
	c := conflictingStatusClient(t, m.DeepCopy(), 1, &calls)
	r := &VMMigrationReconciler{Client: c, Recorder: record.NewFakeRecorder(5)}

	stale := m.DeepCopy()
	stale.UID = types.UID("uid-of-the-deleted-one")
	stale.Status.Message = "stale"
	require.Error(t, r.updateStatus(context.Background(), stale))
	assert.Equal(t, 1, calls, "nothing is written to the re-created object")

	got := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(m), got))
	assert.NotEqual(t, "stale", got.Status.Message)
}

// TestRecordMigrationResult_AppliesOnlyItsFieldsToTheLiveObject pins that an
// RPC result whose first write conflicts is applied to the live object — the
// result's fields only — so nothing another writer stored is lost and the
// result is not lost either.
func TestRecordMigrationResult_AppliesOnlyItsFieldsToTheLiveObject(t *testing.T) {
	_, _, m := snapshotFixture()
	m.Status.Phase = infrav1beta1.MigrationPhaseImporting
	m.Status.DiskInfo = &infrav1beta1.MigrationDiskInfo{SourceChecksum: "checksum-1"}
	c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(m.DeepCopy()).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
	r := &VMMigrationReconciler{Client: c, Recorder: record.NewFakeRecorder(5)}
	key := client.ObjectKeyFromObject(m)

	held := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), key, held))
	// Another write lands while the RPC runs: it records the source VM's UID.
	live := held.DeepCopy()
	live.Status.SourceVMUID = "uid-source-vm"
	require.NoError(t, c.Status().Update(context.Background(), live))

	result := importResultApplier(contracts.ImportDiskResponse{DiskId: "disk-7", Path: "/var/lib/libvirt/images/ns.web-migrated.qcow2"}, "qcow2")
	require.NoError(t, r.recordMigrationResult(context.Background(), held, infrav1beta1.MigrationPhaseImporting,
		func(m *infrav1beta1.VMMigration) { result(m); markImportComplete(m) }))

	got := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), key, got))
	assert.Equal(t, infrav1beta1.MigrationPhaseCreating, got.Status.Phase)
	assert.Equal(t, "disk-7", got.Status.ImportID)
	require.NotNil(t, got.Status.DiskInfo)
	assert.Equal(t, "/var/lib/libvirt/images/ns.web-migrated.qcow2", got.Status.DiskInfo.TargetPath)
	assert.Equal(t, "checksum-1", got.Status.DiskInfo.SourceChecksum, "the export's fields are kept")
	assert.Equal(t, "uid-source-vm", got.Status.SourceVMUID, "the concurrent write is kept")
	assert.Equal(t, got.ResourceVersion, held.ResourceVersion, "the caller holds the object as written")
	assert.Equal(t, got.Status, held.Status)
}

// TestRecordMigrationResult_NotWrittenOnceThePhaseMovedOn pins that a result
// is never applied over a live status that already left the phase the RPC was
// sent in.
func TestRecordMigrationResult_NotWrittenOnceThePhaseMovedOn(t *testing.T) {
	_, _, m := snapshotFixture()
	m.Status.Phase = infrav1beta1.MigrationPhaseImporting
	c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(m.DeepCopy()).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
	r := &VMMigrationReconciler{Client: c, Recorder: record.NewFakeRecorder(5)}
	key := client.ObjectKeyFromObject(m)

	held := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), key, held))
	writeConcurrently(t, c, key)

	err := r.recordMigrationResult(context.Background(), held, infrav1beta1.MigrationPhaseImporting,
		importResultApplier(contracts.ImportDiskResponse{DiskId: "disk-2"}, "qcow2"))
	require.Error(t, err)
	assert.True(t, isMigrationStatusContention(err))

	got := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), key, got))
	assert.Equal(t, "disk-1", got.Status.ImportID, "the stored result is not replaced")
}

// TestRecordMigrationResult_NeverWritesARecreatedObject pins that a result is
// never written to a VMMigration re-created under the same name.
func TestRecordMigrationResult_NeverWritesARecreatedObject(t *testing.T) {
	_, _, m := snapshotFixture()
	m.Status.Phase = infrav1beta1.MigrationPhaseImporting
	calls := 0
	c := conflictingStatusClient(t, m.DeepCopy(), 1, &calls)
	r := &VMMigrationReconciler{Client: c, Recorder: record.NewFakeRecorder(5)}

	stale := m.DeepCopy()
	stale.UID = types.UID("uid-of-the-deleted-one")
	err := r.recordMigrationResult(context.Background(), stale, infrav1beta1.MigrationPhaseImporting,
		importResultApplier(contracts.ImportDiskResponse{DiskId: "disk-2"}, "qcow2"))
	require.ErrorIs(t, err, errMigrationRecreated)
	assert.Equal(t, 1, calls)

	got := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(m), got))
	assert.Empty(t, got.Status.ImportID)
}

// TestImportingPhase_UnrecordedResultIsRecordedByTheNextReconcile pins that
// an ImportDisk result whose write failed is kept and recorded by the next
// reconcile of the phase — the import is not sent again and the migration
// does not advance to Creating without status.diskInfo.
func TestImportingPhase_UnrecordedResultIsRecordedByTheNextReconcile(t *testing.T) {
	ctx := context.Background()
	prov := &countingMigrationProvider{}
	targetProvider, migration := importRaceFixture()
	failing := true
	scheme := capGatingScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(targetProvider, migration).
		WithStatusSubresource(migration).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if failing && !carriesMigrationRequestMarker(obj) {
					return apierrors.NewServiceUnavailable("apiserver unavailable")
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	r := &VMMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(50),
		providerInstanceFn: func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return prov, nil }}
	key := client.ObjectKeyFromObject(migration)

	held := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(ctx, key, held))
	_, err := r.handleImportingPhase(ctx, held)
	require.Error(t, err, "the result could not be written")
	require.EqualValues(t, 1, prov.importCalls.Load())

	failing = false
	next := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(ctx, key, next))
	require.Empty(t, next.Status.ImportID, "nothing was stored")
	res, err := r.handleImportingPhase(ctx, next)
	require.NoError(t, err)
	assert.Equal(t, migrationPhaseAdvanceRequeue, res.RequeueAfter)
	assert.EqualValues(t, 1, prov.importCalls.Load(), "the import is not sent again")

	got := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(ctx, key, got))
	assert.Equal(t, "disk-1", got.Status.ImportID)
	require.NotNil(t, got.Status.DiskInfo)
	assert.Equal(t, "disk-1", got.Status.DiskInfo.TargetDiskID)
	assert.Equal(t, "checksum-1", got.Status.DiskInfo.SourceChecksum)

	// The following reconcile advances to Creating with the result in place.
	_, err = r.handleImportingPhase(ctx, got)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, key, got))
	assert.Equal(t, infrav1beta1.MigrationPhaseCreating, got.Status.Phase)
	assert.Equal(t, "disk-1", got.Status.DiskInfo.TargetDiskID)
	assert.EqualValues(t, 1, prov.importCalls.Load())
}
