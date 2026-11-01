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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Asynchronous provider Delete.
//
// A provider answers Delete either synchronously (an empty task reference: the
// hypervisor VM is gone when the RPC returns, and the finalizer is removed at
// once — unchanged) or with a task reference the manager must observe. For a
// task:
//
//   - The reference is recorded in status.lastTaskRef with DeleteTask=True
//     (the marker that says the reference is a Delete task, not a power task
//     left over from before the deletion), written as a status patch without a
//     resourceVersion precondition and retried (patchVMStatus), so a
//     concurrent metadata edit cannot make it conflict. The manager also keeps
//     it in memory (deleteTasks), so a lost write or a cache that has not
//     caught up never makes it send a second Delete while it runs, and the
//     next reconcile writes the record again before polling it.
//   - While a task is recorded, no Delete is sent: the task is polled with
//     TaskStatus. Completed without an error: the VM is gone and the finalizer
//     is removed. Running: polled again (5 s, growing to 1 min for a long
//     task). A TaskStatus RPC error: the outcome is unknown, the task stays
//     recorded and is asked again with a backoff (15 s doubling to 5 min) —
//     a transient error is never taken for a deleted VM.
//   - Ended with an error, or the provider no longer knows the reference (a
//     stale reference: not-found, invalid or unsupported, e.g. after a
//     provider restart): the provider has no Delete of this VM in flight, so
//     the reference is cleared (DeleteTask=False/DeleteTaskFailed) and the
//     Delete is sent again after a backoff (15 s doubling to 5 min). Sending
//     it again is the same retry a failed synchronous Delete gets: Delete is
//     idempotent under the provider contract (a VM that is already gone
//     answers not-found and releases the finalizer) and owner-checked (a VM
//     re-using the id that is not this VirtualMachine's answers VM_NOT_OWNED
//     and is never destroyed).
//   - After a manager restart the record is read back from status; the only
//     window in which a second Delete can be sent is a restart between the
//     provider accepting the Delete and the record landing, which the retried
//     patch makes narrow, and which the same idempotent, owner-checked Delete
//     covers.
//   - orphan-on-delete releases the VirtualMachine without a provider call,
//     as before; force-delete releases it (with the VMLeftOnHypervisor event)
//     whenever the task's outcome is not a confirmed success.

// vmDeleteTaskPollMin and vmDeleteTaskPollMax bound the poll of a running
// Delete task: the time the task has been recorded (the DeleteTask
// condition's LastTransitionTime, which survives a manager restart), at
// least the minimum and at most the maximum.
const (
	vmDeleteTaskPollMin = 5 * time.Second
	vmDeleteTaskPollMax = time.Minute
)

// vmDeleteTaskRecordRetryInterval is how long the manager waits before it
// tries again to record a Delete task whose status write did not land. No
// Delete is sent meanwhile: the reference is kept in memory.
const vmDeleteTaskRecordRetryInterval = 5 * time.Second

// Event reasons of an asynchronous Delete.
const (
	// eventReasonDeleteTaskFailed: the provider reported the Delete task
	// ended without deleting the VM, or no longer knows it.
	eventReasonDeleteTaskFailed = k8s.ReasonDeleteTaskFailed
	// eventReasonDeleteTaskStatusUnknown: the Delete task's status could not
	// be read.
	eventReasonDeleteTaskStatusUnknown = k8s.ReasonDeleteTaskStatusUnknown
	// eventReasonDeleteTaskNotRecorded: the Delete task could not be written
	// to the VirtualMachine's status.
	eventReasonDeleteTaskNotRecorded = "DeleteTaskNotRecorded"
)

// vmLeftDeleteTaskUnresolved is why a force-delete released a VirtualMachine
// whose Delete task had not been confirmed complete (releaseLeavingHypervisorVM).
const vmLeftDeleteTaskUnresolved = "the provider's Delete task had not completed"

