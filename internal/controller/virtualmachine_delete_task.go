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
	"strings"
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
// once — unchanged) or with a task reference the manager must observe. The
// state of that observation is the DeleteTask condition (with
// status.lastTaskRef), so it survives a manager restart:
//
//   - Recorded: DeleteTask=True and status.lastTaskRef, written as a status
//     patch without a resourceVersion precondition and retried
//     (patchVMStatus), so a concurrent metadata edit cannot make it conflict.
//     The manager also keeps it in memory (deleteTasks), so a lost write or a
//     cache that has not caught up never makes it send a second Delete while
//     it runs; the next reconcile writes the record again before polling it.
//   - Running (True): no Delete is sent; the task is polled with TaskStatus.
//     Completed without an error: the provider confirmed the delete and the
//     finalizer is removed. Still running: polled again (5 s, growing to
//     1 min for a long task). A transient TaskStatus error: the task stays
//     recorded and is asked again with a backoff (15 s doubling to 5 min).
//   - Ambiguous answer (Unknown): the provider answered that it does not know
//     the task — not-found, invalid, unsupported, or a terminal answer that
//     says so (deleteTaskAnswerUnknown) — typically after it restarted and
//     lost its task table, while the destroy may still be running. That is no
//     evidence either way, so no Delete is sent again. The manager asks with
//     the same owner-checked Describe the rest of the controller relies on:
//     reported absent (Exists=false, not-found) or another owner's
//     (VM_NOT_OWNED) — the evidence the synchronous Delete's not-found and
//     VM_NOT_OWNED answers already release on — releases the finalizer, with
//     an event; still present, or not answered, holds it with a backoff
//     counted from when the outcome became unknown, until an operator sets
//     orphan-on-delete or force-delete.
//   - Failed (False): an explicit terminal error from the provider. The
//     reference is cleared and the Delete is sent again after a backoff
//     counted from when the task failed — the condition's
//     LastTransitionTime, so a restart does not shorten it — the time already
//     spent deleting, between 15 s and 5 min. This is the manager's choice,
//     the same retry a failed synchronous Delete gets; it relies on each
//     provider's Delete answering not-found for a VM that is gone and refusing
//     (VM_NOT_OWNED) a VM at the id that is not this VirtualMachine's, as the
//     bundled providers do. The provider contract itself makes no such
//     guarantee.
//   - Provider object gone while a task is recorded: the finalizer is kept
//     (DeleteTaskProviderNotFound) instead of the release a VM with no task
//     gets.
//   - orphan-on-delete releases the VirtualMachine without a provider call,
//     as before; force-delete releases it (with the VMLeftOnHypervisor event)
//     whenever the delete is not confirmed.

// vmDeleteTaskPollMin and vmDeleteTaskPollMax bound the poll of a running
// Delete task: the time since the task was recorded, at least the minimum and
// at most the maximum.
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
	// failed.
	eventReasonDeleteTaskFailed = k8s.ReasonDeleteTaskFailed
	// eventReasonDeleteTaskStatusUnknown: the Delete task's status could not
	// be read.
	eventReasonDeleteTaskStatusUnknown = k8s.ReasonDeleteTaskStatusUnknown
	// eventReasonDeleteTaskOutcomeUnknown: the provider does not know the
	// Delete task.
	eventReasonDeleteTaskOutcomeUnknown = k8s.ReasonDeleteTaskOutcomeUnknown
	// eventReasonDeleteTaskProviderNotFound: the Provider object was deleted
	// while a Delete task was recorded.
	eventReasonDeleteTaskProviderNotFound = k8s.ReasonDeleteTaskProviderNotFound
	// eventReasonDeleteTaskNotRecorded: the Delete task could not be written
	// to the VirtualMachine's status.
	eventReasonDeleteTaskNotRecorded = "DeleteTaskNotRecorded"
	// eventReasonDeleteConfirmedAbsent: a Delete whose task outcome was
	// unknown was confirmed by the provider reporting the VM absent.
	eventReasonDeleteConfirmedAbsent = "DeleteConfirmedAbsent"
)

// Why a force-delete released a VirtualMachine with an unresolved Delete task
// (releaseLeavingHypervisorVM).
const (
	vmLeftDeleteTaskUnresolved   = "the provider's Delete task had not been confirmed complete"
	vmLeftDeleteTaskProviderGone = "its Provider was deleted before its Delete task was confirmed complete"
)

