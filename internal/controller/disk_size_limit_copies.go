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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A clone and a migration copy the source VM's REAL disks, whatever its spec
// says now: a VMClass lowered after the VM was created, a spec.disks entry
// removed (no provider detaches a disk for that), or an adopted VM's disks.
// So they are checked against the limit by the sizes the source's provider
// reports (GetDiskInfo), never by the spec. A size that cannot be read while
// a limit applies refuses the copy (DiskSizeLimitUnverifiable).

const (
	// cloneSourceDisksUnreadableMessage is the hold message of a clone whose
	// source disk sizes the provider did not report.
	cloneSourceDisksUnreadableMessage = "The sizes of the source VM's disks cannot be read from its provider (it does not " +
		"report them, or one is unknown), so they cannot be checked against the disk-size limit: nothing is cloned"
	// migrationSourceDiskUnreadableMessage is the hold message of a migration
	// whose source disk size the provider did not report.
	migrationSourceDiskUnreadableMessage = "The size of the source VM's disk cannot be read from its provider, so it " +
		"cannot be checked against the disk-size limit: nothing is migrated"
	// migrationSizeNotRecordedMessage is the hold message of a migration that
	// reaches ImportDisk with no recorded size for the exported disk (it was
	// exported before a limit applied, or by an earlier release).
	migrationSizeNotRecordedMessage = "The size of the exported disk was not recorded when it was exported, so it " +
		"cannot be checked against the disk-size limit: nothing is imported. Delete this VMMigration and migrate again"
	// cloneOutcome and migrationOutcome complete a DiskSizeExceedsLimit
	// message.
	cloneOutcome     = "nothing is cloned"
	migrationOutcome = "nothing is migrated"
)

// unverifiable is a DiskSizeLimitUnverifiable refusal with message.
func unverifiable(message string) *diskSizeRefusal {
	return &diskSizeRefusal{reason: k8s.ReasonDiskSizeLimitUnverifiable, message: message}
}

// bytesQuantity is n bytes as a binary quantity.
func bytesQuantity(n int64) resource.Quantity {
	return *resource.NewQuantity(n, resource.BinarySI)
}

// cloneDiskSizeRefusal checks what a clone would create on provider against
// the limit that applies there: every disk of the source VM, by the sizes its
// provider reports (GetDiskInfo through inst, for the VM sourceRef
// addresses), and the primary-disk size of the clone's target VMClass
// override. It returns nil when all fit or no limit applies, and an error for
// a failed Kubernetes read (retried) or, as a *sourceOwnerRefusedError, when
// the provider refused the source VM as not its VirtualMachine's (the caller
// fails the clone).
func (r *VMCloneReconciler) cloneDiskSizeRefusal(ctx context.Context, clone *infravirtrigaudiov1beta1.VMClone,
	sourceVM *infravirtrigaudiov1beta1.VirtualMachine, sourceRef contracts.VMRef, inst contracts.Provider,
	provider *infravirtrigaudiov1beta1.Provider) (*diskSizeRefusal, error) {
	limit, err := infravirtrigaudiov1beta1.EffectiveMaxDiskSize(r.MaxDiskSize, provider)
	if err != nil {
		return unverifiable(diskSizeLimitUnverifiableMessage), nil
	}
	if limit == nil {
		return nil, nil
	}
	var disks []infravirtrigaudiov1beta1.DiskSizeRequest
	if ref := clone.Spec.Target.ClassRef; ref != nil && ref.Name != "" {
		req, sized, _, err := classDiskSizeRequest(ctx, r.Client, types.NamespacedName{Namespace: clone.Namespace, Name: ref.Name})
		if err != nil {
			return nil, err
		}
		if sized {
			disks = append(disks, req)
		}
	}
	info, err := inst.GetDiskInfo(ctx, contracts.GetDiskInfoRequest{VM: sourceRef})
	if isOwnerRefusal(err) {
		return nil, &sourceOwnerRefusedError{err: err}
	}
	if err != nil || len(info.Disks) == 0 {
		log.FromContext(ctx).Info("Cannot read the source VM's disk sizes; not cloning while a disk-size limit applies",
			"vm", sourceVM.Name, "disks", len(info.Disks), "error", errString(err))
		return unverifiable(cloneSourceDisksUnreadableMessage), nil
	}
	for i, d := range info.Disks {
		if d.VirtualSizeBytes <= 0 {
			return unverifiable(cloneSourceDisksUnreadableMessage), nil
		}
		// The provider's disk id or path is never echoed into status: the
		// disk is named by its position.
		disks = append(disks, infravirtrigaudiov1beta1.DiskSizeRequest{
			Field: fmt.Sprintf("disk %d of source VM %s", i, sourceVM.Name),
			Size:  bytesQuantity(d.VirtualSizeBytes),
		})
	}
	return checkDiskSizeLimit(r.MaxDiskSize, provider, disks, cloneOutcome), nil
}

