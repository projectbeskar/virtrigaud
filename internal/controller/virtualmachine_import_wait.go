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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// The VirtualMachine controller's half of a VMMigration into a clustered
// Provider (ADR-0007 Addendum B, B1).
//
// For a clustered target the migration creates its target VirtualMachine
// FIRST, with spec.importedDisk naming the migration (migrationRef, source
// "migration") but no disk yet: an empty diskID (B7.1). The ordinary
// scheduling path places that VM — R1, R4, the admission under the
// per-Provider assume lock, the pendingHost checked write, with the source
// disk's size as a disk-space filter (B7.4) — and then, instead of sending a
// Create, holds it with Provisioning=False/WaitingForImport. The migration
// imports the disk to the VM's pending host and patches spec.importedDisk
// with the landed disk; the Create then attaches it in place on that host
// (isOwnMigrationDisk proves it).
//
// The hold rests on the spec alone: a VM with an empty diskID is never
// created, whatever anyone says. Which reason it carries is decided from the
// referenced VMMigration's STATUS, which tenants cannot write: a migration in
// the VM's namespace that created this very VM (its recorded UID or claim,
// migrationOwnsTargetVM). A VM whose migrationRef names a migration that is
// gone, or that did not create it, is held as MigrationNotFound and is not
// even placed: holding fails closed, and a tenant who forges
// spec.importedDisk only holds their own VM.
//
// Once the import is recorded the host is pinned (B1.2): a Create conflict on
// it holds the VM (ImportedDiskPinned) instead of excluding the host and
// re-scheduling, and A6.2's move to an own domain's host is not applied —
// the landed disk exists only on the pending host.

// importWaitRecheckInterval re-checks a VirtualMachine waiting for its import.
// The migration's spec patch reconciles the VM at once; the recheck also
// notices a pending host the migration released (a status-only change, which
// does not trigger a reconcile).
const importWaitRecheckInterval = 30 * time.Second

// Constant hold messages. They name no host, no disk and no other object.
const (
	waitingForImportMessage = "Waiting for the VMMigration that created this VirtualMachine to import its disk to the " +
		"VM's pending host (ADR-0007 Addendum B): the VM is placed, and its host reserves its capacity, but it is not " +
		"created until spec.importedDisk records the landed disk"
	migrationNotFoundMessage = "spec.importedDisk names no disk yet (an empty diskID) and its migrationRef names a " +
		"VMMigration that does not exist in this namespace or did not create this VirtualMachine: the VM is not placed " +
		"or created. Delete it, or let the migration that created it import its disk"
	importedDiskPinnedMessage = "the create on the pending host was refused with a name conflict, but this VM's " +
		"migration imported its disk to that host, so the host is pinned: the VM is not moved to another host, where its " +
		"disk does not exist. Resolve the conflicting domain on the host (an administrator); the create is re-checked " +
		"with a backoff"
)

// awaitingMigrationImport reports whether vm is a migration's target waiting
// for its import: spec.importedDisk names a migration (migrationRef) but no
// disk (an empty diskID). Such a VM is never created.
func awaitingMigrationImport(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	d := vm.Spec.ImportedDisk
	return d != nil && d.DiskID == "" && d.MigrationRef != nil && d.MigrationRef.Name != ""
}

// importingMigrationFor returns the VMMigration vm's spec.importedDisk.
// migrationRef names, in vm's namespace, when it exists, targets vm and created
// it (migrationOwnsTargetVM: its UID or the claim written before its Create).
// A cache that does not show the migration, or its claim, yet is answered from
// the API server. nil (no error) means no such migration; only an API error
// other than NotFound is returned.
func (r *VirtualMachineReconciler) importingMigrationFor(ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine) (*infravirtrigaudiov1beta1.VMMigration, error) {
	d := vm.Spec.ImportedDisk
	if d == nil || d.MigrationRef == nil || d.MigrationRef.Name == "" {
		return nil, nil
	}
	key := types.NamespacedName{Namespace: vm.Namespace, Name: d.MigrationRef.Name}
	owns := func(m *infravirtrigaudiov1beta1.VMMigration) bool {
		return migrationTargetVMKey(m) == types.NamespacedName{Namespace: vm.Namespace, Name: vm.Name} &&
			migrationOwnsTargetVM(m, vm)
	}
	migration := &infravirtrigaudiov1beta1.VMMigration{}
	err := r.Get(ctx, key, migration)
	switch {
	case err == nil && owns(migration):
		return migration, nil
	case err != nil && !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("get VMMigration %s referenced by importedDisk: %w", key, err)
	case r.APIReader == nil:
		return nil, nil
	}
	live := &infravirtrigaudiov1beta1.VMMigration{}
	if err := r.APIReader.Get(ctx, key, live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("re-read VMMigration %s referenced by importedDisk: %w", key, err)
	}
	if !owns(live) {
		return nil, nil
	}
	return live, nil
}

