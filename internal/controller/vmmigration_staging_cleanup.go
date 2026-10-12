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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	storagemigration "github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

// ADR-0011: a VMMigration that stages its disk on an nfs or s3 backend asks
// its TARGET Provider to delete the staged object (DeleteStagedDisk) when the
// migration no longer needs it. Nothing here ever names the object's location
// (bucket, key, server, export, path, endpoint) in a status or an event.

const (
	// migrationConditionStagingCleanup is the VMMigration condition that
	// tracks the staged disk of an nfs / s3 migration: Unknown
	// (migrationReasonStagedDiskMayExist) from just before the export is
	// sent; False (migrationReasonStagingCleanupPending) while its delete is
	// held — its LastTransitionTime is when the hold began, from which the
	// retries back off; True once nothing is left to do (deleted, emptied,
	// left behind, or retained by cleanupPolicy Never).
	migrationConditionStagingCleanup = "StagingCleanup"

	// migrationReasonStagedDiskMayExist: an export was sent, so a staged
	// object (complete or partial) may exist.
	migrationReasonStagedDiskMayExist = "StagedDiskMayExist"
	// migrationReasonStagingCleanupPending is the condition reason and the
	// Warning event reason of a held staged-disk delete.
	migrationReasonStagingCleanupPending = "StagingCleanupPending"
	// migrationReasonStagedDiskDeleted: the staged object was deleted (or
	// was already gone). Also the Normal event reason.
	migrationReasonStagedDiskDeleted = "StagedDiskDeleted"
	// migrationReasonStagedDiskEmptied: the provider could not unlink the
	// staged nfs file and truncated it to zero bytes. Also the Normal event
	// reason.
	migrationReasonStagedDiskEmptied = "StagedDiskEmptied"
	// migrationReasonStagedDiskLeftBehind: the staged object was not deleted
	// and never will be by VirtRigaud. Also the Warning event reason.
	migrationReasonStagedDiskLeftBehind = "StagedDiskLeftBehind"
	// migrationReasonStagedDiskRetained: cleanupPolicy Never keeps the
	// staged object.
	migrationReasonStagedDiskRetained = "StagedDiskRetained"

	// migrationStagingCleanupLogMessage is the manager log line of every held
	// staged-disk delete; it carries the cause, which status and events never
	// do.
	migrationStagingCleanupLogMessage = "Cannot delete the migration's staged disk yet; retrying with a backoff"
)

// Constant condition and event messages of the staging cleanup. None names a
// location, a provider answer, a delay or a time, so a repeated outcome
// writes nothing.
const (
	migrationStagedDiskMayExistMessage = "An export was sent, so a copy of the source disk may be staged on the migration's " +
		"staging backend. It is deleted when the migration succeeds and when the VMMigration is deleted, and also when the " +
		"migration fails for good with cleanupPolicy Always; cleanupPolicy Never keeps it."
	migrationStagedDiskDeletedMessage = "The migration's staged disk was deleted from the staging backend (or was already gone)."
	migrationStagedDiskEmptiedMessage = "The migration's staged disk was emptied: its data was removed, and an empty file " +
		"with the staging name remains on the NFS export (the target provider reaches NFS only through qemu-img, which " +
		"cannot delete a file)."
	migrationStagedDiskRetainedMessage = "The migration's staged disk is kept on the staging backend: spec.options.cleanupPolicy is Never. " +
		"Remove it yourself when it is no longer needed."
	migrationStagedDiskLeftBehindSuffix = " It is left on the staging backend: remove it yourself (see docs/adr/0011-migration-staging-cleanup.md)."
)

// migrationStagingCleanupPendingMessage is the StagingCleanup=False message
// and Warning event message of a held staged-disk delete.
var migrationStagingCleanupPendingMessage = fmt.Sprintf(
	"The migration's staged disk could not be deleted yet and may still be on the staging backend; its delete is retried "+
		"with a backoff of up to %s, and a deleting VMMigration is kept until it succeeds. Set %s=true on the VMMigration "+
		"to remove it anyway (the staged disk is then left on the staging backend). The cause is in the manager log.",
	blockedRetryMax, forceDeleteAnnotation)

