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
	stderrors "errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
)

// The VMMigration finalizer (infrav1beta1.VMMigrationFinalizer) is released
// only once the migration-created source snapshot is deleted (or provably
// gone), or the force-delete annotation says to leave it behind.

const (
	// migrationConditionSnapshotCleanup is the VMMigration condition recording
	// that the source snapshot the migration created could not be deleted yet
	// (False, reason migrationReasonSnapshotCleanupPending). It is removed
	// once the snapshot is deleted. Its LastTransitionTime is when the hold
	// began, from which its retries back off (blockedRetryBackoff).
	migrationConditionSnapshotCleanup = "SnapshotCleanup"

	// migrationReasonSnapshotCleanupPending is the SnapshotCleanup reason and
	// the Warning event reason of a source-snapshot delete that is held and
	// retried.
	migrationReasonSnapshotCleanupPending = "SnapshotCleanupPending"

	// migrationReasonSourceSnapshotDeleted is the Normal event reason recorded
	// when a terminally failed migration deleted its source snapshot.
	migrationReasonSourceSnapshotDeleted = "SourceSnapshotDeleted"

	// migrationSnapshotCleanupLogMessage is the manager log line of every
	// held source-snapshot delete; it carries the cause, which the status
	// and events never do.
	migrationSnapshotCleanupLogMessage = "Cannot delete the migration's source snapshot yet; retrying with a backoff"
)

// migrationSnapshotCleanupPendingMessage is the SnapshotCleanup condition
// message and Warning event message of a held source-snapshot delete. It is
// constant — no cause, provider or resolver text, delay or timestamp — so a
// repeated hold writes nothing and its backoff, not its own status writes,
// paces the retries.
var migrationSnapshotCleanupPendingMessage = fmt.Sprintf(
	"The source snapshot this migration created could not be deleted yet and may still be on the source VM; its delete is "+
		"retried with a backoff of up to %s, and a deleting VMMigration is kept until it succeeds. Set %s=true on the "+
		"VMMigration to remove it anyway (the snapshot is then left on the hypervisor). The cause is in the manager log.",
	blockedRetryMax, forceDeleteAnnotation)

// migrationSnapshotCleanupPending reports whether the migration still records
// a source snapshot that it created itself (not a user-supplied
// spec.source.snapshotRef) and may remove (CleanupPolicy is not Never).
func migrationSnapshotCleanupPending(m *infrav1beta1.VMMigration) bool {
	return cleanupAllowed(m) && m.Status.SnapshotID != "" && m.Spec.Source.SnapshotRef == nil
}

// hasMigrationForceDeleteAnnotation reports whether the VMMigration carries
// the force-delete escape hatch (forceDeleteAnnotation set to "true"): a
// deletion whose source-snapshot delete cannot succeed then releases the
// finalizer, leaving the snapshot on the hypervisor (a
// SnapshotLeftOnHypervisor Warning names it).
func hasMigrationForceDeleteAnnotation(m *infrav1beta1.VMMigration) bool {
	return m.Annotations[forceDeleteAnnotation] == "true"
}

// migrationTerminallyFailed reports whether the migration is Failed and will
// not be retried: it has no retry policy, or its retries are used up
// (spec.options.retryPolicy.maxRetries, default 3).
func migrationTerminallyFailed(m *infrav1beta1.VMMigration) bool {
	if m.Status.Phase != infrav1beta1.MigrationPhaseFailed {
		return false
	}
	if m.Spec.Options == nil || m.Spec.Options.RetryPolicy == nil {
		return true
	}
	maxRetries := int32(3)
	if m.Spec.Options.RetryPolicy.MaxRetries != nil {
		maxRetries = *m.Spec.Options.RetryPolicy.MaxRetries
	}
	return m.Status.RetryCount >= maxRetries
}

// snapshotCleanupHeldSince is when the current source-snapshot cleanup hold
// began (the SnapshotCleanup=False transition), or zero when none is held.
func snapshotCleanupHeldSince(m *infrav1beta1.VMMigration) time.Time {
	c := meta.FindStatusCondition(m.Status.Conditions, migrationConditionSnapshotCleanup)
	if c == nil || c.Status != metav1.ConditionFalse {
		return time.Time{}
	}
	return c.LastTransitionTime.Time
}

// snapshotCleanupNotDue reports whether a held source-snapshot delete was
// recorded less than blockedRetryMin ago, and how long until it is due. The
// VMMigration's own watch reconciles it again on the status write that
// records a hold; that reconcile must not re-send the delete ahead of the
// backoff.
func snapshotCleanupNotDue(m *infrav1beta1.VMMigration, now time.Time) (time.Duration, bool) {
	since := snapshotCleanupHeldSince(m)
	if since.IsZero() {
		return 0, false
	}
	if elapsed := now.Sub(since); elapsed < blockedRetryMin {
		return blockedRetryMin - elapsed, true
	}
	return 0, false
}

