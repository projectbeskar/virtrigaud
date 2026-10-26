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
	"unicode/utf8"

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

// The create marker of a single-host VirtualMachine.
//
// Before a single-host provider Create is sent, the VM's status records
// Provisioning=True with reason CreateRequested (and the Provider it is sent
// through, status.boundProvider). It is written over the VirtualMachine as
// just read from the API server, with the optimistic lock of that read's
// resourceVersion (requestCreate): if the write fails — a Conflict included —
// nothing is sent. The marker is the durable record that the VM may exist on
// the hypervisor although no status.id is recorded (a lost record, a manager
// restart, a Create that ran past its deadline). It is replaced by the
// Create's outcome — the id and its Provisioning condition — once the
// provider answers, and kept when the answer leaves the outcome unknown (a
// transport failure, or an error the provider did not mark as "nothing done").
//
// Each marker write is one attempt; its attempt id is the marker's
// LastTransitionTime in RFC 3339 (markerAttemptID), strictly increasing from
// one attempt to the next.
//
// A VM with the marker and no id is handled as follows.
//
//   - Create: a provider that re-binds the VM it stamped with its owner and
//     reports it (GetCapabilitiesResponse.supports_owner_rebind,
//     providerRebindsOwnedVM: vSphere's owner extraConfig, libvirt's domain
//     metadata, Proxmox's owner stamp, the mock's record) is sent the same
//     Create again, which returns the existing VM's id. Any other provider —
//     one that predates the capability — could create a second VM, so the VM
//     waits, Ready=False/
//     CreateOutcomeUnknown, until an operator records the VM's id (a status
//     patch of status.id) or confirms that this attempt made no VM
//     (virtrigaud.io/create-outcome=not-created@<attempt id>).
//   - Delete: never released as if nothing had been created. A provider that
//     reports owner stamps on ListVMs (libvirt, Proxmox) is asked for the VM
//     stamped with the VirtualMachine's UID, and that VM is deleted by its
//     id; a listed VM whose stamp cannot be relied on makes the answer
//     ambiguous. The finalizer is released without an operator only on
//     positive confirmation (libvirt): once no Create can still be running,
//     every domain name the Create could have used is described and answered
//     a definite not-found. Otherwise the delete waits (CreateOutcomeUnknown) for the
//     same operator action; force-delete releases it with a
//     VMLeftOnHypervisor event, orphan-on-delete detaches it.
//
// Holds that do not create (ProviderDeleting) never write the marker.

// createOutcomeAnnotation, set by an operator to
// createOutcomeNotCreatedPrefix + the marker's attempt id on a VirtualMachine
// waiting with Ready=False/CreateOutcomeUnknown, confirms that that attempt
// made no VM: the manager removes the annotation and creates the VM (or,
// while it is being deleted, releases it). Any other value is ignored and
// removed, and the annotation is removed before every new attempt, so a
// confirmation never outlives the attempt it names.
const createOutcomeAnnotation = "virtrigaud.io/create-outcome"

// createOutcomeNotCreatedPrefix starts a createOutcomeAnnotation value that
// confirms an attempt made no VM; the attempt id follows it.
const createOutcomeNotCreatedPrefix = "not-created@"

// eventReasonCreateOutcomeConfirmed is the Warning event reason recorded
// (audit) when an operator's createOutcomeAnnotation authorizes a create or
// a release.
const eventReasonCreateOutcomeConfirmed = "CreateOutcomeConfirmed"

// eventReasonCreateConfirmedAbsent is the Normal event reason recorded when a
// deleting VM is released because the provider confirmed that its requested
// Create made no VM.
const eventReasonCreateConfirmedAbsent = "CreateConfirmedAbsent"

// createRequestedMessage is the message of the create marker.
const createRequestedMessage = "A provider Create was requested for this VM (the attempt id is this condition's " +
	"lastTransitionTime); its outcome is recorded here once the provider answers"

// createOutcomeUnknownMessage is the Ready message of a VM held because its
// requested Create's outcome is unknown and the provider cannot tell.
var createOutcomeUnknownMessage = fmt.Sprintf("A provider Create was sent for this VM but its outcome was never recorded, and "+
	"this Provider does not report that it re-binds the VM it made (supports_owner_rebind), so no second Create is sent. "+
	"Check the hypervisor for this VM. If it exists, record its id with a status patch of status.id; if it does not, "+
	"annotate the VirtualMachine %s=%s<attempt id>, the attempt id being the lastTransitionTime of the Provisioning "+
	"condition. Re-checked every %s",
	createOutcomeAnnotation, createOutcomeNotCreatedPrefix, createOutcomeUnknownRetryInterval)

