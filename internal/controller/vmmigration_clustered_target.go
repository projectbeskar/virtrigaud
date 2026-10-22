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
	"math"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	conditions "github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Migration into a clustered Provider (ADR-0007 Addendum B, slice B.3): the
// VMMigration controller's half. Only a target Provider with topology: cluster
// that reports supports_routed_import takes this path (B5); single-host,
// vSphere and Proxmox targets keep import-then-create, unchanged (B7.8).
//
//   - Validating, after today's checks and before the source is powered off,
//     snapshotted or exported: the target VirtualMachine is created first,
//     with #394's claim, and spec.importedDisk naming the migration but no
//     disk (an empty diskID, B7.1) and the source disk's size (sizeGiB, the
//     B7.4 disk-space filter). The VirtualMachine controller places it — its
//     capacity is reserved from here on (B7.2) — and holds it
//     (WaitingForImport). The migration waits with TargetPlaced until the
//     target's status.placement.pendingHost is recorded; a target that cannot
//     be placed waits without touching the source and never fails the
//     migration on its own (B1.1).
//   - Importing: the target is re-read live. One with a status.id, or no
//     pendingHost, fails the attempt (TargetNotPending) — except one whose
//     pending host this migration released after a name conflict, which is
//     waited for. Its pendingHost is recorded in importTarget.host (with the
//     Provider's UID and the LandedDiskMayExist marker) BEFORE the R6
//     ImportRequested marker, and ImportDisk is sent with target_host_id and
//     the VM's UID. A re-send goes to the recorded host only; a pendingHost
//     that differs from it holds the migration (ImportOutcomeUnknown).
//   - Creating: the migration does not create a VM. It writes the landed disk
//     into its own target's spec.importedDisk under a resourceVersion
//     precondition (the live object it read); the VirtualMachine controller
//     then creates the VM on its pending host with the disk attached in place.
//   - A definite import refusal: a foreign or unstamped domain with the
//     target's name on the landing host (a plain AlreadyExists) releases the
//     target's pending host — one checked status write clearing pendingHost
//     and adding the host to excludedHosts (B1.2) — and the import follows the
//     VM to its next host. A previous incarnation, a landing file in use, an
//     unreachable host hold the migration instead.
//   - B1.4: a migration that fails for good, is retried or is deleted before
//     its target is bound deletes that unbound target first (UID
//     precondition); the landed-disk cleanup runs only once it is gone.

const (
	// migrationConditionTargetPlaced is the VMMigration condition of a
	// clustered target's placement (ADR-0007 Addendum B, B1): True once the
	// target VirtualMachine has a pending host, False (with why) while the
	// migration waits for it in Validating, or after it released the pending
	// host for a name conflict in Importing.
	migrationConditionTargetPlaced = "TargetPlaced"

	// migrationReasonTargetPlaced: the target VM has a pending host.
	migrationReasonTargetPlaced = "TargetPlaced"
	// migrationReasonWaitingForPlacement: the target VM was created and is not
	// placed yet (no more specific reason on the VM).
	migrationReasonWaitingForPlacement = "WaitingForPlacement"
	// migrationReasonTargetHostReleased: the migration released its target's
	// pending host after the landing host refused the import with a name
	// conflict; it waits for the VM to be placed again.
	migrationReasonTargetHostReleased = "TargetHostReleased"
	// migrationReasonSourceDiskSizeUnknown: the source disk's size cannot be
	// read yet, so the target cannot be placed by disk space (B7.4).
	migrationReasonSourceDiskSizeUnknown = "SourceDiskSizeUnknown"
	// migrationReasonTargetVMDeleting: the target VM this migration created is
	// being deleted; the migration waits for it to be gone.
	migrationReasonTargetVMDeleting = "TargetVMDeleting"

	// ReasonTargetNotPending is the failure reason of an import (or a
	// Validating check) whose clustered target VM is not provably pending: it
	// already has a status.id, or no pending host.
	ReasonTargetNotPending = "TargetNotPending"
	// ReasonClusteredTargetUnsupported is the failure reason of a migration
	// into a clustered Provider that does not report supports_routed_import.
	ReasonClusteredTargetUnsupported = "ClusteredTargetUnsupported"

	// Importing=False hold reasons of a routed import (B1.2, B4).
	migrationReasonTargetHostUnavailable           = "TargetHostUnavailable"
	migrationReasonImportTargetPreviousIncarnation = "ImportTargetPreviousIncarnation"
	migrationReasonLandingDiskInUse                = "LandingDiskInUse"
	// migrationReasonLandingHostRefused: the landing host refused the import
	// before writing anything (a guard refusal, FailedPrecondition).
	migrationReasonLandingHostRefused = "LandingHostRefused"
	// migrationReasonTargetHostMoved is the Ready=False hold of a migration
	// whose target's pending host is no longer the host its disk landed on.
	migrationReasonTargetHostMoved = "TargetHostMoved"
	// migrationReasonCapabilitiesUnknown: the target provider's capabilities
	// cannot be read yet (Validating waits).
	migrationReasonCapabilitiesUnknown = "TargetCapabilitiesUnknown"

	// eventReasonTargetHostReleased is the Normal event of a B1.2 release.
	eventReasonTargetHostReleased = "TargetHostReleased"
	// eventReasonUnboundTargetDeleted is the Normal event of a B1.4 delete.
	eventReasonUnboundTargetDeleted = "UnboundTargetVMDeleted"
	// eventReasonTargetVMImportRecorded is the Normal event of the Creating
	// phase writing the landed disk into the target VM.
	eventReasonTargetVMImportRecorded = "TargetVMImportRecorded"

	// importedDiskSourceMigration is spec.importedDisk.source of a VM a
	// migration created.
	importedDiskSourceMigration = "migration"

	// unboundTargetDeleteRequeue re-checks a target VM whose deletion was
	// requested (its finalizer runs an owner-checked Delete on its pending
	// host).
	unboundTargetDeleteRequeue = 5 * time.Second
)

