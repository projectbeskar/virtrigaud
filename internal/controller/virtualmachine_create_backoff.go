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

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// A Create the provider answered with a retryable VM_DISK_CHECK_FAILED (it
// could not verify that no other VM uses the VM's disk, so it did nothing) or
// a retryable VM_OPERATION_FAILED (the operation reached the host and failed
// there, e.g. a step the host cannot complete) used to be retried every 5 s,
// forever, outside the circuit breaker (which ignores both, by design): each
// retry may cost a scan of every disk on the host, or a full create attempt.
// Such a Create is now retried with the blocked-VM backoff (15 s doubling to
// 5 min), counted from when Ready became False/CreateBackoff.

// createBackoffMessage is the Ready (and, for a Create the provider did not
// perform, Provisioning) message of a Create retried with a backoff. It is
// constant; the provider's answer is in the manager log.
var createBackoffMessage = fmt.Sprintf("The provider could not complete this VM's Create: it could not verify that no other VM "+
	"uses its disk, or the operation failed on its host. It is retried with a backoff of up to %s; the manager log has "+
	"the provider's answer", blockedRetryMax)

// createRefusedMessage is the Ready and Provisioning message of a Create the
// provider refused with a non-retryable FailedPrecondition or
// PermissionDenied (createRefusedByProvider). It is constant; the provider's
// answer is in the Warning event recorded when the hold begins, and in the
// manager log.
var createRefusedMessage = fmt.Sprintf("The provider refused this VM's Create: a precondition only an operator can "+
	"satisfy is not met, or the Provider lacks a permission (the CreateRefused event has its answer). Nothing was "+
	"created. It is re-checked with a backoff of up to %s", blockedRetryMax)

// errReasonCreateBackoff is the metrics reason of a Create retried with a
// backoff.
const errReasonCreateBackoff = "provider-create-backoff"

// createNeedsBackoff reports whether a failed Create is one retried with the
// blocked-VM backoff: a retryable VM_DISK_CHECK_FAILED or VM_OPERATION_FAILED,
// or a refusal (createRefusedByProvider).
func createNeedsBackoff(err error) bool {
	return contracts.IsVMDiskCheckFailed(err) || hasVMOperationFailed(err) || createRefusedByProvider(err)
}

// createRefusedByProvider reports whether a failed Create is a provider
// refusal that retrying cannot change: an untyped FailedPrecondition (a
// precondition only an operator can satisfy; the typed ones — a disk in use,
// a running clone source — are handled as such) or PermissionDenied.
//
// It is treated as "nothing made" (createNothingDone), which relies on the
// providers answering FailedPrecondition only before creating anything: the
// vSphere provider's "imported disk not found ... run the VMMigration again"
// is returned before any clone or CreateVM task starts, and libvirt and
// Proxmox return their FailedPrecondition refusals from request checks. A
// provider that answers FailedPrecondition after a partial create must mark
// it otherwise (e.g. VM_OPERATION_FAILED, whose outcome is unknown).
func createRefusedByProvider(err error) bool {
	if contracts.IsVMDiskInUse(err) || contracts.IsVMSourceRunning(err) {
		return false
	}
	var pe *contracts.ProviderError
	if stderrors.As(err, &pe) {
		return false
	}
	st, ok := status.FromError(err)
	return ok && (st.Code() == codes.FailedPrecondition || st.Code() == codes.PermissionDenied)
}

// hasVMOperationFailed reports whether err carries the gRPC status of a
// per-VM operation that reached its host and failed there
// (contracts.VMOperationFailedReason in a google.rpc.ErrorInfo). The
// transport keeps the status in the chain of a retryable (Unavailable)
// answer.
func hasVMOperationFailed(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok &&
			info.GetReason() == contracts.VMOperationFailedReason && info.GetDomain() == contracts.ErrorInfoDomain {
			return true
		}
	}
	return false
}

// holdCreateBackoff handles a failed Create that createNeedsBackoff selects
// and reports whether it did: Ready=False/CreateBackoff with the constant
// createBackoffMessage — or CreateRefused with createRefusedMessage for a
// refusal (createRefusedByProvider) — and the same Provisioning, replacing
// the create marker, only when the provider made nothing and this is not a
// re-send (createOutcomeKnown: a refusal of a first Create; a
// VM_DISK_CHECK_FAILED or VM_OPERATION_FAILED Create, and any failure of a
// re-send, keeps the marker, its outcome unknown); a Warning event when the hold begins (for a
// refusal it carries the provider's sanitized answer), the status written
// only when it changes, and the next retry on blockedRetryBackoff from when
// Ready took the reason.
func (r *VirtualMachineReconciler) holdCreateBackoff(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	err error,
	attempt createAttempt,
) (ctrl.Result, bool) {
	if !createNeedsBackoff(err) {
		return ctrl.Result{}, false
	}
	reason, msg := k8s.ReasonCreateBackoff, createBackoffMessage
	if createRefusedByProvider(err) {
		reason, msg = k8s.ReasonCreateRefused, createRefusedMessage
	}
	persisted := vm.Status.DeepCopy()
	prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
	transition := prev == nil || prev.Reason != reason
	if transition {
		// The backoff counts from the start of this hold: a Ready that was
		// already False for another reason must not carry its older
		// LastTransitionTime into it.
		meta.RemoveStatusCondition(&vm.Status.Conditions, k8s.ConditionReady)
	}
	condTypes := []string{k8s.ConditionReady}
	if createOutcomeKnown(err, attempt) {
		condTypes = append(condTypes, k8s.ConditionProvisioning)
	}
	for _, condType := range condTypes {
		meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: vm.Generation,
		})
	}
	retry := blockedRetryBackoff(conditionSince(vm.Status.Conditions, k8s.ConditionReady))
	log.FromContext(ctx).Info("Provider could not complete the Create; retrying with a backoff",
		"retryAfter", retry.String(), "error", err.Error())
	metrics.RecordError(errReasonCreateBackoff, metrics.ComponentManager)
	if statusDiffers(persisted, vm) {
		r.updateStatus(ctx, vm)
	}
	if transition {
		event := msg
		if reason == k8s.ReasonCreateRefused {
			// The provider's own answer says what to fix; sanitized, once.
			event = msg + ". Provider answer: " + sanitizeProviderDetail(err)
		}
		r.recordEvent(vm, corev1.EventTypeWarning, reason, event)
	}
	return ctrl.Result{RequeueAfter: retry}, true
}