// deleteTaskRecovery is the recovery sentence every DeleteTask message ends
// with.
var deleteTaskRecovery = fmt.Sprintf("To detach the VirtualMachine without deleting the hypervisor VM, set %s=true; "+
	"to release it without a confirmed delete, set %s=true. Either way the hypervisor VM and its disks may remain "+
	"and must then be checked and removed by hand",
	infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation, forceDeleteAnnotation)

// The DeleteTask (and Ready) messages. Each is constant: the provider's answer
// goes to the manager log only, so a repeated answer never rewrites the status.
var (
	deleteTaskInProgressMessage = "Deleting: the provider is deleting the hypervisor VM as an asynchronous task " +
		"(status.lastTaskRef). The finalizer is kept until the provider reports the task completed. " + deleteTaskRecovery
	deleteTaskStatusUnknownMessage = fmt.Sprintf("Deleting, status unknown: the provider could not be asked for the "+
		"status of its Delete task (status.lastTaskRef), so it is not known whether the hypervisor VM was deleted. The "+
		"finalizer is kept and no other Delete is sent; the status is asked again with a backoff of up to %s. %s",
		blockedRetryMax, deleteTaskRecovery)
	deleteTaskOutcomeUnknownMessage = fmt.Sprintf("Delete outcome unknown: the provider no longer knows its Delete task "+
		"(status.lastTaskRef), for example after it restarted, so it is not known whether the hypervisor VM was or is being "+
		"deleted. No other Delete is sent. The finalizer is kept until the provider reports the VM absent; it is checked "+
		"again with a backoff of up to %s. %s", blockedRetryMax, deleteTaskRecovery)
	deleteTaskFailedMessage = fmt.Sprintf("Delete failed: the provider reported that its Delete task failed, so the "+
		"hypervisor VM may still exist. The finalizer is kept and the Delete is sent again with a backoff of up to %s. %s",
		blockedRetryMax, deleteTaskRecovery)
	deleteTaskProviderNotFoundMessage = "Delete held: this VirtualMachine's Provider was deleted while the provider's " +
		"Delete task (status.lastTaskRef) was not confirmed complete, so the hypervisor VM may still exist. The finalizer " +
		"is kept until the Provider is re-created. " + deleteTaskRecovery
)

// deleteTaskPhase is where the observation of a VM's Delete task stands.
type deleteTaskPhase int

const (
	// deleteTaskNone: no Delete task (none was returned, or it was settled).
	deleteTaskNone deleteTaskPhase = iota
	// deleteTaskRunning: the task is polled with TaskStatus (DeleteTask=True).
	deleteTaskRunning
	// deleteTaskUnresolved: the provider does not know the task; the VM's
	// absence is checked with Describe (DeleteTask=Unknown).
	deleteTaskUnresolved
	// deleteTaskFailed: the task failed; the Delete is sent again after the
	// backoff (DeleteTask=False/DeleteTaskFailed).
	deleteTaskFailed
)

// deleteTaskRecord is a VM's Delete task state: its phase, its reference
// (running and unresolved) and when the phase began (unresolved and failed,
// for their backoffs).
type deleteTaskRecord struct {
	phase deleteTaskPhase
	ref   string
	since time.Time
}

// recordedDeleteTask returns the Delete task state vm's status records.
func recordedDeleteTask(vm *infravirtrigaudiov1beta1.VirtualMachine) deleteTaskRecord {
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionDeleteTask)
	if c == nil {
		return deleteTaskRecord{}
	}
	switch {
	case c.Status == metav1.ConditionTrue && vm.Status.LastTaskRef != "":
		return deleteTaskRecord{phase: deleteTaskRunning, ref: vm.Status.LastTaskRef, since: c.LastTransitionTime.Time}
	case c.Status == metav1.ConditionUnknown:
		return deleteTaskRecord{phase: deleteTaskUnresolved, ref: vm.Status.LastTaskRef, since: c.LastTransitionTime.Time}
	case c.Status == metav1.ConditionFalse && c.Reason == k8s.ReasonDeleteTaskFailed:
		return deleteTaskRecord{phase: deleteTaskFailed, since: c.LastTransitionTime.Time}
	}
	return deleteTaskRecord{}
}