// Why a staged disk is left behind (stagedDiskLeftBehindError.message): one
// constant sentence each.
const (
	stagedDiskLeftUnsupported = "The migration's staged disk was not deleted: the target Provider does not support deleting " +
		"staged disks on this backend (an older provider image, or a provider that cannot reach it)."
	stagedDiskLeftProviderGone = "The migration's staged disk was not deleted: the target Provider no longer exists."
	stagedDiskLeftRefused      = "The migration's staged disk was not deleted: the target provider refused the request."
	stagedDiskLeftNoCredential = "The migration's staged disk was not deleted: the S3 credentials Secret of the migration no longer exists."
	stagedDiskLeftConfig       = "The migration's staged disk was not deleted: the migration's staging configuration is no longer " +
		"valid or no longer allowed by the manager's storage host policy."
	stagedDiskLeftConfigChanged = "The migration's staged disk was not deleted: spec.storage or spec.target.providerRef changed " +
		"since the export, so the object is not where the current spec points."
	stagedDiskLeftUnverified = "The migration's staged disk was not found where the current spec points; the migration was " +
		"exported by an older manager that did not record where, so it may still be at an earlier location."
)

// stagedDiskLeftBehindError is a staged-disk delete that can never succeed:
// the migration's cleanup is released with a StagedDiskLeftBehind Warning
// (message plus migrationStagedDiskLeftBehindSuffix) instead of being held.
type stagedDiskLeftBehindError struct {
	// message is one of the stagedDiskLeft* constants.
	message string
	// cause is the underlying failure, for the manager log only.
	cause error
}

// Error returns the message and the cause.
func (e *stagedDiskLeftBehindError) Error() string {
	if e.cause == nil {
		return e.message
	}
	return e.message + ": " + e.cause.Error()
}

// Unwrap returns the cause.
func (e *stagedDiskLeftBehindError) Unwrap() error { return e.cause }

// leftBehind wraps cause as a stagedDiskLeftBehindError with message.
func leftBehind(message string, cause error) error {
	return &stagedDiskLeftBehindError{message: message, cause: cause}
}

// stagingCleanupTrigger is the point of a migration's life at which its
// staged disk may be deleted (ADR-0011 D3).
type stagingCleanupTrigger int

const (
	// stagingTriggerReady: the migration succeeded (phase Ready).
	stagingTriggerReady stagingCleanupTrigger = iota
	// stagingTriggerTerminalFailure: the migration failed and will not be
	// retried (migrationTerminallyFailed).
	stagingTriggerTerminalFailure
	// stagingTriggerDeletion: the VMMigration is being deleted.
	stagingTriggerDeletion
)

// migrationCleanupPolicy is the migration's spec.options.cleanupPolicy, with
// the CRD default (OnSuccess) when it is unset.
func migrationCleanupPolicy(m *infrav1beta1.VMMigration) string {
	if m.Spec.Options == nil || m.Spec.Options.CleanupPolicy == "" {
		return infrav1beta1.CleanupPolicyOnSuccess
	}
	return m.Spec.Options.CleanupPolicy
}

// migrationStagesOutsideCluster reports whether the migration stages its disk
// on an nfs or s3 backend: the backends whose staged object only a provider
// can delete. (A pvc migration's own PVC is deleted with the VMMigration.)
func migrationStagesOutsideCluster(m *infrav1beta1.VMMigration) bool {
	backend := migrationBackendType(m)
	return backend == storagemigration.BackendNFS || backend == storagemigration.BackendS3
}

// stagedDiskMayExist reports whether an nfs / s3 migration may still have a
// staged object to delete: its StagingCleanup condition is not True, or —
// for a migration that predates the condition — its export completed
// (Exporting=True).
func stagedDiskMayExist(m *infrav1beta1.VMMigration) bool {
	if !migrationStagesOutsideCluster(m) {
		return false
	}
	if c := meta.FindStatusCondition(m.Status.Conditions, migrationConditionStagingCleanup); c != nil {
		return c.Status != metav1.ConditionTrue
	}
	return k8s.IsConditionTrue(m.Status.Conditions, infrav1beta1.VMMigrationConditionExporting)
}

// stagingCleanupWanted is the ADR-0011 D3 decision table: whether the staged
// disk is deleted at trigger. cleanupPolicy Never keeps it always;
// OnSuccess (the default) deletes it on success and when the VMMigration is
// deleted; Always also deletes it when the migration fails for good.
func stagingCleanupWanted(m *infrav1beta1.VMMigration, trigger stagingCleanupTrigger) bool {
	if !stagedDiskMayExist(m) {
		return false
	}
	switch migrationCleanupPolicy(m) {
	case infrav1beta1.CleanupPolicyNever:
		return false
	case infrav1beta1.CleanupPolicyAlways:
		return true
	default: // OnSuccess
		return trigger != stagingTriggerTerminalFailure
	}
}

