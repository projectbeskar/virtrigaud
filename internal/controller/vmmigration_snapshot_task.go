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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/util/k8s"
)

// The migration's source-snapshot step: a task-backed SnapshotCreate is
// polled to completion before the export may start, and a snapshot is reused
// (e.g. by a retried migration) only once it was confirmed complete.

const (
	// migrationReasonSnapshotComplete is the Snapshotting condition reason
	// (status True) of a migration-created source snapshot confirmed
	// complete: created synchronously, or its task finished without error.
	// Only such a snapshot is exported or reused by a retry.
	migrationReasonSnapshotComplete = "SnapshotComplete"
	// migrationReasonSnapshotInProgress is the Snapshotting condition reason
	// (status True) while the snapshot task is polled.
	migrationReasonSnapshotInProgress = "SnapshotInProgress"
	// migrationReasonSnapshotFailed is the Snapshotting condition reason
	// (status False) of a snapshot whose task failed or whose outcome could
	// not be learned; such a snapshot is never exported.
	migrationReasonSnapshotFailed = "SnapshotFailed"
	// migrationReasonSnapshotDiscarded is the Snapshotting condition reason
	// (status False) and event reason recorded when an unconfirmed snapshot
	// is removed so a new one can be taken.
	migrationReasonSnapshotDiscarded = "SnapshotDiscarded"

	// migrationSnapshotTaskPollInterval is how often a running snapshot task
	// is polled.
	migrationSnapshotTaskPollInterval = 10 * time.Second
	// migrationPhaseAdvanceRequeue is the follow-up of a status write that
	// advances the migration's phase. The write itself re-triggers the
	// migration through its watch; this explicit requeue only backs it up
	// and is never immediate, so it cannot act on a cache that does not
	// show the write yet.
	migrationPhaseAdvanceRequeue = 2 * time.Second
	// migrationStaleReadRetryInterval is the retry of a snapshot create
	// skipped because the cached VMMigration was not the live one
	// (confirmMigrationSnapshotNotTaken).
	migrationStaleReadRetryInterval = 2 * time.Second
)

// migrationDefaultSnapshotWait bounds the wait for a source snapshot task when
// the migration sets no spec.options.timeout.
const migrationDefaultSnapshotWait = 2 * time.Hour

// migrationSnapshotWaitLimit is the longest a migration waits for its source
// snapshot task: spec.options.timeout when set, else
// migrationDefaultSnapshotWait.
func migrationSnapshotWaitLimit(m *infrav1beta1.VMMigration) time.Duration {
	if m.Spec.Options != nil && m.Spec.Options.Timeout != nil && m.Spec.Options.Timeout.Duration > 0 {
		return m.Spec.Options.Timeout.Duration
	}
	return migrationDefaultSnapshotWait
}

// migrationSnapshotWaitExceeded reports whether the snapshot task has run for
// longer than migrationSnapshotWaitLimit, counted from when the migration
// started waiting for it (the Snapshotting=True/SnapshotInProgress
// transition), and for how long it has run.
func migrationSnapshotWaitExceeded(m *infrav1beta1.VMMigration, now time.Time) (time.Duration, bool) {
	c := meta.FindStatusCondition(m.Status.Conditions, infrav1beta1.VMMigrationConditionSnapshotting)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != migrationReasonSnapshotInProgress {
		return 0, false
	}
	waited := now.Sub(c.LastTransitionTime.Time)
	return waited, waited > migrationSnapshotWaitLimit(m)
}

// migrationTaskFailedDetail is what the VMMigration's consumer-readable
// status says of a provider task (snapshot, export, import) that ended with
// an error. The task's error text is free-form provider output and goes to
// the manager log only.
const migrationTaskFailedDetail = "the provider's task failed (its answer is in the manager log)"

// migrationSnapshotInProgressMessage is the Snapshotting condition message
// while the snapshot task runs. It is constant, so a poll writes nothing.
const migrationSnapshotInProgressMessage = "The source VM snapshot is being taken; the export starts once its task completes"

// Snapshot name length bounds (migrationSnapshotIdentity).
const (
	// migrationSnapshotNameMax is the smallest snapshot name limit among the
	// providers: Proxmox's 40 characters.
	migrationSnapshotNameMax = 40
	// migrationSnapshotAttemptSuffixMax is the longest attempt suffix,
	// "-r99" (retryPolicy.maxRetries is at most 10).
	migrationSnapshotAttemptSuffixMax = len("-r99")
)

