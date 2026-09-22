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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
)

// trackConn is a Conn whose Close is race-safe to observe, used to assert
// drain/close timing without a live libvirtd. It optionally records how many
// operations ran through it.
type trackConn struct {
	id      HostID
	closed  atomic.Int32
	ops     atomic.Int32
	blockOp chan struct{} // when non-nil, Virsh blocks on it (to pin an op in-flight)
}

func (c *trackConn) HostID() HostID { return c.id }
func (c *trackConn) Virsh(ctx context.Context, _ ...string) (*Result, error) {
	c.ops.Add(1)
	if c.blockOp != nil {
		select {
		case <-c.blockOp:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &Result{Stdout: "ok"}, nil
}
func (c *trackConn) RunHost(context.Context, ...string) (*Result, error) {
	return &Result{Stdout: "ok"}, nil
}
func (c *trackConn) Stream(context.Context, ...string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (c *trackConn) Libvirt(context.Context) (*golibvirt.Libvirt, error) {
	return nil, errors.New("trackConn: Libvirt not implemented")
}
func (c *trackConn) Close() error    { c.closed.Add(1); return nil }
func (c *trackConn) closeCount() int { return int(c.closed.Load()) }

// mockDialer records every dial (host id + the material handed to it) and hands
// back a fresh trackConn per dial, so a test can assert both lazy-open (dial
// count) and that the right endpoint/credentials reached the dialer.
type mockDialer struct {
	mu      sync.Mutex
	calls   []hostsecret.Host       // every host dialed, in order
	history map[HostID][]*trackConn // all conns handed out per id (newest last)
	failFor map[HostID]error        // inject a dial failure for an id
}

func newMockDialer() *mockDialer {
	return &mockDialer{history: map[HostID][]*trackConn{}, failFor: map[HostID]error{}}
}

func (m *mockDialer) dial(_ context.Context, h hostsecret.Host) (Conn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, h)
	if err := m.failFor[HostID(h.ID)]; err != nil {
		return nil, err
	}
	c := &trackConn{id: HostID(h.ID)}
	m.history[HostID(h.ID)] = append(m.history[HostID(h.ID)], c)
	return c, nil
}

func (m *mockDialer) dialCount(id HostID) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, h := range m.calls {
		if HostID(h.ID) == id {
			n++
		}
	}
	return n
}

// nthConn returns the (0-based) n-th trackConn handed out for id.
func (m *mockDialer) nthConn(id HostID, n int) *trackConn {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.history[id]
	if n < 0 || n >= len(h) {
		return nil
	}
	return h[n]
}

// lastCallFor returns the most recent hostsecret.Host the dialer was asked to
// dial for id.
func (m *mockDialer) lastCallFor(id HostID) (hostsecret.Host, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.calls) - 1; i >= 0; i-- {
		if HostID(m.calls[i].ID) == id {
			return m.calls[i], true
		}
	}
	return hostsecret.Host{}, false
}

func host(id, endpoint string, key, known []byte) hostsecret.Host {
	return hostsecret.Host{
		ID:       id,
		Endpoint: endpoint,
		Credentials: hostsecret.Credentials{
			SSHPrivateKey: key,
			KnownHosts:    known,
		},
	}
}

func inv(hosts ...hostsecret.Host) hostsecret.Inventory {
	return hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: hosts}
}