// The DeleteTask (and Ready) messages. Each is constant: the provider's answer
// goes to the manager log only, so a repeated answer never rewrites the status.
var (
	deleteTaskInProgressMessage = fmt.Sprintf("Delete in progress: the provider is deleting the hypervisor VM "+
		"(task in status.lastTaskRef). The finalizer is kept until the provider reports the task complete. "+
		"To release the VirtualMachine without waiting, set %s=true (or %s=true); the hypervisor VM may then remain",
		infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation, forceDeleteAnnotation)
	deleteTaskStatusUnknownMessage = fmt.Sprintf("Delete in progress, outcome unknown: the provider could not be asked "+
		"for the status of its Delete task (status.lastTaskRef), so it is not known whether the hypervisor VM was "+
		"deleted. The finalizer is kept and no other Delete is sent; the status is asked again with a backoff of up to %s. "+
		"To release the VirtualMachine without a confirmed delete, set %s=true (or %s=true); the hypervisor VM may then remain",
		blockedRetryMax, infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation, forceDeleteAnnotation)
	deleteTaskFailedMessage = fmt.Sprintf("Delete in progress: the provider's Delete task ended without deleting the "+
		"hypervisor VM, or the provider no longer knows the task. The finalizer is kept and the Delete is sent again with "+
		"a backoff of up to %s. To release the VirtualMachine without a confirmed delete, set %s=true (or %s=true); the "+
		"hypervisor VM and its disks are then left for manual removal",
		blockedRetryMax, infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation, forceDeleteAnnotation)
)

// deleteTaskRecord is the manager's in-memory record of a VM's Delete task:
// ref is the task in flight, or empty once this manager saw the task end (its
// status record may not have caught up yet).
type deleteTaskRecord struct {
	ref string
}

// recordedDeleteTask returns the Delete task vm's status records: its
// status.lastTaskRef while DeleteTask is True, else empty.
func recordedDeleteTask(vm *infravirtrigaudiov1beta1.VirtualMachine) string {
	if !meta.IsStatusConditionTrue(vm.Status.Conditions, k8s.ConditionDeleteTask) {
		return ""
	}
	return vm.Status.LastTaskRef
}

// deleteTaskState returns this manager's in-memory record of vm's Delete task.
func (r *VirtualMachineReconciler) deleteTaskState(vm *infravirtrigaudiov1beta1.VirtualMachine) (deleteTaskRecord, bool) {
	v, ok := r.deleteTasks.Load(vmSchedulingUID(vm))
	if !ok {
		return deleteTaskRecord{}, false
	}
	rec, ok := v.(deleteTaskRecord)
	return rec, ok
}

// pendingDeleteTask returns the Delete task of vm that is in flight: this
// manager's in-memory record when it has one (it is never older than the
// status this manager wrote), else the one vm's status records (after a
// manager restart).
func (r *VirtualMachineReconciler) pendingDeleteTask(vm *infravirtrigaudiov1beta1.VirtualMachine) string {
	if rec, ok := r.deleteTaskState(vm); ok {
		return rec.ref
	}
	return recordedDeleteTask(vm)
}

// forgetDeleteTask drops everything the manager remembers of vm's Delete task.
func (r *VirtualMachineReconciler) forgetDeleteTask(vm *infravirtrigaudiov1beta1.VirtualMachine) {
	key := vmSchedulingUID(vm)
	r.deleteTasks.Delete(key)
	r.deleteTaskRetry.reset(key)
	r.deleteTaskStatusRetry.reset(key)
}

// setDeleteTaskCondition sets DeleteTask and Ready on vm for a Delete task
// with status, reason and msg, and removes a DeleteBlocked hold recorded
// earlier: the provider has answered.
func setDeleteTaskCondition(vm *infravirtrigaudiov1beta1.VirtualMachine, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionDeleteTask,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: vm.Generation,
	})
	readyReason := reason
	if reason == k8s.ReasonTaskInProgress {
		readyReason = k8s.ReasonDeleting
	}
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             readyReason,
		Message:            msg,
		ObservedGeneration: vm.Generation,
	})
	meta.RemoveStatusCondition(&vm.Status.Conditions, k8s.ConditionDeleteBlocked)
}

// markDeleteTaskInProgress records taskRef on vm as its Delete task in flight.
func markDeleteTaskInProgress(vm *infravirtrigaudiov1beta1.VirtualMachine, taskRef string) {
	vm.Status.LastTaskRef = taskRef
	setDeleteTaskCondition(vm, metav1.ConditionTrue, k8s.ReasonTaskInProgress, deleteTaskInProgressMessage)
}

// persistDeleteTask writes vm's status, which carries its Delete task record,
// as a status patch without a resourceVersion precondition, retried on
// transient errors (patchVMStatus): only this controller writes a deleting
// VM's status, so replacing it overwrites nothing another writer set.
func (r *VirtualMachineReconciler) persistDeleteTask(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) error {
	if err := r.patchVMStatus(ctx, vm, idTest{}); err != nil {
		return fmt.Errorf("record the Delete task of VirtualMachine %s/%s: %w", vm.Namespace, vm.Name, err)
	}
	return nil
}

