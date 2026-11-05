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

package vsphere

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vmware/govmomi/vim25/types"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestTaskStatusResponse_TypedOutcome pins the typed outcome of every vCenter
// task state, with the legacy done and error filled from it (ADR-0016 A7).
func TestTaskStatusResponse_TypedOutcome(t *testing.T) {
	p := &Provider{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cases := []struct {
		name    string
		info    types.TaskInfo
		outcome providerv1.TaskOutcome
		done    bool
		err     string
	}{
		{"success", types.TaskInfo{State: types.TaskInfoStateSuccess}, providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED, true, ""},
		{"error with fault", types.TaskInfo{State: types.TaskInfoStateError,
			Error: &types.LocalizedMethodFault{LocalizedMessage: "file locked"}},
			providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN, true, "file locked"},
		{"error without fault", types.TaskInfo{State: types.TaskInfoStateError},
			providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN, true, "task failed with unknown error"},
		{"queued", types.TaskInfo{State: types.TaskInfoStateQueued}, providerv1.TaskOutcome_TASK_OUTCOME_RUNNING, false, ""},
		{"running", types.TaskInfo{State: types.TaskInfoStateRunning, Progress: 40}, providerv1.TaskOutcome_TASK_OUTCOME_RUNNING, false, ""},
		{"unexpected state", types.TaskInfo{State: "weird"}, providerv1.TaskOutcome_TASK_OUTCOME_UNSPECIFIED, true,
			"unexpected task state: weird"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := taskStatusResponse(p, "task-1", &tc.info)
			assert.Equal(t, tc.outcome, r.GetOutcome())
			assert.Equal(t, tc.done, r.GetDone())
			assert.Equal(t, tc.err, r.GetError())
		})
	}
}