// holdForImport records why vm is not created — reason WaitingForImport or
// MigrationNotFound, on Provisioning and Ready, with msg — writing the status
// only when that changes it, and returns the recheck. One Warning event is
// recorded when a MigrationNotFound hold begins.
func (r *VirtualMachineReconciler) holdForImport(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	reason, msg string) ctrl.Result {
	persisted := vm.Status.DeepCopy()
	prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionProvisioning)
	isNew := prev == nil || prev.Reason != reason
	for _, condType := range []string{k8s.ConditionProvisioning, k8s.ConditionReady} {
		meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: vm.Generation,
		})
	}
	if isNew {
		log.FromContext(ctx).Info("Not creating the VM: it waits for its migration's disk import", "reason", reason,
			"pendingHost", pendingHost(vm))
		if reason == k8s.ReasonMigrationNotFound {
			metrics.RecordError(errReasonMigrationNotFound, metrics.ComponentManager)
			r.recordEvent(vm, corev1.EventTypeWarning, reason, msg)
		}
	}
	if statusDiffers(persisted, vm) {
		r.updateStatus(ctx, vm)
	}
	return ctrl.Result{RequeueAfter: importWaitRecheckInterval}
}

// errReasonMigrationNotFound counts the reconciles that hold a VM waiting for
// an import whose migration does not exist or did not create it.
const errReasonMigrationNotFound = "migration-not-found"

// importedDiskPinned reports whether vm's pending host is pinned by its
// migration's import (ADR-0007 Addendum B, B1.2): vm has an imported disk from
// a migration (migrationRef) and that migration is not provably someone
// else's. A migration that created vm pins it; so does one that cannot be
// found (fail closed: the landed disk may be on the host). Only a migration
// that exists and did NOT create vm leaves the ordinary slice-2 handling.
func (r *VirtualMachineReconciler) importedDiskPinned(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) (bool, error) {
	d := vm.Spec.ImportedDisk
	if d == nil || d.MigrationRef == nil || d.MigrationRef.Name == "" {
		return false, nil
	}
	m, err := r.importingMigrationFor(ctx, vm)
	if err != nil {
		return false, err
	}
	if m != nil {
		return true, nil
	}
	// Not this VM's migration: pinned only when no such migration exists at
	// all (it may have been deleted after the import).
	other := &infravirtrigaudiov1beta1.VMMigration{}
	err = r.Get(ctx, types.NamespacedName{Namespace: vm.Namespace, Name: d.MigrationRef.Name}, other)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get VMMigration %s/%s referenced by importedDisk: %w", vm.Namespace, d.MigrationRef.Name, err)
	}
	return false, nil
}

// holdImportedDiskPinned holds a pinned migration target whose Create on its
// pending host was refused with a name conflict: Placed=False and
// Provisioning=False/ImportedDiskPinned, pendingHost kept (no exclusion, no
// re-schedule), one Warning event when the hold begins, and a retry with the
// blocked-VM backoff. cause goes to the manager log only.
func (r *VirtualMachineReconciler) holdImportedDiskPinned(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	host string, cause error) ctrl.Result {
	prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionPlaced)
	alreadyHeld := prev != nil && prev.Status == metav1.ConditionFalse && prev.Reason == k8s.ReasonImportedDiskPinned
	setPlacedCondition(vm, metav1.ConditionFalse, k8s.ReasonImportedDiskPinned, importedDiskPinnedMessage)
	k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonImportedDiskPinned,
		importedDiskPinnedMessage, vm.Generation)
	log.FromContext(ctx).Info("Create refused on the pending host of a migration target whose disk landed there; "+
		"holding it pinned (not excluded, not re-scheduled)", "host", host, "error", cause.Error())
	metrics.RecordError(errReasonProviderCreateRejected, metrics.ComponentManager)
	if !alreadyHeld {
		r.recordEvent(vm, corev1.EventTypeWarning, k8s.ReasonImportedDiskPinned, importedDiskPinnedMessage)
	}
	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: blockedRetryBackoff(createHoldSince(vm))}
}

// importedDiskPlacementBytes is the disk space a migration target's placement
// needs on its host (ADR-0007 Addendum B, B7.4): spec.importedDisk.sizeGiB,
// which the migration sets from the source disk's virtual size. 0 (no filter)
// for any other VM.
func importedDiskPlacementBytes(vm *infravirtrigaudiov1beta1.VirtualMachine) int64 {
	d := vm.Spec.ImportedDisk
	if d == nil || d.MigrationRef == nil || d.SizeGiB <= 0 {
		return 0
	}
	return int64(d.SizeGiB) * bytesPerGiB
}
