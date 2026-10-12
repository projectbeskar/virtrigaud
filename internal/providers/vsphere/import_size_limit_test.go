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

package vsphere

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/importlimit"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// streamOptimizedVMDK creates a sparse streamOptimized vmdk of virtual size
// size (a qemu-img size such as "2T") — the shape every vSphere import lands.
func streamOptimizedVMDK(t *testing.T, size string) string {
	t.Helper()
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	path := filepath.Join(t.TempDir(), "disk.vmdk")
	out, err := exec.Command("qemu-img", "create", "-f", "vmdk", "-o", "subformat=streamOptimized", path, size).CombinedOutput()
	require.NoError(t, err, string(out))
	return path
}

// TestCheckLandingVirtualSize: an oversized staged disk (a 2 TiB sparse image
// that is only a few KiB on disk) is refused against a 100 GiB limit with
// FailedPrecondition / DISK_SIZE_EXCEEDS_LIMIT; one at the limit, or any disk
// without a limit, passes; a file that cannot be read as vmdk fails closed.
func TestCheckLandingVirtualSize(t *testing.T) {
	ctx := context.Background()
	const limit = int64(100) << 30

	big := streamOptimizedVMDK(t, "2T")
	err := checkLandingVirtualSize(ctx, big, limit)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Equal(t, importlimit.ExceedsMessage, st.Message())
	assert.NoError(t, checkLandingVirtualSize(ctx, big, 0), "no limit: nothing is read or refused")

	fits := streamOptimizedVMDK(t, "100G")
	assert.NoError(t, checkLandingVirtualSize(ctx, fits, limit))

	garbage := filepath.Join(t.TempDir(), "garbage.vmdk")
	require.NoError(t, os.WriteFile(garbage, []byte("not a vmdk"), 0o600))
	err = checkLandingVirtualSize(ctx, garbage, limit)
	require.Error(t, err)
	assert.Equal(t, importlimit.UnverifiableMessage, err.Error(), "an unreadable size fails closed")
}

// TestGetCapabilities_AdvertisesImportSizeLimit: vSphere reports that
// ImportDisk enforces max_virtual_size_bytes.
func TestGetCapabilities_AdvertisesImportSizeLimit(t *testing.T) {
	caps, err := (&Provider{}).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsImportSizeLimit())
}

// TestRefuseOversizedBeforeConvert: the early check refuses a staged qcow2
// already known to be above the limit, before any convert; a size it cannot
// read is left to the authoritative check on the converted vmdk.
func TestRefuseOversizedBeforeConvert(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	ctx := context.Background()
	const limit = int64(100) << 30
	big := filepath.Join(t.TempDir(), "staged.qcow2")
	out, err := exec.Command("qemu-img", "create", "-f", "qcow2", big, "2T").CombinedOutput()
	require.NoError(t, err, string(out))

	err = refuseOversizedBeforeConvert(ctx, big, "qcow2", limit)
	require.Error(t, err)
	assert.Equal(t, importlimit.ExceedsMessage, err.Error())
	assert.NoError(t, refuseOversizedBeforeConvert(ctx, big, "qcow2", 0))

	garbage := filepath.Join(t.TempDir(), "garbage.qcow2")
	require.NoError(t, os.WriteFile(garbage, []byte("x"), 0o600))
	assert.NoError(t, refuseOversizedBeforeConvert(ctx, garbage, "qcow2", limit), "left to the landing check")
}
