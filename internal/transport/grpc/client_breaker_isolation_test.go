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
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	sdkerrors "github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Fault isolation of the per-Provider circuit breaker (#472): a failure the
// provider answered for one VM does not count; a failure without an answer
// (transport, overload) still does. See countsTowardBreaker.

// fakeClock is a settable clock for resilience.Config.Now.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestCountsTowardBreaker_FaultDomains pins the classification table: code x
// answered x reason.
func TestCountsTowardBreaker_FaultDomains(t *testing.T) {
	describe := providerv1.Provider_Describe_FullMethodName
	reason := func(code codes.Code, r string) error {
		return withReason(t, code, r, contracts.ErrorInfoDomain)
	}
	cases := []struct {
		name     string
		err      error
		answered bool
		want     bool
	}{
		// 1. transport failure: no answer.
		{"unanswered Unavailable (connection refused / closing)", status.Error(codes.Unavailable, "connection refused"), false, true},
		{"unanswered Unknown (proxy HTTP 500)", status.Error(codes.Unknown, "unexpected HTTP status code received from server: 500"), false, true},
		{"unanswered Internal (protocol error / RST INTERNAL)", status.Error(codes.Internal, "stream terminated by RST_STREAM"), false, true},
		// 2. provider-wide unavailability, answered.
		{"answered Unavailable", status.Error(codes.Unavailable, "service unavailable: PVE client not configured"), true, true},
		// 3. overload / hang.
		{"unanswered DeadlineExceeded (client deadline)", status.Error(codes.DeadlineExceeded, "context deadline exceeded"), false, true},
		{"answered DeadlineExceeded", status.Error(codes.DeadlineExceeded, "operation timed out"), true, true},
		{"answered ResourceExhausted", status.Error(codes.ResourceExhausted, "rate limit exceeded"), true, false},
		// 4. single host (clustered).
		{"HOST_UNAVAILABLE", reason(codes.Unavailable, contracts.HostUnavailableReason), true, false},
		{"HOST_UNAVAILABLE unanswered flag", reason(codes.Unavailable, contracts.HostUnavailableReason), false, false},
		{"OUTCOME_UNKNOWN_RETRY", reason(codes.Unavailable, contracts.OutcomeUnknownRetryReason), true, false},
		// 5. single VM backend failure, answered.
		{"answered Unknown (plain handler error)", status.Error(codes.Unknown, "domain web: cannot acquire state change lock"), true, false},
		{"answered Internal (sdk NewInternal)", status.Convert(sdkerrors.NewInternal("failed to describe VM", errors.New("api 500"))).Err(), true, false},
		{"answered Unknown without message (no grpc-status)", status.Error(codes.Unknown, ""), true, true},
		{"VM_OPERATION_FAILED Unavailable", reason(codes.Unavailable, contracts.VMOperationFailedReason), false, false},
		{"VM_OPERATION_FAILED Unknown", reason(codes.Unknown, contracts.VMOperationFailedReason), false, false},
		{"VM_DISK_CHECK_FAILED", reason(codes.Unavailable, contracts.VMDiskCheckFailedReason), false, false},
		// 6. single VM operation refusal.
		{"answered NotFound", status.Error(codes.NotFound, "vm missing"), true, false},
		{"answered FailedPrecondition", status.Error(codes.FailedPrecondition, "power off first"), true, false},
		{"Proxmox exists-elsewhere (rc2: VM_OPERATION_FAILED)", reason(codes.Unavailable, contracts.VMOperationFailedReason), true, false},
		{"Proxmox exists-elsewhere (older image: plain Unavailable)", status.Error(codes.Unavailable, "exists elsewhere in the cluster"), true, true},
		// 7. ambiguous outcome: no answer, counted as transport failure.
		{"unanswered Unavailable after the request was sent", status.Error(codes.Unavailable, "error reading from server: EOF"), false, true},
		{"nil", nil, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, countsTowardBreaker(describe, tc.err, tc.answered))
		})
	}

	// ImagePrepare's method-specific exclusions are unchanged either way.
	prepare := providerv1.Provider_ImagePrepare_FullMethodName
	for _, answered := range []bool{false, true} {
		assert.False(t, countsTowardBreaker(prepare, status.Error(codes.DeadlineExceeded, "x"), answered))
		assert.False(t, countsTowardBreaker(prepare, reason(codes.Unavailable, contracts.ImageArtifactInProgressReason), answered))
		assert.True(t, countsTowardBreaker(prepare, status.Error(codes.Unavailable, "provider down"), answered))
	}
}

