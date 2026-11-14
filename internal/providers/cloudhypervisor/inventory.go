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
	"fmt"

	"github.com/projectbeskar/virtrigaud/internal/clustered/inventoryfile"
)

// NewFromFile returns a Provider that fronts the hosts of the inventory file
// at path and follows the file as it changes (inventoryfile.Watcher): a
// reload that removes a host, or changes its endpoint or TLS material,
// closes its connection. A file that cannot be read or parsed at start is an
// error; one that turns unreadable later keeps the last good host set. When
// the watch cannot be set up the Provider serves the hosts it loaded, and
// says so in its log.
func NewFromFile(path string, opts Options) (*Provider, error) {
	inv, err := inventoryfile.LoadInventory(path)
	if err != nil {
		return nil, fmt.Errorf("cloudhypervisor: %w", err)
	}
	p, err := New(opts, inv)
	if err != nil {
		return nil, err
	}
	w, err := inventoryfile.NewWatcher(path, p, p.log)
	if err != nil {
		p.log.Warn("host inventory: the file is not watched; changes need a restart", "path", path, "error", err.Error())
		return p, nil
	}
	w.Start()
	p.watcher = w
	return p, nil
}
