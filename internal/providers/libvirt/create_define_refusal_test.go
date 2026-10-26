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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// libvirtPCIRootRefusal is what libvirt 10 answered the secure-boot domain of
// earlier releases (q35 machine with an i440fx pci-root).
const libvirtPCIRootRefusal = "error: Failed to define domain from /var/lib/virtrigaud/staging/team-a.web-domain.xml.Ab12Cd\n" +
	"error: XML error: The PCI controller with index='0' must be model='pcie-root' for this machine type, " +
	"but model='pci-root' was found instead\n"

func TestLibvirtDefinitionRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, stderr, detail string
		refused              bool
	}{
		{"XML error", libvirtPCIRootRefusal,
			"XML error: The PCI controller with index='0' must be model='pcie-root' for this machine type, but model='pci-root' was found instead", true},
		{"unsupported configuration", "error: Failed to define domain from /x\nerror: unsupported configuration: unexpected feature 'iommu'\n",
			"unsupported configuration: unexpected feature 'iommu'", true},
		{"no OVMF (libvirt 9+)", "error: Failed to define domain from /x\nerror: operation failed: Unable to find 'efi' firmware that is compatible with the current configuration\n",
			"operation failed: Unable to find 'efi' firmware that is compatible with the current configuration", true},
		{"no OVMF (older libvirt)", "error: operation failed: Unable to find any firmware to satisfy 'efi'\n",
			"operation failed: Unable to find any firmware to satisfy 'efi'", true},
		{"transport failure", "error: Disconnected from qemu:///system due to end of file\n", "", false},
		{"name taken", "error: operation failed: domain 'team-a.web' already exists with uuid 1234\n", "", false},
		{"other operation failure", "error: scripted define failure\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail, refused := libvirtDefinitionRefusal(tc.stderr)
			assert.Equal(t, tc.refused, refused)
			assert.Equal(t, tc.detail, detail)
		})
	}

	long, ok := libvirtDefinitionRefusal("error: XML error: " + strings.Repeat("x", 2*maxDefineRefusalDetail))
	require.True(t, ok)
	assert.LessOrEqual(t, len(long), maxDefineRefusalDetail+len("..."), "the detail is bounded")
}

// scriptDefineFailure makes `virsh define` on host fail with stderr.
func (c *createHost) scriptDefineFailure(host, stderr string) {
	c.t.Helper()
	require.NoError(c.t, os.WriteFile(filepath.Join(c.root, host, "fail-define"), []byte(stderr), 0o600))
}

// TestCreate_DefineRefusedIsInvalidSpecAndRemovesDisk: libvirt refusing the
// definition itself is a definite refusal — InvalidSpec (InvalidArgument on
// the wire), never retryable — and the disk the attempt copied is removed, so
// no failed attempt leaves its disk on the pool. A later create, once the
// cause is gone, succeeds.
func TestCreate_DefineRefusedIsInvalidSpecAndRemovesDisk(t *testing.T) {
	c := newCreateHost(t)
	base := c.file(c.images, "ubuntu.qcow2")
	disk := filepath.Join(c.images, "team-a.web-disk.qcow2")
	ctx := context.Background()
	c.scriptDefineFailure("h1", libvirtPCIRootRefusal)

	_, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
	require.Error(t, err)
	assert.True(t, contracts.IsInvalidSpec(err), "a definition libvirt refuses is InvalidSpec: %v", err)
	assert.False(t, contracts.IsRetryable(err), "never retried as if the outcome were open: %v", err)
	assert.NotErrorIs(t, err, errDefineOutcomeUnknown)
	assert.Contains(t, err.Error(), "must be model='pcie-root'", "the status names libvirt's reason")
	assert.NotContains(t, err.Error(), "secret-of-team-a", "no user-data in the refusal")
	st, _ := status.FromError(createRPCError(err))
	assert.Equal(t, codes.InvalidArgument, st.Code())

	assert.Contains(t, c.removals(), disk, "the disk this attempt copied is removed")
	assert.Contains(t, c.log("virsh"), "\tpool-refresh default", "the pool forgets the removed disk")
	assert.Empty(t, c.stagingEntries(), "seed and staged XML are removed too")

	require.NoError(t, os.Remove(filepath.Join(c.root, "h1", "fail-define")))
	resp, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
	require.NoError(t, err)
	assert.Equal(t, "team-a.web", resp.ID)
}

