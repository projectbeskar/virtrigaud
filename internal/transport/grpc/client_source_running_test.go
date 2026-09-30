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
)

// These tests pin the manager's side of the libvirt clone source-state
// refusal (ADR-0007 Slice 5 lab, B2): a libvirt full clone requires a
// powered-off source, and the provider refuses one that is not with
// codes.FailedPrecondition + ErrorInfo{Reason: VM_SOURCE_RUNNING} (plus
// VM_OPERATION_FAILED on a clustered provider). The manager maps it to a
// retryable error marked contracts.ErrVMSourceRunning — so the VMClone
// controller keeps the clone Pending instead of failing it — and never counts
// it toward the per-Provider circuit breaker.

// sourceRunningErr builds the provider's refusal with the given ErrorInfo
// reasons, in domain.
func sourceRunningErr(t *testing.T, domain string, reasons ...string) error {
	t.Helper()
	st := status.New(codes.FailedPrecondition,
		`failed to clone VM: the clone's source VM is "running": power off the source VM to clone it`)
	for _, r := range reasons {
		var err error
		st, err = st.WithDetails(&errdetails.ErrorInfo{Reason: r, Domain: domain})
		require.NoError(t, err)
	}
	return st.Err()
}

func TestMapGRPCError_SourceRunningIsRetryableAndMarked(t *testing.T) {
	c := &Client{}
	for name, err := range map[string]error{
		"single-host": sourceRunningErr(t, contracts.ErrorInfoDomain, contracts.VMSourceRunningReason),
		"clustered": sourceRunningErr(t, contracts.ErrorInfoDomain,
			contracts.VMSourceRunningReason, contracts.VMOperationFailedReason),
	} {
		t.Run(name, func(t *testing.T) {
			mapped := c.mapGRPCError("clone", err)
			assert.True(t, contracts.IsVMSourceRunning(mapped), "marked: %v", mapped)
			assert.True(t, contracts.IsRetryable(mapped), "retried until the source is powered off")
			assert.False(t, contracts.IsConflict(mapped), "not the disk-in-use Conflict")
			assert.Contains(t, mapped.Error(), "power off the source VM to clone it")
		})
	}

	for name, err := range map[string]error{
		"another domain's reason": sourceRunningErr(t, "example.com", contracts.VMSourceRunningReason),
		"no reason":               sourceRunningErr(t, contracts.ErrorInfoDomain),
		"another code": func() error {
			st, err := status.New(codes.Unknown, "x").WithDetails(&errdetails.ErrorInfo{
				Reason: contracts.VMSourceRunningReason, Domain: contracts.ErrorInfoDomain})
			require.NoError(t, err)
			return st.Err()
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			assert.False(t, contracts.IsVMSourceRunning(c.mapGRPCError("clone", err)), "only VirtRigaud's FailedPrecondition is the refusal")
		})
	}
}

func TestSourceRunning_NeverCountsTowardTheBreaker(t *testing.T) {
	for _, err := range []error{
		sourceRunningErr(t, contracts.ErrorInfoDomain, contracts.VMSourceRunningReason),
		sourceRunningErr(t, contracts.ErrorInfoDomain, contracts.VMSourceRunningReason, contracts.VMOperationFailedReason),
	} {
		assert.False(t, isInfraFailure(err))
		assert.False(t, countsTowardBreaker(providerv1.Provider_Clone_FullMethodName, err))
	}
}

// cloneErrServer answers every Clone with err and counts the calls.
type cloneErrServer struct {
	providerv1.UnimplementedProviderServer
	err   error
	calls atomic.Int32
}

func (s *cloneErrServer) Clone(context.Context, *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	s.calls.Add(1)
	return nil, s.err
}

// TestCircuitBreaker_RepeatedSourceRunningRefusalsDoNotTrip: a VMClone whose
// source stays running is retried with a backoff for as long as it waits;
// every refusal reaches the manager over gRPC with its detail, maps to the
// marked retryable error, and the breaker never opens.
func TestCircuitBreaker_RepeatedSourceRunningRefusalsDoNotTrip(t *testing.T) {
	srv := &cloneErrServer{err: sourceRunningErr(t, contracts.ErrorInfoDomain,
		contracts.VMSourceRunningReason, contracts.VMOperationFailedReason)}
	dialer, cleanup := startBufconnServer(t, srv)
	defer cleanup()
	cli, cb := newTestClientWithCB(t, dialer, "b2-source", "b2-source-provider", &resilience.Config{
		FailureThreshold: 2,
		ResetTimeout:     30 * time.Second,
		HalfOpenMaxCalls: 1,
	})
	req := contracts.CloneRequest{
		Source:       contracts.VMRef{ID: "web", HostID: "host-a", Owner: contracts.ObjectIdentity{UID: "uid-web", Namespace: "team-a", Name: "web"}},
		TargetHostID: "host-a", TargetName: "copy",
		TargetVM: contracts.ObjectIdentity{UID: "uid-copy", Namespace: "team-a", Name: "copy"},
	}
	for i := 0; i < 5; i++ {
		_, err := cli.Clone(context.Background(), req)
		require.Error(t, err)
		assert.True(t, contracts.IsVMSourceRunning(err), "mapped to the marked retryable class: %v", err)
	}
	assert.EqualValues(t, 5, srv.calls.Load(), "every call reached the provider (breaker never opened)")
	assert.Equal(t, resilience.StateClosed, cb.GetState())
	assert.Zero(t, cb.GetFailures())
}