// Constant messages. None names a host, a path or another object's state.
const (
	clusteredTargetUnsupportedMessage = "the target provider has topology: cluster and does not report " +
		"supportsRoutedImport (a clustered provider older than ADR-0007 Addendum B), so the disk import cannot be " +
		"routed to a host: migrating into it is refused before any side effect. Upgrade the provider and run the " +
		"migration again"
	clusteredTargetCrossNamespaceMessage = "a clustered target VirtualMachine must be in the VMMigration's own namespace " +
		"(spec.target.namespace unset or equal to it): its landed disk is attached in place only for a VM whose " +
		"migration is in its namespace. Run the migration from the target namespace"
	clusteredTargetCapabilitiesMessage ="waiting for the clustered target provider's capabilities " +
		"(GetCapabilities failed; the cause is in the manager log)"
	clusteredTargetPVCRefusedMessage = "storage backend \"pvc\" is not supported into a clustered provider: the disk " +
		"lands on a host, which reads it from s3 or nfs; use \"s3\" or \"nfs\" (ADR-0007 Addendum B, B3)"
	targetPlacedMessage          = "the target VirtualMachine is placed on a host; its capacity is reserved there"
	waitingForPlacementMessage   = "waiting for the target VirtualMachine to be placed on a host of the clustered provider; the source is not touched until it is"
	sourceDiskSizeUnknownMessage = "waiting to read the source disk's size, which the target's placement needs " +
		"(a host must have room for it); the cause is in the manager log"
	targetVMDeletingMessage = "the target VirtualMachine this migration created is being deleted; waiting for it to be gone"
	targetNotPendingMessage = "the clustered target VirtualMachine is not pending on a host (it already has a provider " +
		"id, or no pending host), so the disk import cannot be routed to it; delete the target VirtualMachine and run " +
		"the migration again"
	targetHostReleasedMessage = "the landing host refused the import because a domain with the target " +
		"VirtualMachine's name that it does not own exists there (nothing was landed); the host was excluded for the " +
		"target and the import follows the VM to the host it is placed on next"
	targetHostUnavailableMessage = "the target's host could not be reached, or another host the import must check " +
		"could not be checked; nothing was landed and the import is retried with a backoff (the target is never moved " +
		"off its host automatically)"
	importTargetPreviousIncarnationMessage = "the landing host holds a previous incarnation of the target " +
		"VirtualMachine (ADR-0007 A6): nothing was landed; an administrator must re-attach or remove that domain " +
		"(docs/clustered-restore.md). The import is retried with a backoff"
	landingDiskInUseMessage = "the landing disk name is used by a domain on a host of the clustered provider: " +
		"nothing was landed. Re-placing the target cannot help on a shared pool; an administrator must resolve it. " +
		"The import is retried with a backoff"
	landingHostRefusedMessage = "the landing host refused the import before writing anything (its lock directory, " +
		"or a file at the landing name, is not as the provider requires, or a required host tool is missing; the cause " +
		"is in the manager log): an administrator must fix the host. The import is retried with a backoff"
	routedImportHostMovedMessage = "The ImportDisk was sent to the target's pending host but its outcome is unknown, " +
		"and the target VirtualMachine's pending host has changed since: the import is sent only to the host it was " +
		"first sent to. Check that host for the disk landed for the target VirtualMachine (ending in -migrated) and " +
		"remove it, then set the target's pending host back or confirm the attempt as not performed"
	targetHostMovedMessage = "the target VirtualMachine's pending host is not the host its disk was imported to, so " +
		"the disk is not recorded on it (a VM created there would have no disk). An administrator must set the " +
		"target's status.placement.pendingHost back to the host recorded in status.storageInfo.importTarget.host, or " +
		"delete the VMMigration"
	targetVMImportRecordedMessage = "the imported disk was recorded on the target VirtualMachine; it is created on its pending host"
)

// gateRoutedImport is the B5 capability gate of a clustered target, at
// Validating, before any side effect: proceed only when the target provider
// reports supports_routed_import. A provider without it fails the migration
// with a constant message; capabilities that cannot be read yet are waited
// for (the provider client error goes to the log only).
func (r *VMMigrationReconciler) gateRoutedImport(ctx context.Context, migration *infrav1beta1.VMMigration,
	targetProvider *infrav1beta1.Provider) (bool, ctrl.Result, error) {
	// The target VM proves its landed disk through a VMMigration in its OWN
	// namespace (isOwnMigrationDisk); one in another namespace could never be
	// created on its host from the landed disk.
	if migrationTargetVMKey(migration).Namespace != migration.Namespace {
		setMigrationCondition(migration, infrav1beta1.VMMigrationConditionValidating, metav1.ConditionFalse,
			ReasonClusteredTargetUnsupported, clusteredTargetCrossNamespaceMessage)
		res, err := r.transitionToFailedWithReason(ctx, migration, ReasonClusteredTargetUnsupported, clusteredTargetCrossNamespaceMessage)
		return false, res, err
	}
	instance, err := r.getProviderInstance(ctx, targetProvider)
	var caps contracts.Capabilities
	if err == nil {
		reporter, ok := instance.(contracts.CapabilityReporter)
		if !ok {
			res, ferr := r.failClusteredTargetUnsupported(ctx, migration)
			return false, res, ferr
		}
		caps, err = reporter.GetCapabilities(ctx)
	}
	if err != nil {
		logging.FromContext(ctx).Info("Cannot read the clustered target provider's capabilities yet", "error", err.Error())
		setMigrationCondition(migration, infrav1beta1.VMMigrationConditionValidating, metav1.ConditionFalse,
			migrationReasonCapabilitiesUnknown, clusteredTargetCapabilitiesMessage)
		if werr := r.updateStatus(ctx, migration); werr != nil {
			return false, ctrl.Result{}, werr
		}
		return false, ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if !caps.SupportsRoutedImport {
		res, ferr := r.failClusteredTargetUnsupported(ctx, migration)
		return false, res, ferr
	}
	return true, ctrl.Result{}, nil
}

// failClusteredTargetUnsupported fails a migration into a clustered Provider
// that does not route the import (B5).
func (r *VMMigrationReconciler) failClusteredTargetUnsupported(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	setMigrationCondition(migration, infrav1beta1.VMMigrationConditionValidating, metav1.ConditionFalse,
		ReasonClusteredTargetUnsupported, clusteredTargetUnsupportedMessage)
	return r.transitionToFailedWithReason(ctx, migration, ReasonClusteredTargetUnsupported, clusteredTargetUnsupportedMessage)
}

// setMigrationCondition sets a condition with the migration's generation. It
// reports whether the stored conditions changed.
func setMigrationCondition(m *infrav1beta1.VMMigration, condType string, status metav1.ConditionStatus, reason, msg string) bool {
	return meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: m.Generation,
	})
}