// errString is err's text, or "" for nil (for log key/values).
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// holdCloneForDiskSizeLimit keeps clone Pending (no Clone RPC, no target VM)
// with refusal as its Ready=False reason and message, written only when it
// changes (markPending), with a Warning event when the hold begins or its
// message changes; it is re-checked every diskSizeLimitRetryInterval.
func (r *VMCloneReconciler) holdCloneForDiskSizeLimit(ctx context.Context, clone *infravirtrigaudiov1beta1.VMClone, refusal *diskSizeRefusal) ctrl.Result {
	prev := meta.FindStatusCondition(clone.Status.Conditions, infravirtrigaudiov1beta1.VMCloneConditionReady)
	isNew := prev == nil || prev.Reason != refusal.reason || prev.Message != refusal.message
	log.FromContext(ctx).Info("Not cloning: a disk is above the disk-size limit, or its size is unknown", "reason", refusal.reason)
	metrics.RecordError(errReasonDiskSizeLimit, metrics.ComponentManager)
	r.markPending(ctx, clone, refusal.reason, refusal.message)
	if isNew && r.Recorder != nil {
		r.Recorder.Event(clone, corev1.EventTypeWarning, refusal.reason, refusal.message)
	}
	return ctrl.Result{RequeueAfter: diskSizeLimitRetryInterval}
}

// migrationTargetLimit is the disk-size limit of migration's target Provider:
// (nil, nil) when none applies, or the refusal of an invalid annotation.
func (r *VMMigrationReconciler) migrationTargetLimit(targetProvider *infravirtrigaudiov1beta1.Provider) (*resource.Quantity, *diskSizeRefusal) {
	limit, err := infravirtrigaudiov1beta1.EffectiveMaxDiskSize(r.MaxDiskSize, targetProvider)
	if err != nil {
		return nil, unverifiable(diskSizeLimitUnverifiableMessage)
	}
	return limit, nil
}

// readSourceDiskSize asks inst for the virtual size of the disk req names. ok
// is false when it cannot be read (an error, or a size of 0); the cause is
// logged, never put in status. An owner refusal (isOwnerRefusal) is returned
// as a *sourceOwnerRefusedError instead: the source is not the source
// VirtualMachine's VM, and the copy must fail rather than wait.
func readSourceDiskSize(ctx context.Context, inst contracts.Provider, req contracts.GetDiskInfoRequest) (resource.Quantity, bool, error) {
	info, err := inst.GetDiskInfo(ctx, req)
	if isOwnerRefusal(err) {
		return resource.Quantity{}, false, &sourceOwnerRefusedError{err: err}
	}
	if err != nil || info.VirtualSizeBytes <= 0 {
		log.FromContext(ctx).Info("Cannot read the source disk's size", "vm", req.VM.ID, "error", errString(err))
		return resource.Quantity{}, false, nil
	}
	return bytesQuantity(info.VirtualSizeBytes), true, nil
}

