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
	"encoding/json"
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

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	// ownStagePath is the stage ImportDisk creates for testOwner()
	// (team-a/web).
	ownStagePath = "/var/tmp/.virtrigaud-import-team-a_web-migrated.qcow2"
	// legacyOwnStagePath is where an earlier release staged web's disk.
	legacyOwnStagePath = "/var/tmp/.virtrigaud-import-web-migrated.qcow2"
)

// ownStageImageJSON is the image the manager sends for testOwner()'s own,
// verified migration stage.
var ownStageImageJSON = imageJSON(ownStagePath, true)

// imageJSON marshals a contracts.VMImage the way the manager's transport does.
func imageJSON(path string, imported bool) string {
	b, err := json.Marshal(contracts.VMImage{Path: path, Format: "qcow2", ImportedDisk: imported})
	if err != nil {
		panic(err) // test helper: a fixed struct always marshals
	}
	return string(b)
}

// recordingNode records every remote command and answers with fail(cmd) (nil:
// success). An importdisk reports the volid it made.
type recordingNode struct {
	mu       sync.Mutex
	commands []string
	fail     func(cmd string) error
}

func (n *recordingNode) run(_ context.Context, cmd string) (string, string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.commands = append(n.commands, cmd)
	if n.fail != nil {
		if err := n.fail(cmd); err != nil {
			return "", "", err
		}
	}
	if strings.Contains(cmd, "qm importdisk ") {
		return "Successfully imported disk as 'unused0:local-lvm:vm-1-disk-0'", "", nil
	}
	return "", "", nil
}

func (n *recordingNode) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.commands...)
}

func newConfinementProvider(t *testing.T) (*Provider, *pvefake.Server, *recordingNode) {
	t.Helper()
	p, srv := newStampTestProvider(t)
	node := &recordingNode{}
	p.ssh = &sshTransport{host: "pve", logger: slog.Default(), runOverride: node.run}
	return p, srv, node
}

// TestCreate_ImportedDiskPathConfined: a Path that is not this VM's own,
// verified import stage is refused before anything happens — no VM, no node
// command (no importdisk, no rm) — with a constant message that never echoes
// the path.
func TestCreate_ImportedDiskPathConfined(t *testing.T) {
	cases := map[string]string{
		"foreign absolute file":             imageJSON("/etc/pve/priv/authkey.key", true),
		"another VM's block device":         imageJSON("/dev/pve/vm-100-disk-0", true),
		"own stage without ImportedDisk":    imageJSON(ownStagePath, false),
		"ImportedDisk missing":              `{"Path":"` + ownStagePath + `","Format":"qcow2"}`,
		"ImportedDisk not a bool":           `{"Path":"` + ownStagePath + `","Format":"qcow2","ImportedDisk":"true"}`,
		"legacy stage without ImportedDisk": imageJSON(legacyOwnStagePath, false),
		"traversal out of the stage":        imageJSON(ownStagePath+"/../../../etc/shadow", true),
		"traversal into the stage dir":      imageJSON("/var/tmp/../var/tmp/.virtrigaud-import-team-a_web-migrated.qcow2", true),
		"another namespace's stage":         imageJSON("/var/tmp/.virtrigaud-import-team-b_web-migrated.qcow2", true),
		"another VM's stage":                imageJSON("/var/tmp/.virtrigaud-import-team-a_db-migrated.qcow2", true),
		"another VM's legacy stage":         imageJSON("/var/tmp/.virtrigaud-import-db-migrated.qcow2", true),
		"stage in another directory":        imageJSON("/tmp/.virtrigaud-import-team-a_web-migrated.qcow2", true),
		"stage with a foreign extension":    imageJSON("/var/tmp/.virtrigaud-import-team-a_web-migrated.img", true),
		"stage with trailing space":         imageJSON(ownStagePath+" ", true),
		"stage with trailing newline":       imageJSON(ownStagePath+"\n", true),
		"relative stage":                    imageJSON(".virtrigaud-import-team-a_web-migrated.qcow2", true),
		"pre-fix bare TargetName stage":     imageJSON("/var/tmp/.virtrigaud-import-web.qcow2", true),
	}
	for name, image := range cases {
		t.Run(name, func(t *testing.T) {
			p, srv, node := newConfinementProvider(t)
			_, err := p.Create(context.Background(), createReq(testOwner(), image))
			require.Error(t, err)
			assert.Equal(t, classInvalidSpec, classOf(t, err))
			msg := status.Convert(err).Message()
			assert.Equal(t, importedDiskRejectedMessage, msg, "a constant refusal")
			var img struct{ Path string }
			require.NoError(t, json.Unmarshal([]byte(image), &img))
			if img.Path != "" && !strings.Contains(importedDiskRejectedMessage, img.Path) {
				assert.NotContains(t, msg, img.Path, "the refusal never echoes the path")
			}
			assert.Empty(t, node.all(), "no node command runs for a refused path")
			assert.Empty(t, srv.VMsNamed(testOwnerName), "nothing is created")
			assert.Empty(t, srv.MutatingRequests(), "no PVE change is requested")
		})
	}
}

