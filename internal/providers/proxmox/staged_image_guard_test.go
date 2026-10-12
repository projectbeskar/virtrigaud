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
	"encoding/base64"
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
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// importNode answers the node commands of an NFS import: the mount+copy
// script, the header inspection (with infoOut), the publish and the check.
type importNode struct {
	mu        sync.Mutex
	t         *testing.T
	infoOut   string
	infoExit  int
	head      []byte
	fetchExit int
	commands  []string
}

// minimalQCOW2 is the leading bytes of a self-contained qcow2 v2 image:
// magic, version 2, 64 KiB clusters, no backing file, no extensions.
func minimalQCOW2() []byte {
	b := make([]byte, 4096)
	copy(b, "QFI\xfb")
	b[7] = 2   // version
	b[23] = 16 // cluster bits
	return b
}

func (n *importNode) run(_ context.Context, cmd string) (string, string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.commands = append(n.commands, cmd)
	if n.fetchExit != 0 && strings.HasPrefix(cmd, "set -e; MNT=") {
		msg := migration.ErrStagedObjectTooLarge.Error()
		if n.fetchExit == nfsNoSpaceExit {
			msg = migration.ErrStagingSpaceExhausted.Error()
		}
		return "", msg, exitErr(n.t, n.fetchExit)
	}
	if strings.HasPrefix(cmd, "head -c ") {
		head := n.head
		if head == nil {
			head = minimalQCOW2()
		}
		return base64.StdEncoding.EncodeToString(head), "", nil
	}
	if strings.HasPrefix(cmd, "unshare -n -- qemu-img info") {
		if n.infoExit != 0 {
			return "", "qemu-img: Could not open", exitErr(n.t, n.infoExit)
		}
		return n.infoOut, "", nil
	}
	return "", "", nil
}

func (n *importNode) ran(prefix string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, c := range n.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func nfsImportReq(t *testing.T) *providerv1.ImportDiskRequest {
	t.Helper()
	uid, gid := int64(2000), int64(2000)
	opts, err := migration.MarshalStorageOptions(migration.StorageOptions{Backend: migration.BackendNFS,
		Server: "nfs.example", Export: "/exports", UID: &uid, GID: &gid})
	require.NoError(t, err)
	return &providerv1.ImportDiskRequest{
		SourceUrl:          "nfs://nfs.example/exports/vmmigrations_team-a_web_export.qcow2?gid=2000&uid=2000",
		BackendType:        migration.BackendNFS,
		StorageOptionsJson: opts,
		Format:             "qcow2",
		TargetName:         "web-migrated",
	}
}

// TestNFSImport_RefusesAStagedImageThatReferencesOtherFiles (review High):
// a staged qcow2 whose header names a backing file, an external data file, or
// that is not qcow2, is refused (InvalidArgument) before anything converts or
// publishes it, and its node-local copy is removed.
func TestNFSImport_RefusesAStagedImageThatReferencesOtherFiles(t *testing.T) {
	for name, info := range map[string]string{
		"backing file to a node secret": `{"format":"qcow2","backing-filename":"/etc/pve/priv/authkey.key"}`,
		"json: backing (SSRF)":          `{"format":"qcow2","full-backing-filename":"json:{\"file.driver\":\"http\",\"file.url\":\"http://169.254.169.254/\"}"}`,
		"external data file":            `{"format":"qcow2","format-specific":{"type":"qcow2","data":{"data-file":"/dev/sda"}}}`,
		"not qcow2":                     `{"format":"vmdk"}`,
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := newStampTestProvider(t)
			node := &importNode{t: t, infoOut: info}
			p.ssh = &sshTransport{host: "pve", logger: slog.Default(), runOverride: node.run}

			_, err := p.ImportDisk(context.Background(), nfsImportReq(t))
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
			assert.False(t, node.ran("mv "), "a refused image is never published")
			assert.False(t, node.ran("qemu-img check"), "nor checked")
			assert.False(t, node.ran("qemu-img convert"), "nor converted")
			assert.True(t, node.ran("rm -f '/var/tmp/"), "its node-local copy is removed")
		})
	}
}

