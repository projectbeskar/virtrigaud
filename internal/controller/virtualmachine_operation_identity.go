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
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Operation identity on the VirtualMachine controller (development builds
// only; ADR-0016 Part A, manager half). For a Provider whose spec.type
// requires operation identity (operation.RequiredProtection), the Create,
// Delete, Power (on, off, reboot, graceful shutdown) and Reconfigure of a VM
// are sent as signed operations, each in this order:
//
//  1. decide: a new attempt only when no attempt of any kind is in flight for
//     the VM and the latest attempt of the kind ended not performed (after a
//     backoff), or there is none; its op_id and digest salt are drawn once;
//  2. persist: the exact request bytes and salt in an intent Secret
//     (operation_intent.go), then the attempt marker in phase Prepared on
//     status.operationAttempts, with JSON Patch test preconditions on the
//     markers (and placement, and id) this reconcile read;
//  3. transmit: the stored bytes under a freshly minted grant; a re-send of
//     the attempt (after a transport error, a lost reply or a restart) sends
//     the same bytes with the same op_id;
//  4. close: Sent with the task reference, then Terminal with the typed
//     outcome, then the intent Secret is deleted.
//
// At most one attempt of any kind is in flight per VM: it is driven to its
// end before anything else is decided (reconcileIdentityGate, identityDelete).
// The other operation kinds of the provider contract have no signed path and
// are refused locally (virtualmachine_operation_identity_kinds.go).
//
// Every other Provider never enters this file: no marker, no intent Secret,
// no envelope, and its capabilities 31-34 are not read here.

// operationCrashPoint names a point of the attempt lifecycle at which a test
// can stop the reconcile as if the manager died there.
type operationCrashPoint string

// The crash points of the attempt lifecycle (tests only).
const (
	// crashAfterPrepared: the marker is Prepared, nothing sent.
	crashAfterPrepared operationCrashPoint = "after-prepared"
	// crashAfterSend: the operation was sent, its answer not recorded.
	crashAfterSend operationCrashPoint = "after-send"
	// crashAfterReply: a signed outcome was read, the marker not Terminal.
	crashAfterReply operationCrashPoint = "after-reply"
	// crashAfterTerminal: the marker is Terminal, the intent Secret not
	// deleted.
	crashAfterTerminal operationCrashPoint = "after-terminal"
)

// errOperationCrashed is what a reconcile stopped at a crash point returns.
var errOperationCrashed = errors.New("stopped at an injected crash point")

// Requeue intervals of operation identity.
const (
	// operationPollInterval is how often an operation in flight is polled.
	operationPollInterval = 5 * time.Second
	// operationHoldRecheckInterval is how often a held attempt is asked
	// about (or re-sent with the same op_id) again.
	operationHoldRecheckInterval = blockedRetryMax
)

// operationHoldRecovery ends every operation-identity hold message.
var operationHoldRecovery = fmt.Sprintf("no new attempt is sent; to release the VirtualMachine after checking its host by hand, "+
	"set %s=true (or %s=true) and delete it (docs/operation-grants-manager.md)",
	infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation, forceDeleteAnnotation)

// Messages of operation-identity conditions. None of them carries request
// bytes, a salt or an identifier beyond reason codes.
var (
	msgOperationSignerUnavailable = "the Provider requires operation identity and this manager has no operation signer " +
		"(a release build has none; a development build needs --" + operation.DevSignerKeyFileFlagName + "): nothing is sent to it"
	msgOperationStateLost = "the VM records a binding or a provider id but no attempt marker of its Create " +
		"(status.operationAttempts was edited or restored); " + operationHoldRecovery
	msgOperationIntentLost = "an attempt in flight names an operation-intent Secret that does not exist " +
		"(for example after an etcd restore), so it cannot be re-sent with its exact request; " + operationHoldRecovery
	msgOperationFailedDirty    = "the Provider reported that the operation failed after an irreversible step began; " + operationHoldRecovery
	msgOperationUnknownToAgent = "the host has no record of the operation's task, which proves nothing about the operation; " +
		operationHoldRecovery
	msgOperationIntentTampered = "the stored intent of an attempt in flight failed its authenticity or consistency checks; " +
		"nothing is sent and the Secret is kept as evidence; " + operationHoldRecovery
	msgOperationRequestTooLarge = "the operation's request is larger than an operation-intent Secret may hold " +
		"(reduce the cloud-init user data); nothing is sent"
	msgOperationProviderMismatch = "the host refused the operation as coming from another Provider than the one it is enrolled with; " +
		"the same attempt is re-sent periodically; " + operationHoldRecovery
)

// identityRequired reports whether the manager requires operation identity
// for provider's mutating calls: from its immutable spec.type only.
func identityRequired(provider *infravirtrigaudiov1beta1.Provider) bool {
	return provider != nil && operation.RequiredProtection(provider.Spec.Type) == operation.ProtectionIdentity
}

// operationHintMismatch checks what a Provider advertises (capabilities
// 31-34, unauthenticated hints) against the protection required for it. It
// returns "" when they match, or why they do not. A Provider that requires
// operation identity must advertise supports_ and requires_operation_identity;
// one that requires none must advertise neither; any fencing gate is a
// mismatch (fencing is not implemented). The required protection is never
// lowered to match.
func operationHintMismatch(required operation.Protection, caps contracts.Capabilities) string {
	switch {
	case caps.SupportsFencing || caps.RequiresFencing:
		return "the Provider advertises fencing, which this manager does not implement"
	case required == operation.ProtectionIdentity && (!caps.SupportsOperationIdentity || !caps.RequiresOperationIdentity):
		return "the Provider's type requires operation identity, but the Provider does not advertise that it supports and requires it"
	case required == operation.ProtectionNone && (caps.SupportsOperationIdentity || caps.RequiresOperationIdentity):
		return "the Provider advertises operation identity, which its type does not use"
	}
	return ""
}

// attemptKindOf is the CRD kind of a contracts operation kind.
func attemptKindOf(kind contracts.OperationKind) infravirtrigaudiov1beta1.OperationAttemptKind {
	return infravirtrigaudiov1beta1.OperationAttemptKind(kind)
}

// findAttempt returns a copy of vm's attempt marker of kind, or nil.
func findAttempt(vm *infravirtrigaudiov1beta1.VirtualMachine, kind contracts.OperationKind) *infravirtrigaudiov1beta1.OperationAttempt {
	for i := range vm.Status.OperationAttempts {
		if vm.Status.OperationAttempts[i].Kind == attemptKindOf(kind) {
			a := vm.Status.OperationAttempts[i]
			return &a
		}
	}
	return nil
}

// inFlightAttempt returns a copy of vm's non-terminal attempt marker, or nil.
// There is at most one: a new attempt is written only when none is in flight.
func inFlightAttempt(vm *infravirtrigaudiov1beta1.VirtualMachine) *infravirtrigaudiov1beta1.OperationAttempt {
	for i := range vm.Status.OperationAttempts {
		if vm.Status.OperationAttempts[i].Phase != infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal {
			a := vm.Status.OperationAttempts[i]
			return &a
		}
	}
	return nil
}

// notPerformed reports whether outcome says the attempt was definitely not
// performed, so a new attempt may follow (after a backoff).
func notPerformed(outcome infravirtrigaudiov1beta1.OperationAttemptOutcome) bool {
	switch outcome {
	case infravirtrigaudiov1beta1.OperationAttemptOutcomeRetired, infravirtrigaudiov1beta1.OperationAttemptOutcomeFailedClean,
		infravirtrigaudiov1beta1.OperationAttemptOutcomeCancelled, infravirtrigaudiov1beta1.OperationAttemptOutcomeRejected:
		return true
	}
	return false
}

// bindingIDOf is vm's placement binding id, or "".
func bindingIDOf(vm *infravirtrigaudiov1beta1.VirtualMachine) string {
	if vm.Status.Placement == nil {
		return ""
	}
	return vm.Status.Placement.BindingID
}

// stateLost reports whether vm records a binding or an id but no Create
// marker: its status was edited or restored, and a new attempt must not be
// minted in the gap.
//
// It also reports a VM that records a provider id, a confirmed host or a
// Bound placement while its Create marker is not Succeeded: the operator
// records those only in the write that makes the Create Succeeded, so the
// status was edited (for example a Succeeded marker rewritten as not
// performed, which would otherwise start a new Create).
func stateLost(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	c := findAttempt(vm, contracts.OperationKindCreate)
	if c == nil {
		return vm.Status.ID != "" || bindingIDOf(vm) != "" || boundHost(vm) != ""
	}
	if c.Outcome == infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded {
		return false
	}
	placed := k8s.GetCondition(vm.Status.Conditions, k8s.ConditionPlaced)
	return vm.Status.ID != "" || boundHost(vm) != "" ||
		(placed != nil && placed.Status == metav1.ConditionTrue && placed.Reason == k8s.ReasonBound)
}

// deleteAwaitingRelease reports whether a is a Delete recorded Succeeded
// whose VM still holds its finalizer: its intent Secret is kept until the
// finalizer is released, as the evidence the release is checked against.
func deleteAwaitingRelease(vm *infravirtrigaudiov1beta1.VirtualMachine, a *infravirtrigaudiov1beta1.OperationAttempt) bool {
	return a.Kind == infravirtrigaudiov1beta1.OperationAttemptKindDelete && a.Phase == infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal &&
		a.Outcome == infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded &&
		controllerutilContains(vm.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
}

// controllerutilContains reports whether list holds v.
func controllerutilContains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// attemptRetryWait is how long after a not-performed attempt a's terminal
// transition the next attempt waits: blockedRetryMin doubling per attempt,
// at most blockedRetryMax.
//
// A lastTransitionTime in the future (an edited status) counts as now, so the
// wait never exceeds blockedRetryMax.
func attemptRetryWait(a *infravirtrigaudiov1beta1.OperationAttempt, now time.Time) time.Duration {
	wait := blockedRetryMin
	for i := int64(1); i < a.Attempt && wait < blockedRetryMax; i++ {
		wait *= 2
	}
	wait = min(wait, blockedRetryMax)
	since := now.Sub(a.LastTransitionTime.Time)
	if since < 0 {
		since = 0
	}
	return min(max(wait-since, 0), blockedRetryMax)
}

// identityStatusWrite is one write of operation-identity status fields:
// the new markers, and optionally the new placement and id. It is sent as a
// JSON Patch on the status subresource whose test operations require the
// markers, placement and id to be as this reconcile read them.
type identityStatusWrite struct {
	attempts     []infravirtrigaudiov1beta1.OperationAttempt
	setPlacement bool
	placement    *infravirtrigaudiov1beta1.PlacementStatus
	setID        bool
	id           string
	// resources, when set, replaces status.currentResources (a Reconfigure
	// that Succeeded records its size in the write that makes it Terminal).
	resources *infravirtrigaudiov1beta1.VirtualMachineResources
}

// rawJSON encodes v; an empty marker list encodes as null (it is not stored).
func rawJSON(v any) (json.RawMessage, error) {
	if a, ok := v.([]infravirtrigaudiov1beta1.OperationAttempt); ok && len(a) == 0 {
		return json.RawMessage("null"), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode status patch value: %w", err)
	}
	return b, nil
}

// writeIdentityStatus applies w to vm through the status subresource, with
// test preconditions on what vm holds. On success vm carries the written
// fields and the new resourceVersion; on any error nothing of w is in vm,
// and the caller must not act as if it were written.
func (r *VirtualMachineReconciler) writeIdentityStatus(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	w identityStatusWrite) error {
	var ops []jsonPatchOp
	add := func(op, path string, v any) error {
		raw, err := rawJSON(v)
		if err != nil {
			return err
		}
		ops = append(ops, jsonPatchOp{Op: op, Path: path, Value: raw})
		return nil
	}
	if err := add("test", "/status/operationAttempts", vm.Status.OperationAttempts); err != nil {
		return err
	}
	if w.setPlacement {
		if err := add("test", "/status/placement", vm.Status.Placement); err != nil {
			return err
		}
	}
	if w.setID {
		var id any
		if vm.Status.ID != "" {
			id = vm.Status.ID
		}
		if err := add("test", "/status/id", id); err != nil {
			return err
		}
	}
	if err := add("add", "/status/operationAttempts", w.attempts); err != nil {
		return err
	}
	if w.setPlacement {
		if err := add("add", "/status/placement", w.placement); err != nil {
			return err
		}
	}
	if w.setID {
		if err := add("add", "/status/id", w.id); err != nil {
			return err
		}
	}
	if w.resources != nil {
		if err := add("add", "/status/currentResources", w.resources); err != nil {
			return err
		}
	}
	body, err := json.Marshal(ops)
	if err != nil {
		return fmt.Errorf("encode status patch: %w", err)
	}
	obj := vm.DeepCopy()
	if err := r.Status().Patch(ctx, obj, client.RawPatch(types.JSONPatchType, body)); err != nil {
		return fmt.Errorf("write the operation attempt of VirtualMachine %s/%s: %w", vm.Namespace, vm.Name, err)
	}
	vm.ResourceVersion = obj.ResourceVersion
	vm.Status.OperationAttempts = obj.Status.OperationAttempts
	vm.Status.Placement = obj.Status.Placement
	vm.Status.ID = obj.Status.ID
	vm.Status.CurrentResources = obj.Status.CurrentResources
	return nil
}

// withAttempt is vm's markers with a replacing the marker of its kind.
func withAttempt(vm *infravirtrigaudiov1beta1.VirtualMachine, a infravirtrigaudiov1beta1.OperationAttempt) []infravirtrigaudiov1beta1.OperationAttempt {
	out := make([]infravirtrigaudiov1beta1.OperationAttempt, 0, len(vm.Status.OperationAttempts)+1)
	replaced := false
	for _, cur := range vm.Status.OperationAttempts {
		if cur.Kind == a.Kind {
			out = append(out, a)
			replaced = true
			continue
		}
		out = append(out, cur)
	}
	if !replaced {
		out = append(out, a)
	}
	return out
}

// markerNotStored reports whether the VM as the API server has it now
// certainly carries no attempt marker of op_id opID (after a marker write
// failed, whatever the error). Any doubt (the read failed) answers false.
func (r *VirtualMachineReconciler) markerNotStored(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, opID string) bool {
	live, gone, err := r.readLiveVM(ctx, vm)
	if err != nil {
		return false
	}
	if gone {
		return true
	}
	for _, a := range live.Status.OperationAttempts {
		if a.OpID == opID {
			return false
		}
	}
	return true
}

// operationCrashed reports whether a test stops the reconcile at point (as if
// the manager died there). Production reconcilers have no hook.
func (r *VirtualMachineReconciler) operationCrashed(ctx context.Context, point operationCrashPoint) bool {
	return r.operationCrashHook != nil && r.operationCrashHook(ctx, point)
}

// holdOperation records Ready=False with reason and msg (and, for a VM not
// being deleted, Provisioning=False with the same), writes the status, and
// returns a requeue after.
func (r *VirtualMachineReconciler) holdOperation(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	reason, msg string, after time.Duration) ctrl.Result {
	log.FromContext(ctx).Info("Operation-identity hold", "reason", reason)
	k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, reason, msg, vm.Generation)
	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: after}
}

