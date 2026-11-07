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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Operation identity on the VirtualMachine controller (development builds
// only; ADR-0016 Part A, manager half). For a Provider whose spec.type
// requires operation identity (operation.RequiredProtection), the Create and
// Delete of a VM are sent as signed operations, in this order:
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
	// crashAfterReply: a task's outcome was read, the marker not Terminal.
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
	case required == operation.ProtectionIdentity && !(caps.SupportsOperationIdentity && caps.RequiresOperationIdentity):
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
func stateLost(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	return findAttempt(vm, contracts.OperationKindCreate) == nil && (vm.Status.ID != "" || bindingIDOf(vm) != "")
}

// attemptRetryWait is how long after a not-performed attempt a's terminal
// transition the next attempt waits: blockedRetryMin doubling per attempt,
// at most blockedRetryMax.
func attemptRetryWait(a *infravirtrigaudiov1beta1.OperationAttempt, now time.Time) time.Duration {
	wait := blockedRetryMin
	for i := int64(1); i < a.Attempt && wait < blockedRetryMax; i++ {
		wait *= 2
	}
	wait = min(wait, blockedRetryMax)
	return max(wait-now.Sub(a.LastTransitionTime.Time), 0)
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

// writeRejected reports whether a failed status write was refused outright
// (it certainly did not change the stored object): a failed test
// precondition, a conflict, an invalid request, forbidden, not found.
func writeRejected(err error) bool {
	return pendingHostWriteRejected(err)
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
	vmClass    *infravirtrigaudiov1beta1.VMClass
}

// identityAttemptFor checks the Provider's client and advertised capabilities
// for an operation-identity reconcile of vm. ok is false when vm was held (or
// must wait), with res to return.
func (r *VirtualMachineReconciler) identityAttemptFor(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerCR *infravirtrigaudiov1beta1.Provider, instance contracts.Provider) (identityAttempt, ctrl.Result, bool) {
	sender, ok := instance.(contracts.OperationSender)
	if !ok {
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
	return identityAttempt{providerCR: providerCR, instance: instance, sender: sender}, ctrl.Result{}, true
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
	if a := inFlightAttempt(vm); a != nil {
		step, res, err := r.driveAttempt(ctx, vm, ia, a)
		if a.Kind == infravirtrigaudiov1beta1.OperationAttemptKindCreate {
			res, err = r.gateCreateDrive(ctx, vm, ia, step, res, err)
		}
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
			return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationFailedDirty, msgOperationFailedDirty, operationHoldRecheckInterval), nil
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
		default:
			return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationStateLost, msgOperationStateLost, operationHoldRecheckInterval), nil
		}
		attemptNo = prev.Attempt + 1
	}
	if r.OperationEnrolments == nil {
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationEnrolmentUnavailable,
			"no operation-identity enrolment is configured for this manager (a development build needs --"+
				operation.DevEnrolmentFileFlagName+"); nothing is sent", operationHoldRecheckInterval), nil
	}
	enrol, err := r.OperationEnrolments.Enrolment(ia.providerCR.Namespace, ia.providerCR.Name, host)
	if err != nil {
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationEnrolmentUnavailable,
			fmt.Sprintf("no operation-identity enrolment is configured for host %s; nothing is sent", host), operationHoldRecheckInterval), nil
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
			fmt.Sprintf("Cannot build the %s operation: %s", opReq.Kind, providerErrorMessage(err)), vm.Generation)
		r.updateStatus(ctx, vm)
		return stepWaiting, ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
	}
	if err := r.storeIntent(ctx, ia.providerCR, vm, prepared); err != nil {
		logger.Error(err, "Cannot store the operation's intent; nothing is sent", "kind", opReq.Kind)
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
			"rejected", writeRejected(err), "error", err.Error())
		if writeRejected(err) {
			// Certainly not stored: the intent Secret is this reconcile's
			// own and nothing will ever name it.
			if derr := r.deleteIntent(ctx, ia.providerCR.Namespace, opIDHex, vm.UID); derr != nil {
				logger.Info("Could not remove the unused operation-intent Secret; the sweeper will", "error", derr.Error())
			}
		}
		return stepWaiting, ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
	}
	logger.Info("Operation attempt recorded", "kind", opReq.Kind, "attempt", attemptNo, "opID", opIDHex, "host", host)
	if r.operationCrashed(ctx, crashAfterPrepared) {
		return stepWaiting, ctrl.Result{}, errOperationCrashed
	}
	return r.sendAttempt(ctx, vm, ia, &marker, prepared, "", true)
}

