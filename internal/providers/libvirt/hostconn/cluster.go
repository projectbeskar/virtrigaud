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

package hostconn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
)

// ClusterRegistry is the N-host Registry for a clustered libvirt provider
// (ADR-0007 D3). Where NewRegistry serves a fixed, pre-built one-host set, a
// ClusterRegistry projects the host set from a parsed inventory
// (hostsecret.Inventory) and reconciles it on change, honouring two invariants
// the ADR (D3, ~206-216) makes load-bearing:
//
//   - lazy-open on host-add: a newly-added host is registered but NOT dialed;
//     the Dialer runs on the first ConnFor for that host, never at reconcile
//     time. So a reload that adds ten hosts costs zero connections until work
//     is actually routed to one.
//   - graceful-drain on host-remove: a removed (or changed) host stops taking
//     new work immediately, but its live connection is closed ONLY once it is
//     idle. A reload therefore never severs an in-flight operation — the
//     connection outlives the reconcile that removed it and is closed when the
//     last borrower returns it.
//
// Access model: a caller borrows a connection with ConnFor and returns it by
// Close-ing the returned Conn (a per-borrow lease wrapper — Close releases the
// lease, it does NOT close the shared underlying connection). The registry
// refuses to close a host's underlying connection while any lease on it is
// outstanding; that refcount is exactly what makes graceful-drain provably
// non-severing. (No RPC handler drives this yet — ADR-0007 P1 builds and
// hot-reloads the connections; host-routed Create/Migrate are later PRs.)
//
// It satisfies the Registry interface, so a clustered provider holds it exactly
// where a single-host provider holds a NewRegistry result.
type ClusterRegistry struct {
	// mu guards live, draining, closed, and every mutable field of every entry
	// (inUse, draining, closed, conn, spec). It is a plain Mutex — the hot path
	// (ConnFor) briefly reserves a lease under it and then dials OUTSIDE it, so
	// a slow network dial never blocks other hosts' ConnFor/Reconcile.
	mu sync.Mutex

	// dial opens a Conn for one host from its inventory entry. It is invoked
	// lazily (first ConnFor per host), never at reconcile time. The libvirt
	// provider supplies the production Dialer (builds a *virshConn); tests inject
	// a mock so the registry/reload can be exercised without a live libvirtd.
	dial Dialer

	// live is the routable host set, keyed by HostID. An entry here may be
	// undialed (conn == nil) until its first ConnFor.
	live map[HostID]*clusterEntry

	// draining holds entries removed/superseded by a reconcile that were still
	// in use at that moment: not routable, closed by release() when their last
	// lease returns. Idle-at-removal entries are closed inline by the reconcile
	// and never land here.
	draining []*clusterEntry

	// closed is set by Close; it rejects further ConnFor/Reconcile and is the
	// terminal state.
	closed bool

	// logger records coarse, non-secret reload events (host id + reason only).
	logger *slog.Logger
}

// clusterEntry is one host's slot in a ClusterRegistry: its desired connection
// spec, its (lazily-dialed) live connection, and the bookkeeping that makes
// graceful-drain non-severing.
type clusterEntry struct {
	id   HostID
	spec hostsecret.Host

	// dialMu serializes the lazy dial for THIS entry so concurrent first-time
	// ConnFor callers dial exactly once. It guards only the dial decision; conn
	// itself is read and written under the registry mu (never under dialMu
	// alone) so there is a single lock discipline for shared state.
	dialMu sync.Mutex

	// conn is the live connection, nil until the first ConnFor dials it.
	// Read/written under ClusterRegistry.mu.
	conn Conn
	// inUse is the count of outstanding leases (ConnFor results not yet
	// Close-d). Guarded by ClusterRegistry.mu. A draining entry with inUse > 0
	// is kept alive; release closes it when inUse falls to 0.
	inUse int
	// draining marks an entry removed/superseded: it takes no new leases and is
	// closed once idle. Guarded by ClusterRegistry.mu.
	draining bool
	// closed guards against a double Close of conn. Guarded by ClusterRegistry.mu.
	closed bool
}

