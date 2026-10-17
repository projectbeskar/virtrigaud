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
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Durable "requested" markers for non-idempotent provider calls (rc2 R6).
//
// Before a VMMigration sends SnapshotCreate, ExportDisk or ImportDisk, and
// before a VMSnapshot sends SnapshotCreate, the object's status records that
// the call was requested — a condition of its own type (e.g. ExportRequested),
// written with the optimistic lock of the resourceVersion the reconcile read.
// Nothing is sent when that write fails. The call's result, or its definite
// failure, replaces the marker in the same status write. A marker found with
// no result therefore means the call may have run although its outcome was
// never recorded: a manager restart or leader change between the call and
// the write of its result (or while a result the write lost was held in
// memory). Each controller then decides whether the call can be sent again
// for that provider and call, or holds the object with a constant
// ...OutcomeUnknown reason until an operator confirms that the earlier call
// left nothing behind (operationOutcomeAnnotation). It never sends a second
// call that could duplicate what the first one made.
//
// Each marker write is one attempt. Its attempt id is the marker's
// LastTransitionTime in RFC 3339, strictly increasing from one attempt to the
// next (setRequestMarker), so a confirmation names exactly one attempt.
//
// A call the provider answered without a definite outcome — a deadline, a
// transport failure after the request was written, an error the provider did
// not classify (providerCallOutcomeKnown) — keeps its marker and is decided
// like a lost result, never failed: the earlier call may still be running.
// Automatic re-sends are counted in the marker (requestResends) and capped;
// past the cap the object is held for an operator.

// operationOutcomeAnnotation, set by an operator to
// operationOutcomeNotPerformedPrefix + the attempt id of a held object's
// requested-call marker, confirms that that call left nothing behind (or that
// the operator removed what it left): the manager removes the annotation and
// sends the call again. Any other value is ignored, and the annotation is
// removed before every new call, so a confirmation never outlives the attempt
// it names.
const operationOutcomeAnnotation = "virtrigaud.io/operation-outcome"

// operationOutcomeNotPerformedPrefix starts an operationOutcomeAnnotation
// value; the attempt id follows it.
const operationOutcomeNotPerformedPrefix = "not-performed@"

// eventReasonOperationOutcomeConfirmed is the Warning event reason recorded
// (audit) when an operator's operationOutcomeAnnotation authorizes a call to
// be sent again.
const eventReasonOperationOutcomeConfirmed = "OperationOutcomeConfirmed"

// errReasonProviderOutcomeUnknown is the metrics reason of an object held
// because a provider call's outcome was not recorded.
const errReasonProviderOutcomeUnknown = "provider-outcome-unknown"

// requestMarkerRecheckInterval re-checks an object held for an operator's
// confirmation (its watch re-drives it as soon as the annotation is set).
const requestMarkerRecheckInterval = time.Minute

// setRequestMarker records in conds a new marker of condType (status True,
// reason and message given) at now: its LastTransitionTime — the attempt id —
// is strictly later than that of any marker of condType conds carries
// already (it has a one-second resolution).
func setRequestMarker(conds *[]metav1.Condition, condType, reason, message string, generation int64, now time.Time) {
	at := now.UTC().Truncate(time.Second)
	if prev := meta.FindStatusCondition(*conds, condType); prev != nil && !at.After(prev.LastTransitionTime.UTC()) {
		at = prev.LastTransitionTime.UTC().Truncate(time.Second).Add(time.Second)
	}
	meta.RemoveStatusCondition(conds, condType)
	*conds = append(*conds, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
		LastTransitionTime: metav1.NewTime(at),
	})
}

// requestMarker returns the marker of condType in conds, or nil.
func requestMarker(conds []metav1.Condition, condType string) *metav1.Condition {
	c := meta.FindStatusCondition(conds, condType)
	if c == nil || c.Status != metav1.ConditionTrue {
		return nil
	}
	return c
}

// requestMarkerAttemptID is the attempt id of the marker of condType in conds
// (its LastTransitionTime in RFC 3339, UTC), or "" when there is none.
func requestMarkerAttemptID(conds []metav1.Condition, condType string) string {
	c := requestMarker(conds, condType)
	if c == nil {
		return ""
	}
	return c.LastTransitionTime.UTC().Format(time.RFC3339)
}

