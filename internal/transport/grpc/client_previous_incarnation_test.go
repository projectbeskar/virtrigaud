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

// TestMapGRPCError_VMPreviousIncarnationIsMarkedConflict pins how the manager
// reads a clustered provider's refusal of a Create or Clone because a previous
// incarnation of the VM exists on a host of the Provider (ADR-0007 A6, R2):
// AlreadyExists with VM_PREVIOUS_INCARNATION is a typed, non-retryable
// Conflict that the controllers can tell apart (they hold the VM instead of
// excluding the host), never counted toward the circuit breaker. A plain
// AlreadyExists, or the reason in another ErrorInfo domain or on another
// code, is an ordinary Conflict (or not one at all).
func TestMapGRPCError_VMPreviousIncarnationIsMarkedConflict(t *testing.T) {
	const msg = `libvirt domain "team-a.web" was not created: a previous incarnation of this VirtualMachine exists`
	st := status.New(codes.AlreadyExists, msg)
	st, err := st.WithDetails(&errdetails.ErrorInfo{Reason: contracts.VMPreviousIncarnationReason, Domain: contracts.ErrorInfoDomain})
	require.NoError(t, err)

	for _, op := range []string{"create", "clone"} {
		mapped := (&Client{}).mapGRPCError(op, st.Err())
		assert.True(t, contracts.IsConflict(mapped), "%s: %v", op, mapped)
		assert.True(t, contracts.IsVMPreviousIncarnation(mapped), "%s: the controllers tell it from another Conflict", op)
		assert.False(t, contracts.IsVMDiskInUse(mapped))
		assert.False(t, contracts.IsRetryable(mapped))
		assert.Contains(t, mapped.Error(), "previous incarnation")
	}
	for _, m := range []string{providerv1.Provider_Create_FullMethodName, providerv1.Provider_Clone_FullMethodName} {
		assert.False(t, countsTowardBreaker(m, st.Err()), "%s: the provider answered", m)
	}

	assert.False(t, contracts.IsVMOwnDomainElsewhere((&Client{}).mapGRPCError("create", st.Err())),
		"no metadata: a previous incarnation under another UID")
	own := status.New(codes.AlreadyExists, "a domain of this VirtualMachine — stamped with its own UID — already exists")
	own, err = own.WithDetails(&errdetails.ErrorInfo{Reason: contracts.VMPreviousIncarnationReason, Domain: contracts.ErrorInfoDomain,
		Metadata: map[string]string{contracts.VMPreviousIncarnationKindKey: contracts.VMPreviousIncarnationKindOwn}})
	require.NoError(t, err)
	ownMapped := (&Client{}).mapGRPCError("create", own.Err())
	assert.True(t, contracts.IsVMPreviousIncarnation(ownMapped), "held like any previous incarnation")
	assert.True(t, contracts.IsVMOwnDomainElsewhere(ownMapped), "and marked as the VM's own domain")

	plain := (&Client{}).mapGRPCError("create", status.Error(codes.AlreadyExists, "taken"))
	assert.True(t, contracts.IsConflict(plain))
	assert.False(t, contracts.IsVMPreviousIncarnation(plain), "a plain AlreadyExists keeps the slice 2 exclusion")

	foreign := status.New(codes.AlreadyExists, "x")
	foreign, err = foreign.WithDetails(&errdetails.ErrorInfo{Reason: contracts.VMPreviousIncarnationReason, Domain: "example.com"})
	require.NoError(t, err)
	assert.False(t, contracts.IsVMPreviousIncarnation((&Client{}).mapGRPCError("create", foreign.Err())),
		"another domain's reason is not ours")

	wrongCode := status.New(codes.FailedPrecondition, "x")
	wrongCode, err = wrongCode.WithDetails(&errdetails.ErrorInfo{Reason: contracts.VMPreviousIncarnationReason, Domain: contracts.ErrorInfoDomain})
	require.NoError(t, err)
	assert.False(t, contracts.IsVMPreviousIncarnation((&Client{}).mapGRPCError("create", wrongCode.Err())),
		"the reason is honoured on AlreadyExists only")
}