// TestCreate_ImportedDiskOwnerMustMatch: the stage is bound to the requesting
// VirtualMachine's namespace and name (CreateRequest.owner and name).
func TestCreate_ImportedDiskOwnerMustMatch(t *testing.T) {
	t.Run("no owner namespace", func(t *testing.T) {
		p, srv, node := newConfinementProvider(t)
		req := createReq(testOwner(), ownStageImageJSON)
		req.Owner = nil
		_, err := p.Create(context.Background(), req)
		assert.Equal(t, classInvalidSpec, classOf(t, err))
		assert.Empty(t, node.all())
		assert.Empty(t, srv.VMsNamed(testOwnerName))
	})
	t.Run("owner from another namespace", func(t *testing.T) {
		p, srv, node := newConfinementProvider(t)
		_, err := p.Create(context.Background(), createReq(otherOwner(), ownStageImageJSON))
		assert.Equal(t, classInvalidSpec, classOf(t, err))
		assert.Empty(t, node.all())
		assert.Empty(t, srv.VMsNamed(testOwnerName))
	})
	t.Run("request name differs from owner name", func(t *testing.T) {
		p, srv, node := newConfinementProvider(t)
		req := createReq(testOwner(), ownStageImageJSON)
		req.Name = "db"
		_, err := p.Create(context.Background(), req)
		assert.Equal(t, classInvalidSpec, classOf(t, err))
		assert.Empty(t, node.all())
		assert.Empty(t, srv.VMsNamed("db"))
	})
}

// TestCreate_LegacyImportStageRefused: a stage an earlier release created
// under the bare "<vm>-migrated" name (shared by same-named VMs of every
// namespace) is refused even for the VM's own verified migration, with its own
// constant "run the migration again" message; nothing is created, nothing runs
// on the node and the file is not removed.
func TestCreate_LegacyImportStageRefused(t *testing.T) {
	for name, owner := range map[string]contracts.ObjectIdentity{
		"with owner":              testOwner(),
		"no owner (no namespace)": {Name: testOwnerName},
	} {
		for _, ext := range importStageFormats {
			t.Run(name+"/"+ext, func(t *testing.T) {
				p, srv, node := newConfinementProvider(t)
				path := legacyImportStagePath(testOwnerName, ext)
				_, err := p.Create(context.Background(), createReq(owner, imageJSON(path, true)))
				assert.Equal(t, classInvalidSpec, classOf(t, err))
				msg := status.Convert(err).Message()
				assert.Equal(t, legacyImportStageMessage, msg)
				assert.NotContains(t, msg, path)
				assert.Empty(t, node.all(), "no node command runs (no importdisk, no rm)")
				assert.Empty(t, srv.VMsNamed(testOwnerName), "nothing is created")
				assert.Empty(t, srv.MutatingRequests(), "no PVE change is requested")
			})
		}
	}
}

// TestCreate_OwnImportStageImportedAndRemoved: the VM's own stage is imported
// (behind the node-side stage check) and removed afterwards.
func TestCreate_OwnImportStageImportedAndRemoved(t *testing.T) {
	for name, path := range map[string]string{
		"namespaced stage":      ownStagePath,
		"namespaced raw stage":  "/var/tmp/.virtrigaud-import-team-a_web-migrated.raw",
		"namespaced vmdk stage": "/var/tmp/.virtrigaud-import-team-a_web-migrated.vmdk",
	} {
		t.Run(name, func(t *testing.T) {
			p, srv, node := newConfinementProvider(t)
			_, err := p.Create(context.Background(), createReq(testOwner(), imageJSON(path, true)))
			require.NoError(t, err)
			require.Len(t, srv.VMsNamed(testOwnerName), 1)

			q := proxmoxShellQuote(path)
			var imported, removed bool
			for _, c := range node.all() {
				if strings.Contains(c, "qm importdisk ") {
					imported = true
					assert.True(t, strings.HasPrefix(c, importStageNodeCheck(path)+" && qm importdisk "),
						"importdisk runs only behind the stage check: %s", c)
					assert.Contains(t, c, " "+q+" ")
				}
				if strings.HasPrefix(c, "rm ") {
					removed = true
					assert.Equal(t, "rm -f -- "+q, c)
				}
			}
			assert.True(t, imported, "the own stage is imported")
			assert.True(t, removed, "the consumed stage is removed")
		})
	}
}

