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
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// These tests pin the follow-up to V3c (rc6 lab): the dial of the persistent
// SSH client was held under a mutex that other calls waited for without a
// bound, so a Describe (30 s) that arrived while a call with a longer deadline
// (a Delete, a detached cleanup) was dialing a host that did not answer waited
// for that dial, not for its own deadline, and ended with its context error
// (codes.Unknown "failed to describe VM on host ...").

// startHandshakeStallServer starts a loopback server that accepts TCP
// connections and never speaks SSH — a host whose handshake does not finish
// (the connect itself is answered, as by a host behind a proxy that holds the
// connection). It returns the address and a counter of accepted connections.
func startHandshakeStallServer(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var accepted atomic.Int32
	var mu sync.Mutex
	var held []net.Conn
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
			accepted.Add(1)
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	return ln.Addr().String(), &accepted
}

// stalledClusteredProvider is a clustered host connection (boundHostWaits)
// whose host accepts TCP connections and never completes the SSH handshake.
func stalledClusteredProvider(t *testing.T) (*VirshProvider, *atomic.Int32) {
	t.Helper()
	addr, accepted := startHandshakeStallServer(t)
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, generateTestHostKey(t).PublicKey())+"\n")
	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "unused"})
	v.boundHostWaits = true
	t.Cleanup(func() { _ = v.Cleanup() })
	return v, accepted
}

type callResult struct {
	err     error
	took    time.Duration
	ctxDone bool
}

// runCalls starts n calls of fn, one every gap, each under its own deadline,
// and returns their results once all have ended.
func runCalls(n int, gap, deadline time.Duration, fn func(ctx context.Context) error) []callResult {
	results := make([]callResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			start := time.Now()
			err := fn(ctx)
			results[i] = callResult{err: err, took: time.Since(start), ctxDone: ctx.Err() != nil}
		}(i)
		time.Sleep(gap)
	}
	wg.Wait()
	return results
}

func TestEnsureSSHClient_ClusteredCallsArrivingDuringALongDialAreAnsweredInTime(t *testing.T) {
	v, accepted := stalledClusteredProvider(t)

	// A call with a long deadline starts dialing the host that does not
	// answer: its dial runs until its own bound.
	leaderDone := make(chan error, 1)
	leaderCtx, leaderCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer leaderCancel()
	go func() {
		_, err := v.runOverSSH(leaderCtx, "virsh list --all")
		leaderDone <- err
	}()
	require.Eventually(t, func() bool { return accepted.Load() > 0 }, 2*time.Second, 5*time.Millisecond,
		"the long call's dial is in flight (the host accepted its connection)")

	const deadline = 3 * time.Second
	results := runCalls(8, 20*time.Millisecond, deadline, func(ctx context.Context) error {
		_, err := v.runOverSSH(ctx, "virsh dominfo web")
		return err
	})
	for i, r := range results {
		require.Error(t, r.err, "call %d", i)
		assert.True(t, isHostNotAnswering(r.err), "call %d: got %v", i, r.err)
		assert.True(t, isHostTransportFailure(r.err), "call %d", i)
		assert.False(t, stderrors.Is(r.err, context.DeadlineExceeded), "call %d: never the call's context error: %v", i, r.err)
		assert.False(t, r.ctxDone, "call %d was answered before its deadline", i)
		assert.Less(t, r.took, deadline, "call %d", i)
		// Answered as a Describe is: HOST_UNAVAILABLE, never codes.Unknown.
		ho := &hostOpError{host: "h", progress: &hostProgress{}, err: r.err}
		assert.Equal(t, outcomeHostUnavailable, hostOutcomeOf(t, hostOpRPCErrorFor(hostOpRead, "describe VM", ho, r.err)), "call %d", i)
	}
	leaderCancel()
	select {
	case <-leaderDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the long call's dial did not end with its context")
	}
}

func TestEnsureSSHClient_ClusteredConcurrentCallsToAnUnreachableHostAllAnswerBeforeTheirDeadline(t *testing.T) {
	v, _ := stalledClusteredProvider(t)

	const deadline = 3 * time.Second
	// Staggered arrivals: later calls find an earlier call's dial in flight,
	// and the dials of calls whose bound ended before theirs.
	results := runCalls(8, 250*time.Millisecond, deadline, func(ctx context.Context) error {
		_, err := v.runVirshCommand(ctx, "dominfo", "web")
		return err
	})
	for i, r := range results {
		require.Error(t, r.err, "call %d", i)
		assert.True(t, isHostNotAnswering(r.err), "call %d: got %v", i, r.err)
		assert.False(t, r.ctxDone, "call %d was answered before its deadline", i)
		assert.Less(t, r.took, deadline, "call %d", i)
	}
	v.sshMu.Lock()
	defer v.sshMu.Unlock()
	assert.Nil(t, v.sshDialing, "no dial is left in flight")
	assert.Nil(t, v.sshClient)
}

func TestEnsureSSHClient_ClusteredCallsWaitingForADialShareItsClient(t *testing.T) {
	// A host that takes a while to answer the handshake, then answers.
	hostKey := generateTestHostKey(t)
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var accepted atomic.Int32
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
			accepted.Add(1)
			mu.Lock()
			conns = append(conns, nConn)
			mu.Unlock()
			go func() {
				select {
				case <-time.After(300 * time.Millisecond):
				case <-time.After(10 * time.Second):
					return
				}
				_, chans, reqs, err := ssh.NewServerConn(nConn, config)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for range chans { //nolint:revive // never accepted: only the connection matters
				}
			}()
		}
	}()
	addr := ln.Addr().String()
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, hostKey.PublicKey())+"\n")
	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "unused"})
	v.boundHostWaits = true
	t.Cleanup(func() { _ = v.Cleanup() })

	clients := make([]*ssh.Client, 8)
	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			clients[i], errs[i] = v.ensureSSHClient(ctx)
		}(i)
	}
	wg.Wait()
	for i := range clients {
		require.NoError(t, errs[i], "call %d", i)
		assert.Same(t, clients[0], clients[i], "call %d shares the one client", i)
	}
	assert.EqualValues(t, 1, accepted.Load(), "one dial for all the calls")
}

func TestEnsureSSHClient_ClusteredDialFailureIsTheAnswerOfTheCallsThatWaitedForIt(t *testing.T) {
	// A port nobody listens on: the dial is refused at once.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, generateTestHostKey(t).PublicKey())+"\n")
	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "unused"})
	v.boundHostWaits = true

	results := runCalls(4, 0, 3*time.Second, func(ctx context.Context) error {
		_, err := v.ensureSSHClient(ctx)
		return err
	})
	for i, r := range results {
		require.Error(t, r.err, "call %d", i)
		assert.False(t, isHostNotAnswering(r.err), "a refused dial is not a wait that ran out: %v", r.err)
		assert.False(t, r.ctxDone, "call %d", i)
	}
}

func TestEnsureSSHClient_SingleHostDialUnchanged(t *testing.T) {
	// The single-host connection dials under sshMu as before: no flight is
	// ever registered, and a refused dial is that dial's own error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, generateTestHostKey(t).PublicKey())+"\n")
	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "unused"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = v.ensureSSHClient(ctx)
	require.Error(t, err)
	assert.False(t, isHostNotAnswering(err), "got %v", err)
	assert.Nil(t, v.sshDialing)
	assert.Nil(t, v.sshClient)
}
