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

// These tests pin host-scoped unavailability (ADR-0007 Addendum A): a clustered
// provider marks "this one host is unknown/draining/unreachable" as
// codes.Unavailable + ErrorInfo{Reason: HOST_UNAVAILABLE}. The manager maps it
// to contracts.ErrorTypeHostUnavailable and keeps it OUT of the per-Provider
// circuit breaker, so one dead host cannot fast-fail every VM on every other
// host of the Provider — while a genuine provider-level Unavailable still trips
// the breaker.

// hostUnavailableErr builds the status a clustered provider returns for a
// host-scoped unavailability.
func hostUnavailableErr(t *testing.T, reason, domain string) error {
	t.Helper()
	st, err := status.New(codes.Unavailable, "connect to host \"host-a\": hostconn: no connection for host").
		WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: domain})
	require.NoError(t, err)
	return st.Err()
}

func TestIsInfraFailure_HostUnavailableIsNotInfra(t *testing.T) {
	assert.False(t, isInfraFailure(hostUnavailableErr(t, contracts.HostUnavailableReason, contracts.HostUnavailableErrorDomain)),
		"a host-scoped Unavailable says nothing about the provider's health")
	assert.True(t, isInfraFailure(status.Error(codes.Unavailable, "provider down")),
		"a provider-level Unavailable still counts")
	assert.True(t, isInfraFailure(hostUnavailableErr(t, "SOMETHING_ELSE", contracts.HostUnavailableErrorDomain)),
		"only the HOST_UNAVAILABLE reason is exempt")
	assert.True(t, isInfraFailure(hostUnavailableErr(t, contracts.HostUnavailableReason, "example.com")),
		"only VirtRigaud's error domain is exempt")
}

// describeErrServer answers every Describe with err and counts the calls.
type describeErrServer struct {
	providerv1.UnimplementedProviderServer
	err   error
	calls atomic.Int32
}

func (s *describeErrServer) Describe(context.Context, *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	s.calls.Add(1)
	return nil, s.err
}

func TestCircuitBreaker_HostUnavailableDoesNotTrip(t *testing.T) {
	srv := &describeErrServer{err: hostUnavailableErr(t, contracts.HostUnavailableReason, contracts.HostUnavailableErrorDomain)}
	dialer, cleanup := startBufconnServer(t, srv)
	defer cleanup()
	cli, cb := newTestClientWithCB(t, dialer, "adr7-host", "adr7-host-provider", &resilience.Config{
		FailureThreshold: 2,
		ResetTimeout:     30 * time.Second,
		HalfOpenMaxCalls: 1,
	})

	for i := 0; i < 6; i++ {
		_, err := cli.Describe(context.Background(), contracts.VMRef{ID: "web", HostID: "host-a"})
		require.Error(t, err)
		assert.True(t, contracts.IsHostUnavailable(err), "mapped to the host-scoped class: %v", err)
		assert.True(t, contracts.IsRetryable(err), "and it stays retryable")
	}
	assert.EqualValues(t, 6, srv.calls.Load(), "every call reached the provider (breaker never opened)")
	assert.Equal(t, resilience.StateClosed, cb.GetState())
	assert.Zero(t, cb.GetFailures(), "host-scoped unavailability never counts toward the breaker")
}

// routedOpErrServer answers every Power and Reconfigure with err and counts the
// calls (ADR-0007 Addendum A, slice 2: both are routed and owner-checked).
type routedOpErrServer struct {
	providerv1.UnimplementedProviderServer
	err   error
	calls atomic.Int32
}

func (s *routedOpErrServer) Power(context.Context, *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	s.calls.Add(1)
	return nil, s.err
}

func (s *routedOpErrServer) Reconfigure(context.Context, *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	s.calls.Add(1)
	return nil, s.err
}

// TestCircuitBreaker_RoutedPowerAndReconfigureHostErrorsDoNotTrip: a routed
// Power or Reconfigure answered with a host-scoped Unavailable, or with
// NotFound (a domain this VM does not own), maps to its typed class and never
// counts toward the per-Provider breaker — one bad host or one foreign domain
// cannot fast-fail the Provider's other VMs.
func TestCircuitBreaker_RoutedPowerAndReconfigureHostErrorsDoNotTrip(t *testing.T) {
	cases := map[string]struct {
		err   error
		class func(error) bool
	}{
		"host unavailable": {hostUnavailableErr(t, contracts.HostUnavailableReason, contracts.HostUnavailableErrorDomain), contracts.IsHostUnavailable},
		"not owned":        {status.Error(codes.NotFound, `libvirt domain "web" on host host-a is not owned by this VirtualMachine`), contracts.IsNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := &routedOpErrServer{err: tc.err}
			dialer, cleanup := startBufconnServer(t, srv)
			defer cleanup()
			cli, cb := newTestClientWithCB(t, dialer, "adr7-ops", "adr7-ops-provider", &resilience.Config{
				FailureThreshold: 2,
				ResetTimeout:     30 * time.Second,
				HalfOpenMaxCalls: 1,
			})
			vm := contracts.VMRef{ID: "web", HostID: "host-a", Owner: contracts.ObjectIdentity{UID: "uid-web"}}
			for i := 0; i < 3; i++ {
				_, err := cli.Power(context.Background(), vm, contracts.PowerOpOn)
				require.Error(t, err)
				assert.True(t, tc.class(err), "power mapped to its class: %v", err)
				_, err = cli.Reconfigure(context.Background(), vm, contracts.CreateRequest{Name: "web"})
				require.Error(t, err)
				assert.True(t, tc.class(err), "reconfigure mapped to its class: %v", err)
			}
			assert.EqualValues(t, 6, srv.calls.Load(), "every call reached the provider (breaker never opened)")
			assert.Equal(t, resilience.StateClosed, cb.GetState())
			assert.Zero(t, cb.GetFailures())
		})
	}
}

