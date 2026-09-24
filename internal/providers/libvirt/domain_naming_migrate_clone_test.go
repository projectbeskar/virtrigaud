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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the single source of truth for the libvirt naming rule
// beyond Create: a migration import lands "<namespace>.<name>-migrated.qcow2"
// for its target VM and that VM's Create attaches exactly that file in place;
// a clone of a VM is named "<targetNamespace>.<targetName>". The operator only
// passes the target VM's identity and records the names/paths the provider
// returns.

// withRegistry wires c.p for the gRPC import path (Server.ImportDisk reaches
// the host through the registry seam).
func (c *createHost) withRegistry() *Server {
	c.t.Helper()
	reg, err := hostconn.NewRegistry(newVirshConn("h1", c.vp))
	require.NoError(c.t, err)
	c.p.registry = reg
	c.p.hostID = "h1"
	return NewServer(c.p)
}

// importFor runs a pvc/file:// ImportDisk of a staged disk for the VM target
// (legacy request when target is zero) and returns the response.
func (c *createHost) importFor(s *Server, staged string, target contracts.ObjectIdentity, targetName string) (*providerv1.ImportDiskResponse, error) {
	req := &providerv1.ImportDiskRequest{SourceUrl: "file://" + staged, Format: "qcow2", TargetName: targetName}
	if target.Namespace != "" {
		req.TargetVm = &providerv1.ObjectIdentity{Namespace: target.Namespace, Name: target.Name}
	}
	return s.ImportDisk(context.Background(), req)
}

func TestMigrationImport_LandsNamespacedDiskAndCreateAdoptsIt(t *testing.T) {
	c := newCreateHost(t)
	s := c.withRegistry()
	staged := c.file(c.outside, "exported.qcow2")
	ctx := context.Background()

	// The operator sends TargetName "<vm>-migrated" (for other providers) and
	// the target VM's identity; the provider lands the namespaced name.
	respA, err := c.importFor(s, staged, ownerTeamA, "web"+contracts.ImportedDiskNameSuffix)
	require.NoError(t, err)
	assert.Equal(t, "team-a.web-migrated", respA.DiskId)
	assert.Equal(t, filepath.Join(c.images, "team-a.web-migrated.qcow2"), respA.Path)

	// Another namespace's "web" migration lands its own file; nothing is overwritten.
	respB, err := c.importFor(s, staged, ownerTeamB, "web"+contracts.ImportedDiskNameSuffix)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(c.images, "team-b.web-migrated.qcow2"), respB.Path)
	assert.NotEqual(t, respA.Path, respB.Path)

	// The migration controller hands respA.Path to the target VM unchanged
	// (status.diskInfo.targetPath -> spec.importedDisk.path), and
	// isOwnMigrationDisk marks it ImportedDisk because the paths are equal.
	// team-a/web's Create — named by the SAME rule — attaches it in place.
	resp, err := c.p.Create(ctx, contracts.CreateRequest{
		Name: "web", Owner: ownerTeamA, Image: contracts.VMImage{Path: respA.Path, ImportedDisk: true},
	})
	require.NoError(t, err)
	assert.Equal(t, "team-a.web", resp.ID)
	refs, err := parseDomainPathRefs(c.domainXML("team-a.web"))
	require.NoError(t, err)
	assert.Equal(t, respA.Path, refs.disks[0], "the imported disk is attached in place, not copied")

	// team-b/web may never attach team-a's landed disk: not its name, so it is
	// a reserved VirtRigaud file and rejected as a base image.
	_, err = c.p.Create(ctx, contracts.CreateRequest{
		Name: "web", Owner: ownerTeamB, Image: contracts.VMImage{Path: respA.Path, ImportedDisk: true},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)

	// A second import for team-a/web while its VM runs on the landed disk
	// must not overwrite it.
	before, err := os.ReadFile(respA.Path) //nolint:gosec // test reads its fixture
	require.NoError(t, err)
	_, err = c.importFor(s, c.file(c.outside, "exported-2.qcow2"), ownerTeamA, "web"+contracts.ImportedDiskNameSuffix)
	require.Error(t, err)
	assert.True(t, contracts.IsConflict(err), "got %v", err)
	after, err := os.ReadFile(respA.Path) //nolint:gosec // test reads its fixture
	require.NoError(t, err)
	assert.Equal(t, before, after, "a live VM's imported disk is never replaced")
}

func TestMigrationImport_LegacyRequestKeepsBareName(t *testing.T) {
	c := newCreateHost(t)
	s := c.withRegistry()
	staged := c.file(c.outside, "exported.qcow2")

	resp, err := c.importFor(s, staged, contracts.ObjectIdentity{}, "web"+contracts.ImportedDiskNameSuffix)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(c.images, "web-migrated.qcow2"), resp.Path, "an older manager keeps the legacy landing name")

	// ... and an older manager's Create (no owner) adopts it.
	vol, err := c.p.createDiskFromHostImage(context.Background(), c.vp, NewStorageProvider(c.vp),
		contracts.CreateRequest{Name: "web", Image: contracts.VMImage{Path: resp.Path, ImportedDisk: true}},
		"web", resp.Path, vmDiskVolumeName("web"), 10)
	require.NoError(t, err)
	assert.Equal(t, resp.Path, vol.Path)
}