// handleTerminalFailure reconciles a migration that failed for good
// (migrationTerminallyFailed). Its only remaining work is cleanup. The staged
// disk of an nfs / s3 migration is deleted when cleanupPolicy is Always
// (reconcileStagingCleanup, ADR-0011 D3; with the default OnSuccess it is kept
// until the VMMigration is deleted); so is the disk its import landed on the
// target provider when no VirtualMachine uses it (reconcileLandedDiskCleanup,
// rc2 R7). The source snapshot it created is
// deleted (deleteSourceSnapshot) — on the LIVE source VM it would otherwise
// stay until the VMMigration is deleted, and on libvirt its metadata blocks
// deleting the source VM. A delete that cannot be made or fails is held
// (holdStagingCleanup / holdSourceSnapshotCleanup) and retried with the
// blocked-VM backoff; nothing else is reconciled.
func (r *VMMigrationReconciler) handleTerminalFailure(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	stagingRetry, err := r.reconcileStagingCleanup(ctx, migration, stagingTriggerTerminalFailure)
	if err != nil {
		return ctrl.Result{}, err
	}
	// A clustered target created before the import and still unbound is
	// deleted first (ADR-0007 Addendum B, B1.4); the landed disk is cleaned
	// up only once it is gone.
	gone, err := r.deleteUnboundTarget(ctx, migration)
	if err != nil {
		return ctrl.Result{}, err
	}
	landedRetry := unboundTargetDeleteRequeue
	if gone {
		if landedRetry, err = r.reconcileLandedDiskCleanup(ctx, migration, stagingTriggerTerminalFailure); err != nil {
			return ctrl.Result{}, err
		}
	}
	res, err := r.terminalSnapshotCleanup(ctx, migration)
	return stagingCleanupRequeue(stagingCleanupRequeue(res, stagingRetry), landedRetry), err
}

