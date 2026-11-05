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

package contracts

// TaskOutcome is the typed result of an asynchronous provider task
// (ADR-0016 A7), the manager-side form of the gRPC TaskOutcome enum.
type TaskOutcome string

const (
	// TaskOutcomeUnspecified: the provider reports no typed outcome (one that
	// predates it, or an answer it cannot type). The task is read from its
	// done flag and error, exactly as before typed outcomes existed.
	TaskOutcomeUnspecified TaskOutcome = ""
	// TaskOutcomeRunning: accepted and not terminal.
	TaskOutcomeRunning TaskOutcome = "Running"
	// TaskOutcomeSucceeded: every step is done and its effect was observed or
	// recorded. It does not prove that the effect still exists.
	TaskOutcomeSucceeded TaskOutcome = "Succeeded"
	// TaskOutcomeFailedClean: a definite failure before any irreversible step.
	TaskOutcomeFailedClean TaskOutcome = "FailedClean"
	// TaskOutcomeFailedDirty: a definite failure after an irreversible step
	// began; something may have happened.
	TaskOutcomeFailedDirty TaskOutcome = "FailedDirty"
	// TaskOutcomeUnknown: the provider has too little evidence to tell what
	// the task did. Not terminal: the task is never read as done.
	TaskOutcomeUnknown TaskOutcome = "Unknown"
	// TaskOutcomeRetired: the task provably never started and never will. It
	// proves nothing about any other attempt.
	TaskOutcomeRetired TaskOutcome = "Retired"
	// TaskOutcomeCancelled: cancelled before any irreversible step; its
	// compensations are done.
	TaskOutcomeCancelled TaskOutcome = "Cancelled"
)

// NotPerformed reports whether o says the task definitely did not perform
// its operation: TaskOutcomeRetired or TaskOutcomeCancelled. Such a task is
// over and is never a success.
func (o TaskOutcome) NotPerformed() bool {
	return o == TaskOutcomeRetired || o == TaskOutcomeCancelled
}

// Typed reports whether o is a typed outcome (anything but
// TaskOutcomeUnspecified).
func (o TaskOutcome) Typed() bool {
	return o != TaskOutcomeUnspecified
}
