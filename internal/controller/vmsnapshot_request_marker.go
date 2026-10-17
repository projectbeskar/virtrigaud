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

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A VMSnapshot's SnapshotCreate survives a manager restart (rc2 R6).
//
// Before the SnapshotCreate is sent, the VMSnapshot records SnapshotRequested
// (markSnapshotRequested; provider_request_marker.go), written with the
// optimistic lock of the resourceVersion read; nothing is sent if that write
// fails. The create's outcome — a task to poll, Ready, or Failed — is written
// with the marker removed. A VMSnapshot still in the initial phase that
// carries the marker (resolveSnapshotRequest) therefore had a create sent
// whose outcome was never recorded — lost to a restart, or the create failed
// without a definite answer (a deadline, a dropped connection; a definite
// failure fails the snapshot and removes the marker):
//
//   - through a clustered (routed) provider the create is sent again: it
//     adopts the snapshot an earlier call made for this VMSnapshot's uid
//     (request_token), and answers "retry" while that call still runs; at
//     most once a minute and snapshotRoutedResendMax times, then held;
//   - through any single-host provider it is not: vSphere would make a second
//     snapshot of the same name, libvirt and Proxmox refuse the name without
//     returning the earlier snapshot, the mock makes a second one. The
//     VMSnapshot waits (Ready=False/SnapshotOutcomeUnknown) for an operator to
//     check the VM, remove a snapshot the create left, and confirm with
//     virtrigaud.io/operation-outcome=not-performed@<attempt id>.

const (
	// snapshotConditionRequested is the VMSnapshot condition recording that a
	// SnapshotCreate was requested and its outcome is not recorded yet.
	snapshotConditionRequested = "SnapshotRequested"
	// snapshotReasonOutcomeUnknown is the Ready reason (and Warning event
	// reason) of a VMSnapshot whose SnapshotCreate's outcome was not recorded.
	snapshotReasonOutcomeUnknown = "SnapshotOutcomeUnknown"
)

// snapshotRequestedMessage is the message of snapshotConditionRequested.
const snapshotRequestedMessage = "A provider SnapshotCreate was requested for this VMSnapshot (the attempt id is this " +
	"condition's lastTransitionTime); its outcome is recorded here once the provider answers"

// snapshotRoutedResendMax caps the automatic re-sends of a routed create
// whose outcome is unknown (about ten minutes at one a minute).
const snapshotRoutedResendMax = 10

// snapshotOperatorActionFormat is the shared tail of the operator messages.
const snapshotOperatorActionFormat = "Check the VM for the snapshot named in the %s event and remove it, then annotate " +
	"the VMSnapshot %s=%s<attempt id>, the attempt id being the lastTransitionTime of the %s condition, to take it " +
	"again; or delete the VMSnapshot (a snapshot the create left is then yours to remove). Re-checked every %s"

// snapshotOutcomeUnknownMessage is the Ready message of a VMSnapshot held for
// an operator. It is constant; the snapshot name and attempt id are in the
// SnapshotOutcomeUnknown event.
var snapshotOutcomeUnknownMessage = fmt.Sprintf("A provider SnapshotCreate was sent for this VMSnapshot but its outcome "+
	"is unknown (its result was lost to a manager restart or leader change, or the create failed without a definite "+
	"answer), and this provider cannot be asked again without risking a second snapshot. "+snapshotOperatorActionFormat,
	snapshotReasonOutcomeUnknown, operationOutcomeAnnotation, operationOutcomeNotPerformedPrefix,
	snapshotConditionRequested, requestMarkerRecheckInterval)

// snapshotResendsExhaustedMessage is the Ready message of a routed create
// re-sent snapshotRoutedResendMax times without a recorded outcome.
var snapshotResendsExhaustedMessage = fmt.Sprintf("The SnapshotCreate was sent again automatically %d times and none "+
	"of them reached a recorded outcome (the provider's answers are in the manager log), so it is not sent again "+
	"without an operator. "+snapshotOperatorActionFormat, snapshotRoutedResendMax,
	snapshotReasonOutcomeUnknown, operationOutcomeAnnotation, operationOutcomeNotPerformedPrefix,
	snapshotConditionRequested, requestMarkerRecheckInterval)

// snapshotRequested reports whether snapshot carries the SnapshotRequested
// marker.
func snapshotRequested(snapshot *infrav1beta1.VMSnapshot) bool {
	return requestMarker(snapshot.Status.Conditions, snapshotConditionRequested) != nil
}

