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
	"strings"
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
	assert.Contains(t, calls, "local sudo -n qemu-img info -U --output=json "+scdOverlayPath, "the documented qemu-img info -U rule")
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
	assert.Contains(t, fx.calls(), "local qemu-img info -U --output=json "+scdOverlayPath, "the historical read as the SSH user")
}

func TestSingleHost_GetDiskInfo_CallerPathIsNeverReadAsRoot(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
	const other = "/var/lib/libvirt/images/other.qcow2"
	_, err := NewServer(fx.p).GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{VmId: scdDomain, DiskId: other})
	require.NoError(t, err)
	calls := fx.calls()
	assert.Contains(t, calls, "local qemu-img info -U --output=json "+other)
	for _, c := range calls {
		assert.False(t, strings.HasPrefix(c, "local sudo"), "a path that is not the domain's disk is read as the SSH user only: %q", c)
	}
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
	assert.Contains(t, fx.calls(), "local sudo -n qemu-img info -U --output=json "+scdOverlayPath)
}