// operationOutcomeConfirmed reports whether annotations carry an operator's
// confirmation (operationOutcomeAnnotation) of the attempt attemptID.
func operationOutcomeConfirmed(annotations map[string]string, attemptID string) bool {
	return attemptID != "" && annotations[operationOutcomeAnnotation] == operationOutcomeNotPerformedPrefix+attemptID
}

// requestResendSuffixFormat is appended to a marker's message on an
// automatic re-send: the number of automatic re-sends since the first call
// (or since an operator's confirmation). It is the only place the count is
// kept (no CRD change), so it is parsed back by requestResends.
const requestResendSuffixFormat = " (automatic re-send %d)"

// requestResendSuffixPrefix starts requestResendSuffixFormat.
const requestResendSuffixPrefix = " (automatic re-send "

// withResendCount is message with the automatic re-send count n (none for 0).
func withResendCount(message string, n int) string {
	if n <= 0 {
		return message
	}
	return message + fmt.Sprintf(requestResendSuffixFormat, n)
}

// requestResends is the automatic re-send count recorded in marker's message
// (0 without a suffix).
func requestResends(marker *metav1.Condition) int {
	if marker == nil {
		return 0
	}
	i := strings.LastIndex(marker.Message, requestResendSuffixPrefix)
	if i < 0 {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(marker.Message[i:], requestResendSuffixFormat, &n); err != nil || n < 0 {
		return 0
	}
	return n
}

// nextRequestResends is the re-send count of the marker about to be written
// over prev: 0 for a first call or one an operator confirmed, else one more
// than prev's.
func nextRequestResends(prev *metav1.Condition, confirmed bool) int {
	if prev == nil || confirmed {
		return 0
	}
	return requestResends(prev) + 1
}

// providerCallOutcomeKnown reports whether a failed SnapshotCreate,
// ExportDisk or ImportDisk answer proves the call's outcome, so its marker
// is replaced by the failure: the request never reached the provider or the
// provider did nothing (contracts.IsNothingDone), or the provider refused it
// before doing anything (createDefinitelyRefused, an owner refusal, a
// disk-size refusal, a disk in use, a running source). Any other failure — a
// deadline, a transport failure after the request was written ("transport is
// closing", a provider pod killed mid-call), an error the provider did not
// classify — leaves the outcome unknown: the call may still be running.
func providerCallOutcomeKnown(err error) bool {
	return contracts.IsNothingDone(err) || createDefinitelyRefused(err) || isOwnerRefusal(err) ||
		contracts.IsDiskSizeExceedsLimit(err) || contracts.IsDiskSizeUnverifiable(err) ||
		contracts.IsVMDiskInUse(err) || contracts.IsVMSourceRunning(err)
}

// providerCallOutcomeKnownFor is providerCallOutcomeKnown for a call that is
// the first of its marker (resend false) or follows an earlier call whose
// outcome was never recorded (resend true: the marker was already on the
// object, whether the manager re-sends it or an operator confirmed it). A
// failure answering a re-send is the answer to THIS request only and says
// nothing about what the earlier call made, so it never makes the outcome
// known: an AlreadyExists or NotFound answering a re-sent SnapshotCreate may
// be the snapshot the first call took. The marker is kept, as for a VM Create
// (createOutcomeKnown).
func providerCallOutcomeKnownFor(err error, resend bool) bool {
	return !resend && providerCallOutcomeKnown(err)
}

// operationOutcomeManagers lists the field managers (with their operation)
// that own obj's operationOutcomeAnnotation, for the confirmation's log line.
// The API server's audit log is the authoritative record of who set it.
func operationOutcomeManagers(obj metav1.Object) []string {
	field := `"f:` + operationOutcomeAnnotation + `"`
	var out []string
	for _, mf := range obj.GetManagedFields() {
		if mf.FieldsV1 != nil && strings.Contains(string(mf.FieldsV1.Raw), field) {
			out = append(out, mf.Manager+"/"+string(mf.Operation))
		}
	}
	return out
}
