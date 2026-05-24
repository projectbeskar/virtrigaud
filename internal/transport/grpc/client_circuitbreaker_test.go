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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/resilience"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestIsInfraFailure exhaustively pins the gRPC-code classification used
// by the CircuitBreaker interceptor (G6 / #111). Any future change to
// this table needs a deliberate review — the classification policy is
// load-bearing for what counts as "provider health" vs "business error."
func TestIsInfraFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		// Counted as infra failure (would trip the breaker).
		{"unavailable", status.Error(codes.Unavailable, "down"), true},
		{"deadline exceeded", status.Error(codes.DeadlineExceeded, "timeout"), true},
		{"internal", status.Error(codes.Internal, "panic in provider"), true},
		{"unknown", status.Error(codes.Unknown, "unspecified"), true},
		{"non-grpc error treated as Unknown -> infra", errors.New("raw transport err"), true},

		// Not counted — business / application / out-of-scope.
		{"nil is not infra", nil, false},
		{"ok is not infra", status.Error(codes.OK, ""), false},
		{"not found is business", status.Error(codes.NotFound, "vm missing"), false},
		{"invalid argument is business", status.Error(codes.InvalidArgument, "bad spec"), false},
		{"already exists is business", status.Error(codes.AlreadyExists, "dup"), false},
		{"failed precondition is business", status.Error(codes.FailedPrecondition, "wrong state"), false},
		{"permission denied is auth", status.Error(codes.PermissionDenied, "no"), false},
		{"unauthenticated is auth", status.Error(codes.Unauthenticated, "who?"), false},
		{"canceled is caller", status.Error(codes.Canceled, "ctx done"), false},
		{"resource exhausted is rate-limit", status.Error(codes.ResourceExhausted, "slow down"), false},
		{"aborted is protocol", status.Error(codes.Aborted, "txn conflict"), false},
		{"out of range is protocol", status.Error(codes.OutOfRange, "bounds"), false},
		{"unimplemented is protocol", status.Error(codes.Unimplemented, "no rpc"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isInfraFailure(tt.err))
		})
	}
}

