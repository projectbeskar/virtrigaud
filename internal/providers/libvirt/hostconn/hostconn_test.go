/*
Copyright 2025.

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
	"io"
	"strings"
	"testing"
)

// fakeConn is a minimal Conn used to exercise the Registry without any real
// host. It records Close calls so eviction/close behaviour can be asserted.
type fakeConn struct {
	id     HostID
	closed int
}

func (f *fakeConn) HostID() HostID { return f.id }
func (f *fakeConn) Virsh(_ context.Context, _ ...string) (*Result, error) {
	return &Result{Stdout: "ok"}, nil
}
func (f *fakeConn) RunHost(_ context.Context, _ ...string) (*Result, error) {
	return &Result{Stdout: "ok"}, nil
}
func (f *fakeConn) Stream(_ context.Context, _ ...string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (f *fakeConn) Close() error {
	f.closed++
	return nil
}

// TestNewRegistry_HoldsExactlyOneHost is the PR-2 shape: one Conn in, one host
// out, and ConnFor returns that same Conn. ADR-0007 P1 changes only the
// constructor call to pass N Conns; the Registry contract is unchanged.
func TestNewRegistry_HoldsExactlyOneHost(t *testing.T) {
	c := &fakeConn{id: "host-a"}
	reg, err := NewRegistry(c)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	if hosts := reg.Hosts(); len(hosts) != 1 || hosts[0] != "host-a" {
		t.Fatalf("Hosts() = %v, want [host-a]", hosts)
	}

	got, err := reg.ConnFor(context.Background(), "host-a")
	if err != nil {
		t.Fatalf("ConnFor(host-a): %v", err)
	}
	if got != c {
		t.Fatalf("ConnFor returned a different Conn than the one registered")
	}
}

// TestRegistry_ConnForUnknownHost verifies an unconfigured host is an error, not
// a silent nil — the Registry never invents a connection it does not hold.
func TestRegistry_ConnForUnknownHost(t *testing.T) {
	reg, err := NewRegistry(&fakeConn{id: "host-a"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, err := reg.ConnFor(context.Background(), "host-b"); err == nil {
		t.Fatal("ConnFor(host-b) succeeded, want error for unknown host")
	}
}

// TestRegistry_ConnForHonorsContext verifies a cancelled context short-circuits
// ConnFor (the lazy-dial path in ADR-0007 P1 depends on this contract).
func TestRegistry_ConnForHonorsContext(t *testing.T) {
	reg, err := NewRegistry(&fakeConn{id: "host-a"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reg.ConnFor(ctx, "host-a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ConnFor with cancelled ctx = %v, want context.Canceled", err)
	}
}

// TestRegistry_Hosts_MultiHostSorted proves the Registry does not assume one
// host: with several Conns it returns them all, sorted. This is the ADR-0007 P1
// shape validated ahead of the constructor change.
func TestRegistry_Hosts_MultiHostSorted(t *testing.T) {
	reg, err := NewRegistry(&fakeConn{id: "host-c"}, &fakeConn{id: "host-a"}, &fakeConn{id: "host-b"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got := reg.Hosts()
	want := []HostID{"host-a", "host-b", "host-c"}
	if len(got) != len(want) {
		t.Fatalf("Hosts() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Hosts() = %v, want %v", got, want)
		}
	}
}

// TestNewRegistry_RejectsDuplicateAndNil guards the two constructor error paths.
func TestNewRegistry_RejectsDuplicateAndNil(t *testing.T) {
	if _, err := NewRegistry(&fakeConn{id: "dup"}, &fakeConn{id: "dup"}); err == nil {
		t.Fatal("NewRegistry with duplicate HostID succeeded, want error")
	}
	if _, err := NewRegistry(nil); err == nil {
		t.Fatal("NewRegistry with nil Conn succeeded, want error")
	}
}

// TestRegistry_EvictClosesAndForgets verifies Evict closes the Conn and drops it
// so a later ConnFor fails.
func TestRegistry_EvictClosesAndForgets(t *testing.T) {
	c := &fakeConn{id: "host-a"}
	reg, err := NewRegistry(c)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	reg.Evict("host-a")
	if c.closed != 1 {
		t.Fatalf("Evict did not Close the Conn (closed=%d, want 1)", c.closed)
	}
	if _, err := reg.ConnFor(context.Background(), "host-a"); err == nil {
		t.Fatal("ConnFor after Evict succeeded, want error")
	}
	if hosts := reg.Hosts(); len(hosts) != 0 {
		t.Fatalf("Hosts() after Evict = %v, want empty", hosts)
	}

	// Evicting an absent host is a no-op, not a panic.
	reg.Evict("host-a")
}

// TestRegistry_CloseClosesEveryConn verifies Close closes all held Conns and
// empties the Registry.
func TestRegistry_CloseClosesEveryConn(t *testing.T) {
	a := &fakeConn{id: "host-a"}
	b := &fakeConn{id: "host-b"}
	reg, err := NewRegistry(a, b)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	if err := reg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if a.closed != 1 || b.closed != 1 {
		t.Fatalf("Close did not close every Conn (a=%d b=%d, want 1,1)", a.closed, b.closed)
	}
	if hosts := reg.Hosts(); len(hosts) != 0 {
		t.Fatalf("Hosts() after Close = %v, want empty", hosts)
	}
}

// closeErrConn is a Conn whose Close fails, to check Close error aggregation.
type closeErrConn struct {
	fakeConn
}

func (c *closeErrConn) Close() error { return errors.New("boom") }

// TestRegistry_CloseAggregatesErrors verifies Close surfaces a failing Conn's
// error rather than swallowing it.
func TestRegistry_CloseAggregatesErrors(t *testing.T) {
	reg, err := NewRegistry(&closeErrConn{fakeConn{id: "host-a"}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := reg.Close(); err == nil {
		t.Fatal("Close with a failing Conn returned nil, want error")
	}
}
