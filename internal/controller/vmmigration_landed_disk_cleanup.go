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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// rc2 R7: a VMMigration whose ImportDisk landed a disk on its target provider
// (vSphere "[ds] <ns>_<vm>-migrated/", libvirt "<pool>/<ns>.<vm>-migrated.qcow2",
// the Proxmox import stage) and that then fails for good, or is deleted,
// without a target VirtualMachine that uses the disk, asks the target provider
// to delete it (DeleteLandedDisk), with the same cleanupPolicy semantics as
// the staged disk (ADR-0011 D3): Never keeps it; OnSuccess (the default)
// deletes it when the VMMigration is deleted; Always also deletes it when the
// migration fails for good. A successful migration's landed disk is its target
// VM's disk and is never deleted. Status and events never name the disk's
// location.

const (
	// migrationConditionLandedDiskCleanup is the VMMigration condition that
	// tracks the disk the migration's import landed on the target provider:
	// Unknown (migrationReasonLandedDiskMayExist) from just before the import
	// is sent; False (migrationReasonLandedDiskCleanupPending) while its
	// delete is held — LastTransitionTime is when the hold began, from which
	// the retries back off; True once nothing is left to do (deleted, kept
	// because a VM uses it or another migration lands the same name, left
	// behind, or retained by cleanupPolicy Never).
	migrationConditionLandedDiskCleanup = "LandedDiskCleanup"

	// migrationReasonLandedDiskMayExist: an import was sent, so a landed disk
	// (complete or partial) may exist on the target provider.
	migrationReasonLandedDiskMayExist = "LandedDiskMayExist"
	// migrationReasonLandedDiskCleanupPending is the condition reason and the
	// Warning event reason of a held landed-disk delete.
	migrationReasonLandedDiskCleanupPending = "LandedDiskCleanupPending"
	// migrationReasonLandedDiskDeleted: the landed disk was deleted (or was
	// already gone). Also the Normal event reason.
	migrationReasonLandedDiskDeleted = "LandedDiskDeleted"
	// migrationReasonLandedDiskInUse: the landed disk is kept because a VM
	// uses it — the migration's target VirtualMachine, or a VM the target
	// provider reports.
	migrationReasonLandedDiskInUse = "LandedDiskInUse"
	// migrationReasonLandedDiskHandedOver: another VMMigration in flight
	// targets the same VirtualMachine and lands its disk under the same name.
	migrationReasonLandedDiskHandedOver = "LandedDiskHandedOver"
	// migrationReasonLandedDiskRetained: cleanupPolicy Never keeps the landed
	// disk.
	migrationReasonLandedDiskRetained = "LandedDiskRetained"
	// migrationReasonLandedDiskLeftBehind: the landed disk was not deleted and
	// never will be by VirtRigaud. Also the Warning event reason.
	migrationReasonLandedDiskLeftBehind = "LandedDiskLeftBehind"

	// migrationLandedDiskCleanupLogMessage is the manager log line of every
	// held landed-disk delete; it carries the cause, which status and events
	// never do.
	migrationLandedDiskCleanupLogMessage = "Cannot delete the migration's landed disk yet; retrying with a backoff"
)

// Constant condition and event messages of the landed-disk cleanup. None
// names a location, a provider answer, a delay or a time, so a repeated
// outcome writes nothing.
const (
	migrationLandedDiskMayExistMessage = "A disk import was sent, so a disk may be landed on the target provider for the " +
		"target VirtualMachine. Unless a VirtualMachine uses it, it is deleted when the VMMigration is deleted, and also " +
		"when the migration fails for good with cleanupPolicy Always; cleanupPolicy Never keeps it."
	migrationLandedDiskDeletedMessage = "The disk the migration's import landed on the target provider was deleted (or " +
		"was already gone)."
	migrationLandedDiskInUseMessage = "The disk the migration's import landed on the target provider is kept: it is the " +
		"disk of the migration's target VirtualMachine, or a VM on the target provider uses it."
	migrationLandedDiskHandedOverMessage = "The disk the migration's import landed on the target provider is kept: " +
		"another VMMigration in progress targets the same VirtualMachine and lands its disk under the same name."
	migrationLandedDiskRetainedMessage = "The disk the migration's import landed on the target provider is kept: " +
		"spec.options.cleanupPolicy is Never. Remove it yourself when it is no longer needed."
	migrationLandedDiskLeftBehindSuffix = " It is left on the target provider: remove it yourself (see docs/upgrading.md, " +
		"\"Leftovers\")."
)

