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
	"log/slog"
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

// fakeQemuImgScript is a stand-in qemu-img: it records its argv and keeps the
// one "NFS object" as $FAKE_DIR/object. info answers its size, or libnfs's
// NFS3ERR_NOENT when it does not exist; create truncates it.
const fakeQemuImgScript = `#!/bin/sh
echo "$@" >> "$FAKE_DIR/calls"
f="$FAKE_DIR/object"
case "$1" in
info)
  if [ -f "$f" ]; then printf '{"virtual-size": %s}' "$(wc -c < "$f" | tr -d ' ')"; else
    echo "qemu-img: Could not open: Failed to open file : NFS: Lookup failed with NFS3ERR_NOENT(-2)" >&2; exit 1; fi ;;
create) : > "$f" ;;
*) exit 2 ;;
esac
`

// newStagedDiskProvider returns a Provider whose qemu-img is the fake, and the
// fake's directory.
func newStagedDiskProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "qemu-img")
	require.NoError(t, os.WriteFile(bin, []byte(fakeQemuImgScript), 0o700)) //nolint:gosec // test writes an executable fake
	t.Setenv("FAKE_DIR", dir)
	return &Provider{logger: slog.Default(), qemuImgPath: bin}, dir
}

func stagedReq(t *testing.T, backend string, opts migration.StorageOptions, creds map[string]string) *providerv1.DeleteStagedDiskRequest {
	t.Helper()
	js, err := migration.MarshalStorageOptions(opts)
	require.NoError(t, err)
	return &providerv1.DeleteStagedDiskRequest{
		Migration:          &providerv1.ObjectIdentity{Uid: "uid-1", Namespace: "team-a", Name: "web"},
		Stage:              migration.StagingStageExport,
		BackendType:        backend,
		StorageOptionsJson: js,
		Credentials:        creds,
	}
}

func TestDeleteStagedDisk_NFSEmptiesTheObjectThroughQemuImg(t *testing.T) {
	p, dir := newStagedDiskProvider(t)
	uid, gid := int64(2000), int64(2000)
	req := stagedReq(t, migration.BackendNFS, migration.StorageOptions{Backend: migration.BackendNFS,
		Server: "nfs.example", Export: "/exports", UID: &uid, GID: &gid}, nil)
	const wantURL = "nfs://nfs.example/exports/vmmigrations-team-a-web-export.qcow2?gid=2000&uid=2000"
	object := filepath.Join(dir, "object")
	require.NoError(t, os.WriteFile(object, []byte("tenant disk bytes"), 0o600))

	resp, err := p.DeleteStagedDisk(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_EMPTIED, resp.GetOutcome())
	data, err := os.ReadFile(object) //nolint:gosec // test reads its fixture
	require.NoError(t, err)
	assert.Empty(t, data, "the tenant's data is gone")

	calls, err := os.ReadFile(filepath.Join(dir, "calls")) //nolint:gosec // test reads its fixture
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	require.Len(t, lines, 2)
	assert.Equal(t, "info --output=json -f raw "+wantURL, lines[0])
	assert.Equal(t, "create -f raw "+wantURL+" 0", lines[1], "the migration's own object, with its uid/gid")

	// Again: already empty, nothing written.
	resp, err = p.DeleteStagedDisk(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_EMPTIED, resp.GetOutcome())

	// A missing object is not found, and is never created.
	require.NoError(t, os.Remove(object))
	resp, err = p.DeleteStagedDisk(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_NOT_FOUND, resp.GetOutcome())
	assert.NoFileExists(t, object)
}

func TestDeleteStagedDisk_RefusesWhatIsNotAMigration(t *testing.T) {
	p, dir := newStagedDiskProvider(t)
	req := stagedReq(t, migration.BackendNFS, migration.StorageOptions{Backend: migration.BackendNFS,
		Server: "nfs.example", Export: "/exports", Path: "../../etc"}, nil)
	_, err := p.DeleteStagedDisk(context.Background(), req)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	req = stagedReq(t, migration.BackendNFS, migration.StorageOptions{Backend: migration.BackendNFS,
		Server: "nfs.example", Export: "/exports"}, nil)
	req.Migration.Name = "web/../../x"
	_, err = p.DeleteStagedDisk(context.Background(), req)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.NoFileExists(t, filepath.Join(dir, "calls"), "qemu-img never ran")
}

func TestDeleteStagedDisk_S3(t *testing.T) {
	p, _ := newStagedDiskProvider(t)
	srv := s3fake.New("staging")
	defer srv.Close()
	srv.Put("vmmigrations/team-a/web/export.vmdk", []byte("disk"))
	srv.Put("vmmigrations/team-a/web2/export.vmdk", []byte("another migration"))
	creds := map[string]string{migration.CredKeyAccessKeyID: "AK", migration.CredKeySecretAccessKey: "SK"}
	req := stagedReq(t, migration.BackendS3, migration.StorageOptions{Backend: migration.BackendS3, Bucket: "staging",
		Endpoint: srv.Endpoint(), UsePathStyle: true}, creds)

	resp, err := p.DeleteStagedDisk(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_DELETED, resp.GetOutcome())
	assert.Equal(t, []string{"vmmigrations/team-a/web2/export.vmdk"}, srv.Keys())

	resp, err = p.DeleteStagedDisk(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_NOT_FOUND, resp.GetOutcome())
}

func TestCapabilities_AdvertisesStagedDiskDelete(t *testing.T) {
	p := &Provider{logger: slog.Default()}
	resp, err := p.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{migration.BackendNFS, migration.BackendS3}, resp.GetSupportedStagedDiskDeleteBackends())
}
