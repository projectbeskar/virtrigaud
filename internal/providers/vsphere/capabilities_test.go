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

package vsphere

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestGetCapabilities_DiskMigration verifies vSphere advertises its implemented
// disk export/import capabilities and formats accurately (issue #178). These
// were previously left at the zero value, understating real support — which
// would wrongly block vSphere migrations once capability gating is enabled (#176).
func TestGetCapabilities_DiskMigration(t *testing.T) {
	p := &Provider{}

	caps, err := p.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})

	require.NoError(t, err)
	require.NotNil(t, caps)

	// The corrected disk-migration flags (issue #178).
	assert.True(t, caps.SupportsDiskExport, "vSphere implements ExportDisk")
	assert.True(t, caps.SupportsDiskImport, "vSphere implements ImportDisk")
	assert.Equal(t, []string{"vmdk", "qcow2", "raw"}, caps.SupportedExportFormats)
	assert.Equal(t, []string{"vmdk", "qcow2", "raw"}, caps.SupportedImportFormats)
	assert.True(t, caps.SupportsExportCompression, "export uses compressed streamOptimized VMDK")

	// Sanity: existing flags remain unchanged.
	assert.True(t, caps.SupportsSnapshots)
	assert.True(t, caps.SupportsLinkedClones)
	// vSphere captures RAM-inclusive snapshots via CreateSnapshot(memory=true) when the
	// VM is powered on; SnapshotCreate already honours req.IncludeMemory (issue #200).
	assert.True(t, caps.SupportsMemorySnapshots)
}
