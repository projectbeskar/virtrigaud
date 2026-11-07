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
	"encoding/json"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Power and Reconfigure of a VM of a Provider that requires operation
// identity (development builds only). They use the attempt lifecycle of
// virtualmachine_operation_identity.go unchanged: one attempt in flight per
// VM across every kind, an intent Secret before the marker, the same op_id
// and bytes on every re-send, and the same outcome, hold and backoff table.
//
// Order when nothing is in flight: a VM being deleted gets its Delete (the
// finalizer path never starts anything else); otherwise the reconcile decides,
// as for every Provider, Create, then the power state, then the size. A
// request that arrives while another attempt is in flight (for example a
// resize while a power-off runs) waits until that attempt is Terminal and is
// then decided from the VM's state at that time.

// repeatGap is the least time between a power or Reconfigure attempt that
// Succeeded and the next attempt of the same kind for the same VM; it doubles
// with each attempt of the kind that follows quickly (attemptRetryWait).
const repeatGap = blockedRetryMin

// repeatableKind reports whether kind may be asked for again after an
// attempt of it Succeeded: the power operations and Reconfigure. A Create or
// a Delete that Succeeded is never sent again.
func repeatableKind(kind contracts.OperationKind) bool {
	switch kind {
	case contracts.OperationKindPowerOn, contracts.OperationKindPowerOff, contracts.OperationKindReboot,
		contracts.OperationKindShutdownGraceful, contracts.OperationKindReconfigure:
		return true
	}
	return false
}

// noteResult records what this reconcile learned about the attempt it drove:
// the verified prepared operation and, after a send that was answered, the
// answer. A nil argument leaves that part as it is.
func (ia identityAttempt) noteResult(prepared *contracts.PreparedOperation, answer *contracts.OperationAnswer) {
	if ia.result == nil {
		return
	}
	if prepared != nil {
		ia.result.prepared = prepared
	}
	if answer != nil {
		ia.result.answer = answer
	}
}

// checkVMTarget checks what a stored Power or Reconfigure request targets:
// vm's recorded provider id, the attempt's host and vm as owner.
func checkVMTarget(vm *infravirtrigaudiov1beta1.VirtualMachine, a *infravirtrigaudiov1beta1.OperationAttempt,
	id, host, owner string) error {
	if vm.Status.ID == "" || id != vm.Status.ID || host != a.HostID || owner != string(vm.UID) {
		return errIntentTampered
	}
	return nil
}

// errNoRequestedResources is requestedResources' answer for a Reconfigure
// request that names no CPU and memory.
var errNoRequestedResources = errors.New("the Reconfigure request names no CPU and memory")

// requestedResources is the CPU and memory a Reconfigure request asks for:
// what its desired configuration (the JSON the client sends) names.
func requestedResources(req *providerv1.ReconfigureRequest) (cpu int32, memoryMiB int64, err error) {
	var desired contracts.CreateRequest
	if err := json.Unmarshal([]byte(req.GetDesiredJson()), &desired); err != nil {
		return 0, 0, fmt.Errorf("decode the desired configuration: %w", err)
	}
	if desired.Class.CPU <= 0 || desired.Class.MemoryMiB <= 0 {
		return 0, 0, errNoRequestedResources
	}
	return desired.Class.CPU, int64(desired.Class.MemoryMiB), nil
}

// reconfigureProjection is the desired configuration a signed Reconfigure
// carries: req without its cloud-init user data and metadata. No Provider's
// Reconfigure reads them (each reads the class's CPU, memory and disk
// defaults, and the disks), so they are left out of the request, and so out
// of the intent Secret and the grant.
func reconfigureProjection(req contracts.CreateRequest) contracts.CreateRequest {
	req.UserData, req.MetaData = nil, nil
	return req
}

// errReconfigureCarriesCloudInit is checkIntentRequest's answer for a stored
// Reconfigure that carries user data or metadata (reconfigureProjection never
// writes one).
var errReconfigureCarriesCloudInit = errors.New("the Reconfigure request carries cloud-init user data or metadata")