// operationInProgress records that an operation of a is in flight.
func (r *VirtualMachineReconciler) operationInProgress(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	a *infravirtrigaudiov1beta1.OperationAttempt, after time.Duration) ctrl.Result {
	msg := fmt.Sprintf("%s attempt %d is in flight (%s)", a.Kind, a.Attempt, a.Phase)
	k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonOperationInProgress, msg, vm.Generation)
	if vm.DeletionTimestamp.IsZero() {
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonOperationInProgress, msg, vm.Generation)
	}
	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: after}
}

// attemptStep is the outcome of one step of an attempt.
type attemptStep int

const (
	// stepWaiting: the attempt is in flight, held, or waiting for a write; res
	// says when to look again.
	stepWaiting attemptStep = iota
	// stepSucceeded: the attempt is Terminal and Succeeded.
	stepSucceeded
	// stepNotPerformed: the attempt is Terminal and was not performed; a new
	// one follows after its backoff.
	stepNotPerformed
	// stepHeld: the attempt is held for an operator.
	stepHeld
)

// identityAttempt carries what one reconcile needs to drive an attempt.
type identityAttempt struct {
	providerCR *infravirtrigaudiov1beta1.Provider
	instance   contracts.Provider
	sender     contracts.OperationSender
	// reader asks hosts for signed answers about the attempt.
	reader  contracts.HostAnswerReader
	vmClass *infravirtrigaudiov1beta1.VMClass
	// deleteRef is the VM reference a Delete attempt must name (set by
	// identityDelete); a stored Delete intent naming another is tampered.
	deleteRef *contracts.VMRef
	// result is what this reconcile learned about the attempt it drove
	// (shared by the copies of the identityAttempt).
	result *attemptResult
}