// markStagedDiskMayExist records, before an nfs / s3 export is sent, that a
// staged object may exist from now on (StagingCleanup=Unknown), the staging
// layout it is named with (status.storageInfo.stagingLayout) and digest — the
// stagingDigest of the configuration the export uses (ADR-0011 D5). It
// reports whether the status changed; the caller persists it BEFORE it builds
// the export URL and sends the export, so an export that fails after writing
// part of the object is still cleaned up, and the import and the delete use
// the same name. The layout is recorded once and then kept: V2 for a migration
// that never staged anything, V1 for one an earlier manager may have staged
// under the legacy name (stagedDiskMayExist, or legacyExportTrace). The digest is that of the latest export.
func markStagedDiskMayExist(m *infrav1beta1.VMMigration, digest string) bool {
	if !migrationStagesOutsideCluster(m) {
		return false
	}
	changed := false
	if m.Status.StorageInfo == nil {
		m.Status.StorageInfo = &infrav1beta1.MigrationStorageInfo{}
	}
	if m.Status.StorageInfo.StagingLayout == "" {
		layout := storagemigration.StagingLayoutV2
		if stagedDiskMayExist(m) || legacyExportTrace(m) {
			layout = storagemigration.StagingLayoutV1
		}
		m.Status.StorageInfo.StagingLayout = layout
		changed = true
	}
	if m.Status.StorageInfo.StagingDigest != digest {
		m.Status.StorageInfo.StagingDigest = digest
		changed = true
	}
	if meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type:               migrationConditionStagingCleanup,
		Status:             metav1.ConditionUnknown,
		Reason:             migrationReasonStagedDiskMayExist,
		Message:            migrationStagedDiskMayExistMessage,
		ObservedGeneration: m.Generation,
	}) {
		changed = true
	}
	return changed
}

// stagingDigestVersion versions stagingDigestInputV1. A new input form gets a
// new version (and struct): the digests already recorded for migrations in
// flight must keep matching.
const stagingDigestVersion = "staging-digest-v1"

// stagingDigestInputV1 is the canonical, versioned input of stagingDigest:
// exactly the spec fields that decide where a migration stages its disk, and
// the target Provider that deletes it. It is built from the spec, never from
// the wire options JSON, so a field later added to that JSON does not change
// the digest of a migration already in flight. Never reorder or rename these
// fields; add a V2 instead.
type stagingDigestInputV1 struct {
	Version           string `json:"v"`
	Backend           string `json:"backend"`
	S3Bucket          string `json:"s3Bucket"`
	S3Endpoint        string `json:"s3Endpoint"`
	S3Region          string `json:"s3Region"`
	S3Prefix          string `json:"s3Prefix"`
	S3UsePathStyle    bool   `json:"s3UsePathStyle"`
	NFSServer         string `json:"nfsServer"`
	NFSExport         string `json:"nfsExport"`
	NFSPath           string `json:"nfsPath"`
	NFSUID            *int64 `json:"nfsUID"`
	NFSGID            *int64 `json:"nfsGID"`
	ProviderNamespace string `json:"providerNamespace"`
	ProviderName      string `json:"providerName"`
	ProviderUID       string `json:"providerUID"`
}

