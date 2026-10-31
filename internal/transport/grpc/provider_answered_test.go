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
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	sdkerrors "github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// These tests pin the "provider answered" detector (request_sent.go) over
// real gRPC connections: an RPC whose status came from the provider's gRPC
// server is answered; one whose status grpc-go synthesized on the client
// (no connection, transport closed, client deadline, a non-gRPC HTTP
// response, the breaker's fast-fail) is not. The circuit breaker relies on
// it (countsTowardBreaker), so each shape is exercised end to end.

// answeredProbe is a unary interceptor that records, after the RPC, what the
// request-sent tracking saw for it.
type answeredProbe struct {
	called   atomic.Bool
	answered atomic.Bool
	begun    atomic.Bool
}

func (p *answeredProbe) interceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		err := invoker(ctx, method, req, reply, cc, opts...)
		p.called.Store(true)
		if rs, ok := requestSentFrom(ctx); ok {
			p.answered.Store(rs.providerAnswered())
			p.begun.Store(rs.begun.Load())
		}
		return err
	}
}

// probeClient dials target with the production call options
// (providerCallDialOptions, no breaker) plus the probe as the innermost
// interceptor.
func probeClient(t *testing.T, target string, extra ...grpc.DialOption) (providerv1.ProviderClient, *answeredProbe) {
	t.Helper()
	p := &answeredProbe{}
	opts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
		providerCallDialOptions("answered-probe", nil)...)
	opts = append(opts, grpc.WithChainUnaryInterceptor(p.interceptor()))
	opts = append(opts, extra...)
	conn, err := grpc.NewClient(target, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return providerv1.NewProviderClient(conn), p
}

// describeServer answers Describe with handler.
type describeServer struct {
	providerv1.UnimplementedProviderServer
	handler func(ctx context.Context) (*providerv1.DescribeResponse, error)
}

func (s *describeServer) Describe(ctx context.Context, _ *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	return s.handler(ctx)
}

// serveLoopback serves srv on a loopback TCP port and returns its address
// and the gRPC server.
func serveLoopback(t *testing.T, srv providerv1.ProviderServer) (string, *grpc.Server) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gsrv := grpc.NewServer()
	providerv1.RegisterProviderServer(gsrv, srv)
	go func() { _ = gsrv.Serve(lis) }()
	t.Cleanup(gsrv.Stop)
	return lis.Addr().String(), gsrv
}

// callDescribe sends one Describe with timeout and returns its error.
func callDescribe(cli providerv1.ProviderClient, timeout time.Duration, opts ...grpc.CallOption) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := cli.Describe(ctx, &providerv1.DescribeRequest{Id: "vm-1"}, opts...)
	return err
}

