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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the provider side of ADR-0007 Addendum A slice 3 for
// GetDiskInfo and ExportDisk: on a clustered provider both run on the bound
// host's leased connection, only for a domain this VM owns, on the disk read
// from that domain's own definition — never on another file of the host.

// teamBOwner is ownerTeamB on the wire.
var teamBOwner = &providerv1.ObjectIdentity{Uid: ownerTeamB.UID, Namespace: ownerTeamB.Namespace, Name: ownerTeamB.Name}

// anyTransport lets the routed export run over the local per-host fakes (a
// real clustered host is always qemu+ssh://).
func anyTransport(libvirtConn) error { return nil }

func TestClustered_GetDiskInfo_RoutedOwnerChecked(t *testing.T) {
	uuid := routingDomainUUID
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	resp, err := NewServer(fx.p).GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamAOwner,
	})
	require.NoError(t, err)
	assert.Equal(t, scdDiskPath, resp.Path)
	assert.Equal(t, "qcow2", resp.Format)
	assert.EqualValues(t, 10737418240, resp.VirtualSizeBytes)
	assert.Equal(t, []string{"s1", "s2"}, resp.Snapshots)
	assert.Equal(t, []string{
		"host-b virsh list --all",
		"host-b virsh dumpxml web",
		"host-b virsh dumpxml " + uuid,
		"local sh -c " + backingKindScript + " sh " + scdDiskPath,
		"local sudo -n qemu-img info -U --output=json " + scdDiskPath,
		"host-b virsh snapshot-list " + uuid + " --name",
	}, fx.calls(), "the disk is read from the checked domain's definition (as root when sudo allows it: a snapshot "+
		"overlay is 0600); nothing is looked up by volume name")
	assert.Zero(t, fx.p.virshProvider.unroutableHits.Load())
}

func TestClustered_GetDiskInfo_ForeignDomainIsNotFoundAndNeverRead(t *testing.T) {
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	_, err := NewServer(fx.p).GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamBOwner,
	})
	assert.Equal(t, codes.NotFound, status.Code(err), "got %v", err)
	assert.Equal(t, []string{"host-b virsh list --all", "host-b virsh dumpxml web"}, fx.calls(),
		"only the read-only ownership check ran")
}

// TestClustered_GetDiskInfo_ExplicitPathMustBeTheDomainsDisk: an explicit disk
// path is honoured only when it is one of the checked domain's own disks; any
// other file on the host is never read.
func TestClustered_GetDiskInfo_ExplicitPathMustBeTheDomainsDisk(t *testing.T) {
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	s := NewServer(fx.p)
	_, err := s.GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, DiskId: "/etc/shadow",
	})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
	for _, c := range fx.calls() {
		assert.NotContains(t, c, "qemu-img", "no other file on the host is read")
	}

	resp, err := s.GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, DiskId: scdDiskPath,
	})
	require.NoError(t, err)
	assert.Equal(t, scdDiskPath, resp.Path)
}

func TestClustered_ExportDisk_RefusesBeforeAnyHost(t *testing.T) {
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	s := NewServer(fx.p)
	for name, tc := range map[string]struct {
		req  *providerv1.ExportDiskRequest
		code codes.Code
	}{
		"pvc (reads the disk from the pod)": {&providerv1.ExportDiskRequest{BackendType: "pvc", DestinationUrl: "pvc://p/k"}, codes.Unimplemented},
		"legacy empty backend (pvc)":        {&providerv1.ExportDiskRequest{DestinationUrl: "pvc://p/k"}, codes.Unimplemented},
		"s3 in direct mode":                 {&providerv1.ExportDiskRequest{BackendType: "s3", TransferMode: "direct"}, codes.InvalidArgument},
		"nfs to a non-nfs URL":              {&providerv1.ExportDiskRequest{BackendType: "nfs", DestinationUrl: "file:///etc"}, codes.InvalidArgument},
		"no host":                           {&providerv1.ExportDiskRequest{BackendType: "nfs", DestinationUrl: "nfs://h/e/k"}, codes.InvalidArgument},
	} {
		t.Run(name, func(t *testing.T) {
			tc.req.VmId, tc.req.Owner = "web", teamAOwner
			if name != "no host" {
				tc.req.TargetHostId = "host-b"
			}
			_, err := s.ExportDisk(context.Background(), tc.req)
			assert.Equal(t, tc.code, status.Code(err), "got %v", err)
		})
	}
	assert.Empty(t, fx.calls(), "a refused export never reaches a host")
}

