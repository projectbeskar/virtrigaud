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

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRecordImagePrepareLegacyRequest verifies the ADR-0009 D7 deprecation
// counter is served from controller-runtime's Registry (the provider's
// /metrics) under its documented name and increments per provider_type.
func TestRecordImagePrepareLegacyRequest(t *testing.T) {
	const name = "virtrigaud_provider_image_prepare_legacy_requests_total"
	libvirt := map[string]string{"provider_type": "libvirt"}
	vsphere := map[string]string{"provider_type": "vsphere"}
	beforeLibvirt := counterValue(t, name, libvirt)
	beforeVSphere := counterValue(t, name, vsphere)

	RecordImagePrepareLegacyRequest("libvirt")
	RecordImagePrepareLegacyRequest("libvirt")

	assert.Equal(t, beforeLibvirt+2, counterValue(t, name, libvirt))
	assert.Equal(t, beforeVSphere, counterValue(t, name, vsphere), "other provider types are not counted")
}

// TestRecordImagePrepareArtifactOutcome verifies the ADR-0009 D11 manager
// counter is served under its documented name and labels.
func TestRecordImagePrepareArtifactOutcome(t *testing.T) {
	const name = "virtrigaud_image_prepare_artifact_total"
	conflict := map[string]string{"provider_type": "vsphere", "outcome": ImageArtifactOutcomeConflict}
	reused := map[string]string{"provider_type": "vsphere", "outcome": ImageArtifactOutcomeReused}
	beforeConflict := counterValue(t, name, conflict)
	beforeReused := counterValue(t, name, reused)

	RecordImagePrepareArtifactOutcome("vsphere", ImageArtifactOutcomeConflict)

	assert.Equal(t, beforeConflict+1, counterValue(t, name, conflict))
	assert.Equal(t, beforeReused, counterValue(t, name, reused), "other outcomes are not counted")
}