// TestNFSImport_PublishesASelfContainedImage: the copy is inspected WITHOUT
// -f (as qm importdisk probes it), then published and checked as qcow2; the
// mount is nosymfollow and the fetch refuses non-regular files.
func TestNFSImport_PublishesASelfContainedImage(t *testing.T) {
	p, _ := newStampTestProvider(t)
	node := &importNode{t: t, infoOut: `{"format":"qcow2","virtual-size":1073741824}`}
	p.ssh = &sshTransport{host: "pve", logger: slog.Default(), runOverride: node.run}

	resp, err := p.ImportDisk(context.Background(), nfsImportReq(t))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(node.commands), 4)
	fetch := node.commands[0]
	assert.Contains(t, fetch, "nosymfollow")
	assert.Contains(t, fetch, `sh "$MNT"/'vmmigrations_team-a_web_export.qcow2' `)
	assert.Contains(t, fetch, "setpriv --reuid 2000 --regid 2000 --clear-groups sh -c ")
	// The PIPE form: the dropped-privilege cat writes only to a pipe; the root
	// shell opens the copy for head, which bounds it.
	assert.Contains(t, fetch, `(setpriv --reuid 2000 --regid 2000 --clear-groups cat -- "$MNT"/'vmmigrations_team-a_web_export.qcow2' || echo failed > "$VRFETCHRC") | head -c `)
	assert.NotContains(t, fetch, `cat -- "$MNT"/'vmmigrations_team-a_web_export.qcow2' >`, "the uid process never holds the copy")
	assert.Contains(t, fetch, `trap 'umount "$MNT" 2>/dev/null || true; rmdir "$MNT" 2>/dev/null || true; rm -f "$VRFETCHRC"`)
	assert.True(t, strings.HasPrefix(node.commands[1], "head -c 65536 -- "), "the header is parsed before qemu opens it")
	assert.True(t, strings.HasPrefix(node.commands[2], "unshare -n -- qemu-img info -U --output=json -- "), "probed, as qm importdisk does")
	assert.NotContains(t, node.commands[2], "-f ")
	assert.True(t, strings.HasPrefix(node.commands[3], "mv -f -- "))
	assert.Contains(t, node.commands[4], "qemu-img check -f qcow2 -- ")
	assert.Equal(t, resp.GetPath(), strings.Trim(strings.Fields(node.commands[3])[4], "'"))
}

// TestNFSImport_UnreadableImageIsInvalidAndSSHFailureIsNot: qemu-img exit 1
// (cannot read the image) is InvalidArgument; ssh's 255 stays retryable.
func TestNFSImport_UnreadableImageIsInvalidAndSSHFailureIsNot(t *testing.T) {
	for exit, want := range map[int]codes.Code{1: codes.InvalidArgument, 255: codes.Internal} {
		p, _ := newStampTestProvider(t)
		node := &importNode{t: t, infoExit: exit}
		p.ssh = &sshTransport{host: "pve", logger: slog.Default(), runOverride: node.run}
		_, err := p.ImportDisk(context.Background(), nfsImportReq(t))
		assert.Equal(t, want, status.Code(err), "exit %d: %v", exit, err)
		assert.False(t, node.ran("mv "))
	}
}

