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
	"slices"
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
	"github.com/projectbeskar/virtrigaud/internal/util/k8s"
)

// A VMMigration's provider calls survive a manager restart (rc2 R6).
//
// Before SnapshotCreate, ExportDisk or ImportDisk is sent, the VMMigration
// records SnapshotRequested / ExportRequested / ImportRequested
// (markMigrationRequested; provider_request_marker.go). The call's result is
// written with it removed (the result appliers, markSnapshotCompleteForExport,
// markExportComplete, markImportComplete), as is a definite failure. A phase
// handler that finds the marker without the result (resolveMigrationRequest)
// knows the call may have run: the result was lost to a manager restart or
// leader change after the call, or while a result whose write failed was held
// in memory (pendingMigrationResult) — or the call failed without a definite
// outcome (a deadline, a transport failure after the request was written;
// afterUnknownMigrationCall keeps the marker and never fails the migration).
// It then:
//
//   - SnapshotCreate: sends it again only through a clustered (routed)
//     provider, which adopts the snapshot an earlier call made for the same
//     request token, and only for the same migration attempt (the token
//     carries the attempt). vSphere makes a second snapshot of the same name,
//     single-host libvirt and Proxmox refuse the name without returning the
//     earlier snapshot, the mock makes a second one: the migration is held
//     (Ready=False/SnapshotOutcomeUnknown).
//   - ExportDisk: the staged object's location is the manager's
//     (destination_url), so a re-send replaces it — where the source provider
//     reports the migration's staging backend in resend_safe_export_backends
//     (no earlier call can still be writing it once its deadline passed, and
//     nothing else is left). It is sent again once migrationResendSettleTime
//     has passed since the marker; otherwise held (ExportOutcomeUnknown).
//   - ImportDisk: the landed disk's name is derived from the target VM, so a
//     re-send replaces it (or is refused because a VM uses it) — where the
//     target provider reports the backend in resend_safe_import_backends;
//     after migrationResendSettleTime, otherwise held (ImportOutcomeUnknown).
//
// Automatic re-sends are capped (maxResends, counted in the marker): past the
// cap the migration is held for an operator. A cause that cannot be decided
// yet (the provider, the source VM or the capabilities cannot be read) is
// retried quietly, with no hold.
//
// A held migration names the operator's action in its Ready message: check
// the provider for what the call made, remove it, and confirm with
// virtrigaud.io/operation-outcome=not-performed@<attempt id> (the marker's
// lastTransitionTime), which sends the call again.

const (
	// migrationConditionSnapshotRequested is the VMMigration condition
	// recording that a SnapshotCreate was requested and its result is not
	// recorded yet.
	migrationConditionSnapshotRequested = "SnapshotRequested"
	// migrationConditionExportRequested records a requested ExportDisk.
	migrationConditionExportRequested = "ExportRequested"
	// migrationConditionImportRequested records a requested ImportDisk.
	migrationConditionImportRequested = "ImportRequested"

	// migrationReasonSnapshotOutcomeUnknown is the Ready reason (and Warning
	// event reason) of a migration whose SnapshotCreate's outcome was not
	// recorded.
	migrationReasonSnapshotOutcomeUnknown = "SnapshotOutcomeUnknown"
	// migrationReasonExportOutcomeUnknown is the Ready reason (and Warning
	// event reason) of a migration whose ExportDisk's outcome was not recorded.
	migrationReasonExportOutcomeUnknown = "ExportOutcomeUnknown"
	// migrationReasonImportOutcomeUnknown is the Ready reason (and Warning
	// event reason) of a migration whose ImportDisk's outcome was not recorded.
	migrationReasonImportOutcomeUnknown = "ImportOutcomeUnknown"
)

// migrationResendSettleTime is how long after an ExportDisk or ImportDisk was
// requested it may be sent again: longer than the manager's 30-minute
// deadline on those calls (internal/transport/grpc), which the provider
// receives as its own deadline, so the earlier call has ended — and on a
// provider that reports the backend re-send safe, stopped writing — before
// the re-send starts.
const migrationResendSettleTime = 35 * time.Minute

