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

package proxmox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestProxmoxProvider_ReconfigureResizesDisk covers the #266 disk-key fix: the
// marshaled contracts.CreateRequest nests disks under "Disks" with a numeric
// "SizeGiB", NOT "disks"/"size". The old wrong keys never matched, so the
// disk-resize branch was unreachable dead code (the same class of bug as the
// CPU/memory key bug). The fake seeds scsi0 at size=32G.
func TestProxmoxProvider_ReconfigureResizesDisk(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	createResp, err := provider.Create(ctx, &providerv1.CreateRequest{
		Name:      "disk-resize",
		ClassJson: `{"CPU": 1, "MemoryMiB": 1024}`,
	})
	require.NoError(t, err)
	if createResp.Task != nil {
		require.NoError(t, waitForTask(ctx, provider, createResp.Task.Id))
	}

	// Grow 32G → 64G: must reach the resize path and return a task. The wrong-key
	// code returned no task at all (the branch was never entered).
	resp, err := provider.Reconfigure(ctx, &providerv1.ReconfigureRequest{
		Id:          createResp.Id,
		DesiredJson: `{"Disks":[{"SizeGiB":64,"Type":"","Name":"root"}]}`,
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Task, "a disk grow must trigger a resize task")

	// Shrink 32G → 8G: the resize path is reached and the shrink guard rejects it.
	// With the old wrong keys this would silently succeed (a no-op), masking the bug.
	_, err = provider.Reconfigure(ctx, &providerv1.ReconfigureRequest{
		Id:          createResp.Id,
		DesiredJson: `{"Disks":[{"SizeGiB":8,"Type":"","Name":"root"}]}`,
	})
	assert.Error(t, err, "a disk shrink must be rejected, proving the resize branch is reached")
}