// TestCircuitBreakerInterceptor_ReturnsInvokerErrorUnchanged (scenario E,
// accounting half): whatever the classification, answered or not, the
// interceptor returns the invoker's error value itself. Only the breaker's
// record changes.
func TestCircuitBreakerInterceptor_ReturnsInvokerErrorUnchanged(t *testing.T) {
	errs := []error{
		status.Error(codes.Unavailable, "transport is closing"),
		status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
		status.Error(codes.Internal, "boom"),
		status.Error(codes.Unknown, "plain"),
		status.Error(codes.NotFound, "gone"),
		withReason(t, codes.Unavailable, contracts.OutcomeUnknownRetryReason, contracts.ErrorInfoDomain),
		withReason(t, codes.Unavailable, contracts.VMOperationFailedReason, contracts.ErrorInfoDomain),
		errors.New("not a status"),
	}
	for _, answered := range []bool{false, true} {
		for _, want := range errs {
			cb := resilience.NewCircuitBreaker("rpc", "unchanged", "unchanged", &resilience.Config{
				FailureThreshold: 100, ResetTimeout: time.Hour, HalfOpenMaxCalls: 1,
			})
			ic := providerCircuitBreakerInterceptor(cb)
			ctx, rs := withRequestSent(context.Background())
			got := ic(ctx, providerv1.Provider_Create_FullMethodName, nil, nil, nil,
				func(ctx context.Context, _ string, _, _ interface{}, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
					got, ok := requestSentFrom(ctx)
					require.True(t, ok)
					require.Same(t, rs, got, "the interceptor uses the RPC's own tracking")
					rs.answered.Store(answered)
					return want
				})
			assert.Same(t, want, got, "answered=%v %v: the error is returned as is", answered, want)
		}
	}
}

// restartableServer serves srv on a fixed loopback address and can be
// stopped and started again (a provider pod restart).
type restartableServer struct {
	t    *testing.T
	addr string
	srv  providerv1.ProviderServer
	gsrv *grpc.Server
}

func newRestartableServer(t *testing.T, srv providerv1.ProviderServer) *restartableServer {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	r := &restartableServer{t: t, addr: lis.Addr().String(), srv: srv}
	r.serve(lis)
	t.Cleanup(func() { r.gsrv.Stop() })
	return r
}

func (r *restartableServer) serve(lis net.Listener) {
	r.gsrv = grpc.NewServer()
	providerv1.RegisterProviderServer(r.gsrv, r.srv)
	go func(g *grpc.Server) { _ = g.Serve(lis) }(r.gsrv)
}

func (r *restartableServer) stop() { r.gsrv.Stop() }

func (r *restartableServer) start() {
	lis, err := net.Listen("tcp", r.addr)
	require.NoError(r.t, err)
	r.serve(lis)
}

// prodClientWithCB builds a Client for addr with the production call options
// and a breaker on clk. The reconnect backoff is shortened so a restarted
// server is reached quickly (dial parameter only; the interceptors and the
// stats handler are the production ones).
func prodClientWithCB(t *testing.T, addr string, cfg *resilience.Config, clk *fakeClock) (*Client, *resilience.CircuitBreaker) {
	t.Helper()
	cfg.Now = clk.Now
	cb := resilience.NewCircuitBreaker("rpc", "isolation", t.Name(), cfg)
	opts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 10 * time.Millisecond, Multiplier: 1, MaxDelay: 20 * time.Millisecond},
			MinConnectTimeout: 100 * time.Millisecond,
		}),
	}, providerCallDialOptions("isolation", cb)...)
	conn, err := grpc.NewClient(addr, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return &Client{conn: conn, client: providerv1.NewProviderClient(conn), vmOps: metrics.NewVMOperationMetrics("isolation", t.Name())}, cb
}