// newTestClientWithCB is a sibling of newTestClient (in
// client_metrics_test.go) but also chains the CircuitBreaker interceptor
// AFTER the metrics interceptor, matching the production order set by
// NewClient. Returns the client and the breaker so tests can assert on
// the breaker's state directly.
func newTestClientWithCB(t *testing.T, dialer func(context.Context, string) (net.Conn, error), providerType, providerName string, cfg *resilience.Config) (*Client, *resilience.CircuitBreaker) {
	t.Helper()
	cb := resilience.NewCircuitBreaker("rpc", providerType, providerName, cfg)
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(
			providerRPCMetricsInterceptor(providerType),
			providerCircuitBreakerInterceptor(cb),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return &Client{conn: conn, client: providerv1.NewProviderClient(conn)}, cb
}

// TestProviderCircuitBreakerInterceptor_InfraErrorsTripBreaker drives
// the breaker through enough Unavailable RPCs to trip it, then asserts
// the next call short-circuits with codes.Unavailable WITHOUT the
// invoker running on the server. The test deliberately uses a
// FailureThreshold of 2 to keep the loop short.
func TestProviderCircuitBreakerInterceptor_InfraErrorsTripBreaker(t *testing.T) {
	serverCalls := 0
	dialer, cleanup := startBufconnServer(t, &fakeProviderServer{
		ValidateFn: func(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
			serverCalls++
			return nil, status.Error(codes.Unavailable, "provider is down")
		},
	})
	defer cleanup()

	cli, cb := newTestClientWithCB(t, dialer, "g6-trip", "g6-trip-provider", &resilience.Config{
		FailureThreshold: 2,
		ResetTimeout:     30 * time.Second, // long — we don't want HalfOpen during this test
		HalfOpenMaxCalls: 1,
	})

	// First two failures count toward the threshold; breaker still closed
	// or just-tripped, but the invoker DOES run.
	require.Error(t, cli.Validate(context.Background()))
	require.Error(t, cli.Validate(context.Background()))
	require.Equal(t, 2, serverCalls, "first two calls should reach the server")
	require.Equal(t, resilience.StateOpen, cb.GetState(), "breaker should be Open after threshold")

	// Third call must be rejected by the breaker BEFORE the invoker runs.
	err := cli.Validate(context.Background())
	require.Error(t, err)
	require.Equal(t, 2, serverCalls, "third call must NOT reach the server (breaker open)")

	// Confirm the error code surfaced to the caller is Unavailable — so
	// downstream retry/mapping logic treats it like any other infra
	// failure rather than a business error.
	//
	// NOTE: c.Validate() wraps the gRPC error via mapGRPCError into a
	// contracts.RetryableError. We inspect via the underlying gRPC
	// status code by re-running through the raw client.
	rawErr := func() error {
		_, e := cli.client.Validate(context.Background(), &providerv1.ValidateRequest{})
		return e
	}()
	require.Error(t, rawErr)
	assert.Equal(t, codes.Unavailable, status.Code(rawErr),
		"breaker-open rejection must surface as codes.Unavailable for uniform downstream handling")
}

// TestProviderCircuitBreakerInterceptor_BusinessErrorsDoNotTrip pins the
// classification policy: a stream of NotFound / InvalidArgument
// responses must NOT trip the breaker, no matter how many. The provider
// is healthy; the requests are just bad.
func TestProviderCircuitBreakerInterceptor_BusinessErrorsDoNotTrip(t *testing.T) {
	dialer, cleanup := startBufconnServer(t, &fakeProviderServer{
		ValidateFn: func(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
			return nil, status.Error(codes.NotFound, "vm missing")
		},
	})
	defer cleanup()

	cli, cb := newTestClientWithCB(t, dialer, "g6-business", "g6-business-provider", &resilience.Config{
		FailureThreshold: 2,
		ResetTimeout:     30 * time.Second,
		HalfOpenMaxCalls: 1,
	})

	// Fire 5 NotFound responses — well past the FailureThreshold of 2.
	for i := 0; i < 5; i++ {
		require.Error(t, cli.Validate(context.Background()))
	}
	assert.Equal(t, resilience.StateClosed, cb.GetState(),
		"breaker must stay Closed for business errors (NotFound), regardless of count")
	assert.Equal(t, 0, cb.GetFailures(),
		"breaker failure counter must not increment on business errors")
}

// TestProviderCircuitBreakerInterceptor_SuccessKeepsBreakerClosed
// confirms the happy path: successful RPCs leave the breaker Closed
// with zero failures. Catches regressions where, e.g., a refactor
// accidentally records every call as a failure.
func TestProviderCircuitBreakerInterceptor_SuccessKeepsBreakerClosed(t *testing.T) {
	dialer, cleanup := startBufconnServer(t, &fakeProviderServer{
		ValidateFn: func(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
			return &providerv1.ValidateResponse{Ok: true}, nil
		},
	})
	defer cleanup()

	cli, cb := newTestClientWithCB(t, dialer, "g6-success", "g6-success-provider", &resilience.Config{
		FailureThreshold: 2,
		ResetTimeout:     30 * time.Second,
		HalfOpenMaxCalls: 1,
	})

	for i := 0; i < 5; i++ {
		require.NoError(t, cli.Validate(context.Background()))
	}
	assert.Equal(t, resilience.StateClosed, cb.GetState(), "successful calls must leave the breaker Closed")
	assert.Equal(t, 0, cb.GetFailures(), "successful calls must not increment failure counter")
}

// TestProviderCircuitBreakerInterceptor_RecoversToClosed exercises the
// full Closed -> Open -> HalfOpen -> Closed lifecycle through the
// interceptor (not just the raw breaker). Verifies the wiring across
// the cb.Call boundary: after ResetTimeout elapses, the next successful
// call transitions to HalfOpen, and after HalfOpenMaxCalls successes
// the breaker closes again.
func TestProviderCircuitBreakerInterceptor_RecoversToClosed(t *testing.T) {
	// Server toggles: first 2 calls fail, then succeed forever.
	failuresLeft := 2
	dialer, cleanup := startBufconnServer(t, &fakeProviderServer{
		ValidateFn: func(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
			if failuresLeft > 0 {
				failuresLeft--
				return nil, status.Error(codes.Unavailable, "down")
			}
			return &providerv1.ValidateResponse{Ok: true}, nil
		},
	})
	defer cleanup()

	cli, cb := newTestClientWithCB(t, dialer, "g6-recover", "g6-recover-provider", &resilience.Config{
		FailureThreshold: 2,
		ResetTimeout:     30 * time.Millisecond,
		HalfOpenMaxCalls: 2,
	})

	// Trip the breaker.
	require.Error(t, cli.Validate(context.Background()))
	require.Error(t, cli.Validate(context.Background()))
	require.Equal(t, resilience.StateOpen, cb.GetState())

	// Wait past ResetTimeout — next call transitions Open -> HalfOpen
	// (and per the #96 fix, counts as the first half-open call).
	time.Sleep(60 * time.Millisecond)

	// Two successful half-open calls -> breaker closes.
	require.NoError(t, cli.Validate(context.Background()))
	require.NoError(t, cli.Validate(context.Background()))
	assert.Equal(t, resilience.StateClosed, cb.GetState(),
		"breaker should return to Closed after HalfOpenMaxCalls successful half-open calls")
}
