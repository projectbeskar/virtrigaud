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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Snapshot revert (spec.snapshot.revertToRef, finding V2 of the v0.4.0-rc5
// validation: the field was accepted but no controller ever acted on it).
//
// The rules, decided by the maintainer:
//
//   - A revert is sent only while the provider reports the VM Off AND
//     spec.powerState is Off or OffGraceful. The controller never powers a VM
//     off for a revert, and does not power it on afterwards: spec.powerState
//     decides that, as always. While the VM is off, nothing else changes its
//     disks, so a revert sent again after a lost answer reaches the same
//     state as the first one.
//   - The VMSnapshot is resolved fail-closed: it must exist in the VM's
//     namespace, name this VM in spec.vmRef, be bound to this VM's UID and
//     Provider (status.vmUID, status.boundProvider), be Ready, not be
//     deleting, and carry the provider's snapshot id (status.snapshotID, the
//     id SnapshotCreate answered).
//   - At most once per request: status.snapshotRevert records the VMSnapshot
//     (name and UID) and the outcome. While it records Succeeded or Failed for
//     the VMSnapshot the field names, nothing is sent — a GitOps re-apply of
//     the same field, or any other spec change, never reverts again. Removing
//     the field clears the record; setting it again is a new request, and so
//     is a ref to another VMSnapshot (another UID).
//   - The attempt is recorded (phase Reverting, attempts, attemptTime) with
//     the optimistic lock of the resourceVersion this reconcile read BEFORE
//     the call; nothing is sent when that write fails, so a stale cache never
//     sends a second call. A provider task is recorded and polled. A call
//     whose outcome is unknown (a deadline, a dropped connection, a manager
//     restart between the call and the write of its answer) leaves the
//     record Reverting with no task: it is sent again, at most
//     snapshotRevertMaxAttempts calls in all and no sooner than
//     snapshotRevertResendWait after the last one, and only after a fresh
//     Describe has shown the VM still Off with spec.powerState still Off.
//   - A definite failure is terminal (phase Failed): it is not retried until
//     the field is removed and set again.

// snapshotRevertRecheckInterval re-checks a requested revert that waits for
// its VMSnapshot (missing, not Ready, not proven to be of this VM) or for a
// Provider that reports snapshot support. The VirtualMachine controller does
// not watch VMSnapshots, so the wait is a requeue; a spec change (the user
// powering the VM off, or naming another VMSnapshot) reconciles at once.
const snapshotRevertRecheckInterval = 30 * time.Second

// snapshotRevertTaskPollInterval polls a revert the provider runs as a task.
const snapshotRevertTaskPollInterval = 10 * time.Second

// snapshotRevertSettleInterval re-describes the VM shortly after a revert
// ends, so its new power state (a revert to a snapshot with memory state can
// leave it running) is observed and spec.powerState applied.
const snapshotRevertSettleInterval = 5 * time.Second

// snapshotRevertResendWait is how long after an attempt whose outcome is
// unknown the revert may be sent again (doubling with each attempt, at most
// snapshotRevertResendMaxWait). It is longer than the manager's own deadline
// for a SnapshotRevert call (5 minutes, internal/transport/grpc), so the
// earlier call has ended, on the manager's side at least, before the next one.
const snapshotRevertResendWait = 6 * time.Minute

// snapshotRevertResendMaxWait caps snapshotRevertResendWait's doubling.
const snapshotRevertResendMaxWait = 30 * time.Minute

// snapshotRevertMaxAttempts bounds the SnapshotRevert calls sent for one
// request whose earlier outcomes were unknown; past it the revert is Failed.
const snapshotRevertMaxAttempts = 5

// Event reasons recorded on a VirtualMachine for a snapshot revert.
const (
	// eventReasonSnapshotRevertStarted is recorded (Normal) when a
	// SnapshotRevert call is sent.
	eventReasonSnapshotRevertStarted = "SnapshotRevertStarted"
	// eventReasonSnapshotRevertSucceeded is recorded (Normal) when the
	// provider confirmed the revert.
	eventReasonSnapshotRevertSucceeded = "SnapshotRevertSucceeded"
	// eventReasonSnapshotRevertFailed is recorded (Warning) when the revert
	// failed, or its outcome stayed unknown after every allowed attempt.
	eventReasonSnapshotRevertFailed = "SnapshotRevertFailed"
)

// errReasonProviderSnapshotRevert is the metrics reason of a failed
// SnapshotRevert call or task.
const errReasonProviderSnapshotRevert = "provider-snapshot-revert"

// snapshotRevertCRDTooOldMessage is the SnapshotReverted message (reason
// Unsupported) when the API server does not store status.snapshotRevert.
const snapshotRevertCRDTooOldMessage = "the installed VirtualMachine CRD does not store status.snapshotRevert, " +
	"so the revert could not be recorded before it is sent; nothing is sent. Upgrade the CRDs to this manager's version"

// snapshotRevertRetryHint ends the message of a failed revert.
const snapshotRevertRetryHint = "remove spec.snapshot.revertToRef and set it again to retry"

// revertToRef returns the VMSnapshot reference spec.snapshot.revertToRef
// names, or nil.
func revertToRef(vm *infravirtrigaudiov1beta1.VirtualMachine) *infravirtrigaudiov1beta1.LocalObjectReference {
	if vm.Spec.Snapshot == nil {
		return nil
	}
	return vm.Spec.Snapshot.RevertToRef
}

// snapshotRevertTerminal reports whether phase ends a revert request.
func snapshotRevertTerminal(phase infravirtrigaudiov1beta1.SnapshotRevertPhase) bool {
	return phase == infravirtrigaudiov1beta1.SnapshotRevertPhaseSucceeded ||
		phase == infravirtrigaudiov1beta1.SnapshotRevertPhaseFailed
}

// setSnapshotRevertCondition sets the SnapshotReverted condition: True only
// for reason Reverted.
func setSnapshotRevertCondition(vm *infravirtrigaudiov1beta1.VirtualMachine, reason, message string) {
	status := metav1.ConditionFalse
	if reason == infravirtrigaudiov1beta1.SnapshotRevertReasonReverted {
		status = metav1.ConditionTrue
	}
	k8s.SetCondition(&vm.Status.Conditions, infravirtrigaudiov1beta1.VirtualMachineConditionSnapshotReverted,
		status, reason, message, vm.Generation)
}

// snapshotRevertReadyReasons are the reasons Ready takes from a False
// SnapshotReverted condition.
var snapshotRevertReadyReasons = map[string]bool{
	infravirtrigaudiov1beta1.SnapshotRevertReasonWaitingForPowerOff:   true,
	infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotNotFound:     true,
	infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotNotReady:     true,
	infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotForAnotherVM: true,
	infravirtrigaudiov1beta1.SnapshotRevertReasonUnsupported:          true,
	infravirtrigaudiov1beta1.SnapshotRevertReasonReverting:            true,
	infravirtrigaudiov1beta1.SnapshotRevertReasonRevertFailed:         true,
}

// applySnapshotRevertReadiness keeps Ready from claiming success while a
// requested revert is pending, in flight or failed: when the SnapshotReverted
// condition is False and Ready is True, unset, or False for an earlier step
// of the revert, Ready is set False with the revert's reason and message. A
// Ready that is False for another reason keeps that reason. It runs on every
// VirtualMachine status write of this controller (updateStatus), so no path
// that sets Ready=True can skip it.
func applySnapshotRevertReadiness(vm *infravirtrigaudiov1beta1.VirtualMachine) {
	c := meta.FindStatusCondition(vm.Status.Conditions, infravirtrigaudiov1beta1.VirtualMachineConditionSnapshotReverted)
	if c == nil || c.Status == metav1.ConditionTrue {
		return
	}
	if ready := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady); ready != nil &&
		ready.Status == metav1.ConditionFalse && !snapshotRevertReadyReasons[ready.Reason] {
		return
	}
	k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, c.Reason, "Snapshot revert: "+c.Message, vm.Generation)
}