// newMigrationTargetVM builds the VirtualMachine a migration creates on its
// target, with disk as its spec.importedDisk: the user's target annotations
// without the reserved keys, then the provenance annotations (written last, so
// they cannot be overridden), the labels without the reserved ones (returned
// as dropped), the target Provider (pinned to the migration's namespace for a
// VM in another one), class, networks, placement policy and power state. The
// caller adds the claim and any import annotations.
func newMigrationTargetVM(migration *infrav1beta1.VMMigration, disk *infrav1beta1.ImportedDiskRef) (*infrav1beta1.VirtualMachine, []string) {
	key := migrationTargetVMKey(migration)
	// User annotations first, without the operator's reserved virtrigaud.io
	// keys (control annotations and provenance). The provenance annotations
	// are informational: what binds the VM to this migration is its UID
	// (status.targetVMUID) and its claim (migrationTargetClaimAnnotation).
	annotations := userTargetAnnotations(migration.Spec.Target.Annotations)
	annotations[migratedFromAnnotation] = fmt.Sprintf("%s/%s", migration.Namespace, migration.Spec.Source.VMRef.Name)
	annotations[migrationProvenanceAnnotation] = migrationProvenance(migration)

	labels, dropped := migrationTargetLabels(migration.Spec.Target.Labels)
	vm := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        key.Name,
			Namespace:   key.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef:  migration.Spec.Target.ProviderRef,
			PowerState:   migrationTargetPowerState(migration),
			ImportedDisk: disk,
		},
	}
	if key.Namespace != migration.Namespace && vm.Spec.ProviderRef.Namespace == "" {
		// The disk is imported through the target Provider resolved in the
		// MIGRATION's namespace (getProvider). A VM in another namespace would
		// resolve an unqualified reference there instead — pin it.
		vm.Spec.ProviderRef.Namespace = migration.Namespace
	}
	if migration.Spec.Target.ClassRef != nil {
		vm.Spec.ClassRef = infrav1beta1.ObjectRef{
			Name:      migration.Spec.Target.ClassRef.Name,
			Namespace: migration.Namespace,
		}
	}
	if len(migration.Spec.Target.Networks) > 0 {
		vm.Spec.Networks = migration.Spec.Target.Networks
	}
	if migration.Spec.Target.PlacementRef != nil {
		vm.Spec.PlacementRef = migration.Spec.Target.PlacementRef
	}
	return vm, dropped
}

// migrationRoutedImport reports whether the migration recorded an import
// routed to a host of a clustered target (importTarget.host).
func migrationRoutedImport(m *infrav1beta1.VMMigration) bool {
	return recordedImportHost(m) != ""
}

// recordedImportHost is importTarget.host: the host the migration's import
// was routed to, or "".
func recordedImportHost(m *infrav1beta1.VMMigration) string {
	if m.Status.StorageInfo == nil || m.Status.StorageInfo.ImportTarget == nil {
		return ""
	}
	return m.Status.StorageInfo.ImportTarget.Host
}

// targetFirstVM reports whether vm is a clustered migration target created
// before its import: it waits for the import, or it has a clustered
// placement record. A single-host target VM (created after the import, never
// placed) is neither.
func targetFirstVM(vm *infrav1beta1.VirtualMachine) bool {
	return awaitingMigrationImport(vm) || vm.Status.Placement != nil
}

// targetPlacementWaitReason is the TargetPlaced=False reason of a target VM
// that is not placed yet: its Placed reason (Unschedulable, RestorePending,
// AllHostsExcluded, ...), else its Provisioning reason when False, else
// WaitingForPlacement.
func targetPlacementWaitReason(vm *infrav1beta1.VirtualMachine) string {
	if c := meta.FindStatusCondition(vm.Status.Conditions, conditions.ConditionPlaced); c != nil && c.Status == metav1.ConditionFalse {
		return c.Reason
	}
	if c := meta.FindStatusCondition(vm.Status.Conditions, conditions.ConditionProvisioning); c != nil &&
		c.Status == metav1.ConditionFalse && c.Reason != conditions.ReasonWaitingForImport {
		return c.Reason
	}
	return migrationReasonWaitingForPlacement
}