// deleteTaskState returns vm's Delete task state: this manager's in-memory
// record when it has one (it is never older than the status this manager
// wrote, which a cache may not show yet), else the one vm's status records
// (after a manager restart or a leader change).
func (r *VirtualMachineReconciler) deleteTaskState(vm *infravirtrigaudiov1beta1.VirtualMachine) deleteTaskRecord {
	if v, ok := r.deleteTasks.Load(vmSchedulingUID(vm)); ok {
		if rec, ok := v.(deleteTaskRecord); ok {
			return rec
		}
	}
	return recordedDeleteTask(vm)
}

// rememberDeleteTask sets this manager's in-memory record of vm's Delete task.
func (r *VirtualMachineReconciler) rememberDeleteTask(vm *infravirtrigaudiov1beta1.VirtualMachine, rec deleteTaskRecord) {
	r.deleteTasks.Store(vmSchedulingUID(vm), rec)
}

// hasUnresolvedDeleteTask reports whether vm has a Delete task whose outcome
// is not settled: running or unresolved.
func (r *VirtualMachineReconciler) hasUnresolvedDeleteTask(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	switch r.deleteTaskState(vm).phase {
	case deleteTaskRunning, deleteTaskUnresolved:
		return true
	}
	return false
}

// forgetDeleteTask drops everything the manager remembers of vm's Delete task.
func (r *VirtualMachineReconciler) forgetDeleteTask(vm *infravirtrigaudiov1beta1.VirtualMachine) {
	key := vmSchedulingUID(vm)
	r.deleteTasks.Delete(key)
	r.deleteTaskStatusRetry.reset(key)
}

// setDeleteTaskCondition sets DeleteTask and Ready on vm with status, reason
// and msg — a status change is stamped with the reconciler's clock, the
// anchor of the backoffs — and removes a DeleteBlocked hold recorded earlier.
func (r *VirtualMachineReconciler) setDeleteTaskCondition(vm *infravirtrigaudiov1beta1.VirtualMachine,
	status metav1.ConditionStatus, reason, msg string) {
	now := metav1.NewTime(r.now())
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionDeleteTask,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: vm.Generation,
		LastTransitionTime: now,
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
		LastTransitionTime: now,
	})
	meta.RemoveStatusCondition(&vm.Status.Conditions, k8s.ConditionDeleteBlocked)
}

// markDeleteTaskInProgress records taskRef on vm as its Delete task in flight.
func (r *VirtualMachineReconciler) markDeleteTaskInProgress(vm *infravirtrigaudiov1beta1.VirtualMachine, taskRef string) {
	vm.Status.LastTaskRef = taskRef
	r.setDeleteTaskCondition(vm, metav1.ConditionTrue, k8s.ReasonTaskInProgress, deleteTaskInProgressMessage)
}

// clearFailedDeleteTask removes a settled DeleteTask=False/DeleteTaskFailed
// from vm, and this manager's record of it, once the Delete is sent again: the
// next attempt's outcome replaces it (a new task, a release, or a retain path,
// whose Ready replaces the DeleteTaskFailed one — clearStaleDeleteHold for an
// ordinary failure). It reports whether vm's status changed.
func (r *VirtualMachineReconciler) clearFailedDeleteTask(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	if rec, ok := r.deleteTasks.Load(vmSchedulingUID(vm)); ok {
		if rec, ok := rec.(deleteTaskRecord); ok && rec.phase == deleteTaskFailed {
			r.deleteTasks.Delete(vmSchedulingUID(vm))
		}
	}
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionDeleteTask)
	if c == nil || c.Status != metav1.ConditionFalse {
		return false
	}
	return meta.RemoveStatusCondition(&vm.Status.Conditions, k8s.ConditionDeleteTask)
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
	r.rememberDeleteTask(vm, deleteTaskRecord{phase: deleteTaskRunning, ref: taskRef, since: r.now()})
	r.markDeleteTaskInProgress(vm, taskRef)
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

// failedDeleteRetryWait is how long after a Delete task failed at failedAt the
// Delete is sent again: the time vm had already spent being deleted then,
// between vmDeleteRetryInterval and blockedRetryMax — so successive failures
// wait about twice as long each time, and the wait is the same after a manager
// restart (failedAt is the DeleteTask condition's LastTransitionTime).
func failedDeleteRetryWait(vm *infravirtrigaudiov1beta1.VirtualMachine, failedAt time.Time) time.Duration {
	elapsed := time.Duration(0)
	if vm.DeletionTimestamp != nil {
		elapsed = failedAt.Sub(vm.DeletionTimestamp.Time)
	}
	return min(max(elapsed, vmDeleteRetryInterval), blockedRetryMax)
}

