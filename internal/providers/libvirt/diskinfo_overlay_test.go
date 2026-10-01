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

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the ADR-0007 Slice 5 lab follow-up for GetDiskInfo (which
// every export resolves its disk with): the sizes of a snapshotted VM's 0600
// libvirt-qemu overlay are read through the in-use check's sudo-first
// `qemu-img info -U`, instead of being reported as 0; sudo refused reads as
// the SSH user, as before; a path the caller names that is not one of the
// domain's disks is never read as root.

// scdVirtualSize and scdActualSize are the sizes the fixtures' qemu-img reports.
const (
	scdVirtualSize = 10737418240
	scdActualSize  = 1073741824
)

func TestSingleHost_GetDiskInfo_SnapshotOverlaySizesAreReadAsRoot(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{scdDomain: overlayDomainXML(scdDomain, scdDomainOpts{})})
	installRootOnlyImage(t, scdOverlayPath)

	resp, err := NewServer(fx.p).GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{VmId: scdDomain})
	require.NoError(t, err)
	assert.Equal(t, scdOverlayPath, resp.Path)
	assert.EqualValues(t, scdVirtualSize, resp.VirtualSizeBytes, "the overlay was read (as root), not reported as 0")
	assert.EqualValues(t, scdActualSize, resp.ActualSizeBytes)
	calls := fx.calls()
	assert.Contains(t, calls, "local sh -c "+backingKindScript+" sh "+scdOverlayPath, "a device or FIFO is never opened as root")
	assert.Contains(t, calls, "local sudo -n qemu-img info -U -f qcow2 --output=json -- "+scdOverlayPath, "the documented qemu-img info -U rule")
	for _, c := range calls {
		assert.NotContains(t, c, "Permission denied")
	}
}

func TestSingleHost_GetDiskInfo_SudoRefusedReadsAsBefore(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{scdDomain: overlayDomainXML(scdDomain, scdDomainOpts{})})
	installRootOnlyImage(t, scdOverlayPath)
	fx.script("local", "fail-sudo", "")

	resp, err := NewServer(fx.p).GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{VmId: scdDomain})
	require.NoError(t, err, "sizes are best-effort")
	assert.Zero(t, resp.VirtualSizeBytes, "the SSH user cannot read the overlay: 0, as before")
	assert.Contains(t, fx.calls(), "local qemu-img info -U -f qcow2 --output=json -- "+scdOverlayPath, "the historical read as the SSH user")
}

// TestSingleHost_GetDiskInfo_CallerPathMustBeTheDomainsDisk: on a single
// host too (parity with the clustered owner-checked path), an explicit disk
// path that is not one of the domain's disks is refused before anything reads
// it — not as root, and not as the SSH user either (no format probe of a file
// the caller names) — and the domain's own disk by path is read as before.
func TestSingleHost_GetDiskInfo_CallerPathMustBeTheDomainsDisk(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
	s := NewServer(fx.p)
	for _, other := range []string{"/var/lib/libvirt/images/other.qcow2", "/etc/shadow"} {
		_, err := s.GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{VmId: scdDomain, DiskId: other})
		require.Error(t, err, other)
		assert.Contains(t, err.Error(), "is not a disk of VM")
	}
	for _, c := range fx.calls() {
		assert.NotContains(t, c, "qemu-img", "no file the caller names is read, by anyone: %q", c)
	}

	resp, err := s.GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{VmId: scdDomain, DiskId: scdDiskPath})
	require.NoError(t, err)
	assert.Equal(t, scdDiskPath, resp.Path)
	assert.Equal(t, "qcow2", resp.Format)
	assert.Contains(t, fx.calls(), "local sudo -n qemu-img info -U -f qcow2 --output=json -- "+scdDiskPath)
}

func TestClustered_GetDiskInfo_SnapshotOverlaySizesAreReadAsRoot(t *testing.T) {
	fx := ownedWebOnB(t, overlayDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	installRootOnlyImage(t, scdOverlayPath)

	resp, err := NewServer(fx.p).GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamAOwner,
	})
	require.NoError(t, err)
	assert.Equal(t, scdOverlayPath, resp.Path)
	assert.EqualValues(t, scdVirtualSize, resp.VirtualSizeBytes)
	assert.Contains(t, fx.calls(), "local sudo -n qemu-img info -U -f qcow2 --output=json -- "+scdOverlayPath)
}
