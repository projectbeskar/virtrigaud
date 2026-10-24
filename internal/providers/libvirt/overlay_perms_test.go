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
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin V7 (v0.4.0-rc7 lab validation): the overlays libvirt
// creates for an external snapshot or a revert are 0600, and the disk in-use
// scan of every OTHER VM's guarded operation must not fail on them.

// qcow2Overlay installs the qemu-img info sidecar of a qcow2 overlay of base
// whose header names base's format, as libvirt writes it.
func (h *fakeHost) qcow2Overlay(disk, base string) {
	h.t.Helper()
	h.info(disk, fmt.Sprintf(`{"filename":%q,"format":"qcow2","backing-filename":%q,"full-backing-filename":%q,`+
		`"backing-filename-format":"qcow2"}`, disk, base, base))
}

// nestedChainDomainXML is a SHUT-OFF domain's definition after libvirt took
// an offline external snapshot (twice): the disk's source is the newest
// overlay, and the chain below it is kept in nested <backingStore> elements,
// each naming its format.
func nestedChainDomainXML(name, uuid, top string, chain ...string) string {
	var bs strings.Builder
	for _, c := range chain {
		fmt.Fprintf(&bs, "<backingStore type='file'><format type='qcow2'/><source file='%s'/>", c)
	}
	bs.WriteString("<backingStore/>")
	for range chain {
		bs.WriteString("</backingStore>")
	}
	return fmt.Sprintf("<domain type='kvm'><name>%s</name><uuid>%s</uuid><devices>"+
		"<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='%s'/>%s<target dev='vda' bus='virtio'/></disk>"+
		"</devices></domain>", name, uuid, top, bs.String())
}

// snapshottedHost defines, on fake host h1, a shut-off VM team-a.web with two
// offline snapshots (disk <- s1 <- s2, s1 readable through sudo only, as
// libvirt leaves it: root-owned 0600) and an unrelated VM team-b.other.
func snapshottedHost(t *testing.T) (h *fakeHost, vp *VirshProvider, disk, s1, s2, other string) {
	t.Helper()
	h = newFakeHost(t)
	vp = h.host("h1")
	disk = h.file(h.images, "team-a.web-disk.qcow2")
	s1 = h.file(h.images, "team-a.web-disk.s1")
	s2 = h.file(h.images, "team-a.web-disk.s2")
	h.qcow2Overlay(s1, disk)
	h.qcow2Overlay(s2, s1)
	require.NoError(t, os.WriteFile(s1+".rootonly", nil, 0o600))
	h.define("h1", "team-a.web", uuidSource, nestedChainDomainXML("team-a.web", uuidSource, s2, s1, disk))
	other = h.file(h.images, "team-b.other-disk.qcow2")
	h.define("h1", "team-b.other", uuidOther, depDomainXML("team-b.other", uuidOther, other, ""))
	return h, vp, disk, s1, s2, other
}

// TestDiskInUseScan_NestedBackingStoreIsNotProbed: the lab failure. A
// shut-off domain whose definition keeps its chain in nested <backingStore>
// elements no longer fails the scan an UNRELATED VM's operation runs: the
// root-only overlay is read once, through sudo, as part of the top disk's
// chain, and never probed again as the SSH user.
func TestDiskInUseScan_NestedBackingStoreIsNotProbed(t *testing.T) {
	h, vp, disk, s1, _, other := snapshottedHost(t)
	ctx := context.Background()

	n, err := diskDependents(ctx, vp, uuidOther, []string{other})
	require.NoError(t, err, "a root-only overlay of another VM must not fail the scan")
	assert.Zero(t, n)
	assert.NotContains(t, h.log("qemu-img"), "info -U --output=json -- "+s1,
		"the nested overlay is never probed as the SSH user")
	assert.Contains(t, h.log("qemu-img"), "info -U -f qcow2 --output=json -- "+s1,
		"it is read once, in the format its parent's header names")

	// The chain below the nested sources still counts: team-a.web uses disk.
	n, err = diskDependents(ctx, vp, uuidOther, []string{disk})
	require.NoError(t, err)
	assert.Equal(t, 1, n, "a file below a nested source is still a reference of its domain")
}

// TestDiskInUseScan_UncoveredNestedSourceIsStillProbed: a nested source the
// top disk's chain does not reach (a definition that disagrees with the
// headers) is walked on its own, probed as the SSH user as before — and an
// image that user cannot read fails the scan closed.
func TestDiskInUseScan_UncoveredNestedSourceIsStillProbed(t *testing.T) {
	h, vp, disk, s1, s2, other := snapshottedHost(t)
	h.qcow2Overlay(s2, disk) // the header skips s1; the definition still lists it

	_, err := diskDependents(context.Background(), vp, uuidOther, []string{other})
	require.Error(t, err, "an unreadable image that no chain read fails the scan closed")
	assert.Contains(t, h.log("qemu-img"), "info -U --output=json -- "+s1, "the uncovered nested source is probed")

	require.NoError(t, os.Remove(s1+".rootonly"))
	_, err = diskDependents(context.Background(), vp, uuidOther, []string{other})
	require.NoError(t, err)
}