// migrationLandedDiskCleanupPendingMessage is the LandedDiskCleanup=False
// message and Warning event message of a held landed-disk delete.
var migrationLandedDiskCleanupPendingMessage = fmt.Sprintf(
	"The disk the migration's import landed on the target provider could not be deleted yet; its delete is retried "+
		"with a backoff of up to %s, and a deleting VMMigration is kept until it succeeds. Set %s=true on the "+
		"VMMigration to remove it anyway (the landed disk is then left on the target provider). The cause is in the "+
		"manager log.",
	blockedRetryMax, forceDeleteAnnotation)

// Why a landed disk is left behind (landedDiskLeftBehindError.message): one
// constant sentence each.
const (
	landedDiskLeftUnsupported = "The migration's landed disk was not deleted: the target Provider does not support " +
		"deleting landed disks (an older provider image)."
	landedDiskLeftProviderGone = "The migration's landed disk was not deleted: the target Provider no longer exists."
	landedDiskLeftProviderChanged = "The migration's landed disk was not deleted: spec.target.providerRef changed, or the " +
		"Provider was replaced, since the import, so the current Provider is not the one the disk was landed on."
	landedDiskLeftRefused = "The migration's landed disk was not deleted: the target provider refused the request."
	landedDiskLeftNamespace = "The migration's landed disk was not deleted: the target namespace no longer grants this " +
		"migration's namespace."
	landedDiskLeftUnrecorded = "The migration's landed disk was not deleted: the VirtualMachine the import was sent for " +
		"was not recorded."
)

// landedDiskLeftBehindError is a landed-disk delete that can never succeed:
// the cleanup is released with a LandedDiskLeftBehind Warning (message plus
// migrationLandedDiskLeftBehindSuffix) instead of being held.
type landedDiskLeftBehindError struct {
	// message is one of the landedDiskLeft* constants.
	message string
	// cause is the underlying failure, for the manager log only.
	cause error
}

// Error returns the message and the cause.
func (e *landedDiskLeftBehindError) Error() string {
	if e.cause == nil {
		return e.message
	}
	return e.message + ": " + e.cause.Error()
}

// Unwrap returns the cause.
func (e *landedDiskLeftBehindError) Unwrap() error { return e.cause }

// landedLeftBehind wraps cause as a landedDiskLeftBehindError with message.
func landedLeftBehind(message string, cause error) error {
	return &landedDiskLeftBehindError{message: message, cause: cause}
}

// landedDiskDone is a final outcome of the landed-disk cleanup: the
// LandedDiskCleanup=True reason and message, and the event type to emit with
// them ("" for none).
type landedDiskDone struct {
	reason, message, eventType string
}

var (
	landedDiskDoneDeleted    = landedDiskDone{migrationReasonLandedDiskDeleted, migrationLandedDiskDeletedMessage, corev1.EventTypeNormal}
	landedDiskDoneInUse      = landedDiskDone{migrationReasonLandedDiskInUse, migrationLandedDiskInUseMessage, ""}
	landedDiskDoneHandedOver = landedDiskDone{migrationReasonLandedDiskHandedOver, migrationLandedDiskHandedOverMessage, ""}
	landedDiskDoneRetained   = landedDiskDone{migrationReasonLandedDiskRetained, migrationLandedDiskRetainedMessage, ""}
)

// landedDiskMayExist reports whether the migration may still have a landed
// disk to clean up: its LandedDiskCleanup condition exists and is not True.
// A migration whose import was sent by a manager older than rc2 has no such
// condition and is left as it was.
func landedDiskMayExist(m *infrav1beta1.VMMigration) bool {
	c := meta.FindStatusCondition(m.Status.Conditions, migrationConditionLandedDiskCleanup)
	return c != nil && c.Status != metav1.ConditionTrue
}

