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

// Package tasks builds TaskStatus answers with a typed outcome (ADR-0016 A7)
// that a manager predating typed outcomes reads the same way.
//
// A provider that sets TaskStatusResponse.outcome must also fill the legacy
// done and error fields from it:
//
//	outcome                    done   error
//	RUNNING, UNKNOWN           false  ""
//	SUCCEEDED                  true   ""
//	FAILED_CLEAN, FAILED_DIRTY true   <message>
//	RETIRED, CANCELLED         true   "not performed: <OUTCOME>{<reason>}"
//
// A retired or cancelled task carries an error so that an older manager never
// reads it as a success (a retired Delete would otherwise release the
// VirtualMachine's finalizer). Status fills both from one call.
package tasks

import (
	"fmt"
	"strings"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// NotPerformedPrefix starts the legacy error of a retired or cancelled task.
const NotPerformedPrefix = "not performed: "

// outcomeEnumPrefix is the prefix every TaskOutcome enum value name carries.
const outcomeEnumPrefix = "TASK_OUTCOME_"

// OutcomeLabel returns the short name of o ("RETIRED" for
// TASK_OUTCOME_RETIRED), or the number of a value this package does not know.
func OutcomeLabel(o providerv1.TaskOutcome) string {
	name, ok := providerv1.TaskOutcome_name[int32(o)]
	if !ok {
		return fmt.Sprintf("%d", int32(o))
	}
	return strings.TrimPrefix(name, outcomeEnumPrefix)
}

// LegacyError returns the error a provider puts in TaskStatusResponse.error
// for outcome o with reason and message: "" for RUNNING, UNKNOWN, SUCCEEDED
// and UNSPECIFIED; message (or, when it is empty, "<OUTCOME>{<reason>}") for
// FAILED_CLEAN and FAILED_DIRTY; "not performed: <OUTCOME>{<reason>}" for
// RETIRED and CANCELLED.
func LegacyError(o providerv1.TaskOutcome, reason, message string) string {
	switch o {
	case providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN, providerv1.TaskOutcome_TASK_OUTCOME_FAILED_DIRTY:
		if message != "" {
			return message
		}
		return fmt.Sprintf("%s{%s}", OutcomeLabel(o), reason)
	case providerv1.TaskOutcome_TASK_OUTCOME_RETIRED, providerv1.TaskOutcome_TASK_OUTCOME_CANCELLED:
		return fmt.Sprintf("%s%s{%s}", NotPerformedPrefix, OutcomeLabel(o), reason)
	}
	return ""
}

// LegacyDone returns the done flag a provider puts in
// TaskStatusResponse.done for outcome o: false for RUNNING, UNKNOWN and
// UNSPECIFIED, true for every terminal outcome.
func LegacyDone(o providerv1.TaskOutcome) bool {
	switch o {
	case providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED,
		providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN,
		providerv1.TaskOutcome_TASK_OUTCOME_FAILED_DIRTY,
		providerv1.TaskOutcome_TASK_OUTCOME_RETIRED,
		providerv1.TaskOutcome_TASK_OUTCOME_CANCELLED:
		return true
	}
	return false
}

// Status returns the TaskStatus answer of outcome o, with its legacy done and
// error filled from it (LegacyDone, LegacyError). reason is the outcome's
// {reason} (never_received, provider_changed, ...), and message the failure
// text of a FAILED_CLEAN or FAILED_DIRTY task; neither may hold a secret, an
// endpoint or a credential. Use it only for an outcome the provider can prove;
// a provider that cannot type an answer keeps outcome UNSPECIFIED.
func Status(o providerv1.TaskOutcome, reason, message string) *providerv1.TaskStatusResponse {
	return &providerv1.TaskStatusResponse{
		Done:          LegacyDone(o),
		Error:         LegacyError(o, reason, message),
		Outcome:       o,
		OutcomeReason: reason,
	}
}
