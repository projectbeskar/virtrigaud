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
	"errors"
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

// TestSourceLimiterIsBounded: the table never grows past its bound; when it
// is full of active sources a new source is refused, and idle ones are
// forgotten first.
func TestSourceLimiterIsBounded(t *testing.T) {
	now := time.Now()
	l := newSourceLimiter(1, 1)
	for i := 0; i < maxTrackedSources; i++ {
		require.True(t, l.allow(net.IPv4(10, byte(i>>16), byte(i>>8), byte(i)).String(), now))
	}
	assert.False(t, l.allow("new", now), "full of active sources")
	assert.LessOrEqual(t, len(l.m), maxTrackedSources)
	assert.True(t, l.allow("new", now.Add(5*time.Second)), "idle sources were forgotten")
}

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

// TestHandshakeSlots: at most MaxHandshakes handshakes run at once; the next
// connection is closed without a handshake.
func TestHandshakeSlots(t *testing.T) {
	th := newThrottle(Throttle{MaxHandshakes: 1}, logr.Discard(), time.Now)
	th.slots <- struct{}{} // a handshake in progress
	c1, c2 := net.Pipe()
	defer func() { _ = c2.Close() }()
	cc := &cappedCreds{t: th}
	_, _, err := cc.ServerHandshake(c1)
	require.True(t, errors.Is(err, errTooManyHandshakes))
	assert.EqualValues(t, 1, th.slotRefused.Load())
	_, err = c1.Write([]byte{1})
	require.Error(t, err, "the refused connection was closed")
}