// migrationDiskSizeRefusal is the Validating-phase check of what a migration
// would create on its target Provider: the source VM's disk — the exported
// primary disk, by the size its provider reports now (GetDiskInfo), never by
// the source's spec — and the primary-disk size of the target VMClass (the
// target VM's disk is grown to it). It returns nil when all fit or no limit
// applies, and an error for a failed read (retried) or, as a
// *sourceOwnerRefusedError, a source the provider refused as not the source
// VirtualMachine's (the caller fails the migration). The exported disk
// is checked again, and its size recorded, right before ExportDisk, and that
// record is checked right before ImportDisk.
func (r *VMMigrationReconciler) migrationDiskSizeRefusal(ctx context.Context, migration *infravirtrigaudiov1beta1.VMMigration,
	sourceVM *infravirtrigaudiov1beta1.VirtualMachine, sourceRef contracts.VMRef, sourceProvider, targetProvider *infravirtrigaudiov1beta1.Provider) (*diskSizeRefusal, error) {
	limit, refusal := r.migrationTargetLimit(targetProvider)
	if refusal != nil || limit == nil {
		return refusal, nil
	}
	var disks []infravirtrigaudiov1beta1.DiskSizeRequest
	// A target class that does not exist leaves the target VM waiting for it:
	// it grows nothing.
	if ref := migration.Spec.Target.ClassRef; ref != nil && ref.Name != "" {
		req, sized, _, err := classDiskSizeRequest(ctx, r.Client, types.NamespacedName{Namespace: migration.Namespace, Name: ref.Name})
		if err != nil {
			return nil, err
		}
		if sized {
			disks = append(disks, req)
		}
	}
	inst, err := r.getProviderInstance(ctx, sourceProvider)
	if err != nil {
		return nil, fmt.Errorf("resolve the source provider to read the disk size: %w", err)
	}
	size, ok, err := readSourceDiskSize(ctx, inst, contracts.GetDiskInfoRequest{VM: sourceRef})
	if err != nil {
		return nil, err
	}
	if !ok {
		return unverifiable(migrationSourceDiskUnreadableMessage), nil
	}
	disks = append(disks, infravirtrigaudiov1beta1.DiskSizeRequest{
		Field: fmt.Sprintf("the disk of source VM %s", sourceVM.Name), Size: size,
	})
	if refusal := checkDiskSizeLimit(r.MaxDiskSize, targetProvider, disks, migrationOutcome); refusal != nil {
		return refusal, nil
	}
	// The import-time check must run too: refuse early, before any export,
	// a target provider that would ignore the limit.
	target, err := r.getProviderInstance(ctx, targetProvider)
	if err != nil {
		return nil, fmt.Errorf("resolve the target provider to check its import-time disk-size limit: %w", err)
	}
	return targetImportLimitRefusal(ctx, target, limit)
}

// exportDiskSizeRefusal is the check right before ExportDisk: the disk
// exportReq exports (the same VM, disk and snapshot) is read through inst and
// checked against the target Provider's limit. It returns the size read, to be
// recorded in status.diskInfo.sourceSize for the check before ImportDisk; no
// size and no refusal when no limit applies (nothing changes then). A source
// the provider refused as not the source VirtualMachine's is returned as a
// *sourceOwnerRefusedError (the caller fails the migration).
func (r *VMMigrationReconciler) exportDiskSizeRefusal(ctx context.Context, migration *infravirtrigaudiov1beta1.VMMigration,
	inst contracts.Provider, exportReq contracts.ExportDiskRequest) (*diskSizeRefusal, *resource.Quantity, error) {
	targetProvider, err := r.getTargetProvider(ctx, migration)
	if err != nil {
		// Without a manager-wide ceiling a target Provider that cannot be
		// read here (gone, or not usable from this namespace) sets no limit
		// at this point: the export goes on exactly as without this check,
		// and the import — which needs that Provider — re-reads it and, if it
		// carries a limit, refuses for want of a recorded size. With a
		// ceiling, wait for it.
		if r.MaxDiskSize == nil {
			log.FromContext(ctx).Info("Target provider unreadable before export; no disk-size limit checked here", "error", err.Error())
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("get the target provider to check the disk-size limit: %w", err)
	}
	limit, refusal := r.migrationTargetLimit(targetProvider)
	if refusal != nil || limit == nil {
		return refusal, nil, nil
	}
	size, ok, err := readSourceDiskSize(ctx, inst, contracts.GetDiskInfoRequest{
		VM: exportReq.VM, DiskId: exportReq.DiskId, SnapshotId: exportReq.SnapshotId,
	})
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return unverifiable(migrationSourceDiskUnreadableMessage), nil, nil
	}
	refusal = checkDiskSizeLimit(r.MaxDiskSize, targetProvider, []infravirtrigaudiov1beta1.DiskSizeRequest{{
		Field: fmt.Sprintf("the disk of source VM %s", migration.Spec.Source.VMRef.Name), Size: size,
	}}, migrationOutcome)
	return refusal, &size, nil
}

