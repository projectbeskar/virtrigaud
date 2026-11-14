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
	"fmt"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/clustered/inventoryfile"
)

// newInventoryWatcher is the watcher constructor; tests replace it.
var newInventoryWatcher = inventoryfile.NewWatcher

// NewFromFile returns a Provider that fronts the hosts of the inventory file
// at path and follows the file as it changes: a reload that removes a host,
// or changes its endpoint or TLS material, closes its connection. The file is
// re-read every Options.InventoryReloadInterval on its own, and also watched
// (inventoryfile.Watcher) for a faster reload. A file that cannot be read or
// parsed at start is an error; one that turns unreadable later keeps the last
// good host set. When the watch cannot be set up, the Provider says so in
// its log at error level and in its Validate answer, and still follows the
// file through the periodic re-read.
func NewFromFile(path string, opts Options) (*Provider, error) {
	inv, err := inventoryfile.LoadInventory(path)
	if err != nil {
		return nil, fmt.Errorf("cloudhypervisor: %w", err)
	}
	p, err := New(opts, inv)
	if err != nil {
		return nil, err
	}
	w, err := newInventoryWatcher(path, p, p.log)
	if err != nil {
		p.unwatched = true
		p.log.Error("host inventory: the file is not watched; changes are picked up by the periodic re-read only",
			"path", path, "interval", p.opts.InventoryReloadInterval.String(), "error", err.Error())
	} else {
		w.Start()
		p.watcher = w
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.stopReload, p.reloadDone = cancel, make(chan struct{})
	go p.reloadEvery(ctx, path)
	return p, nil
}

// reloadEvery re-reads the inventory file every InventoryReloadInterval until
// ctx ends. A file that cannot be read or parsed keeps the last good host
// set.
func (p *Provider) reloadEvery(ctx context.Context, path string) {
	defer close(p.reloadDone)
	t := time.NewTicker(p.opts.InventoryReloadInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		inv, err := inventoryfile.LoadInventory(path)
		if err != nil {
			p.log.Warn("host inventory: re-read skipped; keeping the last good host set", "path", path, "error", err.Error())
			continue
		}
		if err := p.Reconcile(inv); err != nil {
			p.log.Warn("host inventory: reconcile failed", "path", path, "error", err.Error())
		}
	}
}