// Dialer opens a Conn to one host from its inventory entry (endpoint + inlined
// credential material). The libvirt provider supplies the production Dialer;
// tests supply a mock. It is called lazily — on the first ConnFor for a host —
// never eagerly at reconcile time (ADR-0007 D3 lazy-open). ctx bounds the dial.
type Dialer func(ctx context.Context, host hostsecret.Host) (Conn, error)

// Compile-time proof a *ClusterRegistry is a drop-in Registry.
var _ Registry = (*ClusterRegistry)(nil)

// errRegistryClosed is returned by ConnFor/Reconcile after Close.
var errRegistryClosed = errors.New("hostconn: registry is closed")

// NewClusterRegistry builds an N-host registry from a parsed inventory,
// projecting one (lazily-dialed) entry per host keyed by HostID(host.id). It
// does NOT dial: connections open on first ConnFor (ADR-0007 D3 lazy-open).
// dial must be non-nil; logger may be nil (defaults to slog.Default()).
//
// A host with an empty id or an invalid endpoint is skipped, and a duplicated
// id makes EVERY entry with that id unroutable (see desiredHosts). None is
// fatal — a malformed inventory yields a smaller registry, never a panic
// (ADR-0007 D9 fail-safe).
func NewClusterRegistry(inv hostsecret.Inventory, dial Dialer, logger *slog.Logger) (*ClusterRegistry, error) {
	if dial == nil {
		return nil, errors.New("hostconn: NewClusterRegistry requires a non-nil Dialer")
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &ClusterRegistry{
		dial:   dial,
		live:   make(map[HostID]*clusterEntry),
		logger: logger,
	}
	// Reconcile-from-empty populates the live set with lazy entries, reusing the
	// exact add path the hot-reload uses.
	if err := r.Reconcile(inv); err != nil {
		return nil, err
	}
	return r, nil
}

// ConnFor returns a leased connection for id, dialing it lazily on the first
// call for that host (ADR-0007 D3). The returned Conn is a per-borrow lease:
// use it for one operation and Close it when done — Close releases the lease,
// it does not close the shared underlying connection. While any lease is
// outstanding the registry will not drain/close that host, which is what keeps
// a concurrent host-remove from severing this operation.
//
// It errors if id is unknown, is being drained, the registry is closed, ctx is
// done, or the lazy dial fails.
func (r *ClusterRegistry) ConnFor(ctx context.Context, id HostID) (Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Phase 1: reserve a lease under mu. Incrementing inUse BEFORE dialing is
	// what makes a concurrent remove wait for us regardless of the dial's
	// outcome — the reservation, not the finished connection, is the thing drain
	// blocks on.
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errRegistryClosed
	}
	e := r.live[id]
	if e == nil || e.draining {
		r.mu.Unlock()
		return nil, fmt.Errorf("hostconn: no connection for host %q", id)
	}
	e.inUse++
	existing := e.conn
	r.mu.Unlock()

	// Fast path: already dialed.
	if existing != nil {
		return newLeasedConn(r, e, existing), nil
	}

	// Phase 2: lazy dial, serialized per entry by dialMu so we dial exactly
	// once. dialMu is the outer lock; mu is taken only briefly inside, so there
	// is no lock-order inversion with release/Reconcile (which take mu alone).
	e.dialMu.Lock()
	r.mu.Lock()
	c := e.conn
	// Capture the spec under mu: Reconcile's unchanged path may refresh e.spec
	// (labels) concurrently, so the dialer must be handed a snapshot, never the
	// live field.
	spec := e.spec
	r.mu.Unlock()
	if c == nil {
		dialed, err := r.dial(ctx, spec)
		if err != nil {
			e.dialMu.Unlock()
			r.release(e) // undo the reservation; drains if the host was removed meanwhile
			return nil, fmt.Errorf("hostconn: dial host %q: %w", id, err)
		}
		r.mu.Lock()
		if r.closed {
			// Registry was closed while we dialed; don't stash a connection Close
			// will never see. Drop it here.
			r.mu.Unlock()
			e.dialMu.Unlock()
			_ = dialed.Close()
			r.release(e)
			return nil, errRegistryClosed
		}
		e.conn = dialed
		c = dialed
		r.mu.Unlock()
	}
	e.dialMu.Unlock()
	return newLeasedConn(r, e, c), nil
}