// ensureClusteredTargetPlaced is B1 step 1 at Validating: it creates the
// clustered target VirtualMachine (once, with the claim) and reports placed
// once the VM has a pending host (TargetPlaced=True). Otherwise the caller
// returns (res, err): the migration waits — TargetPlaced=False with the VM's
// reason, on the blocked-VM backoff — or it failed.
func (r *VMMigrationReconciler) ensureClusteredTargetPlaced(ctx context.Context, migration *infrav1beta1.VMMigration,
	sourceVM *infrav1beta1.VirtualMachine, sourceRef contracts.VMRef, sourceProvider, targetProvider *infrav1beta1.Provider,
) (bool, ctrl.Result, error) {
	state, vm, err := r.classifyTargetVM(ctx, migration, true)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	switch state {
	case targetVMUndecided:
		return false, ctrl.Result{RequeueAfter: migrationPhaseAdvanceRequeue}, nil
	case targetVMNotOwned:
		res, ferr := r.failTargetVMNotOwned(ctx, migration)
		return false, res, ferr
	case targetVMAbsent:
		// Before any import: a binding to a VM that is gone (deleted by a
		// retry, or by hand) is released and a fresh target is created.
		migration.Status.TargetVMUID = ""
		migration.Status.TargetVMClaim = ""
		res, cerr := r.createClusteredTarget(ctx, migration, sourceVM, sourceRef, sourceProvider, targetProvider)
		return false, res, cerr
	}

	changed := bindTargetVMUID(migration, vm)
	if !vm.DeletionTimestamp.IsZero() {
		return false, r.waitForTargetPlacement(ctx, migration, changed, migrationReasonTargetVMDeleting, targetVMDeletingMessage), nil
	}
	if vm.Status.ID != "" || !awaitingMigrationImport(vm) {
		// Created already, or given a disk by an earlier attempt: not a target
		// an import can be routed to.
		setMigrationCondition(migration, infrav1beta1.VMMigrationConditionValidating, metav1.ConditionFalse,
			ReasonTargetNotPending, targetNotPendingMessage)
		res, ferr := r.transitionToFailedWithReason(ctx, migration, ReasonTargetNotPending, targetNotPendingMessage)
		return false, res, ferr
	}
	if pendingHost(vm) == "" {
		return false, r.waitForTargetPlacement(ctx, migration, changed, targetPlacementWaitReason(vm), waitingForPlacementMessage), nil
	}
	if setMigrationCondition(migration, migrationConditionTargetPlaced, metav1.ConditionTrue, migrationReasonTargetPlaced, targetPlacedMessage) {
		changed = true
		logging.FromContext(ctx).Info("Clustered target VM is placed", "vm", vm.Name, "host", pendingHost(vm))
	}
	if changed {
		if err := r.updateStatus(ctx, migration); err != nil {
			return false, ctrl.Result{}, err
		}
	}
	return true, ctrl.Result{}, nil
}

// waitForTargetPlacement records TargetPlaced=False with reason and msg (and
// any other status change, changed) and returns the blocked-VM backoff from
// when the wait began. A repeated wait writes nothing.
func (r *VMMigrationReconciler) waitForTargetPlacement(ctx context.Context, migration *infrav1beta1.VMMigration,
	changed bool, reason, msg string) ctrl.Result {
	if setMigrationCondition(migration, migrationConditionTargetPlaced, metav1.ConditionFalse, reason, msg) {
		changed = true
	}
	if migration.Status.Message != msg {
		migration.Status.Message = msg
		changed = true
	}
	if changed {
		if err := r.updateStatus(ctx, migration); err != nil {
			logging.FromContext(ctx).Info("Could not record the target placement wait; retried", "error", err.Error())
			return ctrl.Result{RequeueAfter: migrationStaleReadRetryInterval}
		}
	}
	return ctrl.Result{RequeueAfter: blockedRetryBackoff(conditionSince(migration.Status.Conditions, migrationConditionTargetPlaced))}
}

// createClusteredTarget creates the clustered target VirtualMachine before
// the import (B1 step 1.3): spec.importedDisk names the migration but no disk
// and carries the source disk's size, rounded up to GiB (the B7.4 placement
// filter). The claim digest is persisted before the Create, then the VM's
// UID is recorded. The grants are re-read live right before it.
func (r *VMMigrationReconciler) createClusteredTarget(ctx context.Context, migration *infrav1beta1.VMMigration,
	sourceVM *infrav1beta1.VirtualMachine, sourceRef contracts.VMRef, sourceProvider, targetProvider *infrav1beta1.Provider,
) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	sourceInst, err := r.getProviderInstance(ctx, sourceProvider)
	if err != nil {
		logger.Error(err, "Failed to get the source provider instance to read the disk size")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	size, ok, err := readSourceDiskSize(ctx, sourceInst, contracts.GetDiskInfoRequest{VM: sourceRef})
	if isSourceOwnerRefused(err) {
		logger.Info("The source VM's provider refused it as not the source VirtualMachine's", "error", err.Error())
		return r.transitionToFailed(ctx, migration, migrationSourceNotOwnedMessage)
	}
	if err != nil {
		logger.Error(err, "Failed to read the source disk's size")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if !ok {
		return r.waitForTargetPlacement(ctx, migration, false, migrationReasonSourceDiskSizeUnknown, sourceDiskSizeUnknownMessage), nil
	}

	disk := &infrav1beta1.ImportedDiskRef{
		Format:       landedTargetFormat(targetProvider),
		Source:       importedDiskSourceMigration,
		MigrationRef: &infrav1beta1.LocalObjectReference{Name: migration.Name},
		SizeGiB:      diskSizeGiB(size.Value()),
	}
	targetVM, dropped := newMigrationTargetVM(migration, disk)

	if allowed, res, err := r.confirmTargetNamespaceLive(ctx, migration); !allowed {
		return res, err
	}
	if allowed, res, err := r.confirmTargetConsumersLive(ctx, migration); !allowed {
		return res, err
	}
	token, digest, err := newMigrationTargetClaim()
	if err != nil {
		return ctrl.Result{}, err
	}
	migration.Status.TargetVMClaim = digest
	migration.Status.TargetVMUID = ""
	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	targetVM.Annotations[migrationTargetClaimAnnotation] = token

	if err := r.Create(ctx, targetVM); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Decided from the API server on the next reconcile (the claim is
			// new, so it cannot be this Create's).
			return ctrl.Result{RequeueAfter: migrationPhaseAdvanceRequeue}, nil
		}
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to create target VM: %v", err))
	}
	logger.Info("Clustered target VM created before the import; waiting for its placement", "vm", targetVM.Name, "uid", targetVM.UID)
	if len(dropped) > 0 {
		r.Recorder.Event(migration, corev1.EventTypeNormal, eventReasonTargetLabelsDropped, targetLabelsDroppedMessage(dropped))
	}
	r.Recorder.Event(migration, corev1.EventTypeNormal, "TargetVMCreated", fmt.Sprintf("Created target VM %s", targetVM.Name))
	migration.Status.TargetVMUID = string(targetVM.UID)
	return r.waitForTargetPlacement(ctx, migration, true, migrationReasonWaitingForPlacement, waitingForPlacementMessage), nil
}