// stagingDigest is the digest of where a migration stages its disk and who
// deletes it (stagingDigestInputV1). It names no location
// ("sha256:<hex>").
func stagingDigest(m *infrav1beta1.VMMigration, target *infrav1beta1.Provider) (string, error) {
	in := stagingDigestInputV1{
		Version:           stagingDigestVersion,
		Backend:           migrationBackendType(m),
		ProviderNamespace: target.Namespace,
		ProviderName:      target.Name,
		ProviderUID:       string(target.UID),
	}
	if st := m.Spec.Storage; st != nil {
		if s3 := st.S3; s3 != nil {
			in.S3Bucket, in.S3Endpoint, in.S3Region, in.S3Prefix, in.S3UsePathStyle = s3.Bucket, s3.Endpoint, s3.Region, s3.Prefix, s3.UsePathStyle
		}
		if nfs := st.NFS; nfs != nil {
			in.NFSServer, in.NFSExport, in.NFSPath, in.NFSUID, in.NFSGID = nfs.Server, nfs.Export, nfs.Path, nfs.UID, nfs.GID
		}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("encode staging digest: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// recordedStagingDigest is the digest recorded with the last export, or ""
// for a migration exported by an older manager.
func recordedStagingDigest(m *infrav1beta1.VMMigration) string {
	if m.Status.StorageInfo == nil {
		return ""
	}
	return m.Status.StorageInfo.StagingDigest
}

// legacyExportTrace reports whether an earlier manager may already have sent
// this migration's export — and so written (part of) an object under the
// legacy V1 name — although it recorded no StagingCleanup marker: an export
// id, an Exporting condition of any status, or an export task. Such a
// migration keeps V1, so the next export overwrites that object and its
// cleanup finds it, instead of leaving it orphaned under the V1 name.
func legacyExportTrace(m *infrav1beta1.VMMigration) bool {
	if meta.FindStatusCondition(m.Status.Conditions, migrationConditionStagingCleanup) != nil {
		return false
	}
	return m.Status.ExportID != "" ||
		meta.FindStatusCondition(m.Status.Conditions, infrav1beta1.VMMigrationConditionExporting) != nil ||
		(m.Status.Phase == infrav1beta1.MigrationPhaseExporting && m.Status.TaskRef != "")
}

// migrationStagingLayout is the staging layout recorded for the migration, or
// V1 (legacy) when none is: a migration exported before layout V2.
func migrationStagingLayout(m *infrav1beta1.VMMigration) string {
	if m.Status.StorageInfo != nil && m.Status.StorageInfo.StagingLayout != "" {
		return m.Status.StorageInfo.StagingLayout
	}
	return storagemigration.StagingLayoutV1
}

// stagingCleanupHeldSince is when the current staged-disk delete hold began
// (the StagingCleanup=False transition), or zero when none is held.
func stagingCleanupHeldSince(m *infrav1beta1.VMMigration) time.Time {
	c := meta.FindStatusCondition(m.Status.Conditions, migrationConditionStagingCleanup)
	if c == nil || c.Status != metav1.ConditionFalse {
		return time.Time{}
	}
	return c.LastTransitionTime.Time
}

// stagingCleanupNotDue reports whether a held staged-disk delete was recorded
// less than blockedRetryMin ago, and how long until it is due: the reconcile
// its own status write triggers must not re-send the delete ahead of the
// backoff.
func stagingCleanupNotDue(m *infrav1beta1.VMMigration, now time.Time) (time.Duration, bool) {
	since := stagingCleanupHeldSince(m)
	if since.IsZero() {
		return 0, false
	}
	if elapsed := now.Sub(since); elapsed < blockedRetryMin {
		return blockedRetryMin - elapsed, true
	}
	return 0, false
}

// reconcileStagingCleanup runs the staging-cleanup step of a migration that
// is not being deleted, at trigger (Ready or terminal failure). It returns how
// soon a held delete must be retried (0: nothing to retry) and an error only
// when a status write fails. It never blocks the migration's other cleanup.
func (r *VMMigrationReconciler) reconcileStagingCleanup(ctx context.Context, m *infrav1beta1.VMMigration, trigger stagingCleanupTrigger) (time.Duration, error) {
	if stagedDiskMayExist(m) && migrationCleanupPolicy(m) == infrav1beta1.CleanupPolicyNever {
		return 0, r.setStagingCleanupDone(ctx, m, migrationReasonStagedDiskRetained, migrationStagedDiskRetainedMessage, "")
	}
	if !stagingCleanupWanted(m, trigger) {
		return 0, nil
	}
	if wait, notDue := stagingCleanupNotDue(m, time.Now()); notDue {
		return wait, nil
	}
	outcome, err := r.deleteStagedDisk(ctx, m)
	var left *stagedDiskLeftBehindError
	switch {
	case err == nil:
		reason, msg := stagedDiskRemovedReason(outcome)
		logging.FromContext(ctx).Info("Migration staged disk removed", "outcome", string(outcome))
		return 0, r.setStagingCleanupDone(ctx, m, reason, msg, corev1.EventTypeNormal)
	case stderrors.As(err, &left):
		logStagedDiskLeftBehind(ctx, left)
		return 0, r.setStagingCleanupDone(ctx, m, migrationReasonStagedDiskLeftBehind,
			left.message+migrationStagedDiskLeftBehindSuffix, corev1.EventTypeWarning)
	default:
		return r.holdStagingCleanup(ctx, m, err)
	}
}

// stagedDiskRemovedReason is the StagingCleanup reason and message of a
// successful delete.
func stagedDiskRemovedReason(outcome contracts.StagedDiskDeleteOutcome) (string, string) {
	if outcome == contracts.StagedDiskEmptied {
		return migrationReasonStagedDiskEmptied, migrationStagedDiskEmptiedMessage
	}
	return migrationReasonStagedDiskDeleted, migrationStagedDiskDeletedMessage
}

// setStagingCleanupDone records StagingCleanup=True with reason and message,
// and — when that changes the stored status and eventType is set — one event
// of eventType with the same reason and message.
func (r *VMMigrationReconciler) setStagingCleanupDone(ctx context.Context, m *infrav1beta1.VMMigration, reason, message, eventType string) error {
	changed := meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type:               migrationConditionStagingCleanup,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: m.Generation,
	})
	if !changed {
		return nil
	}
	if eventType != "" {
		r.Recorder.Event(m, eventType, reason, message)
	}
	return r.updateStatus(ctx, m)
}

// holdStagingCleanup records a held staged-disk delete —
// StagingCleanup=False/StagingCleanupPending with the constant
// migrationStagingCleanupPendingMessage, and one Warning event when the hold
// begins — only when that changes the stored status, and returns the
// blocked-VM backoff (15 s doubling to 5 min, from when the hold began). cause
// goes to the manager log only.
func (r *VMMigrationReconciler) holdStagingCleanup(ctx context.Context, m *infrav1beta1.VMMigration, cause error) (time.Duration, error) {
	retry := blockedRetryBackoff(stagingCleanupHeldSince(m))
	logging.FromContext(ctx).Info(migrationStagingCleanupLogMessage, "retryAfter", retry.String(), "error", cause.Error())

	before := m.Status.DeepCopy()
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type:               migrationConditionStagingCleanup,
		Status:             metav1.ConditionFalse,
		Reason:             migrationReasonStagingCleanupPending,
		Message:            migrationStagingCleanupPendingMessage,
		ObservedGeneration: m.Generation,
	})
	if equality.Semantic.DeepEqual(before, &m.Status) {
		return retry, nil
	}
	if c := meta.FindStatusCondition(before.Conditions, migrationConditionStagingCleanup); c == nil || c.Status != metav1.ConditionFalse {
		r.Recorder.Event(m, corev1.EventTypeWarning, migrationReasonStagingCleanupPending, migrationStagingCleanupPendingMessage)
	}
	if err := r.updateStatus(ctx, m); err != nil {
		return 0, err
	}
	return retry, nil
}