// vmReadyMessage is the message of Ready=True on a VirtualMachine.
const vmReadyMessage = "VM is ready"

// setVMReady sets Ready=True on vm — unless a requested snapshot revert is
// pending, in flight or failed (SnapshotReverted=False): Ready is then False
// with the revert's reason and message.
func setVMReady(vm *infravirtrigaudiov1beta1.VirtualMachine) {
	if c := meta.FindStatusCondition(vm.Status.Conditions, infravirtrigaudiov1beta1.VirtualMachineConditionSnapshotReverted); c != nil &&
		c.Status != metav1.ConditionTrue {
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, c.Reason, "Snapshot revert: "+c.Message, vm.Generation)
		return
	}
	k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonReconcileSuccess, vmReadyMessage, vm.Generation)
}

// snapshotRevertResendDue is how long the unknown-outcome revert recorded in
// rec must still wait before it is sent again at now (0: it may be sent).
func snapshotRevertResendDue(rec *infravirtrigaudiov1beta1.SnapshotRevertStatus, now time.Time) time.Duration {
	if rec.AttemptTime == nil {
		return 0
	}
	wait := snapshotRevertResendWait
	for i := int32(1); i < rec.Attempts && wait < snapshotRevertResendMaxWait; i++ {
		wait *= 2
	}
	wait = min(wait, snapshotRevertResendMaxWait)
	if due := rec.AttemptTime.Add(wait); now.Before(due) {
		return due.Sub(now)
	}
	return 0
}

