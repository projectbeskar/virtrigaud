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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// The asynchronous Delete with typed TaskStatus outcomes (ADR-0016 A7). The
// answers are what the gRPC client returns for each outcome
// (internal/transport/grpc readTaskStatus).

// TestAsyncDelete_NotPerformedNeverReleases: a Delete task the provider
// reports RETIRED or CANCELLED did not happen. The finalizer is kept, the
// task is settled as failed (DeleteTask=False/DeleteTaskFailed) and the
// Delete is sent again only after the backoff — never a release, even when
// the error text looks like an unknown task and the VM would be reported
// absent.
func TestAsyncDelete_NotPerformedNeverReleases(t *testing.T) {
	answers := map[string]contracts.TaskStatus{
		"retired": {IsCompleted: true, Error: "not performed: RETIRED{never_received}",
			Outcome: contracts.TaskOutcomeRetired, OutcomeReason: "never_received"},
		"cancelled": {IsCompleted: true, Error: "not performed: CANCELLED{}", Outcome: contracts.TaskOutcomeCancelled},
		"retired, text of an unknown task": {IsCompleted: true, Error: "not performed: RETIRED{task not found}",
			Outcome: contracts.TaskOutcomeRetired, OutcomeReason: "task not found"},
	}
	for name, st := range answers {
		t.Run(name, func(t *testing.T) {
			f := newAsyncDeleteFixture(t, nil)
			f.reconcile(f.r) // Delete -> task recorded
			f.prov.answer(st, nil)
			f.prov.answerDescribe(contracts.DescribeResponse{Exists: false}, nil)

			res := f.reconcile(f.r)
			assert.Equal(t, vmDeleteRetryInterval, res.RequeueAfter)
			vm := f.requireHeld()
			c := deleteTaskCondition(vm)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionFalse, c.Status)
			assert.Equal(t, k8s.ReasonDeleteTaskFailed, c.Reason)
			assert.Empty(t, vm.Status.LastTaskRef)
			assert.Zero(t, f.prov.describeCalls(), "a definite answer is not settled by Describe")
			assert.EqualValues(t, 1, f.prov.deleteCalls.Load(), "no Delete before the backoff is over")

			// Polled again within the backoff: still held, nothing sent.
			for i := 0; i < 3; i++ {
				f.reconcile(f.r)
				f.requireHeld()
			}
			assert.EqualValues(t, 1, f.prov.deleteCalls.Load())
		})
	}
}

// TestAsyncDelete_OutcomeUnknownIsNeverDone: a typed UNKNOWN outcome is not
// terminal. The task stays recorded and is polled; no Delete is sent again,
// no Describe settles it, and the finalizer is kept.
func TestAsyncDelete_OutcomeUnknownIsNeverDone(t *testing.T) {
	f := newAsyncDeleteFixture(t, nil)
	f.reconcile(f.r)
	f.prov.answer(contracts.TaskStatus{Outcome: contracts.TaskOutcomeUnknown, OutcomeReason: "vm_died"}, nil)
	f.prov.answerDescribe(contracts.DescribeResponse{Exists: false}, nil)

	for i := 0; i < 4; i++ {
		res := f.reconcile(f.r)
		assert.GreaterOrEqual(t, res.RequeueAfter, vmDeleteTaskPollMin)
		vm := f.requireHeld()
		c := deleteTaskCondition(vm)
		require.NotNil(t, c)
		assert.Equal(t, metav1.ConditionTrue, c.Status)
		assert.Equal(t, k8s.ReasonTaskInProgress, c.Reason)
		assert.Equal(t, "task-delete-1", vm.Status.LastTaskRef)
	}
	assert.EqualValues(t, 1, f.prov.deleteCalls.Load())
	assert.Zero(t, f.prov.describeCalls())
}

// TestAsyncDelete_UnknownToAgentChecksAbsence: a host that has no record of
// the task (UNKNOWN_TO_AGENT, a NotFound) is an ambiguous answer, never done
// and never not-performed: no Delete is sent again, and only an owner-checked
// Describe that reports the VM absent releases it; a VM still present is held.
func TestAsyncDelete_UnknownToAgentChecksAbsence(t *testing.T) {
	unknown := contracts.NewNotFoundError("taskStatus: host has no record of the task",
		fmt.Errorf("%w: rpc", contracts.ErrUnknownToAgent))

	t.Run("still present", func(t *testing.T) {
		f := newAsyncDeleteFixture(t, nil)
		f.reconcile(f.r)
		f.prov.answer(contracts.TaskStatus{}, unknown)
		f.prov.answerDescribe(contracts.DescribeResponse{Exists: true}, nil)
		f.reconcile(f.r)
		vm := f.requireHeld()
		c := deleteTaskCondition(vm)
		require.NotNil(t, c)
		assert.Equal(t, metav1.ConditionUnknown, c.Status)
		assert.Equal(t, k8s.ReasonDeleteTaskOutcomeUnknown, c.Reason)
		assert.EqualValues(t, 1, f.prov.deleteCalls.Load())
	})
	t.Run("confirmed absent", func(t *testing.T) {
		f := newAsyncDeleteFixture(t, nil)
		f.reconcile(f.r)
		f.prov.answer(contracts.TaskStatus{}, unknown)
		f.prov.answerDescribe(contracts.DescribeResponse{Exists: false}, nil)
		f.reconcile(f.r)
		assert.Nil(t, f.get(), "absence proves the Delete's goal")
		assert.EqualValues(t, 1, f.prov.deleteCalls.Load())
	})
}

// TestAsyncDelete_TypedSucceededReleases: a typed SUCCEEDED releases the
// finalizer, as an untyped done does.
func TestAsyncDelete_TypedSucceededReleases(t *testing.T) {
	f := newAsyncDeleteFixture(t, nil)
	f.reconcile(f.r)
	f.prov.answer(contracts.TaskStatus{IsCompleted: true, Outcome: contracts.TaskOutcomeSucceeded}, nil)
	f.reconcile(f.r)
	assert.Nil(t, f.get())
}

// TestDeleteTaskAnswerUnknown_TypedOutcomeNotTextMatched: the text match for
// "the provider does not know this task" applies to untyped answers only.
func TestDeleteTaskAnswerUnknown_TypedOutcomeNotTextMatched(t *testing.T) {
	untyped := contracts.TaskStatus{IsCompleted: true, Error: "task not found"}
	assert.True(t, deleteTaskAnswerUnknown(untyped, nil))
	typed := untyped
	typed.Outcome = contracts.TaskOutcomeFailedClean
	assert.False(t, deleteTaskAnswerUnknown(typed, nil))
	typed.Outcome = contracts.TaskOutcomeRetired
	assert.False(t, deleteTaskAnswerUnknown(typed, nil))
}
