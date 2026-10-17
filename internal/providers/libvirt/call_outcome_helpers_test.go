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

package libvirt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// assertNotPerformedStatus asserts err crosses gRPC as the answer the manager
// reads as "the provider did not perform the operation": codes.Unavailable
// carrying a VM_OPERATION_FAILED ErrorInfo (contracts.ErrOperationNotPerformed,
// contracts.IsNothingDone on the manager's side).
func assertNotPerformedStatus(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "a gRPC status: %v", err)
	assert.Equal(t, codes.Unavailable, st.Code(), "%v", err)
	assert.True(t, contracts.HasErrorInfoReason(err, contracts.VMOperationFailedReason),
		"VM_OPERATION_FAILED reason: %v", err)
}

// assertRefusalStatus asserts err crosses gRPC as the typed refusal code (one
// the manager reads as a definite refusal).
func assertRefusalStatus(t *testing.T, err error, code codes.Code) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, code, status.Code(err), "%v", err)
}

// assertUnknownOutcome asserts err is an answer the manager cannot read as a
// definite outcome: an untyped codes.Unknown (what a plain error crosses gRPC
// as), so it keeps the durable marker and holds the object.
func assertUnknownOutcome(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Nil(t, definiteStatusError(err), "must not be a definite answer: %v", err)
	assert.Equal(t, codes.Unknown, status.Code(err), "%v", err)
}

// assertConstantText asserts err's wire text names none of the given
// fragments (host paths, stderr, URLs, command lines).
func assertConstantText(t *testing.T, err error, leaks ...string) {
	t.Helper()
	require.Error(t, err)
	for _, l := range leaks {
		assert.NotContains(t, status.Convert(err).Message(), l, "the wire text must not leak %q", l)
	}
}