// createOutcomeUnknownDeleteMessage is the Ready message of a VM whose delete
// is held because its requested Create's outcome is unknown.
var createOutcomeUnknownDeleteMessage = fmt.Sprintf("Delete held: a provider Create was sent for this VM but its outcome was "+
	"never recorded, and the manager cannot confirm whether it made a VM, so the finalizer is kept rather than leave that "+
	"VM running unmanaged. If the VM exists, record its id with a status patch of status.id (it is then deleted); if it "+
	"does not, annotate the VirtualMachine %s=%s<attempt id> (the lastTransitionTime of the Provisioning condition). "+
	"%s=true releases it without deleting anything, %s=true detaches it",
	createOutcomeAnnotation, createOutcomeNotCreatedPrefix, forceDeleteAnnotation,
	infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation)

// createOutcomeUnknownRetryInterval re-checks a create held for an unknown
// outcome: it ends with an operator's action.
const createOutcomeUnknownRetryInterval = time.Minute

// createOutcomeSettleTime is how long after the last Create was requested the
// lookup of a deleting VM may conclude that it made no VM: longer than the
// manager's deadline for a Create call, so a Create the provider is still
// running is not mistaken for none.
const createOutcomeSettleTime = 10 * time.Minute

// errReasonCreateOutcomeUnknown is the metrics reason of a create or delete
// held for an unknown create outcome.
const errReasonCreateOutcomeUnknown = "create-outcome-unknown"

// vmLeftCreateOutcomeUnknown is why a force-delete released a VM whose
// requested Create's outcome is unknown (releaseLeavingHypervisorVM).
const vmLeftCreateOutcomeUnknown = "a Create was sent for it whose outcome was never recorded"

// createRequested reports whether vm carries the create marker: no status.id,
// and Provisioning with reason CreateRequested.
func createRequested(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	if vm == nil || vm.Status.ID != "" {
		return false
	}
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionProvisioning)
	return c != nil && c.Reason == k8s.ReasonCreateRequested
}

// markerAttemptID is the attempt id of vm's create marker — its
// LastTransitionTime in RFC 3339, UTC — or "" when it has none.
func markerAttemptID(vm *infravirtrigaudiov1beta1.VirtualMachine) string {
	if !createRequested(vm) {
		return ""
	}
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionProvisioning)
	return c.LastTransitionTime.UTC().Format(time.RFC3339)
}

// setCreateRequested sets a new create marker on vm, at now: an attempt id
// strictly later than that of any marker vm carries already (the
// LastTransitionTime has a one-second resolution), so a confirmation of one
// attempt never matches the next.
func setCreateRequested(vm *infravirtrigaudiov1beta1.VirtualMachine, now time.Time) {
	at := now.UTC().Truncate(time.Second)
	if prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionProvisioning); prev != nil &&
		prev.Reason == k8s.ReasonCreateRequested && !at.After(prev.LastTransitionTime.UTC()) {
		at = prev.LastTransitionTime.UTC().Truncate(time.Second).Add(time.Second)
	}
	meta.RemoveStatusCondition(&vm.Status.Conditions, k8s.ConditionProvisioning)
	vm.Status.Conditions = append(vm.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionProvisioning,
		Status:             metav1.ConditionTrue,
		Reason:             k8s.ReasonCreateRequested,
		Message:            createRequestedMessage,
		ObservedGeneration: vm.Generation,
		LastTransitionTime: metav1.NewTime(at),
	})
}

// setCreateOutcome replaces the create marker with the outcome of a Create
// the provider confirmed: still running (an async task) or done.
func setCreateOutcome(vm *infravirtrigaudiov1beta1.VirtualMachine, async bool) {
	if async {
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonCreating, "VM creation initiated", vm.Generation)
		return
	}
	k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonReconcileSuccess, "VM created", vm.Generation)
}

