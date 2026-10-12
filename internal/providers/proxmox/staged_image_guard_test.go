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
	"encoding/binary"
	"log/slog"
	"strconv"
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

// guardStagePath is the stage nfsImportReq's import writes.
var guardStagePath = importStagePath("team-a", "web", "qcow2")

// minimalQCOW2 is the leading bytes of a self-contained qcow2 v2 image:
// magic, version 2, 64 KiB clusters, no backing file, no extensions.
func minimalQCOW2() []byte {
	b := make([]byte, 4096)
	copy(b, "QFI\xfb")
	b[7] = 2   // version
	b[23] = 16 // cluster bits
	return b
}

// minimalSparseVMDK is the leading bytes of a single-file sparse VMDK of
// 1 GiB with no embedded descriptor.
func minimalSparseVMDK() []byte {
	b := make([]byte, 4096)
	binary.LittleEndian.PutUint32(b[0:], 0x564d444b) // "KDMV"
	binary.LittleEndian.PutUint32(b[4:], 1)          // version
	binary.LittleEndian.PutUint64(b[12:], 2097152)   // capacity in sectors
	binary.LittleEndian.PutUint64(b[56:], 30)        // grain directory offset
	return b
}

// stagedHeaderAnswer answers the two commands of checkStagedNodeImage for a
// self-contained image of the stage's format (from its extension): the
// base64 of its leading bytes, and qemu-img info's JSON.
func stagedHeaderAnswer(cmd string) (string, bool) {
	format := "qcow2"
	for _, f := range importStageFormats {
		if strings.HasSuffix(strings.TrimRight(cmd, "' "), "."+f) || strings.Contains(cmd, "."+f+"'") {
			format = f
		}
	}
	switch {
	case strings.HasPrefix(cmd, "head -c "):
		head := map[string][]byte{"qcow2": minimalQCOW2(), "vmdk": minimalSparseVMDK(), "raw": make([]byte, 4096)}[format]
		return base64.StdEncoding.EncodeToString(head), true
	case strings.HasPrefix(cmd, "unshare -n -- qemu-img info "):
		return `{"format":"` + format + `"}`, true
	}
	return "", false
}

// importNode answers the node commands of an NFS import — the mount + stage
// copy script, the header parse (head, unless overridden), the qemu-img
// probe (infoOut / infoExit) and the qemu-img check — and models the stage
// directory: the copy script publishes the stage, `rm -f -- <stage>` removes
// it.
type importNode struct {
	mu        sync.Mutex
	t         *testing.T
	infoOut   string
	infoExit  int
	head      []byte
	fetchExit int
	checkExit int
	commands  []string
	stages    map[string]bool
}

func (n *importNode) run(_ context.Context, cmd string) (string, string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.commands = append(n.commands, cmd)
	switch {
	case strings.HasPrefix(cmd, "set -e; MNT="):
		if n.fetchExit != 0 {
			msg := map[int]string{
				nfsNotRegularFileExit: nfsNotRegularFileMessage,
				nfsTooLargeExit:       migration.ErrStagedObjectTooLarge.Error(),
				nfsNoSpaceExit:        migration.ErrStagingSpaceExhausted.Error(),
			}[n.fetchExit]
			return "", msg, exitErr(n.t, n.fetchExit)
		}
		if n.stages == nil {
			n.stages = map[string]bool{}
		}
		n.stages[guardStagePath] = true
		return "", "", nil
	case strings.HasPrefix(cmd, "rm -f -- "):
		for path := range n.stages {
			if strings.Contains(cmd, proxmoxShellQuote(path)) {
				delete(n.stages, path)
			}
		}
		return "", "", nil
	case strings.HasPrefix(cmd, "head -c ") && n.head != nil:
		return base64.StdEncoding.EncodeToString(n.head), "", nil
	case strings.HasPrefix(cmd, "unshare -n -- qemu-img info"):
		if n.infoExit != 0 {
			return "", "qemu-img: Could not open", exitErr(n.t, n.infoExit)
		}
		if n.infoOut != "" {
			return n.infoOut, "", nil
		}
	case strings.HasPrefix(cmd, "qemu-img check ") && n.checkExit != 0:
		return "", "corrupt", exitErr(n.t, n.checkExit)
	case strings.HasPrefix(cmd, "qemu-img info -f "):
		// The disk-size limit check (#382) on the stage: 1 MiB.
		return `{"virtual-size": 1048576, "format": "qcow2"}`, "", nil
	}
	if out, ok := stagedHeaderAnswer(cmd); ok {
		return out, "", nil
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

// stageLeft reports whether a stage is left in the modelled stage directory.
func (n *importNode) stageLeft() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.stages) > 0
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
		TargetVm:           &providerv1.ObjectIdentity{Namespace: "team-a", Name: "web"},
	}
}

