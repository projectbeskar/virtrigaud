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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/importlimit"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// localQemuImgHost runs the "!" host commands it is given with the local
// qemu-img, recording them: a host whose staged images are real files.
type localQemuImgHost struct {
	calls []string
	fail  error
}

func (h *localQemuImgHost) runVirshCommand(ctx context.Context, args ...string) (*VirshResult, error) {
	h.calls = append(h.calls, strings.Join(args, " "))
	if h.fail != nil {
		return nil, h.fail
	}
	if len(args) < 2 || args[0] != "!" {
		return nil, fmt.Errorf("unexpected command %q", args)
	}
	out, err := exec.CommandContext(ctx, args[1], args[2:]...).Output() // #nosec G204 -- test runs qemu-img only
	if err != nil {
		return &VirshResult{ExitCode: qemuImgFailureExitCode}, err
	}
	return &VirshResult{Stdout: string(out)}, nil
}

// qcow2Image creates a sparse qcow2 of virtual size size (e.g. "2T").
func qcow2Image(t *testing.T, size string) string {
	t.Helper()
	// The package's host guard shims qemu-img even where it is missing
	// (exit 127), so ask the guard, not PATH.
	requireRealHostTool(t, "qemu-img")
	path := filepath.Join(t.TempDir(), "staged.qcow2")
	out, err := exec.Command("qemu-img", "create", "-f", "qcow2", path, size).CombinedOutput()
	require.NoError(t, err, string(out))
	return path
}

// TestCheckHostImportSize: an oversized staged disk (a 2 TiB sparse qcow2) is
// refused against a 100 GiB limit with FailedPrecondition and a constant
// message; one at the limit passes; with no limit nothing is even read; an
// unreadable size fails closed.
func TestCheckHostImportSize(t *testing.T) {
	ctx := context.Background()
	const limit = int64(100) << 30

	big := qcow2Image(t, "2T")
	h := &localQemuImgHost{}
	err := checkHostImportSize(ctx, h, big, "qcow2", limit)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Equal(t, importlimit.ExceedsMessage, st.Message())
	require.Len(t, h.calls, 1)
	assert.Contains(t, h.calls[0], "qemu-img info -f qcow2 --output=json -- "+big, "the format is pinned")

	h = &localQemuImgHost{}
	assert.NoError(t, checkHostImportSize(ctx, h, big, "qcow2", 0))
	assert.Empty(t, h.calls, "no limit: nothing is read")

	assert.NoError(t, checkHostImportSize(ctx, &localQemuImgHost{}, qcow2Image(t, "100G"), "qcow2", limit))

	err = checkHostImportSize(ctx, &localQemuImgHost{fail: errors.New("ssh: connection reset")}, big, "qcow2", limit)
	require.Error(t, err)
	assert.Equal(t, importlimit.UnverifiableMessage, err.Error(), "an unreadable size fails closed, without the host error")
}

// TestInspectHostImageInfoAs_ReportsVirtualSize: the header read the s3 and
// nfs imports already make also yields the virtual size they check.
func TestInspectHostImageInfoAs_ReportsVirtualSize(t *testing.T) {
	big := qcow2Image(t, "2T")
	h := &localQemuImgHost{}
	info, err := inspectHostImageInfoAs(context.Background(), h, importedImageSubject, big, "qcow2")
	require.NoError(t, err)
	assert.Equal(t, int64(2)<<40, info.VirtualSize)
	assert.Equal(t, "qcow2", info.Format)
	require.Error(t, importlimit.Check(info.VirtualSize, nil, int64(100)<<30))
	assert.Len(t, h.calls, 1, "one read serves both checks")
}

// TestPrivateLocalImportCopy (review M2): over a local connection the pvc
// import checks and lands a private copy of the staged disk, so replacing the
// staged file after the copy changes nothing; a symbolic link or a
// non-regular file is refused when the staged disk is opened
// (openStagedImportSource, staged_source_test.go).
func TestPrivateLocalImportCopy(t *testing.T) {
	staging := t.TempDir()
	src := filepath.Join(staging, "disk.qcow2")
	require.NoError(t, os.WriteFile(src, []byte("staged bytes"), 0o644))

	in, _, err := openStagedImportSource(stagedPVCURL(t, src))
	require.NoError(t, err)
	defer func() { _ = in.Close() }()
	path, cleanup, err := privateLocalImportCopy(in)
	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "staged bytes", string(got))
	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	di, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), di.Mode().Perm())

	// The tenant replaces the staged file: the private copy is unaffected.
	require.NoError(t, os.WriteFile(src, []byte("a much larger disk"), 0o644))
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "staged bytes", string(got))

	cleanup()
	_, err = os.Stat(filepath.Dir(path))
	assert.True(t, os.IsNotExist(err), "cleanup removes the copy and its directory")

	link := filepath.Join(staging, "link.qcow2")
	require.NoError(t, os.Symlink(src, link))
	_, _, err = openStagedImportSource(stagedPVCURL(t, link))
	assert.Equal(t, codes.FailedPrecondition, status.Code(err), "a symbolic link is not followed")
	sub := filepath.Join(staging, "sub")
	require.NoError(t, os.Mkdir(sub, 0o755))
	_, _, err = openStagedImportSource(stagedPVCURL(t, sub))
	assert.Equal(t, codes.FailedPrecondition, status.Code(err), "a directory is not a staged disk")
}

// TestServer_AdvertisesImportSizeLimit: a single-host provider, whose
// ImportDisk enforces max_virtual_size_bytes, advertises it; a clustered one
// (ImportDisk not routed) does not.
func TestServer_AdvertisesImportSizeLimit(t *testing.T) {
	caps, err := (&Server{}).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsImportSizeLimit())
	assert.True(t, clusteredCapabilities().GetSupportsImportSizeLimit(),
		"the routed import enforces the limit with the single-host checks (ADR-0007 Addendum B)")
}