// TestProviderAnswered_HandlerErrors: (1) a handler's plain error (grpc-go
// sends it as Unknown) and (2) explicit statuses — Internal, Unavailable,
// with an ErrorInfo, sdk errors — are answers. grpc-go sends a handler
// error before any header as a trailers-only response (3); a handler that
// sent its headers first is answered too.
func TestProviderAnswered_HandlerErrors(t *testing.T) {
	withInfo := withReason(t, codes.Unavailable, contracts.VMOperationFailedReason, contracts.ErrorInfoDomain)
	cases := []struct {
		name    string
		handler func(ctx context.Context) (*providerv1.DescribeResponse, error)
		code    codes.Code
	}{
		{"success", func(context.Context) (*providerv1.DescribeResponse, error) {
			return &providerv1.DescribeResponse{Exists: true}, nil
		}, codes.OK},
		{"plain error -> Unknown", func(context.Context) (*providerv1.DescribeResponse, error) {
			return nil, errors.New("virDomainGetInfo failed: domain is locked")
		}, codes.Unknown},
		{"status Internal", func(context.Context) (*providerv1.DescribeResponse, error) {
			return nil, status.Error(codes.Internal, "boom")
		}, codes.Internal},
		{"status Unavailable", func(context.Context) (*providerv1.DescribeResponse, error) {
			return nil, status.Error(codes.Unavailable, "PVE client not configured")
		}, codes.Unavailable},
		{"status with ErrorInfo", func(context.Context) (*providerv1.DescribeResponse, error) {
			return nil, withInfo
		}, codes.Unavailable},
		{"sdk NewInternal", func(context.Context) (*providerv1.DescribeResponse, error) {
			return nil, sdkerrors.NewInternal("failed to describe VM", errors.New("api 500"))
		}, codes.Internal},
		{"sdk ToGRPCError of a plain error", func(context.Context) (*providerv1.DescribeResponse, error) {
			return nil, sdkerrors.ToGRPCError(errors.New("vm config unreadable"))
		}, codes.Internal},
		{"headers sent, then error (not trailers-only)", func(ctx context.Context) (*providerv1.DescribeResponse, error) {
			if err := grpc.SendHeader(ctx, metadata.Pairs("x-probe", "1")); err != nil {
				return nil, err
			}
			return nil, errors.New("failed after headers")
		}, codes.Unknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, _ := serveLoopback(t, &describeServer{handler: tc.handler})
			cli, p := probeClient(t, addr)
			err := callDescribe(cli, 5*time.Second)
			assert.Equal(t, tc.code, status.Code(err))
			require.True(t, p.called.Load())
			assert.True(t, p.answered.Load(), "a status the provider's server sent is an answer")
		})
	}
}

// TestProviderAnswered_NoServer: (4) nothing listens (connection refused).
// With WaitForReady (the production default) the RPC ends on its deadline;
// without it, Unavailable at once. Neither is an answer.
func TestProviderAnswered_NoServer(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	require.NoError(t, lis.Close())

	cli, p := probeClient(t, addr)
	err = callDescribe(cli, 300*time.Millisecond)
	assert.Equal(t, codes.DeadlineExceeded, status.Code(err))
	assert.False(t, p.answered.Load(), "connection refused (wait-for-ready) is no answer")

	cli2, p2 := probeClient(t, addr)
	err = callDescribe(cli2, 5*time.Second, grpc.WaitForReady(false))
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.False(t, p2.answered.Load(), "connection refused (fail-fast) is no answer")
}

// TestProviderAnswered_ServerHangs: (5) the provider takes the request and
// does not answer before the client's deadline (overload / hang): no answer.
func TestProviderAnswered_ServerHangs(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	addr, _ := serveLoopback(t, &describeServer{handler: func(context.Context) (*providerv1.DescribeResponse, error) {
		<-release
		return &providerv1.DescribeResponse{}, nil
	}})
	cli, p := probeClient(t, addr)
	err := callDescribe(cli, 200*time.Millisecond)
	assert.Equal(t, codes.DeadlineExceeded, status.Code(err))
	assert.False(t, p.answered.Load(), "the client's own deadline is no answer")
}

// TestProviderAnswered_ConnectionDroppedMidRPC: the provider process goes
// away while the RPC is in flight ("transport is closing"): no answer.
func TestProviderAnswered_ConnectionDroppedMidRPC(t *testing.T) {
	inHandler := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	addr, gsrv := serveLoopback(t, &describeServer{handler: func(context.Context) (*providerv1.DescribeResponse, error) {
		close(inHandler)
		<-release
		return &providerv1.DescribeResponse{}, nil
	}})
	cli, p := probeClient(t, addr)
	errCh := make(chan error, 1)
	go func() { errCh <- callDescribe(cli, 5*time.Second) }()
	<-inHandler
	// Stop closes the listener and every connection; the in-flight RPC fails.
	go gsrv.Stop()
	err := <-errCh
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.False(t, p.answered.Load(), "a dropped connection is no answer")
}

// h2Server serves handler as unencrypted HTTP/2 (prior knowledge, like a
// gRPC client speaks) on a loopback port: a non-gRPC HTTP/2 endpoint such as
// a proxy or load balancer error page.
func h2Server(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: handler, Protocols: protocols, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })
	return lis.Addr().String()
}