func newImportNodeProvider(t *testing.T, node *importNode) *Provider {
	t.Helper()
	p, _ := newStampTestProvider(t)
	node.t = t
	p.ssh = &sshTransport{host: "pve", logger: slog.Default(), runOverride: node.run}
	return p
}

// TestNFSImport_RefusesAStagedImageThatReferencesOtherFiles (review High):
// a staged qcow2 whose header names a backing file, an external data file, or
// that is not qcow2, is refused (InvalidArgument) before anything checks it as
// qcow2, and the stage is removed: nothing is left in the stage directory.
func TestNFSImport_RefusesAStagedImageThatReferencesOtherFiles(t *testing.T) {
	for name, info := range map[string]string{
		"backing file to a node secret": `{"format":"qcow2","backing-filename":"/etc/pve/priv/authkey.key"}`,
		"json: backing (SSRF)":          `{"format":"qcow2","full-backing-filename":"json:{\"file.driver\":\"http\",\"file.url\":\"http://169.254.169.254/\"}"}`,
		"external data file":            `{"format":"qcow2","format-specific":{"type":"qcow2","data":{"data-file":"/dev/sda"}}}`,
		"not qcow2":                     `{"format":"vmdk"}`,
	} {
		t.Run(name, func(t *testing.T) {
			node := &importNode{infoOut: info}
			p := newImportNodeProvider(t, node)

			_, err := p.ImportDisk(context.Background(), nfsImportReq(t))
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
			assert.False(t, node.ran("qemu-img check"), "a refused image is never checked")
			assert.False(t, node.ran("qemu-img convert"), "nor converted")
			assert.True(t, node.ran("rm -f -- "+proxmoxShellQuote(guardStagePath)), "the refused stage is removed")
			assert.False(t, node.stageLeft(), "nothing is left in the stage directory")
		})
	}
}

// TestNFSImport_StagesASelfContainedImage: the staged object is copied into
// the root-only stage directory by the confinement stage script (nosymfollow
// mount, measured as the NFS identity, bounded copy, the PIPE form), then its
// header is parsed, probed WITHOUT -f (as qm importdisk does) and checked as
// qcow2. The stage is kept for Create.
func TestNFSImport_StagesASelfContainedImage(t *testing.T) {
	node := &importNode{infoOut: `{"format":"qcow2","virtual-size":1073741824}`}
	p := newImportNodeProvider(t, node)

	resp, err := p.ImportDisk(context.Background(), nfsImportReq(t))
	require.NoError(t, err)
	require.Len(t, node.commands, 4)
	stage := node.commands[0]
	src := `"$MNT"/'vmmigrations_team-a_web_export.qcow2'`
	assert.Contains(t, stage, "nosymfollow,nosuid,nodev,noexec")
	assert.Contains(t, stage, "TMP=$(mktemp -p '/var/lib/virtrigaud/import-stage' .stage-XXXXXXXXXX)")
	assert.Contains(t, stage, "SZ=$(setpriv --reuid 2000 --regid 2000 --clear-groups sh -c ")
	assert.Contains(t, stage, " sh "+src+"); ")
	// The PIPE form: the dropped-privilege cat writes only to a pipe; the root
	// shell opens the copy for head, which bounds it by the measured size.
	assert.Contains(t, stage, `{ if setpriv --reuid 2000 --regid 2000 --clear-groups cat -- `+src+` 3>&-; then echo 0 >&3; else echo 1 >&3; fi; } 3>"$RC" | head -c $((SZ + 1)) > "$TMP"`)
	assert.NotContains(t, stage, "cat -- "+src+" >", "the NFS identity never holds the copy")
	assert.NotContains(t, stage, ".nfs-download", "no copy outside the stage directory")
	assert.Contains(t, stage, `mv -T -- "$TMP" `+proxmoxShellQuote(guardStagePath))

	assert.True(t, strings.HasPrefix(node.commands[1], "head -c 65536 -- "), "the header is parsed before qemu opens it")
	assert.True(t, strings.HasPrefix(node.commands[2], "unshare -n -- qemu-img info -U --output=json -- "), "probed, as qm importdisk does")
	assert.NotContains(t, node.commands[2], "-f ")
	assert.Equal(t, "qemu-img check -f qcow2 -- "+proxmoxShellQuote(guardStagePath), node.commands[3])
	assert.Equal(t, guardStagePath, resp.GetPath())
	assert.True(t, node.stageLeft(), "the stage is kept for Create")
}