// migrationRequestKind describes the marker and hold of one provider call.
type migrationRequestKind struct {
	// call names the RPC in logs and events.
	call string
	// condType is the marker's condition type (and reason).
	condType string
	// unknownReason is the Ready reason of a migration held for the call's
	// unknown outcome.
	unknownReason string
	// requestedMessage is the marker's message.
	requestedMessage string
	// resendMessage is the Ready message while the call waits to be sent
	// again automatically.
	resendMessage string
	// operatorMessage is the Ready message while the call waits for an
	// operator.
	operatorMessage string
	// exhaustedMessage is the Ready message once maxResends automatic
	// re-sends reached no recorded outcome.
	exhaustedMessage string
	// maxResends caps the automatic re-sends of one call.
	maxResends int
}

// migrationOperatorActionFormat is the shared tail of the operator messages:
// the marker's condition type, the annotation, its prefix and the re-check.
const migrationOperatorActionFormat = " Then annotate the VMMigration %s=%s<attempt id>, the attempt id being the " +
	"lastTransitionTime of the %s condition, to send it again; or delete the VMMigration (anything the call made " +
	"that the migration did not record is left for you to remove). Re-checked every %s"

// Caps on automatic re-sends (migrationRequestKind.maxResends).
const (
	// migrationSnapshotResendMax caps the routed SnapshotCreate re-sends:
	// about ten minutes at one a minute.
	migrationSnapshotResendMax = 10
	// migrationDiskResendMax caps the ExportDisk / ImportDisk re-sends: each
	// is a full disk copy, 35 minutes after the last.
	migrationDiskResendMax = 3
)

// migrationOutcomeUnknownCause is the shared head of the hold messages.
const migrationOutcomeUnknownCause = "its outcome is unknown: its result was lost to a manager restart or leader change, " +
	"or the call failed without a definite answer (a deadline, a dropped connection)"

// migrationExhaustedFormat is the head of the exhausted messages: the call
// and the cap.
const migrationExhaustedFormat = "The %s was sent again automatically %d times and none of them reached a recorded " +
	"outcome, so it is not sent again without an operator. Check the provider (its answers are in the manager log) and "

