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
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/credentials"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
)

// Defaults of the pre-handshake throttle.
//
// In TLS 1.3 the server signs its CertificateVerify before it looks at the
// client's certificate, so every ClientHello from any peer that can reach the
// port costs the host key one signature. The throttle keeps a flood of
// unauthenticated connections from reaching the signer at all: a source
// address may open a few connections a second, and only a few handshakes run
// at once; the rest are closed before the TLS handshake starts. A Provider
// holds one long-lived connection (renewed every MaxConnectionAge), so the
// defaults leave it a wide margin. The agent port must still be firewalled to
// the Provider's address ranges: the throttle bounds the harm of a peer that
// can connect, it does not authenticate one.
const (
	// DefaultMaxHandshakes bounds the TLS handshakes in progress at once.
	DefaultMaxHandshakes = 16
	// DefaultPerSourceRate is the sustained new connections per second one
	// source address may open.
	DefaultPerSourceRate = 2.0
	// DefaultPerSourceBurst is the burst of new connections one source
	// address may open.
	DefaultPerSourceBurst = 10
	// maxTrackedSources bounds the source addresses the throttle remembers;
	// when it is full of active sources, a new source is refused.
	maxTrackedSources = 4096
	// refusalLogEvery is the shortest interval between two log lines of the
	// same kind of refusal; the lines carry the count suppressed since.
	refusalLogEvery = 30 * time.Second
)

// Throttle configures the pre-handshake throttle; a zero field takes its
// default.
type Throttle struct {
	// MaxHandshakes bounds the handshakes in progress at once.
	MaxHandshakes int
	// PerSourceRate and PerSourceBurst are the token bucket of each source
	// address, counted in new connections.
	PerSourceRate float64
	// PerSourceBurst is the bucket size of each source address.
	PerSourceBurst int
}

func (t Throttle) withDefaults() Throttle {
	if t.MaxHandshakes <= 0 {
		t.MaxHandshakes = DefaultMaxHandshakes
	}
	if t.PerSourceRate <= 0 {
		t.PerSourceRate = DefaultPerSourceRate
	}
	if t.PerSourceBurst <= 0 {
		t.PerSourceBurst = DefaultPerSourceBurst
	}
	return t
}

// logGate lets a log line through at most once per interval and counts what
// it held back, so that a flood is one line, not one line per connection.
type logGate struct {
	mu         sync.Mutex
	every      time.Duration
	last       time.Time
	suppressed uint64
}

// allow reports whether to log now, and how many lines were held back since
// the last one that went through.
func (g *logGate) allow(now time.Time) (bool, uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.last.IsZero() && now.Sub(g.last) < g.every {
		g.suppressed++
		return false, 0
	}
	n := g.suppressed
	g.suppressed, g.last = 0, now
	return true, n
}

// sourceBucket is one source address's token bucket.
type sourceBucket struct {
	tokens float64
	last   time.Time
}

// sourceLimiter limits new connections per source address.
type sourceLimiter struct {
	mu    sync.Mutex
	rate  float64
	burst float64
	m     map[string]*sourceBucket
}

func newSourceLimiter(rate float64, burst int) *sourceLimiter {
	return &sourceLimiter{rate: rate, burst: float64(burst), m: map[string]*sourceBucket{}}
}

// allow takes a token of source at now.
func (l *sourceLimiter) allow(source string, now time.Time) bool {
	if guard.Off(guard.SourceThrottle) {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.m[source]
	if !ok {
		if len(l.m) >= maxTrackedSources {
			l.prune(now)
			if len(l.m) >= maxTrackedSources {
				return false
			}
		}
		b = &sourceBucket{tokens: l.burst, last: now}
		l.m[source] = b
	}
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = min(l.burst, b.tokens+el*l.rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// prune forgets the sources whose bucket has refilled: they are
// indistinguishable from new ones.
func (l *sourceLimiter) prune(now time.Time) {
	for k, b := range l.m {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.m, k)
		}
	}
}

// sourceOf returns the host part of a connection's remote address.
func sourceOf(c net.Conn) string {
	addr := c.RemoteAddr()
	if addr == nil {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr.String()); err == nil {
		return host
	}
	return addr.String()
}

// throttle holds the pre-handshake limits of one server.
type throttle struct {
	cfg     Throttle
	sources *sourceLimiter
	slots   chan struct{}
	log     logr.Logger
	now     func() time.Time

	sourceGate, slotGate logGate
	sourceRefused        atomic.Uint64
	slotRefused          atomic.Uint64
}

func newThrottle(cfg Throttle, log logr.Logger, now func() time.Time) *throttle {
	cfg = cfg.withDefaults()
	return &throttle{cfg: cfg, sources: newSourceLimiter(cfg.PerSourceRate, cfg.PerSourceBurst),
		slots: make(chan struct{}, cfg.MaxHandshakes), log: log, now: now,
		sourceGate: logGate{every: refusalLogEvery}, slotGate: logGate{every: refusalLogEvery}}
}

// listener wraps lis so that a connection from a source above its rate is
// closed before it is returned.
func (t *throttle) listener(lis net.Listener) net.Listener {
	return &throttleListener{Listener: lis, t: t}
}

type throttleListener struct {
	net.Listener
	t *throttle
}

// Accept implements net.Listener.
func (l *throttleListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.t.sources.allow(sourceOf(c), l.t.now()) {
			return c, nil
		}
		_ = c.Close()
		l.t.sourceRefused.Add(1)
		if ok, n := l.t.sourceGate.allow(l.t.now()); ok {
			l.t.log.Info("connection refused before the handshake: source above its rate", "suppressedSinceLast", n,
				"refusedTotal", l.t.sourceRefused.Load())
		}
	}
}

// errTooManyHandshakes is returned to gRPC for a connection refused because
// the handshake slots are full.
var errTooManyHandshakes = errors.New("too many handshakes in progress")

// creds wraps transport credentials so that at most MaxHandshakes handshakes
// run at once; a connection that finds no slot is closed without a handshake,
// so it never reaches the signer.
func (t *throttle) creds(inner credentials.TransportCredentials) credentials.TransportCredentials {
	return &cappedCreds{TransportCredentials: inner, t: t}
}

type cappedCreds struct {
	credentials.TransportCredentials
	t *throttle
}

// ServerHandshake implements credentials.TransportCredentials.
func (c *cappedCreds) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	select {
	case c.t.slots <- struct{}{}:
		defer func() { <-c.t.slots }()
	default:
		if guard.Off(guard.HandshakeCap) {
			return c.TransportCredentials.ServerHandshake(conn)
		}
		_ = conn.Close()
		c.t.slotRefused.Add(1)
		if ok, n := c.t.slotGate.allow(c.t.now()); ok {
			c.t.log.Info("connection refused before the handshake: too many handshakes in progress", "suppressedSinceLast", n,
				"refusedTotal", c.t.slotRefused.Load())
		}
		return nil, nil, errTooManyHandshakes
	}
	return c.TransportCredentials.ServerHandshake(conn)
}

// Clone implements credentials.TransportCredentials.
func (c *cappedCreds) Clone() credentials.TransportCredentials {
	return &cappedCreds{TransportCredentials: c.TransportCredentials.Clone(), t: c.t}
}