// diskSizeGiB is bytes rounded up to whole GiB, at least 1 and at most the
// largest int32.
func diskSizeGiB(bytes int64) int32 {
	gib := bytes / bytesPerGiB
	if bytes%bytesPerGiB != 0 {
		gib++
	}
	switch {
	case gib < 1:
		return 1
	case gib > math.MaxInt32:
		return math.MaxInt32
	}
	return int32(gib) // #nosec G115 -- bounded above
}

// routedImportTarget is B1 step 4.1-4.2 at Importing: the clustered target VM
// re-read live, its pending host and UID. held means the caller returns (res,
// err): the target is gone or not this migration's (failed), not pending
// (TargetNotPending), released by this migration for a name conflict and not
// placed again yet (waited for), or pending on another host than an import
// whose outcome is unknown was sent to (held for an operator, B4).
func (r *VMMigrationReconciler) routedImportTarget(ctx context.Context, migration *infrav1beta1.VMMigration) (
	host, uid string, held bool, res ctrl.Result, err error) {
	vm, err := r.liveOwnedTargetVM(ctx, migration)
	if err != nil {
		return "", "", true, ctrl.Result{}, err
	}
	if vm == nil {
		res, err = r.transitionToFailedWithReason(ctx, migration, ReasonTargetVMGone, targetVMGoneMessage)
		return "", "", true, res, err
	}
	if !migrationOwnsTargetVM(migration, vm) {
		res, err = r.failTargetVMNotOwned(ctx, migration)
		return "", "", true, res, err
	}
	changed := bindTargetVMUID(migration, vm)
	host = pendingHost(vm)
	if vm.Status.ID != "" || !awaitingMigrationImport(vm) {
		res, err = r.transitionToFailedWithReason(ctx, migration, ReasonTargetNotPending, targetNotPendingMessage)
		return "", "", true, res, err
	}
	if host == "" {
		if c := meta.FindStatusCondition(migration.Status.Conditions, migrationConditionTargetPlaced); c != nil &&
			c.Status == metav1.ConditionFalse && c.Reason == migrationReasonTargetHostReleased {
			return "", "", true, r.waitForTargetPlacement(ctx, migration, changed, migrationReasonTargetHostReleased,
				targetHostReleasedMessage), nil
		}
		res, err = r.transitionToFailedWithReason(ctx, migration, ReasonTargetNotPending, targetNotPendingMessage)
		return "", "", true, res, err
	}
	// B4: a re-send goes to the host the first send was recorded for, never
	// to a pending host that differs from it.
	if recorded := recordedImportHost(migration); recorded != "" && recorded != host && migrationRequested(migration, longOpImport) {
		attempt := requestMarkerAttemptID(migration.Status.Conditions, migrationConditionImportRequested)
		res, err = r.holdMigrationOutcomeUnknown(ctx, migration, longOpImport, attempt, routedImportHostMovedMessage,
			requestMarkerRecheckInterval)
		return "", "", true, res, err
	}
	if setMigrationCondition(migration, migrationConditionTargetPlaced, metav1.ConditionTrue, migrationReasonTargetPlaced, targetPlacedMessage) {
		changed = true
	}
	if changed {
		if err := r.updateStatus(ctx, migration); err != nil {
			return "", "", true, ctrl.Result{}, err
		}
	}
	return host, string(vm.UID), false, ctrl.Result{}, nil
}

// handleRoutedImportRefusal handles a routed import's definite refusals that
// are not failures (B1.2, B4). The import's marker was removed by the caller
// (a definite answer: nothing was landed). handled == false leaves the
// failure to the caller.
//
//   - The landing host, or a host the import must check, is unavailable
//     (HOST_UNAVAILABLE, VM_DISK_CHECK_FAILED): wait with a backoff; the
//     target is never moved off its host automatically (B7.3).
//   - A previous incarnation of the target on the landing host
//     (VM_PREVIOUS_INCARNATION, either kind): hold, as R2 does for a create.
//   - The landing file is used by a domain of the Provider (VM_DISK_IN_USE):
//     hold; re-placing cannot help on a shared pool.
//   - A plain AlreadyExists — a foreign or unstamped domain with the target's
//     name on the landing host: the B1.2 release of the target's pending
//     host, and the import follows the VM to its next host.
func (r *VMMigrationReconciler) handleRoutedImportRefusal(ctx context.Context, migration *infrav1beta1.VMMigration,
	host string, cause error) (ctrl.Result, bool, error) {
	logger := logging.FromContext(ctx)
	switch {
	case contracts.IsHostUnavailable(cause) || contracts.IsVMDiskCheckFailed(cause):
		logger.Info("Routed import not performed: a host could not be reached or checked; waiting", "host", host, "error", cause.Error())
		res, err := r.holdRoutedImport(ctx, migration, migrationReasonTargetHostUnavailable, targetHostUnavailableMessage)
		return res, true, err
	case contracts.IsVMPreviousIncarnation(cause):
		logger.Info("Routed import refused: a previous incarnation of the target is on the landing host", "host", host, "error", cause.Error())
		res, err := r.holdRoutedImport(ctx, migration, migrationReasonImportTargetPreviousIncarnation, importTargetPreviousIncarnationMessage)
		return res, true, err
	case contracts.IsVMDiskInUse(cause):
		logger.Info("Routed import refused: the landing disk is in use on a host of the provider", "host", host, "error", cause.Error())
		res, err := r.holdRoutedImport(ctx, migration, migrationReasonLandingDiskInUse, landingDiskInUseMessage)
		return res, true, err
	case routedImportNameConflict(cause):
		logger.Info("Routed import refused: a foreign domain with the target's name is on the landing host; releasing it",
			"host", host, "error", cause.Error())
		res, err := r.releaseTargetPendingHost(ctx, migration, host)
		return res, true, err
	}
	return ctrl.Result{}, false, nil
}

