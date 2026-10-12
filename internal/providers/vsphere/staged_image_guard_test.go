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
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	"github.com/projectbeskar/virtrigaud/internal/storage/s3fake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// minimalQCOW2 is the leading bytes of a self-contained qcow2 v2 image.
func minimalQCOW2() []byte {
	b := make([]byte, 4096)
	copy(b, "QFI\xfb")
	b[7] = 2   // version
	b[23] = 16 // cluster bits
	return b
}

// fakeImportQemuImg is a stand-in qemu-img for the NFS import: it records its
// argv; a raw->raw convert copies $FAKE_DIR/object to the destination; info
// prints $FAKE_DIR/info.json; any other convert (to vmdk) fails.
const fakeImportQemuImg = `#!/bin/sh
echo "$@" >> "$FAKE_DIR/calls"
case "$1" in
--version) echo "qemu-img version 7.2.0" ;;
info)
  case "$*" in
  *"-f raw"*) printf '{"virtual-size": %s}' "$(cat "$FAKE_DIR/size" 2>/dev/null || wc -c < "$FAKE_DIR/object" | tr -d ' ')" ;;
  *) cat "$FAKE_DIR/info.json" ;;
  esac ;;
dd) for a in "$@"; do case "$a" in of=*) out="${a#of=}" ;; esac; done
  cp "$FAKE_DIR/object" "$out"
  if [ -f "$FAKE_DIR/grow" ]; then truncate -s "$(cat "$FAKE_DIR/grow")" "$out"; fi ;;
convert) exit 1 ;;
*) exit 2 ;;
esac
`

func nfsImportRequest(t *testing.T) *providerv1.ImportDiskRequest {
	t.Helper()
	opts, err := migration.MarshalStorageOptions(migration.StorageOptions{Backend: migration.BackendNFS,
		Server: "nfs.example", Export: "/exports"})
	require.NoError(t, err)
	return &providerv1.ImportDiskRequest{
		SourceUrl:          "nfs://nfs.example/exports/vmmigrations_team-a_web_export.qcow2",
		BackendType:        migration.BackendNFS,
		StorageOptionsJson: opts,
		Format:             "qcow2",
		TargetName:         "web-migrated",
		TargetVm:           &providerv1.ObjectIdentity{Namespace: "team-a", Name: "web"},
	}
}

// newImportGuardProvider is a vcsim-backed Provider whose qemu-img is the fake
// with the given header.
func newImportGuardProvider(t *testing.T, infoJSON string) (*Provider, string) {
	t.Helper()
	p, _ := newOwnershipSim(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "qemu-img")
	require.NoError(t, os.WriteFile(bin, []byte(fakeImportQemuImg), 0o700)) //nolint:gosec // test writes an executable fake
	require.NoError(t, os.WriteFile(filepath.Join(dir, "object"), minimalQCOW2(), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "info.json"), []byte(infoJSON), 0o600))
	t.Setenv("FAKE_DIR", dir)
	p.qemuImgPath = bin
	return p, dir
}

func fakeCalls(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "calls")) //nolint:gosec // test reads its fixture
	require.NoError(t, err)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// TestNFSImport_RefusesAStagedImageThatReferencesOtherFiles (review High):
