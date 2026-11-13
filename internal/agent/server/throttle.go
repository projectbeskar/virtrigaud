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
	"net/netip"
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
	// DefaultPerSourceHandshakes bounds the handshakes one source may have in
	// progress at once.
	DefaultPerSourceHandshakes = 2
	// DefaultHelloTimeout is how long a new connection may stay silent: it
	// must send its ClientHello within this time or it is closed, and until
	// then it holds no handshake slot.
	DefaultHelloTimeout = 2 * time.Second
	// maxTrackedSources bounds each generation of the table of source
	// addresses the throttle remembers (see sourceLimiter).
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
	// PerSourceHandshakes bounds the handshakes in progress from one source.
	PerSourceHandshakes int
	// HelloTimeout is how long a connection may stay silent before its
	// ClientHello.
	HelloTimeout time.Duration
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
	if t.PerSourceHandshakes <= 0 {
		t.PerSourceHandshakes = DefaultPerSourceHandshakes
	}
	if t.HelloTimeout <= 0 {
		t.HelloTimeout = DefaultHelloTimeout
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

// sourceBucket is one source's token bucket.
type sourceBucket struct {
	tokens float64
	last   time.Time
}

// sourceLimiter limits events per key (a source address) with one token
// bucket each. It remembers at most 2*maxTrackedSources keys, in two
// generations: a key is looked up in the current one and then in the
// previous one (and moved to the current one); when the current one is full
// it becomes the previous one and the old previous one is dropped. So the
// keys forgotten are those not seen for the longest, a new key never has to
// be refused, and the cost of forgetting is amortised O(1) per event, with
// no scan under the lock. A forgotten key starts again with a full bucket.
type sourceLimiter struct {
	mu       sync.Mutex
	rate     float64
	burst    float64
	max      int
	cur, old map[string]*sourceBucket
}

func newSourceLimiter(rate float64, burst int) *sourceLimiter {
	return &sourceLimiter{rate: rate, burst: float64(burst), max: maxTrackedSources,
		cur: map[string]*sourceBucket{}, old: map[string]*sourceBucket{}}
}

// allow takes a token of source at now.
func (l *sourceLimiter) allow(source string, now time.Time) bool {
	if guard.Off(guard.SourceThrottle) {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.cur[source]
	if !ok {
		if b, ok = l.old[source]; ok {
			delete(l.old, source)
		} else {
			b = &sourceBucket{tokens: l.burst, last: now}
		}
		if len(l.cur) >= l.max {
			l.old, l.cur = l.cur, make(map[string]*sourceBucket, l.max)
		}
		l.cur[source] = b
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

// size is the number of keys remembered.
func (l *sourceLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.cur) + len(l.old)
}

// sourceOf returns the key of a connection's source: its IPv4 address, or
// its IPv6 address truncated to the /64 (one subscriber or pod network holds
// a whole /64, so keying on the full address would give one host 2^64
// sources).
func sourceOf(c net.Conn) string {
	addr := c.RemoteAddr()
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	if ip, perr := netip.ParseAddr(host); perr == nil {
		ip = ip.Unmap()
		if ip.Is6() {
			if p, perr := ip.Prefix(64); perr == nil {
				return p.String()
			}
		}
		return ip.String()
	}
	return host
}

// throttle holds the pre-handshake limits of one server.
type throttle struct {
	cfg     Throttle
	sources *sourceLimiter
	slots   chan struct{}
	log     logr.Logger
	now     func() time.Time

	hsMu       sync.Mutex
	hsBySource map[string]int

	sourceGate, slotGate logGate
	sourceRefused        atomic.Uint64
	slotRefused          atomic.Uint64
}

func newThrottle(cfg Throttle, log logr.Logger, now func() time.Time) *throttle {
	cfg = cfg.withDefaults()
	return &throttle{cfg: cfg, sources: newSourceLimiter(cfg.PerSourceRate, cfg.PerSourceBurst),
		slots: make(chan struct{}, cfg.MaxHandshakes), log: log, now: now, hsBySource: map[string]int{},
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

// errTooManyHandshakes is returned for a handshake refused because the
// handshake slots, or the source's share of them, are taken.
var errTooManyHandshakes = errors.New("too many handshakes in progress")

// conn is a connection in the handshake: it knows its source, the slot it
// holds (none until the ClientHello has arrived) and the deadline to restore
// once it has.
type conn struct {
	net.Conn
	t        *throttle
	source   string
	deadline time.Time

	mu   sync.Mutex
	held bool
}

// acquire takes a handshake slot for this connection; it is called when the
// ClientHello has arrived, so a connection that stays silent holds nothing,
// and before the server signs anything. At most MaxHandshakes handshakes run
// at once and at most PerSourceHandshakes from one source; the rest fail
// before the signer is asked. It also gives the connection the rest of the
// handshake deadline back.
func (c *conn) acquire() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held {
		return nil
	}
	t := c.t
	if !guard.Off(guard.HandshakeCap) {
		t.hsMu.Lock()
		if t.hsBySource[c.source] >= t.cfg.PerSourceHandshakes {
			t.hsMu.Unlock()
			t.refuseSlot()
			return errTooManyHandshakes
		}
		t.hsBySource[c.source]++
		t.hsMu.Unlock()
		select {
		case t.slots <- struct{}{}:
		default:
			t.releaseSource(c.source)
			t.refuseSlot()
			return errTooManyHandshakes
		}
	}
	c.held = true
	_ = c.SetReadDeadline(c.deadline)
	return nil
}

// release gives the slot back; it is safe to call twice.
func (c *conn) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.held {
		return
	}
	c.held = false
	if !guard.Off(guard.HandshakeCap) {
		<-c.t.slots
		c.t.releaseSource(c.source)
	}
}

func (t *throttle) releaseSource(source string) {
	t.hsMu.Lock()
	defer t.hsMu.Unlock()
	if t.hsBySource[source] <= 1 {
		delete(t.hsBySource, source)
		return
	}
	t.hsBySource[source]--
}

func (t *throttle) refuseSlot() {
	t.slotRefused.Add(1)
	if ok, n := t.slotGate.allow(t.now()); ok {
		t.log.Info("handshake refused: too many handshakes in progress", "suppressedSinceLast", n,
			"refusedTotal", t.slotRefused.Load())
	}
}

// acquire is the TLS configuration's hook for the ClientHello: it takes the
// slot of the connection the hello came on.
func (t *throttle) acquire(hello net.Conn) error {
	c, ok := hello.(*conn)
	if !ok {
		return errors.New("handshake on a connection the throttle did not see")
	}
	return c.acquire()
}

// creds wraps transport credentials so that a connection must send its
// ClientHello within HelloTimeout, holding no slot until then; it takes a
// slot (see conn.acquire) for the rest of the handshake and gives it back
// when the handshake ends.
func (t *throttle) creds(inner credentials.TransportCredentials) credentials.TransportCredentials {
	return &cappedCreds{TransportCredentials: inner, t: t}
}

type cappedCreds struct {
	credentials.TransportCredentials
	t *throttle
}

// ServerHandshake implements credentials.TransportCredentials.
func (c *cappedCreds) ServerHandshake(raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	// gRPC has set a deadline for the whole handshake; keep it to restore
	// once the ClientHello is in, and meanwhile allow only HelloTimeout.
	deadline := c.t.now().Add(ConnectionTimeout)
	cn := &conn{Conn: raw, t: c.t, source: sourceOf(raw), deadline: deadline}
	defer cn.release()
	_ = raw.SetReadDeadline(c.t.now().Add(c.t.cfg.HelloTimeout))
	return c.TransportCredentials.ServerHandshake(cn)
}

// Clone implements credentials.TransportCredentials.
func (c *cappedCreds) Clone() credentials.TransportCredentials {
	return &cappedCreds{TransportCredentials: c.TransportCredentials.Clone(), t: c.t}
}