// routedImportNameConflict reports whether a routed import's refusal is the
// plain name conflict of B2 step 1 (a foreign or unstamped domain with the
// target's name on the landing host): a Conflict that carries none of the
// typed reasons a Conflict can also stand for.
func routedImportNameConflict(err error) bool {
	return contracts.IsConflict(err) && !contracts.IsVMPreviousIncarnation(err) && !contracts.IsVMDiskInUse(err) &&
		!contracts.IsVMIsImageArtifact(err) && !contracts.IsOwnerStampUnreadable(err) &&
		!contracts.IsDiskSizeExceedsLimit(err) && !contracts.IsDiskSizeUnverifiable(err)
}

// holdRoutedImport keeps the migration in Importing with Importing=False and
// reason, msg (constant), writing only when that changes the status, and
// retries with the blocked-VM backoff from when the hold began.
func (r *VMMigrationReconciler) holdRoutedImport(ctx context.Context, migration *infrav1beta1.VMMigration, reason, msg string) (ctrl.Result, error) {
	prev := meta.FindStatusCondition(migration.Status.Conditions, infrav1beta1.VMMigrationConditionImporting)
	isNew := prev == nil || prev.Reason != reason
	setMigrationCondition(migration, infrav1beta1.VMMigrationConditionImporting, metav1.ConditionFalse, reason, msg)
	migration.Status.Message = msg
	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	if isNew {
		r.Recorder.Event(migration, corev1.EventTypeWarning, reason, msg)
	}
	return ctrl.Result{RequeueAfter: blockedRetryBackoff(conditionSince(migration.Status.Conditions,
		infrav1beta1.VMMigrationConditionImporting))}, nil
}

// releaseTargetPendingHost is the B1.2 release on a definite import name
// conflict: in one checked status write on the migration's OWN target VM —
// re-read live, still unbound and pending on host — pendingHost is cleared
// and host is added to excludedHosts (A2 slice 2), with Placed=False/
// HostExcluded. Nothing was landed there. The migration then waits for the
// VM to be placed again (TargetPlaced=False/TargetHostReleased). A lost write
// leaves the VM on host: the next import is refused the same way and the
// release is retried.
func (r *VMMigrationReconciler) releaseTargetPendingHost(ctx context.Context, migration *infrav1beta1.VMMigration, host string) (ctrl.Result, error) {
	vm, err := r.liveOwnedTargetVM(ctx, migration)
	if err != nil {
		return ctrl.Result{}, err
	}
	if vm == nil || !migrationOwnsTargetVM(migration, vm) {
		// The next Importing reconcile decides it (gone, or not ours).
		return ctrl.Result{RequeueAfter: migrationPhaseAdvanceRequeue}, nil
	}
	if vm.Status.ID == "" && pendingHost(vm) == host {
		pl := vm.Status.Placement
		excludeHost(pl, host)
		pl.PendingHost = ""
		pl.PendingResources = nil
		setPlacedCondition(vm, metav1.ConditionFalse, conditions.ReasonHostExcluded, fmt.Sprintf(
			"the import of this VM's disk to host %s was refused because a same-named domain this VirtualMachine does "+
				"not own exists there, so nothing was landed; the host is excluded for this VM and it will be "+
				"re-scheduled onto another host", host))
		applyDerivedPhase(vm)
		if err := r.Status().Update(ctx, vm); err != nil {
			if apierrors.IsConflict(err) {
				logging.FromContext(ctx).Info("Target pending-host release lost a resourceVersion race; the import is retried on the same host",
					"host", host)
				return ctrl.Result{RequeueAfter: migrationPhaseAdvanceRequeue}, nil
			}
			return ctrl.Result{}, fmt.Errorf("release pending host %s of target VirtualMachine %s/%s: %w", host, vm.Namespace, vm.Name, err)
		}
		r.Recorder.Event(migration, corev1.EventTypeNormal, eventReasonTargetHostReleased, targetHostReleasedMessage)
	}
	return r.waitForTargetPlacement(ctx, migration, true, migrationReasonTargetHostReleased, targetHostReleasedMessage), nil
}

