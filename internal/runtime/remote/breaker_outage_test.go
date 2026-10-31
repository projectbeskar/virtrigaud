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

package remote

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
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// outageProvider is a loopback provider that can be stopped and started
// again on the same address, counting every connection it accepted.
type outageProvider struct {
	t        *testing.T
	addr     string
	srv      *isoServer
	gsrv     *grpc.Server
	accepted int64 // connections accepted by earlier (stopped) listeners
	lis      *connCountingListener
}

func newOutageProvider(t *testing.T) *outageProvider {
	t.Helper()
	p := &outageProvider{t: t, srv: &isoServer{failing: map[string]isoShape{}}}
	p.start("127.0.0.1:0")
	p.addr = p.lis.Addr().String()
	t.Cleanup(p.stop)
	return p
}

func (p *outageProvider) start(addr string) {
	p.t.Helper()
	raw, err := net.Listen("tcp", addr)
	require.NoError(p.t, err)
	p.lis = &connCountingListener{Listener: raw}
	p.gsrv = grpc.NewServer()
	providerv1.RegisterProviderServer(p.gsrv, p.srv)
	go func() { _ = p.gsrv.Serve(p.lis) }()
}

func (p *outageProvider) stop() {
	if p.gsrv == nil {
		return
	}
	p.gsrv.Stop()
	p.accepted += p.lis.accepted.Load()
	p.gsrv = nil
}

func (p *outageProvider) dials() int64 {
	if p.gsrv == nil {
		return p.accepted
	}
	return p.accepted + p.lis.accepted.Load()
}

// TestBreakerIsolation_GenuineOutageThroughResolver drives a genuine
// Provider outage (the provider's gRPC server is gone) through the real
// Resolver, Client and default breaker: the breaker opens, GetProvider then
// fails fast without connecting even after the provider is back, the
// half-open trial recovers it on one new connection, and the healthy VM is
// managed again on that connection without further Validate calls.
func TestBreakerIsolation_GenuineOutageThroughResolver(t *testing.T) {
	prov := newOutageProvider(t)

	now := time.Unix(1_700_000_000, 0)
	cfg := resilience.DefaultConfig()
	cfg.Now = func() time.Time { return now }
	reg := resilience.NewRegistry(cfg)
	r := newValidationResolver(t, reg)
	defer r.CleanupAllClients()
	provider := runningProvider(prov.addr)
	vm := contracts.VMRef{ID: "vm-healthy"}

	breaker := func() resilience.State {
		cb, ok := reg.Get(circuitBreakerName, string(provider.Spec.Type), provider.Name)
		require.True(t, ok)
		return cb.GetState()
	}
	// reconcile resolves the Provider and describes the healthy VM, each call
	// bounded like a short controller deadline.
	reconcile := func(timeout time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		p, err := r.GetProvider(ctx, provider)
		if err != nil {
			return err
		}
		_, err = p.Describe(ctx, vm)
		return err
	}

	require.NoError(t, reconcile(5*time.Second))
	require.EqualValues(t, 1, prov.dials())
	require.EqualValues(t, 1, prov.srv.validates.Load())

	// Outage: the provider's server is gone. Unanswered calls count until the
	// breaker opens.
	prov.stop()
	for i := 0; i < cfg.FailureThreshold && breaker() != resilience.StateOpen; i++ {
		now = now.Add(5 * time.Second)
		require.Error(t, reconcile(200*time.Millisecond))
	}
	require.Equal(t, resilience.StateOpen, breaker(), "a genuine outage opens the breaker")

	// Open: every resolution fails fast and connects nowhere, even once the
	// provider is back (before the reset timeout).
	prov.start(prov.addr)
	dialsBefore := prov.dials()
	for i := 0; i < 10; i++ {
		now = now.Add(5 * time.Second)
		start := time.Now()
		err := reconcile(5 * time.Second)
		require.Error(t, err)
		assert.Less(t, time.Since(start), time.Second, "open: fails fast")
	}
	assert.Equal(t, resilience.StateOpen, breaker())
	assert.Equal(t, dialsBefore, prov.dials(), "open: no connection attempts")
	assert.EqualValues(t, 1, prov.srv.validates.Load(), "open: no Validate reached the provider")

	// Reset timeout elapsed: the half-open trial (Validate + Describes)
	// closes the breaker on one new connection.
	now = now.Add(cfg.ResetTimeout)
	for i := 0; i < cfg.HalfOpenMaxCalls; i++ {
		require.NoError(t, reconcile(5*time.Second), "half-open trial %d", i+1)
	}
	require.Equal(t, resilience.StateClosed, breaker(), "recovered: the breaker closes")
	assert.Equal(t, dialsBefore+1, prov.dials(), "recovery uses one new connection")

	// Healthy VM management is restored on that connection, Validate-free.
	validates := prov.srv.validates.Load()
	for i := 0; i < 20; i++ {
		now = now.Add(2 * time.Minute)
		require.NoError(t, reconcile(5*time.Second))
	}
	assert.Equal(t, resilience.StateClosed, breaker())
	assert.Equal(t, dialsBefore+1, prov.dials(), "the recovered client is reused")
	assert.Equal(t, validates, prov.srv.validates.Load(), "no Validate per reconcile after recovery")
	// 1 before the outage, one per half-open trial, 20 after recovery.
	assert.EqualValues(t, 1+cfg.HalfOpenMaxCalls+20, prov.srv.reached(vm.ID), "every healthy Describe after recovery reached the provider")
}
