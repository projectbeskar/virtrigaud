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
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestImportDiskFromS3_NilProvider verifies the import path fails cleanly when no
// provider/virshProvider is wired rather than panicking. This fires before any
// host interaction, so it is host-independent.
func TestImportDiskFromS3_NilProvider(t *testing.T) {
	s := &Server{}

	resp, err := s.importDiskFromS3(context.Background(), &providerv1.ImportDiskRequest{
		SourceUrl:   "s3://bucket/disk.vmdk",
		BackendType: "s3",
		TargetName:  "imported",
	})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "not initialized")
}

// TestImportDiskFromS3_RequiresSSHTransport verifies the relay-to-host staging
// flow refuses a non-ssh:// libvirt transport: the stage (`cat > file`) and the
// host-side qemu-img convert both need an SSH connection to the libvirt host. The
// guard fires before any S3 client is built, so it is host-independent.
func TestImportDiskFromS3_RequiresSSHTransport(t *testing.T) {
	s := &Server{
		provider: &Provider{
			virshProvider: &VirshProvider{uri: "qemu:///system"},
		},
	}

	resp, err := s.importDiskFromS3(context.Background(), &providerv1.ImportDiskRequest{
		SourceUrl:   "s3://bucket/disk.vmdk",
		BackendType: "s3",
		TargetName:  "imported",
	})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "ssh://",
		"a local transport must be rejected: host-side stage+convert needs SSH")
}

// TestHostStagePath verifies the transient staging file is co-located with the
// target (same pool dir → same filesystem, so qemu-img convert stays
// intra-device), is dot-prefixed (hidden from a casual pool listing), carries the
// volume name, and keeps the .vmdk suffix matching the staged object's format.
func TestHostStagePath(t *testing.T) {
	const pool = "/var/lib/libvirt/images"
	const vol = "imported-disk"

	got := hostStagePath(pool, vol)

	assert.True(t, strings.HasPrefix(got, pool+"/"),
		"stage file must live in the pool directory for an intra-device convert; got %q", got)
	assert.Equal(t, pool, got[:strings.LastIndex(got, "/")],
		"stage file must be directly in the pool dir, not a subdir; got %q", got)
	base := got[strings.LastIndex(got, "/")+1:]
	assert.True(t, strings.HasPrefix(base, ".virtrigaud-import-"),
		"stage file must be dot-prefixed and namespaced; got base %q", base)
	assert.Contains(t, base, vol, "stage file name must carry the volume name; got base %q", base)
	assert.True(t, strings.HasSuffix(base, ".vmdk"),
		"stage file keeps the pre-conversion .vmdk suffix; got base %q", base)
}

// TestHostStagePath_TrailingSlashNormalized verifies a pool path with a trailing
// slash does not produce a doubled separator (so the path stays valid on the
// host).
func TestHostStagePath_TrailingSlashNormalized(t *testing.T) {
	got := hostStagePath("/pool/", "v1")
	assert.False(t, strings.Contains(got, "//"),
		"trailing slash on the pool path must be normalized; got %q", got)
	assert.True(t, strings.HasPrefix(got, "/pool/.virtrigaud-import-v1-"), "got %q", got)
}

// TestHostStagePath_DistinctFromTarget verifies the stage path never collides
// with the converted target (<pool>/<vol>.qcow2): they must be different files so
// cleanup of the stage cannot delete the imported volume.
func TestHostStagePath_DistinctFromTarget(t *testing.T) {
	const pool = "/var/lib/libvirt/images"
	const vol = "disk0"
	target := fmt.Sprintf("%s/%s.qcow2", pool, vol)

	stage := hostStagePath(pool, vol)

	assert.NotEqual(t, target, stage, "stage and target must be distinct files")
	assert.True(t, strings.HasSuffix(target, ".qcow2"))
	assert.True(t, strings.HasSuffix(stage, ".vmdk"))
}

// TestHostStagePath_SanitizedNameStaysContained verifies that a volume name which
// has already been run through sanitizeVolumeName (path separators / ".." removed)
// produces a stage path that cannot escape the pool directory. This is the same
// sanitize the import path applies to req.TargetName before building both the
// stage and target paths.
func TestHostStagePath_SanitizedNameStaysContained(t *testing.T) {
	const pool = "/var/lib/libvirt/images"
	// A hostile target name; sanitizeVolumeName neutralizes separators and "..".
	vol := sanitizeVolumeName("../../etc/evil")

	stage := hostStagePath(pool, vol)

	assert.False(t, strings.Contains(stage, ".."),
		"sanitized stage path must not contain a parent-dir traversal; got %q", stage)
	assert.Equal(t, pool, stage[:strings.LastIndex(stage, "/")],
		"stage file must stay directly inside the pool dir; got %q", stage)
}

// TestStageCmdQuotesPath verifies the staging command shell-quotes the host temp
// path so a path with spaces (or shell metacharacters) is written to exactly the
// intended file under the remote shell. The stage uses `cat > <quoted>`.
func TestStageCmdQuotesPath(t *testing.T) {
	stagePath := "/var/lib/libvirt/images/.virtrigaud-import-my disk-1.vmdk"
	stageCmd := fmt.Sprintf("cat > %s", shellQuote(stagePath))

	assert.Equal(t, "cat > '/var/lib/libvirt/images/.virtrigaud-import-my disk-1.vmdk'", stageCmd,
		"the stage redirect target must be single-quoted so spaces don't split the path")
}

// TestQemuImgStderr verifies the qemu-img stderr is surfaced (so the real cause
// is visible, not masked by an io.Pipe error) and that the empty cases stay tidy.
func TestQemuImgStderr(t *testing.T) {
	assert.Equal(t, "", qemuImgStderr(nil), "nil result yields no suffix")
	assert.Equal(t, "", qemuImgStderr(&VirshResult{Stderr: "   "}),
		"whitespace-only stderr yields no suffix")

	got := qemuImgStderr(&VirshResult{
		Stderr: "qemu-img: Could not open '/dev/stdin': 'file' driver requires '/dev/stdin' to be a regular file\n",
	})
	assert.Equal(t,
		" (qemu-img stderr: qemu-img: Could not open '/dev/stdin': 'file' driver requires '/dev/stdin' to be a regular file)",
		got, "the real qemu-img message must be surfaced, trimmed")
}
