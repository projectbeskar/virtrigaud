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
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// failingStatusWrites returns a fake client seeded with objs whose VMMigration
// status updates answer ServiceUnavailable while *failing is true.
func failingStatusWrites(t *testing.T, failing *atomic.Bool, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if failing.Load() {
					return apierrors.NewServiceUnavailable("apiserver unavailable")
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
}

// reconcileMigrationOnce runs one Reconcile of m and returns its result, the
// stored VMMigration and the reconcile error.
func reconcileMigrationOnce(t *testing.T, r *VMMigrationReconciler, m *infrav1beta1.VMMigration) (ctrl.Result, *infrav1beta1.VMMigration, error) {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)})
	got := &infrav1beta1.VMMigration{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(m), got))
	return res, got, err
}

// TestMigrationSnapshot_UnrecordedSnapshotIsRecordedNotRetaken pins that a
// snapshot whose result could not be written is recorded by the next
// reconcile instead of being taken again (an untracked snapshot on the live
// source VM), for a synchronous and a task-backed SnapshotCreate.
func TestMigrationSnapshot_UnrecordedSnapshotIsRecordedNotRetaken(t *testing.T) {
	for name, sync := range map[string]bool{"sync": true, "task": false} {
		t.Run(name, func(t *testing.T) {
			vm, p, m := snapshottingMigration()
			prov := &snapshotTaskProvider{sync: sync, status: contracts.TaskStatus{IsCompleted: true}}
			var failing atomic.Bool
			failing.Store(true)
			c := failingStatusWrites(t, &failing, vm, p, m)
			r := &VMMigrationReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(50),
				providerInstanceFn: func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return prov, nil }}

			_, got, err := reconcileMigrationOnce(t, r, m)
			require.Error(t, err, "the snapshot's result could not be written")
			require.EqualValues(t, 1, prov.creates.Load())
			require.Empty(t, got.Status.SnapshotID)

			failing.Store(false)
			res, got, err := reconcileMigrationOnce(t, r, got)
			require.NoError(t, err)
			assert.Equal(t, migrationPhaseAdvanceRequeue, res.RequeueAfter)
			assert.Equal(t, "snap-t", got.Status.SnapshotID, "the held result is recorded")
			assert.EqualValues(t, 1, prov.creates.Load(), "the snapshot is not taken again")

			// The migration continues from the recorded snapshot.
			_, got, err = reconcileMigrationOnce(t, r, got)
			require.NoError(t, err)
			assert.Equal(t, infrav1beta1.MigrationPhaseExporting, got.Status.Phase)
			assert.Equal(t, "snap-t", got.Status.SnapshotID)
			assert.EqualValues(t, 1, prov.creates.Load())
			assert.Zero(t, prov.deletes.Load(), "the recorded snapshot is not discarded")
		})
	}
}

// TestExportingPhase_UnrecordedResultIsRecordedBeforeProviderResolution pins
// that an ExportDisk result whose write failed is recorded by the next
// reconcile even when that reconcile cannot resolve the provider: the held
// result is written before anything that can fail the phase.
func TestExportingPhase_UnrecordedResultIsRecordedBeforeProviderResolution(t *testing.T) {
	prov := &countingMigrationProvider{}
	sourceVM, sourceProvider, migration := raceMigrationFixture()
	migration.Finalizers = []string{infrav1beta1.VMMigrationFinalizer}
	var failing atomic.Bool
	failing.Store(true)
	c := failingStatusWrites(t, &failing, sourceVM, sourceProvider, migration)
	resolveFails := false
	r := &VMMigrationReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(50),
		providerInstanceFn: func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
			if resolveFails {
				return nil, errors.New("provider unreachable")
			}
			return prov, nil
		}}

	_, got, err := reconcileMigrationOnce(t, r, migration)
	require.Error(t, err)
	require.EqualValues(t, 1, prov.exportCalls.Load())
	require.Empty(t, got.Status.ExportID)

	failing.Store(false)
	resolveFails = true
	res, got, err := reconcileMigrationOnce(t, r, got)
	require.NoError(t, err)
	assert.Equal(t, migrationPhaseAdvanceRequeue, res.RequeueAfter)
	assert.Equal(t, infrav1beta1.MigrationPhaseExporting, got.Status.Phase, "not failed: the result came first")
	assert.Equal(t, "export-1", got.Status.ExportID)
	require.NotNil(t, got.Status.DiskInfo)
	assert.Equal(t, "checksum-1", got.Status.DiskInfo.SourceChecksum)

	resolveFails = false
	_, got, err = reconcileMigrationOnce(t, r, got)
	require.NoError(t, err)
	assert.Equal(t, infrav1beta1.MigrationPhaseImporting, got.Status.Phase)
	assert.Equal(t, "checksum-1", got.Status.DiskInfo.SourceChecksum)
	assert.EqualValues(t, 1, prov.exportCalls.Load(), "the export is not sent again")
}

