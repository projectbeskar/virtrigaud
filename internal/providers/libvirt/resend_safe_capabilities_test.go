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

package libvirt

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestCapabilities_ResendSafeBackends pins where a re-sent ExportDisk /
// ImportDisk is declared safe (rc2 R6): single-host exports on s3 only (nfs
// and pvc write the destination in place and a cancelled call can still be
// writing it), every single-host import (private dir + rename onto the
// deterministic name); a clustered provider's exports on s3 and nfs (export
// lock + time budget) and no imports (not served).
func TestCapabilities_ResendSafeBackends(t *testing.T) {
	single := &Provider{virshProvider: localHostVP("single")}
	caps, err := NewServer(single).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.Equal(t, []string{migration.BackendS3}, caps.GetResendSafeExportBackends())
	assert.ElementsMatch(t, []string{migration.BackendPVC, migration.BackendNFS, migration.BackendS3},
		caps.GetResendSafeImportBackends())

	caps, err = NewServer(clusterOf(t, []string{"host-a"})).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{migration.BackendNFS, migration.BackendS3}, caps.GetResendSafeExportBackends())
	assert.ElementsMatch(t, []string{migration.BackendNFS, migration.BackendS3}, caps.GetResendSafeImportBackends(),
		"every host command of the routed import is under timeout(1) within the budget (ADR-0007 Addendum B, B3)")
}