// snapshotRevertRefusal reports why snap cannot be reverted to for vm, whose
// Provider is provider: a SnapshotReverted reason and message, or "" when it
// can. It fails closed: a snapshot that cannot be proven to be this VM's is
// refused.
func snapshotRevertRefusal(snap *infravirtrigaudiov1beta1.VMSnapshot, vm *infravirtrigaudiov1beta1.VirtualMachine,
	provider *infravirtrigaudiov1beta1.Provider) (reason, message string) {
	switch {
	case snap.Spec.VMRef.Name != vm.Name:
		return infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotForAnotherVM, fmt.Sprintf(
			"VMSnapshot %s was taken of VirtualMachine %s, not of this one", snap.Name, snap.Spec.VMRef.Name)
	case !snap.DeletionTimestamp.IsZero():
		return infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotNotReady, fmt.Sprintf(
			"VMSnapshot %s is being deleted", snap.Name)
	case snap.Status.Phase != infravirtrigaudiov1beta1.SnapshotPhaseReady:
		phase := string(snap.Status.Phase)
		if phase == "" {
			phase = "not started"
		}
		return infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotNotReady, fmt.Sprintf(
			"VMSnapshot %s is not Ready (phase %s)", snap.Name, phase)
	case snap.Status.SnapshotID == "":
		return infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotNotReady, fmt.Sprintf(
			"VMSnapshot %s records no provider snapshot id (status.snapshotID)", snap.Name)
	case snap.Status.VMUID != "" && snap.Status.VMUID != string(vm.UID):
		return infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotForAnotherVM, fmt.Sprintf(
			"VMSnapshot %s was taken of an earlier VirtualMachine named %s (another UID), not of this one", snap.Name, vm.Name)
	case snap.Status.VMUID == "":
		// Taken by an older manager: only proven to be this VM's when its
		// timestamps say so (legacySnapshotOfVM; the VMSnapshot controller
		// records the binding on its next reconcile).
		if known, same := legacySnapshotOfVM(snap, vm); !known || !same {
			return infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotForAnotherVM, fmt.Sprintf(
				"VMSnapshot %s records no VM UID (status.vmUID) and cannot be proven to be of this VirtualMachine", snap.Name)
		}
	}
	if snapshotProviderMismatch(snap, provider) {
		return infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotForAnotherVM, fmt.Sprintf(
			"VMSnapshot %s was taken through Provider %s/%s, not through this VM's Provider %s/%s",
			snap.Name, snap.Status.BoundProvider.Namespace, snap.Status.BoundProvider.Name, provider.Namespace, provider.Name)
	}
	return "", ""
}

