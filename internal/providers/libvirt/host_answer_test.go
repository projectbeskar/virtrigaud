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
	stderrors "errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin V3c: a clustered host that stops answering is answered the
// same way whichever wait of the call it stops — HOST_UNAVAILABLE for a read
// (Describe), naming the host in the ErrorInfo — and before the call's
// deadline, instead of codes.Unknown "failed to describe VM on host ..." when
// the call ran out of time waiting for the host.

func TestHostAnswerDeadline(t *testing.T) {
	_, ok := hostAnswerDeadline(context.Background())
	assert.False(t, ok, "a context without a deadline has no wait bound")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	d, ok := hostAnswerDeadline(ctx)
	require.True(t, ok)
	ctxDeadline, _ := ctx.Deadline()
	assert.Equal(t, hostAnswerMargin, ctxDeadline.Sub(d), "a long call ends its waits hostAnswerMargin early")

	short, cancelShort := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelShort()
	d, ok = hostAnswerDeadline(short)
	require.True(t, ok)
	shortDeadline, _ := short.Deadline()
	margin := shortDeadline.Sub(d)
	assert.Greater(t, margin, time.Duration(0))
	assert.LessOrEqual(t, margin, 500*time.Millisecond, "a short call keeps three quarters of its time for the host")
}

func TestAcquireExecSlot_BoundedOnAClusteredHostOnly(t *testing.T) {
	full := func(bound bool) *VirshProvider {
		v := &VirshProvider{execSem: make(chan struct{}, 1), boundHostWaits: bound}
		v.execSem <- struct{}{} // a command that never returns holds the only slot
		return v
	}

	t.Run("clustered: a host not answering, before the deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := full(true).acquireExecSlot(ctx)
		require.Error(t, err)
		assert.True(t, isHostNotAnswering(err), "got %v", err)
		assert.False(t, stderrors.Is(err, context.DeadlineExceeded), "never the call's context error")
		assert.True(t, isHostTransportFailure(err))
		assert.Less(t, time.Since(start), 750*time.Millisecond, "answered before the call's deadline")
		assert.NoError(t, ctx.Err())
	})

	t.Run("single-host: unchanged, the call's own deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := full(false).acquireExecSlot(ctx)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.False(t, isHostNotAnswering(err))
	})

	t.Run("clustered: a call that is cancelled keeps its context error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := full(true).acquireExecSlot(ctx)
		assert.ErrorIs(t, err, context.Canceled)
		assert.False(t, isHostNotAnswering(err))
	})
}

// slotHeldCluster is a clustered provider over host-a and host-b (the ops
// fixture's local virsh shims) whose host-b connection has every command slot
// held, as by commands that hang on a host that stopped answering.
// boundHostWaits says whether host-b's waits are bounded (the production
// clustered dialer sets it).
func slotHeldCluster(t *testing.T, boundHostWaits bool) *Provider {
	t.Helper()
	newOpsFixture(t, map[string]map[string]string{"host-a": {}, "host-b": {"web": routingDomainXML("web", ownerTeamA)}})
	inv := hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion}
	conns := map[string]*virshConn{}
	for _, h := range []string{"host-a", "host-b"} {
		inv.Hosts = append(inv.Hosts, hostsecret.Host{ID: h, Endpoint: "qemu+ssh://virt@" + h + "/system"})
		vp := localHostVP(h)
		if h == "host-b" {
			vp.boundHostWaits = boundHostWaits
			for i := 0; i < cap(vp.execSem); i++ {
				vp.execSem <- struct{}{}
			}
		}
		conns[h] = newClusteredVirshConn(hostconn.HostID(h), vp, nil)
	}
	dial := func(_ context.Context, h hostsecret.Host) (hostconn.Conn, error) { return conns[h.ID], nil }
	p, _ := newClusteredProviderForTest(t, inv, dial)
	p.virshProvider = newUnroutableVirshProvider()
	return p
}