// checkReconfigureProjection refuses a Reconfigure request whose desired
// configuration carries user data or metadata.
func checkReconfigureProjection(req *providerv1.ReconfigureRequest) error {
	var desired contracts.CreateRequest
	if err := json.Unmarshal([]byte(req.GetDesiredJson()), &desired); err != nil {
		return fmt.Errorf("decode the desired configuration: %w", err)
	}
	if desired.UserData != nil || desired.MetaData != nil {
		return errReconfigureCarriesCloudInit
	}
	return nil
}

// requestedResourcesOf is requestedResources of a prepared Reconfigure.
func requestedResourcesOf(p *contracts.PreparedOperation) (int32, int64, error) {
	if p == nil || p.Kind != contracts.OperationKindReconfigure {
		return 0, 0, errNoRequestedResources
	}
	m, err := decodePrepared(p)
	if err != nil {
		return 0, 0, err
	}
	req, ok := m.(*providerv1.ReconfigureRequest)
	if !ok {
		return 0, 0, errNoRequestedResources
	}
	return requestedResources(req)
}

// gateDriveStep maps the step of the in-flight attempt a that the gate drove:
// a success records what follows the attempt's kind; anything else is
// returned as it is.
func (r *VirtualMachineReconciler) gateDriveStep(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt, a *infravirtrigaudiov1beta1.OperationAttempt, step attemptStep, res ctrl.Result, err error) (ctrl.Result, error) {
	if err != nil || step != stepSucceeded {
		return res, err
	}
	switch a.Kind {
	case infravirtrigaudiov1beta1.OperationAttemptKindCreate:
		return r.afterCreateSucceeded(ctx, vm, ia), nil
	case infravirtrigaudiov1beta1.OperationAttemptKindReconfigure:
		return r.afterReconfigureSucceeded(ctx, vm, ia, isClusterTopology(ia.providerCR)), nil
	case infravirtrigaudiov1beta1.OperationAttemptKindDelete:
		// A Delete is started only for a VM being deleted; the finalizer
		// path releases it.
		return ctrl.Result{Requeue: true}, nil
	}
	return r.afterPowerSucceeded(ctx, vm, a), nil
}

// powerKindFor is the operation kind of the power state desired asks for.
func powerKindFor(desired string) (contracts.OperationKind, bool) {
	switch infravirtrigaudiov1beta1.PowerState(desired) {
	case infravirtrigaudiov1beta1.PowerStateOn:
		return contracts.OperationKindPowerOn, true
	case infravirtrigaudiov1beta1.PowerStateOff:
		return contracts.OperationKindPowerOff, true
	case infravirtrigaudiov1beta1.PowerStateOffGraceful:
		return contracts.OperationKindShutdownGraceful, true
	}
	return "", false
}

// startIdentityPower is adjustPowerState for a VM of a Provider that requires
// operation identity: a new signed power attempt for the state desired asks
// for (On, Off, OffGraceful).
func (r *VirtualMachineReconciler) startIdentityPower(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerCR *infravirtrigaudiov1beta1.Provider, instance contracts.Provider, ref contracts.VMRef, desired string) (ctrl.Result, error) {
	kind, ok := powerKindFor(desired)
	if !ok {
		log.FromContext(ctx).Error(nil, "Unsupported power state", "state", desired)
		return ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
	}
	return r.startIdentityPowerOp(ctx, vm, providerCR, instance, ref, kind)
}