// setCreateFailure records a create that failed before any provider call
// (an invalid spec, a request that cannot be built) with reason and msg: on
// Provisioning, or — while vm carries the create marker of an earlier attempt
// whose outcome is unknown — on Ready only, so the marker is kept.
func setCreateFailure(vm *infravirtrigaudiov1beta1.VirtualMachine, reason, msg string) {
	condType := k8s.ConditionProvisioning
	if createRequested(vm) {
		condType = k8s.ConditionReady
	}
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: vm.Generation,
	})
}

// createLookupByOwner reports whether provider's ListVMs reports every VM's
// owner stamp (contracts.VMInfoOwnerUIDKey, and VMInfoOwnerStampStateKey for
// a stamp it cannot rely on), so the VM a Create made for a VirtualMachine
// can be found by its UID: libvirt and Proxmox. vSphere re-binds on Create
// but does not report stamps on ListVMs.
func createLookupByOwner(provider *infravirtrigaudiov1beta1.Provider) bool {
	switch provider.Spec.Type {
	case infravirtrigaudiov1beta1.ProviderTypeLibvirt, infravirtrigaudiov1beta1.ProviderTypeProxmox:
		return true
	}
	return false
}

// createConfirmByDomainName reports whether the names a Create for a
// VirtualMachine could have used are known (contracts.LibvirtCreateDomainNames), so a
// definite not-found for each confirms that it made no VM: libvirt. A
// Proxmox VM is identified by a VMID the provider allocates, so its absence
// cannot be confirmed by name.
func createConfirmByDomainName(provider *infravirtrigaudiov1beta1.Provider) bool {
	return provider.Spec.Type == infravirtrigaudiov1beta1.ProviderTypeLibvirt
}

// createNothingDone reports whether a failed Create is an answer in which the
// provider says it made nothing: CREATE_NOT_PERFORMED
// (contracts.IsCreateNotPerformed), or a refusal no retry can change
// (createRefusedByProvider). VM_OPERATION_FAILED, HOST_UNAVAILABLE and
// VM_DISK_CHECK_FAILED are NOT such answers: a provider also sends them for
// an operation that may still complete on its host (libvirt: an outcome it
// could not observe, the same copy still running), so the outcome of a
// Create answered with one stays unknown.
func createNothingDone(err error) bool {
	return contracts.IsCreateNotPerformed(err) || createRefusedByProvider(err)
}

// createDefinitelyRefused reports whether a failed first Create (see
// createOutcomeKnown for a re-send) is an answer in which the provider refused
// it before making anything, so the create marker is replaced by the failure: InvalidSpec, NotFound, NotSupported,
// Unauthorized / PermissionDenied or QuotaExceeded, a "nothing made" answer
// (createNothingDone: CREATE_NOT_PERFORMED, a non-retryable
// FailedPrecondition), or a Conflict — a first Create's name collision, which
// the providers refuse before creating anything; a Conflict answering a
// re-send is not one (createOutcomeKnown). Any other failure — a transport
// failure, a deadline, an error the provider did not classify,
// VM_OPERATION_FAILED, HOST_UNAVAILABLE or VM_DISK_CHECK_FAILED — leaves the
// outcome unknown, and the marker is kept.
func createDefinitelyRefused(err error) bool {
	if contracts.IsConflict(err) || contracts.IsInvalidSpec(err) || contracts.IsNotFound(err) || contracts.IsNotSupported(err) ||
		createNothingDone(err) {
		return true
	}
	var pe *contracts.ProviderError
	return stderrors.As(err, &pe) &&
		(pe.Type == contracts.ErrorTypeUnauthorized || pe.Type == contracts.ErrorTypeQuotaExceeded)
}

// createOutcomeKnown is createDefinitelyRefused for a Create of attempt. A
// failure answering a re-send (attempt.resend, or a re-create of a VM whose
// id is recorded) never makes the outcome known, whatever it is: it is the
// answer to THIS request only, and says nothing about what the earlier
// attempt — whose outcome is the unknown one — made. (vSphere, for example,
// answers Conflict for the VM an earlier attempt made while its clone is
// still in progress; and an InvalidSpec, PermissionDenied or
// CREATE_NOT_PERFORMED answering the re-send does not undo a VM the first
// attempt may have left.) The marker is then kept and the answer recorded on
// Ready only.
func createOutcomeKnown(err error, attempt createAttempt) bool {
	if attempt.resend || attempt.priorID != "" {
		return false
	}
	return createDefinitelyRefused(err)
}

