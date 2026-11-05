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

package tasks

import (
	"strings"
	"testing"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestStatus_LegacyMapping pins the legacy done/error of every outcome: an
// older manager reading only done and error must never take a retired or
// cancelled task for a success.
func TestStatus_LegacyMapping(t *testing.T) {
	cases := []struct {
		o         providerv1.TaskOutcome
		reason    string
		message   string
		wantDone  bool
		wantError string
	}{
		{providerv1.TaskOutcome_TASK_OUTCOME_UNSPECIFIED, "", "", false, ""},
		{providerv1.TaskOutcome_TASK_OUTCOME_RUNNING, "", "", false, ""},
		{providerv1.TaskOutcome_TASK_OUTCOME_UNKNOWN, "vm_died", "", false, ""},
		{providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED, "", "", true, ""},
		{providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN, "", "disk full", true, "disk full"},
		{providerv1.TaskOutcome_TASK_OUTCOME_FAILED_DIRTY, "unlink", "", true, "FAILED_DIRTY{unlink}"},
		{providerv1.TaskOutcome_TASK_OUTCOME_RETIRED, "never_received", "", true, "not performed: RETIRED{never_received}"},
		{providerv1.TaskOutcome_TASK_OUTCOME_CANCELLED, "", "ignored", true, "not performed: CANCELLED{}"},
	}
	for _, tc := range cases {
		t.Run(tc.o.String(), func(t *testing.T) {
			r := Status(tc.o, tc.reason, tc.message)
			if r.GetDone() != tc.wantDone || r.GetError() != tc.wantError {
				t.Fatalf("Status(%v) = done %v error %q, want done %v error %q", tc.o, r.GetDone(), r.GetError(), tc.wantDone, tc.wantError)
			}
			if r.GetOutcome() != tc.o || r.GetOutcomeReason() != tc.reason {
				t.Fatalf("Status(%v) = outcome %v reason %q", tc.o, r.GetOutcome(), r.GetOutcomeReason())
			}
			// The safety property an older manager relies on: done with no
			// error is a success, so it must be exactly SUCCEEDED.
			if r.GetDone() && r.GetError() == "" && tc.o != providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED {
				t.Fatalf("%v reads as a success to a manager that predates typed outcomes", tc.o)
			}
			if (tc.o == providerv1.TaskOutcome_TASK_OUTCOME_RETIRED || tc.o == providerv1.TaskOutcome_TASK_OUTCOME_CANCELLED) &&
				!strings.HasPrefix(r.GetError(), NotPerformedPrefix) {
				t.Fatalf("%v error %q lacks %q", tc.o, r.GetError(), NotPerformedPrefix)
			}
		})
	}
}

// TestOutcomeLabel covers a known value and one this package does not know.
func TestOutcomeLabel(t *testing.T) {
	if got := OutcomeLabel(providerv1.TaskOutcome_TASK_OUTCOME_RETIRED); got != "RETIRED" {
		t.Fatalf("OutcomeLabel(RETIRED) = %q", got)
	}
	if got := OutcomeLabel(providerv1.TaskOutcome(99)); got != "99" {
		t.Fatalf("OutcomeLabel(99) = %q", got)
	}
	if LegacyDone(providerv1.TaskOutcome(99)) || LegacyError(providerv1.TaskOutcome(99), "x", "y") != "" {
		t.Fatal("an unknown outcome must not be terminal")
	}
}
