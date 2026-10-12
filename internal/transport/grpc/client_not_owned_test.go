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

// withReason is a status error of code carrying one ErrorInfo reason in domain.
func withReason(t *testing.T, code codes.Code, reason, domain string) error {
	t.Helper()
	st, err := status.New(code, "refused").WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: domain})
	require.NoError(t, err)
	return st.Err()
}

func TestMapGRPCError_VMNotOwnedIsAMarkedNotFound(t *testing.T) {
	c := &Client{}
	mapped := c.mapGRPCError("delete", withReason(t, codes.NotFound, contracts.VMNotOwnedReason, contracts.ErrorInfoDomain))
	assert.True(t, contracts.IsNotFound(mapped))
	assert.True(t, contracts.IsVMNotOwned(mapped))

	plain := c.mapGRPCError("delete", status.Error(codes.NotFound, "gone"))
	assert.True(t, contracts.IsNotFound(plain))
	assert.False(t, contracts.IsVMNotOwned(plain), "a plain NotFound is a VM that is gone")

	other := c.mapGRPCError("delete", withReason(t, codes.NotFound, contracts.VMNotOwnedReason, "example.com"))
	assert.False(t, contracts.IsVMNotOwned(other), "another domain's reason is not ours")
}

func TestMapGRPCError_OwnerStampUnreadableIsAMarkedConflict(t *testing.T) {
	c := &Client{}
	mapped := c.mapGRPCError("delete", withReason(t, codes.FailedPrecondition, contracts.OwnerStampUnreadableReason, contracts.ErrorInfoDomain))
	assert.True(t, contracts.IsConflict(mapped))
	assert.True(t, contracts.IsOwnerStampUnreadable(mapped))
	assert.False(t, contracts.IsVMIsImageArtifact(mapped))
	assert.False(t, contracts.IsRetryable(mapped))
}