// recordDeleteTask handles a provider Delete answered with taskRef: the task
// is remembered in memory first (no second Delete is sent while it runs),
// then written to status (markDeleteTaskInProgress). The finalizer is kept.
// The task is polled from the next reconcile on, once its record is stored;
// a record that did not land is written again first.
func (r *VirtualMachineReconciler) recordDeleteTask(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, taskRef string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	r.deleteTasks.Store(vmSchedulingUID(vm), deleteTaskRecord{ref: taskRef})
	markDeleteTaskInProgress(vm, taskRef)
	if err := r.persistDeleteTask(ctx, vm); err != nil {
		logger.Error(err, "The provider accepted the Delete as a task that could not be recorded; keeping it in memory, "+
			"retaining the finalizer and recording it again", "id", vm.Status.ID, "taskRef", taskRef)
		r.recordEvent(vm, corev1.EventTypeWarning, eventReasonDeleteTaskNotRecorded,
			"The provider accepted the Delete as an asynchronous task, which could not be recorded on this VirtualMachine's "+
				"status; the manager keeps waiting for it and records it again")
		return ctrl.Result{RequeueAfter: vmDeleteTaskRecordRetryInterval}, nil
	}
	logger.Info("VM deletion initiated as a provider task; retaining the finalizer until it completes",
		"id", vm.Status.ID, "taskRef", taskRef)
	return ctrl.Result{RequeueAfter: vmDeleteTaskPollMin}, nil
}

// continueDeleteTask runs before a provider Delete is sent for vm. A Delete
// task in flight is observed instead (awaitDeleteTask) and handled is true. A
// Delete whose last task ended in failure is not sent again before its
// backoff is over (handled, with the remaining wait), unless force-delete
// releases the VM. Otherwise handled is false and the caller sends the Delete.
func (r *VirtualMachineReconciler) continueDeleteTask(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	p contracts.Provider,
	force bool,
) (ctrl.Result, bool, error) {
	if ref := r.pendingDeleteTask(vm); ref != "" {
		res, err := r.awaitDeleteTask(ctx, vm, p, ref, force)
		return res, true, err
	}
	if _, ok := r.deleteTaskState(vm); ok {
		if wait := r.deleteTaskRetry.remaining(vmSchedulingUID(vm), r.now()); wait > 0 {
			if force {
				res, err := r.releaseLeavingHypervisorVM(ctx, vm, vmLeftDeleteFailed)
				return res, true, err
			}
			return ctrl.Result{RequeueAfter: wait}, true, nil
		}
	}
	return ctrl.Result{}, false, nil
}

// deleteTaskUnknownToProvider reports whether a TaskStatus error says the
// provider will never report the task: it does not know the reference
// (not-found, e.g. its task table was lost in a restart), refuses it as one it
// did not issue (invalid), or cannot report tasks (not supported). The
// provider has no Delete of the VM in flight that it can tell about.
func deleteTaskUnknownToProvider(err error) bool {
	return contracts.IsNotFound(err) || contracts.IsInvalidSpec(err) || contracts.IsNotSupported(err)
}

