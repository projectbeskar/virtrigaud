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
	s := &Server{
		provider: &Provider{
			virshProvider: &VirshProvider{uri: "qemu:///system"},
		},
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

// TestHostExportStagePath verifies the transient flattened qcow2 is co-located
// with the source disk (same dir → same filesystem, so the flatten convert stays
// intra-device), is dot-prefixed (hidden from a casual directory listing),
// carries the VM id, and has the .qcow2 suffix matching the staged/uploaded
// object's format.
func TestHostExportStagePath(t *testing.T) {
	const src = "/var/lib/libvirt/images/ubuntu-libvirt-demo.qcow2"
	const vm = "demo-ubuntu-libvirt"

	got := hostExportStagePath(src, vm)

	const dir = "/var/lib/libvirt/images"
	assert.True(t, strings.HasPrefix(got, dir+"/"),
		"stage file must live in the source dir for an intra-device convert; got %q", got)
	assert.Equal(t, dir, got[:strings.LastIndex(got, "/")],
		"stage file must be directly in the source dir, not a subdir; got %q", got)
	base := got[strings.LastIndex(got, "/")+1:]
	assert.True(t, strings.HasPrefix(base, ".virtrigaud-export-"),
		"stage file must be dot-prefixed and namespaced; got base %q", base)
	assert.Contains(t, base, vm, "stage file name must carry the VM id; got base %q", base)
	assert.True(t, strings.HasSuffix(base, ".qcow2"),
		"stage file keeps the staged-object .qcow2 suffix; got base %q", base)
}

// TestHostExportStagePath_DistinctFromSource verifies the flattened temp never
// collides with the source disk: they must be different files so cleanup of the
// temp cannot delete the source volume.
func TestHostExportStagePath_DistinctFromSource(t *testing.T) {
	const src = "/var/lib/libvirt/images/disk0.qcow2"
	const vm = "vm-1"

	stage := hostExportStagePath(src, vm)

	assert.NotEqual(t, src, stage, "stage and source must be distinct files")
	assert.True(t, strings.HasSuffix(stage, ".qcow2"))
}

// TestHostExportStagePath_SanitizedNameStaysContained verifies that a hostile VM
// id cannot escape the source directory: sanitizeVolumeName neutralizes path
// separators and "..", so the stage path stays directly inside the source dir.
func TestHostExportStagePath_SanitizedNameStaysContained(t *testing.T) {
	const src = "/var/lib/libvirt/images/disk0.qcow2"

	stage := hostExportStagePath(src, "../../etc/evil")

	assert.False(t, strings.Contains(stage, ".."),
		"sanitized stage path must not contain a parent-dir traversal; got %q", stage)
	assert.Equal(t, "/var/lib/libvirt/images", stage[:strings.LastIndex(stage, "/")],
		"stage file must stay directly inside the source dir; got %q", stage)
}

// TestStreamCmdQuotesPath verifies the stream command shell-quotes the host temp
// path so a path with spaces (or shell metacharacters) is read from exactly the
// intended file under the remote shell. The stream uses `cat <quoted>`.
func TestStreamCmdQuotesPath(t *testing.T) {
	hostTmp := "/var/lib/libvirt/images/.virtrigaud-export-my vm-1.qcow2"
	streamCmd := fmt.Sprintf("cat %s", shellQuote(hostTmp))

	assert.Equal(t, "cat '/var/lib/libvirt/images/.virtrigaud-export-my vm-1.qcow2'", streamCmd,
		"the cat source must be single-quoted so spaces don't split the path")
}