// startIdentityPowerOp starts a new signed power attempt of kind (PowerOn,
// PowerOff, Reboot or ShutdownGraceful) for the bound VM ref addresses. The
// request's PowerRequest.op is the one the kind fixes, and the grant names the
// kind, so a host refuses a request whose op does not match its grant.
// No VirtualMachine field asks for a Reboot today; the kind is signed and
// serialised like the others when a caller asks for one.
func (r *VirtualMachineReconciler) startIdentityPowerOp(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerCR *infravirtrigaudiov1beta1.Provider, instance contracts.Provider, ref contracts.VMRef,
	kind contracts.OperationKind) (ctrl.Result, error) {
	if !repeatableKind(kind) || kind == contracts.OperationKindReconfigure {
		return ctrl.Result{}, fmt.Errorf("start a power attempt of kind %q: %w", kind, contracts.ErrOperationIdentityNotImplemented)
	}
	ia, res, ok := r.identityAttemptFor(ctx, vm, providerCR, instance)
	if !ok {
		return res, nil
	}
	if !ref.Routed() || vm.Status.ID == "" {
		return r.holdOperation(ctx, vm, k8s.ReasonOperationStateLost, msgOperationStateLost, operationHoldRecheckInterval), nil
	}
	step, res, err := r.startAttempt(ctx, vm, ia, contracts.OperationRequest{Kind: kind, VM: ref}, ref.HostID)
	if err == nil && step == stepSucceeded {
		a := findAttempt(vm, kind)
		if a == nil {
			return ctrl.Result{RequeueAfter: createdDescribeFallback}, nil
		}
		return r.afterPowerSucceeded(ctx, vm, a), nil
	}
	return res, err
}

// afterPowerSucceeded records a power attempt that Succeeded: the VM is
// described again shortly, and only that Describe makes it Ready.
func (r *VirtualMachineReconciler) afterPowerSucceeded(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	a *infravirtrigaudiov1beta1.OperationAttempt) ctrl.Result {
	k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonOperationInProgress,
		fmt.Sprintf("%s attempt %d Succeeded; the VM is described next", a.Kind, a.Attempt), vm.Generation)
	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: createdDescribeFallback}
}

// startIdentityReconfigure is reconfigureVM's send for a VM of a Provider that
// requires operation identity: a new signed Reconfigure attempt with req (the
// desired configuration reconfigureVM built, disk growth over the limit
// already withheld). The grant covers the exact request bytes, so the CPU and
// memory the attempt asks for are bound to it, and they, not the spec at the
// time the attempt ends, are what is recorded once it Succeeded.
func (r *VirtualMachineReconciler) startIdentityReconfigure(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerCR *infravirtrigaudiov1beta1.Provider, instance contracts.Provider, ref contracts.VMRef,
	vmClass *infravirtrigaudiov1beta1.VMClass, req contracts.CreateRequest) (ctrl.Result, error) {
	ia, res, ok := r.identityAttemptFor(ctx, vm, providerCR, instance)
	if !ok {
		return res, nil
	}
	ia.vmClass = vmClass
	if !ref.Routed() || vm.Status.ID == "" {
		return r.holdOperation(ctx, vm, k8s.ReasonOperationStateLost, msgOperationStateLost, operationHoldRecheckInterval), nil
	}
	step, res, err := r.startAttempt(ctx, vm, ia,
		contracts.OperationRequest{Kind: contracts.OperationKindReconfigure, VM: ref, Spec: reconfigureProjection(req)}, ref.HostID)
	if err == nil && step == stepSucceeded {
		return r.afterReconfigureSucceeded(ctx, vm, ia, ref.Routed()), nil
	}
	return res, err
}

// afterReconfigureSucceeded records a Reconfigure attempt that Succeeded,
// with the CPU and memory its stored request asked for:
//   - answered synchronously without the honest-result marker on a clustered
//     Provider: counted at the larger of the recorded and the requested size,
//     as the unsigned path does (recordUnmarkedResultAt);
//   - answered synchronously as applied to the persistent definition only:
//     recorded as pending a restart (recordRestartPendingAt);
//   - otherwise (a synchronous success, or a task that Succeeded):
//     status.currentResources set to the requested size.
func (r *VirtualMachineReconciler) afterReconfigureSucceeded(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt, routed bool) ctrl.Result {
	r.recordReconfigureSuccess(vm)
	now := metav1.NewTime(r.now())
	vm.Status.LastReconfigureTime = &now
	var prepared *contracts.PreparedOperation
	var answer *contracts.OperationAnswer
	if ia.result != nil {
		prepared, answer = ia.result.prepared, ia.result.answer
	}
	cpu, mem, err := requestedResourcesOf(prepared)
	if err != nil {
		// Unexpected: the request was checked when it was loaded. Nothing is
		// recorded from a request that cannot be read.
		log.FromContext(ctx).Error(err, "Cannot read the size a Reconfigure that Succeeded asked for; status.currentResources is left as it is")
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: createdDescribeFallback}
	}
	switch {
	case answer != nil && routed && !answer.Honest:
		r.recordUnmarkedResultAt(vm, cpu, mem)
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: restartPendingRecheckInterval}
	case answer != nil && answer.RestartRequired:
		r.recordRestartPendingAt(vm, cpu, mem)
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: restartPendingRecheckInterval}
	}
	setCurrentResources(vm, cpu, mem)
	k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonReconcileSuccess, "VM reconfigured successfully",
		vm.Generation)
	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: createdDescribeFallback}
}

