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
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
)

// fakeReconciler records the inventories handed to Reconcile so the watcher can
// be tested without a real registry, dialer, or host.
type fakeReconciler struct {
	mu  sync.Mutex
	got []hostsecret.Inventory
	ch  chan hostsecret.Inventory // optional signal (buffered); tests drain it
}

func (f *fakeReconciler) Reconcile(inv hostsecret.Inventory) error {
	f.mu.Lock()
	f.got = append(f.got, inv)
	f.mu.Unlock()
	if f.ch != nil {
		f.ch <- inv
	}
	return nil
}

func (f *fakeReconciler) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

func mustMarshal(t *testing.T, i hostsecret.Inventory) []byte {
	t.Helper()
	b, err := hostsecret.Marshal(i)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	return b
}

// writeProjectedSecret writes data to dir using the SAME atomic layout the
// kubelet uses for a projected Secret/ConfigMap: a timestamped data directory,
// a `..data` symlink pointing at it (swapped in with an atomic rename), and a
// visible file symlink through `..data`. Calling it again with a new generation
// performs the atomic `..data` swap that an inode-level watch would miss.
func writeProjectedSecret(t *testing.T, dir string, gen int, data []byte) {
	t.Helper()
	genDir := fmt.Sprintf("..%d_data", gen)
	absGenDir := filepath.Join(dir, genDir)
	if err := os.MkdirAll(absGenDir, 0o755); err != nil {
		t.Fatalf("mkdir gen dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(absGenDir, hostsecret.SecretDataKey), data, 0o600); err != nil {
		t.Fatalf("write gen file: %v", err)
	}

	// Atomically swap ..data -> genDir via a temp symlink + rename.
	dataLink := filepath.Join(dir, "..data")
	tmpLink := filepath.Join(dir, "..data_tmp")
	_ = os.Remove(tmpLink)
	if err := os.Symlink(genDir, tmpLink); err != nil {
		t.Fatalf("symlink tmp: %v", err)
	}
	if err := os.Rename(tmpLink, dataLink); err != nil {
		t.Fatalf("atomic rename ..data: %v", err)
	}

	// Visible file symlink through ..data (created once, survives swaps).
	visible := filepath.Join(dir, hostsecret.SecretDataKey)
	if _, err := os.Lstat(visible); os.IsNotExist(err) {
		if err := os.Symlink(filepath.Join("..data", hostsecret.SecretDataKey), visible); err != nil {
			t.Fatalf("symlink visible file: %v", err)
		}
	}
}

// TestLoadInventory covers the shared file-consumption path: valid, valid-empty,
// missing, malformed, and unsupported-schema.
func TestLoadInventory(t *testing.T) {
	dir := t.TempDir()

	t.Run("valid", func(t *testing.T) {
		path := filepath.Join(dir, "valid.json")
		if err := os.WriteFile(path, mustMarshal(t, inv(
			host("host-a", "ep-a", []byte("k"), []byte("kh")),
			host("host-b", "ep-b", []byte("k"), nil),
		)), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := LoadInventory(path)
		if err != nil {
			t.Fatalf("LoadInventory: %v", err)
		}
		if len(got.Hosts) != 2 {
			t.Fatalf("got %d hosts, want 2", len(got.Hosts))
		}
	})

	t.Run("valid empty is not an error", func(t *testing.T) {
		path := filepath.Join(dir, "empty.json")
		if err := os.WriteFile(path, mustMarshal(t, inv()), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := LoadInventory(path)
		if err != nil {
			t.Fatalf("LoadInventory(empty): %v, want nil (zero hosts is valid)", err)
		}
		if len(got.Hosts) != 0 {
			t.Fatalf("got %d hosts, want 0", len(got.Hosts))
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		if _, err := LoadInventory(filepath.Join(dir, "nope.json")); err == nil {
			t.Fatal("LoadInventory(missing) succeeded, want error")
		}
	})

	t.Run("malformed json errors", func(t *testing.T) {
		path := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadInventory(path); err == nil {
			t.Fatal("LoadInventory(malformed) succeeded, want error")
		}
	})

	t.Run("unsupported newer schema errors", func(t *testing.T) {
		path := filepath.Join(dir, "newer.json")
		if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"schemaVersion": %d, "hosts": []}`, hostsecret.SchemaVersion+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadInventory(path); err == nil {
			t.Fatal("LoadInventory(newer schema) succeeded, want error (must not mis-parse a changed shape)")
		}
	})
}

// TestWatcher_ReloadOnce_ReconcilesValid verifies a successful reload hands the
// parsed inventory to the reconciler.
func TestWatcher_ReloadOnce_ReconcilesValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, hostsecret.SecretDataKey)
	if err := os.WriteFile(path, mustMarshal(t, inv(host("host-a", "ep-a", []byte("k"), []byte("kh")))), 0o600); err != nil {
		t.Fatal(err)
	}
	fr := &fakeReconciler{}
	w, err := NewWatcher(path, fr, quietLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer func() { _ = w.Close() }()

	w.reloadOnce()
	if fr.count() != 1 {
		t.Fatalf("reloadOnce reconciled %d times, want 1", fr.count())
	}
}

// TestWatcher_ReloadOnce_KeepsLastGoodOnBadFile verifies a malformed or
// unsupported file does NOT reconcile — the last-good host set is kept, never
// drained, and nothing panics.
func TestWatcher_ReloadOnce_KeepsLastGoodOnBadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, hostsecret.SecretDataKey)
	if err := os.WriteFile(path, mustMarshal(t, inv(host("host-a", "ep-a", []byte("k"), []byte("kh")))), 0o600); err != nil {
		t.Fatal(err)
	}
	fr := &fakeReconciler{}
	w, err := NewWatcher(path, fr, quietLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer func() { _ = w.Close() }()

	w.reloadOnce() // valid -> reconcile #1
	if fr.count() != 1 {
		t.Fatalf("after valid reload, count=%d want 1", fr.count())
	}

	// Overwrite with garbage; a reload must NOT reconcile (keep last-good).
	if err := os.WriteFile(path, []byte("garbage{"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.reloadOnce()
	if fr.count() != 1 {
		t.Fatalf("malformed reload reconciled (count=%d), want kept at 1 (last-good)", fr.count())
	}
}

// TestWatcher_DirectoryWatch_AtomicSwapIsCaught is the crux of the file-watch
// design: with the backstop poll disabled, a reconcile can only be driven by an
// fsnotify EVENT. An atomic `..data` symlink swap (exactly how Kubernetes
// updates a mounted Secret) must still be caught — proving the watch is on the
// DIRECTORY, not the file inode (which the swap orphans).
func TestWatcher_DirectoryWatch_AtomicSwapIsCaught(t *testing.T) {
	dir := t.TempDir()
	writeProjectedSecret(t, dir, 1, mustMarshal(t, inv(host("host-a", "ep-a", []byte("k"), []byte("kh")))))
	path := filepath.Join(dir, hostsecret.SecretDataKey)

	fr := &fakeReconciler{ch: make(chan hostsecret.Inventory, 16)}
	w, err := NewWatcher(path, fr, quietLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	w.debounce = 20 * time.Millisecond
	w.backstop = time.Hour // disable the poll: only the EVENT path can reconcile
	w.Start()
	defer func() { _ = w.Close() }()

	// Atomic Secret update: the inventory now has two hosts.
	writeProjectedSecret(t, dir, 2, mustMarshal(t, inv(
		host("host-a", "ep-a", []byte("k"), []byte("kh")),
		host("host-b", "ep-b", []byte("k"), []byte("kh")),
	)))

	deadline := time.After(3 * time.Second)
	for {
		select {
		case got := <-fr.ch:
			if len(got.Hosts) == 2 {
				return // the directory-watch caught the atomic swap
			}
		case <-deadline:
			t.Fatalf("watcher did not reconcile to 2 hosts after the atomic ..data swap within 3s (reconciles=%d)", fr.count())
		}
	}
}

// TestWatcher_Close_StopsLoopCleanly verifies Start+Close terminates the watch
// goroutine (Close returns, and is idempotent) — no fire-and-forget goroutine.
func TestWatcher_Close_StopsLoopCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, hostsecret.SecretDataKey)
	if err := os.WriteFile(path, mustMarshal(t, inv()), 0o600); err != nil {
		t.Fatal(err)
	}
	fr := &fakeReconciler{}
	w, err := NewWatcher(path, fr, quietLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	w.Start()

	done := make(chan error, 1)
	go func() { done <- w.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return within 3s (watch goroutine leaked)")
	}

	// Idempotent second Close must not panic (fsnotify is already closed).
	_ = w.Close()
}

// TestNewWatcher_MissingDirErrors verifies a watch on a nonexistent directory
// fails (the provider treats this as non-fatal and serves the initial registry).
func TestNewWatcher_MissingDirErrors(t *testing.T) {
	fr := &fakeReconciler{}
	_, err := NewWatcher(filepath.Join(t.TempDir(), "no-such-dir", hostsecret.SecretDataKey), fr, quietLogger())
	if err == nil {
		t.Fatal("NewWatcher on a missing directory succeeded, want error")
	}
}

// TestNewWatcher_NilReconcilerRejected guards the constructor contract.
func TestNewWatcher_NilReconcilerRejected(t *testing.T) {
	if _, err := NewWatcher("/tmp/whatever/hosts.json", nil, quietLogger()); err == nil {
		t.Fatal("NewWatcher with nil Reconciler succeeded, want error")
	}
}
