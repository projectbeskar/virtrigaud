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
