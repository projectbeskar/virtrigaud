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
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/importlimit"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// stagingNode plays the PVE node over SSH for an NFS import: the mount +
// convert script and `qemu-img check` succeed, `qemu-img info` reports the
// staged disk's virtual size (or fails), and every command is recorded.
type stagingNode struct {
	mu          sync.Mutex
	virtualSize int64
	infoErr     error
	cmds        []string
}

func (n *stagingNode) run(_ context.Context, cmd string) (string, string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cmds = append(n.cmds, cmd)
	if strings.HasPrefix(cmd, "qemu-img info ") {
		if n.infoErr != nil {
			return "", "boom", n.infoErr
		}
		return fmt.Sprintf(`{"virtual-size": %d, "format": "qcow2"}`, n.virtualSize), "", nil
	}
	return "", "", nil
}

func (n *stagingNode) ran(prefix string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, c := range n.cmds {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// TestImportDiskFromNFS_DiskSizeLimit drives the Proxmox NFS import end to end
// against a fake node: an oversized staged disk (as the convert produced it)
// is refused with FailedPrecondition / DISK_SIZE_EXCEEDS_LIMIT and its stage
// removed; an unreadable size fails closed the same way; one within the limit
// is staged; with no limit no size is read.
func TestImportDiskFromNFS_DiskSizeLimit(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	const limit = int64(100) << 30
	stage := proxmoxImportStagePath("web-migrated", "qcow2")
	req := func(max int64) *providerv1.ImportDiskRequest {
		return &providerv1.ImportDiskRequest{
			SourceUrl:           "nfs://nfs.example.invalid/export/mig/disk.qcow2",
			BackendType:         migration.BackendNFS,
			TargetName:          "web-migrated",
			StorageOptionsJson:  `{"server":"nfs.example.invalid","export":"/export"}`,
			MaxVirtualSizeBytes: max,
		}
	}

	for _, tc := range []struct {
		name    string
		node    *stagingNode
		max     int64
		message string // "" = staged
	}{
		{name: "oversized", node: &stagingNode{virtualSize: 2 << 40}, max: limit, message: importlimit.ExceedsMessage},
		{name: "size unreadable", node: &stagingNode{infoErr: fmt.Errorf("ssh: broken pipe")}, max: limit, message: importlimit.UnverifiableMessage},
		{name: "within the limit", node: &stagingNode{virtualSize: limit}, max: limit},
		{name: "no limit", node: &stagingNode{virtualSize: 2 << 40}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := createTestProvider(endpoint)
			p.ssh = &sshTransport{host: "pve", logger: slog.Default(), runOverride: tc.node.run}
			resp, err := p.ImportDisk(context.Background(), req(tc.max))
			if tc.message == "" {
				require.NoError(t, err)
				assert.Equal(t, stage, resp.Path)
				assert.Empty(t, tc.node.ran("rm -f "), "the stage is kept for qm importdisk")
				if tc.max == 0 {
					assert.Empty(t, tc.node.ran("qemu-img info "), "no limit: no size is read")
				}
				return
			}
			require.Error(t, err)
			assert.Nil(t, resp)
			st, ok := status.FromError(err)
			require.True(t, ok)
			assert.Equal(t, codes.FailedPrecondition, st.Code())
			assert.Equal(t, tc.message, st.Message())
			info := tc.node.ran("qemu-img info ")
			require.Len(t, info, 1)
			assert.Contains(t, info[0], "-f 'qcow2'", "the stage is read as qcow2: nothing is probed")
			assert.Equal(t, []string{"rm -f " + proxmoxShellQuote(stage)}, tc.node.ran("rm -f "), "the refused stage is removed")
		})
	}
}

// TestGetCapabilities_AdvertisesImportSizeLimit: the production Proxmox
// capability set reports that ImportDisk enforces max_virtual_size_bytes.
func TestGetCapabilities_AdvertisesImportSizeLimit(t *testing.T) {
	p := &Provider{capabilities: GetProviderCapabilities()}
	caps, err := p.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsImportSizeLimit())
}
