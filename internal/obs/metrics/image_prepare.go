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

// imagePrepareLegacyRequestsTotal is the ADR-0009 D7 deprecation signal,
// emitted by PROVIDER processes: one increment per ImagePrepare request that
// carries no image identity — a request from a manager older than ADR-0009,
// served in deprecated legacy (bare-name) mode for one release and refused
// from the next. Operators alert when it is non-zero: the bare-name reuse it
// allows is the cross-tenant artifact defect ADR-0009 closes. It is removed
// together with legacy mode (ADR-0009 Slice 10).
var imagePrepareLegacyRequestsTotal = registerer.NewCounterVec(
	prometheus.CounterOpts{
		Name: "virtrigaud_provider_image_prepare_legacy_requests_total",
		Help: "Total ImagePrepare requests without an image identity (from a manager older than ADR-0009), served in deprecated legacy mode or refused, by provider type. Non-zero means a manager must be upgraded.",
	},
	[]string{"provider_type"},
)

// RecordImagePrepareLegacyRequest counts one legacy (identity-less)
// ImagePrepare request received by a provider of providerType.
func RecordImagePrepareLegacyRequest(providerType string) {
	imagePrepareLegacyRequestsTotal.WithLabelValues(providerType).Inc()
}