// snapshotRevertPowerRefusal is the WaitingForPowerOff message for vm, whose
// provider reports desc, or "" when the revert may be sent: the VM is
// observed Off and spec.powerState is Off or OffGraceful.
func snapshotRevertPowerRefusal(vm *infravirtrigaudiov1beta1.VirtualMachine, snapName string, desc contracts.DescribeResponse) string {
	desired := vm.Spec.PowerState
	if desired == "" {
		desired = infravirtrigaudiov1beta1.PowerStateOn
	}
	desiredOff := desired == infravirtrigaudiov1beta1.PowerStateOff || desired == infravirtrigaudiov1beta1.PowerStateOffGraceful
	if desiredOff && poweredOff(desc) {
		return ""
	}
	observed := desc.PowerState
	if observed == "" {
		observed = "not reported"
	}
	return fmt.Sprintf("the revert to VMSnapshot %s is sent only while the VM is powered off: set spec.powerState: Off "+
		"(VirtRigaud never powers a VM off for a revert). spec.powerState is %s, the provider reports the VM %s. "+
		"After the revert the VM follows spec.powerState", snapName, desired, observed)
}

// reconcileSnapshotRevert carries out the revert spec.snapshot.revertToRef
// requests (see the rules at the top of this file). It runs after a
// successful Describe of the bound VM (desc), with ref addressing it and
// provider its Provider object. handled reports that the reconcile ends here
// with res (a revert task in flight, a call just sent or waiting to be sent
// again, a marker write that failed); otherwise the reconcile goes on, and a
// non-zero res.RequeueAfter is the latest the caller should requeue at. A
// failed read of the VMSnapshot is returned as err.
func (r *VirtualMachineReconciler) reconcileSnapshotRevert(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerInstance contracts.Provider,
	provider *infravirtrigaudiov1beta1.Provider,
	ref contracts.VMRef,
	desc contracts.DescribeResponse,
) (res ctrl.Result, handled bool, err error) {
	rec := vm.Status.SnapshotRevert
	want := revertToRef(vm)

	// A revert task in flight is followed to its end whatever the spec says
	// now: the VM must not be powered on, resized or reverted again under it.
	if rec != nil && rec.Phase == infravirtrigaudiov1beta1.SnapshotRevertPhaseReverting && rec.TaskRef != "" {
		res, err := r.pollSnapshotRevertTask(ctx, vm, providerInstance)
		return res, true, err
	}

	// No revert requested: the record of an earlier one is cleared.
	if want == nil {
		if rec != nil || meta.FindStatusCondition(vm.Status.Conditions, infravirtrigaudiov1beta1.VirtualMachineConditionSnapshotReverted) != nil {
			log.FromContext(ctx).Info("spec.snapshot.revertToRef was removed; clearing the snapshot revert record")
			vm.Status.SnapshotRevert = nil
			meta.RemoveStatusCondition(&vm.Status.Conditions, infravirtrigaudiov1beta1.VirtualMachineConditionSnapshotReverted)
		}
		return ctrl.Result{}, false, nil
	}

	snap := &infravirtrigaudiov1beta1.VMSnapshot{}
	getErr := r.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: want.Name}, snap)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return ctrl.Result{}, true, fmt.Errorf("get VMSnapshot %s/%s for the revert of VirtualMachine %s: %w",
			vm.Namespace, want.Name, vm.Name, getErr)
	}
	found := getErr == nil
	sameRequest := rec != nil && rec.SnapshotName == want.Name &&
		(!found || rec.SnapshotUID == "" || rec.SnapshotUID == string(snap.UID))

	// Done: never sent again while the field names the same VMSnapshot (also
	// once that VMSnapshot is deleted). Another VMSnapshot of the same name
	// (another UID) is a new request.
	if sameRequest && snapshotRevertTerminal(rec.Phase) && rec.SnapshotUID != "" {
		r.showTerminalSnapshotRevert(vm, rec)
		return ctrl.Result{}, false, nil
	}

	// An attempt whose outcome is unknown (Reverting, no task): sent again
	// below, only to the same VMSnapshot (same UID). A VMSnapshot re-created
	// under the name meanwhile is a new request, decided as any other; one
	// that is gone ends the request.
	resend := sameRequest && rec.Phase == infravirtrigaudiov1beta1.SnapshotRevertPhaseReverting && rec.SnapshotUID != ""
	if resend && !found {
		return r.failSnapshotRevert(ctx, vm, infravirtrigaudiov1beta1.SnapshotRevertReasonRevertFailed, fmt.Sprintf(
			"VMSnapshot %s was deleted while the outcome of the revert to it was not known: the VM may or may not "+
				"have been reverted; %s", want.Name, snapshotRevertRetryHint))
	}

	if !found {
		return r.pendSnapshotRevert(vm, want.Name, "", infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotNotFound,
			fmt.Sprintf("VMSnapshot %s not found in namespace %s", want.Name, vm.Namespace))
	}
	if reason, msg := snapshotRevertRefusal(snap, vm, provider); reason != "" {
		if resend {
			return r.failSnapshotRevert(ctx, vm, infravirtrigaudiov1beta1.SnapshotRevertReasonRevertFailed,
				"the outcome of the revert is not known and it cannot be sent again: "+msg+"; "+snapshotRevertRetryHint)
		}
		return r.pendSnapshotRevert(vm, want.Name, string(snap.UID), reason, msg)
	}
	if caps := provider.Status.ReportedCapabilities; caps != nil && !caps.SupportsSnapshots {
		return r.pendSnapshotRevert(vm, want.Name, string(snap.UID), infravirtrigaudiov1beta1.SnapshotRevertReasonUnsupported,
			fmt.Sprintf("Provider %s reports no snapshot support (status.reportedCapabilities.supportsSnapshots is false)", provider.Name))
	}
	if msg := snapshotRevertPowerRefusal(vm, want.Name, desc); msg != "" {
		if resend {
			// The earlier attempt's outcome stays unknown; it is sent again
			// once the VM is off again.
			setSnapshotRevertCondition(vm, infravirtrigaudiov1beta1.SnapshotRevertReasonWaitingForPowerOff, msg)
			return ctrl.Result{}, false, nil
		}
		return r.pendSnapshotRevert(vm, want.Name, string(snap.UID), infravirtrigaudiov1beta1.SnapshotRevertReasonWaitingForPowerOff, msg)
	}
	if resend {
		if rec.Attempts >= snapshotRevertMaxAttempts {
			return r.failSnapshotRevert(ctx, vm, infravirtrigaudiov1beta1.SnapshotRevertReasonRevertFailed, fmt.Sprintf(
				"the provider gave no definite answer to %d revert attempts to VMSnapshot %s: the VM may or may not have been "+
					"reverted; %s", rec.Attempts, want.Name, snapshotRevertRetryHint))
		}
		if wait := snapshotRevertResendDue(rec, r.now()); wait > 0 {
			setSnapshotRevertCondition(vm, infravirtrigaudiov1beta1.SnapshotRevertReasonReverting, rec.Message)
			r.updateStatus(ctx, vm)
			return ctrl.Result{RequeueAfter: wait}, true, nil
		}
	}
	return r.sendSnapshotRevert(ctx, vm, providerInstance, ref, snap, resend)
}

