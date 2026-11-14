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
	"log/slog"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/clustered/inventoryfile"
)

// Reconciler is inventoryfile.Reconciler: what the Watcher drives (a
// ClusterRegistry).
type Reconciler = inventoryfile.Reconciler

// Watcher is inventoryfile.Watcher: it watches the mounted host-inventory
// file and reconciles a ClusterRegistry whenever it changes.
type Watcher = inventoryfile.Watcher

// NewWatcher is inventoryfile.NewWatcher.
func NewWatcher(path string, reg Reconciler, logger *slog.Logger) (*Watcher, error) {
	return inventoryfile.NewWatcher(path, reg, logger)
}

// LoadInventory is inventoryfile.LoadInventory.
func LoadInventory(path string) (hostsecret.Inventory, error) {
	return inventoryfile.LoadInventory(path)
}
