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

package proxmox

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	"github.com/projectbeskar/virtrigaud/internal/storage/s3fake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// stagedDeleteReq is a DeleteStagedDiskRequest for team-a/web's export.
func stagedDeleteReq(t *testing.T, ns, backend string, opts migration.StorageOptions, creds map[string]string) *providerv1.DeleteStagedDiskRequest {
	t.Helper()
	js, err := migration.MarshalStorageOptions(opts)
	require.NoError(t, err)
	return &providerv1.DeleteStagedDiskRequest{
		Migration:          &providerv1.ObjectIdentity{Uid: "uid-1", Namespace: ns, Name: "web"},
		Stage:              migration.StagingStageExport,
		BackendType:        backend,
		StorageOptionsJson: js,
		Credentials:        creds,
	}
}

// recordingNode records the scripts runSSH is asked to run and answers them
// with stdout / err.
type recordingNode struct {
	mu      sync.Mutex
	scripts []string
	stdout  string
	err     error
}

func (n *recordingNode) run(_ context.Context, cmd string) (string, string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.scripts = append(n.scripts, cmd)
	return n.stdout, "mount.nfs: access denied by server while mounting nfs.example:/exports", n.err
}

func nfsOpts() migration.StorageOptions {
	uid, gid := int64(2000), int64(2001)
	return migration.StorageOptions{Backend: migration.BackendNFS, Server: "nfs.example", Export: "/exports/mig",
		Path: "sub", UID: &uid, GID: &gid}
}

func TestDeleteStagedDisk_NFSUnlinksThroughTheNodeMount(t *testing.T) {
	node := &recordingNode{stdout: nfsStagedDeletedMarker + "\n"}
	p := &Provider{logger: slog.Default(), ssh: &sshTransport{host: "pve", logger: slog.Default(), runOverride: node.run}}

	resp, err := p.DeleteStagedDisk(context.Background(), stagedDeleteReq(t, "team-a", migration.BackendNFS, nfsOpts(), nil))
	require.NoError(t, err)
	assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_DELETED, resp.GetOutcome())

	require.Len(t, node.scripts, 1)
	script := node.scripts[0]
	assert.Contains(t, script, `mount -t nfs -o vers=3,nosymfollow,nosuid,nodev,noexec 'nfs.example:/exports/mig' "$MNT"`)
	assert.Contains(t, script, `setpriv --reuid 2000 --regid 2001 --clear-groups sh -c `)
	assert.Contains(t, script, `sh "$MNT"/'sub/vmmigrations-team-a-web-export.qcow2'`,
		"the file is the migration's own staged object under the export's path")
	assert.Contains(t, script, "trap 'umount", "the mount is always undone")

	node.stdout = nfsStagedAbsentMarker + "\n"
	resp, err = p.DeleteStagedDisk(context.Background(), stagedDeleteReq(t, "team-a", migration.BackendNFS, nfsOpts(), nil))
	require.NoError(t, err)
	assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_NOT_FOUND, resp.GetOutcome())
}