// showTerminalSnapshotRevert keeps the SnapshotReverted condition in line
// with a finished request's record.
func (r *VirtualMachineReconciler) showTerminalSnapshotRevert(vm *infravirtrigaudiov1beta1.VirtualMachine,
	rec *infravirtrigaudiov1beta1.SnapshotRevertStatus) {
	if rec.Phase == infravirtrigaudiov1beta1.SnapshotRevertPhaseSucceeded {
		setSnapshotRevertCondition(vm, infravirtrigaudiov1beta1.SnapshotRevertReasonReverted, rec.Message)
		return
	}
	reason := infravirtrigaudiov1beta1.SnapshotRevertReasonRevertFailed
	if c := meta.FindStatusCondition(vm.Status.Conditions, infravirtrigaudiov1beta1.VirtualMachineConditionSnapshotReverted); c != nil &&
		c.Reason == infravirtrigaudiov1beta1.SnapshotRevertReasonUnsupported {
		reason = c.Reason
	}
	setSnapshotRevertCondition(vm, reason, rec.Message)
}

// pendSnapshotRevert records a requested revert that waits (phase Pending,
// the condition's reason and message) without any provider call. The record's
// first request time is kept while it is for the same VMSnapshot.
func (r *VirtualMachineReconciler) pendSnapshotRevert(vm *infravirtrigaudiov1beta1.VirtualMachine, name, uid, reason, msg string) (ctrl.Result, bool, error) {
	requestedAt := metav1.NewTime(r.now())
	if prev := vm.Status.SnapshotRevert; prev != nil && prev.SnapshotName == name && prev.RequestedAt != nil {
		requestedAt = *prev.RequestedAt
	}
	vm.Status.SnapshotRevert = &infravirtrigaudiov1beta1.SnapshotRevertStatus{
		SnapshotName: name,
		SnapshotUID:  uid,
		Phase:        infravirtrigaudiov1beta1.SnapshotRevertPhasePending,
		RequestedAt:  &requestedAt,
		Message:      msg,
	}
	setSnapshotRevertCondition(vm, reason, msg)
	return ctrl.Result{RequeueAfter: snapshotRevertRecheckInterval}, false, nil
}

