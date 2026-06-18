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

package proxmox

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestParsePvesmPath covers the resolution of `pvesm path` stdout into a single
// on-node path: the first non-empty trimmed line wins; surrounding whitespace and
// trailing blank lines are tolerated.
func TestParsePvesmPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain path", "/dev/pve/vm-100-disk-0\n", "/dev/pve/vm-100-disk-0"},
		{"dir storage qcow2", "/var/lib/vz/images/100/vm-100-disk-0.qcow2\n", "/var/lib/vz/images/100/vm-100-disk-0.qcow2"},
		{"leading/trailing whitespace", "   /mnt/pve/nfs/images/100/vm-100-disk-0.qcow2  \n\n", "/mnt/pve/nfs/images/100/vm-100-disk-0.qcow2"},
		{"empty", "", ""},
		{"only whitespace", "   \n\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parsePvesmPath(tc.in))
		})
	}
}

// TestBuildExportFlattenCommand pins the node-side qemu-img flatten argv: -U to
// skip the shared lock, -f forces the source driver, -O qcow2 keeps the native
// format, -c only when compression is requested, and both paths shell-quoted.
func TestBuildExportFlattenCommand(t *testing.T) {
	t.Run("no compression", func(t *testing.T) {
		cmd := buildExportFlattenCommand("qcow2", "/dev/pve/vm-100-disk-0", "/var/tmp/.x.qcow2", false)
		assert.Equal(t,
			"qemu-img convert -U -f qcow2 -O qcow2 '/dev/pve/vm-100-disk-0' '/var/tmp/.x.qcow2'",
			cmd)
		assert.NotContains(t, cmd, " -c ")
	})

	t.Run("compression adds -c", func(t *testing.T) {
		cmd := buildExportFlattenCommand("raw", "/dev/pve/vm-100-disk-0", "/var/tmp/.x.qcow2", true)
		assert.Equal(t,
			"qemu-img convert -U -f raw -O qcow2 -c '/dev/pve/vm-100-disk-0' '/var/tmp/.x.qcow2'",
			cmd)
	})

	t.Run("paths with metacharacters are quoted", func(t *testing.T) {
		cmd := buildExportFlattenCommand("qcow2", "/var/tmp/a b", "/var/tmp/c'd", false)
		assert.Contains(t, cmd, "'/var/tmp/a b'")
		assert.Contains(t, cmd, `'/var/tmp/c'\''d'`)
	})
}

// TestProxmoxExportStagePath asserts the export stage path is in the staging dir,
// dot-prefixed and qcow2-suffixed, and sanitizes path separators out of the VM id
// so it cannot escape the directory.
func TestProxmoxExportStagePath(t *testing.T) {
	p := proxmoxExportStagePath("vm/100")
	assert.True(t, strings.HasPrefix(p, proxmoxExportStageDir+"/.virtrigaud-export-"), "got %q", p)
	assert.True(t, strings.HasSuffix(p, ".qcow2"), "got %q", p)
	assert.NotContains(t, strings.TrimPrefix(p, proxmoxExportStageDir+"/"), "/",
		"vm id separators must be sanitized so the temp cannot escape the staging dir")
}

// TestProxmoxImportStagePath asserts the import stage path is deterministic per id
// (so a retry overwrites rather than leaks) and uses the source format suffix.
func TestProxmoxImportStagePath(t *testing.T) {
	a := proxmoxImportStagePath("web-01-migrated", "qcow2")
	b := proxmoxImportStagePath("web-01-migrated", "qcow2")
	assert.Equal(t, a, b, "import stage path must be deterministic per id (retry-safe)")
	assert.True(t, strings.HasPrefix(a, proxmoxImportStageDir+"/.virtrigaud-import-"), "got %q", a)
	assert.True(t, strings.HasSuffix(a, ".qcow2"), "got %q", a)
}

// TestBuildImportDiskCommand pins the `qm importdisk` argv: vmid, shell-quoted
// path/storage, and an explicit format so PVE does not probe.
func TestBuildImportDiskCommand(t *testing.T) {
	cmd := buildImportDiskCommand(110001, "/var/tmp/.virtrigaud-import-web.qcow2", "local-lvm", "qcow2")
	assert.Equal(t,
		"qm importdisk 110001 '/var/tmp/.virtrigaud-import-web.qcow2' 'local-lvm' --format 'qcow2'",
		cmd)
}

// TestImportedDiskVolume pins the volid qm importdisk produces for the first disk.
func TestImportedDiskVolume(t *testing.T) {
	assert.Equal(t, "local-lvm:vm-110001-disk-0", importedDiskVolume("local-lvm", 110001))
	assert.Equal(t, "ceph-pool:vm-100-disk-0", importedDiskVolume("ceph-pool", 100))
}

// TestBuildImportedDiskSetCommand covers the disk-attach `qm set` command: it
// sets ONLY the virtio-scsi boot disk + boot order (encoding-safe values);
// cloud-init is handled separately via the API.
func TestBuildImportedDiskSetCommand(t *testing.T) {
	cmd := buildImportedDiskSetCommand(110001, "local-lvm:vm-110001-disk-0")
	assert.Contains(t, cmd, "qm set 110001")
	assert.Contains(t, cmd, "--scsihw virtio-scsi-pci")
	assert.Contains(t, cmd, "--scsi0 'local-lvm:vm-110001-disk-0'")
	assert.Contains(t, cmd, "--boot order=scsi0")
	// SSH keys / cloud-init MUST NOT be on the qm set command line (they go via
	// the API to avoid command-line encoding pitfalls).
	assert.NotContains(t, cmd, "--sshkeys")
	assert.NotContains(t, cmd, "cloudinit")
}