// TestRecordLongOpResult_DroppedResultIsReported pins that a result dropped
// because the live status moved on is logged and reported with a Warning
// event (constant message), and is not held for a later reconcile.
func TestRecordLongOpResult_DroppedResultIsReported(t *testing.T) {
	_, _, m := snapshotFixture()
	m.Status.Phase = infrav1beta1.MigrationPhaseImporting
	c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(m.DeepCopy()).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
	rec := record.NewFakeRecorder(5)
	r := &VMMigrationReconciler{Client: c, Recorder: rec}
	key := client.ObjectKeyFromObject(m)

	held := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), key, held))
	writeConcurrently(t, c, key)

	result := importResultApplier(contracts.ImportDiskResponse{DiskId: "disk-2"}, "qcow2")
	err := r.recordLongOpResult(context.Background(), held, longOpImport, infrav1beta1.MigrationPhaseImporting,
		result, result, "import_id", "disk-2")
	require.ErrorIs(t, err, errMigrationStatusMovedOn)

	events := drainRecordedEvents(rec)
	require.Len(t, events, 1)
	assert.True(t, strings.HasPrefix(events[0], "Warning "+migrationReasonProviderResultNotRecorded+" "))
	assert.NotContains(t, events[0], "disk-2", "the event message is constant")
	_, held2 := r.longOpInFlight.Load(longOpKey(held, longOpImport))
	assert.False(t, held2, "a dropped result is not held")
}

// TestPendingMigrationResult_BoundToItsAttempt pins that a result held for
// one attempt of a migration is never applied to a later attempt.
func TestPendingMigrationResult_BoundToItsAttempt(t *testing.T) {
	_, _, m := snapshotFixture()
	r := &VMMigrationReconciler{}
	r.longOpInFlight.Store(longOpKey(m, longOpSnapshot), pendingMigrationResult{
		retryCount: 0,
		apply:      func(m *infrav1beta1.VMMigration) { m.Status.SnapshotID = "from-attempt-0" },
	})
	m.Status.SnapshotID = ""
	m.Status.RetryCount = 1
	assert.False(t, r.applyPendingMigrationResult(m, longOpSnapshot))
	assert.Empty(t, m.Status.SnapshotID)
	_, held := r.longOpInFlight.Load(longOpKey(m, longOpSnapshot))
	assert.False(t, held, "the stale result is dropped")
}

// TestForgetMigrationLongOps pins that releasing a VMMigration drops every
// in-memory guard and held result of it, and only of it.
func TestForgetMigrationLongOps(t *testing.T) {
	_, _, m := snapshotFixture()
	other := m.DeepCopy()
	other.UID = "uid-other"
	r := &VMMigrationReconciler{}
	r.longOpInFlight.Store(longOpKey(m, longOpExport), struct{}{})
	r.longOpInFlight.Store(longOpKey(m, longOpImport), pendingMigrationResult{})
	m2 := m.DeepCopy()
	m2.Generation = 2
	r.longOpInFlight.Store(longOpKey(m2, longOpExport), struct{}{})
	r.longOpInFlight.Store(longOpKey(other, longOpExport), struct{}{})

	r.forgetMigrationLongOps(m)
	n := 0
	r.longOpInFlight.Range(func(k, _ any) bool { n++; return true })
	assert.Equal(t, 1, n)
	_, kept := r.longOpInFlight.Load(longOpKey(other, longOpExport))
	assert.True(t, kept)
}