// clearSnapshotRequest removes the SnapshotRequested marker (in memory; the
// caller's status write persists it), and the Ready hold it caused.
func clearSnapshotRequest(snapshot *infrav1beta1.VMSnapshot) {
	meta.RemoveStatusCondition(&snapshot.Status.Conditions, snapshotConditionRequested)
	clearSnapshotOutcomeUnknownHold(snapshot)
}

// clearSnapshotOutcomeUnknownHold removes the Ready hold of an unknown
// outcome, keeping the marker.
func clearSnapshotOutcomeUnknownHold(snapshot *infrav1beta1.VMSnapshot) {
	if c := meta.FindStatusCondition(snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionReady); c != nil &&
		c.Reason == snapshotReasonOutcomeUnknown {
		meta.RemoveStatusCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionReady)
	}
}

// markSnapshotRequested durably records, right before the SnapshotCreate is
// sent, that it was requested: a new marker (a strictly later attempt id)
// written over snapshot as read (confirmSnapshotNotCreated has just confirmed
// it is the live object), with the optimistic lock of its resourceVersion; an
// automatic re-send counts itself in the marker. An operator's
// operationOutcomeAnnotation is removed first. sent=false means the record
// could not be written and nothing may be sent: res is the result to return.
func (r *VMSnapshotReconciler) markSnapshotRequested(ctx context.Context, snapshot *infrav1beta1.VMSnapshot) (ctrl.Result, bool) {
	logger := logging.FromContext(ctx)
	retry := ctrl.Result{RequeueAfter: snapshotStaleReadRetryInterval}
	resends := nextRequestResends(requestMarker(snapshot.Status.Conditions, snapshotConditionRequested),
		operationOutcomeConfirmed(snapshot.Annotations, requestMarkerAttemptID(snapshot.Status.Conditions, snapshotConditionRequested)))
	if _, ok := snapshot.Annotations[operationOutcomeAnnotation]; ok {
		updated := snapshot.DeepCopy()
		delete(updated.Annotations, operationOutcomeAnnotation)
		if err := r.Update(ctx, updated); err != nil {
			logger.Info("Cannot remove the operation-outcome annotation before the SnapshotCreate; not creating",
				"error", err.Error())
			return retry, false
		}
		snapshot.ResourceVersion = updated.ResourceVersion
		snapshot.Annotations = updated.Annotations
	}
	clearSnapshotOutcomeUnknownHold(snapshot)
	setRequestMarker(&snapshot.Status.Conditions, snapshotConditionRequested, snapshotConditionRequested,
		withResendCount(snapshotRequestedMessage, resends), snapshot.Generation, time.Now())
	if err := r.Status().Update(ctx, snapshot); err != nil {
		logger.Info("Cannot record the requested SnapshotCreate on the VMSnapshot; not creating", "error", err.Error())
		return retry, false
	}
	return ctrl.Result{}, true
}

