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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
)

const (
	// defaultReloadDebounce coalesces a burst of filesystem events (a Secret
	// update fires several) into a single reconcile.
	defaultReloadDebounce = 200 * time.Millisecond

	// defaultReloadBackstop is a low-frequency unconditional re-read: a safety
	// net in case a filesystem event is ever missed (e.g. an inotify overflow
	// under churn). Reloads are idempotent, so a redundant re-read is cheap.
	defaultReloadBackstop = 30 * time.Second
)

// Reconciler is the subset of ClusterRegistry the Watcher drives: it hands a
// freshly-parsed inventory to Reconcile. Narrowing to this interface keeps the
// watcher unit-testable with a fake registry (no dialer, no live host).
type Reconciler interface {
	// Reconcile drives the live host set to the given inventory.
	Reconcile(hostsecret.Inventory) error
}

// Watcher watches the mounted host-inventory file and reconciles a Reconciler
// (a ClusterRegistry) whenever the file changes (ADR-0007 D3 hot-reload).
//
// # Why it watches the DIRECTORY, not the file
//
// A Kubernetes Secret/ConfigMap mount is updated ATOMICALLY: the kubelet writes
// the new content into a fresh timestamped directory, points a `..data` symlink
// at it with a rename, and only then flips the visible file symlinks. The
// visible file's inode is therefore SWAPPED on every update, so an fsnotify
// watch registered on the file inode goes deaf after the first change — inotify
// follows the old, now-orphaned inode. Watching the parent DIRECTORY instead
// catches the `..data` rename (a Create/Rename event in the dir) and re-reads
// the canonical path, which now resolves through the new symlinks. This is the
// same mechanism controller-runtime's certwatcher and Prometheus's config
// reloader use. A low-frequency backstop re-read covers any missed event.
type Watcher struct {
	// path is the canonical inventory file (e.g. /etc/virtrigaud/hosts/hosts.json).
	path string
	// dir is filepath.Dir(path): the directory actually watched.
	dir string
	// reg is reconciled on every successful reload.
	reg Reconciler
	// logger records coarse reload events (path + coarse reason; never secrets).
	logger *slog.Logger

	// debounce/backstop are the loop timings; overridable in tests before Start.
	debounce time.Duration
	backstop time.Duration

	fsw     *fsnotify.Watcher
	stopCh  chan struct{}
	stopOne sync.Once
	wg      sync.WaitGroup
}

// NewWatcher builds a Watcher for the inventory file at path, reconciling reg on
// change. It registers an fsnotify watch on the file's parent directory (see the
// type doc for why the directory and not the file). The watch is not active
// until Start is called. logger may be nil (defaults to slog.Default()).
//
// A non-nil error means the watch could not be established (e.g. the directory
// does not exist); the caller should log and continue serving with the initial,
// non-hot-reloaded registry rather than fail — hot-reload is an enhancement, not
// a prerequisite for serving the hosts already loaded at startup.
func NewWatcher(path string, reg Reconciler, logger *slog.Logger) (*Watcher, error) {
	if reg == nil {
		return nil, errors.New("hostconn: NewWatcher requires a non-nil Reconciler")
	}
	if logger == nil {
		logger = slog.Default()
	}
	dir := filepath.Dir(path)
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create inventory file watcher: %w", err)
	}
	if err := fsw.Add(dir); err != nil {
		_ = fsw.Close()
		return nil, fmt.Errorf("watch inventory directory %s: %w", dir, err)
	}
	return &Watcher{
		path:     path,
		dir:      dir,
		reg:      reg,
		logger:   logger,
		debounce: defaultReloadDebounce,
		backstop: defaultReloadBackstop,
		fsw:      fsw,
		stopCh:   make(chan struct{}),
	}, nil
}

// Start launches the watch loop in a background goroutine. The goroutine has a
// clean shutdown story: it exits when Close is called (stopCh) and Close waits
// for it (wg) — there is no fire-and-forget goroutine.
func (w *Watcher) Start() {
	w.wg.Add(1)
	go w.loop()
}

// Close stops the watch loop, waits for it to drain (including any in-flight
// reconcile), and releases the fsnotify watch. It is safe to call more than
// once. Callers stop the Watcher BEFORE closing the registry it reconciles, so
// no reconcile can race the registry's own Close.
func (w *Watcher) Close() error {
	w.stopOne.Do(func() { close(w.stopCh) })
	w.wg.Wait()
	return w.fsw.Close()
}

// loop is the watch goroutine: it debounces filesystem events into reconciles,
// re-reads on a backstop tick, and returns on stop.
func (w *Watcher) loop() {
	defer w.wg.Done()

	ticker := time.NewTicker(w.backstop)
	defer ticker.Stop()

	var debounceC <-chan time.Time
	for {
		select {
		case <-w.stopCh:
			return
		case _, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			// Any event in the mount directory — most importantly the atomic
			// `..data` symlink swap — (re)arms the debounce. We deliberately do
			// not filter by name: the canonical file is re-read regardless of
			// which entry changed, so we cannot miss the swap by matching the
			// wrong event.
			debounceC = time.After(w.debounce)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.logger.Warn("hostconn: inventory watch error", "path", w.path, "error", err.Error())
		case <-debounceC:
			debounceC = nil
			w.reloadOnce()
		case <-ticker.C:
			w.reloadOnce()
		}
	}
}

// reloadOnce re-reads and reconciles the inventory file. A read/parse/version
// error is NON-fatal and does NOT reconcile: the last-good host set is kept
// (never drain every host on a transient or malformed write), the error is
// logged coarsely, and the next valid event reconciles. It never logs
// credential material — path and coarse reason only.
func (w *Watcher) reloadOnce() {
	inv, err := LoadInventory(w.path)
	if err != nil {
		w.logger.Warn("hostconn: inventory reload skipped; keeping last-good host set",
			"path", w.path, "error", err.Error())
		return
	}
	if err := w.reg.Reconcile(inv); err != nil {
		w.logger.Warn("hostconn: inventory reconcile failed", "path", w.path, "error", err.Error())
	}
}

// LoadInventory reads and parses the host-inventory file at path, gating the
// schema version so a provider that understands version N refuses an unknown
// (newer, or malformed/unversioned) document rather than silently mis-parsing a
// changed shape (hostsecret.SchemaVersion contract). A valid document with ZERO
// hosts is NOT an error — it is a legitimate "this provider currently fronts no
// hosts" state that reconciles to an empty registry.
//
// It is the single file-consumption path shared by the provider's startup load
// and the Watcher's hot-reload, so both agree on what a valid inventory is. Its
// errors carry the path and a coarse reason, never credential bytes.
func LoadInventory(path string) (hostsecret.Inventory, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return hostsecret.Inventory{}, fmt.Errorf("read host inventory %s: %w", path, err)
	}
	inv, err := hostsecret.Unmarshal(data)
	if err != nil {
		// json errors carry offsets/type names, never decoded credential values.
		return hostsecret.Inventory{}, fmt.Errorf("parse host inventory %s: %w", path, err)
	}
	if inv.SchemaVersion < 1 || inv.SchemaVersion > hostsecret.SchemaVersion {
		return hostsecret.Inventory{}, fmt.Errorf(
			"host inventory %s has unsupported schemaVersion %d (this provider understands 1..%d)",
			path, inv.SchemaVersion, hostsecret.SchemaVersion)
	}
	return inv, nil
}