// TestBuildImportedDiskCloudInitValues covers the API cloud-init values: a
// cloud-init drive is always set; ciuser/sshkeys only when present. url.Values
// handles the encoding (notably multi-line SSH keys).
func TestBuildImportedDiskCloudInitValues(t *testing.T) {
	t.Run("drive only when no ciuser/keys", func(t *testing.T) {
		v := buildImportedDiskCloudInitValues("local-lvm", &pveapi.VMConfig{})
		assert.Equal(t, "local-lvm:cloudinit", v.Get("ide2"))
		assert.Empty(t, v.Get("ciuser"))
		assert.Empty(t, v.Get("sshkeys"))
	})

	t.Run("ciuser + multi-line sshkeys", func(t *testing.T) {
		multiKey := "ssh-ed25519 AAAA...one\nssh-ed25519 BBBB...two"
		v := buildImportedDiskCloudInitValues("local-lvm", &pveapi.VMConfig{
			CIUser:  "ubuntu",
			SSHKeys: multiKey + "\n", // trailing newline must be trimmed
		})
		assert.Equal(t, "local-lvm:cloudinit", v.Get("ide2"))
		assert.Equal(t, "ubuntu", v.Get("ciuser"))
		assert.Equal(t, multiKey, v.Get("sshkeys"), "multi-line keys preserved, trailing newline trimmed")
	})
}

// TestSanitizeProxmoxName guards the path-escape defense.
func TestSanitizeProxmoxName(t *testing.T) {
	assert.Equal(t, "vm-100", sanitizeProxmoxName("vm/100"))
	assert.Equal(t, "a-b", sanitizeProxmoxName("a..b"))
	assert.Equal(t, "web", sanitizeProxmoxName("  web  "))
	assert.NotEmpty(t, sanitizeProxmoxName(""))
}

// s3GRPCCode extracts the gRPC status code from a provider error.
func s3GRPCCode(t *testing.T, err error) codes.Code {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "expected a gRPC status error, got %T: %v", err, err)
	return st.Code()
}

// TestExportDisk_NFSBackendRejected verifies the dispatch still rejects an
// unimplemented backend (nfs) honestly, even though s3 is now accepted.
func TestExportDisk_NFSBackendRejected(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)

	_, err = provider.ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
		VmId:        "100",
		BackendType: "nfs",
	})
	assert.Equal(t, codes.Unimplemented, s3GRPCCode(t, err),
		"nfs export backend is not implemented and must be rejected")
}

// TestImportDisk_NFSBackendRejected mirrors the export rejection for import.
func TestImportDisk_NFSBackendRejected(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)

	_, err = provider.ImportDisk(context.Background(), &providerv1.ImportDiskRequest{
		BackendType: "nfs",
	})
	assert.Equal(t, codes.Unimplemented, s3GRPCCode(t, err),
		"nfs import backend is not implemented and must be rejected")
}

// TestExportDisk_S3NoSSHTransport verifies that an s3 export on a provider with no
// SSH data plane returns an actionable Unavailable error rather than panicking.
// createTestProvider builds a provider whose endpoint yields no SSH host, so
// p.ssh is nil.
func TestExportDisk_S3NoSSHTransport(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	provider.ssh = nil // explicit: token-only deployment

	_, err = provider.ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
		VmId:        "100",
		BackendType: "s3",
	})
	assert.Equal(t, codes.Unavailable, s3GRPCCode(t, err))
}

// TestImportDisk_S3NoSSHTransport mirrors the export no-SSH guard for import.
func TestImportDisk_S3NoSSHTransport(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	provider.ssh = nil

	_, err = provider.ImportDisk(context.Background(), &providerv1.ImportDiskRequest{
		BackendType: "s3",
		TargetName:  "web-migrated",
	})
	assert.Equal(t, codes.Unavailable, s3GRPCCode(t, err))
}

// TestParseCreateRequest_ImportedDiskPath verifies the migration import→create
// handoff: a contracts.VMImage carrying a node Path (and no TemplateName)
// populates VMConfig.ImportedDiskPath/Format so Create takes the importdisk path.
func TestParseCreateRequest_ImportedDiskPath(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)

	req := &providerv1.CreateRequest{
		Name:      "web-migrated",
		ClassJson: `{"cpus": 2, "memory": "2Gi"}`,
		// contracts.VMImage marshals with capitalized field names (Path/Format).
		ImageJson: `{"Path":"/var/tmp/.virtrigaud-import-web-migrated.qcow2","Format":"qcow2"}`,
	}

	cfg, _, err := provider.parseCreateRequest(req)
	require.NoError(t, err)
	assert.Equal(t, "/var/tmp/.virtrigaud-import-web-migrated.qcow2", cfg.ImportedDiskPath)
	assert.Equal(t, "qcow2", cfg.ImportedDiskFormat)
	assert.Empty(t, cfg.Template, "an imported-disk create must not be treated as a template clone")
}

// TestParseCreateRequest_TemplateWinsOverPath verifies that when BOTH a
// TemplateName and a Path are present, the template path is taken (the imported
// disk capture is gated on an empty template), so a normal template create is
// never misrouted onto the importdisk path.
func TestParseCreateRequest_TemplateWinsOverPath(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)

	req := &providerv1.CreateRequest{
		Name:      "tmpl-vm",
		ImageJson: `{"TemplateName":"9000","Path":"/var/tmp/should-be-ignored.qcow2"}`,
	}
	cfg, _, err := provider.parseCreateRequest(req)
	require.NoError(t, err)
	assert.Equal(t, "9000", cfg.Template)
	assert.Empty(t, cfg.ImportedDiskPath, "template create must not capture the imported-disk path")
}