// attemptResult is what one reconcile learned about the attempt it drove: the
// verified prepared operation, and the Provider's answer when the outcome came
// from a send (nil when it came from a task).
type attemptResult struct {
	prepared *contracts.PreparedOperation
	answer   *contracts.OperationAnswer
}

// identityAttemptFor checks the Provider's client and advertised capabilities
// for an operation-identity reconcile of vm. ok is false when vm was held (or
// must wait), with res to return.
func (r *VirtualMachineReconciler) identityAttemptFor(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerCR *infravirtrigaudiov1beta1.Provider, instance contracts.Provider) (identityAttempt, ctrl.Result, bool) {
	sender, ok := instance.(contracts.OperationSender)
	if !ok || r.OperationIntentAuth == nil {
		return identityAttempt{}, r.holdOperation(ctx, vm, k8s.ReasonOperationSignerUnavailable, msgOperationSignerUnavailable,
			operationHoldRecheckInterval), false
	}
	reporter, ok := instance.(contracts.CapabilityReporter)
	if !ok {
		return identityAttempt{}, r.holdOperation(ctx, vm, k8s.ReasonOperationIdentityMismatch,
			"the Provider's client reports no capabilities, so its advertised operation identity cannot be checked; nothing is sent",
			operationHoldRecheckInterval), false
	}
	caps, err := reporter.GetCapabilities(ctx)
	if err != nil {
		log.FromContext(ctx).Info("Cannot read the Provider's capabilities; nothing is sent until they are read", "error", err.Error())
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError,
			"Cannot read the provider's capabilities: "+providerFailureDetail(err), vm.Generation)
		r.updateStatus(ctx, vm)
		return identityAttempt{}, ctrl.Result{RequeueAfter: providerErrorRetryInterval}, false
	}
	if why := operationHintMismatch(operation.ProtectionIdentity, caps); why != "" {
		return identityAttempt{}, r.holdOperation(ctx, vm, k8s.ReasonOperationIdentityMismatch,
			why+"; the manager never lowers the protection it requires: nothing is sent until the Provider advertises a match",
			operationHoldRecheckInterval), false
	}
	reader, ok := instance.(contracts.HostAnswerReader)
	if !ok || r.HostAnswers == nil {
		return identityAttempt{}, r.holdOperation(ctx, vm, k8s.ReasonOperationHostAnswerUnavailable, msgHostAnswersUnavailable,
			operationHoldRecheckInterval), false
	}
	return identityAttempt{providerCR: providerCR, instance: instance, sender: sender, reader: reader, result: &attemptResult{}},
		ctrl.Result{}, true
}

// reconcileIdentityGate runs, for a VM of a Provider that requires operation
// identity, before anything else is sent for it: it checks the Provider's
// client and advertised capabilities, holds a VM whose markers were lost, and
// drives an attempt in flight until it is over. handled is false when the
// reconcile goes on as usual (it then reaches startIdentityAttempt for a
// Create).
func (r *VirtualMachineReconciler) reconcileIdentityGate(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerCR *infravirtrigaudiov1beta1.Provider, instance contracts.Provider,
	vmClass *infravirtrigaudiov1beta1.VMClass) (ctrl.Result, bool, error) {
	ia, res, ok := r.identityAttemptFor(ctx, vm, providerCR, instance)
	if !ok {
		return res, true, nil
	}
	ia.vmClass = vmClass
	if stateLost(vm) {
		return r.holdOperation(ctx, vm, k8s.ReasonOperationStateLost, msgOperationStateLost, operationHoldRecheckInterval), true, nil
	}
	r.cleanupTerminalIntents(ctx, vm, providerCR)
	if a := inFlightAttempt(vm); a != nil {
		step, res, err := r.driveAttempt(ctx, vm, ia, a)
		res, err = r.gateDriveStep(ctx, vm, ia, a, step, res, err)
		return res, true, err
	}
	if c := findAttempt(vm, contracts.OperationKindCreate); c != nil && vm.Status.ID == "" &&
		c.Outcome == infravirtrigaudiov1beta1.OperationAttemptOutcomeFailedDirty {
		return r.holdOperation(ctx, vm, k8s.ReasonOperationFailedDirty, msgOperationFailedDirty, operationHoldRecheckInterval), true, nil
	}
	if c := findAttempt(vm, contracts.OperationKindCreate); c != nil && vm.Status.ID == "" &&
		c.Outcome == infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded {
		// A Create recorded Succeeded with no id recorded: the id was lost
		// with the status.
		return r.holdOperation(ctx, vm, k8s.ReasonOperationStateLost, msgOperationStateLost, operationHoldRecheckInterval), true, nil
	}
	return ctrl.Result{}, false, nil
}

// startIdentityCreate starts a new Create attempt of vm on host with req
// (createVMOn, clustered, once the pending host is recorded): steps 1-3 of
// the lifecycle.
func (r *VirtualMachineReconciler) startIdentityCreate(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerCR *infravirtrigaudiov1beta1.Provider, instance contracts.Provider, vmClass *infravirtrigaudiov1beta1.VMClass,
	req contracts.CreateRequest) (ctrl.Result, error) {
	ia, res, ok := r.identityAttemptFor(ctx, vm, providerCR, instance)
	if !ok {
		return res, nil
	}
	ia.vmClass = vmClass
	step, res, err := r.startAttempt(ctx, vm, ia, contracts.OperationRequest{Kind: contracts.OperationKindCreate, Spec: req}, req.TargetHostID)
	if step == stepSucceeded {
		return r.afterCreateSucceeded(ctx, vm, ia), nil
	}
	return res, err
}