func TestClustered_Describe_HostNotAnsweringIsHostUnavailableBeforeTheDeadline(t *testing.T) {
	owner := ownerFromIdentity(ownerTeamA)
	req := &providerv1.DescribeRequest{Id: "web", TargetHostId: "host-b", Owner: owner}

	t.Run("bounded waits: HOST_UNAVAILABLE naming the host, in time", func(t *testing.T) {
		s := NewServer(slotHeldCluster(t, true))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		start := time.Now()
		_, err := s.Describe(ctx, req)
		assert.Equal(t, outcomeHostUnavailable, hostOutcomeOf(t, err))
		assert.Equal(t, "host-b", contracts.UnreachableHost(err), "the answer names the host the call was routed to")
		assert.Contains(t, status.Convert(err).Message(), `host "host-b" is unreachable`)
		assert.Less(t, time.Since(start), 1900*time.Millisecond, "answered before the manager's deadline")
		assert.NoError(t, ctx.Err())
	})

	// The defect V3c pinned: without the bound, the call waited for its own
	// deadline and the context error was answered as a failure of the
	// operation on the host (codes.Unknown, VM_OPERATION_FAILED).
	t.Run("unbounded waits (as before): Unknown at the deadline", func(t *testing.T) {
		s := NewServer(slotHeldCluster(t, false))
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		_, err := s.Describe(ctx, req)
		st := status.Convert(err)
		assert.Equal(t, codes.Unknown, st.Code())
		assert.Contains(t, st.Message(), `failed to describe VM on host "host-b"`)
	})
}

func TestRoutedRPCError_HostUnavailableNamesTheRoutedHostOnly(t *testing.T) {
	// The lease of the routed host failed: named.
	leaseErr := &routedHostUnavailableError{host: "host-b",
		pe: contracts.NewHostUnavailableError(`connect to host "host-b"`, fmt.Errorf("dial qemu+ssh://virt@host-b/system: refused"))}
	err := routedRPCError("delete VM", leaseErr)
	assert.Equal(t, "host-b", contracts.UnreachableHost(err))
	assert.True(t, contracts.IsHostUnavailable(leaseErr), "the wrapper is transparent")
	assert.NotContains(t, status.Convert(err).Message(), "qemu+ssh", "the endpoint never crosses the wire")

	// A transport failure on the leased host before anything ran: named.
	hostDown := &hostOpError{host: "host-b", progress: &hostProgress{},
		err: fmt.Errorf("failed to list domains: %w", &VirshError{ExitCode: -1, Cause: &hostNotAnsweringError{wait: hostWaitSession}})}
	err = routedRPCError("delete VM", hostDown)
	assert.Equal(t, outcomeHostUnavailable, hostOutcomeOf(t, err))
	assert.Equal(t, "host-b", contracts.UnreachableHost(err))

	// The cluster-wide guard could not reach a host — possibly another one:
	// HOST_UNAVAILABLE, but no host is named.
	guard := &clusterGuardIncompleteError{op: "delete", domain: "web", unreachable: true}
	err = routedRPCError("delete VM", &hostOpError{host: "host-b", err: guard, progress: &hostProgress{}})
	assert.Equal(t, outcomeHostUnavailable, hostOutcomeOf(t, err))
	assert.Empty(t, contracts.UnreachableHost(err))
}

// startSilentSSHServer starts a loopback SSH server that completes the
// handshake but never confirms a channel — a host whose connection is up but
// that does not answer — and returns its address and host key.
func startSilentSSHServer(t *testing.T) (string, ssh.Signer) {
	t.Helper()
	hostKey := generateTestHostKey(t)
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			nConn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, nConn)
			mu.Unlock()
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nConn, config)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for range chans { //nolint:revive // never accepted, never rejected: the client waits
				}
			}()
		}
	}()
	return ln.Addr().String(), hostKey
}

func TestRunOverSSH_ClusteredSessionOpenBoundedBeforeTheDeadline(t *testing.T) {
	addr, hostKey := startSilentSSHServer(t)
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, hostKey.PublicKey())+"\n")
	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "unused"})
	v.boundHostWaits = true
	t.Cleanup(func() { _ = v.Cleanup() })

	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := v.runOverSSH(ctx, "virsh list --all")
	require.Error(t, err)
	assert.True(t, isHostNotAnswering(err), "got %v", err)
	assert.True(t, isHostTransportFailure(err))
	assert.Less(t, time.Since(start), 1150*time.Millisecond, "answered before the call's deadline")
	assert.NotNil(t, v.sshClient, "the connection is left to the calls that still use it")
}

func TestDialSSH_ClusteredHandshakeBoundedBeforeTheDeadline(t *testing.T) {
	// A host that accepts the TCP connection and never speaks SSH.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var held []net.Conn
	var mu sync.Mutex
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	addr := ln.Addr().String()
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, generateTestHostKey(t).PublicKey())+"\n")
	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "unused"})
	v.boundHostWaits = true

	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = v.runVirshCommand(ctx, "list", "--all")
	require.Error(t, err)
	assert.True(t, isHostNotAnswering(err), "got %v", err)
	assert.True(t, isHostTransportFailure(err))
	assert.Less(t, time.Since(start), 1150*time.Millisecond, "answered (not retried) before the call's deadline")
}