// setCurrentResources records cpu and memoryMiB as status.currentResources.
func setCurrentResources(vm *infravirtrigaudiov1beta1.VirtualMachine, cpu int32, memoryMiB int64) {
	if vm.Status.CurrentResources == nil {
		vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{}
	}
	vm.Status.CurrentResources.CPU = &cpu
	vm.Status.CurrentResources.MemoryMiB = &memoryMiB
}

// decodePrepared decodes p's request bytes as its kind's request message.
func decodePrepared(p *contracts.PreparedOperation) (proto.Message, error) {
	wire, ok := wireKinds[p.Kind]
	if !ok {
		return nil, fmt.Errorf("operation kind %q: %w", p.Kind, contracts.ErrOperationIdentityNotImplemented)
	}
	m, err := operation.DecodeRequest(wire, p.RequestBytes)
	if err != nil {
		return nil, fmt.Errorf("decode the %s request: %w", p.Kind, err)
	}
	return m, nil
}

// The Delete of a VM of a Provider that requires operation identity, and an
// answer that finds no VM. For every other Provider a Describe or a Delete
// that finds no VM releases the finalizer (virtualmachine_delete_task.go,
// handleDeletion); here it never does. "No such VM" is an unsigned claim that
// a misrouted, restored or lying host can make about a VM that still runs, so
// it does not confirm the Delete, and neither does UNKNOWN_TO_AGENT (no
// record of the task proves nothing). The finalizer is released only by:
//   - a confirmed Delete: a bound task of this Delete attempt Succeeded, or a
//     re-send of the same op_id was answered with success (releaseAfterDelete,
//     confirmDeleteRelease);
//   - no Create ever performed (nothingCreated);
//   - orphan-on-delete or force-delete;
//   - later, an authenticated host answer that the VM is deleted
//     (authenticatedHostDeleted), which does not exist yet.

// msgOperationDeleteUnconfirmed is the hold of a Delete whose VM is reported
// absent, or whose Provider is gone.
var msgOperationDeleteUnconfirmed = "the VM is being deleted, and its host reports no such VM (or its Provider is gone); " +
	"for a Provider that requires operation identity that does not confirm a Delete, so the finalizer is kept; " + operationHoldRecovery

// authenticatedHostDeleted is the extension point for an authenticated answer
// from the VM's host that the VM is deleted: bound to the VM's UID and its
// placement binding, and signed by (or otherwise bound to) the host's
// enrolment, for example a host-signed tombstone. Such an answer would let
// the finalizer of a VM whose Delete is unconfirmed be released. No wire
// shape for it exists yet, so this always answers false and nothing is
// released on an absence.
func (r *VirtualMachineReconciler) authenticatedHostDeleted(context.Context, *infravirtrigaudiov1beta1.VirtualMachine,
	identityAttempt, *infravirtrigaudiov1beta1.OperationAttempt) bool {
	return false
}

// deleteAbsent handles a Delete attempt a whose send the host answered with
// "no such VM": the VM is held (OperationDeleteUnconfirmed), the attempt stays
// as it is and is re-sent with the same op_id when the hold is re-checked.
// Only authenticatedHostDeleted (not implemented) could release it.
func (r *VirtualMachineReconciler) deleteAbsent(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt, a *infravirtrigaudiov1beta1.OperationAttempt) (attemptStep, ctrl.Result, error) {
	if r.authenticatedHostDeleted(ctx, vm, ia, a) {
		return r.closeAttempt(ctx, vm, ia, a, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded, "", "")
	}
	return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationDeleteUnconfirmed, msgOperationDeleteUnconfirmed,
		operationHoldRecheckInterval), nil
}