// landedDiskCleanupWanted is the decision table of the landed-disk cleanup at
// trigger, with the staged disk's cleanupPolicy semantics (ADR-0011 D3):
// Never keeps it always; OnSuccess (the default) deletes it when the
// VMMigration is deleted; Always also deletes it when the migration fails for
// good. On success (Ready) it is the target VM's disk and is never deleted.
func landedDiskCleanupWanted(m *infrav1beta1.VMMigration, trigger stagingCleanupTrigger) bool {
	if !landedDiskMayExist(m) || trigger == stagingTriggerReady {
		return false
	}
	switch migrationCleanupPolicy(m) {
	case infrav1beta1.CleanupPolicyNever:
		return false
	case infrav1beta1.CleanupPolicyAlways:
		return true
	default: // OnSuccess
		return trigger == stagingTriggerDeletion
	}
}

// markLandedDiskMayExist records, before an ImportDisk is sent, that a disk
// may be landed from now on (LandedDiskCleanup=Unknown) and for which
// VirtualMachine and on which Provider (status.storageInfo.importTarget, with
// the Provider's UID). It reports whether the status changed; the caller
// persists it BEFORE the import is sent, so an import that fails after
// landing part of the disk is still cleaned up — for that VM, on that
// Provider only.
func markLandedDiskMayExist(m *infrav1beta1.VMMigration, key client.ObjectKey, provider *infrav1beta1.Provider) bool {
	changed := false
	if m.Status.StorageInfo == nil {
		m.Status.StorageInfo = &infrav1beta1.MigrationStorageInfo{}
	}
	want := infrav1beta1.MigrationImportTarget{Namespace: key.Namespace, Name: key.Name, ProviderUID: string(provider.UID)}
	if t := m.Status.StorageInfo.ImportTarget; t == nil || *t != want {
		m.Status.StorageInfo.ImportTarget = &want
		changed = true
	}
	if meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type:               migrationConditionLandedDiskCleanup,
		Status:             metav1.ConditionUnknown,
		Reason:             migrationReasonLandedDiskMayExist,
		Message:            migrationLandedDiskMayExistMessage,
		ObservedGeneration: m.Generation,
	}) {
		changed = true
	}
	return changed
}

// landedDiskCleanupHeldSince is when the current landed-disk delete hold
// began (the LandedDiskCleanup=False transition), or zero when none is held.
func landedDiskCleanupHeldSince(m *infrav1beta1.VMMigration) time.Time {
	c := meta.FindStatusCondition(m.Status.Conditions, migrationConditionLandedDiskCleanup)
	if c == nil || c.Status != metav1.ConditionFalse {
		return time.Time{}
	}
	return c.LastTransitionTime.Time
}

// landedDiskCleanupNotDue reports whether a held landed-disk delete was
// recorded less than blockedRetryMin ago, and how long until it is due.
func landedDiskCleanupNotDue(m *infrav1beta1.VMMigration, now time.Time) (time.Duration, bool) {
	since := landedDiskCleanupHeldSince(m)
	if since.IsZero() {
		return 0, false
	}
	if elapsed := now.Sub(since); elapsed < blockedRetryMin {
		return blockedRetryMin - elapsed, true
	}
	return 0, false
}

// retainLandedDiskIfNever records LandedDiskCleanup=True/LandedDiskRetained
// when a landed disk may exist and cleanupPolicy is Never. It reports whether
// that is the case.
func (r *VMMigrationReconciler) retainLandedDiskIfNever(ctx context.Context, m *infrav1beta1.VMMigration) (bool, error) {
	if !landedDiskMayExist(m) || migrationCleanupPolicy(m) != infrav1beta1.CleanupPolicyNever {
		return false, nil
	}
	return true, r.setLandedDiskDone(ctx, m, landedDiskDoneRetained)
}

// reconcileLandedDiskCleanup runs the landed-disk cleanup of a migration that
// failed for good (stagingTriggerTerminalFailure). It returns how soon a held
// delete must be retried (0: nothing to retry) and an error only when a status
// write fails. It never blocks the migration's other cleanup.
func (r *VMMigrationReconciler) reconcileLandedDiskCleanup(ctx context.Context, m *infrav1beta1.VMMigration, trigger stagingCleanupTrigger) (time.Duration, error) {
	if retained, err := r.retainLandedDiskIfNever(ctx, m); retained || err != nil {
		return 0, err
	}
	if !landedDiskCleanupWanted(m, trigger) {
		return 0, nil
	}
	if wait, notDue := landedDiskCleanupNotDue(m, time.Now()); notDue {
		return wait, nil
	}
	done, err := r.cleanupLandedDisk(ctx, m)
	var left *landedDiskLeftBehindError
	switch {
	case err == nil:
		logging.FromContext(ctx).Info("Migration landed disk cleanup done", "reason", done.reason)
		return 0, r.setLandedDiskDone(ctx, m, done)
	case stderrors.As(err, &left):
		logLandedDiskLeftBehind(ctx, left)
		return 0, r.setLandedDiskDone(ctx, m, landedDiskLeftBehindDone(left.message))
	default:
		return r.holdLandedDiskCleanup(ctx, m, err)
	}
}

