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

package metrics

import "github.com/prometheus/client_golang/prometheus"

// providerHostInventorySecondsSinceApply is the age of the host inventory a
// clustered Provider routes with: the time since it last applied a valid
// inventory. Emitted by a Provider that follows its inventory through the
// Kubernetes API; the only label is the provider type.
var providerHostInventorySecondsSinceApply = registerer.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "virtrigaud_provider_host_inventory_seconds_since_apply",
		Help: "Seconds since the Provider last applied a valid host inventory, by provider type.",
	},
	[]string{"provider_type"},
)

// providerHostInventoryStale is 1 while the Provider's host inventory is stale (no
// valid inventory applied for several reload intervals), else 0.
var providerHostInventoryStale = registerer.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "virtrigaud_provider_host_inventory_stale",
		Help: "1 while the Provider has applied no valid host inventory for several reload intervals, else 0, by provider type.",
	},
	[]string{"provider_type"},
)

// providerHostInventoryFailedClosed is 1 while the Provider refuses every host
// because its host inventory is older than its fail-closed bound, else 0.
var providerHostInventoryFailedClosed = registerer.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "virtrigaud_provider_host_inventory_failed_closed",
		Help: "1 while the Provider refuses every host because no valid host inventory was applied within its fail-closed bound, else 0, by provider type.",
	},
	[]string{"provider_type"},
)

// SetProviderHostInventoryState records the age of the Provider's host inventory and
// whether it is stale and failed closed.
func SetProviderHostInventoryState(providerType string, secondsSinceApply float64, stale, failedClosed bool) {
	providerHostInventorySecondsSinceApply.WithLabelValues(providerType).Set(secondsSinceApply)
	providerHostInventoryStale.WithLabelValues(providerType).Set(providerInventoryGaugeValue(stale))
	providerHostInventoryFailedClosed.WithLabelValues(providerType).Set(providerInventoryGaugeValue(failedClosed))
}

// providerInventoryGaugeValue is 1 for true and 0 for false.
func providerInventoryGaugeValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
