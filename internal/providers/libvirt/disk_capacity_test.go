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
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// qemuImgSizes answers `qemu-img info` for each path it knows with its
// virtual size, and fails for any other path.
type qemuImgSizes map[string]int64

func (q qemuImgSizes) runVirshCommand(_ context.Context, args ...string) (*VirshResult, error) {
	if len(args) < 3 || args[0] != "!" || args[1] != "qemu-img" || args[2] != "info" {
		return nil, fmt.Errorf("unexpected command %q", strings.Join(args, " "))
	}
	path := args[len(args)-1]
	size, ok := q[path]
	if !ok {
		return nil, fmt.Errorf("no such file %s", path)
	}
	return &VirshResult{Stdout: fmt.Sprintf(`{"virtual-size": %d, "actual-size": 1}`, size)}, nil
}

// TestDomainDiskCapacities: every disk of the domain is listed with its
// virtual size; CD-ROM media and the cloud-init seed are not disks; a disk
// whose size cannot be read (not file-backed, or qemu-img fails) is listed
// with size 0, never dropped.
func TestDomainDiskCapacities(t *testing.T) {
	doc, err := parseDomainDisks(`<domain><name>team-a.web</name><devices>
  <disk type='file' device='disk'><driver type='vmdk'/><source file='/pool/team-a.web.vmdk'/></disk>
  <disk type='file'><driver type='vmdk'/><source file='/pool/data.vmdk'/></disk>
  <disk type='file' device='cdrom'><driver type='raw'/><source file='/iso/installer.iso'/></disk>
  <disk type='file' device='disk'><driver type='raw'/><source file='/seed/team-a.web-cidata.iso'/></disk>
  <disk type='block' device='disk'><driver type='raw'/><source dev='/dev/vg/lv'/></disk>
  <disk type='file' device='disk'><driver type='vmdk'/><source file='/pool/unreadable.vmdk'/></disk>
</devices></domain>`)
	require.NoError(t, err)

	got := domainDiskCapacities(context.Background(), qemuImgSizes{
		"/pool/team-a.web.vmdk": 40 << 30,
		"/pool/data.vmdk":       800 << 30,
	}, doc)
	assert.Equal(t, []contracts.DiskCapacity{
		{ID: "/pool/team-a.web.vmdk", Path: "/pool/team-a.web.vmdk", VirtualSizeBytes: 40 << 30},
		{ID: "/pool/data.vmdk", Path: "/pool/data.vmdk", VirtualSizeBytes: 800 << 30},
		{VirtualSizeBytes: 0}, // the block disk: not read, reported as unknown
		{ID: "/pool/unreadable.vmdk", Path: "/pool/unreadable.vmdk", VirtualSizeBytes: 0},
	}, got)
}

// TestReconfigure_SingleHost_WithheldDiskSizeIsNoOp: the manager withholds a
// disk growth beyond the disk-size limit by sending the class's disk size as
// 0 (DiskDefaults present, SizeGiB 0). The disk is not touched — no
// vol-resize, no blockresize — while the CPU change is applied.
func TestReconfigure_SingleHost_WithheldDiskSizeIsNoOp(t *testing.T) {
	desired := reconfigureTo(4, 0, 0)
	desired.Class.DiskDefaults = &contracts.DiskDefaults{Type: "thin", SizeGiB: 0}
	for name, script := range map[string]map[string]string{
		"stopped": {"cfg-maxvcpus": "8"},
		"running": running(map[string]string{"cfg-maxvcpus": "8"}),
	} {
		t.Run(name, func(t *testing.T) {
			_, calls, err := runSingleHostReconfigure(t, script, desired)
			require.NoError(t, err)
			for _, c := range sizeChanges(calls) {
				assert.NotContains(t, c, "resize", "a withheld disk size never resizes the disk")
			}
			assert.NotEmpty(t, sizeChanges(calls), "the CPU change is applied")
		})
	}
}