// driveAttempt is step 3 or 4 for the attempt a in flight: a Sent attempt
// with a task is polled; any other is re-sent from its intent Secret with
// the same bytes and op_id.
func (r *VirtualMachineReconciler) driveAttempt(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt, a *infravirtrigaudiov1beta1.OperationAttempt) (attemptStep, ctrl.Result, error) {
	prepared, answerID, found, err := r.loadIntent(ctx, ia.providerCR.Namespace, a.OpID, vm.UID)
	switch {
	case err != nil && errors.Is(err, errIntentNotOurs):
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationIntentLost, msgOperationIntentLost, operationHoldRecheckInterval), nil
	case err != nil:
		log.FromContext(ctx).Info("Cannot read the operation-intent Secret; retrying", "error", err.Error())
		return stepWaiting, ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
	case !found:
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationIntentLost, msgOperationIntentLost, operationHoldRecheckInterval), nil
	case attemptKindOf(prepared.Kind) != a.Kind || prepared.Identity.Attempt != uint64(a.Attempt): // #nosec G115 -- CRD minimum 1
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationIntentLost, msgOperationIntentLost, operationHoldRecheckInterval), nil
	}
	if a.Phase == infravirtrigaudiov1beta1.OperationAttemptPhaseSent && a.TaskRef != "" {
		return r.pollAttempt(ctx, vm, ia, a, answerID)
	}
	return r.sendAttempt(ctx, vm, ia, a, prepared, answerID, false)
}

// sendAttempt sends prepared (the attempt a) and records the answer. first is
// true only for the delivery made by the reconcile that wrote a as Prepared.
func (r *VirtualMachineReconciler) sendAttempt(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt, a *infravirtrigaudiov1beta1.OperationAttempt, prepared contracts.PreparedOperation,
	knownAnswerID string, first bool) (attemptStep, ctrl.Result, error) {
	logger := log.FromContext(ctx)
	answer, err := ia.sender.SendOperation(ctx, prepared)
	if r.operationCrashed(ctx, crashAfterSend) {
		return stepWaiting, ctrl.Result{}, errOperationCrashed
	}
	if err != nil {
		return r.sendFailed(ctx, vm, ia, a, err, first)
	}
	if answer.TaskRef == "" {
		id := answer.ID
		if id == "" {
			id = knownAnswerID
		}
		return r.closeAttempt(ctx, vm, ia, a, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded, "", id)
	}
	if a.Kind == infravirtrigaudiov1beta1.OperationAttemptKindCreate && answer.ID != "" {
		if err := r.recordIntentAnswer(ctx, ia.providerCR.Namespace, a.OpID, vm.UID, answer.ID); err != nil {
			logger.Info("Cannot record the Create's answer; the attempt is re-sent", "error", err.Error())
			return stepWaiting, ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
		}
	}
	sent := *a
	sent.Phase, sent.TaskRef, sent.LastTransitionTime = infravirtrigaudiov1beta1.OperationAttemptPhaseSent, answer.TaskRef, metav1.NewTime(r.now())
	if err := r.writeIdentityStatus(ctx, vm, identityStatusWrite{attempts: withAttempt(vm, sent)}); err != nil {
		logger.Info("The Sent marker was not written; the attempt is re-sent with the same op_id", "error", err.Error())
		return stepWaiting, ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
	}
	return stepWaiting, r.operationInProgress(ctx, vm, &sent, operationPollInterval), nil
}

// sendFailed handles a send of the attempt a that failed with err. Only a
// refusal of the first delivery with a reason that proves it was not
// performed ends the attempt (Rejected); a refusal that proves nothing about
// an earlier delivery holds it; anything else (a transport error, VM_BUSY,
// UNATTESTED, ...) leaves it as it is, to be re-sent with the same op_id.
// Every reason is the Provider's unsigned claim.
func (r *VirtualMachineReconciler) sendFailed(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt, a *infravirtrigaudiov1beta1.OperationAttempt, err error, first bool) (attemptStep, ctrl.Result, error) {
	log.FromContext(ctx).Info("The operation was not accepted", "kind", a.Kind, "attempt", a.Attempt, "first", first,
		"error", providerFailureDetail(err))
	switch {
	case contracts.HasErrorInfoReason(err, contracts.ProviderMismatchReason):
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationProviderMismatch, msgOperationProviderMismatch, operationHoldRecheckInterval), nil
	case definiteRejection(err) && first:
		return r.closeAttempt(ctx, vm, ia, a, infravirtrigaudiov1beta1.OperationAttemptOutcomeRejected, rejectionReason(err), "")
	case redeliveryRefusal(err) != "":
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationOutcomeUnknown,
			fmt.Sprintf("the host refused a re-delivery of %s attempt %d (%s), which proves nothing about an earlier delivery; "+
				"the same attempt is re-sent periodically; %s", a.Kind, a.Attempt, redeliveryRefusal(err), operationHoldRecovery),
			operationHoldRecheckInterval), nil
	}
	k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError,
		fmt.Sprintf("%s attempt %d was not accepted; it is re-sent with the same identity: %s", a.Kind, a.Attempt, providerFailureDetail(err)),
		vm.Generation)
	r.updateStatus(ctx, vm)
	return stepWaiting, ctrl.Result{RequeueAfter: blockedRetryMin}, nil
}