// TestCreate_FailedAttemptDiskRemovedOnlyWhenDomainAbsent: any failed define
// whose domain is verified absent removes the attempt's disk (and stays
// retryable unless libvirt refused the definition); a define whose outcome
// cannot be established keeps it — the domain may reference it.
func TestCreate_FailedAttemptDiskRemovedOnlyWhenDomainAbsent(t *testing.T) {
	ctx := context.Background()

	t.Run("define failed, domain absent: retryable, disk removed", func(t *testing.T) {
		c := newCreateHost(t)
		base := c.file(c.images, "ubuntu.qcow2")
		c.scriptDefineFailure("h1", "")
		require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "fail-define"), nil, 0o600))

		_, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
		require.Error(t, err)
		assert.False(t, contracts.IsInvalidSpec(err))
		assert.True(t, contracts.IsRetryable(err), "%v", err)
		assert.Contains(t, c.removals(), filepath.Join(c.images, "team-a.web-disk.qcow2"))
	})

	t.Run("define outcome unknown: disk kept", func(t *testing.T) {
		c := newCreateHost(t)
		base := c.file(c.images, "ubuntu.qcow2")
		c.scriptDefineFailure("h1", libvirtPCIRootRefusal)
		require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "fail-domuuid"), nil, 0o600))

		_, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
		require.Error(t, err)
		assert.ErrorIs(t, err, errDefineOutcomeUnknown)
		assert.False(t, contracts.IsInvalidSpec(err), "an XML error whose domain cannot be checked is not a definite refusal")
		assert.Empty(t, c.removals(), "the domain may exist and reference its disk")
	})

	t.Run("blank disk: removed", func(t *testing.T) {
		c := newCreateHost(t)
		c.scriptDefineFailure("h1", libvirtPCIRootRefusal)
		req := c.createReq(ownerTeamA, "")
		req.Image = contracts.VMImage{}

		_, err := c.p.Create(ctx, req)
		require.Error(t, err)
		assert.True(t, contracts.IsInvalidSpec(err), "%v", err)
		assert.Contains(t, c.removals(), filepath.Join(c.images, "team-a.web-disk"))
	})
}

// TestCreate_DefineRefusedKeepsImportedDisk: a migration's imported disk,
// attached in place, is never removed by a failed create — it is not the
// attempt's copy, and it holds the migrated VM.
func TestCreate_DefineRefusedKeepsImportedDisk(t *testing.T) {
	c := newCreateHost(t)
	s := c.withRegistry()
	staged := c.file(c.outside, "exported.qcow2")
	landed, err := c.importFor(s, staged, ownerTeamA, "web"+contracts.ImportedDiskNameSuffix)
	require.NoError(t, err)
	c.scriptDefineFailure("h1", libvirtPCIRootRefusal)

	_, err = c.p.Create(context.Background(), contracts.CreateRequest{
		Name: "web", Owner: ownerTeamA, Image: contracts.VMImage{Path: landed.Path, ImportedDisk: true},
	})
	require.Error(t, err)
	assert.True(t, contracts.IsInvalidSpec(err), "%v", err)
	assert.NotContains(t, c.removals(), landed.Path, "the imported disk is kept")
	assert.FileExists(t, landed.Path)
}

// TestCreate_Clustered_DefineRefusedIsInvalidSpecAndRemovesDisk is the
// routed create (clustered provider, pending host host-a): the same
// InvalidSpec answer and the same cleanup, after the cluster-wide check that
// no domain on any host of the Provider uses the disk.
func TestCreate_Clustered_DefineRefusedIsInvalidSpecAndRemovesDisk(t *testing.T) {
	c := newCreateHost(t)
	c.host("host-a")
	c.host("host-b")
	p, _, _ := routedCluster(t)
	p.imageDirs = []string{c.images}
	p.hostStagingDir, p.hostSeedDir = c.staging, c.staging
	base := c.file(c.images, "ubuntu.qcow2")
	// `virsh define <file>` runs through runRemoteVirshCommand, which the
	// fake host serves as $FAKE_DEFAULT_HOST (h1) on a non-system URI.
	c.scriptDefineFailure("host-a", libvirtPCIRootRefusal)
	c.scriptDefineFailure("h1", libvirtPCIRootRefusal)

	req := c.createReq(ownerTeamA, base)
	req.TargetHostID = "host-a"
	_, err := p.Create(context.Background(), req)
	require.Error(t, err)
	assert.True(t, contracts.IsInvalidSpec(err), "%v", err)
	st, _ := status.FromError(createRPCError(err))
	assert.Equal(t, codes.InvalidArgument, st.Code(), "a definite refusal, not a host failure")

	disk := filepath.Join(c.images, "team-a.web-disk.qcow2")
	assert.Contains(t, c.removals(), disk)
	var scannedB bool
	for _, call := range c.virshCalls("host-b") {
		if strings.HasPrefix(call, "list --all") {
			scannedB = true
		}
	}
	assert.True(t, scannedB, "the other host is checked before the disk is removed")
}
