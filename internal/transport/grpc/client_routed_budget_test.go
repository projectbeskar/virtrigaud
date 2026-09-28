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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the manager's side of the routed time budgets (ADR-0007
// Addendum A slice 3, security review): a clustered provider answers a long
// routed call (Clone, SnapshotCreate, ExportDisk) BEFORE the manager's
// deadline, as VM_OPERATION_FAILED — codes.Unknown when the budget ran out,
// codes.Unavailable when an earlier attempt's copy is still running. Neither
// counts toward the per-Provider breaker, and the in-progress answer is
// retryable. No transport change was needed; a plain DeadlineExceeded still
// counts.

func routedVMOpStatus(t *testing.T, code codes.Code, msg string) error {
	t.Helper()
	st, err := status.New(code, msg).WithDetails(&errdetails.ErrorInfo{
		Reason: contracts.VMOperationFailedReason, Domain: contracts.ErrorInfoDomain})
	require.NoError(t, err)
	return st.Err()
}

func TestRoutedBudgetAnswers_StayOutOfTheBreaker(t *testing.T) {
	stopped := routedVMOpStatus(t, codes.Unknown, `clone VM on host "host-a" did not finish within its time budget and was stopped`)
	inProgress := routedVMOpStatus(t, codes.Unavailable, `clone VM on host "host-a": the same copy is still running from an earlier attempt; retry later`)

	for _, method := range []string{
		providerv1.Provider_Clone_FullMethodName,
		providerv1.Provider_SnapshotCreate_FullMethodName,
		providerv1.Provider_ExportDisk_FullMethodName,
	} {
		assert.False(t, countsTowardBreaker(method, stopped), "%s: a budget overrun is VM-scoped", method)
		assert.False(t, countsTowardBreaker(method, inProgress), "%s: a copy in progress is VM-scoped", method)
		assert.True(t, countsTowardBreaker(method, status.Error(codes.DeadlineExceeded, "deadline")),
			"%s: a plain DeadlineExceeded still counts — which is why the provider answers first", method)
	}

	c := &Client{}
	assert.True(t, contracts.IsRetryable(c.mapGRPCError("clone", inProgress)), "a copy in progress is retried")
	assert.False(t, contracts.IsRetryable(c.mapGRPCError("clone", stopped)), "a budget overrun is a failure, not a retry loop")
}
