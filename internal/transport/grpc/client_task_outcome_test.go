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

package grpc

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	sdktasks "github.com/projectbeskar/virtrigaud/sdk/provider/tasks"
)

// These tests pin the manager's reading of typed TaskStatus outcomes
// (ADR-0016 A7): a RETIRED or CANCELLED task is a definite not-performed and
// never a success, UNKNOWN is never done, and UNSPECIFIED keeps the reading
// of done and error that predates typed outcomes.

// taskAnswerServer answers every TaskStatus with resp, or err, and counts the
// calls.
type taskAnswerServer struct {
	providerv1.UnimplementedProviderServer
	resp  *providerv1.TaskStatusResponse
	err   error
	calls atomic.Int32
}

func (s *taskAnswerServer) TaskStatus(context.Context, *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

func TestIsTaskComplete_TypedOutcomes(t *testing.T) {
	type want struct {
		done         bool
		notPerformed bool
		failed       bool // a plain "task failed" error
		outcome      contracts.TaskOutcome
		completed    bool   // TaskStatus.IsCompleted
		errContains  string // TaskStatus.Error
	}
	cases := []struct {
		name string
		resp *providerv1.TaskStatusResponse
		want want
	}{
		// UNSPECIFIED: exactly the historical reading.
		{"legacy running", &providerv1.TaskStatusResponse{}, want{}},
		{"legacy done", &providerv1.TaskStatusResponse{Done: true}, want{done: true, completed: true}},
		{"legacy failed", &providerv1.TaskStatusResponse{Done: true, Error: "boom"},
			want{done: true, failed: true, completed: true, errContains: "boom"}},
		{"legacy error while not done is still a failure", &providerv1.TaskStatusResponse{Error: "boom"},
			want{done: true, failed: true, errContains: "boom"}},
		{"legacy not-performed text from a provider without outcome", &providerv1.TaskStatusResponse{Done: true,
			Error: "not performed: RETIRED{deadline}"},
			want{done: true, failed: true, completed: true, errContains: "not performed"}},
		// Typed outcomes.
		{"running", sdktasks.Status(providerv1.TaskOutcome_TASK_OUTCOME_RUNNING, "", ""),
			want{outcome: contracts.TaskOutcomeRunning}},
		{"running wins over a stray done", &providerv1.TaskStatusResponse{Done: true,
			Outcome: providerv1.TaskOutcome_TASK_OUTCOME_RUNNING}, want{outcome: contracts.TaskOutcomeRunning}},
		{"unknown is never done", &providerv1.TaskStatusResponse{Done: true, Error: "x",
			Outcome: providerv1.TaskOutcome_TASK_OUTCOME_UNKNOWN, OutcomeReason: "vm_died"},
			want{outcome: contracts.TaskOutcomeUnknown}},
		{"succeeded", sdktasks.Status(providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED, "", ""),
			want{done: true, completed: true, outcome: contracts.TaskOutcomeSucceeded}},
		{"succeeded contradicted by an error reads as legacy", &providerv1.TaskStatusResponse{Done: true, Error: "boom",
			Outcome: providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED},
			want{done: true, failed: true, completed: true, errContains: "boom"}},
		{"succeeded without done reads as legacy", &providerv1.TaskStatusResponse{
			Outcome: providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED}, want{}},
		{"failed clean", sdktasks.Status(providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN, "", "disk full"),
			want{done: true, failed: true, completed: true, outcome: contracts.TaskOutcomeFailedClean, errContains: "disk full"}},
		{"failed dirty without message", &providerv1.TaskStatusResponse{
			Outcome: providerv1.TaskOutcome_TASK_OUTCOME_FAILED_DIRTY, OutcomeReason: "unlink"},
			want{done: true, failed: true, completed: true, outcome: contracts.TaskOutcomeFailedDirty, errContains: "FAILED_DIRTY{unlink}"}},
		{"retired", sdktasks.Status(providerv1.TaskOutcome_TASK_OUTCOME_RETIRED, "never_received", ""),
			want{done: true, notPerformed: true, completed: true, outcome: contracts.TaskOutcomeRetired,
				errContains: "not performed: RETIRED{never_received}"}},
		{"retired without the legacy fields is still not a success", &providerv1.TaskStatusResponse{
			Outcome: providerv1.TaskOutcome_TASK_OUTCOME_RETIRED, OutcomeReason: "deadline"},
			want{done: true, notPerformed: true, completed: true, outcome: contracts.TaskOutcomeRetired,
				errContains: "not performed: RETIRED{deadline}"}},
		{"cancelled", sdktasks.Status(providerv1.TaskOutcome_TASK_OUTCOME_CANCELLED, "", ""),
			want{done: true, notPerformed: true, completed: true, outcome: contracts.TaskOutcomeCancelled,
				errContains: "not performed: CANCELLED"}},
		{"an outcome this manager does not know reads as legacy", &providerv1.TaskStatusResponse{Done: true,
			Outcome: providerv1.TaskOutcome(42)}, want{done: true, completed: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &taskAnswerServer{resp: tc.resp}
			c := newTestClientForTasks(t, srv, "outcome", "outcome-provider")

			done, err := c.IsTaskComplete(context.Background(), "t-1")
			assert.Equal(t, tc.want.done, done, "IsTaskComplete done")
			assert.Equal(t, tc.want.notPerformed, contracts.IsNotPerformed(err), "IsNotPerformed(%v)", err)
			switch {
			case tc.want.notPerformed:
				require.Error(t, err)
				assert.False(t, contracts.IsRetryable(err), "polling a not-performed task again changes nothing")
				assert.False(t, contracts.IsNotFound(err))
			case tc.want.failed:
				require.Error(t, err)
				assert.Contains(t, err.Error(), "task failed")
			default:
				require.NoError(t, err)
			}

			st, err := c.TaskStatus(context.Background(), "t-1")
			require.NoError(t, err)
			assert.Equal(t, tc.want.outcome, st.Outcome)
			assert.Equal(t, tc.want.completed, st.IsCompleted, "TaskStatus IsCompleted")
			if tc.want.errContains == "" {
				assert.Empty(t, st.Error)
			} else {
				assert.Contains(t, st.Error, tc.want.errContains)
			}
			// The one property every caller relies on: completed without an
			// error is a success, and only SUCCEEDED (or a legacy done) is one.
			if st.IsCompleted && st.Error == "" {
				assert.NotContains(t, []contracts.TaskOutcome{contracts.TaskOutcomeRetired, contracts.TaskOutcomeCancelled,
					contracts.TaskOutcomeUnknown, contracts.TaskOutcomeFailedClean, contracts.TaskOutcomeFailedDirty}, st.Outcome)
			}
		})
	}
}

// unknownToAgentErr builds a clustered provider's answer for a task its host
// never issued.
func unknownToAgentErr(t *testing.T, domain string) error {
	t.Helper()
	st, err := status.New(codes.NotFound, "host \"host-a\" has no record of the task").
		WithDetails(&errdetails.ErrorInfo{Reason: contracts.UnknownToAgentReason, Domain: domain})
	require.NoError(t, err)
	return st.Err()
}

func TestIsTaskComplete_UnknownToAgentIsNotFoundNeverDone(t *testing.T) {
	srv := &taskAnswerServer{err: unknownToAgentErr(t, contracts.ErrorInfoDomain)}
	c := newTestClientForTasks(t, srv, "uta", "uta-provider")

	done, err := c.IsTaskComplete(context.Background(), "host-task/v1/host-a/forged")
	assert.False(t, done)
	assert.True(t, contracts.IsNotFound(err), "mapped to NotFound: %v", err)
	assert.True(t, contracts.IsUnknownToAgent(err), "marked UNKNOWN_TO_AGENT: %v", err)
	assert.False(t, contracts.IsNotPerformed(err), "UNKNOWN_TO_AGENT proves nothing about the task")
	assert.False(t, contracts.IsNothingDone(err))

	_, err = c.TaskStatus(context.Background(), "host-task/v1/host-a/forged")
	assert.True(t, contracts.IsUnknownToAgent(err))

	// A plain NotFound, or the reason in another domain, is not marked.
	srv.err = status.Error(codes.NotFound, "no such task")
	_, err = c.IsTaskComplete(context.Background(), "x")
	assert.True(t, contracts.IsNotFound(err))
	assert.False(t, contracts.IsUnknownToAgent(err))
	srv.err = unknownToAgentErr(t, "example.com")
	_, err = c.IsTaskComplete(context.Background(), "x")
	assert.False(t, contracts.IsUnknownToAgent(err))
}

// partAErr builds a status of code carrying a Part A reason.
func partAErr(t *testing.T, code codes.Code, reason, domain string) error {
	t.Helper()
	st, err := status.New(code, "agent answer").WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: domain})
	require.NoError(t, err)
	return st.Err()
}