// TestNFSImport_CopyBoundFollowsTheImportSizeLimit (#382): the stage copy is
// bounded by the provider bound lowered by max_virtual_size_bytes (with room
// for the image's metadata), and the same bound refuses a header that
// declares a larger virtual size.
func TestNFSImport_CopyBoundFollowsTheImportSizeLimit(t *testing.T) {
	const max = int64(1) << 30
	want, err := migration.StagedCopyLimit(max)
	require.NoError(t, err)

	node := &importNode{infoOut: `{"format":"qcow2"}`}
	p := newImportNodeProvider(t, node)
	req := nfsImportReq(t)
	req.MaxVirtualSizeBytes = max
	_, err = p.ImportDisk(context.Background(), req)
	require.NoError(t, err)
	assert.Contains(t, node.commands[0], `if [ "$SZ" -gt `+strconv.FormatInt(want, 10)+` ]; then`)

	big := minimalQCOW2()
	binary.BigEndian.PutUint64(big[24:], uint64(2*max)) // declared virtual size
	node = &importNode{head: big, infoOut: `{"format":"qcow2"}`}
	p = newImportNodeProvider(t, node)
	_, err = p.ImportDisk(context.Background(), req)
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
	assert.False(t, node.ran("unshare -n -- qemu-img info"), "refused from the header alone")
	assert.False(t, node.stageLeft(), "the refused stage is removed")
}

// TestNFSImport_FailuresLeaveNoStage (rebase item 6): an unreadable image
// (qemu-img exit 1) is InvalidArgument, an SSH failure (255) stays retryable,
// a failing qemu-img check is Internal; in every case the stage is removed.
func TestNFSImport_FailuresLeaveNoStage(t *testing.T) {
	for name, tc := range map[string]struct {
		node *importNode
		want codes.Code
	}{
		"unreadable image":   {node: &importNode{infoExit: 1}, want: codes.InvalidArgument},
		"ssh failure":        {node: &importNode{infoExit: 255}, want: codes.Internal},
		"qemu-img check":     {node: &importNode{checkExit: 3}, want: codes.Internal},
		"unparsable header":  {node: &importNode{head: []byte("not an image")}, want: codes.InvalidArgument},
		"header is raw vmdk": {node: &importNode{head: minimalSparseVMDK()}, want: codes.InvalidArgument},
	} {
		t.Run(name, func(t *testing.T) {
			p := newImportNodeProvider(t, tc.node)
			_, err := p.ImportDisk(context.Background(), nfsImportReq(t))
			assert.Equal(t, tc.want, status.Code(err), "%v", err)
			assert.True(t, tc.node.ran("rm -f -- "+proxmoxShellQuote(guardStagePath)))
			assert.False(t, tc.node.stageLeft(), "nothing is left in the stage directory")
		})
	}
}