// awaitDeleteTask observes vm's Delete task taskRef (see the comment at the
// top of this file). A record that is not in vm's status yet is written
// first; the status is written again only when the observed state changes.
func (r *VirtualMachineReconciler) awaitDeleteTask(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	p contracts.Provider,
	taskRef string,
	force bool,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := vmSchedulingUID(vm)

	if recordedDeleteTask(vm) != taskRef {
		markDeleteTaskInProgress(vm, taskRef)
		if err := r.persistDeleteTask(ctx, vm); err != nil {
			if force {
				return r.releaseLeavingHypervisorVM(ctx, vm, vmLeftDeleteTaskUnresolved)
			}
			logger.Error(err, "Cannot record the provider's Delete task yet; retaining the finalizer and trying again",
				"id", vm.Status.ID, "taskRef", taskRef)
			return ctrl.Result{RequeueAfter: vmDeleteTaskRecordRetryInterval}, nil
		}
	}
	persisted := vm.Status.DeepCopy()

	st, err := p.TaskStatus(ctx, taskRef)
	switch {
	case err != nil && deleteTaskUnknownToProvider(err):
		return r.deleteTaskEnded(ctx, vm, persisted, taskRef, force, err.Error())
	case err != nil:
		// Unknown outcome: the task stays recorded and no Delete is sent.
		metrics.RecordError(errReasonProviderTask, metrics.ComponentManager)
		if force {
			logger.Error(err, "Cannot read the provider's Delete task status but force-delete annotation is set; removing finalizer",
				"id", vm.Status.ID, "taskRef", taskRef, "annotation", forceDeleteAnnotation)
			return r.releaseLeavingHypervisorVM(ctx, vm, vmLeftDeleteTaskUnresolved)
		}
		prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionDeleteTask)
		transition := prev == nil || prev.Reason != k8s.ReasonDeleteTaskStatusUnknown
		setDeleteTaskCondition(vm, metav1.ConditionTrue, k8s.ReasonDeleteTaskStatusUnknown, deleteTaskStatusUnknownMessage)
		retry := r.deleteTaskStatusRetry.nextWithin(key, r.now(), blockedRetryMin, blockedRetryMax)
		logger.Info("Cannot read the provider's Delete task status; retaining the finalizer and asking again",
			"id", vm.Status.ID, "taskRef", taskRef, "retryAfter", retry.String(), "error", err.Error())
		r.writeDeleteTaskStatus(ctx, vm, persisted)
		if transition {
			r.recordEvent(vm, corev1.EventTypeWarning, eventReasonDeleteTaskStatusUnknown, deleteTaskStatusUnknownMessage)
		}
		return ctrl.Result{RequeueAfter: retry}, nil
	case st.Error != "":
		return r.deleteTaskEnded(ctx, vm, persisted, taskRef, force, st.Error)
	case !st.IsCompleted:
		r.deleteTaskStatusRetry.reset(key)
		if force {
			logger.Info("The provider's Delete task is still running but force-delete annotation is set; removing finalizer",
				"id", vm.Status.ID, "taskRef", taskRef, "annotation", forceDeleteAnnotation)
			return r.releaseLeavingHypervisorVM(ctx, vm, vmLeftDeleteTaskUnresolved)
		}
		markDeleteTaskInProgress(vm, taskRef)
		r.writeDeleteTaskStatus(ctx, vm, persisted)
		retry := min(max(time.Since(conditionSince(vm.Status.Conditions, k8s.ConditionDeleteTask)), vmDeleteTaskPollMin), vmDeleteTaskPollMax)
		logger.V(1).Info("Provider Delete task still running; retaining the finalizer", "id", vm.Status.ID, "taskRef", taskRef,
			"retryAfter", retry.String())
		return ctrl.Result{RequeueAfter: retry}, nil
	default:
		logger.Info("Provider Delete task completed; the hypervisor VM is deleted", "id", vm.Status.ID, "taskRef", taskRef)
		return r.removeFinalizer(ctx, vm)
	}
}

// deleteTaskEnded handles a Delete task the provider reports ended in failure
// or no longer knows (detail is its answer, for the log only): with
// force-delete the VM is released; otherwise the reference is cleared,
// DeleteTask=False/DeleteTaskFailed recorded, and the Delete is sent again
// after the backoff (continueDeleteTask).
func (r *VirtualMachineReconciler) deleteTaskEnded(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	persisted *infravirtrigaudiov1beta1.VirtualMachineStatus,
	taskRef string,
	force bool,
	detail string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := vmSchedulingUID(vm)
	metrics.RecordError(errReasonProviderDelete, metrics.ComponentManager)
	if force {
		logger.Info("The provider's Delete task did not delete the VM but force-delete annotation is set; removing finalizer",
			"id", vm.Status.ID, "taskRef", taskRef, "annotation", forceDeleteAnnotation, "detail", detail)
		return r.releaseLeavingHypervisorVM(ctx, vm, vmLeftDeleteFailed)
	}
	r.deleteTasks.Store(key, deleteTaskRecord{})
	r.deleteTaskStatusRetry.reset(key)
	retry := r.deleteTaskRetry.nextWithin(key, r.now(), vmDeleteRetryInterval, blockedRetryMax)
	vm.Status.LastTaskRef = ""
	setDeleteTaskCondition(vm, metav1.ConditionFalse, k8s.ReasonDeleteTaskFailed, deleteTaskFailedMessage)
	logger.Info("The provider's Delete task ended without deleting the VM; retaining the finalizer and sending the Delete again",
		"id", vm.Status.ID, "taskRef", taskRef, "retryAfter", retry.String(), "detail", detail)
	r.writeDeleteTaskStatus(ctx, vm, persisted)
	r.recordEvent(vm, corev1.EventTypeWarning, eventReasonDeleteTaskFailed, deleteTaskFailedMessage)
	return ctrl.Result{RequeueAfter: retry}, nil
}

// writeDeleteTaskStatus writes vm's status when it differs from persisted. A
// failed write is logged only: the in-memory record already holds what the
// next reconcile acts on.
func (r *VirtualMachineReconciler) writeDeleteTaskStatus(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	persisted *infravirtrigaudiov1beta1.VirtualMachineStatus,
) {
	if !statusDiffers(persisted, vm) {
		return
	}
	if err := r.persistDeleteTask(ctx, vm); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update the VirtualMachine's Delete task status")
	}
}