// describeWithin sends a Describe with timeout.
func describeWithin(cli *Client, id string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := cli.Describe(ctx, contracts.VMRef{ID: id})
	return err
}

// modeServer answers Describe by mode: ok, answered Internal / Unknown, or
// hang until released (overload).
type modeServer struct {
	providerv1.UnimplementedProviderServer
	hang    atomic.Bool
	release chan struct{}
	calls   atomic.Int64
}

func (s *modeServer) Describe(ctx context.Context, r *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	s.calls.Add(1)
	if s.hang.Load() {
		select {
		case <-s.release:
		case <-ctx.Done():
		}
		return nil, status.Error(codes.Unavailable, "too late")
	}
	switch {
	case len(r.GetId()) > 4 && r.GetId()[:4] == "int-":
		return nil, sdkerrors.NewInternal("failed to describe VM", errors.New("api 500"))
	case len(r.GetId()) > 4 && r.GetId()[:4] == "unk-":
		return nil, fmt.Errorf("domain %s: cannot acquire state change lock", r.GetId())
	}
	return &providerv1.DescribeResponse{Exists: true}, nil
}

// TestBreakerIsolation_TransportFailureOpensAndRecovers is scenario B: the
// provider's server stops (connection refused); the breaker opens after the
// threshold, fails fast while open (request not sent), and once the server is
// back the half-open trial calls close it again.
func TestBreakerIsolation_TransportFailureOpensAndRecovers(t *testing.T) {
	srv := &modeServer{release: make(chan struct{})}
	rs := newRestartableServer(t, srv)
	clk := newFakeClock()
	cli, cb := prodClientWithCB(t, rs.addr, &resilience.Config{FailureThreshold: 3, ResetTimeout: time.Minute, HalfOpenMaxCalls: 3}, clk)

	require.NoError(t, describeWithin(cli, "web", 5*time.Second))
	rs.stop()
	for i := 0; i < 3; i++ {
		err := describeWithin(cli, "web", 150*time.Millisecond)
		require.Error(t, err)
	}
	require.Equal(t, resilience.StateOpen, cb.GetState(), "connection refused opens the breaker")

	start := time.Now()
	err := describeWithin(cli, "web", 5*time.Second)
	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second, "open: fails fast")
	assert.True(t, contracts.IsRequestNotSent(err), "a fast-fail is reported as not sent")

	rs.start()
	clk.Advance(time.Minute + time.Second)
	for i := 0; i < 3; i++ {
		require.NoError(t, describeWithin(cli, "web", 5*time.Second), "half-open trial %d", i+1)
	}
	assert.Equal(t, resilience.StateClosed, cb.GetState(), "recovered: the breaker closes")
	require.NoError(t, describeWithin(cli, "web", 5*time.Second))
	assert.Equal(t, resilience.StateClosed, cb.GetState(), "and stays closed")
}

// TestBreakerIsolation_OverloadStillOpens is scenario C: a provider too slow
// to answer before the client's deadline opens the breaker (no answer).
// Answered ResourceExhausted does not (existing design: back off the call).
func TestBreakerIsolation_OverloadStillOpens(t *testing.T) {
	srv := &modeServer{release: make(chan struct{})}
	defer close(srv.release)
	rs := newRestartableServer(t, srv)
	cli, cb := prodClientWithCB(t, rs.addr, &resilience.Config{FailureThreshold: 3, ResetTimeout: time.Minute, HalfOpenMaxCalls: 1}, newFakeClock())

	srv.hang.Store(true)
	for i := 0; i < 3; i++ {
		require.Error(t, describeWithin(cli, "web", 100*time.Millisecond))
	}
	assert.Equal(t, resilience.StateOpen, cb.GetState(), "an overloaded provider (no answer in time) opens the breaker")

	exhausted := &describeErrServer{err: sdkerrors.NewRateLimit(time.Second)}
	addr, _ := serveLoopback(t, exhausted)
	cli2, cb2 := prodClientWithCB(t, addr, &resilience.Config{FailureThreshold: 2, ResetTimeout: time.Minute, HalfOpenMaxCalls: 1}, newFakeClock())
	for i := 0; i < 5; i++ {
		require.Error(t, describeWithin(cli2, "web", 5*time.Second))
	}
	assert.Equal(t, resilience.StateClosed, cb2.GetState())
	assert.EqualValues(t, 5, exhausted.calls.Load())
}