// migrationSnapshotIdentity is the name hint and request token of the source
// snapshot this attempt of the migration takes: "<vm>-migration-<uid8>" and
// the migration's UID for the first attempt (unchanged), with "-r<n>"
// appended on retry n. A retry after an unconfirmed snapshot was forgotten
// (cleanupPolicy Never leaves it on the source VM) therefore never collides
// with that snapshot's name, and a provider that adopts by request token never
// adopts it.
//
// The name hint always fits migrationSnapshotNameMax, the smallest snapshot
// name limit among the providers (Proxmox: 40 characters; libvirt sanitises
// to 64): the VM part is cut, deterministically, to leave room for
// "-migration-<uid8>" and the longest attempt suffix.
func migrationSnapshotIdentity(m *infrav1beta1.VMMigration) (nameHint, requestToken string) {
	uid := string(m.UID)
	short := uid
	if len(short) > 8 {
		short = short[:8]
	}
	vmPart := m.Spec.Source.VMRef.Name
	if room := migrationSnapshotNameMax - len("-migration-") - len(short) - migrationSnapshotAttemptSuffixMax; len(vmPart) > room {
		vmPart = vmPart[:room]
	}
	nameHint = fmt.Sprintf("%s-migration-%s", vmPart, short)
	requestToken = uid
	if m.Status.RetryCount > 0 {
		nameHint = fmt.Sprintf("%s-r%d", nameHint, m.Status.RetryCount)
		requestToken = fmt.Sprintf("%s-r%d", uid, m.Status.RetryCount)
	}
	return nameHint, requestToken
}

// migrationSnapshotConfirmed reports whether the migration's recorded source
// snapshot was confirmed complete (Snapshotting=True/SnapshotComplete).
func migrationSnapshotConfirmed(m *infrav1beta1.VMMigration) bool {
	c := meta.FindStatusCondition(m.Status.Conditions, infrav1beta1.VMMigrationConditionSnapshotting)
	return c != nil && c.Status == metav1.ConditionTrue && c.Reason == migrationReasonSnapshotComplete
}

// advanceToExporting records the source snapshot as complete and moves the
// migration to Exporting, clearing the snapshot task reference first: the
// Exporting phase uses status.taskRef for its own task.
func (r *VMMigrationReconciler) advanceToExporting(ctx context.Context, migration *infrav1beta1.VMMigration, message string) (ctrl.Result, error) {
	alreadyConfirmed := migrationSnapshotConfirmed(migration)
	migration.Status.Phase = infrav1beta1.MigrationPhaseExporting
	migration.Status.Message = message
	migration.Status.TaskRef = ""
	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionSnapshotting,
		metav1.ConditionTrue, migrationReasonSnapshotComplete, "Source VM snapshot created", migration.Generation)
	if !alreadyConfirmed {
		r.Recorder.Event(migration, corev1.EventTypeNormal, migrationReasonSnapshotComplete,
			fmt.Sprintf("Snapshot %s of the source VM is complete", migration.Status.SnapshotID))
	}
	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: migrationPhaseAdvanceRequeue}, nil
}

// pollMigrationSnapshotTask polls the task of a task-backed SnapshotCreate
// (status.taskRef in the Snapshotting phase) through TaskStatus, as the
// VMSnapshot controller does:
//
//   - still running: Snapshotting=True/SnapshotInProgress (constant
//     message; written once) and a poll after migrationSnapshotTaskPollInterval;
//   - finished without error: the snapshot is confirmed and the migration
//     moves to Exporting with the task reference cleared;
//   - finished with an error: Snapshotting=False/SnapshotFailed, the task
//     reference cleared, the snapshot id kept (so the terminal-failure or
//     deletion cleanup removes whatever the task left, and a retry discards
//     it), and the migration fails — then retried per its retry policy;
//   - the provider no longer knows the task (NotFound, e.g. after a provider
//     restart): the outcome cannot be learned, handled like a failure;
//   - any other poll error: transient, polled again (nothing is written).
func (r *VMMigrationReconciler) pollMigrationSnapshotTask(
	ctx context.Context,
	migration *infrav1beta1.VMMigration,
	providerInstance contracts.Provider,
) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	taskRef := migration.Status.TaskRef
	taskStatus, err := providerInstance.TaskStatus(ctx, taskRef)
	switch {
	case contracts.IsNotFound(err):
		logger.Error(err, "The provider no longer knows the snapshot task; its outcome cannot be learned", "task_id", taskRef)
		return r.failMigrationSnapshot(ctx, migration, "Snapshot creation failed: the provider no longer knows its task, "+
			"so the snapshot's outcome cannot be confirmed")
	case err != nil:
		logger.Info("Could not poll the snapshot task; polling again", "task_id", taskRef, "error", err.Error())
		return ctrl.Result{RequeueAfter: migrationSnapshotTaskPollInterval}, nil
	case taskStatus.Error != "":
		// The task's own text stays in the manager log: it is free-form
		// provider output (paths, hosts, datastore names).
		logger.Info("Snapshot task failed", "task_id", taskRef, "error", taskStatus.Error)
		return r.failMigrationSnapshot(ctx, migration, "Snapshot creation failed: "+migrationTaskFailedDetail)
	case !taskStatus.IsCompleted:
		if waited, over := migrationSnapshotWaitExceeded(migration, time.Now()); over {
			logger.Info("Snapshot task did not finish in time; failing the snapshot", "task_id", taskRef,
				"waited", waited.String(), "limit", migrationSnapshotWaitLimit(migration).String())
			return r.failMigrationSnapshot(ctx, migration, fmt.Sprintf(
				"Snapshot creation failed: its task did not finish within %s", migrationSnapshotWaitLimit(migration)))
		}
		logger.Info("Snapshot task still running", "task_id", taskRef)
		before := migration.Status.DeepCopy()
		k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionSnapshotting,
			metav1.ConditionTrue, migrationReasonSnapshotInProgress, migrationSnapshotInProgressMessage, migration.Generation)
		if !equality.Semantic.DeepEqual(before, &migration.Status) {
			if err := r.updateStatus(ctx, migration); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: migrationSnapshotTaskPollInterval}, nil
	}
	logger.Info("Snapshot task completed", "task_id", taskRef, "snapshot_id", migration.Status.SnapshotID)
	return r.advanceToExporting(ctx, migration, "Snapshot created, starting export")
}

