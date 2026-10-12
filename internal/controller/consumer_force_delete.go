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
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// The cross-namespace force-delete gate (review M4), shared by the
// VirtualMachine, VMSnapshot and VMMigration controllers. force-delete
// releases an object without a confirmed provider delete, leaving its
// hypervisor state (a VM, a snapshot) behind. On a clustered Provider that
// state keeps occupying the shared hosts outside the capacity accounting, so
// an object in another namespace than its clustered Provider — a consumer —
// may do that only when the Provider allows it with
// infra.virtrigaud.io/allow-consumer-orphan-on-delete: "true". Otherwise the
// force-delete is ignored (the delete is retried as if it were not set) and
// ForceDeleteRefused=True/ForceDeleteNotAllowed says so, with a constant
// message and one Warning event.

// consumerDetachDeniedFor reports whether an object in namespace ns that acts
// through the Provider at key may NOT leave its hypervisor state behind: the
// Provider exists (read through reader), has topology: cluster, is in another
// namespace than ns, and does not carry
// ProviderAllowConsumerOrphanOnDeleteAnnotation: "true". A Provider that does
// not exist denies nothing (there is no accounting to leave); a failed read is
// returned as an error.
func consumerDetachDeniedFor(ctx context.Context, reader client.Reader, key types.NamespacedName, ns string) (bool, error) {
	provider := &infravirtrigaudiov1beta1.Provider{}
	if err := reader.Get(ctx, key, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get Provider %s to decide a consumer detach from namespace %s: %w", key, ns, err)
	}
	return isClusterTopology(provider) && ns != provider.Namespace &&
		provider.Annotations[infravirtrigaudiov1beta1.ProviderAllowConsumerOrphanOnDeleteAnnotation] != "true", nil
}

// snapshotForceDeleteRefusedMessage is the ForceDeleteRefused message (and
// Warning event) of a VMSnapshot in namespace ns whose VM's clustered Provider
// is provider. It is constant for a given VMSnapshot.
func snapshotForceDeleteRefusedMessage(ns string, provider types.NamespacedName) string {
	return fmt.Sprintf("%s=true is not honoured: VMSnapshots in namespace %s may not be released from the clustered "+
		"Provider %s without a confirmed provider delete (the snapshot would stay on its host outside the capacity "+
		"accounting). The delete keeps being retried as if the annotation were not set; ask the Provider's administrator "+
		"to set %s=\"true\" on it, or to remove the snapshot",
		forceDeleteAnnotation, ns, provider, infravirtrigaudiov1beta1.ProviderAllowConsumerOrphanOnDeleteAnnotation)
}

// migrationForceDeleteRefusedMessage is the ForceDeleteRefused message (and
// Warning event) of a VMMigration in namespace ns whose source VM's clustered
// Provider is provider. It is constant for a given VMMigration.
func migrationForceDeleteRefusedMessage(ns string, provider types.NamespacedName) string {
	return fmt.Sprintf("%s=true is not honoured: VMMigrations in namespace %s may not be released from the clustered "+
		"Provider %s without a confirmed delete of their source snapshot (it would stay on the source VM's host outside "+
		"the capacity accounting). The delete keeps being retried as if the annotation were not set; ask the Provider's "+
		"administrator to set %s=\"true\" on it, or to remove the snapshot",
		forceDeleteAnnotation, ns, provider, infravirtrigaudiov1beta1.ProviderAllowConsumerOrphanOnDeleteAnnotation)
}

// setForceDeleteRefused sets ForceDeleteRefused=True/ForceDeleteNotAllowed
// with msg in conds and reports whether that begins the refusal (it was not
// True before).
func setForceDeleteRefused(conds *[]metav1.Condition, generation int64, msg string) bool {
	prev := meta.FindStatusCondition(*conds, k8s.ConditionForceDeleteRefused)
	isNew := prev == nil || prev.Status != metav1.ConditionTrue
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               k8s.ConditionForceDeleteRefused,
		Status:             metav1.ConditionTrue,
		Reason:             k8s.ReasonForceDeleteNotAllowed,
		Message:            msg,
		ObservedGeneration: generation,
	})
	return isNew
}