// startAttempt is steps 1-3 for a new attempt of opReq on host. It refuses
// while any attempt is in flight, while the latest attempt of the kind is
// held or in its backoff, and when the host has no enrolment.
func (r *VirtualMachineReconciler) startAttempt(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt, opReq contracts.OperationRequest, host string) (attemptStep, ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if a := inFlightAttempt(vm); a != nil {
		return stepWaiting, ctrl.Result{RequeueAfter: operationPollInterval}, nil
	}
	attemptNo := int64(1)
	if prev := findAttempt(vm, opReq.Kind); prev != nil {
		switch {
		case prev.Outcome == infravirtrigaudiov1beta1.OperationAttemptOutcomeFailedDirty:
			acked, err := r.consumeAcknowledgement(ctx, vm, prev)
			if err != nil {
				return stepWaiting, ctrl.Result{}, err
			}
			if !acked {
				return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationFailedDirty, msgOperationFailedDirty, operationHoldRecheckInterval), nil
			}
		case notPerformed(prev.Outcome):
			if wait := attemptRetryWait(prev, r.now()); wait > 0 {
				msg := fmt.Sprintf("%s attempt %d was not performed (%s); attempt %d is sent in %s",
					prev.Kind, prev.Attempt, prev.Outcome, prev.Attempt+1, wait.Round(time.Second))
				k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonOperationRetryBackoff, msg, vm.Generation)
				r.updateStatus(ctx, vm)
				return stepWaiting, ctrl.Result{RequeueAfter: wait}, nil
			}
		case prev.Outcome == infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded && opReq.Kind == contracts.OperationKindDelete:
			return stepSucceeded, ctrl.Result{}, nil
		case prev.Outcome == infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded && repeatableKind(opReq.Kind):
			// A power operation or a Reconfigure may be asked for again once
			// the last one Succeeded (the VM changed since), but not sooner
			// than repeatGap after it, doubling over consecutive successes
			// of the kind within repeatWindow (repeatGapWait), so a guest
			// that powers itself off, or a Provider whose Describe lags its
			// task, cannot drive a fast loop of new attempts.
			if wait := r.repeatGapWait(vm, prev, r.now()); wait > 0 {
				return stepWaiting, ctrl.Result{RequeueAfter: wait}, nil
			}
		default:
			return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationStateLost, msgOperationStateLost, operationHoldRecheckInterval), nil
		}
		attemptNo = prev.Attempt + 1
	}
	enrol, step, res, ok := r.grantEnrolment(ctx, vm, ia, host)
	if !ok {
		return step, res, nil
	}
	binding := bindingIDOf(vm)
	newBinding := false
	if binding == "" {
		if opReq.Kind != contracts.OperationKindCreate || vm.Status.Placement == nil {
			return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationStateLost, msgOperationStateLost, operationHoldRecheckInterval), nil
		}
		b, err := operation.NewRandomID()
		if err != nil {
			return stepWaiting, ctrl.Result{}, fmt.Errorf("draw a binding id: %w", err)
		}
		binding, newBinding = hex.EncodeToString(b), true
	}
	bindingBytes, err := hex.DecodeString(binding)
	if err != nil || len(bindingBytes) != operation.IDLen {
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationStateLost, msgOperationStateLost, operationHoldRecheckInterval), nil
	}
	opID, err := operation.NewRandomID()
	if err != nil {
		return stepWaiting, ctrl.Result{}, fmt.Errorf("draw an op_id: %w", err)
	}
	identity := contracts.OperationAttemptIdentity{
		ProviderUID: string(ia.providerCR.UID),
		HostID:      host,
		EnrolmentID: enrol.EnrolmentID,
		Epoch:       contracts.OperationEpoch{Counter: enrol.Epoch.Counter, Holder: enrol.Epoch.Holder},
		VMUID:       string(vm.UID),
		BindingID:   bindingBytes,
		OpID:        opID,
		Attempt:     uint64(attemptNo), // #nosec G115 -- attemptNo >= 1
		MintEpoch:   contracts.OperationEpoch{Counter: enrol.Epoch.Counter, Holder: enrol.Epoch.Holder},
	}
	prepared, err := ia.sender.BuildOperation(ctx, identity, opReq)
	if err != nil {
		logger.Error(err, "Cannot build the operation; nothing is sent", "kind", opReq.Kind)
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError,
			fmt.Sprintf("Cannot build the %s operation from the VM's specification; nothing is sent (see the manager log)", opReq.Kind),
			vm.Generation)
		r.updateStatus(ctx, vm)
		return stepWaiting, ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
	}
	if len(prepared.RequestBytes) > maxIntentRequestBytes {
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationRequestTooLarge, msgOperationRequestTooLarge, operationHoldRecheckInterval), nil
	}
	if err := r.storeIntent(ctx, ia.providerCR, vm, prepared); err != nil {
		logger.Error(err, "Cannot store the operation's intent; nothing is sent", "kind", opReq.Kind)
		if errors.Is(err, errIntentTooLarge) {
			return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationRequestTooLarge, msgOperationRequestTooLarge, operationHoldRecheckInterval), nil
		}
		return stepWaiting, ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
	}
	opIDHex := hex.EncodeToString(opID)
	marker := infravirtrigaudiov1beta1.OperationAttempt{
		Kind: attemptKindOf(opReq.Kind), Attempt: attemptNo, OpID: opIDHex, BindingID: binding, HostID: host,
		Phase: infravirtrigaudiov1beta1.OperationAttemptPhasePrepared, LastTransitionTime: metav1.NewTime(r.now()),
	}
	w := identityStatusWrite{attempts: withAttempt(vm, marker)}
	if newBinding {
		pl := vm.Status.Placement.DeepCopy()
		pl.BindingID = binding
		w.setPlacement, w.placement = true, pl
	}
	if err := r.writeIdentityStatus(ctx, vm, w); err != nil {
		logger.Info("The attempt marker was not written; nothing is sent", "kind", opReq.Kind, "attempt", attemptNo,
			"error", err.Error())
		if r.markerNotStored(ctx, vm, opIDHex) {
			// Certainly not stored: the intent Secret is this reconcile's
			// own and nothing will ever name it.
			if derr := r.deleteIntent(ctx, ia.providerCR.Namespace, opIDHex, vm.UID); derr != nil {
				logger.Info("Could not remove the unused operation-intent Secret; the sweeper will", "error", derr.Error())
			}
		}
		return stepWaiting, ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
	}
	logger.Info("Operation attempt recorded", "kind", opReq.Kind, "attempt", attemptNo, "opID", opIDHex, "host", host)
	// The host's store position the manager holds now is persisted before the
	// attempt is first sent: every answer about the attempt is checked
	// against at least that position, also after a restart.
	if err := r.HostAnswers.persistPosition(ctx, ia.providerCR, host); err != nil {
		logger.Info("The host's store position was not persisted; the attempt is sent later", "error", err.Error())
		return stepWaiting, ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
	}
	if r.operationCrashed(ctx, crashAfterPrepared) {
		return stepWaiting, ctrl.Result{}, errOperationCrashed
	}
	ia.noteResult(&prepared, nil)
	return r.sendAttempt(ctx, vm, ia, &marker, prepared, true)
}

// driveAttempt is step 3 or 4 for the attempt a in flight, once its intent
// Secret authenticates and matches a, vm and the Provider
// (loadVerifiedIntent): the host is asked about its op_id, and only a
// verified signed answer moves it (driveSigned); it is re-sent from the
// Secret with the same bytes and op_id only after the host signed that it
// has no record of it.
func (r *VirtualMachineReconciler) driveAttempt(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt, a *infravirtrigaudiov1beta1.OperationAttempt) (attemptStep, ctrl.Result, error) {
	prepared, predecessor, step, res, ok := r.loadVerifiedIntent(ctx, vm, ia, a)
	if !ok {
		return step, res, nil
	}
	ia.noteResult(&prepared, nil)
	if a.Phase == infravirtrigaudiov1beta1.OperationAttemptPhaseSent && !r.taskRefBound(vm, a) {
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationStateLost, msgTaskNotBound, operationHoldRecheckInterval), nil
	}
	return r.driveSigned(ctx, vm, ia, a, prepared, predecessor, false)
}

// loadVerifiedIntent loads the intent of attempt a and checks it: it must
// authenticate (OperationIntentAuth), and its identity must name vm's UID,
// the Provider's UID, a's op_id, kind, attempt, host and binding, and the
// host's current enrolment; a Delete's request must name the VM a Delete is
// routed to now (ia.deleteRef), and a Create's request a's host and vm as
// owner. ok is false when it does not, with the hold to return: a missing
// Secret is OperationIntentLost; anything else is OperationIntentTampered
// (with a Warning event), the Secret is kept as evidence, and nothing is
// sent.
func (r *VirtualMachineReconciler) loadVerifiedIntent(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt, a *infravirtrigaudiov1beta1.OperationAttempt) (contracts.PreparedOperation, bool, attemptStep, ctrl.Result, bool) {
	prepared, found, err := r.loadIntent(ctx, vm, a.OpID, ia.providerCR.Namespace)
	switch {
	case err != nil && errors.Is(err, operation.ErrNoIntentAuthenticator):
		return prepared, false, stepHeld, r.holdOperationSignerUnavailable(ctx, vm), false
	case err != nil && errors.Is(err, errIntentTampered):
		return prepared, false, stepHeld, r.holdIntentTampered(ctx, vm, a, err), false
	case err != nil:
		log.FromContext(ctx).Info("Cannot read the operation-intent Secret; retrying", "error", err.Error())
		return prepared, false, stepWaiting, ctrl.Result{RequeueAfter: providerErrorRetryInterval}, false
	case !found:
		return prepared, false, stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationIntentLost, msgOperationIntentLost, operationHoldRecheckInterval), false
	}
	err = r.checkIntent(ctx, vm, ia, a, prepared)
	switch {
	case err == nil:
		return prepared, false, stepWaiting, ctrl.Result{}, true
	case errors.Is(err, errIntentPredecessor):
		return prepared, true, stepWaiting, ctrl.Result{}, true
	case errors.Is(err, errNoEnrolmentSource):
		return prepared, false, stepHeld, r.holdNoEnrolmentSource(ctx, vm), false
	}
	return prepared, false, stepHeld, r.holdIntentTampered(ctx, vm, a, err), false
}

