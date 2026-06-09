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

package libvirt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// domblklistStdout is a representative `virsh domblklist <dom>` output: a
// primary virtio disk plus a cloud-init CD-ROM that must be skipped.
const domblklistStdout = ` Target   Source
---------------------------------------------------
 vda      /var/lib/libvirt/images/vm-foo.qcow2
 hda      /var/lib/libvirt/images/vm-foo-cidata.iso
`

// TestParseDomblklistPrimaryTarget_SkipsCloudInit verifies the primary disk
// target is resolved from the live domain topology and the cloud-init ISO row
// is skipped — naming-convention agnostic, not the "<vmid>-disk" guess (#201,
// #207).
func TestParseDomblklistPrimaryTarget_SkipsCloudInit(t *testing.T) {
	target, err := parseDomblklistPrimaryTarget(domblklistStdout)
	require.NoError(t, err)
	assert.Equal(t, "vda", target)
}

// TestParseDomblklistPrimaryTarget_SkipsEmptyCdrom verifies an empty CD-ROM row
// (source "-") is skipped and the first real disk wins.
func TestParseDomblklistPrimaryTarget_SkipsEmptyCdrom(t *testing.T) {
	out := ` Target   Source
------------------------------------------------
 sda      -
 vdb      /var/lib/libvirt/images/vm-bar.qcow2
`
	target, err := parseDomblklistPrimaryTarget(out)
	require.NoError(t, err)
	assert.Equal(t, "vdb", target)
}

// TestParseDomblklistPrimaryTarget_NoDisk verifies an error when no usable disk
// row exists (only a cloud-init ISO).
func TestParseDomblklistPrimaryTarget_NoDisk(t *testing.T) {
	out := ` Target   Source
------------------------------------------------
 hda      /var/lib/libvirt/images/vm-foo-cidata.iso
`
	_, err := parseDomblklistPrimaryTarget(out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no primary disk target")
}

// TestParseDomblkinfoCapacity_Bytes verifies the virtual Capacity (bytes) is
// parsed for the grow-only guard, ignoring Allocation/Physical.
func TestParseDomblkinfoCapacity_Bytes(t *testing.T) {
	out := `Capacity:       10737418240
Allocation:     2147483648
Physical:       2147483648
`
	capBytes, err := parseDomblkinfoCapacity(out)
	require.NoError(t, err)
	assert.Equal(t, int64(10737418240), capBytes) // 10 GiB
}

// TestParseDomblkinfoCapacity_Missing verifies an error when no Capacity line
// is present.
func TestParseDomblkinfoCapacity_Missing(t *testing.T) {
	_, err := parseDomblkinfoCapacity("Allocation: 0\nPhysical: 0\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Capacity line")
}

// TestBlockresizeSizeArg verifies the size argument uses the explicit "G"
// suffix virsh blockresize requires (unsuffixed would be interpreted as KiB).
func TestBlockresizeSizeArg(t *testing.T) {
	assert.Equal(t, "20G", blockresizeSizeArg(20))
	assert.Equal(t, "1G", blockresizeSizeArg(1))
}

// TestShouldGrowDisk_GrowOnlyGuard verifies the grow-only + idempotency guard:
// grow when strictly larger past the rounding threshold; skip when equal,
// smaller (would-be shrink), or within rounding.
func TestShouldGrowDisk_GrowOnlyGuard(t *testing.T) {
	tenGiB := int64(10) * bytesPerGiB
	twentyGiB := int64(20) * bytesPerGiB

	tests := []struct {
		name    string
		current int64
		desired int64
		want    bool
	}{
		{"grow 10->20 GiB", tenGiB, twentyGiB, true},
		{"equal is no-op (idempotent)", tenGiB, tenGiB, false},
		{"shrink 20->10 GiB is rejected", twentyGiB, tenGiB, false},
		{"sub-threshold delta is no-op", tenGiB, tenGiB + (512 * 1024), false},
		{"just past threshold grows", tenGiB, tenGiB + bytesPerGiB, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shouldGrowDisk(tc.current, tc.desired))
		})
	}
}

// TestFsGrowCommands verifies the best-effort in-guest FS-grow sequence:
// growpart on the partition, then resize2fs (ext) and xfs_growfs (xfs), built
// from the resolved target device.
func TestFsGrowCommands(t *testing.T) {
	cmds := fsGrowCommands("vda")
	require.Len(t, cmds, 3)
	assert.Equal(t, "growpart /dev/vda 1", cmds[0])
	assert.Equal(t, "resize2fs /dev/vda1", cmds[1])
	assert.Equal(t, "xfs_growfs /", cmds[2])
}

// TestBytesPerGiB sanity-checks the GiB constant used for byte-accurate
// comparisons against domblkinfo Capacity.
func TestBytesPerGiB(t *testing.T) {
	assert.Equal(t, int64(1073741824), bytesPerGiB)
}
