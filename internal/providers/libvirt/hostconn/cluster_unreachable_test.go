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
	"testing"
	"time"
)

// TestClusterRegistry_UnroutableHosts pins the hosts the registry knows but
// cannot route to (ADR-0007 A6.1 review, item 2a): entries it rejected (a
// duplicated id, an invalid endpoint) and the operator's tombstones; never an
// empty id, never a routable host; never dialed; replaced by every Reconcile.
func TestClusterRegistry_UnroutableHosts(t *testing.T) {
	d := newMockDialer()
	i := inv(host("h1", "ep-1", nil, nil), host("dup", "ep-2", nil, nil), host("dup", "ep-3", nil, nil),
		host("bad", "not a url", nil, nil), host("", "ep-4", nil, nil))
	i.UnroutableHostIDs = []string{"tomb", "h1", ""}
	r := mustCluster(t, d, i)

	if got := r.UnroutableHosts(); !hostsEqual(got, "bad", "dup", "tomb") {
		t.Fatalf("UnroutableHosts = %v", got)
	}
	if got := r.Hosts(); !hostsEqual(got, "h1") {
		t.Fatalf("Hosts = %v: an unroutable host is never routable", got)
	}
	for _, id := range []HostID{"bad", "dup", "tomb"} {
		if _, err := r.ConnFor(context.Background(), id); err == nil {
			t.Fatalf("%s must not be connectable", id)
		}
		if n := d.dialCount(id); n != 0 {
			t.Fatalf("%s was dialed %d times", id, n)
		}
	}

	if err := r.Reconcile(inv(host("h1", "ep-1", nil, nil))); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := r.UnroutableHosts(); len(got) != 0 {
		t.Fatalf("a Reconcile replaces the set; got %v", got)
	}
}

// TestClusterRegistry_RecentlyUnreachable pins the memo the cluster-wide disk
// guard uses to fail fast without dialing (ADR-0007 A6.1 review): a failed
// lazy dial or MarkUnreachable makes a host recently unreachable for the
// window given; a successful dial clears it; unknown hosts report false; and
// ConnFor itself still dials.
func TestClusterRegistry_RecentlyUnreachable(t *testing.T) {
	d := newMockDialer()
	d.failFor["h1"] = errors.New("no route to host")
	r := mustCluster(t, d, inv(host("h1", "ep-1", nil, nil), host("h2", "ep-2", nil, nil)))

	if r.RecentlyUnreachable("h1", time.Minute) {
		t.Fatal("never dialed: not known unreachable")
	}
	if _, err := r.ConnFor(context.Background(), "h1"); err == nil {
		t.Fatal("dial must fail")
	}
	if !r.RecentlyUnreachable("h1", time.Minute) {
		t.Fatal("a failed dial marks the host")
	}
	if r.RecentlyUnreachable("h1", 0) {
		t.Fatal("outside the window it is not recent")
	}
	if _, err := r.ConnFor(context.Background(), "h1"); err == nil || d.dialCount("h1") != 2 {
		t.Fatalf("ConnFor still dials (count %d)", d.dialCount("h1"))
	}

	d.mu.Lock()
	delete(d.failFor, "h1")
	d.mu.Unlock()
	c, err := r.ConnFor(context.Background(), "h1")
	if err != nil {
		t.Fatalf("ConnFor: %v", err)
	}
	_ = c.Close()
	if r.RecentlyUnreachable("h1", time.Minute) {
		t.Fatal("a successful dial clears the mark")
	}

	r.MarkUnreachable("h2")
	if !r.RecentlyUnreachable("h2", time.Minute) {
		t.Fatal("MarkUnreachable marks a routable host")
	}
	r.MarkUnreachable("nope")
	if r.RecentlyUnreachable("nope", time.Minute) {
		t.Fatal("an unknown host is never reported")
	}
}

// TestClusterRegistry_Snapshot (A6.1 fix verification, N6): Snapshot reads
// the routable hosts, the unroutable ones and the recently unreachable ones
// under one lock, and agrees with Hosts, UnroutableHosts and
// RecentlyUnreachable.
func TestClusterRegistry_Snapshot(t *testing.T) {
	d := newMockDialer()
	i := inv(host("h1", "ep-1", nil, nil), host("h2", "ep-2", nil, nil), host("bad", "not a url", nil, nil))
	i.UnroutableHostIDs = []string{"tomb"}
	r := mustCluster(t, d, i)
	r.MarkUnreachable("h2")

	s := r.Snapshot(time.Minute)
	if !hostsEqual(s.Routable, "h1", "h2") || !hostsEqual(s.Routable, r.Hosts()...) {
		t.Fatalf("Routable = %v, Hosts = %v", s.Routable, r.Hosts())
	}
	if !hostsEqual(s.Unroutable, "bad", "tomb") || !hostsEqual(s.Unroutable, r.UnroutableHosts()...) {
		t.Fatalf("Unroutable = %v, UnroutableHosts = %v", s.Unroutable, r.UnroutableHosts())
	}
	if !hostsEqual(s.RecentlyUnreachable, "h2") {
		t.Fatalf("RecentlyUnreachable = %v", s.RecentlyUnreachable)
	}
	if got := r.Snapshot(0).RecentlyUnreachable; len(got) != 0 {
		t.Fatalf("outside the window nothing is recent; got %v", got)
	}
}