// checkIntent cross-checks an authenticated intent against the state the
// manager holds now (see loadVerifiedIntent).
func (r *VirtualMachineReconciler) checkIntent(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	a *infravirtrigaudiov1beta1.OperationAttempt, p contracts.PreparedOperation) error {
	err := r.checkIntentIdentity(ctx, vm, ia, a, p.Kind, p.Identity)
	if err != nil && !errors.Is(err, errIntentPredecessor) {
		return err
	}
	if rerr := checkIntentRequest(vm, ia, a, p); rerr != nil {
		return rerr
	}
	return err
}

// checkIntentIdentity is the part of checkIntent about the stored identity
// of kind: it must name a's kind, attempt, op_id, host and binding, vm's UID,
// the Provider's UID, and the host's current enrolment (or a predecessor one:
// errIntentPredecessor). It is all a redacted intent can be checked against;
// its request is bound by its digest instead.
func (r *VirtualMachineReconciler) checkIntentIdentity(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	a *infravirtrigaudiov1beta1.OperationAttempt, kind contracts.OperationKind, id contracts.OperationAttemptIdentity) error {
	switch {
	case attemptKindOf(kind) != a.Kind:
		return fmt.Errorf("kind: %w", errIntentTampered)
	case id.Attempt != uint64(a.Attempt): // #nosec G115 -- CRD minimum 1
		return fmt.Errorf("attempt: %w", errIntentTampered)
	case hex.EncodeToString(id.OpID) != a.OpID:
		return fmt.Errorf("op_id: %w", errIntentTampered)
	case id.VMUID != string(vm.UID):
		return fmt.Errorf("VM uid: %w", errIntentTampered)
	case id.ProviderUID != string(ia.providerCR.UID):
		return fmt.Errorf("Provider uid: %w", errIntentTampered)
	case id.HostID != a.HostID:
		return fmt.Errorf("host: %w", errIntentTampered)
	case hex.EncodeToString(id.BindingID) != a.BindingID:
		return fmt.Errorf("binding: %w", errIntentTampered)
	}
	if r.HostAnswers == nil {
		return errNoEnrolmentSource
	}
	trust, err := r.HostAnswers.Trust.HostTrust(ctx, ia.providerCR, a.HostID)
	if err != nil {
		if errors.Is(err, operation.ErrNoEnrolment) {
			return fmt.Errorf("enrolment: %w", errIntentTampered)
		}
		return errNoEnrolmentSource
	}
	enrol := trust.Enrolment
	if !bytes.Equal(enrol.EnrolmentID, id.EnrolmentID) || enrol.Epoch.Counter != id.Epoch.Counter ||
		!bytes.Equal(enrol.Epoch.Holder, id.Epoch.Holder) {
		for _, pre := range trust.Predecessors {
			if bytes.Equal(pre, id.EnrolmentID) {
				return errIntentPredecessor
			}
		}
		return fmt.Errorf("enrolment: %w", errIntentTampered)
	}
	return nil
}

// errNoEnrolmentSource is checkIntent's answer when the host's enrolment
// cannot be read: the intent's enrolment cannot be checked, so nothing is
// sent.
var errNoEnrolmentSource = errors.New("the host's operation-identity enrolment cannot be read")

// errIntentPredecessor is checkIntent's answer for an intent issued under an
// earlier enrolment that the host still holds frozen: the attempt may be
// asked about by op_id, never re-sent.
var errIntentPredecessor = errors.New("the intent names a predecessor enrolment of the host")

// msgNoEnrolmentSource is the hold of a manager that cannot read the host's
// enrolment.
var msgNoEnrolmentSource = "the host's operation-identity enrolment cannot be read (a development build needs per-host agent " +
	"enrolment, --" + DevHostEnrolmentNamespaceFlagName + "); nothing is sent"

// holdNoEnrolmentSource holds vm when no enrolment source is loaded.
func (r *VirtualMachineReconciler) holdNoEnrolmentSource(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) ctrl.Result {
	return r.holdOperation(ctx, vm, k8s.ReasonOperationEnrolmentUnavailable, msgNoEnrolmentSource, operationHoldRecheckInterval)
}

// msgTaskNotBound is the hold of an attempt whose task reference is not bound
// to it.
var msgTaskNotBound = "the task reference recorded for an operation attempt is not bound to it " +
	"(status.operationAttempts may have been edited): it is neither polled nor used to confirm an outcome; " + operationHoldRecovery

// taskTag is the hex task tag binding taskRef to the attempt a of vm
// (operation.TaskMessage under the intent authenticator).
func (r *VirtualMachineReconciler) taskTag(vm *infravirtrigaudiov1beta1.VirtualMachine, a *infravirtrigaudiov1beta1.OperationAttempt,
	taskRef string) string {
	return hex.EncodeToString(r.OperationIntentAuth.SealIntent(taskMessage(vm, a, taskRef)))
}

// taskMessage is the message a task tag covers: the attempt's op_id, kind
// and number, the VM's UID and the task reference.
func taskMessage(vm *infravirtrigaudiov1beta1.VirtualMachine, a *infravirtrigaudiov1beta1.OperationAttempt, taskRef string) []byte {
	return operation.TaskMessage([]byte(a.OpID), []byte(a.Kind), []byte(strconv.FormatInt(a.Attempt, 10)),
		[]byte(vm.UID), []byte(taskRef), []byte(strconv.FormatBool(a.Honest)), []byte(strconv.FormatBool(a.RestartRequired)))
}

// taskRefBound reports whether what a's Sent marker records (its task
// reference, possibly empty, the provider's id and the Reconfigure flags) is
// what the operator recorded for it: its TaskTag verifies over a's op_id,
// kind, attempt, vm's UID, the reference and the flags, and no other marker
// of vm names the same non-empty reference.
func (r *VirtualMachineReconciler) taskRefBound(vm *infravirtrigaudiov1beta1.VirtualMachine, a *infravirtrigaudiov1beta1.OperationAttempt) bool {
	if r.OperationIntentAuth == nil {
		return false
	}
	for _, other := range vm.Status.OperationAttempts {
		if a.TaskRef != "" && other.Kind != a.Kind && other.TaskRef == a.TaskRef {
			return false
		}
	}
	tag, err := hex.DecodeString(a.TaskTag)
	return err == nil && r.OperationIntentAuth.OpenIntent(taskMessage(vm, a, a.TaskRef), tag)
}