// createAttempt describes the single-host Create about to be requested.
type createAttempt struct {
	// priorID is the id the stored VM may still record (a re-create of a VM
	// whose hypervisor VM is gone).
	priorID string
	// markerID is the attempt id of the create marker the reconcile decided
	// on (a re-send, or an operator's confirmation); "" for a VM the cache
	// showed without a marker.
	markerID string
	// resend is set when the Create is the same Create sent again to a
	// provider that re-binds the VM it made.
	resend bool
	// confirmed is set when an operator's createOutcomeAnnotation confirmed
	// that attempt markerID made no VM.
	confirmed bool
	// sentMarkerID is the attempt id of the create marker the single-host
	// Create was sent under (requestCreate wrote it); "" for a clustered one.
	sentMarkerID string
	// sentHost is the pending host a clustered Create was sent to; "" for a
	// single-host one.
	sentHost string
}

// createOutcomeConfirmed reports whether vm's createOutcomeAnnotation
// confirms that the attempt of its create marker made no VM.
func createOutcomeConfirmed(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	id := markerAttemptID(vm)
	return id != "" && vm.Annotations[createOutcomeAnnotation] == createOutcomeNotCreatedPrefix+id
}

// requestCreate prepares a single-host Create of vm through providerCR and
// reports whether it may be sent.
//
// The VM is read live (uncached). It is not created when it is gone or
// replaced, is being deleted, has a newer spec (generation) than the one this
// reconcile built the request from, records an id meanwhile (other than
// attempt.priorID), or its
// create marker is not the one the reconcile decided on: a marker the cached
// copy did not show, or — for a re-send or a confirmation — another attempt,
// or a confirmation that is no longer there. Any createOutcomeAnnotation is
// removed before the new attempt. Then the new marker and the Provider are
// written over the live object with the optimistic lock of its
// resourceVersion; if that write fails (a Conflict included), nothing is
// sent. On success vm carries the written status and resourceVersion.
func (r *VirtualMachineReconciler) requestCreate(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerCR *infravirtrigaudiov1beta1.Provider,
	attempt createAttempt,
) (ctrl.Result, bool) {
	logger := log.FromContext(ctx)
	requeue := ctrl.Result{RequeueAfter: providerErrorRetryInterval}
	live, gone, err := r.readLiveVM(ctx, vm)
	switch {
	case err != nil:
		logger.Error(err, "Cannot read the VirtualMachine before its Create; not creating")
		return requeue, false
	case gone:
		logger.Info("The VirtualMachine was deleted or replaced; not creating")
		return ctrl.Result{}, false
	case !live.DeletionTimestamp.IsZero():
		logger.Info("The VirtualMachine is being deleted; not creating")
		return requeue, false
	case live.Generation != vm.Generation:
		logger.Info("The VirtualMachine's spec changed since this reconcile read it; not creating",
			"read", vm.Generation, "live", live.Generation)
		return requeue, false
	case live.Status.ID != "" && live.Status.ID != attempt.priorID:
		logger.Info("The VirtualMachine records a provider id already; not creating", "id", live.Status.ID)
		r.forgetUnsavedCreate(vm.UID)
		return requeue, false
	case markerAttemptID(live) != attempt.markerID:
		logger.Info("The VirtualMachine's requested Create is not the one this reconcile decided on; not creating",
			"recorded", markerAttemptID(live), "decided", attempt.markerID)
		return requeue, false
	case attempt.confirmed && !createOutcomeConfirmed(live):
		logger.Info("The operator's confirmation is no longer on the VirtualMachine; not creating")
		return requeue, false
	}

	// A confirmation names one attempt: it is removed before the next.
	if _, ok := live.Annotations[createOutcomeAnnotation]; ok {
		if err := r.removeCreateOutcomeAnnotation(ctx, live); err != nil {
			logger.Error(err, "Cannot remove the create-outcome annotation before a new Create; not creating")
			return requeue, false
		}
	}

	toWrite := live.DeepCopy()
	toWrite.Status.ID = ""
	toWrite.Status.ObservedGeneration = toWrite.Generation
	setCreateRequested(toWrite, r.now())
	carryDisksApplied(vm, toWrite)
	recordBoundProvider(toWrite, providerCR)
	applyDerivedPhase(toWrite)
	writeCtx, cancel := context.WithTimeout(ctx, createRecordAttemptTimeout)
	defer cancel()
	if err := r.Status().Update(writeCtx, toWrite); err != nil {
		logger.Error(err, "Cannot record the requested Create on the VirtualMachine; not creating",
			"conflict", apierrors.IsConflict(err))
		return requeue, false
	}
	vm.ResourceVersion = toWrite.ResourceVersion
	vm.Annotations = toWrite.Annotations
	vm.Status = toWrite.Status
	return ctrl.Result{}, true
}

