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

package importlimit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// reasonOf returns the ErrorInfo reason of err's gRPC status (through any
// %w wrapping), or "".
func reasonOf(t *testing.T, err error) string {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok, "a gRPC status: %v", err)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			assert.Equal(t, contracts.ErrorInfoDomain, info.GetDomain())
			return info.GetReason()
		}
	}
	return ""
}

func TestCheck(t *testing.T) {
	const gib = int64(1) << 30
	assert.NoError(t, Check(64<<40, nil, 0), "no limit")
	assert.NoError(t, Check(0, errors.New("unreadable"), 0), "no limit: nothing to verify")
	assert.NoError(t, Check(100*gib, nil, 100*gib), "at the limit")

	err := Check(100*gib+1, nil, 100*gib)
	require.Error(t, err)
	assert.Equal(t, ExceedsMessage, err.Error())
	assert.Equal(t, contracts.DiskSizeExceedsLimitReason, reasonOf(t, fmt.Errorf("import: %w", err)), "survives wrapping")

	for _, unknown := range []error{Check(0, nil, gib), Check(10, errors.New("boom"), gib)} {
		require.Error(t, unknown)
		assert.Equal(t, UnverifiableMessage, unknown.Error())
		assert.Equal(t, contracts.DiskSizeUnverifiableReason, reasonOf(t, unknown))
	}
}

func TestQemuImgVirtualSize(t *testing.T) {
	n, err := QemuImgVirtualSize(`{"virtual-size": 107374182400, "format": "qcow2"}`)
	require.NoError(t, err)
	assert.Equal(t, int64(100)<<30, n)
	for _, bad := range []string{``, `{}`, `{"virtual-size": 0}`, `not json`} {
		_, err := QemuImgVirtualSize(bad)
		assert.Error(t, err, bad)
	}
}

// TestLocalVirtualSize reads a real qcow2 header when qemu-img is installed.
func TestLocalVirtualSize(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	path := filepath.Join(t.TempDir(), "big.qcow2")
	require.NoError(t, exec.Command("qemu-img", "create", "-f", "qcow2", path, "2T").Run())
	n, err := LocalVirtualSize(context.Background(), path, "qcow2")
	require.NoError(t, err)
	assert.Equal(t, int64(2)<<40, n)
	_, err = LocalVirtualSize(context.Background(), filepath.Join(t.TempDir(), "missing"), "qcow2")
	assert.Error(t, err)
	require.NoError(t, os.WriteFile(path, []byte("not an image"), 0o600))
	_, err = LocalVirtualSize(context.Background(), path, "qcow2")
	assert.Error(t, err, "a file that is not a qcow2 is not read as one")
}