// migrationRequests are the markers of the three calls.
var migrationRequests = map[migrationLongOp]migrationRequestKind{
	longOpSnapshot: {
		call:          "SnapshotCreate",
		condType:      migrationConditionSnapshotRequested,
		unknownReason: migrationReasonSnapshotOutcomeUnknown,
		requestedMessage: "A SnapshotCreate of the source VM was requested (the attempt id is this condition's " +
			"lastTransitionTime); the snapshot is recorded here once the provider answers",
		resendMessage: "The source snapshot's SnapshotCreate was sent but " + migrationOutcomeUnknownCause +
			". It is sent again, at most once a minute: the provider returns the snapshot an earlier call made for this " +
			"migration attempt",
		operatorMessage: fmt.Sprintf("The source snapshot's SnapshotCreate was sent but "+migrationOutcomeUnknownCause+
			", and the provider cannot be asked again without risking a second snapshot of the live source VM. Check the "+
			"source VM for the snapshot named in the %s event and remove it."+
			migrationOperatorActionFormat, migrationReasonSnapshotOutcomeUnknown, operationOutcomeAnnotation,
			operationOutcomeNotPerformedPrefix, migrationConditionSnapshotRequested, requestMarkerRecheckInterval),
		exhaustedMessage: fmt.Sprintf(migrationExhaustedFormat+"the source VM for the snapshot named in the %s event."+
			migrationOperatorActionFormat, "SnapshotCreate", migrationSnapshotResendMax, migrationReasonSnapshotOutcomeUnknown,
			operationOutcomeAnnotation, operationOutcomeNotPerformedPrefix, migrationConditionSnapshotRequested,
			requestMarkerRecheckInterval),
		maxResends: migrationSnapshotResendMax,
	},
	longOpExport: {
		call:          "ExportDisk",
		condType:      migrationConditionExportRequested,
		unknownReason: migrationReasonExportOutcomeUnknown,
		requestedMessage: "An ExportDisk of the source disk was requested (the attempt id is this condition's " +
			"lastTransitionTime); its result is recorded here once the provider answers",
		resendMessage: fmt.Sprintf("The ExportDisk was sent but "+migrationOutcomeUnknownCause+". It is sent again "+
			"%s after it was requested (after the earlier call's deadline): the source provider replaces the staged "+
			"object", migrationResendSettleTime),
		operatorMessage: fmt.Sprintf("The ExportDisk was sent but "+migrationOutcomeUnknownCause+", and the source "+
			"provider does not report that a re-sent export is safe on this staging backend "+
			"(resend_safe_export_backends): an earlier export could still be writing the staged object. Make sure no "+
			"export of this migration is still running on the source provider."+
			migrationOperatorActionFormat, operationOutcomeAnnotation, operationOutcomeNotPerformedPrefix,
			migrationConditionExportRequested, requestMarkerRecheckInterval),
		exhaustedMessage: fmt.Sprintf(migrationExhaustedFormat+"that no export of this migration is still running."+
			migrationOperatorActionFormat, "ExportDisk", migrationDiskResendMax, operationOutcomeAnnotation,
			operationOutcomeNotPerformedPrefix, migrationConditionExportRequested, requestMarkerRecheckInterval),
		maxResends: migrationDiskResendMax,
	},
	longOpImport: {
		call:          "ImportDisk",
		condType:      migrationConditionImportRequested,
		unknownReason: migrationReasonImportOutcomeUnknown,
		requestedMessage: "An ImportDisk to the target provider was requested (the attempt id is this condition's " +
			"lastTransitionTime); its result is recorded here once the provider answers",
		resendMessage: fmt.Sprintf("The ImportDisk was sent but "+migrationOutcomeUnknownCause+". It is sent again "+
			"%s after it was requested (after the earlier call's deadline): the target provider lands the disk at the "+
			"same name, replacing what an earlier call landed", migrationResendSettleTime),
		operatorMessage: fmt.Sprintf("The ImportDisk was sent but "+migrationOutcomeUnknownCause+", and the target "+
			"provider does not report that a re-sent import is safe on this staging backend "+
			"(resend_safe_import_backends): it could land a second disk. Check the target provider for the disk landed "+
			"for the target VirtualMachine (named for it, ending in -migrated) and remove it."+
			migrationOperatorActionFormat, operationOutcomeAnnotation, operationOutcomeNotPerformedPrefix,
			migrationConditionImportRequested, requestMarkerRecheckInterval),
		exhaustedMessage: fmt.Sprintf(migrationExhaustedFormat+"the target provider for the disk landed for the target "+
			"VirtualMachine (ending in -migrated)."+migrationOperatorActionFormat, "ImportDisk", migrationDiskResendMax,
			operationOutcomeAnnotation, operationOutcomeNotPerformedPrefix, migrationConditionImportRequested,
			requestMarkerRecheckInterval),
		maxResends: migrationDiskResendMax,
	},
}

// migrationResultRecorded reports whether migration's status records the
// result of op (or, for ExportDisk / ImportDisk, that it completed).
func migrationResultRecorded(m *infrav1beta1.VMMigration, op migrationLongOp) bool {
	switch op {
	case longOpSnapshot:
		return m.Status.SnapshotID != ""
	case longOpExport:
		return m.Status.ExportID != "" || k8s.IsConditionTrue(m.Status.Conditions, infrav1beta1.VMMigrationConditionExporting)
	case longOpImport:
		return m.Status.ImportID != "" || k8s.IsConditionTrue(m.Status.Conditions, infrav1beta1.VMMigrationConditionImporting)
	}
	return false
}