// resolveCreateMarker decides the create of a single-host VM that carries the
// create marker (createRequested) and has no unsaved record. proceed reports
// whether the caller creates — with attempt describing the decision, and
// attempt.resend when the Create is re-sent as it was (its image was prepared
// before the first attempt); otherwise res is the result to return. A
// createOutcomeAnnotation that does not confirm this marker's attempt is
// ignored and removed.
func (r *VirtualMachineReconciler) resolveCreateMarker(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	persisted *infravirtrigaudiov1beta1.VirtualMachineStatus,
	instance contracts.Provider,
) (res ctrl.Result, attempt createAttempt, proceed bool, err error) {
	logger := log.FromContext(ctx)
	attempt.markerID = markerAttemptID(vm)
	switch {
	case createOutcomeConfirmed(vm):
		logger.Info("An operator confirmed that the requested Create made no VM; creating it",
			"annotation", createOutcomeAnnotation, "attempt", attempt.markerID)
		// The audit event is recorded once the confirmation is acted on: after
		// the new marker is written (recordConfirmedCreate).
		attempt.confirmed = true
		return ctrl.Result{}, attempt, true, nil
	case vm.Annotations[createOutcomeAnnotation] != "":
		if err := r.ignoreCreateOutcomeAnnotation(ctx, vm); err != nil {
			return ctrl.Result{}, attempt, false, err
		}
	}
	rebinds, capErr := providerRebindsOwnedVM(ctx, instance)
	if capErr != nil {
		// Never re-send on an unknown answer: try again later.
		logger.Info("Cannot read the provider's capabilities; not re-sending the requested Create yet", "error", capErr.Error())
		return ctrl.Result{RequeueAfter: providerErrorRetryInterval}, attempt, false, nil
	}
	if rebinds {
		// Re-sends are paced (review L1): while they keep failing, each
		// waits longer, so an outage does not rewrite the marker and Ready
		// every few seconds.
		if wait := resendWait(vm, time.Now()); wait > 0 {
			logger.V(1).Info("Waiting before re-sending the requested Create", "retryAfter", wait.String())
			return ctrl.Result{RequeueAfter: wait}, attempt, false, nil
		}
		logger.Info("A requested Create's outcome was not recorded; re-sending it (the provider re-binds the VM it stamped)")
		attempt.resend = true
		return ctrl.Result{}, attempt, true, nil
	}
	return r.holdCreateOutcomeUnknown(ctx, vm, persisted, createOutcomeUnknownMessage, createOutcomeUnknownRetryInterval), attempt, false, nil
}

// holdCreateOutcomeUnknown records Ready=False/CreateOutcomeUnknown with msg
// on vm (the marker is left as it is), writes the status only when that
// changes it, records a Warning event when the hold begins, and returns
// retry.
func (r *VirtualMachineReconciler) holdCreateOutcomeUnknown(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	persisted *infravirtrigaudiov1beta1.VirtualMachineStatus,
	msg string,
	retry time.Duration,
) ctrl.Result {
	prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
	isNew := prev == nil || prev.Reason != k8s.ReasonCreateOutcomeUnknown || prev.Message != msg
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             k8s.ReasonCreateOutcomeUnknown,
		Message:            msg,
		ObservedGeneration: vm.Generation,
	})
	log.FromContext(ctx).Info("A requested Create's outcome is unknown and cannot be confirmed; waiting for an operator",
		"provider", placementProviderKey(vm).String(), "attempt", markerAttemptID(vm), "retryAfter", retry.String())
	metrics.RecordError(errReasonCreateOutcomeUnknown, metrics.ComponentManager)
	if statusDiffers(persisted, vm) {
		r.updateStatus(ctx, vm)
	}
	if isNew {
		r.recordEvent(vm, corev1.EventTypeWarning, k8s.ReasonCreateOutcomeUnknown, msg)
	}
	return ctrl.Result{RequeueAfter: retry}
}