// resolveSnapshotRequest decides a VMSnapshot in the initial phase that
// carries the SnapshotRequested marker. held reports that createSnapshot must
// return res, err; otherwise it continues and sends the SnapshotCreate.
//
//   - No marker: continue. A recorded snapshot id: the marker is stale.
//   - The cached VMSnapshot is not the live one: retry from a fresh read.
//   - An operator confirmed this attempt: continue (Warning event, audit; the
//     annotation's field managers are logged).
//   - The Provider or the VM's address cannot be read yet: retried after
//     requestMarkerRecheckInterval, with no hold and no event.
//   - A routed (clustered) create: continue — the provider adopts the earlier
//     call's snapshot by request token — paced from the last request, and
//     held after snapshotRoutedResendMax re-sends.
//   - Otherwise: held (Ready=False/SnapshotOutcomeUnknown).
func (r *VMSnapshotReconciler) resolveSnapshotRequest(ctx context.Context, snapshot *infrav1beta1.VMSnapshot,
	vm *infrav1beta1.VirtualMachine) (bool, ctrl.Result, error) {
	marker := requestMarker(snapshot.Status.Conditions, snapshotConditionRequested)
	if marker == nil {
		return false, ctrl.Result{}, nil
	}
	if snapshot.Status.SnapshotID != "" {
		clearSnapshotRequest(snapshot)
		return false, ctrl.Result{}, nil
	}
	logger := logging.FromContext(ctx)

	live := &infrav1beta1.VMSnapshot{}
	if err := r.liveReader().Get(ctx, client.ObjectKeyFromObject(snapshot), live); err != nil {
		if apierrors.IsNotFound(err) {
			return true, ctrl.Result{}, nil
		}
		return true, ctrl.Result{}, fmt.Errorf("re-read VMSnapshot %s/%s to decide its SnapshotCreate: %w",
			snapshot.Namespace, snapshot.Name, err)
	}
	if live.UID != snapshot.UID || live.ResourceVersion != snapshot.ResourceVersion {
		logger.Info("The cached VMSnapshot is not the live one; deciding the requested create from a fresh read",
			"cachedResourceVersion", snapshot.ResourceVersion, "liveResourceVersion", live.ResourceVersion)
		return true, ctrl.Result{RequeueAfter: snapshotStaleReadRetryInterval}, nil
	}

	attempt := requestMarkerAttemptID(snapshot.Status.Conditions, snapshotConditionRequested)
	if operationOutcomeConfirmed(snapshot.Annotations, attempt) {
		logger.Info("An operator confirmed that the requested SnapshotCreate left nothing behind; sending it again",
			"attempt", attempt, "annotationManagers", operationOutcomeManagers(snapshot))
		r.Recorder.Event(snapshot, corev1.EventTypeWarning, eventReasonOperationOutcomeConfirmed,
			fmt.Sprintf("An operator confirmed that the SnapshotCreate of attempt %s left nothing behind; it is sent again", attempt))
		return false, ctrl.Result{}, nil
	}

	provider, err := getVMProvider(ctx, r.Client, vm)
	var ref contracts.VMRef
	if err == nil {
		ref, err = vmRefFor(vm, provider)
	}
	if err != nil {
		logger.Info("Cannot tell yet whether the requested SnapshotCreate may be sent again; checking again",
			"error", err.Error(), "retryAfter", requestMarkerRecheckInterval.String())
		return true, ctrl.Result{RequeueAfter: requestMarkerRecheckInterval}, nil
	}
	if ref.HostID == "" {
		return true, r.holdSnapshotOutcomeUnknown(ctx, snapshot, attempt, snapshotOutcomeUnknownMessage), nil
	}
	// Routed: the provider adopts the snapshot an earlier call made for this
	// VMSnapshot's uid. Each re-send writes a new marker; its watch event must
	// not re-send at once, and the re-sends are capped.
	if requestResends(marker) >= snapshotRoutedResendMax {
		return true, r.holdSnapshotOutcomeUnknown(ctx, snapshot, attempt, snapshotResendsExhaustedMessage), nil
	}
	if wait := time.Until(marker.LastTransitionTime.Add(requestMarkerRecheckInterval)); wait > 0 {
		return true, ctrl.Result{RequeueAfter: max(wait, snapshotStaleReadRetryInterval)}, nil
	}
	logger.Info("The requested SnapshotCreate's outcome was not recorded; sending it again (routed: the provider "+
		"adopts the snapshot made for this request token)", "attempt", attempt)
	return false, ctrl.Result{}, nil
}

// holdSnapshotOutcomeUnknown keeps snapshot in the initial phase with
// Ready=False/SnapshotOutcomeUnknown and message (snapshotOutcomeUnknownMessage
// or snapshotResendsExhaustedMessage, both constant), records a Warning event
// naming the snapshot and the attempt when the hold begins, and re-checks
// after requestMarkerRecheckInterval. A repeated hold writes nothing.
func (r *VMSnapshotReconciler) holdSnapshotOutcomeUnknown(ctx context.Context, snapshot *infrav1beta1.VMSnapshot,
	attempt, message string) ctrl.Result {
	res := ctrl.Result{RequeueAfter: requestMarkerRecheckInterval}
	prev := meta.FindStatusCondition(snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionReady)
	if prev != nil && prev.Status == metav1.ConditionFalse && prev.Reason == snapshotReasonOutcomeUnknown &&
		prev.Message == message && snapshot.Status.Message == message {
		return res
	}
	logging.FromContext(ctx).Info("The requested SnapshotCreate's outcome was not recorded; holding the snapshot",
		"attempt", attempt)
	metrics.RecordError(errReasonProviderOutcomeUnknown, metrics.ComponentManager)
	meta.SetStatusCondition(&snapshot.Status.Conditions, metav1.Condition{
		Type:               infrav1beta1.VMSnapshotConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             snapshotReasonOutcomeUnknown,
		Message:            message,
		ObservedGeneration: snapshot.Generation,
	})
	snapshot.Status.Message = message
	if err := r.updateStatus(ctx, snapshot); err != nil {
		return ctrl.Result{RequeueAfter: snapshotStaleReadRetryInterval}
	}
	name := r.buildSnapshotCreateRequest(snapshot, contracts.VMRef{}).NameHint
	r.Recorder.Event(snapshot, corev1.EventTypeWarning, snapshotReasonOutcomeUnknown,
		fmt.Sprintf("The result of the SnapshotCreate requested at %s (attempt id) was not recorded; the snapshot it asked "+
			"for is named %q", attempt, name))
	return res
}