// checkIntentRequest decodes the stored request as its kind's message and
// checks what it targets: a Delete names ia.deleteRef (its id, host and
// owner), a Create targets a's host with vm as owner.
func checkIntentRequest(vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	a *infravirtrigaudiov1beta1.OperationAttempt, p contracts.PreparedOperation) error {
	wire, ok := wireKinds[p.Kind]
	if !ok {
		return fmt.Errorf("kind: %w", errIntentTampered)
	}
	msg, err := operation.DecodeRequest(wire, p.RequestBytes)
	if err != nil {
		return fmt.Errorf("request: %w", errIntentTampered)
	}
	switch req := msg.(type) {
	case *providerv1.DeleteRequest:
		if ia.deleteRef == nil {
			return fmt.Errorf("a Delete outside a deletion: %w", errIntentTampered)
		}
		ref := ia.deleteRef
		if req.GetId() != ref.ID || req.GetTargetHostId() != ref.HostID || req.GetTargetHostId() != a.HostID ||
			req.GetOwner().GetUid() != string(vm.UID) {
			return fmt.Errorf("Delete target: %w", errIntentTampered)
		}
	case *providerv1.CreateRequest:
		if req.GetTargetHostId() != a.HostID || req.GetOwner().GetUid() != string(vm.UID) {
			return fmt.Errorf("Create target: %w", errIntentTampered)
		}
	case *providerv1.PowerRequest:
		row, _ := operation.RowFor(wire) // DecodeRequest refused a kind with no row
		if req.GetOp() != row.FixedPowerOp {
			return fmt.Errorf("power op: %w", errIntentTampered)
		}
		if err := checkVMTarget(vm, a, req.GetId(), req.GetTargetHostId(), req.GetOwner().GetUid()); err != nil {
			return fmt.Errorf("Power target: %w", err)
		}
	case *providerv1.ReconfigureRequest:
		if err := checkVMTarget(vm, a, req.GetId(), req.GetTargetHostId(), req.GetOwner().GetUid()); err != nil {
			return fmt.Errorf("Reconfigure target: %w", err)
		}
		if _, _, err := requestedResources(req); err != nil {
			return fmt.Errorf("Reconfigure resources: %w", errIntentTampered)
		}
		if err := checkReconfigureProjection(req); err != nil {
			return fmt.Errorf("Reconfigure projection: %w", errIntentTampered)
		}
	default:
		return fmt.Errorf("unexpected request type: %w", errIntentTampered)
	}
	return nil
}

// wireKinds maps the kinds this controller sends to their wire kinds.
var wireKinds = map[contracts.OperationKind]providerv1.OperationKind{
	contracts.OperationKindCreate:           providerv1.OperationKind_OPERATION_KIND_CREATE,
	contracts.OperationKindDelete:           providerv1.OperationKind_OPERATION_KIND_DELETE,
	contracts.OperationKindPowerOn:          providerv1.OperationKind_OPERATION_KIND_POWER_ON,
	contracts.OperationKindPowerOff:         providerv1.OperationKind_OPERATION_KIND_POWER_OFF,
	contracts.OperationKindReboot:           providerv1.OperationKind_OPERATION_KIND_REBOOT,
	contracts.OperationKindShutdownGraceful: providerv1.OperationKind_OPERATION_KIND_SHUTDOWN_GRACEFUL,
	contracts.OperationKindReconfigure:      providerv1.OperationKind_OPERATION_KIND_RECONFIGURE,
}

// holdIntentTampered holds vm for an intent that failed loadVerifiedIntent,
// and records a Warning event (without any of the Secret's content) when the
// hold begins.
func (r *VirtualMachineReconciler) holdIntentTampered(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	a *infravirtrigaudiov1beta1.OperationAttempt, cause error) ctrl.Result {
	log.FromContext(ctx).Info("The operation-intent Secret failed its checks; nothing is sent and it is kept as evidence",
		"kind", a.Kind, "attempt", a.Attempt, "opID", a.OpID, "check", cause.Error())
	if c := k8s.GetCondition(vm.Status.Conditions, k8s.ConditionReady); c == nil || c.Reason != k8s.ReasonOperationIntentTampered {
		r.recordEvent(vm, corev1.EventTypeWarning, k8s.ReasonOperationIntentTampered, fmt.Sprintf(
			"The stored intent of %s attempt %d (Secret %s in the Provider's namespace) failed its authenticity or consistency "+
				"checks; nothing is sent and the Secret is kept", a.Kind, a.Attempt, intentSecretName(a.OpID)))
	}
	return r.holdOperation(ctx, vm, k8s.ReasonOperationIntentTampered, msgOperationIntentTampered, operationHoldRecheckInterval)
}

// sendAttempt delivers prepared (the attempt a) and then asks the host about
// it. The answer to a send, a success or a refusal, is the Provider's
// unsigned claim: it never decides the attempt, whatever its reason (a
// refusal of a mutating call carries no signed answer, and a grant or
// Provider refusal is definite for that delivery only). It only records the
// task reference, the provider's VM id (Create) and the Reconfigure flags on
// the Sent marker; the attempt moves on the host's signed answer about its
// op_id (driveSigned). first is true only for the delivery made by the
// reconcile that wrote a as Prepared.
func (r *VirtualMachineReconciler) sendAttempt(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt, a *infravirtrigaudiov1beta1.OperationAttempt, prepared contracts.PreparedOperation,
	first bool) (attemptStep, ctrl.Result, error) {
	if len(prepared.RequestBytes) == 0 {
		// Only a full intent can be sent; never an empty or partial request.
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationIntentLost, msgOperationIntentLost, operationHoldRecheckInterval), nil
	}
	answer, err := ia.sender.SendOperation(ctx, prepared)
	if r.operationCrashed(ctx, crashAfterSend) {
		return stepWaiting, ctrl.Result{}, errOperationCrashed
	}
	var sentAnswer *contracts.OperationAnswer
	if err != nil {
		log.FromContext(ctx).Info("The operation was not accepted; the host is asked about its op_id", "kind", a.Kind,
			"attempt", a.Attempt, "first", first, "error", providerFailureDetail(err))
		if contracts.HasErrorInfoReason(err, contracts.ProviderMismatchReason) {
			return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationProviderMismatch, msgOperationProviderMismatch,
				operationHoldRecheckInterval), nil
		}
	} else {
		ia.noteResult(nil, &answer)
		sentAnswer = &answer
	}
	sent, werr := r.markSent(ctx, vm, a, sentAnswer)
	if werr != nil {
		log.FromContext(ctx).Info("The Sent marker was not written; the host is asked about the attempt again", "error", werr.Error())
		return stepWaiting, ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
	}
	if err != nil && repeatableKind(contracts.OperationKind(a.Kind)) {
		// A host-level answer (an unavailable host, a VM missing on its host
		// or not this VM's) is reported as such and paced as the unsigned
		// path paces it; it decides nothing, and the host is asked about
		// the op_id when the hold is re-checked.
		ref := contracts.VMRef{ID: vm.Status.ID, HostID: a.HostID, Owner: vmOwnerIdentity(vm)}
		errReason := errReasonProviderPower
		if a.Kind == infravirtrigaudiov1beta1.OperationAttemptKindReconfigure {
			errReason = errReasonProviderReconfigure
		}
		if res, handled := r.handleRoutedOpError(ctx, vm, ref, err, errReason); handled {
			return stepHeld, res, nil
		}
		if res, held := r.holdOwnerRefusal(ctx, vm, ref, err, string(a.Kind)); held {
			return stepHeld, res, nil
		}
	}
	return r.driveSigned(ctx, vm, ia, sent, prepared, false, true)
}

// markSent writes a as Sent, recording what the answer to a send (nil: none)
// carried: the task reference, bound to the attempt by its task tag, the
// provider's VM id of a Create (kept on the marker, never in the Secret,
// until the Create Succeeded) and the flags of a Reconfigure. A marker that
// is already Sent keeps its transition time, so a re-send does not extend
// the attempt's deadline, and a known value is not cleared by a send that
// was not answered.
func (r *VirtualMachineReconciler) markSent(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	a *infravirtrigaudiov1beta1.OperationAttempt, answer *contracts.OperationAnswer) (*infravirtrigaudiov1beta1.OperationAttempt, error) {
	sent := *a
	if sent.Phase != infravirtrigaudiov1beta1.OperationAttemptPhaseSent {
		sent.Phase, sent.LastTransitionTime = infravirtrigaudiov1beta1.OperationAttemptPhaseSent, metav1.NewTime(r.now())
	}
	if answer != nil {
		if answer.TaskRef != "" {
			sent.TaskRef = answer.TaskRef
		}
		if a.Kind == infravirtrigaudiov1beta1.OperationAttemptKindCreate && answer.ID != "" {
			sent.ID = answer.ID
		}
		if a.Kind == infravirtrigaudiov1beta1.OperationAttemptKindReconfigure {
			sent.Honest, sent.RestartRequired = answer.Honest, answer.RestartRequired
		}
	}
	sent.TaskTag = r.taskTag(vm, &sent, sent.TaskRef)
	if sent == *a {
		return a, nil
	}
	if err := r.writeIdentityStatus(ctx, vm, identityStatusWrite{attempts: withAttempt(vm, sent)}); err != nil {
		return nil, err
	}
	return &sent, nil
}

