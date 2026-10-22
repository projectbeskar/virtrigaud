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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
)

// routedTargetDiskMatches reports whether vm's spec.importedDisk is exactly
// the disk m's routed import landed (#416 M2): it names m (migrationRef), its
// diskID is status.importID and its path status.diskInfo.targetPath. The
// spec is writable by whoever may edit the VM, so a routed migration never
// completes — or deletes its source — on a target whose disk was swapped.
func routedTargetDiskMatches(m *infrav1beta1.VMMigration, vm *infrav1beta1.VirtualMachine) bool {
	d := vm.Spec.ImportedDisk
	return d != nil && d.MigrationRef != nil && d.MigrationRef.Name == m.Name &&
		m.Status.ImportID != "" && d.DiskID == m.Status.ImportID &&
		m.Status.DiskInfo != nil && d.Path == m.Status.DiskInfo.TargetPath
}

// holdTargetDiskMismatch holds a routed migration whose target VM no longer
// carries the disk its import landed (#416 M2): Ready=False/TargetDiskMismatch
// with a constant message and one Warning event when the hold begins. The
// migration never reaches Ready (so spec.source.deleteAfterMigration never
// runs) until the target's spec.importedDisk is restored, or it is deleted.
func (r *VMMigrationReconciler) holdTargetDiskMismatch(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	prev := meta.FindStatusCondition(migration.Status.Conditions, infrav1beta1.VMMigrationConditionReady)
	isNew := prev == nil || prev.Reason != migrationReasonTargetDiskMismatch
	changed := setMigrationCondition(migration, infrav1beta1.VMMigrationConditionReady, metav1.ConditionFalse,
		migrationReasonTargetDiskMismatch, targetDiskMismatchMessage)
	if migration.Status.Message != targetDiskMismatchMessage {
		migration.Status.Message = targetDiskMismatchMessage
		changed = true
	}
	if changed {
		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}
	}
	if isNew {
		logging.FromContext(ctx).Info("The routed migration's target VM does not carry the imported disk; holding")
		r.Recorder.Event(migration, corev1.EventTypeWarning, migrationReasonTargetDiskMismatch, targetDiskMismatchMessage)
	}
	return ctrl.Result{RequeueAfter: requestMarkerRecheckInterval}, nil
}