// removeCreateOutcomeAnnotation removes createOutcomeAnnotation from vm (a
// metadata patch; vm then carries the stored object's metadata).
func (r *VirtualMachineReconciler) removeCreateOutcomeAnnotation(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) error {
	if _, ok := vm.Annotations[createOutcomeAnnotation]; !ok {
		return nil
	}
	status := vm.Status.DeepCopy()
	// Optimistic lock: the patch succeeds only if the VirtualMachine is still
	// the version this reconcile read, so keeping this reconcile's status
	// under the new resourceVersion below can never carry a status older than
	// one another writer stored meanwhile. A conflict is returned and the
	// reconcile runs again from the live object.
	patch := client.MergeFromWithOptions(vm.DeepCopy(), client.MergeFromWithOptimisticLock{})
	delete(vm.Annotations, createOutcomeAnnotation)
	if err := r.Patch(ctx, vm, patch); err != nil {
		return fmt.Errorf("remove annotation %s from VirtualMachine %s/%s: %w", createOutcomeAnnotation, vm.Namespace, vm.Name, err)
	}
	// A metadata patch answers with the stored status; keep this reconcile's.
	vm.Status = *status
	return nil
}

// ignoreCreateOutcomeAnnotation removes a createOutcomeAnnotation that does
// not confirm the attempt of vm's create marker (a value for an older
// attempt, or a malformed one): it authorizes nothing.
func (r *VirtualMachineReconciler) ignoreCreateOutcomeAnnotation(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) error {
	log.FromContext(ctx).Info("Ignoring and removing a create-outcome annotation that does not name the current attempt",
		"value", truncateForLog(vm.Annotations[createOutcomeAnnotation], maxLoggedAnnotationBytes), "attempt", markerAttemptID(vm))
	return r.removeCreateOutcomeAnnotation(ctx, vm)
}

// resolveCreateMarkerForDelete decides the delete of a VM that carries the
// create marker and has no id (see the file comment). resolved reports that
// the VM's id was found (set on vm in memory) and the caller deletes it as a
// bound VM; otherwise res and err are the result to return (released, held,
// or an error).
func (r *VirtualMachineReconciler) resolveCreateMarkerForDelete(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	force bool,
) (res ctrl.Result, resolved bool, err error) {
	logger := log.FromContext(ctx)
	if vm.Annotations[createOutcomeAnnotation] != "" {
		if res, released, err := r.releaseConfirmedNotCreated(ctx, vm); released || err != nil {
			return res, false, err
		}
	}

	key := placementProviderKey(vm)
	provider := &infravirtrigaudiov1beta1.Provider{}
	if err := getForConsumer(ctx, r.Client, key, provider, vm.Namespace); err != nil {
		switch {
		case isConsumerNotAllowed(err):
			if res, retain := r.retainForUnroutableDelete(ctx, vm, force, err); retain {
				return res, false, nil
			}
			res, err := r.releaseLeavingHypervisorVM(ctx, vm, vmLeftNotRoutable)
			return res, false, err
		case apierrors.IsNotFound(err):
			// The Provider in the VM's own namespace is gone: there is nothing
			// to delete through, as for any VM.
			res, err := r.removeFinalizer(ctx, vm)
			return res, false, err
		default:
			logger.Error(err, "Failed to get provider for deletion")
			metrics.RecordError(errReasonDepsError, metrics.ComponentManager)
			return ctrl.Result{RequeueAfter: providerErrorRetryInterval}, false, nil
		}
	}

	if createLookupByOwner(provider) {
		if instance, ierr := r.getProviderInstance(ctx, provider); ierr != nil {
			logger.Info("Cannot reach the provider to look for the VM a requested Create made", "error", ierr.Error())
		} else {
			id, found, ok := findCreatedVMByOwner(ctx, instance, vm)
			switch {
			case ok && found:
				logger.Info("Found the VM a requested Create made by its owner stamp; deleting it", "id", id)
				vm.Status.ID = id
				return ctrl.Result{}, true, nil
			case ok && createConfirmByDomainName(provider) && createOutcomeSettled(vm, r.now()) && confirmDomainsAbsent(ctx, instance, vm):
				logger.Info("The provider confirms that the requested Create made no VM; releasing the finalizer",
					"attempt", markerAttemptID(vm))
				res, err := r.removeFinalizer(ctx, vm)
				if err == nil {
					r.recordEvent(vm, corev1.EventTypeNormal, eventReasonCreateConfirmedAbsent, fmt.Sprintf(
						"The provider has no VM for the Create requested at %s (none stamped with this VirtualMachine's "+
							"UID, and every domain name it could have used is absent): released without a provider delete",
						markerAttemptID(vm)))
				}
				return res, false, err
			}
		}
	}

	if force {
		res, err := r.releaseLeavingHypervisorVM(ctx, vm, vmLeftCreateOutcomeUnknown)
		return res, false, err
	}
	var since time.Time
	if vm.DeletionTimestamp != nil {
		since = vm.DeletionTimestamp.Time
	}
	return r.holdCreateOutcomeUnknown(ctx, vm, vm.Status.DeepCopy(), createOutcomeUnknownDeleteMessage, blockedRetryBackoff(since)), false, nil
}