func TestDeleteStagedDisk_NFSFailures(t *testing.T) {
	t.Run("a node failure is retryable and names no location", func(t *testing.T) {
		node := &recordingNode{err: errors.New("exit status 32")}
		p := &Provider{logger: slog.Default(), ssh: &sshTransport{host: "pve", logger: slog.Default(), runOverride: node.run}}
		_, err := p.DeleteStagedDisk(context.Background(), stagedDeleteReq(t, "team-a", migration.BackendNFS, nfsOpts(), nil))
		require.Error(t, err)
		assert.Equal(t, codes.Unavailable, status.Code(err))
		msg := status.Convert(err).Message()
		assert.NotContains(t, msg, "nfs.example")
		assert.NotContains(t, msg, "/exports")
	})
	t.Run("no result printed is an error, not a success", func(t *testing.T) {
		node := &recordingNode{stdout: ""}
		p := &Provider{logger: slog.Default(), ssh: &sshTransport{host: "pve", logger: slog.Default(), runOverride: node.run}}
		_, err := p.DeleteStagedDisk(context.Background(), stagedDeleteReq(t, "team-a", migration.BackendNFS, nfsOpts(), nil))
		require.Error(t, err)
		assert.Equal(t, codes.Internal, status.Code(err))
	})
	t.Run("no SSH data plane", func(t *testing.T) {
		p := &Provider{logger: slog.Default()}
		_, err := p.DeleteStagedDisk(context.Background(), stagedDeleteReq(t, "team-a", migration.BackendNFS, nfsOpts(), nil))
		assert.Equal(t, codes.Unavailable, status.Code(err))
	})
	t.Run("a request that is not a migration never reaches the node", func(t *testing.T) {
		node := &recordingNode{stdout: nfsStagedDeletedMarker}
		p := &Provider{logger: slog.Default(), ssh: &sshTransport{host: "pve", logger: slog.Default(), runOverride: node.run}}
		for _, req := range []*providerv1.DeleteStagedDiskRequest{
			stagedDeleteReq(t, "../../etc", migration.BackendNFS, nfsOpts(), nil),
			stagedDeleteReq(t, "team-a", migration.BackendPVC, nfsOpts(), nil),
			stagedDeleteReq(t, "team-a", migration.BackendNFS, migration.StorageOptions{Backend: migration.BackendNFS,
				Server: "nfs.example", Export: "/exports", Path: "../.."}, nil),
		} {
			_, err := p.DeleteStagedDisk(context.Background(), req)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
		}
		assert.Empty(t, node.scripts)
	})
}

// runDeleteCmd runs nfsStagedDeleteCmd locally with $MNT set to mnt (no
// mount, no setpriv), returning stdout and the error.
func runDeleteCmd(t *testing.T, mnt, rel string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", nfsStagedDeleteCmd(rel, nil, nil)) //nolint:gosec // test runs the command under test
	cmd.Env = append(os.Environ(), "MNT="+mnt)
	out, err := cmd.Output()
	return string(out), err
}

// exitCodeOf is the exit status behind a command error, or -1.
func exitCodeOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// TestNFSStagedDeleteCmd_FileStillThereAfterRmIsAFailure (re-review Low e):
// an rm that reports success but leaves the file (a fake rm earlier in PATH,
// standing in for an NFS server that ignores the unlink) exits 5 and is never
// reported DELETED. Runs as any user.
func TestNFSStagedDeleteCmd_FileStillThereAfterRmIsAFailure(t *testing.T) {
	mnt := t.TempDir()
	own := filepath.Join(mnt, "vmmigrations_team-a_web_export.qcow2")
	require.NoError(t, os.WriteFile(own, []byte("disk"), 0o600))
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "rm"), []byte("#!/bin/sh\nexit 0\n"), 0o700))      //nolint:gosec // test writes an executable fake
	cmd := exec.Command("sh", "-c", nfsStagedDeleteCmd("vmmigrations_team-a_web_export.qcow2", nil, nil)) //nolint:gosec // test runs the command under test
	cmd.Env = append(os.Environ(), "MNT="+mnt, "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.Output()
	require.Error(t, err)
	assert.Equal(t, 5, exitCodeOf(err), "the file is still there: exit 5")
	assert.NotContains(t, string(out), nfsStagedDeletedMarker)
	assert.FileExists(t, own)
}