func TestClustered_ExportDisk_NFSRoutedOnTheOwnedDisk(t *testing.T) {
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	fx.p.hostDiskTransportFn = anyTransport
	resp, err := NewServer(fx.p).ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, BackendType: "nfs", DestinationUrl: "nfs://nas/exports/web.qcow2",
	})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(resp.ExportId, "export-libvirt-nfs-web-"), resp.ExportId)
	assert.Nil(t, resp.Task)
	calls := fx.calls()
	assert.Equal(t, []string{"host-b virsh list --all", "host-b virsh dumpxml web", "host-b virsh dumpxml " + routingDomainUUID}, calls[:3],
		"owner check, then the disk resolved from the checked domain")
	// The export reads the source as root (privileged_copy.go) and reaches
	// the NFS server as the SSH user's uid and gid, as it always has.
	assert.Contains(t, calls, "local qemu-img convert -U -f qcow2 -O qcow2 "+scdDiskPath+
		" nfs://nas/exports/web.qcow2?uid="+scdSSHUID+"&gid="+scdSSHGID)
	assert.Zero(t, fx.p.virshProvider.unroutableHits.Load())
}

// TestClustered_ExportDisk_S3FlattensTheOwnedDiskOnItsHost drives the routed
// s3 export as far as the local fakes allow: the owned domain's disk is
// flattened on the leased connection and the temp is removed again when the
// stream cannot start (a local fake has no SSH stream). The failure is
// VM-scoped, outside the per-Provider circuit breaker.
func TestClustered_ExportDisk_S3FlattensTheOwnedDiskOnItsHost(t *testing.T) {
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	fx.p.hostDiskTransportFn = anyTransport
	_, err := NewServer(fx.p).ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, BackendType: "s3",
		DestinationUrl:     "s3://bucket/web.qcow2",
		StorageOptionsJson: `{"bucket":"bucket","endpoint":"http://127.0.0.1:1"}`,
		Credentials:        map[string]string{"accessKeyID": "a", "secretAccessKey": "s"},
	})
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, contracts.VMOperationFailedReason, errorInfoReason(st), "got %v", err)
	assert.NotContains(t, st.Message(), "secretAccessKey")
	calls := fx.calls()
	assert.Contains(t, calls, "local mktemp -d "+s3ExportStageTemplate,
		"the flattened copy is made in a private directory (mktemp -d: mode 0700) in the source disk's directory")
	assert.Contains(t, calls, "local sh -c "+createCopyOutputScript+" sh "+exportStageUmask+" "+s3ExportTemp,
		"the SSH user creates the private (0600) staging file root writes into")
	assert.Contains(t, calls, "local qemu-img convert -U -f qcow2 -O qcow2 "+scdDiskPath+" "+s3ExportTemp,
		"the owned domain's disk is flattened on its host")
	assert.Contains(t, calls, "local rm -rf -- "+s3ExportStageDir, "the flattened temp is removed with its directory")
}

// s3ExportStageTemplate, s3ExportStageDir and s3ExportTemp are the mktemp -d
// template, the private directory and the temp in it a clustered s3 export of
// "web" flattens into (the routed mktemp fake only prints the path: nothing is
// created in /var/lib/libvirt/images).
const (
	s3ExportStageTemplate = "/var/lib/libvirt/images/" + vmDiskWriteDirPrefix + mktempTemplateSuffix
	s3ExportStageDir      = "/var/lib/libvirt/images/" + vmDiskWriteDirPrefix + "0000000000"
	s3ExportTemp          = s3ExportStageDir + "/.virtrigaud-export-web.qcow2"
)

func TestClustered_ExportDisk_ForeignDomainIsNotFoundAndNeverRead(t *testing.T) {
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	fx.p.hostDiskTransportFn = anyTransport
	_, err := NewServer(fx.p).ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamBOwner, BackendType: "nfs", DestinationUrl: "nfs://nas/e/web.qcow2",
	})
	assert.Equal(t, codes.NotFound, status.Code(err), "got %v", err)
	assert.Equal(t, []string{"host-b virsh list --all", "host-b virsh dumpxml web"}, fx.calls())
}

// TestClustered_ExportDisk_NonSSHHostIsRefusedWithoutItsEndpoint pins the
// default transport check: the refusal names the host id, never its endpoint.
func TestClustered_ExportDisk_NonSSHHostIsRefusedWithoutItsEndpoint(t *testing.T) {
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	_, err := NewServer(fx.p).ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, BackendType: "nfs", DestinationUrl: "nfs://nas/e/web.qcow2",
	})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
	assert.Contains(t, err.Error(), `"host-b"`)
	assert.NotContains(t, err.Error(), "qemu:///")
}