// terminalSnapshotCleanup deletes the source snapshot a terminally failed
// migration created, holding a delete that cannot be made or fails.
func (r *VMMigrationReconciler) terminalSnapshotCleanup(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	if !migrationSnapshotCleanupPending(migration) {
		logger.V(1).Info("Migration permanently failed, skipping reconciliation",
			"retries", migration.Status.RetryCount)
		return ctrl.Result{}, nil
	}
	if wait, notDue := snapshotCleanupNotDue(migration, time.Now()); notDue {
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	snapshotID := migration.Status.SnapshotID
	if err := r.deleteSourceSnapshot(ctx, migration); err != nil {
		return r.holdSourceSnapshotCleanup(ctx, migration, err)
	}
	meta.RemoveStatusCondition(&migration.Status.Conditions, migrationConditionSnapshotCleanup)
	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("Source snapshot deleted on terminal failure", "snapshot_id", snapshotID)
	r.Recorder.Event(migration, corev1.EventTypeNormal, migrationReasonSourceSnapshotDeleted,
		"The source snapshot this failed migration created was deleted (or was already gone on the provider)")
	return ctrl.Result{}, nil
}

// releaseSourceSnapshotForDeletion runs the source-snapshot step of a
// VMMigration's deletion. hold == true means the finalizer is kept and the
// caller returns (res, err): the snapshot may still be on the source VM and
// its delete can still succeed (a transient or provider-unavailable error, a
// Provider this namespace may not use, a VM with no provider call possible).
// hold == false lets the deletion proceed: nothing to delete, the snapshot is
// deleted or provably gone (its source VM, its Provider in the migration's
// own namespace, or the snapshot itself is not found), or force-delete says
// to leave it behind (with a SnapshotLeftOnHypervisor Warning).
func (r *VMMigrationReconciler) releaseSourceSnapshotForDeletion(ctx context.Context, migration *infrav1beta1.VMMigration) (bool, ctrl.Result, error) {
	if !migrationSnapshotCleanupPending(migration) {
		return false, ctrl.Result{}, nil
	}
	// force-delete is honoured unless the source VM is a consumer, in another
	// namespace, of a clustered Provider that has not allowed it
	// (migrationForceDeleteHonoured).
	force, err := r.migrationForceDeleteHonoured(ctx, migration)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	if wait, notDue := snapshotCleanupNotDue(migration, time.Now()); notDue {
		if !force {
			return true, ctrl.Result{RequeueAfter: wait}, nil
		}
		// Just tried and failed: force-delete is acted on at once.
		r.recordSourceSnapshotLeft(ctx, migration, migration.Status.SnapshotID, nil)
		return false, ctrl.Result{}, nil
	}

	snapshotID := migration.Status.SnapshotID
	if err := r.deleteSourceSnapshot(ctx, migration); err != nil {
		if force {
			r.recordSourceSnapshotLeft(ctx, migration, snapshotID, err)
			return false, ctrl.Result{}, nil
		}
		if stderrors.Is(err, errSourceSnapshotUnaddressable) {
			// Nothing can address the snapshot any more (its VM or Provider
			// is gone): holding would wait forever. Released, never reported
			// deleted.
			r.recordSourceSnapshotUnaddressable(ctx, migration, snapshotID, err)
			return false, ctrl.Result{}, nil
		}
		res, herr := r.holdSourceSnapshotCleanup(ctx, migration, err)
		return true, res, herr
	}
	logging.FromContext(ctx).Info("Source snapshot cleaned up during deletion", "snapshot_id", snapshotID)
	return false, ctrl.Result{}, nil
}

// holdSourceSnapshotCleanup records a held source-snapshot delete —
// SnapshotCleanup=False/SnapshotCleanupPending with the constant
// migrationSnapshotCleanupPendingMessage, and a Warning event — only when
// that changes the stored status, and retries with the blocked-VM backoff
// (blockedRetryBackoff: 15 s doubling to 5 min, from when the hold began).
// cause goes to the manager log only: it can carry resolver detail (a TLS
// Secret name, a Service address) that a namespace's users must not read.
func (r *VMMigrationReconciler) holdSourceSnapshotCleanup(ctx context.Context, migration *infrav1beta1.VMMigration, cause error) (ctrl.Result, error) {
	retry := blockedRetryBackoff(snapshotCleanupHeldSince(migration))
	logging.FromContext(ctx).Info(migrationSnapshotCleanupLogMessage,
		"snapshot_id", migration.Status.SnapshotID, "retryAfter", retry.String(), "error", cause.Error())

	before := migration.Status.DeepCopy()
	meta.SetStatusCondition(&migration.Status.Conditions, metav1.Condition{
		Type:               migrationConditionSnapshotCleanup,
		Status:             metav1.ConditionFalse,
		Reason:             migrationReasonSnapshotCleanupPending,
		Message:            migrationSnapshotCleanupPendingMessage,
		ObservedGeneration: migration.Generation,
	})
	if !equality.Semantic.DeepEqual(before, &migration.Status) {
		if c := meta.FindStatusCondition(before.Conditions, migrationConditionSnapshotCleanup); c == nil || c.Status != metav1.ConditionFalse {
			r.Recorder.Event(migration, corev1.EventTypeWarning, migrationReasonSnapshotCleanupPending, migrationSnapshotCleanupPendingMessage)
		}
		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: retry}, nil
}

// recordSourceSnapshotUnaddressable records that a VMMigration is removed
// although its source snapshot was not deleted because nothing can address it
// any more (errSourceSnapshotUnaddressable): a SnapshotLeftOnHypervisor
// Warning naming the snapshot and the source VM, and the cause in the log.
func (r *VMMigrationReconciler) recordSourceSnapshotUnaddressable(ctx context.Context, migration *infrav1beta1.VMMigration, snapshotID string, cause error) {
	msg := fmt.Sprintf("Removing the VMMigration although its source snapshot %q of VM %s/%s was not deleted: the source VM or "+
		"its Provider no longer exists, so nothing can delete it; if the VM still exists on the hypervisor, the snapshot is left there",
		snapshotID, migration.Namespace, migration.Spec.Source.VMRef.Name)
	logging.FromContext(ctx).Info("Source snapshot can no longer be addressed; removing finalizer, it may be left on the hypervisor",
		"snapshot_id", snapshotID, "error", cause.Error())
	r.Recorder.Event(migration, corev1.EventTypeWarning, eventReasonSnapshotLeftOnHypervisor, msg)
}

// recordSourceSnapshotLeft records that force-delete removes the VMMigration
// although its source snapshot was not deleted: a SnapshotLeftOnHypervisor
// Warning naming the snapshot and the source VM, and the cause in the log.
func (r *VMMigrationReconciler) recordSourceSnapshotLeft(ctx context.Context, migration *infrav1beta1.VMMigration, snapshotID string, cause error) {
	msg := fmt.Sprintf("%s=true: removing the VMMigration although its source snapshot %q of VM %s/%s was not deleted; "+
		"it is left on the hypervisor", forceDeleteAnnotation, snapshotID, migration.Namespace, migration.Spec.Source.VMRef.Name)
	causeText := ""
	if cause != nil {
		causeText = cause.Error()
	}
	logging.FromContext(ctx).Info("Source snapshot not deleted but force-delete annotation is set; removing finalizer, the snapshot is left on the hypervisor",
		"snapshot_id", snapshotID, "annotation", forceDeleteAnnotation, "error", causeText)
	r.Recorder.Event(migration, corev1.EventTypeWarning, eventReasonSnapshotLeftOnHypervisor, msg)
}