// TestNFSStagedDeleteCmd_RunsAsWritten runs the node-side command against a
// local directory standing in for the mounted export: it removes only the
// named file, removes a symbolic link and never its target, reports an absent
// file, and refuses (rather than reporting absent) when the directory cannot be
// searched.
func TestNFSStagedDeleteCmd_RunsAsWritten(t *testing.T) {
	mnt := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(mnt, "sub"), 0o755))
	own := filepath.Join(mnt, "sub", "vmmigrations-team-a-web-export.qcow2")
	other := filepath.Join(mnt, "sub", "vmmigrations-team-b-web-export.qcow2")
	require.NoError(t, os.WriteFile(own, []byte("disk"), 0o600))
	require.NoError(t, os.WriteFile(other, []byte("other tenant"), 0o600))

	out, err := runDeleteCmd(t, mnt, "sub/vmmigrations-team-a-web-export.qcow2")
	require.NoError(t, err)
	assert.Contains(t, out, nfsStagedDeletedMarker)
	assert.NoFileExists(t, own)
	assert.FileExists(t, other, "another tenant's file is untouched")

	out, err = runDeleteCmd(t, mnt, "sub/vmmigrations-team-a-web-export.qcow2")
	require.NoError(t, err)
	assert.Contains(t, out, nfsStagedAbsentMarker, "idempotent: a second delete finds nothing")

	// A symbolic link at the staging name is removed; its target stays.
	target := filepath.Join(t.TempDir(), "precious")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o600))
	require.NoError(t, os.Symlink(target, own))
	out, err = runDeleteCmd(t, mnt, "sub/vmmigrations-team-a-web-export.qcow2")
	require.NoError(t, err)
	assert.Contains(t, out, nfsStagedDeletedMarker)
	assert.NoFileExists(t, own)
	assert.FileExists(t, target, "rm removes the link, never what it points to")

	// A rel with shell metacharacters is an argument, never code.
	out, err = runDeleteCmd(t, mnt, `sub/$(touch pwned)`)
	require.NoError(t, err)
	assert.Contains(t, out, nfsStagedAbsentMarker)
	assert.NoFileExists(t, filepath.Join(mnt, "pwned"))

	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions; the permission cases need a non-root test user")
	}

	// A file in a directory that cannot be written: rm fails. It must be a
	// failure, never DELETED (review M1).
	readOnly := filepath.Join(mnt, "readonly")
	require.NoError(t, os.MkdirAll(readOnly, 0o755))
	ro := filepath.Join(readOnly, "vmmigrations_team-a_web_export.qcow2")
	require.NoError(t, os.WriteFile(ro, []byte("disk"), 0o600))
	require.NoError(t, os.Chmod(readOnly, 0o555))
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o755) }) //nolint:gosec // test restores its fixture
	out, err = runDeleteCmd(t, mnt, "readonly/vmmigrations_team-a_web_export.qcow2")
	require.Error(t, err, "a failed rm is a failure")
	assert.Equal(t, 4, exitCodeOf(err), "rm failed: exit 4")
	assert.NotContains(t, out, nfsStagedDeletedMarker)
	assert.FileExists(t, ro)

	locked := filepath.Join(mnt, "locked")
	require.NoError(t, os.MkdirAll(locked, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o600))
	require.NoError(t, os.Chmod(locked, 0o600))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) }) //nolint:gosec // test restores its fixture
	out, err = runDeleteCmd(t, mnt, "locked/f")
	require.Error(t, err, "an unsearchable directory is a failure, not an absent file")
	assert.False(t, strings.Contains(out, nfsStagedAbsentMarker))
}

func TestDeleteStagedDisk_S3RemovesOnlyTheMigrationsObject(t *testing.T) {
	srv := s3fake.New("staging")
	defer srv.Close()
	srv.Put("p/vmmigrations/team-a/web/export.vmdk", []byte("disk"))
	srv.Put("p/vmmigrations/team-b/web/export.vmdk", []byte("other tenant"))
	p := &Provider{logger: slog.Default()}

	opts := migration.StorageOptions{Backend: migration.BackendS3, Bucket: "staging", Endpoint: srv.Endpoint(),
		UsePathStyle: true, Prefix: "p"}
	creds := map[string]string{migration.CredKeyAccessKeyID: "AK", migration.CredKeySecretAccessKey: "SK"}
	resp, err := p.DeleteStagedDisk(context.Background(), stagedDeleteReq(t, "team-a", migration.BackendS3, opts, creds))
	require.NoError(t, err)
	assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_DELETED, resp.GetOutcome())
	assert.False(t, srv.Has("p/vmmigrations/team-a/web/export.vmdk"))
	assert.True(t, srv.Has("p/vmmigrations/team-b/web/export.vmdk"))
}

func TestCapabilities_StagedDiskDeleteBackends(t *testing.T) {
	resp, err := GetProviderCapabilities().GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{migration.BackendNFS, migration.BackendS3}, resp.GetSupportedStagedDiskDeleteBackends())
}