// releaseStagedDiskForDeletion runs the staging-cleanup step of a VMMigration's
// deletion (ADR-0011 D4). hold == true keeps the finalizer and the caller
// returns (res, err): the staged disk may still be on the backend and its
// delete can still succeed. hold == false lets the deletion proceed: nothing
// to delete (pvc, nothing staged, cleanupPolicy Never), the staged disk is
// deleted, its delete can never succeed (a StagedDiskLeftBehind Warning), or
// force-delete says to leave it behind (also a StagedDiskLeftBehind Warning).
func (r *VMMigrationReconciler) releaseStagedDiskForDeletion(ctx context.Context, m *infrav1beta1.VMMigration) (bool, ctrl.Result, error) {
	if !stagingCleanupWanted(m, stagingTriggerDeletion) {
		return false, ctrl.Result{}, nil
	}
	logger := logging.FromContext(ctx)
	force := hasMigrationForceDeleteAnnotation(m)

	// A held delete (or export-task wait) that is not due yet sends nothing —
	// not even the export task poll — ahead of its backoff; force-delete is
	// acted on at once.
	if wait, notDue := stagingCleanupNotDue(m, time.Now()); notDue {
		if !force {
			return true, ctrl.Result{RequeueAfter: wait}, nil
		}
		if werr := r.recordStagedDiskForceLeft(ctx, m, nil); werr != nil {
			return true, ctrl.Result{}, werr
		}
		return false, ctrl.Result{}, nil
	}

	// Review M4: an export task still running on the source provider would
	// write the full object AFTER the delete. Wait until it is finished.
	if running, err := r.exportTaskMayBeRunning(ctx, m); running || err != nil {
		if force {
			if werr := r.recordStagedDiskForceLeft(ctx, m, err); werr != nil {
				return true, ctrl.Result{}, werr
			}
			return false, ctrl.Result{}, nil
		}
		if err == nil {
			err = errExportTaskRunning
		}
		retry, herr := r.holdStagingCleanup(ctx, m, err)
		return true, ctrl.Result{RequeueAfter: retry}, herr
	}

	outcome, err := r.deleteStagedDisk(ctx, m)
	var left *stagedDiskLeftBehindError
	switch {
	// Each outcome is persisted (StagingCleanup=True) BEFORE the finalizer is
	// removed, so a finalizer update that conflicts re-runs nothing: no second
	// RPC, no second event.
	case err == nil:
		reason, msg := stagedDiskRemovedReason(outcome)
		logger.Info("Migration staged disk removed during deletion", "outcome", string(outcome))
		if werr := r.setStagingCleanupDone(ctx, m, reason, msg, corev1.EventTypeNormal); werr != nil {
			return true, ctrl.Result{}, werr
		}
		return false, ctrl.Result{}, nil
	case stderrors.As(err, &left):
		logStagedDiskLeftBehind(ctx, left)
		if werr := r.setStagingCleanupDone(ctx, m, migrationReasonStagedDiskLeftBehind,
			left.message+migrationStagedDiskLeftBehindSuffix, corev1.EventTypeWarning); werr != nil {
			return true, ctrl.Result{}, werr
		}
		return false, ctrl.Result{}, nil
	case force:
		if werr := r.recordStagedDiskForceLeft(ctx, m, err); werr != nil {
			return true, ctrl.Result{}, werr
		}
		return false, ctrl.Result{}, nil
	default:
		retry, herr := r.holdStagingCleanup(ctx, m, err)
		return true, ctrl.Result{RequeueAfter: retry}, herr
	}
}