// failSnapshotRevert ends the recorded request as Failed (terminal) with
// reason and msg, writes the status and records a Warning event.
func (r *VirtualMachineReconciler) failSnapshotRevert(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	reason, msg string) (ctrl.Result, bool, error) {
	log.FromContext(ctx).Info("Snapshot revert failed", "reason", reason, "detail", msg)
	metrics.RecordError(errReasonProviderSnapshotRevert, metrics.ComponentManager)
	rec := vm.Status.SnapshotRevert
	if rec == nil {
		rec = &infravirtrigaudiov1beta1.SnapshotRevertStatus{}
		if want := revertToRef(vm); want != nil {
			rec.SnapshotName = want.Name
		}
		vm.Status.SnapshotRevert = rec
	}
	now := metav1.NewTime(r.now())
	rec.Phase = infravirtrigaudiov1beta1.SnapshotRevertPhaseFailed
	rec.TaskRef = ""
	rec.CompletionTime = &now
	rec.Message = msg
	setSnapshotRevertCondition(vm, reason, msg)
	r.recordEvent(vm, corev1.EventTypeWarning, eventReasonSnapshotRevertFailed, msg)
	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: snapshotRevertSettleInterval}, true, nil
}

// succeedSnapshotRevert ends the recorded request as Succeeded, writes the
// status and records a Normal event.
func (r *VirtualMachineReconciler) succeedSnapshotRevert(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) (ctrl.Result, error) {
	rec := vm.Status.SnapshotRevert
	now := metav1.NewTime(r.now())
	rec.Phase = infravirtrigaudiov1beta1.SnapshotRevertPhaseSucceeded
	rec.TaskRef = ""
	rec.CompletionTime = &now
	rec.Message = fmt.Sprintf("reverted to VMSnapshot %s; the VM's power follows spec.powerState. To revert to it again, "+
		"remove spec.snapshot.revertToRef, then set it again", rec.SnapshotName)
	log.FromContext(ctx).Info("Snapshot revert succeeded", "snapshot", rec.SnapshotName, "attempts", rec.Attempts)
	setSnapshotRevertCondition(vm, infravirtrigaudiov1beta1.SnapshotRevertReasonReverted, rec.Message)
	// Ready was False only for the revert: the VM was just described, and
	// the revert is done.
	if ready := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady); ready != nil &&
		ready.Status == metav1.ConditionFalse && snapshotRevertReadyReasons[ready.Reason] {
		setVMReady(vm)
	}
	r.recordEvent(vm, corev1.EventTypeNormal, eventReasonSnapshotRevertSucceeded, "Reverted to VMSnapshot "+rec.SnapshotName)
	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: snapshotRevertSettleInterval}, nil
}