// definiteRejectionReasons are the refusals that prove the refused delivery
// was not performed and name its own operation, so a refused FIRST delivery
// ends the attempt. NONCE_CONSUMED and REQUEST_CONSUMED (this delivery only)
// and PROVIDER_MISMATCH (held) are handled apart.
var definiteRejectionReasons = []string{
	contracts.GrantUnauthenticatedReason, contracts.GrantMismatchReason, contracts.DigestMismatchReason,
	contracts.DeadlineExceededNotPerformedReason,
}

// definiteRejection reports whether err carries a definiteRejectionReasons
// reason.
func definiteRejection(err error) bool {
	return rejectionReason(err) != ""
}

// rejectionReason is err's definiteRejectionReasons reason, or "".
func rejectionReason(err error) string {
	for _, reason := range definiteRejectionReasons {
		if contracts.HasErrorInfoReason(err, reason) {
			return reason
		}
	}
	return ""
}

// redeliveryRefusalReasons are the refusals of a re-delivery that prove
// nothing about an earlier delivery of the same attempt: the attempt is held.
var redeliveryRefusalReasons = []string{
	contracts.GrantUnauthenticatedReason, contracts.GrantMismatchReason, contracts.DigestMismatchReason,
	contracts.DeadlineExceededNotPerformedReason, contracts.OpIDConflictReason, contracts.NonceConsumedReason,
	contracts.RequestConsumedReason,
}

// redeliveryRefusal is err's redeliveryRefusalReasons reason, or "".
func redeliveryRefusal(err error) string {
	for _, reason := range redeliveryRefusalReasons {
		if contracts.HasErrorInfoReason(err, reason) {
			return reason
		}
	}
	return ""
}

