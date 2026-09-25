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

// diskInUseErr builds a provider's disk-dependents refusal: FailedPrecondition
// with VM_DISK_IN_USE, plus VM_OPERATION_FAILED on a clustered (routed) call.
func diskInUseErr(t *testing.T, msg string, routed bool) error {
	t.Helper()
	st := status.New(codes.FailedPrecondition, msg)
	reasons := []string{contracts.VMDiskInUseReason}
	if routed {
		reasons = append(reasons, contracts.VMOperationFailedReason)
	}
	for _, r := range reasons {
		var err error
		st, err = st.WithDetails(&errdetails.ErrorInfo{Reason: r, Domain: contracts.ErrorInfoDomain})
		require.NoError(t, err)
	}
	return st.Err()
}

// TestMapGRPCError_VMDiskInUseIsConflict pins how the manager reads a
// provider's refusal to delete (or snapshot) a VM whose disk another VM uses:
// a typed, non-retryable Conflict carrying the provider's message, never
// counted toward the per-Provider circuit breaker, on the single-host and the
// routed (clustered) wire form alike. Any other FailedPrecondition keeps its
// historical untyped mapping.
func TestMapGRPCError_VMDiskInUseIsConflict(t *testing.T) {
	const msg = `failed to delete VM: delete of libvirt domain "team-a.web" refused: its disk is the backing file ` +
		`(or a disk) of 1 other domain(s) on this host, such as a linked clone of this VM; delete the linked clones first`
	for _, routed := range []bool{false, true} {
		err := diskInUseErr(t, msg, routed)
		mapped := (&Client{}).mapGRPCError("delete", err)
		assert.True(t, contracts.IsConflict(mapped), "routed=%v: %v", routed, mapped)
		assert.False(t, contracts.IsRetryable(mapped))
		assert.Contains(t, mapped.Error(), "delete the linked clones first")
		for _, m := range []string{providerv1.Provider_Delete_FullMethodName, providerv1.Provider_SnapshotRevert_FullMethodName} {
			assert.False(t, countsTowardBreaker(m, err), "routed=%v %s: a refusal says nothing about the provider's health", routed, m)
		}
	}

	plain := (&Client{}).mapGRPCError("image prepare", status.Error(codes.FailedPrecondition, "import folder missing"))
	assert.False(t, contracts.IsConflict(plain), "only the VM_DISK_IN_USE reason is a Conflict: %v", plain)
	assert.Contains(t, plain.Error(), "image prepare failed: import folder missing")

	other := status.New(codes.FailedPrecondition, "x")
	other, werr := other.WithDetails(&errdetails.ErrorInfo{Reason: contracts.VMDiskInUseReason, Domain: "example.com"})
	require.NoError(t, werr)
	assert.False(t, contracts.IsConflict((&Client{}).mapGRPCError("delete", other.Err())), "another domain's reason is not ours")
}
