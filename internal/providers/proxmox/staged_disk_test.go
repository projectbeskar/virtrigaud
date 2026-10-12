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

// stagedDeleteNode records the scripts runSSH is asked to run and answers them
// with stdout / err.
type stagedDeleteNode struct {
	mu      sync.Mutex
	scripts []string
	stdout  string
	err     error
}

func (n *stagedDeleteNode) run(_ context.Context, cmd string) (string, string, error) {
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
	node := &stagedDeleteNode{stdout: nfsStagedDeletedMarker + "\n"}
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
		node := &stagedDeleteNode{err: errors.New("exit status 32")}
		p := &Provider{logger: slog.Default(), ssh: &sshTransport{host: "pve", logger: slog.Default(), runOverride: node.run}}
		_, err := p.DeleteStagedDisk(context.Background(), stagedDeleteReq(t, "team-a", migration.BackendNFS, nfsOpts(), nil))
		require.Error(t, err)
		assert.Equal(t, codes.Unavailable, status.Code(err))
		msg := status.Convert(err).Message()
		assert.NotContains(t, msg, "nfs.example")
		assert.NotContains(t, msg, "/exports")
	})
	t.Run("no result printed is an error, not a success", func(t *testing.T) {
		node := &stagedDeleteNode{stdout: ""}
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
		node := &stagedDeleteNode{stdout: nfsStagedDeletedMarker}
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

// TestDeleteStagedDisk_RemovesTheTargetVMsImportStage (rebase item 3): with
// target_vm the node-side import stage of that VM — in every stage format,
// only paths isImportStagePath accepts — is removed with `rm -f --` BEFORE the
// staged object; a removal that fails is retryable and the object is not
// touched; a target_vm that cannot name a stage is refused before anything
// runs; without target_vm no stage is touched; without the SSH data plane
// (an s3-only setup) nothing can have been staged and the object is removed.
func TestDeleteStagedDisk_RemovesTheTargetVMsImportStage(t *testing.T) {
	withTarget := func(ns, name string) *providerv1.DeleteStagedDiskRequest {
		req := stagedDeleteReq(t, "team-a", migration.BackendNFS, nfsOpts(), nil)
		req.TargetVm = &providerv1.ObjectIdentity{Namespace: ns, Name: name}
		return req
	}
	newProvider := func(node *stagedDeleteNode) *Provider {
		return &Provider{logger: slog.Default(), ssh: &sshTransport{host: "pve", logger: slog.Default(), runOverride: node.run}}
	}

	t.Run("removed first, every format", func(t *testing.T) {
		node := &stagedDeleteNode{stdout: nfsStagedDeletedMarker + "\n"}
		resp, err := newProvider(node).DeleteStagedDisk(context.Background(), withTarget("team-a", "web"))
		require.NoError(t, err)
		assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_DELETED, resp.GetOutcome())
		require.Len(t, node.scripts, 2)
		assert.Equal(t, "rm -f -- '/var/lib/virtrigaud/import-stage/team-a_web-migrated.qcow2' "+
			"'/var/lib/virtrigaud/import-stage/team-a_web-migrated.raw' "+
			"'/var/lib/virtrigaud/import-stage/team-a_web-migrated.vmdk'", node.scripts[0])
		assert.Contains(t, node.scripts[1], "mount -t nfs ", "then the staged object")
	})
	t.Run("a failed removal is retryable and the object is kept", func(t *testing.T) {
		node := &stagedDeleteNode{err: errors.New("exit status 1")}
		_, err := newProvider(node).DeleteStagedDisk(context.Background(), withTarget("team-a", "web"))
		assert.Equal(t, codes.Unavailable, status.Code(err))
		assert.NotContains(t, status.Convert(err).Message(), "/var/lib", "the status names no path")
		require.Len(t, node.scripts, 1, "the staged object is not touched")
	})
	for name, tv := range map[string][2]string{
		"traversal name":       {"team-a", "../../etc"},
		"namespace with _":     {"team_a", "web"},
		"upper-case namespace": {"Team-A", "web"},
		"name only":            {"", "web"},
		"namespace only":       {"team-a", ""},
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			node := &stagedDeleteNode{stdout: nfsStagedDeletedMarker + "\n"}
			_, err := newProvider(node).DeleteStagedDisk(context.Background(), withTarget(tv[0], tv[1]))
			assert.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
			assert.Empty(t, node.scripts, "nothing runs on the node")
		})
	}
	t.Run("no target: no stage is touched", func(t *testing.T) {
		node := &stagedDeleteNode{stdout: nfsStagedDeletedMarker + "\n"}
		_, err := newProvider(node).DeleteStagedDisk(context.Background(), stagedDeleteReq(t, "team-a", migration.BackendNFS, nfsOpts(), nil))
		require.NoError(t, err)
		require.Len(t, node.scripts, 1)
		assert.NotContains(t, node.scripts[0], "import-stage")
	})
	t.Run("s3 without the SSH data plane", func(t *testing.T) {
		srv := s3fake.New("staging")
		defer srv.Close()
		srv.Put("p/vmmigrations/team-a/web/export.vmdk", []byte("disk"))
		opts := migration.StorageOptions{Backend: migration.BackendS3, Bucket: "staging", Endpoint: srv.Endpoint(),
			UsePathStyle: true, Prefix: "p"}
		req := stagedDeleteReq(t, "team-a", migration.BackendS3, opts,
			map[string]string{migration.CredKeyAccessKeyID: "AK", migration.CredKeySecretAccessKey: "SK"})
		req.TargetVm = &providerv1.ObjectIdentity{Namespace: "team-a", Name: "web"}
		resp, err := (&Provider{logger: slog.Default()}).DeleteStagedDisk(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_DELETED, resp.GetOutcome())
	})
}

func TestCapabilities_StagedDiskDeleteBackends(t *testing.T) {
	resp, err := GetProviderCapabilities().GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{migration.BackendNFS, migration.BackendS3}, resp.GetSupportedStagedDiskDeleteBackends())
}