// TestNFSImport_CopyRefusalsLeaveNothing (re-review Medium 1): a staged object
// that is not a regular file or is above the bound is InvalidArgument, one
// that does not fit in the node's free space is ResourceExhausted; all
// constant, none names the server, and nothing is staged or inspected.
func TestNFSImport_CopyRefusalsLeaveNothing(t *testing.T) {
	for exit, want := range map[int]codes.Code{
		nfsNotRegularFileExit: codes.InvalidArgument,
		nfsTooLargeExit:       codes.InvalidArgument,
		nfsNoSpaceExit:        codes.ResourceExhausted,
	} {
		node := &importNode{fetchExit: exit}
		p := newImportNodeProvider(t, node)
		_, err := p.ImportDisk(context.Background(), nfsImportReq(t))
		assert.Equal(t, want, status.Code(err), "exit %d: %v", exit, err)
		assert.NotContains(t, status.Convert(err).Message(), "nfs.example")
		assert.False(t, node.ran("head -c "), "nothing is inspected")
		assert.False(t, node.stageLeft(), "nothing is staged")
	}
}

// TestNFSImport_RequiresTheTargetVM: the stage is named after the target VM;
// a request without one is refused before anything runs on the node.
func TestNFSImport_RequiresTheTargetVM(t *testing.T) {
	node := &importNode{}
	p := newImportNodeProvider(t, node)
	req := nfsImportReq(t)
	req.TargetVm = nil
	_, err := p.ImportDisk(context.Background(), req)
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	assert.Empty(t, node.commands)
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
			node := &importNode{head: head, infoOut: `{"format":"qcow2"}`}
			p := newImportNodeProvider(t, node)
			_, err := p.ImportDisk(context.Background(), nfsImportReq(t))
			assert.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
			assert.False(t, node.ran("unshare -n -- qemu-img info"), "qemu never opens a refused header")
			assert.False(t, node.stageLeft())
		})
	}
}

// TestCreate_RechecksTheStageHeader (rebase item 2): Create parses and checks
// the stage's header again before it builds anything: a refused header is
// InvalidSpec, nothing is created, no importdisk runs and the stage is
// removed; a header that could not be read is VM-scoped not-performed and the
// stage is kept for a retry.
func TestCreate_RechecksTheStageHeader(t *testing.T) {
	backing := minimalQCOW2()
	backing[15], backing[19] = 1, 8 // backing file offset/size
	t.Run("refused", func(t *testing.T) {
		p, srv, node := newConfinementProvider(t)
		p.ssh.runOverride = func(ctx context.Context, cmd string) (string, string, error) {
			if strings.HasPrefix(cmd, "head -c ") {
				_, _, _ = node.run(ctx, cmd)
				return base64.StdEncoding.EncodeToString(backing), "", nil
			}
			return node.run(ctx, cmd)
		}
		_, err := p.Create(context.Background(), createReq(testOwner(), ownStageImageJSON))
		assert.Equal(t, classInvalidSpec, classOf(t, err))
		assert.Empty(t, srv.VMsNamed(testOwnerName), "nothing is created")
		var removed bool
		for _, c := range node.all() {
			assert.NotContains(t, c, "qm importdisk", "the refused stage is never imported")
			removed = removed || c == "rm -f -- "+proxmoxShellQuote(ownStagePath)
		}
		assert.True(t, removed, "the refused stage is removed")
	})
	t.Run("unreadable", func(t *testing.T) {
		p, srv, node := newConfinementProvider(t)
		node.fail = func(cmd string) error {
			if strings.HasPrefix(cmd, "head -c ") {
				return exitErr(t, 255)
			}
			return nil
		}
		_, err := p.Create(context.Background(), createReq(testOwner(), ownStageImageJSON))
		assert.Equal(t, classNothingRemainsVM, classOf(t, err))
		assert.Empty(t, srv.VMsNamed(testOwnerName), "nothing is created")
		for _, c := range node.all() {
			assert.False(t, strings.HasPrefix(c, "rm "), "the stage is kept for a retry: %s", c)
		}
	})
}