// hostSplitServer answers Describe for host-a VMs with HOST_UNAVAILABLE and
// succeeds for every other host.
type hostSplitServer struct {
	providerv1.UnimplementedProviderServer
	down       error
	healthyHit atomic.Int64
}

func (s *hostSplitServer) Describe(_ context.Context, r *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	if r.GetTargetHostId() == "host-a" {
		return nil, s.down
	}
	s.healthyHit.Add(1)
	return &providerv1.DescribeResponse{Exists: true}, nil
}

// TestBreakerIsolation_ClusteredHostUnavailableExcluded is scenario D: a down
// host's VMs keep failing with HOST_UNAVAILABLE; the breaker stays closed and
// every call for a healthy host's VM proceeds.
func TestBreakerIsolation_ClusteredHostUnavailableExcluded(t *testing.T) {
	srv := &hostSplitServer{down: hostUnavailableErr(t, contracts.HostUnavailableReason, contracts.HostUnavailableErrorDomain)}
	addr, _ := serveLoopback(t, srv)
	cli, cb := prodClientWithCB(t, addr, &resilience.Config{FailureThreshold: 2, ResetTimeout: time.Minute, HalfOpenMaxCalls: 1}, newFakeClock())
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		_, err := cli.Describe(ctx, contracts.VMRef{ID: "web-a", HostID: "host-a"})
		require.True(t, contracts.IsHostUnavailable(err))
		_, err = cli.Describe(ctx, contracts.VMRef{ID: "web-a2", HostID: "host-a"})
		require.True(t, contracts.IsHostUnavailable(err))
		_, err = cli.Describe(ctx, contracts.VMRef{ID: "web-b", HostID: "host-b"})
		require.NoError(t, err)
	}
	assert.Equal(t, resilience.StateClosed, cb.GetState())
	assert.EqualValues(t, 20, srv.healthyHit.Load())
}

// createServer answers Create with handler.
type createServer struct {
	providerv1.UnimplementedProviderServer
	handler func(ctx context.Context) (*providerv1.CreateResponse, error)
}

func (s *createServer) Create(ctx context.Context, _ *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	return s.handler(ctx)
}

