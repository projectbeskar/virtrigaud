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
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin, over a real gRPC connection, which failed Clone calls are
// proven to have done nothing (contracts.IsNothingDone) — and so may be sent
// again although a single-host Clone carries no request token — and which
// have an unknown outcome.

// cloneHandlerServer answers Clone with handler.
type cloneHandlerServer struct {
	providerv1.UnimplementedProviderServer
	handler func(ctx context.Context) (*providerv1.CloneResponse, error)
	calls   atomic.Int32
}

func (s *cloneHandlerServer) Clone(ctx context.Context, _ *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	s.calls.Add(1)
	return s.handler(ctx)
}

// newSentTrackingClient builds a Client with the production call options
// (providerCallDialOptions, shared with NewClient) over dialer.
func newSentTrackingClient(t *testing.T, dialer func(context.Context, string) (net.Conn, error), cb *resilience.CircuitBreaker) *Client {
	t.Helper()
	opts := append([]grpc.DialOption{
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, providerCallDialOptions("sent-test", cb)...)
	conn, err := grpc.NewClient("passthrough:///bufnet", opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return &Client{conn: conn, client: providerv1.NewProviderClient(conn), vmOps: metrics.NewVMOperationMetrics("sent-test", "p")}
}

func cloneReqForSentTest() contracts.CloneRequest {
	return contracts.CloneRequest{Source: contracts.VMRef{ID: "vm-1"}, TargetName: "copy",
		TargetVM: contracts.ObjectIdentity{Namespace: "team-a", Name: "copy"}}
}

func TestClone_NoConnectionIsProvenNotSent(t *testing.T) {
	refused := func(context.Context, string) (net.Conn, error) { return nil, errors.New("connection refused") }
	cli := newSentTrackingClient(t, refused, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := cli.Clone(ctx, cloneReqForSentTest())
	require.Error(t, err)
	assert.True(t, contracts.IsRequestNotSent(err), "no connection was ever made: %v", err)
	assert.True(t, contracts.IsNothingDone(err))
	assert.True(t, contracts.IsRetryable(err))
}

func TestClone_BreakerFastFailIsProvenNotSent(t *testing.T) {
	srv := &cloneHandlerServer{handler: func(context.Context) (*providerv1.CloneResponse, error) {
		return nil, status.Error(codes.Unavailable, "provider is down")
	}}
	dialer, cleanup := startBufconnServer(t, srv)
	defer cleanup()
	cb := resilience.NewCircuitBreaker("rpc", "sent-test", "p", &resilience.Config{
		FailureThreshold: 1, ResetTimeout: time.Minute, HalfOpenMaxCalls: 1})
	cli := newSentTrackingClient(t, dialer, cb)

	_, err := cli.Clone(context.Background(), cloneReqForSentTest())
	require.Error(t, err)
	assert.False(t, contracts.IsNothingDone(err), "the provider got it and answered a bare Unavailable: outcome unknown")

	_, err = cli.Clone(context.Background(), cloneReqForSentTest())
	require.Error(t, err)
	assert.EqualValues(t, 1, srv.calls.Load(), "the breaker refused the second call")
	assert.True(t, contracts.IsRequestNotSent(err), "a fast-fail never reached the provider: %v", err)
}

func TestClone_TransportFailureMidRPCIsOutcomeUnknown(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	gsrv := grpc.NewServer()
	entered := make(chan struct{})
	srv := &cloneHandlerServer{handler: func(ctx context.Context) (*providerv1.CloneResponse, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	providerv1.RegisterProviderServer(gsrv, srv)
	go func() { _ = gsrv.Serve(lis) }()
	dialer := func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }
	cli := newSentTrackingClient(t, dialer, nil)

	go func() {
		<-entered
		gsrv.Stop() // the provider pod dies while it runs the Clone
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := cli.Clone(ctx, cloneReqForSentTest())
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
	assert.False(t, contracts.IsRequestNotSent(err), "the request reached the provider: %v", err)
	assert.False(t, contracts.IsNothingDone(err), "outcome unknown: never sent again")
	_ = lis.Close()
}

func TestClone_ProviderAnswerThatItDidNothing(t *testing.T) {
	for name, reason := range map[string]string{
		"operation not performed": contracts.VMOperationFailedReason,
		"host unavailable":        contracts.HostUnavailableReason,
		"disk check failed":       contracts.VMDiskCheckFailedReason,
	} {
		t.Run(name, func(t *testing.T) {
			srv := &cloneHandlerServer{handler: func(context.Context) (*providerv1.CloneResponse, error) {
				st, err := status.New(codes.Unavailable, "failed to clone VM: the libvirt host could not be reached").
					WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: contracts.ErrorInfoDomain})
				require.NoError(t, err)
				return nil, st.Err()
			}}
			dialer, cleanup := startBufconnServer(t, srv)
			defer cleanup()
			cli := newSentTrackingClient(t, dialer, nil)

			_, err := cli.Clone(context.Background(), cloneReqForSentTest())
			require.Error(t, err)
			assert.False(t, contracts.IsRequestNotSent(err))
			assert.True(t, contracts.IsNothingDone(err), "the provider says it did nothing: %v", err)
		})
	}

	t.Run("a bare Unavailable answer is outcome unknown", func(t *testing.T) {
		srv := &cloneHandlerServer{handler: func(context.Context) (*providerv1.CloneResponse, error) {
			return nil, status.Error(codes.Unavailable, "something went wrong")
		}}
		dialer, cleanup := startBufconnServer(t, srv)
		defer cleanup()
		_, err := newSentTrackingClient(t, dialer, nil).Clone(context.Background(), cloneReqForSentTest())
		require.Error(t, err)
		assert.False(t, contracts.IsNothingDone(err))
	})
}

// TestClone_WithoutTheStatsHandlerNothingIsProvenNotSent (re-review L-c): the
// tracking fails closed. A client built without the stats handler sees no
// Begin, so even an RPC that never connected is outcome-unknown — never
// "nothing done".
func TestClone_WithoutTheStatsHandlerNothingIsProvenNotSent(t *testing.T) {
	refused := func(context.Context, string) (net.Conn, error) { return nil, errors.New("connection refused") }
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(refused),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
		grpc.WithChainUnaryInterceptor(requestSentInterceptor()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	cli := &Client{conn: conn, client: providerv1.NewProviderClient(conn), vmOps: metrics.NewVMOperationMetrics("sent-test", "p")}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = cli.Clone(ctx, cloneReqForSentTest())
	require.Error(t, err)
	assert.False(t, contracts.IsRequestNotSent(err), "no Begin reported: nothing is proven: %v", err)
	assert.False(t, contracts.IsNothingDone(err))
}

// TestNewClient_UsesTheSharedCallOptions pins that the production client is
// built with the request-sent tracking: an RPC to an endpoint nothing listens
// on is proven not sent.
func TestNewClient_UsesTheSharedCallOptions(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	require.NoError(t, lis.Close()) // nothing listens there now

	cli, err := NewClient(context.Background(), addr, "sent-test", "p", nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err = cli.Clone(ctx, cloneReqForSentTest())
	require.Error(t, err)
	assert.True(t, contracts.IsRequestNotSent(err), "%v", err)
}
