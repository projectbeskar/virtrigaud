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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	"github.com/projectbeskar/virtrigaud/internal/storage/s3fake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// nfsObjectHost is a libvirt host whose qemu-img reaches one staged NFS
// object (nil: absent) and records every host command.
type nfsObjectHost struct {
	fakeSeamConn
	object   []byte
	commands [][]string
}

func (h *nfsObjectHost) RunHost(_ context.Context, argv ...string) (*hostconn.Result, error) {
	h.commands = append(h.commands, argv)
	if len(argv) < 2 || argv[0] != "qemu-img" {
		return nil, errors.New("unexpected host command")
	}
	switch argv[1] {
	case "info":
		if h.object == nil {
			return &hostconn.Result{ExitCode: 1, Stderr: "qemu-img: Could not open: Failed to open file : NFS3ERR_NOENT(-2)"},
				errors.New("exit status 1")
		}
		return &hostconn.Result{Stdout: fmt.Sprintf(`{"virtual-size": %d}`, len(h.object))}, nil
	case "create":
		h.object = []byte{}
		return &hostconn.Result{}, nil
	}
	return nil, errors.New("unexpected qemu-img command")
}

func stagedDeleteRequest(t *testing.T, backend string, opts migration.StorageOptions, creds map[string]string) *providerv1.DeleteStagedDiskRequest {
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

func TestServer_DeleteStagedDisk_NFSEmptiesOnTheHost(t *testing.T) {
	host := &nfsObjectHost{object: []byte("tenant disk")}
	s := &Server{provider: &fakeSeamProvider{hostConn: host}}
	uid, gid := int64(3000), int64(3000)
	req := stagedDeleteRequest(t, migration.BackendNFS, migration.StorageOptions{Backend: migration.BackendNFS,
		Server: "nfs.example", Export: "/exports", UID: &uid, GID: &gid}, nil)
	const wantURL = "nfs://nfs.example/exports/vmmigrations-team-a-web-export.qcow2?gid=3000&uid=3000"

	resp, err := s.DeleteStagedDisk(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_EMPTIED, resp.GetOutcome())
	assert.Empty(t, host.object, "the tenant's data is gone")
	require.Len(t, host.commands, 2)
	assert.Equal(t, []string{"qemu-img", "info", "--output=json", "-f", "raw", wantURL}, host.commands[0])
	assert.Equal(t, []string{"qemu-img", "create", "-f", "raw", wantURL, "0"}, host.commands[1])

	host.object = nil
	resp, err = s.DeleteStagedDisk(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_NOT_FOUND, resp.GetOutcome())
	assert.Nil(t, host.object, "a missing object is never created")
}

func TestServer_DeleteStagedDisk_NFSNeedsTheSSHTransport(t *testing.T) {
	host := &nfsObjectHost{fakeSeamConn: fakeSeamConn{uriVal: "qemu:///system"}, object: []byte("x")}
	s := &Server{provider: &fakeSeamProvider{hostConn: host}}
	_, err := s.DeleteStagedDisk(context.Background(), stagedDeleteRequest(t, migration.BackendNFS,
		migration.StorageOptions{Backend: migration.BackendNFS, Server: "nfs.example", Export: "/exports"}, nil))
	assert.Equal(t, codes.Unimplemented, status.Code(err))
	assert.Empty(t, host.commands)
}

func TestServer_DeleteStagedDisk_RefusesWhatIsNotAMigration(t *testing.T) {
	host := &nfsObjectHost{object: []byte("x")}
	s := &Server{provider: &fakeSeamProvider{hostConn: host}}
	req := stagedDeleteRequest(t, migration.BackendNFS, migration.StorageOptions{Backend: migration.BackendNFS,
		Server: "nfs.example", Export: "/exports"}, nil)
	req.Migration.Namespace = "team-a/../../etc"
	_, err := s.DeleteStagedDisk(context.Background(), req)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Empty(t, host.commands, "no host command runs for a refused request")
}

func TestServer_DeleteStagedDisk_S3(t *testing.T) {
	srv := s3fake.New("staging")
	defer srv.Close()
	srv.Put("vmmigrations/team-a/web/export.vmdk", []byte("disk"))
	srv.Put("vmmigrations/team-b/web/export.vmdk", []byte("other"))
	s := &Server{provider: &fakeSeamProvider{hostConn: &nfsObjectHost{}}}
	creds := map[string]string{migration.CredKeyAccessKeyID: "AK", migration.CredKeySecretAccessKey: "SK"}

	resp, err := s.DeleteStagedDisk(context.Background(), stagedDeleteRequest(t, migration.BackendS3,
		migration.StorageOptions{Backend: migration.BackendS3, Bucket: "staging", Endpoint: srv.Endpoint(), UsePathStyle: true}, creds))
	require.NoError(t, err)
	assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_DELETED, resp.GetOutcome())
	assert.Equal(t, []string{"vmmigrations/team-b/web/export.vmdk"}, srv.Keys())
}

// TestCapabilities_StagedDiskDeleteIsSingleHostOnly: a single-host provider
// advertises nfs and s3; a clustered provider advertises none and answers
// Unimplemented.
func TestCapabilities_StagedDiskDeleteBackends(t *testing.T) {
	single := &Provider{virshProvider: localHostVP("single")}
	caps, err := NewServer(single).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{migration.BackendNFS, migration.BackendS3}, caps.GetSupportedStagedDiskDeleteBackends())

	// ADR-0007 Addendum B: a clustered provider removes s3 objects from the
	// pod and empties nfs ones on the import's host.
	clustered := NewServer(clusterOf(t, []string{"host-a"}))
	caps, err = clustered.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{migration.BackendNFS, migration.BackendS3}, caps.GetSupportedStagedDiskDeleteBackends())
	_, err = clustered.DeleteStagedDisk(context.Background(), stagedDeleteRequest(t, migration.BackendNFS,
		migration.StorageOptions{Backend: migration.BackendNFS, Server: "nfs.example", Export: "/exports"}, nil))
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "an nfs delete needs the import's host: %v", err)
}