// closeAttempt writes the attempt a Terminal with outcome (and, for a
// successful Create, the VM's id and confirmed placement in the same write),
// then deletes its intent Secret.
func (r *VirtualMachineReconciler) closeAttempt(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt, a *infravirtrigaudiov1beta1.OperationAttempt, outcome infravirtrigaudiov1beta1.OperationAttemptOutcome,
	reason, answerID string) (attemptStep, ctrl.Result, error) {
	logger := log.FromContext(ctx)
	done := *a
	done.Phase, done.Outcome, done.LastTransitionTime = infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, outcome, metav1.NewTime(r.now())
	done.OutcomeReason = truncateForLog(reason, 128)
	w := identityStatusWrite{attempts: withAttempt(vm, done)}
	if a.Kind == infravirtrigaudiov1beta1.OperationAttemptKindCreate && outcome == infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded {
		if answerID == "" {
			return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationIntentLost, msgOperationIntentLost, operationHoldRecheckInterval), nil
		}
		promoted := vm.DeepCopy()
		promotePendingHost(promoted, a.HostID)
		w.setPlacement, w.placement = true, promoted.Status.Placement
		w.setID, w.id = true, answerID
	}
	if a.Kind == infravirtrigaudiov1beta1.OperationAttemptKindReconfigure && outcome == infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded {
		// The size is recorded in the same write as Terminal, so a restart
		// in between cannot lose it.
		w.resources = r.reconfiguredResources(vm, ia, isClusterTopology(ia.providerCR))
	}
	if err := r.writeIdentityStatus(ctx, vm, w); err != nil {
		logger.Info("The Terminal marker was not written; the attempt is asked about again", "error", err.Error())
		return stepWaiting, ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
	}
	logger.Info("Operation attempt is over", "kind", a.Kind, "attempt", a.Attempt, "outcome", outcome, "reason", done.OutcomeReason)
	if r.operationCrashed(ctx, crashAfterTerminal) {
		return stepWaiting, ctrl.Result{}, errOperationCrashed
	}
	// A Delete that Succeeded keeps its intent Secret until the finalizer is
	// released, and a Create that was not performed keeps it while it is the
	// VM's latest: each is what a later release asks the host about again.
	switch {
	case !intentKeptAsEvidence(vm, &done):
		if err := r.deleteIntent(ctx, ia.providerCR.Namespace, a.OpID, vm.UID); err != nil {
			logger.Info("Could not remove the operation-intent Secret; the sweeper will", "error", err.Error())
		}
	case createNotPerformedLatest(&done):
		// Kept for asking the host again, but only its identity and digest:
		// the request bytes, cloud-init user data included, are dropped.
		r.ensureRedacted(ctx, ia.providerCR, vm, &done)
	}
	switch outcome {
	case infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded:
		if a.Kind == infravirtrigaudiov1beta1.OperationAttemptKindCreate {
			setPlacedCondition(vm, metav1.ConditionTrue, k8s.ReasonBound, fmt.Sprintf("VM is bound to host %s", a.HostID))
		}
		return stepSucceeded, ctrl.Result{}, nil
	case infravirtrigaudiov1beta1.OperationAttemptOutcomeFailedDirty:
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationFailedDirty, msgOperationFailedDirty, operationHoldRecheckInterval), nil
	}
	wait := attemptRetryWait(&done, r.now())
	msg := fmt.Sprintf("%s attempt %d was not performed (%s); attempt %d is sent in %s", done.Kind, done.Attempt, outcome,
		done.Attempt+1, wait.Round(time.Second))
	k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonOperationRetryBackoff, msg, vm.Generation)
	r.updateStatus(ctx, vm)
	return stepNotPerformed, ctrl.Result{RequeueAfter: wait}, nil
}

// cleanupTerminalIntents deletes the intent Secrets of vm's attempts that
// became Terminal within operationIntentSweepGrace: a manager that stopped
// between an attempt's Terminal write and its Secret delete leaves one behind.
// Later ones are left to the sweeper. The latest Create, when not performed,
// has its intent redacted (ensureRedacted): checked once per manager process,
// which finishes a redaction a crash interrupted whenever it happened, and
// not read again. So a settled VM costs no reads.
func (r *VirtualMachineReconciler) cleanupTerminalIntents(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	provider *infravirtrigaudiov1beta1.Provider) {
	ns := provider.Namespace
	for _, a := range vm.Status.OperationAttempts {
		if createNotPerformedLatest(&a) {
			// A redaction interrupted (by a crash or a failed write) is
			// finished here; once this process has seen it done, it is not
			// read again.
			r.ensureRedacted(ctx, provider, vm, &a)
			continue
		}
		if a.Phase != infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal || r.now().Sub(a.LastTransitionTime.Time) > operationIntentSweepGrace ||
			intentKeptAsEvidence(vm, &a) {
			continue
		}
		if err := r.deleteIntent(ctx, ns, a.OpID, vm.UID); err != nil {
			log.FromContext(ctx).Info("Could not remove a finished attempt's operation-intent Secret; the sweeper will",
				"kind", a.Kind, "error", err.Error())
		}
	}
}

// eventReasonIntentRedactionBlocked is the Warning event of a redaction that
// cannot be done (a Secret at the redacted intent's name that is not this
// attempt's, or a full intent that does not authenticate).
const eventReasonIntentRedactionBlocked = "OperationIntentRedactionBlocked"

// ensureRedacted redacts the intent of a, a not-performed latest Create,
// unless this process already saw it redacted (then it costs no read). A
// blocked redaction is announced once per op_id with a Warning event that
// names both Secrets and none of their content; the full intent stays.
func (r *VirtualMachineReconciler) ensureRedacted(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	vm *infravirtrigaudiov1beta1.VirtualMachine, a *infravirtrigaudiov1beta1.OperationAttempt) {
	if _, done := r.redactedOps.Load(a.OpID); done {
		return
	}
	err := r.redactIntent(ctx, provider, vm, a.OpID)
	if err == nil {
		r.redactedOps.Store(a.OpID, struct{}{})
		return
	}
	log.FromContext(ctx).Info("Could not redact a not-performed Create's operation-intent Secret; retrying", "opID", a.OpID,
		"error", err.Error())
	if !errors.Is(err, errIntentTampered) && !errors.Is(err, errRedactionMismatch) {
		return
	}
	if _, warned := r.redactionWarned.LoadOrStore(a.OpID, struct{}{}); warned {
		return
	}
	r.recordEvent(vm, corev1.EventTypeWarning, eventReasonIntentRedactionBlocked, fmt.Sprintf(
		"The intent of %s attempt %d cannot be redacted: Secret %s or %s in the Provider's namespace is not this attempt's "+
			"(or does not authenticate). The full intent is kept; inspect both, delete the wrong redacted Secret, and the next "+
			"reconcile redacts (docs/operation-grants-manager.md)", a.Kind, a.Attempt, intentSecretName(a.OpID), redactedIntentName(a.OpID)))
}

// afterCreateSucceeded records what follows a successful Create once its id
// and placement are written: the Create's outcome, the applied size, the
// Bound condition. The VM is described next.
func (r *VirtualMachineReconciler) afterCreateSucceeded(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt) ctrl.Result {
	setCreateOutcome(vm, false)
	if ia.vmClass != nil {
		r.updateCurrentResources(vm, ia.vmClass)
	}
	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: createdDescribeFallback}
}