// snapshotForceDeleteHonoured decides, for a VMSnapshot being deleted, whether
// its force-delete escape hatch may release it without a confirmed provider
// snapshot delete. It is false when the annotation is not set, and — the
// cross-namespace gate — when the snapshot records a provider snapshot of vm
// (nil: the VM no longer exists) and vm's clustered Provider is in another
// namespace and has not allowed consumers to leave state behind
// (consumerDetachDeniedFor). A refused force-delete sets ForceDeleteRefused
// (status written, and a Warning event recorded, only when it begins); the
// condition is removed once the force-delete is honoured or the annotation is
// gone. A failed Provider read is returned (the finalizer stays).
func (r *VMSnapshotReconciler) snapshotForceDeleteHonoured(
	ctx context.Context,
	snapshot *infravirtrigaudiov1beta1.VMSnapshot,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
) (bool, error) {
	if !hasSnapshotForceDeleteAnnotation(snapshot) {
		return false, r.clearSnapshotForceDeleteRefused(ctx, snapshot)
	}
	if vm == nil || snapshot.Status.SnapshotID == "" {
		return true, r.clearSnapshotForceDeleteRefused(ctx, snapshot)
	}
	key := placementProviderKey(vm)
	denied, err := consumerDetachDeniedFor(ctx, r.Client, key, snapshot.Namespace)
	if err != nil {
		return false, err
	}
	if !denied {
		return true, r.clearSnapshotForceDeleteRefused(ctx, snapshot)
	}
	msg := snapshotForceDeleteRefusedMessage(snapshot.Namespace, key)
	before := snapshot.Status.DeepCopy()
	isNew := setForceDeleteRefused(&snapshot.Status.Conditions, snapshot.Generation, msg)
	log.FromContext(ctx).Info("Not honouring force-delete of a consumer VMSnapshot on a clustered Provider; the delete is retried as if it were not set",
		"provider", key.String(), "annotation", forceDeleteAnnotation)
	metrics.RecordError(errReasonForceDeleteNotAllowed, metrics.ComponentManager)
	if isNew {
		r.Recorder.Event(snapshot, corev1.EventTypeWarning, k8s.ReasonForceDeleteNotAllowed, msg)
	}
	if !equality.Semantic.DeepEqual(before, &snapshot.Status) {
		if err := r.updateStatus(ctx, snapshot); err != nil {
			return false, err
		}
	}
	return false, nil
}

// clearSnapshotForceDeleteRefused removes ForceDeleteRefused from snapshot
// and writes the status when it was there.
func (r *VMSnapshotReconciler) clearSnapshotForceDeleteRefused(ctx context.Context, snapshot *infravirtrigaudiov1beta1.VMSnapshot) error {
	if meta.RemoveStatusCondition(&snapshot.Status.Conditions, k8s.ConditionForceDeleteRefused) {
		return r.updateStatus(ctx, snapshot)
	}
	return nil
}

// migrationForceDeleteHonoured decides, for a VMMigration being deleted whose
// source snapshot is still to be deleted, whether its force-delete escape
// hatch may release it without that delete. It is false when the annotation
// is not set, and — the cross-namespace gate — when the source VM's clustered
// Provider is in another namespace and has not allowed consumers to leave
// state behind (consumerDetachDeniedFor). A source VM that no longer exists
// leaves nothing to gate. A refused force-delete sets ForceDeleteRefused
// (status written, and a Warning event recorded, only when it begins); the
// condition is removed once the force-delete is honoured or the annotation is
// gone. A failed read is returned (the finalizer stays).
func (r *VMMigrationReconciler) migrationForceDeleteHonoured(ctx context.Context, migration *infravirtrigaudiov1beta1.VMMigration) (bool, error) {
	if !hasMigrationForceDeleteAnnotation(migration) {
		return false, r.clearMigrationForceDeleteRefused(ctx, migration)
	}
	sourceVM, err := r.getSourceVM(ctx, migration)
	if apierrors.IsNotFound(err) {
		return true, r.clearMigrationForceDeleteRefused(ctx, migration)
	}
	if err != nil {
		return false, fmt.Errorf("get source VM to decide force-delete of VMMigration %s/%s: %w", migration.Namespace, migration.Name, err)
	}
	key := placementProviderKey(sourceVM)
	denied, err := consumerDetachDeniedFor(ctx, r.Client, key, migration.Namespace)
	if err != nil {
		return false, err
	}
	if !denied {
		return true, r.clearMigrationForceDeleteRefused(ctx, migration)
	}
	msg := migrationForceDeleteRefusedMessage(migration.Namespace, key)
	before := migration.Status.DeepCopy()
	isNew := setForceDeleteRefused(&migration.Status.Conditions, migration.Generation, msg)
	log.FromContext(ctx).Info("Not honouring force-delete of a consumer VMMigration on a clustered Provider; the delete is retried as if it were not set",
		"provider", key.String(), "annotation", forceDeleteAnnotation)
	metrics.RecordError(errReasonForceDeleteNotAllowed, metrics.ComponentManager)
	if isNew {
		r.Recorder.Event(migration, corev1.EventTypeWarning, k8s.ReasonForceDeleteNotAllowed, msg)
	}
	if !equality.Semantic.DeepEqual(before, &migration.Status) {
		if err := r.updateStatus(ctx, migration); err != nil {
			return false, err
		}
	}
	return false, nil
}

// clearMigrationForceDeleteRefused removes ForceDeleteRefused from migration
// and writes the status when it was there.
func (r *VMMigrationReconciler) clearMigrationForceDeleteRefused(ctx context.Context, migration *infravirtrigaudiov1beta1.VMMigration) error {
	if meta.RemoveStatusCondition(&migration.Status.Conditions, k8s.ConditionForceDeleteRefused) {
		return r.updateStatus(ctx, migration)
	}
	return nil
}