// sendSnapshotRevert records the attempt (phase Reverting) with the optimistic
// lock of the resourceVersion this reconcile read, and only then sends
// SnapshotRevert for snap. resend is the manager's own re-send of an attempt
// whose outcome was not known.
func (r *VirtualMachineReconciler) sendSnapshotRevert(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerInstance contracts.Provider,
	ref contracts.VMRef,
	snap *infravirtrigaudiov1beta1.VMSnapshot,
	resend bool,
) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	now := metav1.NewTime(r.now())
	prev := vm.Status.SnapshotRevert
	rec := &infravirtrigaudiov1beta1.SnapshotRevertStatus{
		SnapshotName: snap.Name,
		SnapshotUID:  string(snap.UID),
		Phase:        infravirtrigaudiov1beta1.SnapshotRevertPhaseReverting,
		Attempts:     1,
		RequestedAt:  &now,
		AttemptTime:  &now,
		Message:      fmt.Sprintf("reverting to VMSnapshot %s (provider snapshot %s)", snap.Name, snap.Status.SnapshotID),
	}
	if prev != nil && prev.SnapshotName == snap.Name && prev.RequestedAt != nil {
		rec.RequestedAt = prev.RequestedAt
	}
	if resend {
		rec.Attempts = prev.Attempts + 1
		rec.Message += fmt.Sprintf("; attempt %d: the outcome of the previous one is not known, and the VM is still powered off", rec.Attempts)
	}
	vm.Status.SnapshotRevert = rec
	setSnapshotRevertCondition(vm, infravirtrigaudiov1beta1.SnapshotRevertReasonReverting, rec.Message)

	// Nothing is sent unless the attempt is on record: a write that fails (a
	// Conflict: the cached VM was not the stored one) sends nothing.
	if err := r.writeStatusChecked(ctx, vm); err != nil {
		logger.Info("Cannot record the snapshot revert attempt; not sending it", "error", err.Error(),
			"conflict", apierrors.IsConflict(err))
		return ctrl.Result{RequeueAfter: providerErrorRetryInterval}, true, nil
	}
	// The write decoded the stored object into vm: take the record from it.
	// An API server whose VirtualMachine CRD predates status.snapshotRevert
	// prunes it: nothing could stop the revert from being sent again on
	// every reconcile, so none is sent.
	rec = vm.Status.SnapshotRevert
	if rec == nil {
		logger.Info("The installed VirtualMachine CRD does not store status.snapshotRevert; not sending the revert")
		setSnapshotRevertCondition(vm, infravirtrigaudiov1beta1.SnapshotRevertReasonUnsupported, snapshotRevertCRDTooOldMessage)
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: snapshotRevertRecheckInterval}, true, nil
	}
	logger.Info("Sending SnapshotRevert", "snapshot", snap.Name, "snapshot_id", snap.Status.SnapshotID,
		"attempt", rec.Attempts, "host", ref.HostID)
	r.recordEvent(vm, corev1.EventTypeNormal, eventReasonSnapshotRevertStarted,
		fmt.Sprintf("Reverting to VMSnapshot %s (attempt %d)", snap.Name, rec.Attempts))

	taskRef, err := providerInstance.SnapshotRevert(ctx, ref, snap.Status.SnapshotID)
	if err != nil {
		return r.snapshotRevertCallFailed(ctx, vm, err)
	}
	if taskRef != "" {
		rec.TaskRef = taskRef
		rec.Message = fmt.Sprintf("reverting to VMSnapshot %s (provider snapshot %s, task %s)", snap.Name, snap.Status.SnapshotID, taskRef)
		setSnapshotRevertCondition(vm, infravirtrigaudiov1beta1.SnapshotRevertReasonReverting, rec.Message)
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: snapshotRevertTaskPollInterval}, true, nil
	}
	res, err := r.succeedSnapshotRevert(ctx, vm)
	return res, true, err
}