// TestCreate_ImportStageNodeCheckFails: a stage that fails the node-side check
// (a symbolic link, a second hard link, not a regular file) is not imported:
// the importdisk command fails as a whole, the VM shell is removed (nothing
// remains) and the stage is not removed.
func TestCreate_ImportStageNodeCheckFails(t *testing.T) {
	p, srv, node := newConfinementProvider(t)
	node.fail = func(cmd string) error {
		if strings.HasPrefix(cmd, importStageNodeCheck(ownStagePath)) {
			return exitErr(t, 1) // `[ ! -L ... ]` is false: the shell exits 1
		}
		return nil
	}
	_, err := p.Create(context.Background(), createReq(testOwner(), ownStageImageJSON))
	assert.Equal(t, classNothingRemainsVM, classOf(t, err))
	assert.Empty(t, srv.VMsNamed(testOwnerName), "the half-built shell is removed")
	for _, c := range node.all() {
		assert.False(t, strings.HasPrefix(c, "rm "), "the stage is not removed: %s", c)
		assert.False(t, strings.HasPrefix(c, "qm set "), "nothing is attached: %s", c)
	}
}

// TestCleanupNodeFile_RemovesOnlyImportStages: rm never runs on a path that is
// not an import stage.
func TestCleanupNodeFile_RemovesOnlyImportStages(t *testing.T) {
	for _, path := range []string{
		"/etc/pve/priv/authkey.key",
		"/dev/pve/vm-100-disk-0",
		"/var/tmp/.virtrigaud-import-../../etc/shadow.qcow2",
		"/var/tmp/.virtrigaud-import-x/../../etc/shadow.qcow2",
		"/var/tmp/.virtrigaud-import-x.qcow2/..",
		"/var/tmp/.virtrigaud-nfsmnt-abc",
		"/var/tmp/.virtrigaud-import-web.qcow2\n",
		"/var/tmp/.virtrigaud-import-Web-migrated.qcow2",
		"/var/tmp/.virtrigaud-import-.qcow2",
		legacyOwnStagePath,
		"/var/tmp/.virtrigaud-import-team-a_../../etc/x-migrated.qcow2",
	} {
		t.Run(path, func(t *testing.T) {
			p, _, node := newConfinementProvider(t)
			p.cleanupNodeFile(path)
			assert.Empty(t, node.all())
		})
	}
	p, _, node := newConfinementProvider(t)
	p.cleanupNodeFile(ownStagePath)
	assert.Equal(t, []string{"rm -f -- " + proxmoxShellQuote(ownStagePath)}, node.all())
}

// TestImportStageReady_RefusesNonStagePath (rebuild of an interrupted
// imported-disk create): a path that is not an import stage is never checked
// on the node, and the check of a stage is the full node-side stage check.
func TestImportStageReady_RefusesNonStagePath(t *testing.T) {
	p, _, node := newConfinementProvider(t)
	err := p.importStageReady(context.Background(), "/etc/pve/priv/authkey.key")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "/etc/pve")
	assert.Empty(t, node.all())

	require.NoError(t, p.importStageReady(context.Background(), ownStagePath))
	assert.Equal(t, []string{importStageNodeCheck(ownStagePath)}, node.all())
}