// TestProviderAnswered_NonGRPCHTTPResponse: (6) an HTTP/2 endpoint that is
// not a gRPC server (a proxy's 502/503 page, an HTTP 500) answers with a
// non-gRPC content-type; grpc-go derives the status from the HTTP code on the
// client. Not a provider answer — whatever code it maps to.
func TestProviderAnswered_NonGRPCHTTPResponse(t *testing.T) {
	cases := []struct {
		httpStatus int
		code       codes.Code
	}{
		{http.StatusServiceUnavailable, codes.Unavailable},
		{http.StatusBadGateway, codes.Unavailable},
		{http.StatusInternalServerError, codes.Unknown},
		{http.StatusBadRequest, codes.Internal},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.httpStatus), func(t *testing.T) {
			addr := h2Server(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(tc.httpStatus)
				_, _ = w.Write([]byte("upstream connect error"))
			})
			cli, p := probeClient(t, addr)
			err := callDescribe(cli, 5*time.Second)
			assert.Equal(t, tc.code, status.Code(err))
			assert.False(t, p.answered.Load(), "a non-gRPC HTTP response is not a provider answer")
		})
	}
}

// TestProviderAnswered_GRPCContentTypeWithoutStatus documents the edge of
// the detector: a peer that speaks the gRPC content-type but ends the stream
// without a grpc-status (a broken gRPC peer, or a proxy that fakes the
// content-type) IS reported as answered by grpc-go — with codes.Unknown and
// an empty message. countsTowardBreaker therefore does not treat an answered
// Unknown with an empty message as a provider answer.
func TestProviderAnswered_GRPCContentTypeWithoutStatus(t *testing.T) {
	addr := h2Server(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	cli, p := probeClient(t, addr)
	err := callDescribe(cli, 5*time.Second)
	assert.Equal(t, codes.Unknown, status.Code(err))
	assert.Empty(t, status.Convert(err).Message())
	assert.True(t, p.answered.Load())
}

// TestProviderAnswered_HTTP1Endpoint: an HTTP/1.1-only endpoint never
// completes the HTTP/2 handshake: no answer.
func TestProviderAnswered_HTTP1Endpoint(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })

	cli, p := probeClient(t, lis.Addr().String())
	err = callDescribe(cli, 300*time.Millisecond)
	require.Error(t, err)
	assert.False(t, p.answered.Load(), "an HTTP/1.1 endpoint is no answer")
}

// TestProviderAnswered_BreakerFastFail: (7) the breaker's refusal never runs
// the invoker: the RPC did not begin and nothing answered it.
func TestProviderAnswered_BreakerFastFail(t *testing.T) {
	addr, _ := serveLoopback(t, &describeServer{handler: func(context.Context) (*providerv1.DescribeResponse, error) {
		return nil, status.Error(codes.Unavailable, "down")
	}})
	cb := resilience.NewCircuitBreaker("rpc", "answered-ff", "answered-ff", &resilience.Config{
		FailureThreshold: 1, ResetTimeout: time.Hour, HalfOpenMaxCalls: 1,
	})
	p := &answeredProbe{}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(requestSentInterceptor(), p.interceptor(), providerCircuitBreakerInterceptor(cb)),
		grpc.WithStatsHandler(requestSentStatsHandler{}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	cli := providerv1.NewProviderClient(conn)

	require.Error(t, callDescribe(cli, 5*time.Second))
	require.Equal(t, resilience.StateOpen, cb.GetState())
	require.True(t, p.answered.Load(), "the counted call was an answered Unavailable")

	err = callDescribe(cli, 5*time.Second)
	var ff *breakerFastFailError
	require.ErrorAs(t, err, &ff)
	assert.False(t, p.answered.Load(), "a breaker fast-fail is no answer")
	assert.False(t, p.begun.Load(), "a breaker fast-fail never begins the RPC")
}