// migrationRequested reports whether migration carries op's marker.
func migrationRequested(m *infrav1beta1.VMMigration, op migrationLongOp) bool {
	return requestMarker(m.Status.Conditions, migrationRequests[op].condType) != nil
}

// clearMigrationRequest removes op's marker from migration (in memory; the
// caller's status write persists it), and the Ready hold it caused.
func clearMigrationRequest(m *infrav1beta1.VMMigration, op migrationLongOp) {
	meta.RemoveStatusCondition(&m.Status.Conditions, migrationRequests[op].condType)
	clearMigrationOutcomeUnknownHold(m, op)
}

// clearMigrationOutcomeUnknownHold removes the Ready hold of op's unknown
// outcome, keeping the marker.
func clearMigrationOutcomeUnknownHold(m *infrav1beta1.VMMigration, op migrationLongOp) {
	if c := meta.FindStatusCondition(m.Status.Conditions, infrav1beta1.VMMigrationConditionReady); c != nil &&
		c.Reason == migrationRequests[op].unknownReason {
		meta.RemoveStatusCondition(&m.Status.Conditions, infrav1beta1.VMMigrationConditionReady)
	}
}

// clearingMigrationRequest returns apply followed by clearMigrationRequest of
// op: a result applier that also removes the call's marker in the same write.
func clearingMigrationRequest(op migrationLongOp, apply func(*infrav1beta1.VMMigration)) func(*infrav1beta1.VMMigration) {
	return func(m *infrav1beta1.VMMigration) {
		apply(m)
		clearMigrationRequest(m, op)
	}
}

// markMigrationRequested durably records, right before op is sent, that it
// was requested: a new marker (a strictly later attempt id) written over
// migration as read, with the optimistic lock of its resourceVersion; an
// automatic re-send counts itself in the marker (nextRequestResends). An
// operator's operationOutcomeAnnotation is removed first, so it never
// authorizes a later attempt. The returned callAttempt says whether a marker was
// already on the object (an earlier call whose outcome was never recorded) and
// whether an operator confirmed it (providerCallOutcomeKnownFor). Any error means nothing may be sent; a Conflict
// is a status contention (Reconcile requeues). On success migration carries
// the written status and resourceVersion.
func (r *VMMigrationReconciler) markMigrationRequested(ctx context.Context, migration *infrav1beta1.VMMigration, op migrationLongOp) (callAttempt, error) {
	kind := migrationRequests[op]
	prev := requestMarker(migration.Status.Conditions, kind.condType)
	confirmed := operationOutcomeConfirmed(migration.Annotations, requestMarkerAttemptID(migration.Status.Conditions, kind.condType))
	attempt := newCallAttempt(prev, confirmed)
	resends := nextRequestResends(prev, operationOutcomeConfirmed(migration.Annotations,
		requestMarkerAttemptID(migration.Status.Conditions, kind.condType)))
	if _, ok := migration.Annotations[operationOutcomeAnnotation]; ok {
		updated := migration.DeepCopy()
		delete(updated.Annotations, operationOutcomeAnnotation)
		if err := r.Update(ctx, updated); err != nil {
			return attempt, fmt.Errorf("remove the %s annotation of VMMigration %s/%s before %s: %w", operationOutcomeAnnotation,
				migration.Namespace, migration.Name, migrationRequests[op].call, wrapMigrationStatusConflict(err))
		}
		migration.ResourceVersion = updated.ResourceVersion
		migration.Annotations = updated.Annotations
	}
	clearMigrationOutcomeUnknownHold(migration, op)
	setRequestMarker(&migration.Status.Conditions, kind.condType, kind.condType,
		withResendCount(kind.requestedMessage, resends), migration.Generation, time.Now())
	if err := r.updateStatus(ctx, migration); err != nil {
		logging.FromContext(ctx).Info("Cannot record the requested provider call on the VMMigration; not sending it",
			"call", kind.call)
		return attempt, err
	}
	return attempt, nil
}