// continueDeleteTask runs before a provider Delete is sent for vm (ref is the
// owner-checked address of its VM). A Delete task that is unresolved is
// observed instead (awaitDeleteTask, verifyDeleteTaskByDescribe) and handled
// is true. A Delete whose last task failed is not sent again before its
// backoff is over (handled, with the remaining wait), unless force-delete
// releases the VM. Otherwise handled is false and the caller sends the Delete.
func (r *VirtualMachineReconciler) continueDeleteTask(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	p contracts.Provider,
	ref contracts.VMRef,
	force bool,
) (ctrl.Result, bool, error) {
	rec := r.deleteTaskState(vm)
	switch rec.phase {
	case deleteTaskRunning:
		res, err := r.awaitDeleteTask(ctx, vm, p, ref, rec, force)
		return res, true, err
	case deleteTaskUnresolved:
		if res, ok, err := r.ensureDeleteTaskRecorded(ctx, vm, rec, force); !ok {
			return res, true, err
		}
		res, err := r.verifyDeleteTaskByDescribe(ctx, vm, p, ref, r.deleteTaskState(vm), force)
		return res, true, err
	case deleteTaskFailed:
		if wait := rec.since.Add(failedDeleteRetryWait(vm, rec.since)).Sub(r.now()); wait > 0 {
			if force {
				res, err := r.releaseLeavingHypervisorVM(ctx, vm, vmLeftDeleteFailed)
				return res, true, err
			}
			return ctrl.Result{RequeueAfter: wait}, true, nil
		}
	}
	return ctrl.Result{}, false, nil
}

// ensureDeleteTaskRecorded writes rec into vm's status when its status does
// not show it yet (a lost write, or a cache that has not caught up), before
// anything relies on it. ok is false when the write failed: the caller
// returns res (a retry, or a force-delete release).
func (r *VirtualMachineReconciler) ensureDeleteTaskRecorded(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	rec deleteTaskRecord,
	force bool,
) (ctrl.Result, bool, error) {
	stored := recordedDeleteTask(vm)
	if stored.phase == rec.phase && stored.ref == rec.ref {
		return ctrl.Result{}, true, nil
	}
	vm.Status.LastTaskRef = rec.ref
	if rec.phase == deleteTaskUnresolved {
		r.setDeleteTaskCondition(vm, metav1.ConditionUnknown, k8s.ReasonDeleteTaskOutcomeUnknown, deleteTaskOutcomeUnknownMessage)
	} else {
		r.markDeleteTaskInProgress(vm, rec.ref)
	}
	if err := r.persistDeleteTask(ctx, vm); err != nil {
		if force {
			res, rerr := r.releaseLeavingHypervisorVM(ctx, vm, vmLeftDeleteTaskUnresolved)
			return res, false, rerr
		}
		log.FromContext(ctx).Error(err, "Cannot record the provider's Delete task yet; retaining the finalizer and trying again",
			"id", vm.Status.ID, "taskRef", rec.ref)
		return ctrl.Result{RequeueAfter: vmDeleteTaskRecordRetryInterval}, false, nil
	}
	return ctrl.Result{}, true, nil
}

// unknownTaskAnswers are the terminal TaskStatus errors with which the bundled
// providers say they do not know a task reference at all (the mock provider's
// lost task table; Proxmox VE rejecting a UPID it does not report or cannot
// parse). They say nothing about the delete's outcome.
var unknownTaskAnswers = []string{"task not found", "does not report this task", "invalid task id format"}

