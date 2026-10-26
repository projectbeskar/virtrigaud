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

package proxmox

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/capabilities"
)

// TestCapabilitySummary pins rc1 finding B3: the startup log's capability
// list is what the provider advertises. Linked clones are not in it; what
// GetCapabilities reports is.
func TestCapabilitySummary(t *testing.T) {
	names, diskTypes, networkTypes := CapabilitySummary()
	assert.NotContains(t, names, string(capabilities.CapabilityLinkedClones))
	assert.NotContains(t, names, "linked-clones")

	resp, err := GetProviderCapabilities().GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	reported := map[capabilities.Capability]bool{
		capabilities.CapabilitySnapshots:             resp.GetSupportsSnapshots(),
		capabilities.CapabilityMemorySnapshots:       resp.GetSupportsMemorySnapshots(),
		capabilities.CapabilityLinkedClones:          resp.GetSupportsLinkedClones(),
		capabilities.CapabilityImageImport:           resp.GetSupportsImageImport(),
		capabilities.CapabilityReconfigureOnline:     resp.GetSupportsReconfigureOnline(),
		capabilities.CapabilityDiskExpansionOnline:   resp.GetSupportsDiskExpansionOnline(),
		capabilities.CapabilityDiskExport:            resp.GetSupportsDiskExport(),
		capabilities.CapabilityDiskImport:            resp.GetSupportsDiskImport(),
		capabilities.CapabilityOwnerRebind:           resp.GetSupportsOwnerRebind(),
		capabilities.CapabilityClustering:            resp.GetSupportsClustering(),
		capabilities.CapabilityImageArtifactIdentity: resp.GetSupportsImageArtifactIdentity(),
	}
	for c, want := range reported {
		assert.Equal(t, want, slices.Contains(names, string(c)), "capability %s", c)
	}
	assert.Equal(t, resp.GetSupportedDiskTypes(), diskTypes)
	assert.Equal(t, resp.GetSupportedNetworkTypes(), networkTypes)
}