// release returns one lease. If the entry is draining and this was its last
// lease, its underlying connection is closed now (deferred graceful-drain
// completing) and it is dropped from the draining set.
func (r *ClusterRegistry) release(e *clusterEntry) {
	r.mu.Lock()
	if e.inUse > 0 {
		e.inUse--
	}
	var toClose Conn
	if e.inUse == 0 && e.draining && !e.closed && e.conn != nil {
		e.closed = true
		toClose = e.conn
		r.removeFromDrainingLocked(e)
	}
	r.mu.Unlock()
	if toClose != nil {
		_ = toClose.Close()
	}
}

// Reconcile drives the live host set to the parsed inventory, applying the D3
// semantics per host:
//
//   - add (id present in inv, absent here): register a lazy entry (no dial).
//   - remove (id here, absent in inv): graceful-drain — stop routing, close
//     when idle.
//   - change (id in both, endpoint or credential material differs): drain the
//     old connection and register a fresh lazy entry (drain-then-reopen). A
//     label-only change is NOT a connection change and does not drain.
//   - unchanged: keep the connection; refresh non-connection metadata (labels).
//
// It never dials and never severs an in-flight operation. It is safe to call
// concurrently with ConnFor. After Close it errors.
func (r *ClusterRegistry) Reconcile(inv hostsecret.Inventory) error {
	desired := desiredHosts(inv, r.logger)

	var toClose []Conn
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errRegistryClosed
	}

	// Existing entries: keep / change / remove.
	for id, e := range r.live {
		want, ok := desired[id]
		if !ok {
			r.startDrainLocked(e, &toClose)
			delete(r.live, id)
			r.logger.Info("hostconn: host removed; draining connection", "host", string(id))
			continue
		}
		if hostConnChanged(e.spec, want) {
			r.startDrainLocked(e, &toClose)
			r.live[id] = &clusterEntry{id: id, spec: want}
			r.logger.Info("hostconn: host changed; draining old connection and reopening lazily", "host", string(id))
			continue
		}
		// Unchanged connection: keep it, but refresh non-connection metadata
		// (labels) so future label consumers see the latest without a redial.
		e.spec = want
	}

	// Brand-new hosts: lazy entry, no dial.
	for id, want := range desired {
		if _, ok := r.live[id]; !ok {
			r.live[id] = &clusterEntry{id: id, spec: want}
			r.logger.Info("hostconn: host added; will dial lazily on first use", "host", string(id))
		}
	}
	r.mu.Unlock()

	for _, c := range toClose {
		_ = c.Close()
	}
	return nil
}

// desiredHosts projects a parsed inventory into the routable host set, applying
// the provider-side admission rules every entry must pass before it can ever be
// dialed. It is the single consumption point for BOTH the startup load
// (NewClusterRegistry) and the hot-reload (Watcher), so a hand-edited inventory
// Secret cannot bypass the Host CRD's admission validation:
//
//   - empty id: skipped (an unaddressable connection is never useful).
//   - invalid endpoint (hostsecret.ValidateEndpoint): skipped. The endpoint's
//     path becomes the libvirt connection URI forwarded to the hypervisor
//     host, so an endpoint outside the accepted shapes is never routable.
//   - duplicate id: EVERY entry carrying that id is skipped. A duplicate is an
//     ambiguous inventory (two endpoints/credential sets for one id); picking
//     "the first" would make routing depend on document order, so neither is
//     routable until the inventory is fixed.
//
// None of these is fatal — a malformed inventory yields a smaller registry,
// never a panic (ADR-0007 D9 fail-safe). Log lines carry the host id and a
// coarse reason only, never the endpoint or credential material.
func desiredHosts(inv hostsecret.Inventory, logger *slog.Logger) map[HostID]hostsecret.Host {
	counts := make(map[HostID]int, len(inv.Hosts))
	for _, h := range inv.Hosts {
		counts[HostID(h.ID)]++
	}

	desired := make(map[HostID]hostsecret.Host, len(inv.Hosts))
	for _, h := range inv.Hosts {
		id := HostID(h.ID)
		switch {
		case id == "":
			logger.Warn("hostconn: skipping inventory host with empty id")
			continue
		case counts[id] > 1:
			logger.Warn("hostconn: skipping duplicated inventory host id (ambiguous; no entry is routable)",
				"host", string(id), "occurrences", counts[id])
			continue
		}
		if err := hostsecret.ValidateEndpoint(h.Endpoint); err != nil {
			logger.Warn("hostconn: skipping inventory host with invalid endpoint",
				"host", string(id), "error", err.Error())
			continue
		}
		desired[id] = h
	}
	return desired
}

