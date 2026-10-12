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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

func postCleanupCondition(m *infrav1beta1.VMMigration) *metav1.Condition {
	return meta.FindStatusCondition(m.Status.Conditions, migrationConditionPostMigrationCleanup)
}

// readyDeleteAfterFixture is a Ready migration with deleteAfterMigration and a
// source snapshot to delete, its source VM (uid-src-1) and source Provider.
func readyDeleteAfterFixture() (*infrav1beta1.VirtualMachine, *infrav1beta1.Provider, *infrav1beta1.VMMigration) {
	vm, p, m := snapshotFixture()
	vm.UID = "uid-src-1"
	m.Spec.Source.DeleteAfterMigration = true
	m.Spec.Target.PowerOn = true
	m.Status.Phase = infrav1beta1.MigrationPhaseReady
	m.Finalizers = []string{vmMigrationFinalizer}
	return vm, p, m
}

// TestLegacyReadyMigrationDeletesNothing (maintainer decision D1): a migration
// that an earlier release completed (no PostMigrationCleanup marker) never
// deletes its source VM or source snapshot after the upgrade. Its cleanup is
// closed with one SourceDeletionSkippedPreUpgrade Warning and never retried.
func TestLegacyReadyMigrationDeletesNothing(t *testing.T) {
	vm, p, m := readyDeleteAfterFixture()
	prov := &scriptedSnapshotDeleteProvider{}
	h := newSnapshotHoldHarness(t, prov, vm, p, m)

	assert.Equal(t, ctrl.Result{}, h.reconcile(t, m))
	assert.Zero(t, prov.calls.Load(), "the source snapshot is not deleted")
	require.NoError(t, h.c.Get(context.Background(), client.ObjectKeyFromObject(vm), &infrav1beta1.VirtualMachine{}),
		"the source VM is not deleted")
	got := h.get(t, m)
	require.NotNil(t, got.Status.StorageInfo)
	assert.True(t, got.Status.StorageInfo.CleanedUp)
	c := postCleanupCondition(got)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, migrationReasonSourceDeletionSkippedPreUpgrade, c.Reason)
	assert.Equal(t, "snap-abc", got.Status.SnapshotID, "kept for the operator")
	assert.Len(t, filterEvents(drainRecordedEvents(h.rec), migrationReasonSourceDeletionSkippedPreUpgrade), 1)

	h.reconcile(t, got)
	assert.Empty(t, filterEvents(drainRecordedEvents(h.rec), migrationReasonSourceDeletionSkippedPreUpgrade), "closed once")
	assert.Zero(t, prov.calls.Load())
}

// TestLegacyReadyWithNothingToSkipIsClosedQuietly: no Warning when nothing
// would have been deleted.
func TestLegacyReadyWithNothingToSkipIsClosedQuietly(t *testing.T) {
	vm, p, m := readyDeleteAfterFixture()
	m.Spec.Source.DeleteAfterMigration = false
	m.Status.SnapshotID = ""
	h := newSnapshotHoldHarness(t, &scriptedSnapshotDeleteProvider{}, vm, p, m)
	h.reconcile(t, m)
	assert.Equal(t, migrationReasonSourceDeletionSkippedPreUpgrade, postCleanupCondition(h.get(t, m)).Reason)
	assert.Empty(t, filterEvents(drainRecordedEvents(h.rec), migrationReasonSourceDeletionSkippedPreUpgrade))
}