// recordImportOnTarget is the Creating phase of a clustered target (B1 step
// 5): the landed disk (diskID, path, format) and the import annotations are
// written into the target VM's spec.importedDisk with the resourceVersion of
// the live object as precondition, and the VirtualMachine controller creates
// the VM on its pending host with the disk attached in place. A target whose
// pending host is not the host the disk landed on is held for an operator.
func (r *VMMigrationReconciler) recordImportOnTarget(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	vm, err := r.liveOwnedTargetVM(ctx, migration)
	if err != nil {
		return ctrl.Result{}, err
	}
	if vm == nil {
		return r.transitionToFailedWithReason(ctx, migration, ReasonTargetVMGone, targetVMGoneMessage)
	}
	if !migrationOwnsTargetVM(migration, vm) {
		return r.failTargetVMNotOwned(ctx, migration)
	}
	if !awaitingMigrationImport(vm) {
		return r.awaitOwnedTargetVM(ctx, migration, vm)
	}
	if vm.Status.ID != "" {
		return r.transitionToFailedWithReason(ctx, migration, ReasonTargetNotPending, targetNotPendingMessage)
	}
	if migration.Status.DiskInfo == nil || migration.Status.ImportID == "" {
		return r.transitionToFailed(ctx, migration,
			"import did not record disk info (status.diskInfo or status.importID is empty); cannot record the disk on the target VM")
	}
	if recorded := recordedImportHost(migration); recorded == "" || pendingHost(vm) != recorded {
		changed := setMigrationCondition(migration, infrav1beta1.VMMigrationConditionReady, metav1.ConditionFalse,
			migrationReasonTargetHostMoved, targetHostMovedMessage)
		if migration.Status.Message != targetHostMovedMessage {
			migration.Status.Message = targetHostMovedMessage
			changed = true
		}
		if changed {
			if err := r.updateStatus(ctx, migration); err != nil {
				return ctrl.Result{}, err
			}
			r.Recorder.Event(migration, corev1.EventTypeWarning, migrationReasonTargetHostMoved, targetHostMovedMessage)
		}
		return ctrl.Result{RequeueAfter: requestMarkerRecheckInterval}, nil
	}

	if allowed, res, err := r.confirmTargetNamespaceLive(ctx, migration); !allowed {
		return res, err
	}
	if allowed, res, err := r.confirmTargetConsumersLive(ctx, migration); !allowed {
		return res, err
	}
	bound := bindTargetVMUID(migration, vm)
	vm.Spec.ImportedDisk.DiskID = migration.Status.ImportID
	vm.Spec.ImportedDisk.Path = migration.Status.DiskInfo.TargetPath
	if f := migration.Status.DiskInfo.TargetFormat; f != "" {
		vm.Spec.ImportedDisk.Format = f
	}
	if vm.Annotations == nil {
		vm.Annotations = map[string]string{}
	}
	vm.Annotations[importedDiskIDAnnotation] = migration.Status.ImportID
	vm.Annotations[diskChecksumAnnotation] = migration.Status.DiskInfo.TargetChecksum
	// The live object's resourceVersion is the precondition: only this
	// object, as read, is changed.
	if err := r.Update(ctx, vm); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: migrationPhaseAdvanceRequeue}, nil
		}
		return ctrl.Result{}, fmt.Errorf("record the imported disk on target VirtualMachine %s/%s: %w", vm.Namespace, vm.Name, err)
	}
	logging.FromContext(ctx).Info("Recorded the imported disk on the clustered target VM", "vm", vm.Name, "host", pendingHost(vm))
	r.Recorder.Event(migration, corev1.EventTypeNormal, eventReasonTargetVMImportRecorded, targetVMImportRecordedMessage)
	if c := meta.FindStatusCondition(migration.Status.Conditions, infrav1beta1.VMMigrationConditionReady); c != nil &&
		c.Reason == migrationReasonTargetHostMoved {
		meta.RemoveStatusCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionReady)
		bound = true
	}
	if bound || migration.Status.Message != targetVMImportRecordedMessage {
		migration.Status.Message = targetVMImportRecordedMessage
		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: targetVMWaitInterval}, nil
}

// deleteUnboundTarget is B1.4: it deletes the migration's own clustered target
// VM while it is unbound (no status.id: no domain was created for it, and it
// was never completed), under a UID precondition, freeing its capacity and its
// name. gone reports that no such VM remains — none was recorded, it is
// absent, bound, a single-host target (never deleted here) or not this
// migration's, or the target namespace no longer grants this migration's
// (nothing is done there). While a requested delete runs (the VM's finalizer
// sends an owner-checked Delete to its pending host) gone is false.
func (r *VMMigrationReconciler) deleteUnboundTarget(ctx context.Context, migration *infrav1beta1.VMMigration) (bool, error) {
	if migration.Status.TargetVMUID == "" && migration.Status.TargetVMClaim == "" {
		return true, nil
	}
	key := migrationTargetVMKey(migration)
	allowed, err := targetNamespaceAllowed(ctx, r.liveReader(), migration.Namespace, key.Namespace)
	if err != nil {
		return false, fmt.Errorf("check the target namespace grant before deleting the unbound target VM: %w", err)
	}
	if !allowed {
		return true, nil
	}
	vm := &infrav1beta1.VirtualMachine{}
	if err := r.liveReader().Get(ctx, key, vm); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("re-read target VirtualMachine %s: %w", key, err)
	}
	if !migrationOwnsTargetVM(migration, vm) || vm.Status.ID != "" || !targetFirstVM(vm) ||
		vm.Annotations[migrationCompletedAnnotation] == "true" {
		return true, nil
	}
	if !vm.DeletionTimestamp.IsZero() {
		return false, nil
	}
	if err := r.Delete(ctx, vm, client.Preconditions{UID: &vm.UID}); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("delete unbound target VirtualMachine %s: %w", key, err)
	}
	logging.FromContext(ctx).Info("Deleted the migration's unbound clustered target VM", "vm", key.String())
	r.Recorder.Event(migration, corev1.EventTypeNormal, eventReasonUnboundTargetDeleted,
		fmt.Sprintf("Deleted the unbound target VM %s (it was never created on a host)", key.Name))
	return false, nil
}