// TestBreakerIsolation_AmbiguousOutcomeUnchanged is scenario E end to end: a
// Create whose request was sent and whose connection then dropped is still an
// unknown outcome (not "not sent") and still counts; an answered Internal
// Create maps to the same error with and without the breaker; a fast-fail is
// still "not sent".
func TestBreakerIsolation_AmbiguousOutcomeUnchanged(t *testing.T) {
	inHandler := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	srv := &createServer{handler: func(context.Context) (*providerv1.CreateResponse, error) {
		inHandler <- struct{}{}
		<-release
		return &providerv1.CreateResponse{Id: "x"}, nil
	}}
	rs := newRestartableServer(t, srv)
	cli, cb := prodClientWithCB(t, rs.addr, &resilience.Config{FailureThreshold: 1, ResetTimeout: time.Hour, HalfOpenMaxCalls: 1}, newFakeClock())

	errCh := make(chan error, 1)
	go func() {
		_, err := cli.Create(context.Background(), contracts.CreateRequest{Name: "web"})
		errCh <- err
	}()
	<-inHandler
	go rs.stop()
	err := <-errCh
	require.Error(t, err)
	assert.False(t, contracts.IsRequestNotSent(err), "a dropped connection after the request was sent is an unknown outcome")
	assert.False(t, contracts.IsNothingDone(err))
	assert.Equal(t, resilience.StateOpen, cb.GetState(), "a transport failure still counts")

	_, err = cli.Create(context.Background(), contracts.CreateRequest{Name: "web"})
	require.Error(t, err)
	assert.True(t, contracts.IsRequestNotSent(err), "the breaker's fast-fail is still reported as not sent")

	// An answered Internal maps identically with and without a breaker.
	answeredSrv := &createServer{handler: func(context.Context) (*providerv1.CreateResponse, error) {
		return nil, sdkerrors.NewInternal("failed to create VM", errors.New("api 500"))
	}}
	addr, _ := serveLoopback(t, answeredSrv)
	withCB, cb2 := prodClientWithCB(t, addr, &resilience.Config{FailureThreshold: 1, ResetTimeout: time.Hour, HalfOpenMaxCalls: 1}, newFakeClock())
	conn, cerr := grpc.NewClient(addr, append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
		providerCallDialOptions("isolation", nil)...)...)
	require.NoError(t, cerr)
	t.Cleanup(func() { _ = conn.Close() })
	noCB := &Client{conn: conn, client: providerv1.NewProviderClient(conn), vmOps: metrics.NewVMOperationMetrics("isolation", "nocb")}
	_, e1 := withCB.Create(context.Background(), contracts.CreateRequest{Name: "web"})
	_, e2 := noCB.Create(context.Background(), contracts.CreateRequest{Name: "web"})
	require.Error(t, e1)
	assert.Equal(t, e2.Error(), e1.Error())
	assert.Equal(t, contracts.IsRequestNotSent(e2), contracts.IsRequestNotSent(e1))
	assert.Equal(t, contracts.IsNothingDone(e2), contracts.IsNothingDone(e1))
	assert.Equal(t, contracts.IsRetryable(e2), contracts.IsRetryable(e1))
	assert.Equal(t, resilience.StateClosed, cb2.GetState(), "an answered Internal does not count")
}

// TestBreakerIsolation_Concurrent is scenario F: many goroutines through the
// production interceptor chain and breaker. Answered VM-scoped failures mixed
// with successes never open it; unanswered failures (overload) do; once the
// provider is healthy again the half-open trial calls close it.
func TestBreakerIsolation_Concurrent(t *testing.T) {
	srv := &modeServer{release: make(chan struct{})}
	rs := newRestartableServer(t, srv)
	clk := newFakeClock()
	cli, cb := prodClientWithCB(t, rs.addr, &resilience.Config{FailureThreshold: 5, ResetTimeout: time.Minute, HalfOpenMaxCalls: 3}, clk)

	run := func(workers, perWorker int, id func(w, i int) string, timeout time.Duration) {
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < perWorker; i++ {
					_ = describeWithin(cli, id(w, i), timeout)
				}
			}(w)
		}
		wg.Wait()
	}

	// Phase 1: answered Internal / Unknown failures and successes.
	run(32, 40, func(w, i int) string {
		switch (w + i) % 3 {
		case 0:
			return "int-vm"
		case 1:
			return "unk-vm"
		}
		return "ok-vm"
	}, 5*time.Second)
	assert.Equal(t, resilience.StateClosed, cb.GetState(), "answered VM-scoped failures never open the breaker")
	assert.EqualValues(t, 32*40, srv.calls.Load(), "every call reached the provider")

	// Phase 2: unanswered failures (the provider hangs past the deadline).
	srv.hang.Store(true)
	run(16, 4, func(int, int) string { return "ok-vm" }, 50*time.Millisecond)
	assert.Equal(t, resilience.StateOpen, cb.GetState(), "unanswered failures open the breaker")

	// Phase 3: the provider is healthy again; after ResetTimeout concurrent
	// callers race for the half-open slots and the trial calls close it.
	srv.hang.Store(false)
	close(srv.release)
	clk.Advance(time.Minute + time.Second)
	run(16, 10, func(int, int) string { return "ok-vm" }, 5*time.Second)
	assert.Equal(t, resilience.StateClosed, cb.GetState(), "half-open trial calls close the breaker")
	require.NoError(t, describeWithin(cli, "ok-vm", 5*time.Second))
}