// errExportTaskRunning is the hold cause of a deleting VMMigration whose
// export task is still running on the source provider.
var errExportTaskRunning = stderrors.New("the migration's export task is still running on the source provider")

// exportTaskMayBeRunning reports whether the migration's export may still be
// writing its staged object: it is in phase Exporting with an export task
// (status.taskRef) that the source provider does not report finished. A
// source VirtualMachine or Provider that no longer exists leaves nothing to
// ask (and nothing to wait for through VirtRigaud): false. Any other failure
// to ask is returned, and the caller holds. (Every provider's ExportDisk is
// synchronous today; a synchronous export cannot overlap a deletion, whose
// reconcile is serialized with the export's.)
func (r *VMMigrationReconciler) exportTaskMayBeRunning(ctx context.Context, m *infrav1beta1.VMMigration) (bool, error) {
	if m.Status.Phase != infrav1beta1.MigrationPhaseExporting || m.Status.TaskRef == "" {
		return false, nil
	}
	sourceVM, err := r.getSourceVM(ctx, m)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get source VM to check the export task: %w", err)
	}
	sourceProvider, err := r.sourceProviderFor(ctx, m, sourceVM)
	if apierrors.IsNotFound(err) && !isConsumerNotAllowed(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("resolve source provider to check the export task: %w", err)
	}
	instance, err := r.getProviderInstance(ctx, sourceProvider)
	if err != nil {
		return false, fmt.Errorf("get source provider instance to check the export task: %w", err)
	}
	done, err := instance.IsTaskComplete(ctx, m.Status.TaskRef)
	if err != nil {
		return false, fmt.Errorf("check the export task: %w", err)
	}
	return !done, nil
}

// recordStagedDiskForceLeft records that force-delete removes the VMMigration
// although its staged disk was not deleted: StagingCleanup=True with reason
// StagedDiskLeftBehind and one Warning (naming no location), persisted before
// the finalizer is removed; the cause goes to the log at V(1).
func (r *VMMigrationReconciler) recordStagedDiskForceLeft(ctx context.Context, m *infrav1beta1.VMMigration, cause error) error {
	msg := fmt.Sprintf("%s=true: removing the VMMigration although its staged disk was not deleted.%s",
		forceDeleteAnnotation, migrationStagedDiskLeftBehindSuffix)
	logger := logging.FromContext(ctx)
	logger.Info("Staged disk not deleted but force-delete annotation is set; removing finalizer, the staged disk is left behind",
		"annotation", forceDeleteAnnotation)
	if cause != nil {
		logger.V(1).Info("Staged disk force-delete cause", "error", cause.Error())
	}
	return r.setStagingCleanupDone(ctx, m, migrationReasonStagedDiskLeftBehind, msg, corev1.EventTypeWarning)
}

// logStagedDiskLeftBehind logs a left-behind staged disk: the constant reason
// at Info, the cause (which can name a host, a path or a provider answer) at
// V(1) only.
func logStagedDiskLeftBehind(ctx context.Context, left *stagedDiskLeftBehindError) {
	logger := logging.FromContext(ctx)
	logger.Info("Migration staged disk left behind", "reason", left.message)
	if left.cause != nil {
		logger.V(1).Info("Migration staged disk left behind: cause", "error", left.cause.Error())
	}
}