func TestCheckImportedDiskPath(t *testing.T) {
	owner := contracts.ObjectIdentity{UID: "u", Namespace: "team-a", Name: "web"}
	require.NoError(t, checkImportedDiskPath(ownStagePath, true, owner, "web"))

	// The VM's own legacy bare-name stage gets its own "run it again" refusal,
	// with or without the namespace; not imported, it is the generic one.
	for _, o := range []contracts.ObjectIdentity{owner, {}} {
		err := checkImportedDiskPath(legacyOwnStagePath, true, o, "web")
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		assert.Equal(t, legacyImportStageMessage, status.Convert(err).Message())
	}
	err := checkImportedDiskPath(legacyOwnStagePath, false, owner, "web")
	assert.Equal(t, importedDiskRejectedMessage, status.Convert(err).Message())

	for name, tc := range map[string]struct {
		path     string
		imported bool
		owner    contracts.ObjectIdentity
		vm       string
	}{
		"not imported":            {ownStagePath, false, owner, "web"},
		"no vm name":              {ownStagePath, true, owner, ""},
		"vm name not owner name":  {ownStagePath, true, owner, "db"},
		"vm name not a DNS name":  {"/var/tmp/.virtrigaud-import-team-a_Web-migrated.qcow2", true, contracts.ObjectIdentity{Namespace: "team-a"}, "Web"},
		"vm name with a slash":    {"/var/tmp/.virtrigaud-import-team-a_../x-migrated.qcow2", true, contracts.ObjectIdentity{Namespace: "team-a"}, "../x"},
		"namespace not DNS label": {"/var/tmp/.virtrigaud-import-team.a_web-migrated.qcow2", true, contracts.ObjectIdentity{Namespace: "team.a"}, "web"},
		"no namespace":            {ownStagePath, true, contracts.ObjectIdentity{}, "web"},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkImportedDiskPath(tc.path, tc.imported, tc.owner, tc.vm)
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.Equal(t, importedDiskRejectedMessage, status.Convert(err).Message())
		})
	}
}

func TestImportStageName(t *testing.T) {
	assert.Equal(t, "team-a_web-migrated", importStageName("team-a", "web"))
	assert.NotEqual(t, importStageName("a", "b-c"), importStageName("a-b", "c"),
		"the separator keeps namespace and name apart")

	long := strings.Repeat("n", 63)
	name := strings.Repeat("v", 253)
	got := importStageName(long, name)
	assert.LessOrEqual(t, len(got), maxImportStageNameBytes)
	assert.True(t, strings.HasSuffix(got, contracts.ImportedDiskNameSuffix))
	assert.Contains(t, got, importStageHashMarker)
	assert.NotEqual(t, got, importStageName(long, name[:252]+"w"), "bounded names stay distinct")
	assert.True(t, isImportStagePath(importStageFilePath(got, "qcow2")))
	assert.LessOrEqual(t, len(importStageFilePrefix+got+".qcow2"), 255, "fits a file name")
}

func TestImportStageForRequest(t *testing.T) {
	tv := &providerv1.ObjectIdentity{Namespace: "team-a", Name: "web"}
	name, path, err := importStageForRequest(&providerv1.ImportDiskRequest{TargetVm: tv, TargetName: "web-migrated"}, "vmdk")
	require.NoError(t, err)
	assert.Equal(t, "team-a_web-migrated", name)
	assert.Equal(t, "/var/tmp/.virtrigaud-import-team-a_web-migrated.vmdk", path)

	_, _, err = importStageForRequest(&providerv1.ImportDiskRequest{TargetVm: tv}, "qcow2")
	require.NoError(t, err, "target_name is optional")

	for name, req := range map[string]*providerv1.ImportDiskRequest{
		"no target_vm (older manager)": {TargetName: "web-migrated"},
		"no target namespace":          {TargetVm: &providerv1.ObjectIdentity{Name: "web"}},
		"target_name of another VM":    {TargetVm: tv, TargetName: "db-migrated"},
		"target name traversal":        {TargetVm: &providerv1.ObjectIdentity{Namespace: "team-a", Name: "../../etc"}},
		"target namespace not a label": {TargetVm: &providerv1.ObjectIdentity{Namespace: "Team_A", Name: "web"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := importStageForRequest(req, "qcow2")
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
		})
	}
}

// TestImportDisk_RequiresTargetVM: both node-side import paths refuse a
// request without the target VM identity before running anything on the node.
func TestImportDisk_RequiresTargetVM(t *testing.T) {
	for name, req := range map[string]*providerv1.ImportDiskRequest{
		"s3": {BackendType: "s3", TargetName: "web-migrated", SourceUrl: "s3://bucket/web.qcow2"},
		"nfs": {BackendType: "nfs", TargetName: "web-migrated",
			SourceUrl:          "nfs://172.16.56.13/export/virtrigaud/web.qcow2",
			StorageOptionsJson: `{"server":"172.16.56.13","export":"/export/virtrigaud"}`},
	} {
		t.Run(name, func(t *testing.T) {
			p, _, node := newConfinementProvider(t)
			_, err := p.ImportDisk(context.Background(), req)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.Empty(t, node.all(), "nothing runs on the node")
		})
	}
}