// importDiskSizeRefusal is the check right before ImportDisk: the exported
// disk's size, recorded at export (status.diskInfo.sourceSize), against the
// target Provider's limit now. A limit with no recorded size refuses the
// import (fail closed).
func (r *VMMigrationReconciler) importDiskSizeRefusal(migration *infravirtrigaudiov1beta1.VMMigration,
	targetProvider *infravirtrigaudiov1beta1.Provider) *diskSizeRefusal {
	limit, refusal := r.migrationTargetLimit(targetProvider)
	if refusal != nil || limit == nil {
		return refusal
	}
	if migration.Status.DiskInfo == nil || migration.Status.DiskInfo.SourceSize == nil {
		return unverifiable(migrationSizeNotRecordedMessage)
	}
	return checkDiskSizeLimit(r.MaxDiskSize, targetProvider, []infravirtrigaudiov1beta1.DiskSizeRequest{{
		Field: fmt.Sprintf("the exported disk of source VM %s", migration.Spec.Source.VMRef.Name),
		Size:  *migration.Status.DiskInfo.SourceSize,
	}}, "nothing is imported")
}

// providerImportRefusedMessage is the Importing=False message of an import
// the target provider refused because the disk it would land is above limit
// (in bytes). It names the limit, so a changed limit is a new message — and
// importRefusedByTargetAt lets the import be sent again.
func providerImportRefusedMessage(limitBytes int64) string {
	return fmt.Sprintf("The target provider refused to import the staged disk: its virtual size is above the disk-size "+
		"limit %s (the staged object differs from what was exported, or was replaced). Nothing was imported. Delete this "+
		"VMMigration and migrate again, or raise the limit", resource.NewQuantity(limitBytes, resource.BinarySI).String())
}

// importUnverifiableMaxRetryInterval caps the doubling wait before an import
// refused as DISK_SIZE_UNVERIFIABLE is sent again (2, 4, 8, 16, 32 minutes).
// It stays below the hour after which the backoff forgets a run of failures.
const importUnverifiableMaxRetryInterval = 32 * time.Minute

// providerImportUnverifiableMessage is the Importing=False message of an
// import the target provider refused because it could not read the disk's
// virtual size while a limit applies. The import is sent again (the read may
// have failed transiently) on a doubling backoff: each attempt is a full
// download or convert.
const providerImportUnverifiableMessage = "The target provider could not read the virtual size of the staged disk, and a " +
	"disk-size limit applies: nothing was imported. The import is retried after 2 minutes, then waits twice as long " +
	"each time, up to 32 minutes"

