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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// The live Provider check before a creating RPC (ADR-0010). A Provider being
// deleted is held by its in-use finalizer while anything that records using
// it exists — a VirtualMachine's create marker or pending host, a clone, a
// migration. The check against the informer cache can be stale: a deletion
// that began after the cached read, and was already released by the in-use
// finalizer because the object had not yet recorded its use, would let a
// Create, Clone or ImportDisk make a hypervisor VM or disk through a Provider
// that is gone. So right before each of those RPCs — after the object has
// recorded its use — the Provider is re-read from the API server, and the RPC
// is not sent when it is being deleted, gone, or replaced by another object
// of the same name.

// providerDeletingLive reports whether provider is being deleted, read live
// through reader (the API server, not the cache): true when the live object
// has a deletionTimestamp, is gone, or has another UID (the object resolved
// earlier was deleted and re-created). A read error is returned: the caller
// sends nothing.
func providerDeletingLive(ctx context.Context, reader client.Reader, provider *infravirtrigaudiov1beta1.Provider) (bool, error) {
	live := &infravirtrigaudiov1beta1.Provider{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(provider), live); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("re-read Provider %s/%s: %w", provider.Namespace, provider.Name, err)
	}
	if provider.UID != "" && live.UID != provider.UID {
		return true, nil
	}
	return !live.DeletionTimestamp.IsZero(), nil
}

// holdIfProviderDeletingLive re-reads providerCR live right before vm's
// provider Create (providerDeletingLive) and, when it is being deleted (or
// gone), holds the VM instead (holdForDeletingProvider: Ready=False/
// ProviderDeleting, a constant message, one Warning event, a slow re-check)
// and reports held. A read error also holds — nothing is sent — and is
// retried on the provider-error cadence. persisted is the status as it was
// last written; the hold writes only when it changes it.
func (r *VirtualMachineReconciler) holdIfProviderDeletingLive(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	persisted *infravirtrigaudiov1beta1.VirtualMachineStatus,
	providerCR *infravirtrigaudiov1beta1.Provider,
) (ctrl.Result, bool) {
	deleting, err := providerDeletingLive(ctx, r.liveReader(), providerCR)
	if err != nil {
		log.FromContext(ctx).Error(err, "Cannot re-read the Provider before the Create; not creating")
		return ctrl.Result{RequeueAfter: providerErrorRetryInterval}, true
	}
	if !deleting {
		return ctrl.Result{}, false
	}
	return r.holdForDeletingProvider(ctx, vm, persisted, providerCR), true
}

// cloneProviderDeletingMessage is the Ready message of a VMClone held because
// its Provider is being deleted. It is constant.
const cloneProviderDeletingMessage = "The Provider of the source VirtualMachine is being deleted, and nothing is cloned " +
	"through a Provider that is: the clone is not started. Delete this VMClone, or wait until a Provider of that name " +
	"exists again and is not being deleted"

// holdCloneIfProviderDeletingLive re-reads provider live right before the
// Clone RPC and, when it is being deleted (or gone), holds the clone: Ready=
// False/ProviderDeleting with the constant cloneProviderDeletingMessage (Phase
// Pending while nothing was started), one Warning event when the hold begins,
// the status written only when it changes, and a re-check every
// providerDeletingRetryInterval. A read error is returned (nothing is sent).
func (r *VMCloneReconciler) holdCloneIfProviderDeletingLive(
	ctx context.Context,
	clone *infravirtrigaudiov1beta1.VMClone,
	provider *infravirtrigaudiov1beta1.Provider,
) (held bool, result ctrl.Result, err error) {
	deleting, err := providerDeletingLive(ctx, r.liveReader(), provider)
	if err != nil {
		log.FromContext(ctx).Error(err, "Cannot re-read the Provider before the Clone; not cloning")
		return true, ctrl.Result{}, err
	}
	if !deleting {
		return false, ctrl.Result{}, nil
	}
	log.FromContext(ctx).Info("Not cloning: the Provider is being deleted",
		"provider", provider.Namespace+"/"+provider.Name, "retryAfter", providerDeletingRetryInterval.String())
	metrics.RecordError(errReasonProviderDeleting, metrics.ComponentManager)
	before := clone.Status.DeepCopy()
	prev := meta.FindStatusCondition(clone.Status.Conditions, infravirtrigaudiov1beta1.VMCloneConditionReady)
	isNew := prev == nil || prev.Reason != k8s.ReasonProviderDeleting
	if clone.Status.TaskRef == "" && clone.Status.TargetVMID == "" {
		clone.Status.Phase = infravirtrigaudiov1beta1.ClonePhasePending
	}
	clone.Status.Message = cloneProviderDeletingMessage
	meta.SetStatusCondition(&clone.Status.Conditions, metav1.Condition{
		Type:               infravirtrigaudiov1beta1.VMCloneConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             k8s.ReasonProviderDeleting,
		Message:            cloneProviderDeletingMessage,
		ObservedGeneration: clone.Generation,
	})
	if isNew && r.Recorder != nil {
		r.Recorder.Event(clone, corev1.EventTypeWarning, k8s.ReasonProviderDeleting, cloneProviderDeletingMessage)
	}
	if !equality.Semantic.DeepEqual(before, &clone.Status) {
		_ = r.updateStatus(ctx, clone) //nolint:errcheck // status errors retried next reconcile
	}
	return true, ctrl.Result{RequeueAfter: providerDeletingRetryInterval}, nil
}