// TestStampedReadyMigrationDeletesOnlyTheExportedSourceVM: a migration this
// manager completed deletes its source snapshot and the source VM whose UID it
// recorded at Validating; another VirtualMachine under the source name (UID
// differs, or — with no UID recorded — created after the migration) is never
// deleted (SourceDeletionRefused), and the cleanup is closed either way.
func TestStampedReadyMigrationDeletesOnlyTheExportedSourceVM(t *testing.T) {
	for name, tc := range map[string]struct {
		recordedUID string
		vmCreated   time.Time
		wantDeleted bool
	}{
		"recorded UID matches":                     {recordedUID: "uid-src-1", wantDeleted: true},
		"source re-created (UID differs)":          {recordedUID: "uid-src-0", wantDeleted: false},
		"no UID recorded, VM older than migration": {vmCreated: time.Now().Add(-48 * time.Hour), wantDeleted: true},
		"no UID recorded, VM created after it":     {vmCreated: time.Now().Add(time.Hour), wantDeleted: false},
	} {
		t.Run(name, func(t *testing.T) {
			vm, p, m := readyDeleteAfterFixture()
			m.CreationTimestamp = metav1.NewTime(time.Now().Add(-24 * time.Hour))
			if !tc.vmCreated.IsZero() {
				vm.CreationTimestamp = metav1.NewTime(tc.vmCreated)
			}
			m.Status.SourceVMUID = tc.recordedUID
			markPostMigrationCleanupPending(m)
			prov := &scriptedSnapshotDeleteProvider{}
			h := newSnapshotHoldHarness(t, prov, vm, p, m)

			h.reconcile(t, m)
			assert.EqualValues(t, 1, prov.calls.Load(), "the source snapshot is deleted")
			err := h.c.Get(context.Background(), types.NamespacedName{Namespace: vm.Namespace, Name: vm.Name}, &infrav1beta1.VirtualMachine{})
			assert.Equal(t, tc.wantDeleted, apierrors.IsNotFound(err), "source VM deleted: %v", err)
			got := h.get(t, m)
			assert.True(t, got.Status.StorageInfo.CleanedUp)
			assert.Equal(t, migrationReasonPostMigrationCleanupComplete, postCleanupCondition(got).Reason)
			refused := filterEvents(drainRecordedEvents(h.rec), migrationReasonSourceDeletionRefused)
			if tc.wantDeleted {
				assert.Empty(t, refused)
			} else {
				assert.Len(t, refused, 1)
			}
		})
	}
}

// TestReadyTransitionStampsTheCleanupMarker: the status write that sets
// Phase=Ready carries the PostMigrationCleanup marker.
func TestReadyTransitionStampsTheCleanupMarker(t *testing.T) {
	sourceVM, sourceProvider, targetProvider, migration := creatingFixture("default")
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidatingTarget
	target := readyTargetVM("default")
	target.Annotations["virtrigaud.io/migration"] = migration.Namespace + "/" + migration.Name
	r, _ := directionReconciler(t, &capturingMigrationProvider{}, sourceVM, sourceProvider, targetProvider, migration, target)

	_, err := r.handleValidatingTargetPhase(context.Background(), migration)
	require.NoError(t, err)
	got := getXNSMigration(t, r, migration)
	require.Equal(t, infrav1beta1.MigrationPhaseReady, got.Status.Phase)
	c := postCleanupCondition(got)
	require.NotNil(t, c, "stamped with Phase=Ready")
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, migrationReasonPostMigrationCleanupPending, c.Reason)
}

// TestValidatingRecordsTheSourceVMUID: validation records the UID of the
// source VirtualMachine it validated.
func TestValidatingRecordsTheSourceVMUID(t *testing.T) {
	migration, objs := xnsMigration("")
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
	var srcUID types.UID
	for _, o := range objs {
		if vm, ok := o.(*infrav1beta1.VirtualMachine); ok && vm.Name == migration.Spec.Source.VMRef.Name {
			vm.UID = "uid-validated-src"
			srcUID = vm.UID
		}
	}
	require.NotEmpty(t, srcUID)
	r, _ := newXNSMigrationReconciler(t, capGatingScheme(t), &migrationSpy{}, append(objs, migration)...)
	_, err := r.handleValidatingPhase(context.Background(), migration)
	require.NoError(t, err)
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseExporting, got.Status.Phase)
	assert.Equal(t, string(srcUID), got.Status.SourceVMUID)
}