// deleteStagedDisk asks the migration's target Provider to delete its staged
// disk (ADR-0011 D2): the Provider spec.target.providerRef names, resolved
// through the live grant check. It returns the outcome, a
// stagedDiskLeftBehindError when the delete can never succeed (no such
// Provider; a provider without the RPC or this backend; a refused request; an
// S3 credentials Secret that is gone; a staging configuration that is no
// longer valid or allowed), or any other error when a retry can still succeed.
// Before every call the staging host is checked against the SSRF host policy
// again: spec.storage may have changed since Validating.
func (r *VMMigrationReconciler) deleteStagedDisk(ctx context.Context, m *infrav1beta1.VMMigration) (contracts.StagedDiskDeleteOutcome, error) {
	backend := migrationBackendType(m)
	if err := r.validateStorageConfig(ctx, m.Spec.Storage); err != nil {
		if stderrors.Is(err, storagemigration.ErrHostUnresolvable) {
			// A DNS failure can be transient (review M3): held, not left
			// behind. Only a policy denial is permanent.
			return "", fmt.Errorf("staging host check: %w", err)
		}
		return "", leftBehind(stagedDiskLeftConfig, err)
	}
	optionsJSON, err := storageOptionsJSON(m)
	if err != nil {
		return "", leftBehind(stagedDiskLeftConfig, err)
	}

	provider, err := r.targetProviderFor(ctx, r.liveReader(), m)
	if apierrors.IsNotFound(err) && !isConsumerNotAllowed(err) {
		return "", leftBehind(stagedDiskLeftProviderGone, err)
	}
	if err != nil {
		return "", fmt.Errorf("resolve target provider: %w", err)
	}
	// ADR-0011 D5 (review M2): delete only where the export staged. A spec
	// edited since then points elsewhere: a NOT_FOUND there would be reported
	// "deleted" while the object remains, and a delete could be aimed at
	// another location.
	recorded := recordedStagingDigest(m)
	if recorded != "" {
		current, err := stagingDigest(m, provider)
		if err != nil {
			return "", leftBehind(stagedDiskLeftConfig, err)
		}
		if current != recorded {
			return "", leftBehind(stagedDiskLeftConfigChanged, nil)
		}
	}

	instance, err := r.getProviderInstance(ctx, provider)
	if err != nil {
		return "", fmt.Errorf("get target provider instance: %w", err)
	}
	deleter, ok := instance.(contracts.StagedDiskDeleter)
	if !ok {
		return "", stagedDiskUnsupported(m, stderrors.New("the provider client has no DeleteStagedDisk"))
	}
	reporter, ok := instance.(contracts.CapabilityReporter)
	if !ok {
		return "", stagedDiskUnsupported(m, stderrors.New("the provider client reports no capabilities"))
	}
	caps, err := reporter.GetCapabilities(ctx)
	if err != nil {
		return "", fmt.Errorf("query target provider capabilities: %w", err)
	}
	if !slices.Contains(caps.SupportedStagedDiskDeleteBackends, backend) {
		return "", stagedDiskUnsupported(m, fmt.Errorf("backend %q not in supportedStagedDiskDeleteBackends", backend))
	}

	creds, err := r.loadS3Credentials(ctx, m)
	if apierrors.IsNotFound(err) {
		return "", leftBehind(stagedDiskLeftNoCredential, err)
	}
	if err != nil {
		return "", fmt.Errorf("load staging credentials: %w", err)
	}
	targetVM, err := r.stagedImportTarget(ctx, m)
	if err != nil {
		return "", err
	}

	outcome, err := deleter.DeleteStagedDisk(ctx, contracts.StagedDiskDeleteRequest{
		Migration:          contracts.ObjectIdentity{UID: string(m.UID), Namespace: m.Namespace, Name: m.Name},
		Stage:              storagemigration.StagingStageExport,
		BackendType:        backend,
		StorageOptionsJSON: optionsJSON,
		Credentials:        creds,
		Layout:             migrationStagingLayout(m),
		TargetVM:           targetVM,
	})
	if err == nil && outcome == contracts.StagedDiskNotFound && recorded == "" {
		// Exported by an older manager that recorded no digest: "not found"
		// at the current spec proves nothing about the object.
		return "", leftBehind(stagedDiskLeftUnverified, nil)
	}
	switch {
	case err == nil:
		return outcome, nil
	case contracts.IsNotFound(err):
		if recorded == "" {
			return "", leftBehind(stagedDiskLeftUnverified, err)
		}
		return contracts.StagedDiskNotFound, nil
	case contracts.IsNotSupported(err):
		return "", stagedDiskUnsupported(m, err)
	case contracts.IsInvalidSpec(err):
		return "", leftBehind(stagedDiskLeftRefused, err)
	default:
		return "", fmt.Errorf("delete staged disk: %w", err)
	}
}

// markImportTarget records, for an nfs / s3 migration, the VirtualMachine
// key its ImportDisk is about to be sent for
// (status.storageInfo.importTarget). It reports whether the status changed;
// the caller persists it BEFORE the import is sent, so an import that fails
// after the provider staged something is still cleaned up — for that VM only.
func markImportTarget(m *infrav1beta1.VMMigration, key client.ObjectKey) bool {
	if !migrationStagesOutsideCluster(m) {
		return false
	}
	if m.Status.StorageInfo == nil {
		m.Status.StorageInfo = &infrav1beta1.MigrationStorageInfo{}
	}
	if t := m.Status.StorageInfo.ImportTarget; t != nil && t.Namespace == key.Namespace && t.Name == key.Name {
		return false
	}
	m.Status.StorageInfo.ImportTarget = &infrav1beta1.MigrationImportTarget{Namespace: key.Namespace, Name: key.Name}
	return true
}