// failMigrationSnapshot records a source snapshot whose task failed (or whose
// outcome could not be learned) and fails the migration with message: the
// snapshot id is kept for cleanup, the task reference cleared, and the
// snapshot marked Snapshotting=False/SnapshotFailed so it is never exported or
// reused.
func (r *VMMigrationReconciler) failMigrationSnapshot(ctx context.Context, migration *infrav1beta1.VMMigration, message string) (ctrl.Result, error) {
	migration.Status.TaskRef = ""
	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionSnapshotting,
		metav1.ConditionFalse, migrationReasonSnapshotFailed, "The source VM snapshot was not confirmed complete; it is not exported", migration.Generation)
	return r.transitionToFailed(ctx, migration, message)
}

// discardUnconfirmedSnapshot removes a recorded source snapshot that was never
// confirmed complete (its task failed, or an earlier attempt stopped before
// its outcome was known), so that a new one is taken: an unconfirmed snapshot
// is never exported. With CleanupPolicy=Never the snapshot is not deleted but
// forgotten, with a Warning naming it. A delete that fails fails the attempt
// (retried per the retry policy); the snapshot id is kept for the next try.
func (r *VMMigrationReconciler) discardUnconfirmedSnapshot(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	snapshotID := migration.Status.SnapshotID
	if cleanupAllowed(migration) {
		if err := r.deleteSourceSnapshot(ctx, migration); err != nil {
			logger.Error(err, "Could not remove the unconfirmed source snapshot", "snapshot_id", snapshotID)
			return r.transitionToFailed(ctx, migration,
				"Could not remove an earlier source snapshot that was never confirmed complete: "+migrationFailureDetail(err))
		}
		r.Recorder.Event(migration, corev1.EventTypeNormal, migrationReasonSnapshotDiscarded,
			fmt.Sprintf("Removed source snapshot %q, which was never confirmed complete; a new one is taken", snapshotID))
	} else {
		migration.Status.SnapshotID = ""
		r.Recorder.Event(migration, corev1.EventTypeWarning, migrationReasonSnapshotDiscarded,
			fmt.Sprintf("Source snapshot %q was never confirmed complete; cleanupPolicy is Never, so it is left on the "+
				"source VM and a new one is taken", snapshotID))
	}
	logger.Info("Discarded an unconfirmed source snapshot; a new one is taken", "snapshot_id", snapshotID)
	migration.Status.TaskRef = ""
	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionSnapshotting,
		metav1.ConditionFalse, migrationReasonSnapshotDiscarded, "An unconfirmed source snapshot was removed; a new one is taken", migration.Generation)
	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: migrationPhaseAdvanceRequeue}, nil
}

// confirmMigrationSnapshotNotTaken re-reads the VMMigration from the API
// server (liveReader) right before its SnapshotCreate and allows the create
// only if the live object is the one this reconcile read — same UID and
// resourceVersion — and still records no snapshot (Snapshotting, no snapshot
// id or task). A VMMigration that is gone ends the reconcile; a cache that is
// behind is retried shortly from a fresh read; a read error is returned.
func (r *VMMigrationReconciler) confirmMigrationSnapshotNotTaken(ctx context.Context, migration *infrav1beta1.VMMigration) (bool, ctrl.Result, error) {
	live := &infrav1beta1.VMMigration{}
	if err := r.liveReader().Get(ctx, client.ObjectKeyFromObject(migration), live); err != nil {
		if apierrors.IsNotFound(err) {
			return false, ctrl.Result{}, nil
		}
		return false, ctrl.Result{}, fmt.Errorf("re-read VMMigration %s/%s before its snapshot: %w", migration.Namespace, migration.Name, err)
	}
	if live.UID != migration.UID || live.ResourceVersion != migration.ResourceVersion ||
		live.Status.Phase != infrav1beta1.MigrationPhaseSnapshotting || live.Status.SnapshotID != "" || live.Status.TaskRef != "" {
		logging.FromContext(ctx).Info("The cached VMMigration is not the live one; not taking the snapshot from it",
			"cachedResourceVersion", migration.ResourceVersion, "liveResourceVersion", live.ResourceVersion,
			"livePhase", live.Status.Phase)
		return false, ctrl.Result{RequeueAfter: migrationStaleReadRetryInterval}, nil
	}
	return true, ctrl.Result{}, nil
}