// TestClusterDiskGuardFailures_NeverCounted pins the two ways a clustered
// provider answers a Create, Clone or Delete whose cluster-wide disk guard
// could not check every host (ADR-0007 A6, R3): HOST_UNAVAILABLE (a host
// could not be reached) and VM_DISK_CHECK_FAILED + VM_OPERATION_FAILED (a
// host answered but could not be scanned). Both are retryable and neither
// counts toward the per-Provider circuit breaker.
func TestClusterDiskGuardFailures_NeverCounted(t *testing.T) {
	unreachable := status.New(codes.Unavailable, "create VM: a host of this Provider could not be reached")
	unreachable, err := unreachable.WithDetails(&errdetails.ErrorInfo{Reason: contracts.HostUnavailableReason, Domain: contracts.HostUnavailableErrorDomain})
	require.NoError(t, err)
	checkFailed := status.New(codes.Unavailable, "create VM: could not verify that no other VM uses the disk")
	for _, r := range []string{contracts.VMDiskCheckFailedReason, contracts.VMOperationFailedReason} {
		checkFailed, err = checkFailed.WithDetails(&errdetails.ErrorInfo{Reason: r, Domain: contracts.ErrorInfoDomain})
		require.NoError(t, err)
	}
	for _, st := range []*status.Status{unreachable, checkFailed} {
		for _, m := range []string{providerv1.Provider_Create_FullMethodName, providerv1.Provider_Clone_FullMethodName,
			providerv1.Provider_Delete_FullMethodName} {
			assert.False(t, countsTowardBreaker(m, st.Err()), "%s: %s", m, st.Message())
		}
		assert.True(t, contracts.IsRetryable((&Client{}).mapGRPCError("create", st.Err())), st.Message())
	}
	assert.True(t, contracts.IsHostUnavailable((&Client{}).mapGRPCError("create", unreachable.Err())))
	assert.False(t, contracts.IsHostUnavailable((&Client{}).mapGRPCError("create", checkFailed.Err())))
	assert.True(t, contracts.IsVMDiskCheckFailed((&Client{}).mapGRPCError("delete", checkFailed.Err())),
		"marked, so a controller can back off and say why")
	assert.False(t, contracts.IsVMDiskCheckFailed((&Client{}).mapGRPCError("delete", unreachable.Err())))
	assert.False(t, contracts.IsVMDiskCheckFailed((&Client{}).mapGRPCError("delete", status.Error(codes.Unavailable, "down"))))
}

// guardErrServer answers every Create, Clone and Delete with err and counts
// the calls.
type guardErrServer struct {
	providerv1.UnimplementedProviderServer
	err   error
	calls atomic.Int32
}

func (s *guardErrServer) Create(context.Context, *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	s.calls.Add(1)
	return nil, s.err
}

func (s *guardErrServer) Clone(context.Context, *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	s.calls.Add(1)
	return nil, s.err
}

func (s *guardErrServer) Delete(context.Context, *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	s.calls.Add(1)
	return nil, s.err
}

// TestCircuitBreaker_ClusterDiskGuardAnswersDoNotTrip drives the three
// answers of the cluster-wide disk guard (ADR-0007 A6.1) — a previous
// incarnation, a host that could not be reached, a host that could not be
// scanned — through the real client and circuit breaker on Create, Clone and
// Delete: every call reaches the provider and the breaker never counts one.
func TestCircuitBreaker_ClusterDiskGuardAnswersDoNotTrip(t *testing.T) {
	previous := status.New(codes.AlreadyExists, "create of libvirt domain \"team-a.web\" refused: a previous incarnation")
	previous, err := previous.WithDetails(&errdetails.ErrorInfo{Reason: contracts.VMPreviousIncarnationReason, Domain: contracts.ErrorInfoDomain})
	require.NoError(t, err)
	unreachable := status.New(codes.Unavailable, "a host of this Provider could not be reached")
	unreachable, err = unreachable.WithDetails(&errdetails.ErrorInfo{Reason: contracts.HostUnavailableReason, Domain: contracts.HostUnavailableErrorDomain})
	require.NoError(t, err)
	checkFailed := status.New(codes.Unavailable, "could not verify on every host of this Provider")
	for _, r := range []string{contracts.VMDiskCheckFailedReason, contracts.VMOperationFailedReason} {
		checkFailed, err = checkFailed.WithDetails(&errdetails.ErrorInfo{Reason: r, Domain: contracts.ErrorInfoDomain})
		require.NoError(t, err)
	}
	for name, st := range map[string]*status.Status{"previous incarnation": previous, "host unreachable": unreachable, "check failed": checkFailed} {
		t.Run(name, func(t *testing.T) {
			srv := &guardErrServer{err: st.Err()}
			dialer, cleanup := startBufconnServer(t, srv)
			defer cleanup()
			cli, cb := newTestClientWithCB(t, dialer, "a61-guard", "a61-guard-provider", &resilience.Config{
				FailureThreshold: 2,
				ResetTimeout:     30 * time.Second,
				HalfOpenMaxCalls: 1,
			})
			owner := contracts.ObjectIdentity{UID: "uid-web", Namespace: "team-a", Name: "web"}
			ctx := context.Background()
			for i := 0; i < 3; i++ {
				_, cerr := cli.Create(ctx, contracts.CreateRequest{Name: "web", TargetHostID: "host-b", Owner: owner})
				require.Error(t, cerr)
				_, kerr := cli.Clone(ctx, contracts.CloneRequest{
					Source: contracts.VMRef{ID: "team-a.src", HostID: "host-b", Owner: owner}, TargetHostID: "host-b",
					TargetName: "web", TargetVM: owner,
				})
				require.Error(t, kerr)
				_, derr := cli.Delete(ctx, contracts.VMRef{ID: "team-a.web", HostID: "host-b", Owner: owner})
				require.Error(t, derr)
			}
			assert.EqualValues(t, 9, srv.calls.Load(), "every call reached the provider (the breaker never opened)")
			assert.Equal(t, resilience.StateClosed, cb.GetState())
			assert.Zero(t, cb.GetFailures(), "no answer of the guard counts toward the breaker")
		})
	}
}
