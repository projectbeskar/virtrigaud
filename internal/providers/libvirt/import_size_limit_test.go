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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/importlimit"
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
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
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
