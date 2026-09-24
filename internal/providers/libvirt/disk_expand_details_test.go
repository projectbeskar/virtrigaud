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

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin how a clustered Reconfigure finds the disk it resizes
// (ADR-0007 Addendum A, slice 2): the owner-checked domain's own primary disk
// from `virsh domblklist --details`, never a volume found by name.

func TestParseDomblklistDetailsPrimary(t *testing.T) {
	const header = " Type   Device   Target   Source\n------------------------------------------------\n"
	cases := []struct {
		name    string
		out     string
		want    primaryDisk
		wantErr bool
	}{
		{
			name: "file disk after skipping cdroms and cloud-init",
			out: header +
				" file   cdrom    hda      -\n" +
				" file   disk     vdb      /var/lib/libvirt/images/web-cidata.iso\n" +
				" file   disk     vda      /var/lib/libvirt/images/web-disk.qcow2\n",
			want: primaryDisk{sourceType: diskSourceTypeFile, target: "vda", path: "/var/lib/libvirt/images/web-disk.qcow2"},
		},
		{
			name: "block device",
			out:  header + " block  disk     vda      /dev/vg0/web\n",
			want: primaryDisk{sourceType: diskSourceTypeBlock, target: "vda", path: "/dev/vg0/web"},
		},
		{
			name: "a path with a space",
			out:  header + " file   disk     vda      /var/lib/libvirt/my images/web.qcow2\n",
			want: primaryDisk{sourceType: diskSourceTypeFile, target: "vda", path: "/var/lib/libvirt/my images/web.qcow2"},
		},
		{name: "network disk has no host path", out: header + " network disk     vda      pool/web\n", wantErr: true},
		{name: "relative source is refused", out: header + " file   disk     vda      images/web.qcow2\n", wantErr: true},
		{name: "option-looking source is refused", out: header + " file   disk     vda      --pool\n", wantErr: true},
		{name: "no disk at all", out: header + " file   cdrom    hda      /iso/x.iso\n", wantErr: true},
		{name: "empty output", out: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDomblklistDetailsPrimary(tc.out)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestClustered_Reconfigure_OnlineGrowOfANetworkDiskFails: the online grow of
// a disk with no host path fails the disk step (as a failed blockresize does)
// instead of resizing anything found by name.
func TestClustered_Reconfigure_OnlineGrowOfANetworkDiskFails(t *testing.T) {
	fx, p := clusteredOpsFixture(t)
	fx.script("host-b", "state", "running\n")
	fx.script("host-b", "disktype", "network")
	fx.script("host-b", "disksource", "pool/web")

	_, err := p.Reconfigure(context.Background(), webOnHostB, reconfigureTo(0, 0, 20))
	require.Error(t, err)
	assert.True(t, contracts.IsRetryable(err), "%v", err)
	for _, c := range fx.calls() {
		assert.NotContains(t, c, " vol-resize ", "nothing is resized")
		assert.NotContains(t, c, " blockresize ", "nothing is resized")
	}
}

func TestResizeVolumeByPath_RefusesARelativePath(t *testing.T) {
	sp := NewStorageProvider(newUnroutableVirshProvider())
	require.Error(t, sp.ResizeVolumeByPath(context.Background(), "web-disk", 20))
	assert.Zero(t, sp.virshProvider.unroutableHits.Load(), "refused before any command runs")
}
