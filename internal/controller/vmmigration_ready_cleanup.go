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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
)

// The post-migration cleanup guard (ADR-0011, maintainer decision D1).
//
// Before this release the Ready cleanup failed first for every migration with
// spec.storage (a manager-side PVC cleanup that could not work), so the source
// snapshot was never deleted and spec.source.deleteAfterMigration never ran.
// With that fixed, a migration completed long ago would delete its source VM
// on the first reconcile after the upgrade. The controller therefore runs the
// source cleanup only for a migration it completed itself: the
// PostMigrationCleanup condition is written in the same status write as
// Phase=Ready. A Ready migration without it is closed with one Warning and
// nothing deleted.

const (
	// migrationConditionPostMigrationCleanup tracks the source cleanup of a
	// Ready migration: False (PostMigrationCleanupPending) from the status
	// write that sets Phase=Ready, True when the cleanup is finished
	// (PostMigrationCleanupComplete) or was skipped for a migration completed
	// under an older manager (SourceDeletionSkippedPreUpgrade).
	migrationConditionPostMigrationCleanup = "PostMigrationCleanup"

	// migrationReasonPostMigrationCleanupPending: Ready, cleanup to run.
	migrationReasonPostMigrationCleanupPending = "PostMigrationCleanupPending"
	// migrationReasonPostMigrationCleanupComplete: the cleanup ran.
	migrationReasonPostMigrationCleanupComplete = "PostMigrationCleanupComplete"
	// migrationReasonSourceDeletionSkippedPreUpgrade is the condition reason
	// and Warning event reason of a migration completed under an older
	// manager: its source snapshot and source VM are not deleted.
	migrationReasonSourceDeletionSkippedPreUpgrade = "SourceDeletionSkippedPreUpgrade"
	// migrationReasonSourceDeletionRefused is the Warning event reason of a
	// deleteAfterMigration that found another VirtualMachine under the source
	// name (re-created since the migration exported it): nothing is deleted.
	migrationReasonSourceDeletionRefused = "SourceDeletionRefused"
)

// Constant condition and event messages of the post-migration cleanup.
const (
	migrationPostMigrationCleanupPendingMessage = "The migration completed; its source cleanup " +
		"(source snapshot, and the source VM with spec.source.deleteAfterMigration) runs next."
	migrationPostMigrationCleanupCompleteMessage    = "The migration's source cleanup is finished."
	migrationSourceDeletionSkippedPreUpgradeMessage = "This migration completed under an earlier VirtRigaud release, whose " +
		"post-migration cleanup never ran. To avoid deleting a source VM long after the migration, the source snapshot and " +
		"the source VM (spec.source.deleteAfterMigration) are NOT deleted now: remove them yourself if they are no longer needed."
	migrationSourceDeletionRefusedMessage = "spec.source.deleteAfterMigration: the source VirtualMachine is not the one this " +
		"migration exported (it was re-created under the same name, or created after the migration), so it was not deleted."
)

// markPostMigrationCleanupPending stamps a migration that becomes Ready under
// this manager (PostMigrationCleanup=False/Pending); the caller writes it with
// Phase=Ready.
func markPostMigrationCleanupPending(m *infrav1beta1.VMMigration) {
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type:               migrationConditionPostMigrationCleanup,
		Status:             metav1.ConditionFalse,
		Reason:             migrationReasonPostMigrationCleanupPending,
		Message:            migrationPostMigrationCleanupPendingMessage,
		ObservedGeneration: m.Generation,
	})
}

// postMigrationCleanupStamped reports whether this manager completed the
// migration (the PostMigrationCleanup condition exists).
func postMigrationCleanupStamped(m *infrav1beta1.VMMigration) bool {
	return meta.FindStatusCondition(m.Status.Conditions, migrationConditionPostMigrationCleanup) != nil
}

// setPostMigrationCleanupDone sets PostMigrationCleanup=True with reason and
// message (in memory; the caller persists it).
func setPostMigrationCleanupDone(m *infrav1beta1.VMMigration, reason, message string) {
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type:               migrationConditionPostMigrationCleanup,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: m.Generation,
	})
}

// closeLegacyReadyCleanup closes the cleanup of a Ready migration that an
// older manager completed: nothing is deleted, the cleanup is recorded as
// done (status.storageInfo.cleanedUp, PostMigrationCleanup=True with reason
// SourceDeletionSkippedPreUpgrade) so it is never retried, and — when a source
// snapshot or the source VM would have been deleted — one
// SourceDeletionSkippedPreUpgrade Warning is recorded.
func (r *VMMigrationReconciler) closeLegacyReadyCleanup(ctx context.Context, m *infrav1beta1.VMMigration) error {
	skipped := m.Spec.Source.DeleteAfterMigration || migrationSnapshotCleanupPending(m)
	if m.Status.StorageInfo == nil {
		m.Status.StorageInfo = &infrav1beta1.MigrationStorageInfo{}
	}
	m.Status.StorageInfo.CleanedUp = true
	setPostMigrationCleanupDone(m, migrationReasonSourceDeletionSkippedPreUpgrade, migrationSourceDeletionSkippedPreUpgradeMessage)
	if err := r.updateStatus(ctx, m); err != nil {
		return err
	}
	logging.FromContext(ctx).Info("Migration completed under an earlier release: source cleanup skipped",
		"deleteAfterMigration", m.Spec.Source.DeleteAfterMigration, "skippedSomething", skipped)
	if skipped {
		r.Recorder.Event(m, corev1.EventTypeWarning, migrationReasonSourceDeletionSkippedPreUpgrade,
			migrationSourceDeletionSkippedPreUpgradeMessage)
	}
	return nil
}

// migrationCacheIsStale reports whether the stored migration is newer than m
// (a cache that has not caught up yet), read live through the APIReader.
func (r *VMMigrationReconciler) migrationCacheIsStale(ctx context.Context, m *infrav1beta1.VMMigration) (bool, error) {
	live := &infrav1beta1.VMMigration{}
	if err := r.liveReader().Get(ctx, client.ObjectKeyFromObject(m), live); err != nil {
		return false, fmt.Errorf("read the live VMMigration: %w", err)
	}
	return live.ResourceVersion != m.ResourceVersion, nil
}
