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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestCapabilities_ResendSafeBackends pins where a re-sent ExportDisk /
// ImportDisk is declared safe (rc2 R6): s3 only, both ways. The nfs export
// copies onto the staged file in place on the node, and the nfs import's
// node script (copy, then mv -T onto the stage) outlives a cancelled call
// with no time limit or lock: neither is re-send safe.
func TestCapabilities_ResendSafeBackends(t *testing.T) {
	resp, err := GetProviderCapabilities().GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.Equal(t, []string{migration.BackendS3}, resp.GetResendSafeExportBackends())
	assert.Equal(t, []string{migration.BackendS3}, resp.GetResendSafeImportBackends())
}