// pollAttempt reads the task of the Sent attempt a (a plain read: it never
// writes unless the task is over) and closes the attempt on a terminal
// outcome.
func (r *VirtualMachineReconciler) pollAttempt(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt, a *infravirtrigaudiov1beta1.OperationAttempt, answerID string) (attemptStep, ctrl.Result, error) {
	st, err := ia.instance.TaskStatus(ctx, a.TaskRef)
	if err != nil {
		if contracts.IsUnknownToAgent(err) {
			return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationUnknownToAgent, msgOperationUnknownToAgent, operationHoldRecheckInterval), nil
		}
		log.FromContext(ctx).Info("Cannot read the operation's task status; asking again", "error", providerFailureDetail(err))
		return stepWaiting, ctrl.Result{RequeueAfter: blockedRetryMin}, nil
	}
	var outcome infravirtrigaudiov1beta1.OperationAttemptOutcome
	switch st.Outcome {
	case contracts.TaskOutcomeRunning:
		return stepWaiting, r.operationInProgress(ctx, vm, a, operationPollInterval), nil
	case contracts.TaskOutcomeSucceeded:
		outcome = infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded
	case contracts.TaskOutcomeFailedClean:
		outcome = infravirtrigaudiov1beta1.OperationAttemptOutcomeFailedClean
	case contracts.TaskOutcomeRetired:
		outcome = infravirtrigaudiov1beta1.OperationAttemptOutcomeRetired
	case contracts.TaskOutcomeCancelled:
		outcome = infravirtrigaudiov1beta1.OperationAttemptOutcomeCancelled
	case contracts.TaskOutcomeFailedDirty:
		outcome = infravirtrigaudiov1beta1.OperationAttemptOutcomeFailedDirty
	case contracts.TaskOutcomeUnknown:
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationOutcomeUnknown,
			fmt.Sprintf("the Provider cannot tell what %s attempt %d did; it is asked again periodically; %s", a.Kind, a.Attempt,
				operationHoldRecovery), operationHoldRecheckInterval), nil
	default:
		// An untyped answer: done without an error is a success; done with
		// one cannot be told clean from dirty.
		switch {
		case !st.IsCompleted:
			return stepWaiting, r.operationInProgress(ctx, vm, a, operationPollInterval), nil
		case st.Error == "":
			outcome = infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded
		default:
			return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationOutcomeUnknown,
				fmt.Sprintf("the Provider reported %s attempt %d failed without saying whether anything was done; %s", a.Kind, a.Attempt,
					operationHoldRecovery), operationHoldRecheckInterval), nil
		}
	}
	if r.operationCrashed(ctx, crashAfterReply) {
		return stepWaiting, ctrl.Result{}, errOperationCrashed
	}
	return r.closeAttempt(ctx, vm, ia, a, outcome, st.OutcomeReason, answerID)
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
	if err := r.writeIdentityStatus(ctx, vm, w); err != nil {
		logger.Info("The Terminal marker was not written; the attempt is asked about again", "error", err.Error())
		return stepWaiting, ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
	}
	logger.Info("Operation attempt is over", "kind", a.Kind, "attempt", a.Attempt, "outcome", outcome, "reason", done.OutcomeReason)
	if r.operationCrashed(ctx, crashAfterTerminal) {
		return stepWaiting, ctrl.Result{}, errOperationCrashed
	}
	if err := r.deleteIntent(ctx, ia.providerCR.Namespace, a.OpID, vm.UID); err != nil {
		logger.Info("Could not remove the operation-intent Secret; the sweeper will", "error", err.Error())
	}
	switch {
	case outcome == infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded:
		if a.Kind == infravirtrigaudiov1beta1.OperationAttemptKindCreate {
			setPlacedCondition(vm, metav1.ConditionTrue, k8s.ReasonBound, fmt.Sprintf("VM is bound to host %s", a.HostID))
		}
		return stepSucceeded, ctrl.Result{}, nil
	case outcome == infravirtrigaudiov1beta1.OperationAttemptOutcomeFailedDirty:
		return stepHeld, r.holdOperation(ctx, vm, k8s.ReasonOperationFailedDirty, msgOperationFailedDirty, operationHoldRecheckInterval), nil
	}
	wait := attemptRetryWait(&done, r.now())
	msg := fmt.Sprintf("%s attempt %d was not performed (%s); attempt %d is sent in %s", done.Kind, done.Attempt, outcome,
		done.Attempt+1, wait.Round(time.Second))
	k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonOperationRetryBackoff, msg, vm.Generation)
	r.updateStatus(ctx, vm)
	return stepNotPerformed, ctrl.Result{RequeueAfter: wait}, nil
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

// gateCreateDrive maps the step of an in-flight Create the gate drove: a
// success records what follows a Create.
func (r *VirtualMachineReconciler) gateCreateDrive(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	ia identityAttempt, step attemptStep, res ctrl.Result, err error) (ctrl.Result, error) {
	if step == stepSucceeded {
		return r.afterCreateSucceeded(ctx, vm, ia), nil
	}
	return res, err
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
	if stateLost(vm) {
		if force {
			return r.releaseLeavingHypervisorVM(ctx, vm, vmLeftOperationHeld)
		}
		return r.holdOperation(ctx, vm, k8s.ReasonOperationStateLost, msgOperationStateLost, operationHoldRecheckInterval), nil
	}
	if a := inFlightAttempt(vm); a != nil {
		step, res, err := r.driveAttempt(ctx, vm, ia, a)
		return r.afterDeleteStep(ctx, vm, a.Kind, step, res, err, force)
	}
	if d := findAttempt(vm, contracts.OperationKindDelete); d != nil && d.Outcome == infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded {
		return r.removeFinalizer(ctx, vm)
	}
	if nothingCreated(vm) {
		log.FromContext(ctx).Info("No Create of this VM was ever performed; releasing it without a Delete")
		return r.removeFinalizer(ctx, vm)
	}
	step, res, err := r.startAttempt(ctx, vm, ia, contracts.OperationRequest{Kind: contracts.OperationKindDelete, VM: ref}, ref.HostID)
	return r.afterDeleteStep(ctx, vm, infravirtrigaudiov1beta1.OperationAttemptKindDelete, step, res, err, force)
}

// afterDeleteStep maps a step of an attempt of kind driven by identityDelete.
func (r *VirtualMachineReconciler) afterDeleteStep(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	kind infravirtrigaudiov1beta1.OperationAttemptKind, step attemptStep, res ctrl.Result, err error, force bool) (ctrl.Result, error) {
	switch {
	case err != nil:
		return res, err
	case step == stepSucceeded && kind == infravirtrigaudiov1beta1.OperationAttemptKindDelete:
		return r.removeFinalizer(ctx, vm)
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