// identityDelete is the finalizer of a VM of a Provider that requires
// operation identity, in place of the unsigned Delete: an attempt in flight
// (of any kind) is driven first; the finalizer is released only once a
// Delete attempt is Terminal and Succeeded, or when nothing was ever created
// (no Create attempt, or the latest one was not performed and no id is
// recorded). Orphan-on-delete is decided before this is reached;
// force-delete releases the VM wherever this would hold it.
func (r *VirtualMachineReconciler) identityDelete(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerCR *infravirtrigaudiov1beta1.Provider, instance contracts.Provider, ref contracts.VMRef, force bool) (ctrl.Result, error) {
	ia, res, ok := r.identityAttemptFor(ctx, vm, providerCR, instance)
	if !ok {
		if force {
			return r.releaseLeavingHypervisorVM(ctx, vm, vmLeftOperationHeld)
		}
		return res, nil
	}
	ia.deleteRef = &ref
	if stateLost(vm) {
		if force {
			return r.releaseLeavingHypervisorVM(ctx, vm, vmLeftOperationHeld)
		}
		return r.holdOperation(ctx, vm, k8s.ReasonOperationStateLost, msgOperationStateLost, operationHoldRecheckInterval), nil
	}
	if a := inFlightAttempt(vm); a != nil {
		step, res, err := r.driveAttempt(ctx, vm, ia, a)
		if err == nil && step == stepHeld && !force && supersedableByDelete(vm, a) {
			return r.supersedeForDelete(ctx, vm, a)
		}
		return r.afterDeleteStep(ctx, vm, ia, a.Kind, step, res, err, force)
	}
	r.cleanupTerminalIntents(ctx, vm, providerCR)
	if d := findAttempt(vm, contracts.OperationKindDelete); d != nil && d.Outcome == infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded {
		return r.confirmDeleteRelease(ctx, vm, ia, d, force)
	}
	if nothingCreated(vm) {
		if c := findAttempt(vm, contracts.OperationKindCreate); c != nil && !r.signedNotPerformed(ctx, vm, ia, c) {
			// The latest Create is recorded not performed, but the host does
			// not sign that again: the stored outcome alone is not enough.
			if force {
				return r.releaseLeavingHypervisorVM(ctx, vm, vmLeftOperationHeld)
			}
			return r.holdOperation(ctx, vm, k8s.ReasonOperationStateLost, msgCreateNotPerformedUnconfirmed, operationHoldRecheckInterval), nil
		}
		log.FromContext(ctx).Info("No Create of this VM was ever performed; releasing it without a Delete")
		return r.removeFinalizer(ctx, vm)
	}
	step, res, err := r.startAttempt(ctx, vm, ia, contracts.OperationRequest{Kind: contracts.OperationKindDelete, VM: ref}, ref.HostID)
	return r.afterDeleteStep(ctx, vm, ia, infravirtrigaudiov1beta1.OperationAttemptKindDelete, step, res, err, force)
}

// msgDeleteNotConfirmed is the hold of a Delete recorded Succeeded that its
// stored intent or the Provider does not confirm.
var msgDeleteNotConfirmed = "the VM's Delete is recorded Succeeded, but its stored intent or the Provider does not confirm it " +
	"(status.operationAttempts may have been edited); " + operationHoldRecovery

// confirmDeleteRelease releases the finalizer of a VM whose Delete attempt d
// is recorded Succeeded but whose release was not made by the reconcile that
// recorded it (a restart in between, or an edited status): the Delete's
// intent Secret must authenticate and match d, and the host must confirm it
// again with a fresh signed answer: the Delete's op_id COMPLETED, or the
// VM's placement DELETED (signedDeleteConfirmed). A task status, a re-send
// or a Describe that finds no VM never does. Otherwise the VM is held (or
// released by force-delete).
func (r *VirtualMachineReconciler) confirmDeleteRelease(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt, d *infravirtrigaudiov1beta1.OperationAttempt, force bool) (ctrl.Result, error) {
	hold := func(res ctrl.Result) (ctrl.Result, error) {
		if force {
			return r.releaseLeavingHypervisorVM(ctx, vm, vmLeftOperationHeld)
		}
		return res, nil
	}
	prepared, found, err := r.loadIntent(ctx, vm, d.OpID, ia.providerCR.Namespace)
	switch {
	case err == nil && !found:
		return hold(r.holdOperation(ctx, vm, k8s.ReasonOperationStateLost, msgDeleteNotConfirmed, operationHoldRecheckInterval))
	case err != nil && errors.Is(err, errIntentTampered):
		return hold(r.holdIntentTampered(ctx, vm, d, err))
	case err != nil:
		log.FromContext(ctx).Info("Cannot read the Delete's operation-intent Secret; retrying", "error", err.Error())
		return ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
	}
	cerr := r.checkIntent(ctx, vm, ia, d, prepared)
	switch {
	case cerr == nil, errors.Is(cerr, errIntentPredecessor):
	case errors.Is(cerr, errNoEnrolmentSource):
		return hold(r.holdNoEnrolmentSource(ctx, vm))
	default:
		return hold(r.holdIntentTampered(ctx, vm, d, cerr))
	}
	if !r.signedDeleteConfirmed(ctx, vm, ia, d, refOf(prepared)) {
		return hold(r.holdOperation(ctx, vm, k8s.ReasonOperationStateLost, msgDeleteNotConfirmed, operationHoldRecheckInterval))
	}
	return r.releaseAfterDelete(ctx, vm, ia, d.OpID)
}

// releaseAfterDelete removes the finalizer of a VM whose Delete attempt opID
// Succeeded, then deletes that attempt's intent Secret (the sweeper removes
// it if this fails).
func (r *VirtualMachineReconciler) releaseAfterDelete(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt, opID string) (ctrl.Result, error) {
	res, err := r.removeFinalizer(ctx, vm)
	if err != nil {
		return res, err
	}
	if derr := r.deleteIntent(ctx, ia.providerCR.Namespace, opID, vm.UID); derr != nil {
		log.FromContext(ctx).Info("Could not remove the Delete's operation-intent Secret; the sweeper will", "error", derr.Error())
	}
	return res, nil
}

// afterDeleteStep maps a step of an attempt of kind driven by identityDelete.
func (r *VirtualMachineReconciler) afterDeleteStep(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt, kind infravirtrigaudiov1beta1.OperationAttemptKind, step attemptStep, res ctrl.Result, err error,
	force bool) (ctrl.Result, error) {
	switch {
	case err != nil:
		return res, err
	case step == stepSucceeded && kind == infravirtrigaudiov1beta1.OperationAttemptKindDelete:
		opID := ""
		if d := findAttempt(vm, contracts.OperationKindDelete); d != nil {
			opID = d.OpID
		}
		return r.releaseAfterDelete(ctx, vm, ia, opID)
	case step == stepSucceeded:
		// The Create in flight is over (its id is recorded): the Delete
		// follows on the next reconcile.
		return ctrl.Result{Requeue: true}, nil
	case force && (step == stepHeld || step == stepNotPerformed):
		return r.releaseLeavingHypervisorVM(ctx, vm, vmLeftOperationHeld)
	}
	return res, nil
}

// nothingCreated reports whether no Create of vm was ever performed: it has
// no id and either no Create attempt (and no binding: a Create attempt is
// always recorded before its first send) or a latest Create attempt that was
// not performed (an attempt follows another only when that one was not
// performed).
func nothingCreated(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	if vm.Status.ID != "" {
		return false
	}
	c := findAttempt(vm, contracts.OperationKindCreate)
	if c == nil {
		return bindingIDOf(vm) == ""
	}
	return c.Phase == infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal && notPerformed(c.Outcome)
}

// vmLeftOperationHeld is why a force-delete released a VM whose
// operation-identity delete was held.
const vmLeftOperationHeld = "its operation-identity delete was held"

// identityAttemptsNeedHost reports whether an unbound VM still has an
// operation attempt that may have done something on a host (its pending host
// was cleared by hand): one in flight, or a Create that was performed or may
// have been.
func identityAttemptsNeedHost(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	if len(vm.Status.OperationAttempts) == 0 {
		return false
	}
	return inFlightAttempt(vm) != nil || !nothingCreated(vm)
}

// holdOperationSignerUnavailable holds vm of a Provider that requires
// operation identity when the manager has no signer.
func (r *VirtualMachineReconciler) holdOperationSignerUnavailable(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) ctrl.Result {
	return r.holdOperation(ctx, vm, k8s.ReasonOperationSignerUnavailable, msgOperationSignerUnavailable, operationHoldRecheckInterval)
}