// the vSphere pod copies the staged object byte for byte (-f raw), checks the
// local copy's header, and refuses one that names a backing file, a data file
// or a json: target, before any conversion to vmdk.
func TestNFSImport_RefusesAStagedImageThatReferencesOtherFiles(t *testing.T) {
	for name, info := range map[string]string{
		"backing file to the SA token": `{"format":"qcow2","backing-filename":"/var/run/secrets/kubernetes.io/serviceaccount/token"}`,
		"json: backing":                `{"format":"qcow2","full-backing-filename":"json:{\"file.driver\":\"nbd\"}"}`,
		"external data file":           `{"format":"qcow2","format-specific":{"type":"qcow2","data":{"data-file":"/etc/virtrigaud/credentials/password"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			p, dir := newImportGuardProvider(t, info)
			_, err := p.ImportDisk(context.Background(), nfsImportRequest(t))
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
			calls := fakeCalls(t, dir)
			for _, c := range calls {
				assert.NotContains(t, c, "-O vmdk", "a refused image is never converted")
			}
			assert.Contains(t, strings.Join(calls, "\n"), "dd -f raw -O raw bs=1M count=1 if=nfs://nfs.example/exports/vmmigrations_team-a_web_export.qcow2",
				"the staged object is first copied byte for byte")
			assert.Contains(t, strings.Join(calls, "\n"), "info -U --output=json -f qcow2 -- /tmp/")
		})
	}
}

// TestNFSImport_ConvertsTheCheckedLocalCopy: a self-contained header passes;
// the vmdk convert reads the LOCAL copy as qcow2, never the NFS URL.
func TestNFSImport_ConvertsTheCheckedLocalCopy(t *testing.T) {
	p, dir := newImportGuardProvider(t, `{"format":"qcow2","virtual-size":1048576}`)
	_, err := p.ImportDisk(context.Background(), nfsImportRequest(t))
	require.Error(t, err, "the fake qemu-img cannot produce a vmdk")
	assert.NotEqual(t, codes.InvalidArgument, status.Code(err))
	var vmdkConvert string
	for _, c := range fakeCalls(t, dir) {
		if strings.Contains(c, "-O vmdk") {
			vmdkConvert = c
		}
	}
	require.NotEmpty(t, vmdkConvert, "the checked copy is converted")
	assert.Contains(t, vmdkConvert, "-f qcow2")
	assert.NotContains(t, vmdkConvert, "nfs://")
	assert.Contains(t, vmdkConvert, ".staged.qcow2")
}

// TestS3Import_RefusesAStagedImageThatReferencesOtherFiles: the same guard on
// the S3 import: the downloaded object's header is checked before conversion.
func TestS3Import_RefusesAStagedImageThatReferencesOtherFiles(t *testing.T) {
	p, dir := newImportGuardProvider(t, `{"format":"qcow2","backing-filename":"/etc/virtrigaud/credentials/password"}`)
	srv := s3fake.New("staging")
	defer srv.Close()
	srv.Put("vmmigrations/team-a/web/export.vmdk", minimalQCOW2())
	opts, err := migration.MarshalStorageOptions(migration.StorageOptions{Backend: migration.BackendS3, Bucket: "staging",
		Endpoint: srv.Endpoint(), UsePathStyle: true})
	require.NoError(t, err)
	_, err = p.ImportDisk(context.Background(), &providerv1.ImportDiskRequest{
		SourceUrl:          "s3://staging/vmmigrations/team-a/web/export.vmdk",
		BackendType:        migration.BackendS3,
		TransferMode:       migration.TransferModeRelay,
		StorageOptionsJson: opts,
		Credentials:        map[string]string{migration.CredKeyAccessKeyID: "AK", migration.CredKeySecretAccessKey: "SK"},
		Format:             "qcow2",
		TargetName:         "web-migrated",
		TargetVm:           &providerv1.ObjectIdentity{Namespace: "team-a", Name: "web"},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
	assert.Contains(t, strings.Join(fakeCalls(t, dir), "\n"), "info -U --output=json -f qcow2 -- /tmp/")
}

// TestNFSImport_ParsesTheHeaderBeforeQemuOpensIt (re-review Medium 2): the
// copy's bytes are parsed before qemu-img info opens it; a header naming an
// external data file is refused and qemu-img info never runs.
func TestNFSImport_ParsesTheHeaderBeforeQemuOpensIt(t *testing.T) {
	p, dir := newImportGuardProvider(t, `{"format":"qcow2"}`)
	head := minimalQCOW2()
	head[7] = 3     // version 3
	head[79] = 4    // incompatible features: external data file
	head[103] = 104 // header length
	require.NoError(t, os.WriteFile(filepath.Join(dir, "object"), head, 0o600))
	_, err := p.ImportDisk(context.Background(), nfsImportRequest(t))
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
	for _, c := range fakeCalls(t, dir) {
		assert.False(t, strings.HasPrefix(c, "info -U --output=json -f qcow2"), "qemu-img info never opens a refused header as qcow2")
	}
}

// TestNFSImport_IsBounded (re-review Medium 1): the copy into the pod's /tmp
// is bounded: an object larger than the limit, or one that does not fit twice
// in /tmp's free space, is refused before any copy (no dd); an object that
// grew past the limit during the copy is refused after it, and nothing is
// left in /tmp.
func TestNFSImport_IsBounded(t *testing.T) {
	t.Setenv(migration.EnvStagedImportMaxGiB, "1")
	for name, tc := range map[string]struct {
		size     string
		free     int64
		grow     bool
		wantCode codes.Code
		wantDD   bool
	}{
		"larger than the limit": {size: "1073741825", free: 1 << 40, wantCode: codes.InvalidArgument},
		"not enough free /tmp":  {size: "1048576", free: 1 << 20, wantCode: codes.ResourceExhausted},
		"grew during the copy":  {size: "4096", free: 1 << 40, grow: true, wantCode: codes.InvalidArgument, wantDD: true},
	} {
		t.Run(name, func(t *testing.T) {
			p, dir := newImportGuardProvider(t, `{"format":"qcow2"}`)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "size"), []byte(tc.size), 0o600))
			if tc.grow {
				// The fake dd extends its (sparse) copy to 1 GiB + 1 byte.
				require.NoError(t, os.WriteFile(filepath.Join(dir, "grow"), []byte("1073741825"), 0o600))
			}
			orig := tmpFreeBytes
			tmpFreeBytes = func(string) (int64, error) { return tc.free, nil }
			defer func() { tmpFreeBytes = orig }()

			_, err := p.ImportDisk(context.Background(), nfsImportRequest(t))
			require.Error(t, err)
			assert.Equal(t, tc.wantCode, status.Code(err), "got %v", err)
			assert.NotContains(t, status.Convert(err).Message(), "nfs.example")
			calls := strings.Join(fakeCalls(t, dir), "\n")
			assert.Equal(t, tc.wantDD, strings.Contains(calls, "dd -f raw -O raw bs=1M count=1 if=nfs://"),
				"the copy is bounded by the measured size (4096 bytes: one MiB block)")
			assert.NotContains(t, calls, "-O vmdk", "nothing is converted")
			matches, _ := filepath.Glob("/tmp/team-a_web-migrated*.staged.qcow2")
			assert.Empty(t, matches, "nothing is left in /tmp")
		})
	}
}

// TestPodTmpFreeBytes_BoundedByTheEmptyDirLimit (re-review N3): with the
// emptyDir limit in VIRTRIGAUD_TMP_SIZE_LIMIT, the space left is the limit
// minus what /tmp already holds — never the node's free space.
func TestPodTmpFreeBytes_BoundedByTheEmptyDirLimit(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "held"), make([]byte, 512<<10), 0o600))

	t.Setenv(migration.EnvTmpSizeLimit, "")
	node, err := podTmpFreeBytes(dir)
	require.NoError(t, err)

	t.Setenv(migration.EnvTmpSizeLimit, "1Mi")
	free, err := podTmpFreeBytes(dir)
	require.NoError(t, err)
	assert.LessOrEqual(t, free, int64(512<<10), "1 MiB limit minus the 512 KiB already held")
	assert.Less(t, free, node)

	t.Setenv(migration.EnvTmpSizeLimit, "256Ki")
	free, err = podTmpFreeBytes(dir)
	require.NoError(t, err)
	assert.Zero(t, free, "already over the limit: nothing left")

	t.Setenv(migration.EnvTmpSizeLimit, "lots")
	_, err = podTmpFreeBytes(dir)
	assert.Error(t, err)
}

// s3ImportRequest is a qcow2 S3 import of key from srv.
func s3ImportRequest(t *testing.T, srv *s3fake.Server, key string) *providerv1.ImportDiskRequest {
	t.Helper()
	opts, err := migration.MarshalStorageOptions(migration.StorageOptions{Backend: migration.BackendS3, Bucket: "staging",
		Endpoint: srv.Endpoint(), UsePathStyle: true})
	require.NoError(t, err)
	return &providerv1.ImportDiskRequest{
		SourceUrl: "s3://staging/" + key, BackendType: migration.BackendS3, TransferMode: migration.TransferModeRelay,
		StorageOptionsJson: opts, Format: "qcow2", TargetName: "web-migrated",
		Credentials: map[string]string{migration.CredKeyAccessKeyID: "AK", migration.CredKeySecretAccessKey: "SK"},
		TargetVm:    &providerv1.ObjectIdentity{Namespace: "team-a", Name: "web"},
	}
}

// TestS3Import_IsBounded (re-review N4b): the S3 download into the pod is
// bounded by the object's reported size and the import limit, and needs room
// for twice it in /tmp. An object above the limit or without room is refused
// before any GET; a backend that serves more than it reported is refused
// during the download. Nothing is inspected or converted.
func TestS3Import_IsBounded(t *testing.T) {
	const key = "vmmigrations/team-a/web/export.vmdk"
	t.Setenv(migration.EnvStagedImportMaxGiB, "1")
	for name, tc := range map[string]struct {
		reported int64
		free     int64
		wantCode codes.Code
		wantGET  bool
	}{
		"reported above the limit":     {reported: (1 << 30) + 1, free: 1 << 40, wantCode: codes.InvalidArgument},
		"no room in /tmp":              {reported: 4096, free: 4096, wantCode: codes.ResourceExhausted},
		"serves more than it reported": {reported: 100, free: 1 << 40, wantCode: codes.InvalidArgument, wantGET: true},
	} {
		t.Run(name, func(t *testing.T) {
			p, dir := newImportGuardProvider(t, `{"format":"qcow2"}`)
			srv := s3fake.New("staging")
			defer srv.Close()
			srv.Put(key, minimalQCOW2())
			srv.ReportSize(key, tc.reported)
			orig := tmpFreeBytes
			tmpFreeBytes = func(string) (int64, error) { return tc.free, nil }
			defer func() { tmpFreeBytes = orig }()

			_, err := p.ImportDisk(context.Background(), s3ImportRequest(t, srv, key))
			require.Error(t, err)
			assert.Equal(t, tc.wantCode, status.Code(err), "got %v", err)
			gets := 0
			for _, r := range srv.Requests() {
				if r.Method == "GET" {
					gets++
				}
			}
			assert.Equal(t, tc.wantGET, gets > 0)
			_, statErr := os.Stat(filepath.Join(dir, "calls"))
			assert.True(t, os.IsNotExist(statErr), "nothing is inspected or converted")
		})
	}
}