// snapshotRevertCallFailed records a failed SnapshotRevert call. A failure
// whose outcome is not known — the provider not reached or not answering in
// time, a retryable answer — keeps the record Reverting with no task: it is
// sent again (reconcileSnapshotRevert) while the VM stays off. A definite
// answer (a refusal, an unimplemented RPC, an error the provider reported) is
// terminal.
func (r *VirtualMachineReconciler) snapshotRevertCallFailed(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, err error) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	rec := vm.Status.SnapshotRevert
	detail := providerFailureDetail(err)
	switch {
	case contracts.IsNotSupported(err):
		logger.Error(err, "The provider does not implement SnapshotRevert")
		return r.failSnapshotRevert(ctx, vm, infravirtrigaudiov1beta1.SnapshotRevertReasonUnsupported, fmt.Sprintf(
			"the provider does not support reverting to a snapshot: %s", detail))
	case contracts.IsRetryable(err) || isProviderTransportFailure(err) || contracts.IsOutcomeUnknownRetry(err):
		logger.Error(err, "SnapshotRevert did not reach a definite outcome; it is sent again while the VM stays off",
			"attempt", rec.Attempts)
		metrics.RecordError(errReasonProviderSnapshotRevert, metrics.ComponentManager)
		rec.Message = fmt.Sprintf("the outcome of revert attempt %d to VMSnapshot %s is not known (%s); it is sent again "+
			"while the VM stays powered off", rec.Attempts, rec.SnapshotName, detail)
		setSnapshotRevertCondition(vm, infravirtrigaudiov1beta1.SnapshotRevertReasonReverting, rec.Message)
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: snapshotRevertResendDue(rec, r.now())}, true, nil
	default:
		logger.Error(err, "SnapshotRevert failed")
		return r.failSnapshotRevert(ctx, vm, infravirtrigaudiov1beta1.SnapshotRevertReasonRevertFailed, fmt.Sprintf(
			"revert to VMSnapshot %s failed: %s; %s", rec.SnapshotName, detail, snapshotRevertRetryHint))
	}
}

// pollSnapshotRevertTask polls the revert task recorded in
// status.snapshotRevert.taskRef: Succeeded once it completed, Failed when it
// ended in failure. A task the provider no longer knows (NotFound) leaves the
// outcome unknown: the task is forgotten and the revert is sent again, as
// for any unknown outcome, while the VM stays off.
func (r *VirtualMachineReconciler) pollSnapshotRevertTask(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerInstance contracts.Provider) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	rec := vm.Status.SnapshotRevert
	done, err := providerInstance.IsTaskComplete(ctx, rec.TaskRef)
	switch {
	case err != nil && done:
		res, _, ferr := r.failSnapshotRevert(ctx, vm, infravirtrigaudiov1beta1.SnapshotRevertReasonRevertFailed, fmt.Sprintf(
			"revert to VMSnapshot %s failed: the provider task ended in failure: %s; %s",
			rec.SnapshotName, providerFailureDetail(err), snapshotRevertRetryHint))
		return res, ferr
	case err != nil && contracts.IsNotFound(err):
		logger.Info("The provider no longer knows the revert task; its outcome is not known", "taskRef", rec.TaskRef)
		rec.Message = fmt.Sprintf("the provider no longer knows revert task %s, so its outcome is not known; the revert to "+
			"VMSnapshot %s is sent again while the VM stays powered off", rec.TaskRef, rec.SnapshotName)
		rec.TaskRef = ""
		setSnapshotRevertCondition(vm, infravirtrigaudiov1beta1.SnapshotRevertReasonReverting, rec.Message)
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: snapshotRevertTaskPollInterval}, nil
	case err != nil:
		logger.Error(err, "Cannot check the snapshot revert task; checked again", "taskRef", rec.TaskRef)
		metrics.RecordError(errReasonProviderTask, metrics.ComponentManager)
		return ctrl.Result{RequeueAfter: snapshotRevertTaskPollInterval}, nil
	case !done:
		logger.V(1).Info("Snapshot revert task still in progress", "taskRef", rec.TaskRef)
		setSnapshotRevertCondition(vm, infravirtrigaudiov1beta1.SnapshotRevertReasonReverting, rec.Message)
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: snapshotRevertTaskPollInterval}, nil
	}
	return r.succeedSnapshotRevert(ctx, vm)
}

// writeStatusChecked writes vm's status like updateStatus, with the optimistic
// lock of vm's resourceVersion, and returns the error: nothing that must be on
// record before a provider call is sent unless it is.
func (r *VirtualMachineReconciler) writeStatusChecked(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) error {
	applySnapshotRevertReadiness(vm)
	applyDerivedPhase(vm)
	if err := r.Status().Update(ctx, vm); err != nil {
		return fmt.Errorf("update status of VirtualMachine %s/%s: %w", vm.Namespace, vm.Name, err)
	}
	return nil
}