// startDrainLocked begins draining e (must hold mu). An idle entry is closed
// inline (its conn appended to toClose, closed by the caller outside mu); an
// in-use entry is parked in r.draining and closed by release when its last
// lease returns — never severed here.
func (r *ClusterRegistry) startDrainLocked(e *clusterEntry, toClose *[]Conn) {
	e.draining = true
	if e.inUse > 0 {
		r.draining = append(r.draining, e)
		return
	}
	// Idle: close now (if ever dialed) and drop.
	if !e.closed {
		e.closed = true
		if e.conn != nil {
			*toClose = append(*toClose, e.conn)
		}
	}
}

// removeFromDrainingLocked drops e from the draining slice (must hold mu).
func (r *ClusterRegistry) removeFromDrainingLocked(e *clusterEntry) {
	for i, d := range r.draining {
		if d == e {
			r.draining = append(r.draining[:i], r.draining[i+1:]...)
			return
		}
	}
}

// Hosts returns the ids of every routable host, sorted. Draining hosts are
// excluded — they are being removed and take no new work.
func (r *ClusterRegistry) Hosts() []HostID {
	r.mu.Lock()
	ids := make([]HostID, 0, len(r.live))
	for id := range r.live {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// HostMeta returns the NON-SECRET inventory metadata the registry holds for a
// routable host: its endpoint (address) and a copy of its placement labels. It
// is the read-only accessor the host-inventory RPCs (ADR-0007 P1 ListHosts/
// GetHostInfo) use to fill HostInfo.address / HostInfo.labels without dialing —
// the endpoint and labels come from the parsed inventory (hostsecret.Host), not
// from a live query.
//
// It deliberately never exposes hostsecret.Host.Credentials: a clustered
// provider must not surface connection secrets through an inventory RPC
// (ADR-0007 Security). The returned labels map is a defensive copy the caller
// may retain or mutate without racing Reconcile, which refreshes the live
// spec (labels included) under the same mu on a label-only change.
//
// ok is false for an unknown or draining host (mirroring Hosts(), which also
// excludes draining hosts): a host being removed takes no new work and reports
// no fresh inventory.
func (r *ClusterRegistry) HostMeta(id HostID) (address string, labels map[string]string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, present := r.live[id]
	if !present || e.draining {
		return "", nil, false
	}
	if len(e.spec.Labels) > 0 {
		labels = make(map[string]string, len(e.spec.Labels))
		for k, v := range e.spec.Labels {
			labels[k] = v
		}
	}
	return e.spec.Endpoint, labels, true
}

// Evict graceful-drains the connection for id, if present: it stops routing new
// work to id immediately and closes the underlying connection once it is idle
// (never severing an in-flight lease). Unlike the single-host registry's
// immediate Evict, a lease-tracking registry drains — the safe reading of the
// Registry contract for a connection that may be in use.
func (r *ClusterRegistry) Evict(id HostID) {
	var toClose []Conn
	r.mu.Lock()
	if e, ok := r.live[id]; ok {
		delete(r.live, id)
		r.startDrainLocked(e, &toClose)
	}
	r.mu.Unlock()
	for _, c := range toClose {
		_ = c.Close()
	}
}

// Close marks the registry closed and closes every connection it holds (live
// and draining), returning the joined close errors. It is safe to call more
// than once.
//
// Close force-closes even connections with outstanding leases: it is the
// shutdown path, invoked AFTER the gRPC server's GracefulStop has drained
// in-flight RPCs (so in practice no lease is outstanding). This differs from
// Reconcile/Evict, which never force-close an in-use connection — the
// non-severing guarantee applies to reloads, not to process shutdown.
func (r *ClusterRegistry) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	var conns []Conn
	for _, e := range r.live {
		e.draining = true
		if !e.closed && e.conn != nil {
			e.closed = true
			conns = append(conns, e.conn)
		}
	}
	r.live = make(map[HostID]*clusterEntry)
	for _, e := range r.draining {
		if !e.closed && e.conn != nil {
			e.closed = true
			conns = append(conns, e.conn)
		}
	}
	r.draining = nil
	r.mu.Unlock()

	var errs []error
	for _, c := range conns {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// hostConnChanged reports whether a host's CONNECTION-relevant fields differ
// between two inventory entries: the endpoint or either piece of credential
// material. Labels are intentionally excluded — a label-only change is placement
// metadata, not a reason to drain and re-dial a healthy connection.
func hostConnChanged(a, b hostsecret.Host) bool {
	return a.Endpoint != b.Endpoint ||
		!bytes.Equal(a.Credentials.SSHPrivateKey, b.Credentials.SSHPrivateKey) ||
		!bytes.Equal(a.Credentials.KnownHosts, b.Credentials.KnownHosts)
}

// leasedConn is the per-borrow lease ConnFor returns. It delegates every
// operation to the shared underlying Conn and, on Close, releases the lease
// (decrementing the host's in-use count) rather than closing that shared
// connection. Close is idempotent; using the lease after Close is a caller bug
// and is refused rather than allowed to touch a connection the registry may
// have since drained.
type leasedConn struct {
	reg      *ClusterRegistry
	entry    *clusterEntry
	conn     Conn
	released atomic.Bool
	once     sync.Once
}

// newLeasedConn wraps conn as a lease held against e in registry r.
func newLeasedConn(r *ClusterRegistry, e *clusterEntry, conn Conn) *leasedConn {
	return &leasedConn{reg: r, entry: e, conn: conn}
}

// errLeaseReleased is returned by lease operations after the lease is Close-d.
var errLeaseReleased = errors.New("hostconn: connection lease already released")

// HostID returns the underlying connection's host id.
func (l *leasedConn) HostID() HostID { return l.conn.HostID() }

// Unwrap returns the shared underlying connection this lease borrows. It lets a
// caller that holds a lease reach a richer, driver-specific view of its OWN
// Conn — e.g. the libvirt provider narrowing to its *virshConn to run a
// create-on-host over the chosen connection (ADR-0007 P1) — without the lease
// having to re-export every driver method.
//
// It is valid ONLY while the lease is held (before Close): the returned Conn is
// the shared handle the registry may drain and close once the lease is
// released, so a caller must not retain it past Close, exactly as a Conn must
// not be cached across calls (hostconn.Conn doc). Unwrap deliberately does not
// consult released — it is an escape hatch for use under an active lease, and
// the release guard stays on the delegating methods (Virsh/RunHost/…) that a
// post-Close caller would otherwise reach.
func (l *leasedConn) Unwrap() Conn { return l.conn }

// Virsh delegates to the underlying connection (refused after release).
func (l *leasedConn) Virsh(ctx context.Context, args ...string) (*Result, error) {
	if l.released.Load() {
		return nil, errLeaseReleased
	}
	return l.conn.Virsh(ctx, args...)
}

// RunHost delegates to the underlying connection (refused after release).
func (l *leasedConn) RunHost(ctx context.Context, argv ...string) (*Result, error) {
	if l.released.Load() {
		return nil, errLeaseReleased
	}
	return l.conn.RunHost(ctx, argv...)
}

// Stream delegates to the underlying connection (refused after release).
func (l *leasedConn) Stream(ctx context.Context, argv ...string) (io.ReadCloser, error) {
	if l.released.Load() {
		return nil, errLeaseReleased
	}
	return l.conn.Stream(ctx, argv...)
}

// Libvirt delegates to the underlying connection (refused after release).
func (l *leasedConn) Libvirt(ctx context.Context) (*golibvirt.Libvirt, error) {
	if l.released.Load() {
		return nil, errLeaseReleased
	}
	return l.conn.Libvirt(ctx)
}

// Close releases the lease (idempotent). It does NOT close the shared
// underlying connection — the registry owns that lifecycle and closes it when
// the host drains and its last lease is released.
func (l *leasedConn) Close() error {
	l.once.Do(func() {
		l.released.Store(true)
		l.reg.release(l.entry)
	})
	return nil
}
