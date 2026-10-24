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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
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

// TestGuard_UnreadableImageSaysWhatToFix: a guard that fails because a disk
// image on the host cannot be read says so — permission denied, and where the
// repair is documented — instead of a generic transient host error, without
// naming the image (another tenant's VM's). It stays fail-closed and
// retryable: Unavailable + VM_DISK_CHECK_FAILED, which the manager retries
// with a backoff (an operator's fix on the host needs no change to the
// Kubernetes object).
func TestGuard_UnreadableImageSaysWhatToFix(t *testing.T) {
	h, vp, disk, s1, s2, _ := snapshottedHost(t)
	h.qcow2Overlay(s2, disk) // s1 is reached by no chain: probed, unreadable

	err := refuseIfDiskHasDependents(context.Background(), vp, "team-b.other", guardOpSnapshotCreate)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `snapshot create of libvirt domain "team-b.other" not performed: `+
		`permission denied reading a disk image on its host`)
	assert.Contains(t, err.Error(), unreadableImageDoc)
	assert.NotContains(t, err.Error(), "transient")
	assert.NotContains(t, err.Error(), "team-a", "another VM's image is never named")
	assert.NotContains(t, err.Error(), s1)
	assert.True(t, contracts.IsRetryable(err), "fail closed, retried")

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.True(t, errorInfoReasons(st)[contracts.VMDiskCheckFailedReason])

	routed, ok := status.FromError(routedRPCError("create snapshot",
		&hostOpError{host: "host-a", err: err, progress: &hostProgress{}}))
	require.True(t, ok)
	assert.Equal(t, codes.Unavailable, routed.Code())
	assert.True(t, errorInfoReasons(routed)[contracts.VMDiskCheckFailedReason])
	assert.True(t, errorInfoReasons(routed)[contracts.VMOperationFailedReason])
	assert.Contains(t, routed.Message(), unreadableImageDoc)

	// Any other failure keeps the generic message.
	generic := guardCheckFailed(guardOpDelete, "team-b.other", errors.New("boom"))
	assert.Contains(t, generic.Error(), "transient host error")
}

// TestClusteredDelete_UnreadableImageOnAnotherHost: through the cluster-wide
// guard's fan-out, a host whose shut-off domain's disk the provider cannot
// read (no sudo rule, a 0600 image) fails the delete closed with the
// operator-actionable answer — naming no host, path or other domain — and
// nothing is removed.
func TestClusteredDelete_UnreadableImageOnAnotherHost(t *testing.T) {
	s := newSharedPool(t)
	disk := s.disk("team-a.web-disk.qcow2", "live-disk")
	s.defineOn("host-b", "team-a.web", uuidVMOnB, guardDomainXML("team-a.web", uuidVMOnB, disk, ownerTeamA, false))
	other := s.disk("other-vm-disk.qcow2", "x")
	require.NoError(t, os.WriteFile(other+".rootonly", nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(s.root, "sudo-refuses"), nil, 0o600))
	s.defineOn("host-a", "other-vm", uuidForeign, guardDomainXML("other-vm", uuidForeign, other, ownerForeign, false))

	_, err := NewServer(s.p).Delete(context.Background(), &providerv1.DeleteRequest{
		Id: "team-a.web", TargetHostId: "host-b", Owner: teamAOwner,
	})
	st, ok := status.FromError(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.True(t, errorInfoReasons(st)[contracts.VMDiskCheckFailedReason], "got %v", st)
	assert.Contains(t, st.Message(), "permission denied reading a disk image on a host of this Provider")
	assert.Contains(t, st.Message(), unreadableImageDoc)
	requireWireSafe(t, st.Message(), s.images, "other-vm")
	s.requireUntouched("host-b", "team-a.web")
	assert.FileExists(t, disk)
}

// TestClusterGuard_UnreadableImageSaysWhatToFix: the cluster-wide guard says
// the same when a host answered but one of its images cannot be read.
func TestClusterGuard_UnreadableImageSaysWhatToFix(t *testing.T) {
	e := (&clusterGuardIncompleteError{unreadable: true}).with(guardOpDelete, "team-b.other")
	assert.Contains(t, e.Error(), "permission denied reading a disk image on a host of this Provider")
	assert.Contains(t, e.Error(), unreadableImageDoc)
	assert.True(t, contracts.IsRetryable(e))
	st := e.GRPCStatus()
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.True(t, errorInfoReasons(st)[contracts.VMDiskCheckFailedReason])

	unreachable := (&clusterGuardIncompleteError{unreachable: true, unreadable: true}).with(guardOpDelete, "x")
	assert.Contains(t, unreachable.Error(), "could not be reached", "an unreachable host wins")

	wrapped := hostCheckFailed("read backing chain", &unreadableImageError{path: "/p/x", cause: errors.New("exit 1")})
	assert.True(t, isUnreadableImage(wrapped), "found through hostCheckFailed")
	assert.NotContains(t, wrapped.Error(), "/p/x", "the path stays in the provider log")
}