// migrationProviderDeletingMessage is the Ready message of a VMMigration held
// because its target Provider is being deleted. It is constant.
const migrationProviderDeletingMessage = "The target Provider is being deleted, and no disk is imported through a " +
	"Provider that is: the import is not started. Delete this VMMigration, or wait until a Provider of that name " +
	"exists again and is not being deleted"

// holdMigrationIfProviderDeletingLive re-reads the target provider live right
// before the ImportDisk RPC and, when it is being deleted (or gone), holds the
// migration in its phase: Ready=False/ProviderDeleting with the constant
// migrationProviderDeletingMessage, one Warning event when the hold begins,
// the status written only when it changes, and a re-check every
// providerDeletingRetryInterval. It is not a failure: nothing was imported,
// and the import starts once a Provider of that name is usable. When the
// Provider is usable, a hold recorded earlier is removed in memory (the next
// status write persists it). A read error is returned (nothing is sent).
func (r *VMMigrationReconciler) holdMigrationIfProviderDeletingLive(
	ctx context.Context,
	migration *infravirtrigaudiov1beta1.VMMigration,
	provider *infravirtrigaudiov1beta1.Provider,
) (held bool, result ctrl.Result, err error) {
	deleting, err := providerDeletingLive(ctx, r.liveReader(), provider)
	if err != nil {
		log.FromContext(ctx).Error(err, "Cannot re-read the target Provider before the import; not importing")
		return true, ctrl.Result{}, err
	}
	prev := meta.FindStatusCondition(migration.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionReady)
	wasHeld := prev != nil && prev.Reason == k8s.ReasonProviderDeleting
	if !deleting {
		if wasHeld {
			meta.RemoveStatusCondition(&migration.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionReady)
		}
		return false, ctrl.Result{}, nil
	}
	log.FromContext(ctx).Info("Not importing: the target Provider is being deleted",
		"provider", provider.Namespace+"/"+provider.Name, "retryAfter", providerDeletingRetryInterval.String())
	metrics.RecordError(errReasonProviderDeleting, metrics.ComponentManager)
	before := migration.Status.DeepCopy()
	migration.Status.Message = migrationProviderDeletingMessage
	migration.Status.ObservedGeneration = migration.Generation
	meta.SetStatusCondition(&migration.Status.Conditions, metav1.Condition{
		Type:               infravirtrigaudiov1beta1.VMMigrationConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             k8s.ReasonProviderDeleting,
		Message:            migrationProviderDeletingMessage,
		ObservedGeneration: migration.Generation,
	})
	if !wasHeld && r.Recorder != nil {
		r.Recorder.Event(migration, corev1.EventTypeWarning, k8s.ReasonProviderDeleting, migrationProviderDeletingMessage)
	}
	if !equality.Semantic.DeepEqual(before, &migration.Status) {
		if err := r.updateStatus(ctx, migration); err != nil {
			return true, ctrl.Result{}, err
		}
	}
	return true, ctrl.Result{RequeueAfter: providerDeletingRetryInterval}, nil
}