// resolveMigrationRequest decides a phase whose call op carries its marker
// without a recorded result. held reports that the phase must stop here and
// return res, err; otherwise the phase continues and sends op (again).
//
//   - No marker, a recorded result, or a call this manager process issued
//     (the in-memory guard of ExportDisk / ImportDisk): continue.
//   - The cached VMMigration is not the live one: retry shortly from a fresh
//     read — a cache that does not show the result yet is not an unknown
//     outcome.
//   - An operator confirmed this attempt (operationOutcomeAnnotation):
//     continue (audited with a Warning event; the annotation's field
//     managers are logged).
//   - maxResends automatic re-sends reached no outcome: held for an operator
//     (exhaustedMessage).
//   - It cannot be told yet whether the call may be sent again (the source
//     VM, a Provider or its capabilities cannot be read): retried after
//     requestMarkerRecheckInterval, with no hold and no event.
//   - The call may be sent again (migrationResendAllowed): continue once its
//     settle time has passed, held until then.
//   - Otherwise: held for an operator (Ready=False/<call>OutcomeUnknown).
func (r *VMMigrationReconciler) resolveMigrationRequest(ctx context.Context, migration *infrav1beta1.VMMigration,
	op migrationLongOp) (bool, ctrl.Result, error) {
	kind := migrationRequests[op]
	marker := requestMarker(migration.Status.Conditions, kind.condType)
	if marker == nil {
		return false, ctrl.Result{}, nil
	}
	if migrationResultRecorded(migration, op) {
		clearMigrationRequest(migration, op)
		return false, ctrl.Result{}, nil
	}
	if op != longOpSnapshot && r.longOpAlreadyStarted(migration, op) {
		return false, ctrl.Result{}, nil
	}
	logger := logging.FromContext(ctx).WithValues("call", kind.call)

	live := &infrav1beta1.VMMigration{}
	if err := r.liveReader().Get(ctx, client.ObjectKeyFromObject(migration), live); err != nil {
		if apierrors.IsNotFound(err) {
			return true, ctrl.Result{}, nil
		}
		return true, ctrl.Result{}, fmt.Errorf("re-read VMMigration %s/%s to decide its %s: %w",
			migration.Namespace, migration.Name, kind.call, err)
	}
	if live.UID != migration.UID || live.ResourceVersion != migration.ResourceVersion {
		logger.Info("The cached VMMigration is not the live one; deciding the requested call from a fresh read",
			"cachedResourceVersion", migration.ResourceVersion, "liveResourceVersion", live.ResourceVersion)
		return true, ctrl.Result{RequeueAfter: migrationStaleReadRetryInterval}, nil
	}

	attempt := requestMarkerAttemptID(migration.Status.Conditions, kind.condType)
	if operationOutcomeConfirmed(migration.Annotations, attempt) {
		logger.Info("An operator confirmed that the requested call left nothing behind; sending it again", "attempt", attempt,
			"annotationManagers", operationOutcomeManagers(migration))
		r.Recorder.Event(migration, corev1.EventTypeWarning, eventReasonOperationOutcomeConfirmed,
			fmt.Sprintf("An operator confirmed that the %s of attempt %s left nothing behind; it is sent again", kind.call, attempt))
		return false, ctrl.Result{}, nil
	}

	if requestResends(marker) >= kind.maxResends {
		res, err := r.holdMigrationOutcomeUnknown(ctx, migration, op, attempt, kind.exhaustedMessage, requestMarkerRecheckInterval)
		return true, res, err
	}
	resend, wait, err := r.migrationResendAllowed(ctx, migration, op, marker)
	if err != nil {
		logger.Info("Cannot tell yet whether the requested call may be sent again; checking again", "error", err.Error(),
			"retryAfter", requestMarkerRecheckInterval.String())
		return true, ctrl.Result{RequeueAfter: requestMarkerRecheckInterval}, nil
	}
	if resend && wait <= 0 {
		logger.Info("The requested call's outcome was not recorded; sending it again (safe for this provider and call)",
			"attempt", attempt)
		return false, ctrl.Result{}, nil
	}
	message, requeue := kind.operatorMessage, requestMarkerRecheckInterval
	if resend {
		message, requeue = kind.resendMessage, wait
	}
	res, err := r.holdMigrationOutcomeUnknown(ctx, migration, op, attempt, message, requeue)
	return true, res, err
}