// TestNFSStagedFetchCmd_RunsAsWritten runs the fetch command (under the
// mount script's `set -e`) against a local directory standing in for the
// mount: a regular file is copied byte for byte (mode 0600); a symbolic link
// or a directory at the staging name is refused with exit 6, a file larger
// than the bound with exit 7 — and in each refusal nothing is read into the
// copy (the caller removes it).
func TestNFSStagedFetchCmd_RunsAsWritten(t *testing.T) {
	mnt := t.TempDir()
	dst := filepath.Join(t.TempDir(), "stage.nfs-download")
	run := func(rel string, limit int64) (string, int) {
		cmd := exec.Command("sh", "-c", "set -e; "+nfsStagedFetchCmd(rel, dst, nil, nil, limit)) //nolint:gosec // test runs the command under test
		cmd.Env = append(os.Environ(), "MNT="+mnt)
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		require.NoError(t, err)
		return string(out), 0
	}
	copied := func() []byte {
		data, err := os.ReadFile(dst) //nolint:gosec // test reads its fixture
		if os.IsNotExist(err) {
			return nil
		}
		require.NoError(t, err)
		return data
	}
	const bound = int64(1) << 30

	require.NoError(t, os.WriteFile(filepath.Join(mnt, "good.qcow2"), []byte("QFI\xfbdata"), 0o600))
	_, code := run("good.qcow2", bound)
	assert.Equal(t, 0, code)
	assert.Equal(t, []byte("QFI\xfbdata"), copied())
	fi, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	require.NoError(t, os.Remove(dst))

	out, code := run("good.qcow2", 4)
	assert.Equal(t, nfsTooLargeExit, code, "larger than the bound")
	assert.Contains(t, out, migration.ErrStagedObjectTooLarge.Error())
	assert.Empty(t, copied(), "nothing is copied")
	_ = os.Remove(dst)

	secret := filepath.Join(t.TempDir(), "authkey.key")
	require.NoError(t, os.WriteFile(secret, []byte("PRIVATE"), 0o600))
	require.NoError(t, os.Symlink(secret, filepath.Join(mnt, "link.qcow2")))
	out, code = run("link.qcow2", bound)
	assert.Equal(t, nfsNotRegularFileExit, code)
	assert.Contains(t, out, nfsNotRegularFileMessage)
	assert.Empty(t, copied(), "a symbolic link is never read")
	_ = os.Remove(dst)

	require.NoError(t, os.Mkdir(filepath.Join(mnt, "dir.qcow2"), 0o700))
	_, code = run("dir.qcow2", bound)
	assert.Equal(t, nfsNotRegularFileExit, code)
	assert.Empty(t, copied())
	_ = os.Remove(dst)

	// The object grows after it was measured (a tenant NFS server reports a
	// small size, then serves more): the checks see 9 bytes, then cat finds
	// many more. The copy is bounded by the MEASURED size + 1 — not by the
	// (much larger) limit — and refused as too large (7), not as a read
	// failure, though head closing the pipe makes cat fail.
	grow := filepath.Join(mnt, "grow.qcow2")
	require.NoError(t, os.WriteFile(grow, []byte("123456789"), 0o600))
	cmd := exec.Command("sh", "-c", //nolint:gosec // test runs the command under test
		"set -e; "+strings.Replace(nfsStagedFetchCmd("grow.qcow2", dst, nil, nil, bound), "; VRFETCHRC=",
			"; head -c 100000 /dev/zero >> "+proxmoxShellQuote(grow)+"; VRFETCHRC=", 1))
	cmd.Env = append(os.Environ(), "MNT="+mnt)
	grown, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	require.ErrorAs(t, err, &ee, string(grown))
	assert.Equal(t, nfsTooLargeExit, ee.ExitCode())
	assert.Len(t, copied(), 10, "head copied at most bound+1 bytes")
	_ = os.Remove(dst)

	if os.Geteuid() == 0 {
		return
	}
	// A staged file the uid cannot read: the read failure is exit 9.
	unreadable := filepath.Join(mnt, "unreadable.qcow2")
	require.NoError(t, os.WriteFile(unreadable, []byte("data"), 0o000))
	out, code = run("unreadable.qcow2", bound)
	assert.Equal(t, nfsReadFailedExit, code, out)
	assert.Contains(t, out, nfsFetchReadFailedMessage)
}

// TestNFSImport_BoundRefusalsLeaveNothing (re-review Medium 1): a staged
// object above the bound is InvalidArgument, one that does not fit in the
// node's free space is ResourceExhausted; both constant, and the node-local
// copy is removed.
func TestNFSImport_BoundRefusalsLeaveNothing(t *testing.T) {
	for exit, want := range map[int]codes.Code{nfsTooLargeExit: codes.InvalidArgument, nfsNoSpaceExit: codes.ResourceExhausted} {
		p, _ := newStampTestProvider(t)
		node := &importNode{t: t, fetchExit: exit}
		p.ssh = &sshTransport{host: "pve", logger: slog.Default(), runOverride: node.run}
		_, err := p.ImportDisk(context.Background(), nfsImportReq(t))
		assert.Equal(t, want, status.Code(err), "exit %d: %v", exit, err)
		assert.NotContains(t, status.Convert(err).Message(), "nfs.example")
		assert.True(t, node.ran("rm -f '/var/tmp/"), "the node-local copy is removed")
		assert.False(t, node.ran("head -c "), "nothing is inspected")
	}
}

// TestNFSImport_ParsesTheHeaderBeforeQemuOpensIt (re-review Medium 2): a
// header that names an external data file (which qemu would open while
// reading it) is refused from its bytes alone: qemu-img never runs on it.
func TestNFSImport_ParsesTheHeaderBeforeQemuOpensIt(t *testing.T) {
	v3 := make([]byte, 4096)
	copy(v3, "QFI\xfb")
	v3[7] = 3   // version 3
	v3[23] = 16 // cluster bits
	v3[79] = 4  // incompatible features: external data file
	v3[103] = 104
	for name, head := range map[string][]byte{
		"external data file bit": v3,
		"backing file":           func() []byte { b := minimalQCOW2(); b[15] = 1; b[19] = 8; return b }(),
		"not qcow2":              []byte("KDMV but not what is expected"),
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := newStampTestProvider(t)
			node := &importNode{t: t, head: head, infoOut: `{"format":"qcow2"}`}
			p.ssh = &sshTransport{host: "pve", logger: slog.Default(), runOverride: node.run}
			_, err := p.ImportDisk(context.Background(), nfsImportReq(t))
			assert.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
			assert.False(t, node.ran("unshare -n -- qemu-img info"), "qemu never opens a refused header")
			assert.False(t, node.ran("mv "))
		})
	}
}