// releaseLandedDiskForDeletion runs the landed-disk step of a VMMigration's
// deletion. hold == true keeps the finalizer and the caller returns (res,
// err): the landed disk may still be on the target and its delete can still
// succeed. hold == false lets the deletion proceed: nothing to delete, the
// disk is deleted or kept for good (a VM uses it, another migration lands the
// same name, cleanupPolicy Never), its delete can never succeed (a
// LandedDiskLeftBehind Warning), or force-delete says to leave it behind (also
// a LandedDiskLeftBehind Warning). Each outcome is persisted BEFORE the
// finalizer is removed.
func (r *VMMigrationReconciler) releaseLandedDiskForDeletion(ctx context.Context, m *infrav1beta1.VMMigration) (bool, ctrl.Result, error) {
	if retained, err := r.retainLandedDiskIfNever(ctx, m); retained || err != nil {
		return err != nil, ctrl.Result{}, err
	}
	if !landedDiskCleanupWanted(m, stagingTriggerDeletion) {
		return false, ctrl.Result{}, nil
	}
	force := hasMigrationForceDeleteAnnotation(m)
	if wait, notDue := landedDiskCleanupNotDue(m, time.Now()); notDue {
		if !force {
			return true, ctrl.Result{RequeueAfter: wait}, nil
		}
		if werr := r.recordLandedDiskForceLeft(ctx, m, nil); werr != nil {
			return true, ctrl.Result{}, werr
		}
		return false, ctrl.Result{}, nil
	}

	done, err := r.cleanupLandedDisk(ctx, m)
	var left *landedDiskLeftBehindError
	switch {
	case err == nil:
		logging.FromContext(ctx).Info("Migration landed disk cleanup done during deletion", "reason", done.reason)
		if werr := r.setLandedDiskDone(ctx, m, done); werr != nil {
			return true, ctrl.Result{}, werr
		}
		return false, ctrl.Result{}, nil
	case stderrors.As(err, &left):
		logLandedDiskLeftBehind(ctx, left)
		if werr := r.setLandedDiskDone(ctx, m, landedDiskLeftBehindDone(left.message)); werr != nil {
			return true, ctrl.Result{}, werr
		}
		return false, ctrl.Result{}, nil
	case force:
		if werr := r.recordLandedDiskForceLeft(ctx, m, err); werr != nil {
			return true, ctrl.Result{}, werr
		}
		return false, ctrl.Result{}, nil
	default:
		retry, herr := r.holdLandedDiskCleanup(ctx, m, err)
		return true, ctrl.Result{RequeueAfter: retry}, herr
	}
}

// landedDiskLeftBehindDone is the LandedDiskLeftBehind outcome with message
// (a landedDiskLeft* constant).
func landedDiskLeftBehindDone(message string) landedDiskDone {
	return landedDiskDone{migrationReasonLandedDiskLeftBehind, message + migrationLandedDiskLeftBehindSuffix, corev1.EventTypeWarning}
}

// setLandedDiskDone records LandedDiskCleanup=True with done's reason and
// message, and — when that changes the stored status and done has an event
// type — one event with the same reason and message.
func (r *VMMigrationReconciler) setLandedDiskDone(ctx context.Context, m *infrav1beta1.VMMigration, done landedDiskDone) error {
	if !meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type:               migrationConditionLandedDiskCleanup,
		Status:             metav1.ConditionTrue,
		Reason:             done.reason,
		Message:            done.message,
		ObservedGeneration: m.Generation,
	}) {
		return nil
	}
	if done.eventType != "" {
		r.Recorder.Event(m, done.eventType, done.reason, done.message)
	}
	return r.updateStatus(ctx, m)
}

