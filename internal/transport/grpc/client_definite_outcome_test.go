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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestMapGRPCError_SingleHostLibvirtDefiniteAnswers pins how the manager reads
// the answers a single-host libvirt provider gives a failed SnapshotCreate,
// ExportDisk or ImportDisk (rc2 R21, internal/providers/libvirt/call_outcome.go):
// "not performed" (codes.Unavailable with VM_OPERATION_FAILED) proves nothing
// was done and stays out of the circuit breaker; a typed refusal maps to its
// contracts error; an untyped codes.Unknown proves nothing.
func TestMapGRPCError_SingleHostLibvirtDefiniteAnswers(t *testing.T) {
	c := &Client{}
	const op = "exportDisk"

	notPerformed := createNotPerformedErr(t, contracts.VMOperationFailedReason)
	mapped := c.mapGRPCError(op, notPerformed)
	assert.True(t, contracts.IsNothingDone(mapped), "a not-performed answer proves nothing was done")
	assert.True(t, contracts.IsRetryable(mapped))
	assert.False(t, isInfraFailure(notPerformed), "one tenant's failing export must not open the Provider's breaker")
	assert.False(t, countsTowardBreaker("/provider.v1.Provider/ExportDisk", notPerformed, false))

	assert.True(t, contracts.IsNotFound(c.mapGRPCError(op, status.Error(codes.NotFound, "the libvirt domain was not found on the host"))))
	assert.True(t, contracts.IsInvalidSpec(c.mapGRPCError(op, status.Error(codes.InvalidArgument, "bad"))))
	assert.True(t, contracts.IsConflict(c.mapGRPCError(op, status.Error(codes.AlreadyExists, "a snapshot with this name already exists"))))
	assert.True(t, contracts.IsNotSupported(c.mapGRPCError(op, status.Error(codes.Unimplemented, "no"))))

	// A non-retryable FailedPrecondition and a PermissionDenied keep their
	// status in the chain: the manager reads them as refusals.
	for _, code := range []codes.Code{codes.FailedPrecondition, codes.PermissionDenied} {
		err := c.mapGRPCError(op, status.Error(code, "refused"))
		st, ok := status.FromError(err)
		require.True(t, ok, code.String())
		assert.Equal(t, code, st.Code())
		assert.False(t, contracts.IsNothingDone(err))
	}

	// An untyped answer - what every failure was before R21 - proves nothing.
	untyped := c.mapGRPCError(op, status.Error(codes.Unknown, "host command failed"))
	assert.False(t, contracts.IsNothingDone(untyped))
	assert.False(t, contracts.IsNotFound(untyped) || contracts.IsInvalidSpec(untyped) || contracts.IsConflict(untyped))
	assert.True(t, isInfraFailure(status.Error(codes.Unknown, "host command failed")), "an untyped failure still counts")
}

// TestMapGRPCError_RoutedSnapshotCreateAnswers (V8) pins how the manager reads
// a clustered libvirt provider's answers to a routed SnapshotCreate: a disk
// in-use check that could not run (VM_DISK_CHECK_FAILED with
// VM_OPERATION_FAILED, the rc7 lab's answer) and a host that was unavailable
// prove nothing was done; an OUTCOME_UNKNOWN_RETRY (sent with
// VM_OPERATION_FAILED) proves nothing.
func TestMapGRPCError_RoutedSnapshotCreateAnswers(t *testing.T) {
	c := &Client{}
	const op = "snapshotCreate"
	for name, tc := range map[string]struct {
		reasons      []string
		nothingDone  bool
		unknownRetry bool
	}{
		"disk check failed": {[]string{contracts.VMDiskCheckFailedReason, contracts.VMOperationFailedReason}, true, false},
		"host unavailable":  {[]string{contracts.HostUnavailableReason}, true, false},
		"outcome unknown":   {[]string{contracts.OutcomeUnknownRetryReason, contracts.VMOperationFailedReason}, false, true},
	} {
		mapped := c.mapGRPCError(op, createNotPerformedErr(t, tc.reasons...))
		assert.True(t, contracts.IsRetryable(mapped), name)
		assert.Equal(t, tc.nothingDone, contracts.IsNothingDone(mapped), name)
		assert.Equal(t, tc.unknownRetry, contracts.IsOutcomeUnknownRetry(mapped), name)
	}
}