// identityDeleteUnconfirmed reports whether vm records operation attempts
// (only a Provider that requires operation identity has them) of which a
// Create was, or may have been, performed: its finalizer is not released
// because its Provider is gone (handleDeletion), since nothing can confirm
// its Delete.
func identityDeleteUnconfirmed(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	return len(vm.Status.OperationAttempts) > 0 && !nothingCreated(vm)
}

// reconfiguredResources is status.currentResources after the Reconfigure
// attempt this reconcile drove Succeeded: the size its stored request asked
// for, or, for a synchronous answer applied only to the persistent definition
// or without the honest-result marker on a clustered Provider, the larger of
// that and the recorded size per resource. nil when the request cannot be
// read (unexpected: it was checked when it was loaded).
func (r *VirtualMachineReconciler) reconfiguredResources(vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	routed bool) *infravirtrigaudiov1beta1.VirtualMachineResources {
	if ia.result == nil {
		return nil
	}
	cpu, mem, err := requestedResourcesOf(ia.result.prepared)
	if err != nil {
		return nil
	}
	if answer := ia.result.answer; answer != nil && (answer.RestartRequired || (routed && !answer.Honest)) {
		cpu, mem = max(r.getCurrentCPU(vm), cpu), max(r.getCurrentMemoryMiB(vm), mem)
	}
	return &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}
}

// A Delete supersedes a held power or Reconfigure attempt. One attempt in
// flight per VM is the rule, but a power or Reconfigure attempt whose outcome
// cannot be learned (the host has no record of its task, cannot tell what it
// did, refuses its re-delivery, or its intent or task binding is lost or
// fails its checks) would otherwise block the VM's Delete for ever, leaving
// only orphan-on-delete or force-delete, which leave the VM on its host.
// Whatever such an attempt did or still does, a Delete that is confirmed
// removes the VM, so for a VM being deleted the held attempt is ended as
// Superseded (its outcome stays unknown, its intent Secret is kept as
// evidence while the VM exists, and a Warning event records it) and the
// signed Delete follows. Nothing else supersedes a held attempt:
//   - a held or in-flight Create is never superseded: the Delete needs the
//     VM's id, and a Create that may still run could create the VM after the
//     Delete;
//   - a PowerOff (or graceful shutdown) never supersedes a held PowerOn (or
//     Reboot): the held operation could still run on the host after the
//     PowerOff and leave the VM on, while the operator reports it off.

// supersedableHoldReasons are the holds of an attempt in flight whose outcome
// the controller cannot learn.
var supersedableHoldReasons = map[string]bool{
	k8s.ReasonOperationUnknownToAgent: true,
	k8s.ReasonOperationOutcomeUnknown: true,
	k8s.ReasonOperationIntentLost:     true,
	k8s.ReasonOperationIntentTampered: true,
	k8s.ReasonOperationStateLost:      true,
}

// supersedableByDelete reports whether the held attempt a in flight may be
// superseded by vm's Delete: a power or Reconfigure attempt (never a Create
// or a Delete) held for a reason that the controller cannot resolve.
func supersedableByDelete(vm *infravirtrigaudiov1beta1.VirtualMachine, a *infravirtrigaudiov1beta1.OperationAttempt) bool {
	if !repeatableKind(contracts.OperationKind(a.Kind)) {
		return false
	}
	c := k8s.GetCondition(vm.Status.Conditions, k8s.ConditionReady)
	return c != nil && c.Status == metav1.ConditionFalse && supersedableHoldReasons[c.Reason]
}

// eventReasonOperationSuperseded is the Warning event of a superseded
// attempt.
const eventReasonOperationSuperseded = "OperationSuperseded"