func TestIsInfraFailure_PartAReasonsAreNotInfra(t *testing.T) {
	for _, reason := range contracts.PartAReasons() {
		for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded, codes.Internal, codes.Unknown} {
			assert.False(t, isInfraFailure(partAErr(t, code, reason, contracts.ErrorInfoDomain)),
				"%s on %s is breaker-exempt", reason, code)
			assert.True(t, isInfraFailure(partAErr(t, code, reason, "example.com")),
				"%s in another domain is not VirtRigaud's answer: %s still counts", reason, code)
		}
	}
}

func TestCircuitBreaker_PartAReasonsDoNotTrip(t *testing.T) {
	for _, reason := range contracts.PartAReasons() {
		t.Run(reason, func(t *testing.T) {
			srv := &describeErrServer{err: partAErr(t, codes.Unavailable, reason, contracts.ErrorInfoDomain)}
			dialer, cleanup := startBufconnServer(t, srv)
			defer cleanup()
			cli, cb := newTestClientWithCB(t, dialer, "parta-"+reason, "parta-provider", &resilience.Config{
				FailureThreshold: 2,
				ResetTimeout:     30 * time.Second,
				HalfOpenMaxCalls: 1,
			})
			for i := 0; i < 5; i++ {
				_, err := cli.Describe(context.Background(), contracts.VMRef{ID: "web", HostID: "host-a"})
				require.Error(t, err)
			}
			assert.EqualValues(t, 5, srv.calls.Load(), "every call reached the provider (breaker never opened)")
			assert.Equal(t, resilience.StateClosed, cb.GetState())
			assert.Zero(t, cb.GetFailures())
		})
	}
}
