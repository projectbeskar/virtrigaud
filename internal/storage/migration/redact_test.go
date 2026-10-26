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

package migration

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	sdkerrors "github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

const (
	redactNFSURL  = "nfs://nfs.example.com/exports/tenant-a/sub/vmmigrations_ns_m_export.qcow2?gid=1000&uid=1000"
	redactNFSOpts = `{"backend":"nfs","server":"nfs.example.com","export":"/exports/tenant-a","path":"sub"}`
	redactS3URL   = "s3://tenant-bucket/pfx/vmmigrations/ns/m/export.vmdk"
	redactS3Opts  = `{"backend":"s3","bucket":"tenant-bucket","prefix":"pfx","endpoint":"https://s3.example.com:9000"}`
)

// nfsMountDenied is what a node-side mount and qemu-img print for a refused
// export: the server and export without a scheme, and the URL.
var nfsMountDenied = "mount.nfs: access denied by server while mounting nfs.example.com:/exports/tenant-a; " +
	"qemu-img: Could not open '" + redactNFSURL + "'"

func TestStagingLocations(t *testing.T) {
	nfs := StagingLocations(redactNFSURL, redactNFSOpts)
	for _, want := range []string{redactNFSURL, "nfs.example.com", "/exports/tenant-a/sub", "/exports/tenant-a", "/exports",
		"vmmigrations_ns_m_export.qcow2", "sub"} {
		assert.Contains(t, nfs, want)
	}
	s3 := StagingLocations(redactS3URL, redactS3Opts)
	for _, want := range []string{redactS3URL, "pfx/vmmigrations/ns/m/export.vmdk", "tenant-bucket", "pfx",
		"s3.example.com:9000", "s3.example.com"} {
		assert.Contains(t, s3, want)
	}
	assert.Contains(t, StagingLocations("s3://b/k", "not json"), "b/k", "an unknown bucket keeps the whole rest")
}

// TestRedactStagingError: the answer of a vSphere or Proxmox ExportDisk /
// ImportDisk keeps its code, retryability and details, and names no staging
// location in its status message — which is what crosses the wire and what
// the SDK's request log prints.
func TestRedactStagingError(t *testing.T) {
	locs := StagingLocations(redactNFSURL, redactNFSOpts)
	leaks := []string{"nfs.example.com", "/exports/tenant-a", "vmmigrations_ns_m_export", "uid=", "gid="}
	assertClean := func(t *testing.T, err error) {
		t.Helper()
		require.Error(t, err)
		for _, text := range []string{err.Error(), status.Convert(err).Message()} {
			for _, l := range leaks {
				assert.NotContains(t, text, l)
			}
		}
	}

	t.Run("an SDK ProviderError", func(t *testing.T) {
		pe := sdkerrors.NewInternal(fmt.Sprintf("node-side nfs-mount qemu-img export failed (stderr: %s)", nfsMountDenied),
			errors.New("exit status 32"))
		pe.Retryable = true
		got := RedactStagingError(pe, locs...)
		assertClean(t, got)
		var rpe *sdkerrors.ProviderError
		require.ErrorAs(t, got, &rpe)
		assert.Equal(t, codes.Internal, rpe.Code)
		assert.True(t, rpe.Retryable)
		assert.Contains(t, status.Convert(got).Message(), "access denied by server", "the cause stays readable")
		assert.Contains(t, pe.Message, "nfs.example.com", "the original is not changed")
	})
	t.Run("a gRPC status keeps its code and details", func(t *testing.T) {
		st, err := status.New(codes.Unavailable, nfsMountDenied).WithDetails(&errdetails.ErrorInfo{Reason: "VM_OPERATION_FAILED"})
		require.NoError(t, err)
		got := RedactStagingError(fmt.Errorf("wrapped: %w", st.Err()), locs...)
		assertClean(t, got)
		gst := status.Convert(got)
		assert.Equal(t, codes.Unavailable, gst.Code())
		require.Len(t, gst.Details(), 1)
	})
	t.Run("a plain error keeps its chain", func(t *testing.T) {
		base := errors.New("base")
		got := RedactStagingError(fmt.Errorf("%s: %w", nfsMountDenied, base), locs...)
		assertClean(t, got)
		assert.ErrorIs(t, got, base)
	})
	t.Run("an S3 error", func(t *testing.T) {
		got := RedactStagingError(sdkerrors.NewInternal("s3 upload failed during stream",
			errors.New(`Put "https://s3.example.com:9000/tenant-bucket/pfx/vmmigrations/ns/m/export.vmdk?uploads": `+
				`dial tcp s3.example.com:9000: connection refused`)), StagingLocations(redactS3URL, redactS3Opts)...)
		for _, l := range []string{"tenant-bucket", "s3.example.com", "pfx/vmmigrations"} {
			assert.NotContains(t, status.Convert(got).Message(), l)
		}
		assert.Contains(t, status.Convert(got).Message(), "connection refused")
	})
	assert.NoError(t, RedactStagingError(nil, locs...))
}