// afterUnknownMigrationCall handles op's failure cause that leaves its
// outcome unknown (!providerCallOutcomeKnown): the earlier call may still be
// running, so the marker is kept and the migration is never failed for it —
// it is decided like a lost result (resolveMigrationRequest: re-sent where
// safe after the settle time, held otherwise). Nothing is sent in this
// reconcile.
func (r *VMMigrationReconciler) afterUnknownMigrationCall(ctx context.Context, migration *infrav1beta1.VMMigration,
	op migrationLongOp, cause error) (ctrl.Result, error) {
	logging.FromContext(ctx).Error(cause, "The provider call failed without a definite outcome; keeping its marker",
		"call", migrationRequests[op].call)
	held, res, err := r.resolveMigrationRequest(ctx, migration, op)
	if held {
		return res, err
	}
	return ctrl.Result{RequeueAfter: migrationStaleReadRetryInterval}, nil
}

// migrationResendAllowed reports whether op, requested with marker and with
// no recorded outcome, may be sent again, and how long it must still wait.
// An error means it could not be told (the call is not sent).
func (r *VMMigrationReconciler) migrationResendAllowed(ctx context.Context, migration *infrav1beta1.VMMigration,
	op migrationLongOp, marker *metav1.Condition) (bool, time.Duration, error) {
	switch op {
	case longOpSnapshot:
		// The request token carries the attempt: a snapshot requested by an
		// earlier attempt is never adopted by this one's.
		if migration.Status.LastRetryTime != nil && marker.LastTransitionTime.Before(migration.Status.LastRetryTime) {
			return false, 0, nil
		}
		sourceVM, err := r.getSourceVM(ctx, migration)
		if err != nil {
			return false, 0, err
		}
		sourceProvider, err := r.sourceProviderFor(ctx, migration, sourceVM)
		if err != nil {
			return false, 0, err
		}
		ref, err := vmRefFor(sourceVM, sourceProvider)
		if err != nil {
			return false, 0, err
		}
		// Only a routed (clustered) SnapshotCreate adopts the snapshot an
		// earlier call made for the same request token. A re-send is paced
		// from the last request (each re-send writes a new marker, whose
		// watch event must not re-send at once).
		if ref.HostID == "" {
			return false, 0, nil
		}
		return true, time.Until(marker.LastTransitionTime.Add(requestMarkerRecheckInterval)), nil
	case longOpExport, longOpImport:
		var provider *infrav1beta1.Provider
		var err error
		if op == longOpExport {
			var sourceVM *infrav1beta1.VirtualMachine
			if sourceVM, err = r.getSourceVM(ctx, migration); err != nil {
				return false, 0, err
			}
			provider, err = r.sourceProviderFor(ctx, migration, sourceVM)
		} else {
			provider, err = r.targetProviderFor(ctx, r.Client, migration)
		}
		if err != nil {
			return false, 0, err
		}
		instance, err := r.getProviderInstance(ctx, provider)
		if err != nil {
			return false, 0, err
		}
		reporter, ok := instance.(contracts.CapabilityReporter)
		if !ok {
			return false, 0, nil
		}
		caps, err := reporter.GetCapabilities(ctx)
		if err != nil {
			return false, 0, fmt.Errorf("read the provider's capabilities (resend_safe_*_backends): %w", err)
		}
		safe := caps.ResendSafeExportBackends
		if op == longOpImport {
			safe = caps.ResendSafeImportBackends
		}
		if !slices.Contains(safe, migrationBackendType(migration)) {
			return false, 0, nil
		}
		return true, time.Until(marker.LastTransitionTime.Add(migrationResendSettleTime)), nil
	}
	return false, 0, nil
}