// quietLogger discards output so the high-frequency reconcile logs in the
// concurrency soak do not flood test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func mustCluster(t *testing.T, d *mockDialer, i hostsecret.Inventory) *ClusterRegistry {
	t.Helper()
	r, err := NewClusterRegistry(i, d.dial, quietLogger())
	if err != nil {
		t.Fatalf("NewClusterRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func hostsEqual(got []HostID, want ...HostID) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestClusterRegistry_BuildsNHostsFromInventory is the fixture-parse shape: an
// N-host inventory yields an N-host registry keyed by id, and on first use each
// host's OWN endpoint + credential material reaches the dialer.
func TestClusterRegistry_BuildsNHostsFromInventory(t *testing.T) {
	d := newMockDialer()
	fixture := inv(
		host("host-b", "qemu+ssh://virt@host-b/system", []byte("KEY-B"), []byte("KH-B")),
		host("host-a", "qemu+ssh://virt@host-a/system", []byte("KEY-A"), []byte("KH-A")),
		host("host-c", "qemu+ssh://virt@host-c/system", []byte("KEY-C"), nil),
	)
	r := mustCluster(t, d, fixture)

	if got := r.Hosts(); !hostsEqual(got, "host-a", "host-b", "host-c") {
		t.Fatalf("Hosts() = %v, want [host-a host-b host-c]", got)
	}
	// Building the registry must NOT dial anything (lazy-open).
	if len(d.calls) != 0 {
		t.Fatalf("NewClusterRegistry dialed %d hosts, want 0 (lazy-open)", len(d.calls))
	}

	// First use of host-a hands host-a's material to the dialer.
	lc, err := r.ConnFor(context.Background(), "host-a")
	if err != nil {
		t.Fatalf("ConnFor(host-a): %v", err)
	}
	defer func() { _ = lc.Close() }()
	call, ok := d.lastCallFor("host-a")
	if !ok {
		t.Fatal("dialer was never called for host-a")
	}
	if call.Endpoint != "qemu+ssh://virt@host-a/system" {
		t.Fatalf("host-a endpoint = %q, want the host-a endpoint", call.Endpoint)
	}
	if string(call.Credentials.SSHPrivateKey) != "KEY-A" || string(call.Credentials.KnownHosts) != "KH-A" {
		t.Fatalf("host-a got wrong credential material: key=%q known=%q",
			call.Credentials.SSHPrivateKey, call.Credentials.KnownHosts)
	}
	if lc.HostID() != "host-a" {
		t.Fatalf("lease HostID = %q, want host-a", lc.HostID())
	}
}

// TestClusterRegistry_LazyOpen proves the ADR-0007 D3 lazy-open contract: a host
// appears in Hosts() with zero dials, and the dialer runs only on the FIRST
// ConnFor, then the connection is reused.
func TestClusterRegistry_LazyOpen(t *testing.T) {
	d := newMockDialer()
	r := mustCluster(t, d, inv(host("host-a", "ep-a", []byte("k"), []byte("kh"))))

	if d.dialCount("host-a") != 0 {
		t.Fatal("host-a dialed at construction, want lazy")
	}
	c1, err := r.ConnFor(context.Background(), "host-a")
	if err != nil {
		t.Fatalf("ConnFor: %v", err)
	}
	if d.dialCount("host-a") != 1 {
		t.Fatalf("first ConnFor dialed %d times, want 1", d.dialCount("host-a"))
	}
	_ = c1.Close()

	c2, err := r.ConnFor(context.Background(), "host-a")
	if err != nil {
		t.Fatalf("ConnFor #2: %v", err)
	}
	_ = c2.Close()
	if d.dialCount("host-a") != 1 {
		t.Fatalf("second ConnFor re-dialed (count=%d), want reuse (1)", d.dialCount("host-a"))
	}
}

// TestClusterRegistry_Reconcile_AddIsLazy proves a hot-reload that ADDS a host
// registers it (Hosts() grows) without dialing it; the dial waits for first use.
func TestClusterRegistry_Reconcile_AddIsLazy(t *testing.T) {
	d := newMockDialer()
	r := mustCluster(t, d, inv(host("host-a", "ep-a", []byte("k"), []byte("kh"))))

	if err := r.Reconcile(inv(
		host("host-a", "ep-a", []byte("k"), []byte("kh")),
		host("host-b", "ep-b", []byte("k"), []byte("kh")),
	)); err != nil {
		t.Fatalf("Reconcile add: %v", err)
	}
	if got := r.Hosts(); !hostsEqual(got, "host-a", "host-b") {
		t.Fatalf("Hosts() after add = %v, want [host-a host-b]", got)
	}
	if d.dialCount("host-b") != 0 {
		t.Fatal("added host-b was dialed eagerly, want lazy-open on first ConnFor")
	}
	c, err := r.ConnFor(context.Background(), "host-b")
	if err != nil {
		t.Fatalf("ConnFor(host-b): %v", err)
	}
	_ = c.Close()
	if d.dialCount("host-b") != 1 {
		t.Fatalf("host-b dialed %d times after first ConnFor, want 1", d.dialCount("host-b"))
	}
}

// TestClusterRegistry_Reconcile_RemoveIdleClosesNow proves a removed host that
// is idle at reconcile time is closed immediately and drops out of Hosts().
func TestClusterRegistry_Reconcile_RemoveIdleClosesNow(t *testing.T) {
	d := newMockDialer()
	r := mustCluster(t, d, inv(
		host("host-a", "ep-a", []byte("k"), []byte("kh")),
		host("host-b", "ep-b", []byte("k"), []byte("kh")),
	))
	// Dial host-b, then release it so it is idle.
	c, err := r.ConnFor(context.Background(), "host-b")
	if err != nil {
		t.Fatalf("ConnFor(host-b): %v", err)
	}
	_ = c.Close()
	connB := d.nthConn("host-b", 0)

	if err := r.Reconcile(inv(host("host-a", "ep-a", []byte("k"), []byte("kh")))); err != nil {
		t.Fatalf("Reconcile remove: %v", err)
	}
	if got := r.Hosts(); !hostsEqual(got, "host-a") {
		t.Fatalf("Hosts() after remove = %v, want [host-a]", got)
	}
	if connB.closeCount() != 1 {
		t.Fatalf("idle removed host-b closed %d times, want 1", connB.closeCount())
	}
	if _, err := r.ConnFor(context.Background(), "host-b"); err == nil {
		t.Fatal("ConnFor(host-b) after remove succeeded, want error")
	}
}

// TestClusterRegistry_Reconcile_RemoveInFlightIsNotSevered is THE non-severing
// guarantee: a host removed while a lease on it is outstanding is NOT closed
// underneath the borrower; it is drained (closed) only once that lease returns.
func TestClusterRegistry_Reconcile_RemoveInFlightIsNotSevered(t *testing.T) {
	d := newMockDialer()
	r := mustCluster(t, d, inv(
		host("host-a", "ep-a", []byte("k"), []byte("kh")),
		host("host-b", "ep-b", []byte("k"), []byte("kh")),
	))

	// Borrow host-b and hold the lease (an operation "in flight").
	lease, err := r.ConnFor(context.Background(), "host-b")
	if err != nil {
		t.Fatalf("ConnFor(host-b): %v", err)
	}
	connB := d.nthConn("host-b", 0)

	// Reconcile removes host-b while the lease is still held.
	if err := r.Reconcile(inv(host("host-a", "ep-a", []byte("k"), []byte("kh")))); err != nil {
		t.Fatalf("Reconcile remove: %v", err)
	}
	// It is gone from routing...
	if got := r.Hosts(); !hostsEqual(got, "host-a") {
		t.Fatalf("Hosts() = %v, want [host-a] (host-b draining)", got)
	}
	if _, err := r.ConnFor(context.Background(), "host-b"); err == nil {
		t.Fatal("ConnFor(host-b) during drain succeeded, want error (no new work)")
	}
	// ...but the in-flight connection is NOT severed.
	if connB.closeCount() != 0 {
		t.Fatalf("in-flight host-b closed underneath the borrower (count=%d), MUST be 0", connB.closeCount())
	}
	// The held lease still works.
	if _, err := lease.Virsh(context.Background(), "list"); err != nil {
		t.Fatalf("held lease Virsh failed: %v", err)
	}

	// Returning the lease drains it: now it closes.
	_ = lease.Close()
	if connB.closeCount() != 1 {
		t.Fatalf("host-b not closed after its last lease returned (count=%d), want 1", connB.closeCount())
	}
	// Using the lease after release is refused, not a use-after-close.
	if _, err := lease.Virsh(context.Background(), "list"); !errors.Is(err, errLeaseReleased) {
		t.Fatalf("lease use after Close = %v, want errLeaseReleased", err)
	}
}

// TestClusterRegistry_Reconcile_ChangeDrainsAndReopens proves an endpoint- or
// credential-change drains the old connection and reopens (lazily) a fresh one,
// while a label-only change leaves the connection intact.
func TestClusterRegistry_Reconcile_ChangeDrainsAndReopens(t *testing.T) {
	d := newMockDialer()
	r := mustCluster(t, d, inv(host("host-a", "ep-a", []byte("k1"), []byte("kh"))))

	c, err := r.ConnFor(context.Background(), "host-a")
	if err != nil {
		t.Fatalf("ConnFor: %v", err)
	}
	_ = c.Close()
	old := d.nthConn("host-a", 0)

	// Change the endpoint -> drain old, reopen lazily.
	if err := r.Reconcile(inv(host("host-a", "ep-a-NEW", []byte("k1"), []byte("kh")))); err != nil {
		t.Fatalf("Reconcile change: %v", err)
	}
	if old.closeCount() != 1 {
		t.Fatalf("changed host-a old conn closed %d times, want 1 (drained)", old.closeCount())
	}
	if got := r.Hosts(); !hostsEqual(got, "host-a") {
		t.Fatalf("Hosts() after change = %v, want [host-a]", got)
	}
	// Reopen is lazy: not yet re-dialed.
	if d.dialCount("host-a") != 1 {
		t.Fatalf("host-a re-dialed eagerly after change (count=%d), want lazy", d.dialCount("host-a"))
	}
	c2, err := r.ConnFor(context.Background(), "host-a")
	if err != nil {
		t.Fatalf("ConnFor after change: %v", err)
	}
	_ = c2.Close()
	if d.dialCount("host-a") != 2 {
		t.Fatalf("host-a dial count = %d after reopen, want 2", d.dialCount("host-a"))
	}
	newCall, _ := d.lastCallFor("host-a")
	if newCall.Endpoint != "ep-a-NEW" {
		t.Fatalf("reopened host-a endpoint = %q, want ep-a-NEW", newCall.Endpoint)
	}

	// A label-only change must NOT drain the (now second) connection.
	second := d.nthConn("host-a", 1)
	changed := host("host-a", "ep-a-NEW", []byte("k1"), []byte("kh"))
	changed.Labels = map[string]string{"zone": "r7"}
	if err := r.Reconcile(inv(changed)); err != nil {
		t.Fatalf("Reconcile label-only: %v", err)
	}
	if second.closeCount() != 0 {
		t.Fatalf("label-only change drained the connection (count=%d), must be 0", second.closeCount())
	}
	if d.dialCount("host-a") != 2 {
		t.Fatalf("label-only change re-dialed host-a (count=%d), want 2", d.dialCount("host-a"))
	}
}

// TestClusterRegistry_Reconcile_ChangeInFlightNotSevered proves a change on a
// host with an outstanding lease drains the OLD connection only once the lease
// returns — the same non-severing guarantee as remove.
func TestClusterRegistry_Reconcile_ChangeInFlightNotSevered(t *testing.T) {
	d := newMockDialer()
	r := mustCluster(t, d, inv(host("host-a", "ep-a", []byte("k1"), []byte("kh"))))

	lease, err := r.ConnFor(context.Background(), "host-a")
	if err != nil {
		t.Fatalf("ConnFor: %v", err)
	}
	old := d.nthConn("host-a", 0)

	if err := r.Reconcile(inv(host("host-a", "ep-a", []byte("k2"), []byte("kh")))); err != nil {
		t.Fatalf("Reconcile cred change: %v", err)
	}
	if old.closeCount() != 0 {
		t.Fatalf("old conn severed while lease held (count=%d), MUST be 0", old.closeCount())
	}
	_ = lease.Close()
	if old.closeCount() != 1 {
		t.Fatalf("old conn not drained after lease returned (count=%d), want 1", old.closeCount())
	}
}

// TestClusterRegistry_EmptyAndMalformedInventory proves an empty inventory is a
// valid (empty) registry, and defensive skips (empty id, duplicate id) never
// panic and never invent hosts.
func TestClusterRegistry_EmptyAndMalformedInventory(t *testing.T) {
	d := newMockDialer()

	r := mustCluster(t, d, inv())
	if got := r.Hosts(); len(got) != 0 {
		t.Fatalf("empty inventory -> Hosts() = %v, want empty", got)
	}

	// Empty-id and duplicate-id entries are skipped, not fatal.
	r2 := mustCluster(t, d, inv(
		host("", "ep", []byte("k"), nil),
		host("dup", "ep1", []byte("k"), nil),
		host("dup", "ep2", []byte("k"), nil),
		host("ok", "ep-ok", []byte("k"), nil),
	))
	if got := r2.Hosts(); !hostsEqual(got, "dup", "ok") {
		t.Fatalf("Hosts() = %v, want [dup ok] (empty id skipped, first dup wins)", got)
	}
}

// TestClusterRegistry_DialFailureIsRecoverable proves a failed lazy dial returns
// an error, does NOT wedge the host (inUse is undone), and a later successful
// dial works.
func TestClusterRegistry_DialFailureIsRecoverable(t *testing.T) {
	d := newMockDialer()
	d.failFor["host-a"] = errors.New("boom")
	r := mustCluster(t, d, inv(host("host-a", "ep-a", []byte("k"), []byte("kh"))))

	if _, err := r.ConnFor(context.Background(), "host-a"); err == nil {
		t.Fatal("ConnFor with failing dial succeeded, want error")
	}
	// The failed reservation must be undone: the host is still drainable and
	// re-dialable.
	delete(d.failFor, "host-a")
	c, err := r.ConnFor(context.Background(), "host-a")
	if err != nil {
		t.Fatalf("ConnFor after dial recovered: %v", err)
	}
	_ = c.Close()
	// Removing it now must close exactly the one good conn, proving inUse was
	// not leaked by the earlier failure.
	conn := d.nthConn("host-a", 0)
	if err := r.Reconcile(inv()); err != nil {
		t.Fatalf("Reconcile remove: %v", err)
	}
	if conn.closeCount() != 1 {
		t.Fatalf("recovered host closed %d times on remove, want 1 (inUse not leaked)", conn.closeCount())
	}
}

// TestClusterRegistry_ClosedRejects proves the terminal state: ConnFor/Reconcile
// error after Close, and Close is idempotent and closes held connections.
func TestClusterRegistry_ClosedRejects(t *testing.T) {
	d := newMockDialer()
	r, err := NewClusterRegistry(inv(host("host-a", "ep-a", []byte("k"), []byte("kh"))), d.dial, nil)
	if err != nil {
		t.Fatalf("NewClusterRegistry: %v", err)
	}
	c, err := r.ConnFor(context.Background(), "host-a")
	if err != nil {
		t.Fatalf("ConnFor: %v", err)
	}
	_ = c.Close()
	conn := d.nthConn("host-a", 0)

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if conn.closeCount() != 1 {
		t.Fatalf("Close did not close host-a (count=%d)", conn.closeCount())
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v, want nil (idempotent)", err)
	}
	if _, err := r.ConnFor(context.Background(), "host-a"); !errors.Is(err, errRegistryClosed) {
		t.Fatalf("ConnFor after Close = %v, want errRegistryClosed", err)
	}
	if err := r.Reconcile(inv()); !errors.Is(err, errRegistryClosed) {
		t.Fatalf("Reconcile after Close = %v, want errRegistryClosed", err)
	}
}

// TestClusterRegistry_NilDialerRejected guards the constructor contract.
func TestClusterRegistry_NilDialerRejected(t *testing.T) {
	if _, err := NewClusterRegistry(inv(), nil, nil); err == nil {
		t.Fatal("NewClusterRegistry with nil Dialer succeeded, want error")
	}
}

// TestClusterRegistry_ConcurrentConnForAndReconcile is the -race soak: many
// goroutines borrow/return connections while others hot-reload the host set.
// It must be race-free and must never sever a lease held across a reload (each
// borrower completes an op on its lease before returning it). Run under
// `go test -race`.
func TestClusterRegistry_ConcurrentConnForAndReconcile(t *testing.T) {
	d := newMockDialer()
	base := []hostsecret.Host{
		host("h0", "ep0", []byte("k"), []byte("kh")),
		host("h1", "ep1", []byte("k"), []byte("kh")),
		host("h2", "ep2", []byte("k"), []byte("kh")),
		host("h3", "ep3", []byte("k"), []byte("kh")),
	}
	r := mustCluster(t, d, inv(base...))

	const workers = 8
	const iters = 400
	var wg sync.WaitGroup

	// Borrowers: acquire a random host, run an op, return it.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				id := HostID(fmt.Sprintf("h%d", (seed+i)%4))
				c, err := r.ConnFor(context.Background(), id)
				if err != nil {
					continue // host may be mid-reload; that's fine
				}
				_, _ = c.Virsh(context.Background(), "list")
				_ = c.Close()
			}
		}(w)
	}

	// Reloaders: churn the host set (remove/add/change) concurrently.
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				var hosts []hostsecret.Host
				for j := 0; j < 4; j++ {
					if (i+j+seed)%5 == 0 {
						continue // drop this host this round
					}
					ep := fmt.Sprintf("ep%d", j)
					if (i+j+seed)%7 == 0 {
						ep += "-v2" // change this host this round
					}
					hosts = append(hosts, host(fmt.Sprintf("h%d", j), ep, []byte("k"), []byte("kh")))
				}
				_ = r.Reconcile(inv(hosts...))
			}
		}(w)
	}

	wg.Wait()
}