// vmOperationFailedErr builds the status a clustered provider returns for a
// per-VM operation that reached its host and failed there.
func vmOperationFailedErr(t *testing.T, domain string) error {
	t.Helper()
	st, err := status.New(codes.Unknown, "failed to reconfigure VM: Retryable: online disk grow failed (caused by: blockresize ...)").
		WithDetails(&errdetails.ErrorInfo{Reason: contracts.VMOperationFailedReason, Domain: domain})
	require.NoError(t, err)
	return st.Err()
}

func TestIsInfraFailure_VMOperationFailedIsNotInfra(t *testing.T) {
	assert.False(t, isInfraFailure(vmOperationFailedErr(t, contracts.ErrorInfoDomain)),
		"one VM's failed operation says nothing about the provider's health")
	assert.True(t, isInfraFailure(vmOperationFailedErr(t, "example.com")), "only VirtRigaud's error domain is exempt")
	assert.True(t, isInfraFailure(status.Error(codes.Unknown, "failed to reconfigure VM: boom")),
		"a plain Unknown (no detail: a single-host provider, or a provider-level failure) still counts")
}

// TestCircuitBreaker_RepeatedVMOperationFailuresDoNotTrip is the slice 2
// review's scenario: one tenant's VM whose Reconfigure keeps failing on its
// host (retried every few seconds) must not open the breaker for every VM of
// the Provider — while the same failures WITHOUT the detail still do.
func TestCircuitBreaker_RepeatedVMOperationFailuresDoNotTrip(t *testing.T) {
	vm := contracts.VMRef{ID: "web", HostID: "host-a", Owner: contracts.ObjectIdentity{UID: "uid-web"}}
	newCB := func(name string, err error) (*routedOpErrServer, *Client, *resilience.CircuitBreaker) {
		srv := &routedOpErrServer{err: err}
		dialer, cleanup := startBufconnServer(t, srv)
		t.Cleanup(cleanup)
		cli, cb := newTestClientWithCB(t, dialer, name, name+"-provider", &resilience.Config{
			FailureThreshold: 2,
			ResetTimeout:     30 * time.Second,
			HalfOpenMaxCalls: 1,
		})
		return srv, cli, cb
	}

	srv, cli, cb := newCB("adr7-vmop", vmOperationFailedErr(t, contracts.ErrorInfoDomain))
	for i := 0; i < 5; i++ {
		_, err := cli.Reconfigure(context.Background(), vm, contracts.CreateRequest{Name: "web"})
		require.Error(t, err)
		_, err = cli.Power(context.Background(), vm, contracts.PowerOpOn)
		require.Error(t, err)
	}
	assert.EqualValues(t, 10, srv.calls.Load(), "every call reached the provider (breaker never opened)")
	assert.Equal(t, resilience.StateClosed, cb.GetState())

	plainSrv, plainCli, plainCB := newCB("adr7-plain", status.Error(codes.Unknown, "provider internal failure"))
	for i := 0; i < 3; i++ {
		_, err := plainCli.Power(context.Background(), vm, contracts.PowerOpOn)
		require.Error(t, err)
	}
	assert.Equal(t, resilience.StateOpen, plainCB.GetState(), "a plain Unknown still opens the breaker")
	assert.EqualValues(t, 2, plainSrv.calls.Load())
}

func TestCircuitBreaker_ProviderUnavailableStillTrips(t *testing.T) {
	srv := &describeErrServer{err: status.Error(codes.Unavailable, "provider pod is down")}
	dialer, cleanup := startBufconnServer(t, srv)
	defer cleanup()
	cli, cb := newTestClientWithCB(t, dialer, "adr7-prov", "adr7-prov-provider", &resilience.Config{
		FailureThreshold: 2,
		ResetTimeout:     30 * time.Second,
		HalfOpenMaxCalls: 1,
	})

	for i := 0; i < 3; i++ {
		_, err := cli.Describe(context.Background(), contracts.VMRef{ID: "web", HostID: "host-a"})
		require.Error(t, err)
		assert.False(t, contracts.IsHostUnavailable(err))
	}
	assert.Equal(t, resilience.StateOpen, cb.GetState(), "a provider-level Unavailable still opens the breaker")
	assert.EqualValues(t, 2, srv.calls.Load(), "the third call was short-circuited")
}
