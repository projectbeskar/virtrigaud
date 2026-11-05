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

package mock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestTaskStatus_TypedOutcome: the mock reports RUNNING, SUCCEEDED and
// FAILED_CLEAN with the matching legacy done/error, and an unknown task with
// no typed outcome (its historical answer, unchanged).
func TestTaskStatus_TypedOutcome(t *testing.T) {
	ctx := context.Background()
	p, _ := newImageProvider(t, time.Hour)
	ask := func(id string) *providerv1.TaskStatusResponse {
		t.Helper()
		st, err := p.TaskStatus(ctx, &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: id}})
		require.NoError(t, err)
		return st
	}
	start := func() string {
		t.Helper()
		resp, err := p.ImagePrepare(ctx, identityReq("team-a", "ubuntu", imgUID, digestA, provUID))
		require.NoError(t, err)
		require.NotNil(t, resp.GetTask())
		return resp.GetTask().GetId()
	}

	id := start()
	st := ask(id)
	assert.Equal(t, providerv1.TaskOutcome_TASK_OUTCOME_RUNNING, st.GetOutcome())
	assert.False(t, st.GetDone())
	assert.Empty(t, st.GetError())

	require.True(t, p.FinishTask(id, ""))
	st = ask(id)
	assert.Equal(t, providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED, st.GetOutcome())
	assert.True(t, st.GetDone())
	assert.Empty(t, st.GetError())

	require.True(t, p.FinishTask(id, "download failed"))
	st = ask(id)
	assert.Equal(t, providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN, st.GetOutcome())
	assert.True(t, st.GetDone())
	assert.Equal(t, "download failed", st.GetError())

	st = ask("no-such-task")
	assert.Equal(t, providerv1.TaskOutcome_TASK_OUTCOME_UNSPECIFIED, st.GetOutcome(), "the mock cannot prove anything about a task it does not know")
	assert.True(t, st.GetDone())
	assert.Equal(t, "task not found", st.GetError())
}