// releaseConfirmedNotCreated releases a deleting VM whose
// createOutcomeAnnotation confirms that its marker's attempt made no VM —
// re-checked on the live object, with an audit Warning event — and reports
// whether it did. A value that confirms nothing is ignored and removed.
func (r *VirtualMachineReconciler) releaseConfirmedNotCreated(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	if !createOutcomeConfirmed(vm) {
		return ctrl.Result{}, false, r.ignoreCreateOutcomeAnnotation(ctx, vm)
	}
	live, gone, err := r.readLiveVM(ctx, vm)
	switch {
	case err != nil:
		return ctrl.Result{}, false, err
	case gone:
		return ctrl.Result{}, true, nil
	case !createOutcomeConfirmed(live):
		logger.Info("The operator's confirmation is no longer on the VirtualMachine; not releasing it")
		return ctrl.Result{RequeueAfter: providerErrorRetryInterval}, true, nil
	}
	attemptID := markerAttemptID(vm)
	logger.Info("An operator confirmed that the requested Create made no VM; releasing the finalizer",
		"annotation", createOutcomeAnnotation, "attempt", attemptID)
	res, err := r.removeFinalizer(ctx, vm)
	if err != nil {
		return res, true, err
	}
	r.recordEvent(vm, corev1.EventTypeWarning, eventReasonCreateOutcomeConfirmed, fmt.Sprintf(
		"%s=%s%s: an operator confirmed that the Create requested at %s made no VM; the VirtualMachine is released "+
			"without a provider delete", createOutcomeAnnotation, createOutcomeNotCreatedPrefix, attemptID, attemptID))
	return res, true, nil
}

// createOutcomeSettled reports whether the create marker of vm was set more
// than createOutcomeSettleTime before now, so no Create it records can still
// be running.
func createOutcomeSettled(vm *infravirtrigaudiov1beta1.VirtualMachine, now time.Time) bool {
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionProvisioning)
	return c != nil && now.Sub(c.LastTransitionTime.Time) > createOutcomeSettleTime
}

// findCreatedVMByOwner asks instance for the VM stamped with vm's UID (its
// ListVMs reports owner stamps, createLookupByOwner). ok is false when it
// could not be asked or answered ambiguously: more than one such VM, or any
// listed VM whose stamp the provider reports it cannot rely on
// (contracts.VMInfoOwnerStampStateKey present, whatever its value — that VM
// may be this one). Otherwise found reports whether one was listed, and id
// is its id. A listing may skip a VM it cannot read, so "none listed" alone
// never proves that no VM exists (confirmDomainsAbsent).
func findCreatedVMByOwner(
	ctx context.Context,
	instance contracts.Provider,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
) (id string, found, ok bool) {
	logger := log.FromContext(ctx)
	list, err := instance.ListVMs(ctx)
	if err != nil {
		logger.Info("Cannot list the provider's VMs to look for the VM a requested Create made", "error", err.Error())
		return "", false, false
	}
	var ids []string
	for _, info := range list.VMs {
		if _, unreliable := info.ProviderRaw[contracts.VMInfoOwnerStampStateKey]; unreliable {
			logger.Info("A listed VM's owner stamp cannot be relied on; not concluding", "id", info.ID,
				"state", info.ProviderRaw[contracts.VMInfoOwnerStampStateKey])
			return "", false, false
		}
		if uids := contracts.OwnerUIDs(info); len(uids) == 1 && uids[0] == string(vm.UID) {
			ids = append(ids, info.ID)
		}
	}
	switch len(ids) {
	case 0:
		return "", false, true
	case 1:
		return ids[0], true, true
	default:
		logger.Info("More than one VM is stamped with this VirtualMachine's UID; not choosing one", "ids", ids)
		return "", false, false
	}
}