// holdLandedDiskCleanup records a held landed-disk delete —
// LandedDiskCleanup=False/LandedDiskCleanupPending with the constant
// migrationLandedDiskCleanupPendingMessage, and one Warning event when the
// hold begins — only when that changes the stored status, and returns the
// blocked-VM backoff (15 s doubling to 5 min, from when the hold began). cause
// goes to the manager log only.
func (r *VMMigrationReconciler) holdLandedDiskCleanup(ctx context.Context, m *infrav1beta1.VMMigration, cause error) (time.Duration, error) {
	retry := blockedRetryBackoff(landedDiskCleanupHeldSince(m))
	logging.FromContext(ctx).Info(migrationLandedDiskCleanupLogMessage, "retryAfter", retry.String(), "error", cause.Error())

	before := m.Status.DeepCopy()
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type:               migrationConditionLandedDiskCleanup,
		Status:             metav1.ConditionFalse,
		Reason:             migrationReasonLandedDiskCleanupPending,
		Message:            migrationLandedDiskCleanupPendingMessage,
		ObservedGeneration: m.Generation,
	})
	if equality.Semantic.DeepEqual(before, &m.Status) {
		return retry, nil
	}
	if c := meta.FindStatusCondition(before.Conditions, migrationConditionLandedDiskCleanup); c == nil || c.Status != metav1.ConditionFalse {
		r.Recorder.Event(m, corev1.EventTypeWarning, migrationReasonLandedDiskCleanupPending, migrationLandedDiskCleanupPendingMessage)
	}
	if err := r.updateStatus(ctx, m); err != nil {
		return 0, err
	}
	return retry, nil
}

// recordLandedDiskForceLeft records that force-delete removes the VMMigration
// although its landed disk was not deleted: LandedDiskCleanup=True with reason
// LandedDiskLeftBehind and one Warning (naming no location), persisted before
// the finalizer is removed; the cause goes to the log at V(1).
func (r *VMMigrationReconciler) recordLandedDiskForceLeft(ctx context.Context, m *infrav1beta1.VMMigration, cause error) error {
	logger := logging.FromContext(ctx)
	logger.Info("Landed disk not deleted but force-delete annotation is set; removing finalizer, the landed disk is left behind",
		"annotation", forceDeleteAnnotation)
	if cause != nil {
		logger.V(1).Info("Landed disk force-delete cause", "error", cause.Error())
	}
	return r.setLandedDiskDone(ctx, m, landedDiskDone{
		reason: migrationReasonLandedDiskLeftBehind,
		message: fmt.Sprintf("%s=true: removing the VMMigration although its landed disk was not deleted.%s",
			forceDeleteAnnotation, migrationLandedDiskLeftBehindSuffix),
		eventType: corev1.EventTypeWarning,
	})
}

// logLandedDiskLeftBehind logs a left-behind landed disk: the constant reason
// at Info, the cause (which can name a host, a path or a provider answer) at
// V(1) only.
func logLandedDiskLeftBehind(ctx context.Context, left *landedDiskLeftBehindError) {
	logger := logging.FromContext(ctx)
	logger.Info("Migration landed disk left behind", "reason", left.message)
	if left.cause != nil {
		logger.V(1).Info("Migration landed disk left behind: cause", "error", left.cause.Error())
	}
}