// holdMigrationOutcomeUnknown keeps migration in its phase with
// Ready=False/<call>OutcomeUnknown and message (constant per call and
// decision), records a Warning event naming the attempt when the hold begins
// or its message changes, and re-checks after requeue. A repeated hold
// writes nothing.
func (r *VMMigrationReconciler) holdMigrationOutcomeUnknown(ctx context.Context, migration *infrav1beta1.VMMigration,
	op migrationLongOp, attempt, message string, requeue time.Duration) (ctrl.Result, error) {
	kind := migrationRequests[op]
	requeue = max(requeue, migrationStaleReadRetryInterval)
	prev := meta.FindStatusCondition(migration.Status.Conditions, infrav1beta1.VMMigrationConditionReady)
	if prev != nil && prev.Status == metav1.ConditionFalse && prev.Reason == kind.unknownReason && prev.Message == message &&
		migration.Status.Message == message {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	logging.FromContext(ctx).Info("The requested provider call's outcome was not recorded; holding the migration",
		"call", kind.call, "attempt", attempt, "retryAfter", requeue.String())
	metrics.RecordError(errReasonProviderOutcomeUnknown, metrics.ComponentManager)
	meta.SetStatusCondition(&migration.Status.Conditions, metav1.Condition{
		Type:               infrav1beta1.VMMigrationConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             kind.unknownReason,
		Message:            message,
		ObservedGeneration: migration.Generation,
	})
	migration.Status.Message = message
	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Event(migration, corev1.EventTypeWarning, kind.unknownReason, migrationOutcomeUnknownEvent(migration, op, attempt))
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// migrationOutcomeUnknownEvent is the Warning event message of a hold: the
// call, its attempt id and, for SnapshotCreate, the snapshot name it asked
// for (manager-derived, never provider text).
func migrationOutcomeUnknownEvent(migration *infrav1beta1.VMMigration, op migrationLongOp, attempt string) string {
	kind := migrationRequests[op]
	msg := fmt.Sprintf("The result of the %s requested at %s (attempt id) was not recorded", kind.call, attempt)
	if op == longOpSnapshot {
		name, _ := migrationSnapshotIdentity(migration)
		marker := requestMarker(migration.Status.Conditions, kind.condType)
		if marker != nil && migration.Status.LastRetryTime != nil && marker.LastTransitionTime.Before(migration.Status.LastRetryTime) {
			msg += fmt.Sprintf("; it was requested by an earlier attempt of this migration, so the snapshot it asked for "+
				"is named like %q with that attempt's -r<n> suffix (none for the first attempt)", name)
		} else {
			msg += fmt.Sprintf("; the snapshot it asked for is named %q", name)
		}
	}
	return msg
}

// errExportCallMayStillWrite is the staging-cleanup hold cause of a deleting
// VMMigration whose ExportDisk's outcome is unknown and whose deadline has not
// passed yet (exportCallMayStillWrite).
var errExportCallMayStillWrite = stderrors.New("an ExportDisk whose outcome is unknown may still be writing the staged object")

// exportCallMayStillWrite reports whether m carries an ExportRequested marker
// with no recorded export result whose settle time (migrationResendSettleTime,
// past the call's deadline) has not passed at now: its export may still be
// writing the staged object, so the staged object is not deleted yet.
func exportCallMayStillWrite(m *infrav1beta1.VMMigration, now time.Time) bool {
	marker := requestMarker(m.Status.Conditions, migrationConditionExportRequested)
	if marker == nil || migrationResultRecorded(m, longOpExport) {
		return false
	}
	return now.Before(marker.LastTransitionTime.Add(migrationResendSettleTime))
}
