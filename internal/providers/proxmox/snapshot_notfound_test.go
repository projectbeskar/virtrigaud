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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestSnapshotDelete_MissingSnapshotIsNotFound pins, against pvefake, that
// deleting a snapshot that is not there is answered gRPC NotFound — the
// manager's idempotent "already gone". PVE deletes a snapshot in a worker task
// that fails when there is nothing to delete, which the manager would retry
// forever (a VMMigration or VMSnapshot finalizer held for good).
func TestSnapshotDelete_MissingSnapshotIsNotFound(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	t.Run("never taken", func(t *testing.T) {
		_, err := provider.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "100", SnapshotId: "mig-404"})
		assert.Equal(t, codes.NotFound, status.Code(err), "%v", err)
	})

	t.Run("the running-state pseudo-entry is not a snapshot", func(t *testing.T) {
		_, err := provider.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "100", SnapshotId: "current"})
		assert.Equal(t, codes.NotFound, status.Code(err), "%v", err)
	})

	t.Run("deleted, then deleted again", func(t *testing.T) {
		created, err := provider.SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{VmId: "100", NameHint: "mig-snap"})
		require.NoError(t, err)
		if created.Task != nil {
			require.NoError(t, waitForTask(ctx, provider, created.Task.Id))
		}
		resp, err := provider.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "100", SnapshotId: created.SnapshotId})
		require.NoError(t, err)
		if resp.Task != nil {
			require.NoError(t, waitForTask(ctx, provider, resp.Task.Id))
		}
		_, err = provider.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "100", SnapshotId: created.SnapshotId})
		assert.Equal(t, codes.NotFound, status.Code(err), "a repeated delete is the manager's idempotent success: %v", err)
	})

	t.Run("VM gone", func(t *testing.T) {
		_, err := provider.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "999", SnapshotId: "mig-snap"})
		assert.Equal(t, codes.NotFound, status.Code(err), "%v", err)
	})
}