func TestIsImportStagePath(t *testing.T) {
	assert.True(t, isImportStagePath(ownStagePath))
	assert.True(t, isImportStagePath("/var/tmp/.virtrigaud-import-team-a_web-migrated.vmdk"))
	assert.False(t, isImportStagePath(legacyOwnStagePath), "legacy bare-name stages are never removed")
	assert.False(t, isImportStagePath("/var/tmp/.virtrigaud-import-imported-disk-1700000000.raw"))
	assert.False(t, isImportStagePath("/var/tmp/.virtrigaud-import-team-a_web.qcow2"), "no -migrated suffix")
	assert.False(t, isImportStagePath("/var/tmp/.virtrigaud-import-a..b.qcow2"))
	assert.False(t, isImportStagePath("/var/tmp/.virtrigaud-import-a/b.qcow2"))
	assert.False(t, isImportStagePath("/var/tmp/.virtrigaud-export-a.qcow2"))
}

// TestImportStageNodeCheck_RealShell runs the node-side stage check against
// real files with the local POSIX sh and bash (GNU stat, as on a PVE node).
// Only a regular, single-link file owned by the running user with no group or
// other write bit passes. (A file owned by another user cannot be made
// without root; the owner comparison is exercised by the passing case.)
func TestImportStageNodeCheck_RealShell(t *testing.T) {
	if err := exec.Command("stat", "-c", "%h", "/").Run(); err != nil {
		t.Skip("GNU stat not available")
	}
	dir := t.TempDir()
	mk := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte("disk"), 0o600))
		require.NoError(t, os.Chmod(p, mode))
		return p
	}
	ok600 := mk("ok600.qcow2", 0o600)
	ok644 := mk("ok644.qcow2", 0o644)
	groupWrite := mk("g.qcow2", 0o660)
	otherWrite := mk("o.qcow2", 0o602)
	linked := mk("linked.qcow2", 0o600)
	require.NoError(t, os.Link(linked, filepath.Join(dir, "second-name.qcow2")))
	symlink := filepath.Join(dir, "sym.qcow2")
	require.NoError(t, os.Symlink(ok600, symlink))
	subdir := filepath.Join(dir, "d.qcow2")
	require.NoError(t, os.Mkdir(subdir, 0o700))
	quoted := mk("it's quoted.qcow2", 0o600)

	cases := map[string]bool{
		ok600:                           true,
		ok644:                           true,
		quoted:                          true,
		groupWrite:                      false,
		otherWrite:                      false,
		linked:                          false,
		symlink:                         false,
		subdir:                          false,
		filepath.Join(dir, "missing.x"): false,
	}
	for _, shell := range []string{"sh", "bash"} {
		if _, err := exec.LookPath(shell); err != nil {
			continue
		}
		for path, want := range cases {
			err := exec.Command(shell, "-c", importStageNodeCheck(path)).Run()
			assert.Equal(t, want, err == nil, "%s: %s (err %v)", shell, filepath.Base(path), err)
		}
	}
}

// TestCreate_ImportedDiskFormatConfined: spec.importedDisk.format is
// tenant-writable and becomes the `qm importdisk --format` value; anything but
// qcow2, raw or vmdk is refused with a constant message before anything
// happens.
func TestCreate_ImportedDiskFormatConfined(t *testing.T) {
	for name, format := range map[string]string{
		"unknown format":      `"vdi"`,
		"shell metacharacter": `"qcow2; rm -rf /"`,
		"upper case":          `"QCOW2"`,
		"not a string":        `5`,
		"object":              `{"x":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			p, srv, node := newConfinementProvider(t)
			image := `{"Path":"` + ownStagePath + `","Format":` + format + `,"ImportedDisk":true}`
			_, err := p.Create(context.Background(), createReq(testOwner(), image))
			assert.Equal(t, classInvalidSpec, classOf(t, err))
			assert.Equal(t, importedDiskFormatRejectedMessage, status.Convert(err).Message())
			assert.Empty(t, node.all())
			assert.Empty(t, srv.VMsNamed(testOwnerName))
			assert.Empty(t, srv.MutatingRequests())
		})
	}
}

func TestImportedDiskFormat(t *testing.T) {
	for _, ok := range []any{nil, "", "qcow2", "raw", "vmdk"} {
		got, err := importedDiskFormat(ok)
		require.NoError(t, err, "%v", ok)
		if s, isStr := ok.(string); isStr {
			assert.Equal(t, s, got)
		}
	}
	for _, bad := range []any{"vdi", "qcow2 ", true, 1.0} {
		_, err := importedDiskFormat(bad)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "%v", bad)
	}
}