// supersedeForDelete ends the held attempt a of vm, being deleted, as
// Terminal/Superseded, keeps its intent Secret, records a Warning event
// (names and numbers only), and requeues: the next reconcile sends the
// signed Delete.
func (r *VirtualMachineReconciler) supersedeForDelete(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	a *infravirtrigaudiov1beta1.OperationAttempt) (ctrl.Result, error) {
	done := *a
	done.Phase, done.Outcome = infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, infravirtrigaudiov1beta1.OperationAttemptOutcomeSuperseded
	done.OutcomeReason, done.LastTransitionTime = "", metav1.NewTime(r.now())
	if err := r.writeIdentityStatus(ctx, vm, identityStatusWrite{attempts: withAttempt(vm, done)}); err != nil {
		log.FromContext(ctx).Info("The superseded marker was not written; retrying", "error", err.Error())
		return ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
	}
	log.FromContext(ctx).Info("A held attempt was superseded by the VM's Delete", "kind", a.Kind, "attempt", a.Attempt, "opID", a.OpID)
	r.recordEvent(vm, corev1.EventTypeWarning, eventReasonOperationSuperseded, fmt.Sprintf(
		"%s attempt %d (op_id %s) was held, and its outcome cannot be learned; it is superseded by the VM's Delete. What it did on the "+
			"host is unknown; its intent Secret %s is kept in the Provider's namespace as evidence while the VM exists",
		a.Kind, a.Attempt, a.OpID, intentSecretName(a.OpID)))
	return ctrl.Result{Requeue: true}, nil
}

// intentKeptAsEvidence reports whether the intent Secret of the Terminal
// attempt a is kept while vm exists: a Delete awaiting its release, or an
// attempt a Delete superseded.
func intentKeptAsEvidence(vm *infravirtrigaudiov1beta1.VirtualMachine, a *infravirtrigaudiov1beta1.OperationAttempt) bool {
	return deleteAwaitingRelease(vm, a) || a.Outcome == infravirtrigaudiov1beta1.OperationAttemptOutcomeSuperseded
}

// acknowledgeOperationAnnotation, set on a VirtualMachine to the op_id of its
// power or Reconfigure attempt that ended FailedDirty, acknowledges that an
// operator checked the VM: the hold on that kind ends and, after the next
// Describe, a new attempt of it may follow. It names one attempt exactly:
// any other value (another op_id, a Create's or a Delete's) does nothing. The
// manager removes it once consumed and records an event.
const acknowledgeOperationAnnotation = "virtrigaud.io/acknowledge-operation"

// eventReasonOperationAcknowledged is the event of a consumed acknowledgement.
const eventReasonOperationAcknowledged = "OperationAcknowledged"

// consumeAcknowledgement reports whether vm carries an acknowledgement of the
// FailedDirty attempt prev (a power or Reconfigure attempt, whose op_id the
// annotation names exactly) and, if so, removes the annotation and records an
// event. A Create or Delete attempt is never acknowledged this way.
func (r *VirtualMachineReconciler) consumeAcknowledgement(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	prev *infravirtrigaudiov1beta1.OperationAttempt) (bool, error) {
	if !repeatableKind(contracts.OperationKind(prev.Kind)) || prev.OpID == "" ||
		vm.Annotations[acknowledgeOperationAnnotation] != prev.OpID {
		return false, nil
	}
	patched := vm.DeepCopy()
	delete(patched.Annotations, acknowledgeOperationAnnotation)
	if err := r.Patch(ctx, patched, client.MergeFromWithOptions(vm, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, fmt.Errorf("remove the operation acknowledgement of VirtualMachine %s/%s: %w", vm.Namespace, vm.Name, err)
	}
	vm.Annotations, vm.ResourceVersion = patched.Annotations, patched.ResourceVersion
	log.FromContext(ctx).Info("A FailedDirty attempt was acknowledged", "kind", prev.Kind, "attempt", prev.Attempt, "opID", prev.OpID)
	r.recordEvent(vm, corev1.EventTypeNormal, eventReasonOperationAcknowledged, fmt.Sprintf(
		"The FailedDirty hold of %s attempt %d (op_id %s) was acknowledged with %s; a new attempt of that kind may follow",
		prev.Kind, prev.Attempt, prev.OpID, acknowledgeOperationAnnotation))
	return true, nil
}