// recordedImportTarget is the VM key the migration's import was sent for
// (markImportTarget), or false when none was recorded.
func recordedImportTarget(m *infrav1beta1.VMMigration) (client.ObjectKey, bool) {
	if m.Status.StorageInfo == nil || m.Status.StorageInfo.ImportTarget == nil {
		return client.ObjectKey{}, false
	}
	t := m.Status.StorageInfo.ImportTarget
	if t.Namespace == "" || t.Name == "" {
		return client.ObjectKey{}, false
	}
	return client.ObjectKey{Namespace: t.Namespace, Name: t.Name}, true
}

// migrationInFlight reports whether a VMMigration may still import into, or
// create from, its target: not being deleted, not Ready, and not failed for
// good (a Failed migration waiting for a retry is still in flight).
func migrationInFlight(m *infrav1beta1.VMMigration) bool {
	return m.DeletionTimestamp.IsZero() && m.Status.Phase != infrav1beta1.MigrationPhaseReady && !migrationTerminallyFailed(m)
}

// stagedImportTarget is the target VM DeleteStagedDisk names so the target
// provider also removes the provider-side copy its ImportDisk kept for that
// VM until Create (Proxmox: the node-side import stage, named after the
// target VM, not the migration). It is ONLY the VM the import was sent for,
// as recorded in status (recordedImportTarget) — never the current
// spec.target, which a tenant may have edited since. It is zero (only the
// staged object is removed) when no import was recorded; when the recorded VM
// is in another namespace that no longer grants this migration's namespace
// (AllowedSourceNamespacesAnnotation, read live); and when another VMMigration
// still in flight (migrationInFlight) targets, or recorded an import for, the
// same VM: that one's import may have written, and its Create may be about to
// read, the same stage. A failed read is an error (the delete is held and
// retried).
func (r *VMMigrationReconciler) stagedImportTarget(ctx context.Context, m *infrav1beta1.VMMigration) (contracts.ObjectIdentity, error) {
	key, ok := recordedImportTarget(m)
	if !ok {
		return contracts.ObjectIdentity{}, nil
	}
	allowed, err := targetNamespaceAllowed(ctx, r.liveReader(), m.Namespace, key.Namespace)
	if err != nil {
		return contracts.ObjectIdentity{}, fmt.Errorf("check the import target namespace grant: %w", err)
	}
	if !allowed {
		return contracts.ObjectIdentity{}, nil
	}
	migrations := &infrav1beta1.VMMigrationList{}
	if err := r.List(ctx, migrations); err != nil {
		return contracts.ObjectIdentity{}, fmt.Errorf("list VMMigrations sharing the target VM: %w", err)
	}
	for i := range migrations.Items {
		other := &migrations.Items[i]
		if other.UID == m.UID || !migrationInFlight(other) {
			continue
		}
		if migrationTargetVMKey(other) == key {
			return contracts.ObjectIdentity{}, nil
		}
		if recorded, ok := recordedImportTarget(other); ok && recorded == key {
			return contracts.ObjectIdentity{}, nil
		}
	}
	return contracts.ObjectIdentity{Namespace: key.Namespace, Name: key.Name}, nil
}

// stagedDiskUnsupportedGrace is how long a target provider that does not
// support the staged-disk delete is waited for before the staged disk is
// recorded as left behind (review M3): during a rolling upgrade the manager
// runs before the provider pods are rolled, and an older provider image
// reports nothing.
const stagedDiskUnsupportedGrace = 24 * time.Hour

// stagedDiskUnsupported is the error of a target provider that cannot delete
// the staged disk (cause): a retryable hold while the hold is younger than
// stagedDiskUnsupportedGrace (the provider may be rolled to an image that can),
// and a stagedDiskLeftBehindError after it.
func stagedDiskUnsupported(m *infrav1beta1.VMMigration, cause error) error {
	since := stagingCleanupHeldSince(m)
	if since.IsZero() || time.Since(since) < stagedDiskUnsupportedGrace {
		return fmt.Errorf("the target provider does not support the staged-disk delete yet (held for up to %s): %w",
			stagedDiskUnsupportedGrace, cause)
	}
	return leftBehind(stagedDiskLeftUnsupported, cause)
}

// stagingCleanupRequeue merges a held staging delete's retry into res: the
// sooner of the two requeues.
func stagingCleanupRequeue(res ctrl.Result, retry time.Duration) ctrl.Result {
	if retry > 0 && (res.RequeueAfter == 0 || retry < res.RequeueAfter) {
		res.RequeueAfter = retry
	}
	return res
}
