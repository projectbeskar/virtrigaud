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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestExportDiskToS3_NilProvider verifies the export path fails cleanly when no
// provider/virshProvider is wired rather than panicking. This fires before any
// host interaction, so it is host-independent.
func TestExportDiskToS3_NilProvider(t *testing.T) {
	s := &Server{}

	resp, err := s.exportDiskToS3(context.Background(), &providerv1.ExportDiskRequest{
		VmId:           "vm-1",
		DestinationUrl: "s3://bucket/disk.qcow2",
		BackendType:    "s3",
	})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "not initialized")
}

// TestExportDiskToS3_RequiresSSHTransport verifies the host-side flatten +
// stream-out flow refuses a non-ssh:// libvirt transport: the flatten
// (`qemu-img convert`) and the stream (`cat <hostTmp>`) both need an SSH
// connection to the libvirt host. The guard fires before any S3 client is built,
// so it is host-independent.
func TestExportDiskToS3_RequiresSSHTransport(t *testing.T) {
	vp := &VirshProvider{uri: "qemu:///system"}
	reg, err := hostconn.NewRegistry(newVirshConn("host-a", vp))
	require.NoError(t, err)
	s := &Server{
		provider: &Provider{registry: reg, hostID: "host-a"},
	}

	resp, err := s.exportDiskToS3(context.Background(), &providerv1.ExportDiskRequest{
		VmId:           "vm-1",
		DestinationUrl: "s3://bucket/disk.qcow2",
		BackendType:    "s3",
	})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "ssh://",
		"a local transport must be rejected: host-side flatten+stream needs SSH")
}

// TestHostExportStageTemplate verifies the transient flattened qcow2 is
// co-located with the source disk (same dir → same filesystem, so the flatten
// convert stays intra-device), is dot-prefixed (hidden from a casual directory
// listing), carries the VM id, and ends in the mktemp template (the random
// part and the .qcow2 suffix are added by mktemp).
func TestHostExportStageTemplate(t *testing.T) {
	const src = "/var/lib/libvirt/images/ubuntu-libvirt-demo.qcow2"
	const vm = "demo-ubuntu-libvirt"

	got := hostExportStageTemplate(src, vm)

	const dir = "/var/lib/libvirt/images"
	assert.Equal(t, dir, got[:strings.LastIndex(got, "/")],
		"stage file must be directly in the source dir, not a subdir; got %q", got)
	base := got[strings.LastIndex(got, "/")+1:]
	assert.True(t, strings.HasPrefix(base, ".virtrigaud-export-"),
		"stage file must be dot-prefixed and namespaced; got base %q", base)
	assert.Contains(t, base, vm, "stage file name must carry the VM id; got base %q", base)
	assert.True(t, strings.HasSuffix(base, "."+mktempTemplateSuffix), "a mktemp template; got base %q", base)
	assert.Equal(t, ".qcow2", hostExportStageSuffix, "the staged-object suffix")
}

// TestHostExportStageTemplate_SanitizedNameStaysContained verifies that a
// hostile VM id cannot escape the source directory: sanitizeVolumeName
// neutralizes path separators and "..", so the stage path stays directly
// inside the source dir.
func TestHostExportStageTemplate_SanitizedNameStaysContained(t *testing.T) {
	const src = "/var/lib/libvirt/images/disk0.qcow2"

	stage := hostExportStageTemplate(src, "../../etc/evil")

	assert.False(t, strings.Contains(stage, ".."),
		"sanitized stage path must not contain a parent-dir traversal; got %q", stage)
	assert.Equal(t, "/var/lib/libvirt/images", stage[:strings.LastIndex(stage, "/")],
		"stage file must stay directly inside the source dir; got %q", stage)
}

// TestFlattenForExport runs the export's flatten with the real commands
// (mktemp, qemu-img, rm): the staging file is made for this export alone
// (unpredictable name next to the source, distinct from it), private (0600),
// holds the flattened disk, and is removed by its cleanup — even when the
// request was cancelled; a failed flatten leaves nothing behind.
func TestFlattenForExport(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not available")
	}
	vp := NewVirshProvider(&ProviderConfig{})
	vp.uri = "test:///export"
	dir := t.TempDir()
	src := filepath.Join(dir, "disk0.qcow2")
	out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", src, "1M").CombinedOutput() //nolint:gosec // test-controlled paths
	require.NoError(t, err, "%s", out)
	staged := func() []string {
		m, err := filepath.Glob(filepath.Join(dir, ".virtrigaud-export-*"))
		require.NoError(t, err)
		return m
	}

	ctx, cancel := context.WithCancel(context.Background())
	tmp, cleanup, err := flattenForExport(ctx, vp, src, "vm-1")
	require.NoError(t, err)
	assert.Equal(t, dir, filepath.Dir(tmp))
	assert.NotEqual(t, src, tmp)
	assert.Regexp(t, `^\.virtrigaud-export-vm-1\.[A-Za-z0-9]{10}\.qcow2$`, filepath.Base(tmp))
	fi, err := os.Stat(tmp)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "a full copy of a VM's disk is private")
	info, err := exec.Command("qemu-img", "info", "--output=json", tmp).Output() //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Contains(t, string(info), `"format": "qcow2"`)

	tmp2, cleanup2, err := flattenForExport(ctx, vp, src, "vm-1")
	require.NoError(t, err)
	assert.NotEqual(t, tmp, tmp2, "one staging file per export")
	cleanup2()

	cancel() // the request is gone: the cleanup still runs
	cleanup()
	assert.Empty(t, staged(), "removed")

	_, _, err = flattenForExport(context.Background(), vp, filepath.Join(dir, "missing.qcow2"), "vm-1")
	require.Error(t, err)
	assert.Empty(t, staged(), "a failed flatten leaves nothing behind")
}

// TestStreamCmdQuotesPath verifies the export stream (conn.Stream(ctx, "cat",
// hostTmp)) reaches the remote shell with the host temp path shell-quoted, so a
// path with spaces (or shell metacharacters) is read from exactly the intended
// file. Stream flattens its argv with shellJoin.
func TestStreamCmdQuotesPath(t *testing.T) {
	hostTmp := "/var/lib/libvirt/images/.virtrigaud-export-my vm-1.qcow2"
	streamCmd, err := shellJoin([]string{"cat", hostTmp})
	require.NoError(t, err)

	assert.Equal(t, "cat '/var/lib/libvirt/images/.virtrigaud-export-my vm-1.qcow2'", streamCmd,
		"the cat source must be single-quoted so spaces don't split the path")
}
