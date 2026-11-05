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

package client

import (
	"strings"
	"testing"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/tasks"
)

// TestTaskResult covers how WaitForTask reads every typed outcome and the
// legacy (UNSPECIFIED) answers, which keep their historical reading.
func TestTaskResult(t *testing.T) {
	cases := []struct {
		name    string
		resp    *providerv1.TaskStatusResponse
		done    bool
		errText string // "" = no error
	}{
		{"legacy running", &providerv1.TaskStatusResponse{}, false, ""},
		{"legacy running with error keeps polling", &providerv1.TaskStatusResponse{Error: "x"}, false, ""},
		{"legacy success", &providerv1.TaskStatusResponse{Done: true}, true, ""},
		{"legacy failure", &providerv1.TaskStatusResponse{Done: true, Error: "boom"}, true, "task failed: boom"},
		{"running", tasks.Status(providerv1.TaskOutcome_TASK_OUTCOME_RUNNING, "", ""), false, ""},
		{"unknown is never done", &providerv1.TaskStatusResponse{Done: true, Outcome: providerv1.TaskOutcome_TASK_OUTCOME_UNKNOWN}, false, ""},
		{"succeeded", tasks.Status(providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED, "", ""), true, ""},
		{"succeeded contradicted by an error", &providerv1.TaskStatusResponse{Done: true, Error: "boom",
			Outcome: providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED}, true, "task failed: boom"},
		{"failed clean", tasks.Status(providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN, "", "disk full"), true, "task failed: disk full"},
		{"failed dirty", tasks.Status(providerv1.TaskOutcome_TASK_OUTCOME_FAILED_DIRTY, "unlink", ""), true, "FAILED_DIRTY{unlink}"},
		{"retired is not a success", &providerv1.TaskStatusResponse{Done: true,
			Outcome: providerv1.TaskOutcome_TASK_OUTCOME_RETIRED, OutcomeReason: "deadline"}, true, "not performed: RETIRED{deadline}"},
		{"cancelled", tasks.Status(providerv1.TaskOutcome_TASK_OUTCOME_CANCELLED, "", ""), true, "not performed: CANCELLED{}"},
		{"future outcome falls back to legacy", &providerv1.TaskStatusResponse{Done: true, Outcome: 42}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done, err := taskResult(tc.resp)
			if done != tc.done {
				t.Fatalf("done = %v, want %v", done, tc.done)
			}
			switch {
			case tc.errText == "" && err != nil:
				t.Fatalf("unexpected error %v", err)
			case tc.errText != "" && (err == nil || !strings.Contains(err.Error(), tc.errText)):
				t.Fatalf("error = %v, want it to contain %q", err, tc.errText)
			}
		})
	}
}
