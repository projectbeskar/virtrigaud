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

// Host inventory readiness (development builds only): for each Host of a
// Provider whose type runs a per-host agent, one series per inventory
// condition, labelled with the condition's current reason (a closed set the
// caller enforces). The value is 1 while the condition is True, 0 otherwise.
// A reason change replaces the series, so each Host has at most one series per
// condition; the series are removed with the Host.
var (
	hostInventoryCondition = registerer.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "virtrigaud_host_inventory_condition",
			Help: "Host inventory condition (AgentInventoryVerified, ComponentsReady) of a Host of a Provider that runs a per-host agent: 1 while True, 0 otherwise, labelled by namespace, host, condition and current reason (development builds only).",
		},
		[]string{"namespace", "host", "condition", "reason"},
	)
	componentAllowList = registerer.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "virtrigaud_component_allow_list",
			Help: "The manager's component allow-list, set once at start: 1 for the current state (loaded|not_in_force|not_configured|skipped), labelled with the allow-list's SHA-256 (empty when none is loaded; development builds only).",
		},
		[]string{"state", "sha256"},
	)
)

// States of the component allow-list (the "state" label of
// virtrigaud_component_allow_list).
const (
	// AllowListLoaded: an allow-list is in force.
	AllowListLoaded = "loaded"
	// AllowListNotInForce: an allow-list is configured but could not be put in
	// force.
	AllowListNotInForce = "not_in_force"
	// AllowListNotConfigured: no allow-list is configured.
	AllowListNotConfigured = "not_configured"
	// AllowListSkipped: the development-only flag that skips the allow-list
	// is set.
	AllowListSkipped = "skipped"
)

// SetHostInventoryCondition publishes the current reason of a Host's
// inventory condition, replacing every other series of that condition, so
// each Host has exactly one series per condition whatever was published
// before.
func SetHostInventoryCondition(namespace, host, condition, reason string, isTrue bool) {
	hostInventoryCondition.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "host": host, "condition": condition})
	v := 0.0
	if isTrue {
		v = 1
	}
	hostInventoryCondition.WithLabelValues(namespace, host, condition, reason).Set(v)
}

// DeleteHostInventoryConditions removes every inventory condition series of a
// Host.
func DeleteHostInventoryConditions(namespace, host string) {
	hostInventoryCondition.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "host": host})
}

// SetComponentAllowList publishes the state of the component allow-list and
// its SHA-256 ("" when none is loaded).
func SetComponentAllowList(state, sha256 string) {
	componentAllowList.Reset()
	componentAllowList.WithLabelValues(state, sha256).Set(1)
}