// deleteTaskAnswerUnknown reports whether a TaskStatus answer says the
// provider does not know the task (rather than that it ran and failed): an
// RPC error that is not-found, invalid or unsupported, or a terminal error of
// unknownTaskAnswers.
func deleteTaskAnswerUnknown(st contracts.TaskStatus, err error) bool {
	if err != nil {
		return contracts.IsNotFound(err) || contracts.IsInvalidSpec(err) || contracts.IsNotSupported(err)
	}
	msg := strings.ToLower(st.Error)
	for _, s := range unknownTaskAnswers {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// awaitDeleteTask polls vm's running Delete task rec (see the comment at the
// top of this file). A record that is not in vm's status yet is written
// first; the status is written again only when the observed state changes.
func (r *VirtualMachineReconciler) awaitDeleteTask(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	p contracts.Provider,
	ref contracts.VMRef,
	rec deleteTaskRecord,
	force bool,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := vmSchedulingUID(vm)
	taskRef := rec.ref

	if res, ok, err := r.ensureDeleteTaskRecorded(ctx, vm, rec, force); !ok {
		return res, err
	}
	persisted := vm.Status.DeepCopy()

	st, err := p.TaskStatus(ctx, taskRef)
	switch {
	case deleteTaskAnswerUnknown(st, err):
		detail := st.Error
		if err != nil {
			detail = err.Error()
		}
		return r.deleteTaskOutcomeUnknown(ctx, vm, p, ref, persisted, taskRef, force, detail)
	case err != nil:
		// The status could not be read: the task stays recorded and no Delete is sent.
		metrics.RecordError(errReasonProviderTask, metrics.ComponentManager)
		if force {
			logger.Error(err, "Cannot read the provider's Delete task status but force-delete annotation is set; removing finalizer",
				"id", vm.Status.ID, "taskRef", taskRef, "annotation", forceDeleteAnnotation)
			return r.releaseLeavingHypervisorVM(ctx, vm, vmLeftDeleteTaskUnresolved)
		}
		prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionDeleteTask)
		transition := prev == nil || prev.Reason != k8s.ReasonDeleteTaskStatusUnknown
		r.setDeleteTaskCondition(vm, metav1.ConditionTrue, k8s.ReasonDeleteTaskStatusUnknown, deleteTaskStatusUnknownMessage)
		retry := r.deleteTaskStatusRetry.nextWithin(key, r.now(), blockedRetryMin, blockedRetryMax)
		logger.Info("Cannot read the provider's Delete task status; retaining the finalizer and asking again",
			"id", vm.Status.ID, "taskRef", taskRef, "retryAfter", retry.String(), "error", err.Error())
		r.writeDeleteTaskStatus(ctx, vm, persisted)
		if transition {
			r.recordEvent(vm, corev1.EventTypeWarning, eventReasonDeleteTaskStatusUnknown, deleteTaskStatusUnknownMessage)
		}
		return ctrl.Result{RequeueAfter: retry}, nil
	case st.Error != "":
		return r.deleteTaskFailed(ctx, vm, persisted, taskRef, force, st.Error)
	case !st.IsCompleted:
		r.deleteTaskStatusRetry.reset(key)
		if force {
			logger.Info("The provider's Delete task is still running but force-delete annotation is set; removing finalizer",
				"id", vm.Status.ID, "taskRef", taskRef, "annotation", forceDeleteAnnotation)
			return r.releaseLeavingHypervisorVM(ctx, vm, vmLeftDeleteTaskUnresolved)
		}
		r.markDeleteTaskInProgress(vm, taskRef)
		r.writeDeleteTaskStatus(ctx, vm, persisted)
		age := r.now().Sub(conditionSince(vm.Status.Conditions, k8s.ConditionDeleteTask))
		retry := min(max(age, vmDeleteTaskPollMin), vmDeleteTaskPollMax)
		logger.V(1).Info("Provider Delete task still running; retaining the finalizer", "id", vm.Status.ID, "taskRef", taskRef,
			"retryAfter", retry.String())
		return ctrl.Result{RequeueAfter: retry}, nil
	default:
		logger.Info("Provider Delete task completed; the hypervisor VM is deleted", "id", vm.Status.ID, "taskRef", taskRef)
		return r.removeFinalizer(ctx, vm)
	}
}

// deleteTaskOutcomeUnknown handles a Delete task the provider does not know
// (detail is its answer, for the log only): DeleteTask=Unknown/
// DeleteTaskOutcomeUnknown is recorded (the task reference is kept, as the
// record of what was sent), and the VM's absence is checked at once
// (verifyDeleteTaskByDescribe). No Delete is sent again.
func (r *VirtualMachineReconciler) deleteTaskOutcomeUnknown(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	p contracts.Provider,
	ref contracts.VMRef,
	persisted *infravirtrigaudiov1beta1.VirtualMachineStatus,
	taskRef string,
	force bool,
	detail string,
) (ctrl.Result, error) {
	metrics.RecordError(errReasonProviderTask, metrics.ComponentManager)
	log.FromContext(ctx).Info("The provider does not know the Delete task; not sending the Delete again, checking whether the VM is gone",
		"id", vm.Status.ID, "taskRef", taskRef, "detail", detail)
	r.deleteTaskStatusRetry.reset(vmSchedulingUID(vm))
	r.setDeleteTaskCondition(vm, metav1.ConditionUnknown, k8s.ReasonDeleteTaskOutcomeUnknown, deleteTaskOutcomeUnknownMessage)
	rec := deleteTaskRecord{phase: deleteTaskUnresolved, ref: taskRef, since: conditionSince(vm.Status.Conditions, k8s.ConditionDeleteTask)}
	r.rememberDeleteTask(vm, rec)
	r.writeDeleteTaskStatus(ctx, vm, persisted)
	r.recordEvent(vm, corev1.EventTypeWarning, eventReasonDeleteTaskOutcomeUnknown, deleteTaskOutcomeUnknownMessage)
	return r.verifyDeleteTaskByDescribe(ctx, vm, p, ref, rec, force)
}

// verifyDeleteTaskByDescribe settles a Delete whose task outcome is unknown
// (rec, unresolved) with an owner-checked Describe of the VM: reported absent
// (Exists=false or not-found) releases the finalizer with a
// DeleteConfirmedAbsent event; another owner's VM at the id (VM_NOT_OWNED)
// releases it as a synchronous Delete answered VM_NOT_OWNED does
// (recordVMNotOwnedRelease). A clustered VM whose own domain is on another
// host is held as the synchronous path holds it. Anything else — the VM still
// there, or no answer — keeps the finalizer: force-delete releases it,
// otherwise it is checked again with a backoff counted from when the outcome
// became unknown. No Delete is sent.
func (r *VirtualMachineReconciler) verifyDeleteTaskByDescribe(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	p contracts.Provider,
	ref contracts.VMRef,
	rec deleteTaskRecord,
	force bool,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	desc, err := p.Describe(ctx, ref)
	absent := (err == nil && !desc.Exists) || (contracts.IsNotFound(err) && !contracts.IsVMNotOwned(err))
	switch {
	case absent && ref.Routed() && heldForOwnDomainElsewhere(vm):
		if !force {
			return r.holdDelete(ctx, vm, k8s.ReasonOwnDomainOnAnotherHost, ownDomainDeleteMessage, errOwnDomainUnplaced), nil
		}
		return r.releaseLeavingHypervisorVM(ctx, vm, vmLeftOwnDomainElsewhere)
	case absent:
		logger.Info("The provider reports the VM absent after a Delete task with an unknown outcome; removing the finalizer",
			"id", vm.Status.ID, "taskRef", rec.ref)
		res, rerr := r.removeFinalizer(ctx, vm)
		if rerr == nil {
			r.recordEvent(vm, corev1.EventTypeNormal, eventReasonDeleteConfirmedAbsent, fmt.Sprintf(
				"The provider did not know the Delete task, and reports the hypervisor VM %q absent: the delete is complete",
				vm.Status.ID))
		}
		return res, rerr
	case contracts.IsVMNotOwned(err):
		r.recordVMNotOwnedRelease(ctx, vm, ref)
		return r.removeFinalizer(ctx, vm)
	case force:
		logger.Info("The Delete task's outcome is unknown and the VM is not confirmed absent, but force-delete annotation is set; removing finalizer",
			"id", vm.Status.ID, "taskRef", rec.ref, "annotation", forceDeleteAnnotation)
		return r.releaseLeavingHypervisorVM(ctx, vm, vmLeftDeleteTaskUnresolved)
	}
	persisted := vm.Status.DeepCopy()
	r.setDeleteTaskCondition(vm, metav1.ConditionUnknown, k8s.ReasonDeleteTaskOutcomeUnknown, deleteTaskOutcomeUnknownMessage)
	r.writeDeleteTaskStatus(ctx, vm, persisted)
	retry := blockedRetryBackoffAt(rec.since, r.now())
	if err != nil {
		logger.Info("Cannot check whether the VM is gone after a Delete task with an unknown outcome; retaining the finalizer",
			"id", vm.Status.ID, "taskRef", rec.ref, "retryAfter", retry.String(), "error", err.Error())
	} else {
		logger.Info("The VM is still present after a Delete task with an unknown outcome; retaining the finalizer, not sending Delete again",
			"id", vm.Status.ID, "taskRef", rec.ref, "retryAfter", retry.String())
	}
	return ctrl.Result{RequeueAfter: retry}, nil
}

// blockedRetryBackoffAt is blockedRetryBackoff measured at now: the time spent
// since since, bounded to [blockedRetryMin, blockedRetryMax].
func blockedRetryBackoffAt(since, now time.Time) time.Duration {
	if since.IsZero() {
		return blockedRetryMin
	}
	return min(max(now.Sub(since), blockedRetryMin), blockedRetryMax)
}

// deleteTaskFailed handles a Delete task the provider explicitly reports
// failed (detail is its answer, for the log only): with force-delete the VM is
// released; otherwise the reference is cleared, DeleteTask=False/
// DeleteTaskFailed recorded, and the Delete is sent again after the backoff
// (continueDeleteTask, failedDeleteRetryWait).
func (r *VirtualMachineReconciler) deleteTaskFailed(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	persisted *infravirtrigaudiov1beta1.VirtualMachineStatus,
	taskRef string,
	force bool,
	detail string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	metrics.RecordError(errReasonProviderDelete, metrics.ComponentManager)
	if force {
		logger.Info("The provider's Delete task failed but force-delete annotation is set; removing finalizer",
			"id", vm.Status.ID, "taskRef", taskRef, "annotation", forceDeleteAnnotation, "detail", detail)
		return r.releaseLeavingHypervisorVM(ctx, vm, vmLeftDeleteFailed)
	}
	r.deleteTaskStatusRetry.reset(vmSchedulingUID(vm))
	vm.Status.LastTaskRef = ""
	r.setDeleteTaskCondition(vm, metav1.ConditionFalse, k8s.ReasonDeleteTaskFailed, deleteTaskFailedMessage)
	failedAt := conditionSince(vm.Status.Conditions, k8s.ConditionDeleteTask)
	r.rememberDeleteTask(vm, deleteTaskRecord{phase: deleteTaskFailed, since: failedAt})
	retry := failedDeleteRetryWait(vm, failedAt)
	logger.Info("The provider's Delete task failed; retaining the finalizer and sending the Delete again after the backoff",
		"id", vm.Status.ID, "taskRef", taskRef, "retryAfter", retry.String(), "detail", detail)
	r.writeDeleteTaskStatus(ctx, vm, persisted)
	r.recordEvent(vm, corev1.EventTypeWarning, eventReasonDeleteTaskFailed, deleteTaskFailedMessage)
	return ctrl.Result{RequeueAfter: retry}, nil
}

// retainForDeleteTaskProviderGone keeps the finalizer of vm, whose Provider
// object is gone while its Delete task is unresolved (hasUnresolvedDeleteTask):
// without the Provider the task can no longer be observed, so the release a
// VM with no task gets would leave a hypervisor VM whose delete was never
// confirmed. DeleteTask keeps its status (the phase) with reason
// DeleteTaskProviderNotFound; one Warning event when the hold begins; checked
// again with a backoff from when the task was recorded. force-delete releases
// it (orphan-on-delete has released it before this point).
func (r *VirtualMachineReconciler) retainForDeleteTaskProviderGone(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	force bool,
) (ctrl.Result, error) {
	if force {
		return r.releaseLeavingHypervisorVM(ctx, vm, vmLeftDeleteTaskProviderGone)
	}
	rec := r.deleteTaskState(vm)
	persisted := vm.Status.DeepCopy()
	status := metav1.ConditionTrue
	if rec.phase == deleteTaskUnresolved {
		status = metav1.ConditionUnknown
	}
	prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionDeleteTask)
	transition := prev == nil || prev.Reason != k8s.ReasonDeleteTaskProviderNotFound
	vm.Status.LastTaskRef = rec.ref
	r.setDeleteTaskCondition(vm, status, k8s.ReasonDeleteTaskProviderNotFound, deleteTaskProviderNotFoundMessage)
	retry := blockedRetryBackoffAt(conditionSince(vm.Status.Conditions, k8s.ConditionDeleteTask), r.now())
	log.FromContext(ctx).Info("The Provider was deleted while a Delete task is unresolved; retaining the finalizer",
		"id", vm.Status.ID, "taskRef", rec.ref, "retryAfter", retry.String())
	r.writeDeleteTaskStatus(ctx, vm, persisted)
	if transition {
		r.recordEvent(vm, corev1.EventTypeWarning, eventReasonDeleteTaskProviderNotFound, deleteTaskProviderNotFoundMessage)
	}
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
