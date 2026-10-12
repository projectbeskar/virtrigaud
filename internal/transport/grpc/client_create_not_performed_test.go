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

// createNotPerformedErr builds a provider's Unavailable answer with the given
// ErrorInfo reasons in VirtRigaud's domain.
func createNotPerformedErr(t *testing.T, reasons ...string) error {
	t.Helper()
	st := status.New(codes.Unavailable, "cannot allocate a VMID; nothing was created")
	for _, r := range reasons {
		var err error
		st, err = st.WithDetails(&errdetails.ErrorInfo{Reason: r, Domain: contracts.ErrorInfoDomain})
		require.NoError(t, err)
	}
	return st.Err()
}

// TestMapGRPCError_CreateNotPerformed: CREATE_NOT_PERFORMED is marked on the
// mapped error (contracts.IsCreateNotPerformed), which keeps its type and
// retryability.
func TestMapGRPCError_CreateNotPerformed(t *testing.T) {
	c := &Client{}
	mapped := c.mapGRPCError("create", createNotPerformedErr(t, contracts.CreateNotPerformedReason))
	assert.True(t, contracts.IsCreateNotPerformed(mapped))
	assert.True(t, contracts.IsRetryable(mapped), "an Unavailable answer stays retryable")

	mapped = c.mapGRPCError("create", createNotPerformedErr(t, contracts.CreateNotPerformedReason, contracts.VMOperationFailedReason))
	assert.True(t, contracts.IsCreateNotPerformed(mapped))

	for name, err := range map[string]error{
		"plain Unavailable":   status.Error(codes.Unavailable, "boom"),
		"VM_OPERATION_FAILED": createNotPerformedErr(t, contracts.VMOperationFailedReason),
	} {
		assert.False(t, contracts.IsCreateNotPerformed(c.mapGRPCError("create", err)), name)
	}
}

// TestIsInfraFailure_CreateNotPerformedScope: a provider-wide
// CREATE_NOT_PERFORMED (no VM_OPERATION_FAILED) counts toward the circuit
// breaker, so a hypervisor outage opens it; one VM's does not.
func TestIsInfraFailure_CreateNotPerformedScope(t *testing.T) {
	assert.True(t, isInfraFailure(createNotPerformedErr(t, contracts.CreateNotPerformedReason)))
	assert.False(t, isInfraFailure(createNotPerformedErr(t, contracts.CreateNotPerformedReason, contracts.VMOperationFailedReason)))
}
