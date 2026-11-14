//go:build virtrigaud_dev

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

package cloudhypervisor

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
)

// errPoolClosed is returned by reconcile once the pool is closed.
var errPoolClosed = errors.New("the host pool is closed")

// pool holds one client connection per host the Provider fronts, created
// from the inventory and closed when the host leaves it.
type pool struct {
	opts *Options
	log  *slog.Logger

	mu         sync.Mutex
	hosts      map[string]*hostEntry
	unroutable []string
	closed     bool
}

// hostEntry is one fronted host: its inventory entry, its lazy connection,
// its breaker, the transport the connection runs on now and the Hello made
// on a transport.
type hostEntry struct {
	id      string
	trust   *agentTrust
	breaker *resilience.CircuitBreaker
	opts    *Options

	// helloSlot serialises the Hello of one host; a waiter gives up when its
	// context ends.
	helloSlot chan struct{}

	mu   sync.Mutex
	spec hostsecret.Host
	conn *grpc.ClientConn
	// gen numbers the client connections of the entry (a recycle makes a
	// new one), so that a stale transport event is told apart. A client
	// connection carries one transport only: once one began, it dials no
	// other (genUsed), so that no call, not even one grpc-go retries
	// transparently, reaches a new transport that had no Hello.
	gen     uint64
	genUsed bool
	closed  bool
	// transport is the transport the current connection runs on, from the
	// connection's stats events: its local address names it uniquely.
	transport transportInfo
	// leafNotAfter is the expiry of the agent certificate the last handshake
	// accepted.
	leafNotAfter time.Time
	// hello is the outcome of the Hello made on the transport helloLocal:
	// nil for a compatible agent, the refusal for an incompatible one.
	hello      error
	helloLocal string
	// apiVersion is the agent's API version from the last verified Hello.
	apiVersion uint32
}

// transportInfo is one established transport of an entry's connection.
type transportInfo struct {
	gen      uint64
	local    string
	began    time.Time
	notAfter time.Time
}

// errEntryClosed is returned for a call that holds an entry the inventory
// removed or replaced.
var errEntryClosed = errors.New("the host's entry is closed")

// newPool returns an empty pool.
func newPool(opts *Options, log *slog.Logger) *pool {
	return &pool{opts: opts, log: log, hosts: map[string]*hostEntry{}}
}

// get returns the entry of a fronted host.
func (p *pool) get(host string) (*hostEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.hosts[host]
	return e, ok
}