// releaseUnboundTargetForDeletion runs B1.4 in a VMMigration's deletion,
// before its landed disk is cleaned up: hold == true keeps the finalizer while
// the unbound target VM is being deleted. force-delete does not wait for it.
func (r *VMMigrationReconciler) releaseUnboundTargetForDeletion(ctx context.Context, migration *infrav1beta1.VMMigration) (bool, ctrl.Result, error) {
	gone, err := r.deleteUnboundTarget(ctx, migration)
	if err != nil {
		if hasMigrationForceDeleteAnnotation(migration) {
			logging.FromContext(ctx).Info("Could not delete the unbound target VM; force-delete releases the VMMigration", "error", err.Error())
			return false, ctrl.Result{}, nil
		}
		return true, ctrl.Result{}, err
	}
	if !gone && !hasMigrationForceDeleteAnnotation(migration) {
		return true, ctrl.Result{RequeueAfter: unboundTargetDeleteRequeue}, nil
	}
	return false, ctrl.Result{}, nil
}

// cleanupRoutedLandedDiskForRetry runs before a retry of a migration whose
// import was routed to a host (importTarget.host) and may have landed a disk
// there: the retry places a fresh target, possibly on another host, and
// records that host instead, so the earlier landed disk is cleaned up first —
// once the unbound target is gone, with the cleanupPolicy semantics (Never
// keeps it). held == true: the retry waits (res, err).
func (r *VMMigrationReconciler) cleanupRoutedLandedDiskForRetry(ctx context.Context, migration *infrav1beta1.VMMigration) (bool, ctrl.Result, error) {
	if !migrationRoutedImport(migration) || !landedDiskMayExist(migration) {
		return false, ctrl.Result{}, nil
	}
	if retained, err := r.retainLandedDiskIfNever(ctx, migration); retained || err != nil {
		return err != nil, ctrl.Result{}, err
	}
	if wait, notDue := landedDiskCleanupNotDue(migration, time.Now()); notDue {
		return true, ctrl.Result{RequeueAfter: wait}, nil
	}
	done, err := r.cleanupLandedDisk(ctx, migration)
	var left *landedDiskLeftBehindError
	switch {
	case err == nil:
		return false, ctrl.Result{}, r.setLandedDiskDone(ctx, migration, done)
	case stderrors.As(err, &left):
		logLandedDiskLeftBehind(ctx, left)
		return false, ctrl.Result{}, r.setLandedDiskDone(ctx, migration, landedDiskLeftBehindDone(left.message))
	default:
		retry, herr := r.holdLandedDiskCleanup(ctx, migration, err)
		return true, ctrl.Result{RequeueAfter: retry}, herr
	}
}

// stagedDiskHost is the host of a clustered target Provider that empties an
// nfs staged object (B4): importTarget.host, or — when the import was never
// routed — the migration's own target VM's pending or bound host. "" when
// none is known.
func (r *VMMigrationReconciler) stagedDiskHost(ctx context.Context, migration *infrav1beta1.VMMigration) (string, error) {
	if host := recordedImportHost(migration); host != "" {
		return host, nil
	}
	if migration.Status.TargetVMUID == "" && migration.Status.TargetVMClaim == "" {
		return "", nil
	}
	key := migrationTargetVMKey(migration)
	allowed, err := targetNamespaceAllowed(ctx, r.liveReader(), migration.Namespace, key.Namespace)
	if err != nil || !allowed {
		return "", err
	}
	vm := &infrav1beta1.VirtualMachine{}
	if err := r.liveReader().Get(ctx, key, vm); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("re-read target VirtualMachine %s: %w", key, err)
	}
	if !migrationOwnsTargetVM(migration, vm) || vm.Status.Placement == nil {
		return "", nil
	}
	if h := vm.Status.Placement.Host; h != "" {
		return h, nil
	}
	return vm.Status.Placement.PendingHost, nil
}

// migrationForTargetVM maps a VirtualMachine to the VMMigration its
// provenance annotation names ("<namespace>/<name>", written by the
// migration that created it). The annotation is only a trigger: the
// reconcile decides everything from the migration's status (its UID or claim).
func migrationForTargetVM(_ context.Context, obj client.Object) []reconcile.Request {
	ns, name, ok := strings.Cut(obj.GetAnnotations()[migrationProvenanceAnnotation], "/")
	if !ok || ns == "" || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
}

// targetPlacementChanged passes a VirtualMachine update whose pending host,
// bound host or deletion changed, for a VM a migration created (it carries
// the provenance annotation).
func targetPlacementChanged() predicate.Predicate {
	hosts := func(o client.Object) (string, string) {
		vm, ok := o.(*infrav1beta1.VirtualMachine)
		if !ok || vm.Status.Placement == nil {
			return "", ""
		}
		return vm.Status.Placement.PendingHost, vm.Status.Placement.Host
	}
	created := func(o client.Object) bool { return o.GetAnnotations()[migrationProvenanceAnnotation] != "" }
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !created(e.ObjectNew) {
				return false
			}
			op, oh := hosts(e.ObjectOld)
			np, nh := hosts(e.ObjectNew)
			return op != np || oh != nh || e.ObjectOld.GetDeletionTimestamp().IsZero() != e.ObjectNew.GetDeletionTimestamp().IsZero()
		},
		DeleteFunc:  func(e event.DeleteEvent) bool { return created(e.Object) },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// routedImportPreconditionRefused reports whether a routed import was
// answered with an untyped FailedPrecondition: a pre-write refusal of the
// landing host (a guard refusal, a missing host tool). The typed refusals
// that travel as FailedPrecondition (VM_DISK_IN_USE, the disk-size limit)
// are mapped to ProviderErrors by the transport and are not this.
func routedImportPreconditionRefused(err error) bool {
	var pe *contracts.ProviderError
	if stderrors.As(err, &pe) {
		return false
	}
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.FailedPrecondition
}
