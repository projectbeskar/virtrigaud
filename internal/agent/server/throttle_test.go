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

package server

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSourceLimiter(t *testing.T) {
	now := time.Now()
	l := newSourceLimiter(1, 2)
	assert.True(t, l.allow("a", now))
	assert.True(t, l.allow("a", now))
	assert.False(t, l.allow("a", now), "burst used")
	assert.True(t, l.allow("b", now), "another source has its own bucket")
	assert.True(t, l.allow("a", now.Add(1100*time.Millisecond)), "refilled")
	assert.False(t, l.allow("a", now.Add(1200*time.Millisecond)))
}

// TestSourceLimiterIsBounded: the table never holds more than two
// generations; a new source is never refused for lack of room (the sources
// seen longest ago are forgotten and start again with a full bucket), and a
// source that stays active keeps its bucket.
func TestSourceLimiterIsBounded(t *testing.T) {
	now := time.Now()
	l := newSourceLimiter(0.001, 1)
	require.True(t, l.allow("busy", now))
	require.False(t, l.allow("busy", now))
	for i := 0; i < 3*maxTrackedSources; i++ {
		require.True(t, l.allow(net.IPv4(10, byte(i>>16), byte(i>>8), byte(i)).String(), now), "a new source is never refused")
		if i%100 == 0 {
			require.False(t, l.allow("busy", now), "an active source keeps its bucket")
		}
	}
	assert.LessOrEqual(t, l.size(), 2*maxTrackedSources)
}

func TestSourceOf(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.7:1000":                    "192.0.2.7",
		"[::ffff:192.0.2.7]:1000":           "192.0.2.7",
		"[2001:db8:1:2:aaaa:bbbb:cccc:1]:9": "2001:db8:1:2::/64",
		"[2001:db8:1:2:1111:2222:3333:4]:9": "2001:db8:1:2::/64",
		"[2001:db8:1:3::1]:9":               "2001:db8:1:3::/64",
	} {
		c1, c2 := net.Pipe()
		_ = c2.Close()
		got := sourceOf(addrConn{c1, in})
		_ = c1.Close()
		assert.Equal(t, want, got, in)
	}
}

// addrConn is a connection with a chosen remote address.
type addrConn struct {
	net.Conn
	remote string
}

func (a addrConn) RemoteAddr() net.Addr { return fakeAddr(a.remote) }

type fakeAddr string

func (fakeAddr) Network() string  { return "tcp" }
func (a fakeAddr) String() string { return string(a) }

func TestLogGate(t *testing.T) {
	g := logGate{every: time.Minute}
	now := time.Now()
	ok, n := g.allow(now)
	assert.True(t, ok)
	assert.EqualValues(t, 0, n)
	for i := 0; i < 5; i++ {
		ok, _ = g.allow(now.Add(time.Duration(i) * time.Second))
		assert.False(t, ok)
	}
	ok, n = g.allow(now.Add(2 * time.Minute))
	assert.True(t, ok)
	assert.EqualValues(t, 5, n, "the held back lines are counted")
}

// TestThrottleListenerClosesExcessConnections: connections from a source
// above its rate are closed before they are returned to the server.
func TestThrottleListenerClosesExcessConnections(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	th := newThrottle(Throttle{PerSourceRate: 0.001, PerSourceBurst: 2}, logr.Discard(), time.Now)
	tl := th.listener(lis)
	defer func() { _ = tl.Close() }()
	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	var conns []net.Conn
	for i := 0; i < 5; i++ {
		c, err := net.Dial("tcp", lis.Addr().String())
		require.NoError(t, err)
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	require.Eventually(t, func() bool { return th.sourceRefused.Load() == 3 }, 5*time.Second, 10*time.Millisecond)
	assert.Len(t, accepted, 2, "only the burst reached the server")
}

// TestHandshakeSlots: at most MaxHandshakes handshakes run at once and at
// most PerSourceHandshakes from one source, counted from the ClientHello; a
// connection that has not sent one holds nothing, and a slot is given back.
func TestHandshakeSlots(t *testing.T) {
	th := newThrottle(Throttle{MaxHandshakes: 3, PerSourceHandshakes: 2}, logr.Discard(), time.Now)
	mk := func(remote string) *conn {
		c1, c2 := net.Pipe()
		t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
		return &conn{Conn: addrConn{c1, remote}, t: th, source: sourceOf(addrConn{c1, remote})}
	}
	a1, a2, a3, b1, b2 := mk("10.0.0.1:1"), mk("10.0.0.1:2"), mk("10.0.0.1:3"), mk("10.0.0.2:1"), mk("10.0.0.2:2")
	silent := mk("10.0.0.1:4")
	silent.release() // never acquired: holds nothing, releasing is a no-op
	require.NoError(t, a1.acquire())
	require.NoError(t, a2.acquire())
	require.ErrorIs(t, a3.acquire(), errTooManyHandshakes, "one source's share is 2")
	require.NoError(t, b1.acquire())
	require.ErrorIs(t, b2.acquire(), errTooManyHandshakes, "all 3 slots are taken")
	assert.EqualValues(t, 2, th.slotRefused.Load())
	a1.release()
	a1.release()
	require.NoError(t, b2.acquire(), "a released slot is reusable, once")
	require.ErrorIs(t, a3.acquire(), errTooManyHandshakes)
}

// TestPeerIdentityFailsClosed: a caller without a verified client
// certificate URI has no identity, so no signed answer is made for it.
func TestPeerIdentityFailsClosed(t *testing.T) {
	assert.Equal(t, "", peerIdentity(context.Background()))
}
