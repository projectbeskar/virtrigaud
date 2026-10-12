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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin that PVE's error messages are recognised whether PVE puts
// them in the HTTP status line's reason phrase (real PVE: body {"data":null})
// or in the body, and that a message that matches nothing stays a retryable
// error, never NotFound.

func TestPVEErrorText_BothForms(t *testing.T) {
	for name, inStatusLine := range map[string]bool{"message in the body": false, "message in the status line": true} {
		t.Run(name, func(t *testing.T) {
			fake, endpoint, err := pvefake.StartFakeServer()
			require.NoError(t, err)
			fake.ErrorsInStatusLine(inStatusLine)
			provider := createTestProvider(endpoint)
			ctx := context.Background()

			// A snapshot PVE says does not exist is typed.
			_, err = provider.client.DeleteSnapshot(ctx, "pve", 100, "mig-404")
			assert.True(t, errors.Is(err, pveapi.ErrSnapshotNotExist), "%v", err)

			// A VM on another node is answered "Configuration file ... does
			// not exist": that is no snapshot answer, and stays an error.
			fake.MoveVM(100, "pve2")
			_, err = provider.client.DeleteSnapshot(ctx, "pve", 100, "mig-404")
			require.Error(t, err)
			assert.False(t, errors.Is(err, pveapi.ErrSnapshotNotExist), "%v", err)
			fake.MoveVM(100, "pve")

			// The VMID probe reads "VM <id> already exists" from either
			// form: a VM hidden from the token is never taken for absent.
			id := takeSnapshot(t, provider, "mig-acl")
			fake.HideFromResources(100)
			_, err = provider.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "100", SnapshotId: id})
			assert.Equal(t, codes.Unavailable, status.Code(err), "%v", err)

			// A free VMID is still absent.
			_, err = provider.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "999", SnapshotId: id})
			assert.Equal(t, codes.NotFound, status.Code(err), "%v", err)
		})
	}
}