// providerImportRefusal maps an ImportDisk error to the hold it calls for: a
// provider's DISK_SIZE_EXCEEDS_LIMIT or DISK_SIZE_UNVERIFIABLE refusal of an
// import sent with limitBytes; nil for any other error. Its messages are
// constant for a given limit; the provider's own text is never shown.
func providerImportRefusal(err error, limitBytes int64) *diskSizeRefusal {
	switch {
	case contracts.IsDiskSizeExceedsLimit(err):
		return &diskSizeRefusal{reason: k8s.ReasonDiskSizeExceedsLimit, message: providerImportRefusedMessage(limitBytes)}
	case contracts.IsDiskSizeUnverifiable(err):
		return unverifiable(providerImportUnverifiableMessage)
	}
	return nil
}

// importRefusedByTargetAt reports whether migration is held because its
// target provider refused the import for this same limit: the refusal is a
// fact about the staged disk, so the import (a full download or convert) is
// not sent again until the limit changes.
func importRefusedByTargetAt(migration *infravirtrigaudiov1beta1.VMMigration, limit *resource.Quantity) bool {
	c := meta.FindStatusCondition(migration.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionImporting)
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == k8s.ReasonDiskSizeExceedsLimit &&
		c.Message == providerImportRefusedMessage(limit.Value())
}

// importSizeLimitUnsupportedMessage is the hold message of a migration whose
// target provider does not enforce the limit at the import.
const importSizeLimitUnsupportedMessage = "The target provider does not enforce the disk-size limit when it imports " +
	"a disk (it does not report supports_import_size_limit): upgrade the provider image. Nothing is migrated into it " +
	"while a disk-size limit applies"

// targetImportLimitRefusal is the refusal of a migration into inst (the
// target Provider's instance) while limit applies and the provider does not
// report CapabilityImportSizeLimit: an older provider image silently ignores
// ImportDiskRequest.MaxVirtualSizeBytes, so the import-time check — the one
// that holds against a replaced staged object — would not run. nil when no
// limit applies (nothing changes then) or the provider reports it. An error
// only for a failed capability query (retried).
func targetImportLimitRefusal(ctx context.Context, inst contracts.Provider, limit *resource.Quantity) (*diskSizeRefusal, error) {
	if limit == nil {
		return nil, nil
	}
	reporter, ok := inst.(contracts.CapabilityReporter)
	if !ok {
		return unverifiable(importSizeLimitUnsupportedMessage), nil
	}
	caps, err := reporter.GetCapabilities(ctx)
	if err != nil {
		return nil, fmt.Errorf("query the target provider's capabilities: %w", err)
	}
	if !caps.SupportsImportSizeLimit {
		return unverifiable(importSizeLimitUnsupportedMessage), nil
	}
	return nil, nil
}

// holdMigrationForDiskSizeLimit keeps migration in its current phase — no
// further provider call for it — with refusal as the condType=False reason
// and message (Validating, Exporting or Importing), written only when it
// changes, with a Warning event when the hold begins or its message changes;
// it is re-checked every diskSizeLimitRetryInterval.
func (r *VMMigrationReconciler) holdMigrationForDiskSizeLimit(ctx context.Context, migration *infravirtrigaudiov1beta1.VMMigration,
	condType string, refusal *diskSizeRefusal) (ctrl.Result, error) {
	log.FromContext(ctx).Info("Not migrating: a disk is above the disk-size limit, or its size is unknown",
		"reason", refusal.reason, "phase", migration.Status.Phase)
	metrics.RecordError(errReasonDiskSizeLimit, metrics.ComponentManager)
	prev := meta.FindStatusCondition(migration.Status.Conditions, condType)
	if prev != nil && prev.Status == metav1.ConditionFalse && prev.Reason == refusal.reason && prev.Message == refusal.message {
		return ctrl.Result{RequeueAfter: diskSizeLimitRetryInterval}, nil
	}
	meta.SetStatusCondition(&migration.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionFalse,
		Reason:             refusal.reason,
		Message:            refusal.message,
		ObservedGeneration: migration.Generation,
	})
	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	if r.Recorder != nil {
		r.Recorder.Event(migration, corev1.EventTypeWarning, refusal.reason, refusal.message)
	}
	return ctrl.Result{RequeueAfter: diskSizeLimitRetryInterval}, nil
}
