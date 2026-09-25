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

// Outcomes of virtrigaud_image_prepare_artifact_total (ADR-0009 D11): what an
// identity ImagePrepare call found or did at the artifact name the provider
// derived, as the manager observes it.
const (
	// ImageArtifactOutcomeCreated: the provider started or completed a new
	// import (the stamp echo says the artifact was not reused).
	ImageArtifactOutcomeCreated = "created"
	// ImageArtifactOutcomeReused: an existing artifact with a matching stamp
	// was reused.
	ImageArtifactOutcomeReused = "reused"
	// ImageArtifactOutcomeInProgress: the artifact is still being prepared for
	// the requesting VMImage by another request (a retryable answer).
	ImageArtifactOutcomeInProgress = "in_progress"
	// ImageArtifactOutcomeConflict: an artifact at the derived name was not
	// prepared for the requesting VMImage; the provider refused to use or
	// replace it.
	ImageArtifactOutcomeConflict = "conflict"
	// ImageArtifactOutcomeAbandonedCleanup: the provider removed an abandoned
	// artifact of the requesting VMImage before importing it again. Reserved:
	// the ImagePrepare response does not report it, so the manager never
	// records it (a re-import after a cleanup is counted as created).
	ImageArtifactOutcomeAbandonedCleanup = "abandoned_cleanup"
)

// imagePrepareArtifactTotal counts the outcome of the identity ImagePrepare
// calls the MANAGER issues (ADR-0009 D11), by provider type. A rising
// "conflict" count means an artifact that was not prepared for the requesting
// VMImage sits at its derived name, and an operator must act.
var imagePrepareArtifactTotal = registerer.NewCounterVec(
	prometheus.CounterOpts{
		Name: "virtrigaud_image_prepare_artifact_total",
		Help: "Total identity ImagePrepare outcomes observed by the manager (created, reused, in_progress, conflict), by provider type (ADR-0009).",
	},
	[]string{"provider_type", "outcome"},
)

// RecordImagePrepareArtifactOutcome counts one identity ImagePrepare outcome
// (an ImageArtifactOutcome* value) through a Provider of providerType.
func RecordImagePrepareArtifactOutcome(providerType, outcome string) {
	imagePrepareArtifactTotal.WithLabelValues(providerType, outcome).Inc()
}