func TestMigrationImport_MismatchedTargetIsInvalidArgument(t *testing.T) {
	c := newCreateHost(t)
	s := c.withRegistry()
	_, err := c.importFor(s, c.file(c.outside, "exported.qcow2"), ownerTeamA, "db"+contracts.ImportedDiskNameSuffix)
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
	assert.NotContains(t, c.log("qemu-img"), "convert", "nothing is written for a malformed request")
}

func TestImportedDiskVolumeName(t *testing.T) {
	got, err := importedDiskVolumeName(ownerTeamA, "web-migrated")
	require.NoError(t, err)
	assert.Equal(t, "team-a.web-migrated", got)
	assert.Equal(t, importedVolumeFileName("team-a.web"), got+qcow2Ext,
		"the landing file is exactly the file Create adopts for team-a/web")

	got, err = importedDiskVolumeName(contracts.ObjectIdentity{Namespace: "team-a", Name: "web"}, "")
	require.NoError(t, err)
	assert.Equal(t, "team-a.web-migrated", got, "no UID is needed to name the landing disk")

	got, err = importedDiskVolumeName(contracts.ObjectIdentity{}, "web-migrated")
	require.NoError(t, err)
	assert.Equal(t, "web-migrated", got, "legacy")

	_, err = importedDiskVolumeName(ownerTeamA, "db-migrated")
	requireInvalidSpec(t, err)
}

// TestClone_TargetIsNamespaced clones team-a/web into team-b/web-clone: the new
// domain (and its disk) is "team-b.web-clone", returned as the target VM ID;
// it carries no owner stamp (the source's is stripped).
func TestClone_TargetIsNamespaced(t *testing.T) {
	c := newCreateHost(t)
	base := c.file(c.images, "ubuntu.qcow2")
	ctx := context.Background()
	_, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
	require.NoError(t, err)

	resp, err := c.p.Clone(ctx, contracts.CloneRequest{
		Source:     contracts.VMRef{ID: "team-a.web"},
		TargetName: "web-clone",
		TargetVM:   contracts.ObjectIdentity{Namespace: "team-b", Name: "web-clone"},
	})
	require.NoError(t, err)
	assert.Equal(t, "team-b.web-clone", resp.TargetVmID)

	x := c.domainXML("team-b.web-clone")
	d, err := parseDomainLibvirtxml(x)
	require.NoError(t, err)
	assert.Equal(t, "team-b.web-clone", d.Name)
	refs, err := parseDomainPathRefs(x)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(c.images, "team-b.web-clone-disk.qcow2"), refs.disks[0])
	owners, err := domainOwners(x)
	require.NoError(t, err)
	assert.Empty(t, owners, "the clone never claims the source VM's owner")

	// A second clone to the same target is refused (never binds or redefines).
	_, err = c.p.Clone(ctx, contracts.CloneRequest{
		Source:     contracts.VMRef{ID: "team-a.web"},
		TargetName: "web-clone",
		TargetVM:   contracts.ObjectIdentity{Namespace: "team-b", Name: "web-clone"},
	})
	requireInvalidSpec(t, err)

	// An older manager (no TargetVM) keeps the bare name.
	resp, err = c.p.Clone(ctx, contracts.CloneRequest{Source: contracts.VMRef{ID: "team-a.web"}, TargetName: "web-clone"})
	require.NoError(t, err)
	assert.Equal(t, "web-clone", resp.TargetVmID)

	// A TargetVM that does not name the target is refused before any host work.
	_, err = c.p.Clone(ctx, contracts.CloneRequest{
		Source:     contracts.VMRef{ID: "team-a.web"},
		TargetName: "web-clone",
		TargetVM:   contracts.ObjectIdentity{Namespace: "team-b", Name: "other"},
	})
	requireInvalidSpec(t, err)
}

// TestServer_Clone_ThreadsTargetVM proves the wire field reaches the provider.
func TestServer_Clone_ThreadsTargetVM(t *testing.T) {
	c := newCreateHost(t)
	base := c.file(c.images, "ubuntu.qcow2")
	_, err := c.p.Create(context.Background(), c.createReq(ownerTeamA, base))
	require.NoError(t, err)

	resp, err := NewServer(c.p).Clone(context.Background(), &providerv1.CloneRequest{
		SourceVmId: "team-a.web",
		TargetName: "web-clone",
		TargetVm:   &providerv1.ObjectIdentity{Namespace: "team-a", Name: "web-clone"},
	})
	require.NoError(t, err)
	assert.Equal(t, "team-a.web-clone", resp.TargetVmId)
}