// ids returns the fronted host ids, sorted.
func (p *pool) ids() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.hosts))
	for id := range p.hosts {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// newEntry builds the entry of an inventory host. Nothing is dialed yet.
func (p *pool) newEntry(h hostsecret.Host) (*hostEntry, error) {
	trust, err := newAgentTrust(h, p.opts.ProviderUID, p.opts.Now)
	if err != nil {
		return nil, err
	}
	e := &hostEntry{id: h.ID, trust: trust, breaker: newHostBreaker(h.ID, p.opts.Breaker), opts: p.opts,
		helloSlot: make(chan struct{}, 1), spec: h}
	trust.accepted = e.accepted
	cc, err := e.dial(1)
	if err != nil {
		return nil, err
	}
	e.conn, e.gen = cc, 1
	return e, nil
}

// dial returns a new lazy client connection of generation gen.
func (e *hostEntry) dial(gen uint64) (*grpc.ClientConn, error) {
	return dialAgent(e.spec.Endpoint, e.trust, e.opts.ClientCertificate, e.dialer(gen), connStats{e: e, gen: gen})
}

// errConnectionRetired is returned by the dialer of a client connection that
// already carried a transport.
var errConnectionRetired = errors.New("the client connection already carried a transport")

// dialer is the dial function of connection gen: it refuses once a transport
// of gen began, or when gen is no longer the entry's connection.
func (e *hostEntry) dialer(gen uint64) func(ctx context.Context, addr string) (net.Conn, error) {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		e.mu.Lock()
		retired := gen != e.gen || e.genUsed || e.closed
		e.mu.Unlock()
		if retired {
			return nil, errConnectionRetired
		}
		if e.opts.DialContext != nil {
			return e.opts.DialContext(ctx, addr)
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

// accepted records the expiry of an agent certificate a handshake accepted.
func (e *hostEntry) accepted(notAfter time.Time) {
	e.mu.Lock()
	e.leafNotAfter = notAfter
	e.mu.Unlock()
}

// transportBegan records that connection gen established a transport whose
// local address is local.
func (e *hostEntry) transportBegan(gen uint64, local string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if gen != e.gen {
		return
	}
	e.genUsed = true
	e.transport = transportInfo{gen: gen, local: local, began: e.opts.Now(), notAfter: e.leafNotAfter}
}

// transportEnded records that the transport local of connection gen ended.
func (e *hostEntry) transportEnded(gen uint64, local string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.transport.gen == gen && e.transport.local == local {
		e.transport = transportInfo{}
	}
}

// connection returns the entry's client connection for a new call. A
// connection whose transport ended, or whose transport is older than the maximum connection age, or
// whose agent certificate has expired, is replaced first, so that the agent
// is verified again by a new handshake. A closed entry returns
// errEntryClosed.
func (e *hostEntry) connection() (*grpc.ClientConn, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, errEntryClosed
	}
	t, now := e.transport, e.opts.Now()
	ended := e.genUsed && t.local == ""
	stale := ended || !t.began.IsZero() && (now.Sub(t.began) >= e.opts.MaxConnectionAge || (!t.notAfter.IsZero() && !now.Before(t.notAfter)))
	if !stale {
		cc := e.conn
		e.mu.Unlock()
		return cc, nil
	}
	cc, err := e.dial(e.gen + 1)
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	old := e.conn
	e.conn, e.gen, e.genUsed, e.transport = cc, e.gen+1, false, transportInfo{}
	e.mu.Unlock()
	_ = old.Close()
	return cc, nil
}

// awaitConnection gives cc up to limit to become READY. It reports false
// only when a connection attempt is still under way at limit (an agent that
// accepts connections and never completes the handshake): nothing can be
// sent. Otherwise the call proceeds: on a READY connection, or failing fast
// with the error of the attempt that failed. A connection that failed before
// is retried at once rather than after its backoff (and the call waits for
// that attempt), so that a host that came back is reached by the next call;
// while a failed connection retries, its state stays TRANSIENT_FAILURE until
// it is READY.
func awaitConnection(ctx context.Context, cc *grpc.ClientConn, limit time.Duration) bool {
	s := cc.GetState()
	switch s {
	case connectivity.Ready, connectivity.Shutdown:
		return true
	case connectivity.TransientFailure:
		cc.ResetConnectBackoff()
	case connectivity.Idle:
		cc.Connect()
	}
	cctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	for {
		if !cc.WaitForStateChange(cctx, s) {
			// Still TRANSIENT_FAILURE: the retry failed too; fail fast with
			// its error. Still CONNECTING: nothing was established.
			return s == connectivity.TransientFailure
		}
		s = cc.GetState()
		switch s {
		case connectivity.Ready, connectivity.Shutdown, connectivity.TransientFailure:
			return true
		case connectivity.Idle:
			cc.Connect()
		}
	}
}

// shutdown closes the entry and its connection, which fails its calls in
// flight; a call that still holds the entry is refused errEntryClosed.
func (e *hostEntry) shutdown() {
	e.mu.Lock()
	e.closed = true
	cc := e.conn
	e.mu.Unlock()
	_ = cc.Close()
}

// currentHello returns the outcome of the Hello made on the transport the
// connection runs on now, if one was made on it.
func (e *hostEntry) currentHello() (made bool, outcome error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.helloLocal == "" || e.helloLocal != e.transport.local {
		return false, nil
	}
	return true, e.hello
}

// forgetHello drops the Hello made on transport local, when a call found
// the connection on another transport than the one its Hello was made on.
func (e *hostEntry) forgetHello(local string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.helloLocal != local {
		e.helloLocal = ""
	}
}

// ready reports whether the host's agent passed its Hello on the current
// transport and its breaker is not open.
func (e *hostEntry) ready() bool {
	made, err := e.currentHello()
	return made && err == nil && e.breaker.GetState() != resilience.StateOpen
}

// sameConnection reports whether a and b name the same agent the same way:
// the same endpoint and the same TLS material. Labels are not part of it.
func sameConnection(a, b hostsecret.Host) bool {
	x, y := a.Credentials.AgentTLS, b.Credentials.AgentTLS
	if a.Endpoint != b.Endpoint || x == nil || y == nil {
		return false
	}
	if string(x.CABundle) != string(y.CABundle) || x.ExpectedSAN != y.ExpectedSAN || len(x.KeyPins) != len(y.KeyPins) {
		return false
	}
	for i := range x.KeyPins {
		if x.KeyPins[i] != y.KeyPins[i] {
			return false
		}
	}
	return true
}

// reconcile makes the fronted hosts those of inv. An entry that fails
// hostsecret.ValidateAgentHost, or shares its id with another, is not
// routed. A host that left the inventory, or whose endpoint or TLS material
// changed, has its connection closed before reconcile returns; a host whose
// labels alone changed keeps it.
func (p *pool) reconcile(inv hostsecret.Inventory) error {
	count := map[string]int{}
	for _, h := range inv.Hosts {
		count[h.ID]++
	}
	want := map[string]hostsecret.Host{}
	unroutable := map[string]bool{}
	for _, id := range inv.UnroutableHostIDs {
		unroutable[id] = true
	}
	for _, h := range inv.Hosts {
		if count[h.ID] > 1 {
			unroutable[h.ID] = true
			p.log.Warn("host inventory: duplicate host id; not routed", "host", h.ID)
			continue
		}
		if err := hostsecret.ValidateAgentHost(h); err != nil {
			unroutable[h.ID] = true
			p.log.Warn("host inventory: entry refused; not routed", "host", h.ID, "error", err.Error())
			continue
		}
		want[h.ID] = h
	}

	var closing []*hostEntry
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errPoolClosed
	}
	for id, e := range p.hosts {
		h, ok := want[id]
		switch {
		case !ok || !sameConnection(e.spec, h):
			closing = append(closing, e)
			delete(p.hosts, id)
		default:
			e.mu.Lock()
			e.spec = h
			e.mu.Unlock()
		}
	}
	for id, h := range want {
		if _, ok := p.hosts[id]; ok {
			continue
		}
		e, err := p.newEntry(h)
		if err != nil {
			unroutable[id] = true
			p.log.Warn("host inventory: entry refused; not routed", "host", id, "error", err.Error())
			continue
		}
		p.hosts[id] = e
	}
	p.unroutable = p.unroutable[:0]
	for id := range unroutable {
		p.unroutable = append(p.unroutable, id)
	}
	sort.Strings(p.unroutable)
	p.mu.Unlock()

	for _, e := range closing {
		p.log.Info("host inventory: closing the connection of a removed or changed host", "host", e.id)
		e.shutdown()
	}
	return nil
}

// close closes every connection. The pool fronts no host afterwards.
func (p *pool) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	hosts := p.hosts
	p.hosts = map[string]*hostEntry{}
	p.mu.Unlock()
	for _, e := range hosts {
		e.shutdown()
	}
}