// cleanupLandedDisk decides and, when due, performs the landed-disk delete. It
// returns the final outcome, a landedDiskLeftBehindError when the delete can
// never succeed, or any other error when a retry can still succeed.
//
// The disk is kept (landedDiskDoneInUse) when the migration succeeded (its
// target VM owns the disk), and when a VirtualMachine exists under the name
// the import was sent for that is the migration's own target (status.
// targetVMUID or its claim) or that names an imported disk at all — it is, or
// is about to be, attached; its own deletion removes it. It is handed over
// when another VMMigration in flight targets, or recorded an import for, the
// same VirtualMachine. Otherwise DeleteLandedDisk is sent to the target
// Provider the import was sent to (the recorded UID, resolved through the
// live grant check), naming only the recorded VirtualMachine, never the
// current spec.target. The provider refuses (VM_DISK_IN_USE) a disk any VM
// on it uses; that disk is kept too.
func (r *VMMigrationReconciler) cleanupLandedDisk(ctx context.Context, m *infrav1beta1.VMMigration) (landedDiskDone, error) {
	if m.Status.Phase == infrav1beta1.MigrationPhaseReady {
		return landedDiskDoneInUse, nil
	}
	if m.Status.StorageInfo == nil || m.Status.StorageInfo.ImportTarget == nil {
		return landedDiskDone{}, landedLeftBehind(landedDiskLeftUnrecorded, nil)
	}
	recorded := *m.Status.StorageInfo.ImportTarget
	key, ok := recordedImportTarget(m)
	if !ok {
		return landedDiskDone{}, landedLeftBehind(landedDiskLeftUnrecorded, nil)
	}
	live := r.liveReader()
	allowed, err := targetNamespaceAllowed(ctx, live, m.Namespace, key.Namespace)
	if err != nil {
		return landedDiskDone{}, fmt.Errorf("check the import target namespace grant: %w", err)
	}
	if !allowed {
		return landedDiskDone{}, landedLeftBehind(landedDiskLeftNamespace, nil)
	}

	vm := &infrav1beta1.VirtualMachine{}
	switch err := live.Get(ctx, key, vm); {
	case err == nil:
		if migrationOwnsTargetVM(m, vm) || vm.Spec.ImportedDisk != nil {
			return landedDiskDoneInUse, nil
		}
	case !apierrors.IsNotFound(err):
		return landedDiskDone{}, fmt.Errorf("get the import target VirtualMachine: %w", err)
	}

	migrations := &infrav1beta1.VMMigrationList{}
	if err := r.List(ctx, migrations); err != nil {
		return landedDiskDone{}, fmt.Errorf("list VMMigrations sharing the target VM: %w", err)
	}
	for i := range migrations.Items {
		other := &migrations.Items[i]
		if other.UID == m.UID || !migrationInFlight(other) {
			continue
		}
		if migrationTargetVMKey(other) == key {
			return landedDiskDoneHandedOver, nil
		}
		if otherKey, ok := recordedImportTarget(other); ok && otherKey == key {
			return landedDiskDoneHandedOver, nil
		}
	}

	provider, err := r.targetProviderFor(ctx, live, m)
	if apierrors.IsNotFound(err) && !isConsumerNotAllowed(err) {
		return landedDiskDone{}, landedLeftBehind(landedDiskLeftProviderGone, err)
	}
	if err != nil {
		return landedDiskDone{}, fmt.Errorf("resolve target provider: %w", err)
	}
	if recorded.ProviderUID != "" && string(provider.UID) != recorded.ProviderUID {
		return landedDiskDone{}, landedLeftBehind(landedDiskLeftProviderChanged, nil)
	}
	instance, err := r.getProviderInstance(ctx, provider)
	if err != nil {
		return landedDiskDone{}, fmt.Errorf("get target provider instance: %w", err)
	}
	deleter, ok := instance.(contracts.LandedDiskDeleter)
	if !ok {
		return landedDiskDone{}, landedLeftBehind(landedDiskLeftUnsupported, stderrors.New("the provider client has no DeleteLandedDisk"))
	}
	reporter, ok := instance.(contracts.CapabilityReporter)
	if !ok {
		return landedDiskDone{}, landedLeftBehind(landedDiskLeftUnsupported, stderrors.New("the provider client reports no capabilities"))
	}
	caps, err := reporter.GetCapabilities(ctx)
	if err != nil {
		return landedDiskDone{}, fmt.Errorf("query target provider capabilities: %w", err)
	}
	if !caps.SupportsLandedDiskDelete {
		return landedDiskDone{}, landedLeftBehind(landedDiskLeftUnsupported, stderrors.New("supportsLandedDiskDelete is false"))
	}

	path := ""
	if m.Status.DiskInfo != nil {
		path = m.Status.DiskInfo.TargetPath
	}
	_, err = deleter.DeleteLandedDisk(ctx, contracts.LandedDiskDeleteRequest{
		Migration: contracts.ObjectIdentity{UID: string(m.UID), Namespace: m.Namespace, Name: m.Name},
		TargetVM:  contracts.ObjectIdentity{Namespace: key.Namespace, Name: key.Name},
		Path:      path,
	})
	switch {
	case err == nil:
		return landedDiskDoneDeleted, nil
	case contracts.IsVMDiskInUse(err):
		return landedDiskDoneInUse, nil
	case contracts.IsNotSupported(err):
		return landedDiskDone{}, landedLeftBehind(landedDiskLeftUnsupported, err)
	case contracts.IsInvalidSpec(err):
		return landedDiskDone{}, landedLeftBehind(landedDiskLeftRefused, err)
	default:
		return landedDiskDone{}, fmt.Errorf("delete landed disk: %w", err)
	}
}