// confirmDomainsAbsent reports whether instance gives a definite not-found
// for every libvirt domain name a Create for vm could have used
// (contracts.LibvirtCreateDomainNames): a Describe answered NotFound, or answered with
// exists=false. Any other answer — the domain exists, whoever it belongs to,
// or the provider could not tell — is not a confirmation.
func confirmDomainsAbsent(ctx context.Context, instance contracts.Provider, vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	logger := log.FromContext(ctx)
	for _, name := range contracts.LibvirtCreateDomainNames(vm.Namespace, vm.Name) {
		desc, err := instance.Describe(ctx, contracts.VMRef{ID: name})
		switch {
		case err == nil && !desc.Exists, contracts.IsNotFound(err):
			continue
		case err != nil:
			logger.Info("Cannot confirm that a domain the requested Create could have made is absent", "domain", name,
				"error", err.Error())
			return false
		default:
			logger.Info("A domain the requested Create could have made exists", "domain", name)
			return false
		}
	}
	return true
}

// createFailureReadyReasons are the Ready reasons a failed Create records.
var createFailureReadyReasons = map[string]bool{
	k8s.ReasonProviderError:    true,
	k8s.ReasonCreateBackoff:    true,
	k8s.ReasonCreateRefused:    true,
	k8s.ReasonValidationError:  true,
	k8s.ReasonProviderConflict: true,
}

// resendWait is how long a re-send of vm's requested Create must still wait
// at now (0: it is due). The re-send is due blockedRetryBackoff after the
// marker's LastTransitionTime (the last attempt), the backoff counted from
// when the failing streak began: Ready's LastTransitionTime while Ready is
// False for a create failure (createFailureReadyReasons), else the marker's.
// So the first re-send comes 15 s after the last attempt at the earliest,
// and while re-sends keep failing the same way each waits about as long as
// the streak has lasted (doubling, up to 5 minutes). A marker found long after
// its attempt (a manager restart) is re-sent at once.
func resendWait(vm *infravirtrigaudiov1beta1.VirtualMachine, now time.Time) time.Duration {
	marker := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionProvisioning)
	if marker == nil || marker.Reason != k8s.ReasonCreateRequested {
		return 0
	}
	last := marker.LastTransitionTime.Time
	since := last
	if ready := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady); ready != nil &&
		ready.Status == metav1.ConditionFalse && createFailureReadyReasons[ready.Reason] && ready.LastTransitionTime.Before(&marker.LastTransitionTime) {
		since = ready.LastTransitionTime.Time
	}
	backoff := min(max(now.Sub(since), blockedRetryMin), blockedRetryMax)
	return last.Add(backoff).Sub(now)
}

// recordConfirmedCreate records the audit Warning event of an operator's
// confirmation that attempt.markerID made no VM, once the create it authorizes
// is under way: requestCreate has written the new attempt's marker (and
// removed the annotation). A confirmation that is not acted on — the live
// object changed, the marker write failed — records nothing.
func (r *VirtualMachineReconciler) recordConfirmedCreate(vm *infravirtrigaudiov1beta1.VirtualMachine, attempt createAttempt) {
	if !attempt.confirmed {
		return
	}
	r.recordEvent(vm, corev1.EventTypeWarning, eventReasonCreateOutcomeConfirmed, fmt.Sprintf(
		"%s=%s%s: an operator confirmed that the Create requested at %s made no VM; the VM is created",
		createOutcomeAnnotation, createOutcomeNotCreatedPrefix, attempt.markerID, attempt.markerID))
}

// maxLoggedAnnotationBytes caps a tenant-written annotation value copied into
// the manager log (an annotation value may be up to 256 KiB).
const maxLoggedAnnotationBytes = 128

// truncateForLog returns s cut to at most maxBytes on a rune boundary, with
// an ellipsis when it was cut.
func truncateForLog(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
