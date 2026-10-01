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
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	transportgrpc "github.com/projectbeskar/virtrigaud/internal/transport/grpc"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/middleware"
)

// ADR-0007 Slice 5 lab, B3: a routed clone that failed on its host reached the
// manager as `rpc error: code = Unknown desc = failed to clone VM on host
// "dome"`. codes.Unknown alone would count toward the manager's per-Provider
// circuit breaker — one tenant's failing clone, recreated, could then open it
// for every VM of the Provider. These tests drive the REAL libvirt Server
// (clustered, on the routed fake hosts) through a real gRPC hop — a loopback
// TCP listener with the provider's SDK interceptors (recovery, logging), and
// the manager's transport client with its circuit-breaker interceptor — and
// pin that such a failure carries VM_OPERATION_FAILED on the wire and never
// counts, while a failure the breaker must count (the provider unreachable)
// still does.

// serveProviderOverTCP serves s on a loopback TCP port behind the SDK
// interceptors the libvirt provider binary installs, and returns its address.
func serveProviderOverTCP(t *testing.T, s providerv1.ProviderServer) (string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	unary, _ := middleware.Build(&middleware.Config{
		Recovery: &middleware.RecoveryConfig{Enabled: true},
		Logging:  &middleware.LoggingConfig{Enabled: true},
	})
	gsrv := grpc.NewServer(grpc.ChainUnaryInterceptor(unary...))
	providerv1.RegisterProviderServer(gsrv, s)
	go func() { _ = gsrv.Serve(lis) }()
	return lis.Addr().String(), gsrv.Stop
}

// managerClient is the manager's transport client to addr, with a circuit
// breaker that opens after two counted failures.
func managerClient(t *testing.T, addr string) (*transportgrpc.Client, *resilience.CircuitBreaker) {
	t.Helper()
	cb := resilience.NewCircuitBreaker("b3", "libvirt", "b3-provider", &resilience.Config{
		FailureThreshold: 2,
		ResetTimeout:     time.Minute,
		HalfOpenMaxCalls: 1,
	})
	cli, err := transportgrpc.NewClient(context.Background(), addr, "libvirt", "b3-provider", cb, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	return cli, cb
}

// managerCloneReq is routedCloneReq as the VMClone controller sends it.
func managerCloneReq() contracts.CloneRequest {
	return contracts.CloneRequest{
		Source:       contracts.VMRef{ID: "web", HostID: "host-b", Owner: ownerTeamA},
		TargetHostID: "host-b",
		TargetName:   "copy",
		TargetVM:     cloneTarget,
	}
}

func TestClustered_CloneFailureOverGRPC_NeverCountsTowardTheBreaker(t *testing.T) {
	fx := sourceWeb(t, nil)
	failQemuImgConvert(t, fx)
	addr, stop := serveProviderOverTCP(t, NewServer(fx.p))
	defer stop()
	cli, cb := managerClient(t, addr)

	for i := 0; i < 4; i++ {
		_, err := cli.Clone(context.Background(), managerCloneReq())
		require.Error(t, err)
		assert.Contains(t, err.Error(), `failed to clone VM on host "host-b"`)
		assert.False(t, contracts.IsRetryable(err), "a clone that failed on its host is a failure, not a retry loop")
		assert.False(t, contracts.IsVMSourceRunning(err))
		assert.NotContains(t, err.Error(), "/var/lib", "no host path reaches the manager")
	}
	assert.Equal(t, resilience.StateClosed, cb.GetState(), "a per-VM clone failure (VM_OPERATION_FAILED) never opens the breaker")
	assert.Zero(t, cb.GetFailures())

	// The harness counts what it must: with the provider gone, calls fail
	// with an infrastructure code and the breaker opens.
	stop()
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		_, err := cli.Clone(ctx, managerCloneReq())
		cancel()
		require.Error(t, err)
	}
	assert.Equal(t, resilience.StateOpen, cb.GetState(), "an unreachable provider still opens the breaker")
}

func TestClustered_CloneOfRunningSourceOverGRPC_IsMarkedAndNeverCounts(t *testing.T) {
	fx := sourceWeb(t, nil)
	fx.script("host-b", "state", "running\n")
	addr, stop := serveProviderOverTCP(t, NewServer(fx.p))
	defer stop()
	cli, cb := managerClient(t, addr)

	for i := 0; i < 4; i++ {
		_, err := cli.Clone(context.Background(), managerCloneReq())
		require.Error(t, err)
		assert.True(t, contracts.IsVMSourceRunning(err), "the VMClone controller keeps the clone Pending: %v", err)
		assert.Contains(t, err.Error(), "power off the source VM to clone it")
	}
	assert.Equal(t, resilience.StateClosed, cb.GetState())
	assert.Zero(t, cb.GetFailures())
	assertNoCopy(t, fx.calls())
}
