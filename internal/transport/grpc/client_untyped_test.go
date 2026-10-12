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
)

// An answer mapGRPCError does not categorize keeps its text and its gRPC
// status, so the controller can read the code and details.
func TestMapGRPCError_UntypedKeepsTheStatus(t *testing.T) {
	for _, code := range []codes.Code{codes.FailedPrecondition, codes.PermissionDenied, codes.Internal, codes.Unknown} {
		mapped := (&Client{}).mapGRPCError("create", status.Error(code, "imported disk not found"))
		assert.Equal(t, "create failed: imported disk not found", mapped.Error(), code.String())
		st, ok := status.FromError(mapped)
		require.True(t, ok, code.String())
		assert.Equal(t, code, st.Code())
		var pe *contracts.ProviderError
		assert.NotErrorAs(t, mapped, &pe, "still untyped")
	}

	withInfo, err := status.New(codes.Unknown, "blockresize failed").WithDetails(
		&errdetails.ErrorInfo{Reason: contracts.VMOperationFailedReason, Domain: contracts.ErrorInfoDomain})
	require.NoError(t, err)
	st, ok := status.FromError((&Client{}).mapGRPCError("create", withInfo.Err()))
	require.True(t, ok)
	assert.Len(t, st.Details(), 1, "the details are kept")
}

// CREATE_NOT_PERFORMED marks the mapped error, whatever the code, and keeps
// its category and text.
func TestMapGRPCError_CreateNotPerformedAnyCode(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.Internal, codes.FailedPrecondition} {
		st, err := status.New(code, "template clone refused").WithDetails(
			&errdetails.ErrorInfo{Reason: contracts.CreateNotPerformedReason, Domain: contracts.ErrorInfoDomain})
		require.NoError(t, err)
		mapped := (&Client{}).mapGRPCError("create", st.Err())
		assert.True(t, contracts.IsCreateNotPerformed(mapped), code.String())
		assert.Contains(t, mapped.Error(), "template clone refused")
	}
	assert.True(t, contracts.IsRetryable(contracts.NewRetryableError("x", nil)))
	plain := (&Client{}).mapGRPCError("create", status.Error(codes.Unavailable, "down"))
	assert.False(t, contracts.IsCreateNotPerformed(plain))
	other, err := status.New(codes.Unavailable, "x").WithDetails(
		&errdetails.ErrorInfo{Reason: contracts.CreateNotPerformedReason, Domain: "example.com"})
	require.NoError(t, err)
	assert.False(t, contracts.IsCreateNotPerformed((&Client{}).mapGRPCError("create", other.Err())), "another domain's reason is not ours")
}
