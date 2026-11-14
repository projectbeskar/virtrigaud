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
// its breaker and the result of the Hello on its current connection.
type hostEntry struct {
	id      string
	spec    hostsecret.Host
	conn    *grpc.ClientConn
	breaker *resilience.CircuitBreaker
	cancel  context.CancelFunc
	done    chan struct{}

	// helloMu serialises the Hello of one host.
	helloMu sync.Mutex

	mu sync.Mutex
	// epoch counts the times the connection left the READY state: a new
	// epoch is a new connection to the agent, which needs a new Hello.
	epoch uint64
	// hello is the outcome of the Hello made in helloEpoch: nil error for a
	// compatible agent, the refusal for an incompatible one.
	hello      error
	helloEpoch uint64
	helloDone  bool
	// apiVersion is the agent's API version from the last verified Hello.
	apiVersion uint32
}

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

// newEntry builds the entry of an inventory host and starts the watch of its
// connection state. Nothing is dialed yet.
func (p *pool) newEntry(h hostsecret.Host) (*hostEntry, error) {
	trust, err := newAgentTrust(h, p.opts.ProviderUID, p.opts.Now)
	if err != nil {
		return nil, err
	}
	cc, err := dialAgent(h.Endpoint, trust, p.opts.ClientCertificate, p.opts.DialContext)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &hostEntry{id: h.ID, spec: h, conn: cc, breaker: newHostBreaker(h.ID, p.opts.Breaker), cancel: cancel,
		done: make(chan struct{})}
	go e.watch(ctx)
	return e, nil
}

// watch bumps the entry's epoch every time its connection leaves READY, so
// that the next call makes a new Hello. It returns when ctx ends (shutdown).
func (e *hostEntry) watch(ctx context.Context) {
	defer close(e.done)
	for {
		s := e.conn.GetState()
		if !e.conn.WaitForStateChange(ctx, s) {
			return
		}
		if s == connectivity.Ready {
			e.mu.Lock()
			e.epoch++
			e.mu.Unlock()
		}
	}
}

// awaitConnection gives the connection to the agent up to limit to become
// READY. It reports false only when a connection attempt is still under way
// at limit (an agent that accepts connections and never completes the
// handshake): nothing can be sent. Otherwise the call proceeds: on a READY
// connection, or failing fast with the error of the attempt that failed. A
// connection that failed before is retried at once rather than after its
// backoff (and the call waits for that attempt), so that a host that came
// back is reached by the next call; while a failed connection retries, its
// state stays TRANSIENT_FAILURE until it is READY.
func (e *hostEntry) awaitConnection(ctx context.Context, limit time.Duration) bool {
	s := e.conn.GetState()
	switch s {
	case connectivity.Ready, connectivity.Shutdown:
		return true
	case connectivity.TransientFailure:
		e.conn.ResetConnectBackoff()
	case connectivity.Idle:
		e.conn.Connect()
	}
	cctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	for {
		if !e.conn.WaitForStateChange(cctx, s) {
			// Still TRANSIENT_FAILURE: the retry failed too; fail fast with
			// its error. Still CONNECTING: nothing was established.
			return s == connectivity.TransientFailure
		}
		s = e.conn.GetState()
		switch s {
		case connectivity.Ready, connectivity.Shutdown, connectivity.TransientFailure:
			return true
		case connectivity.Idle:
			e.conn.Connect()
		}
	}
}

// shutdown closes the entry's connection, which fails its calls in flight,
// and stops the watch of its state.
func (e *hostEntry) shutdown() {
	e.cancel()
	_ = e.conn.Close()
	<-e.done
}

// currentHello returns the outcome of the Hello made on the current
// connection, if one was made.
func (e *hostEntry) currentHello() (made bool, outcome error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.helloDone || e.helloEpoch != e.epoch {
		return false, nil
	}
	return true, e.hello
}

// ready reports whether the host's agent passed its Hello on the current
// connection and its breaker is not open.
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
