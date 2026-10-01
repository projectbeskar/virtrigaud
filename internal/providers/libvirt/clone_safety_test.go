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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the clone hardening of the delete-safety fix: linked clones
// are refused, a clone's disk is never world-writable, and its UEFI varstore
// is never written through a symlink or over another domain's varstore.

// cloneReq full-clones team-a.web into team-b/copy.
func cloneReq() contracts.CloneRequest {
	return contracts.CloneRequest{
		Source:     contracts.VMRef{ID: "team-a.web"},
		TargetName: "copy",
		TargetVM:   contracts.ObjectIdentity{Namespace: "team-b", Name: "copy"},
	}
}

// TestClone_LinkedIsRefusedBeforeAnyHostCommand pins that libvirt linked
// clones are disabled: a Linked request is an sdk InvalidSpec (gRPC
// InvalidArgument) naming FullClone, and no host command runs — the provider's
// host connection refuses and counts any call.
func TestClone_LinkedIsRefusedBeforeAnyHostCommand(t *testing.T) {
	p := &Provider{virshProvider: newUnroutableVirshProvider()}
	req := cloneReq()
	req.Linked = true

	_, err := p.Clone(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	assert.Contains(t, err.Error(), "linked clones are disabled on libvirt in this release")
	assert.Contains(t, err.Error(), "use FullClone")

	_, err = NewServer(p).Clone(context.Background(), &providerv1.CloneRequest{
		SourceVmId: "team-a.web", TargetName: "copy", Linked: true,
		TargetVm: &providerv1.ObjectIdentity{Namespace: "team-b", Name: "copy"},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "a non-retryable answer the breaker never counts")
	assert.Contains(t, status.Convert(err).Message(), "use FullClone")
	assert.Zero(t, p.virshProvider.unroutableHits.Load(), "no host command may run")
}

func TestClone_FullCloneIsUnchanged(t *testing.T) {
	c := newCreateHost(t)
	_, err := c.p.Create(context.Background(), c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2")))
	require.NoError(t, err)
	c.resetLogs()

	resp, err := c.p.Clone(context.Background(), cloneReq())
	require.NoError(t, err)
	assert.Equal(t, "team-b.copy", resp.TargetVmID)
	disk := filepath.Join(c.images, "team-b.copy-disk.qcow2")
	assert.FileExists(t, disk)
	// The copy reads the source as root (sudo -n) in the format its
	// definition names (privileged_copy.go).
	assert.Regexp(t, `convert -f qcow2 -O qcow2 `+regexp.QuoteMeta(filepath.Join(c.images, "team-a.web-disk.qcow2"))+" "+
		regexp.QuoteMeta(c.images+"/"+vmDiskWriteDirPrefix)+`[A-Za-z0-9]{10}/team-b\.copy-disk\.qcow2\n`, c.log("qemu-img"),
		"an independent, flattened copy, written in a private directory and renamed into place")
	requireNoWriteDirsLeft(t, c.images)
	assert.NotContains(t, c.log("qemu-img"), "create", "no overlay")
	assert.NoFileExists(t, disk+".info.json", "no backing file")
	assert.Contains(t, c.domainXML("team-b.copy"), "<source file='"+disk+"'/>")
}

// requireNoWriteDirsLeft asserts no private write directory (diskWriteDir) is
// left in dir.
func requireNoWriteDirsLeft(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), vmDiskWriteDirPrefix), "left behind: %s", e.Name())
	}
}

// requireCreatedWithVMDiskMode asserts disk was created with vmDiskMode (the
// fake qemu-img writes it through the umask the provider set) and chowned
// without following a symbolic link — never chmod'ed.
func requireCreatedWithVMDiskMode(t *testing.T, sudoLog []string, disk string) {
	t.Helper()
	fi, err := os.Lstat(disk)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), fi.Mode().Perm(), "created owner rw, group r, nothing for others")
	assert.Contains(t, sudoLog, "chown -h libvirt-qemu:kvm -- "+disk)
	for _, l := range sudoLog {
		assert.False(t, strings.HasPrefix(l, "chmod "), "no chmod by root, which follows a symlink: %q", l)
		assert.False(t, strings.HasPrefix(l, "chown ") && !strings.HasPrefix(l, "chown -h "), "chown never follows a symlink: %q", l)
	}
}

func TestClone_DiskIsNotWorldWritable(t *testing.T) {
	c := newCreateHost(t)
	_, err := c.p.Create(context.Background(), c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2")))
	require.NoError(t, err)
	c.resetLogs()

	_, err = c.p.Clone(context.Background(), cloneReq())
	require.NoError(t, err)
	requireCreatedWithVMDiskMode(t, splitLines(c.log("sudo")), filepath.Join(c.images, "team-b.copy-disk.qcow2"))
}

// TestVMDisks_AreNotWorldWritable pins vmDiskMode on the other VM-disk paths:
// a Create that copies a base image into the VM's disk, a migration's
// imported disk attached in place, and a blank disk libvirt creates — created
// 0640 and chowned libvirt-qemu:kvm with -h, never chmod'ed.
func TestVMDisks_AreNotWorldWritable(t *testing.T) {
	c := newCreateHost(t)
	ctx := context.Background()
	_, err := c.p.Create(ctx, c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2")))
	require.NoError(t, err)
	copied := filepath.Join(c.images, "team-a.web-disk.qcow2")

	s := c.withRegistry()
	landed, err := c.importFor(s, c.file(c.outside, "exported.qcow2"), ownerTeamB, "web"+contracts.ImportedDiskNameSuffix)
	require.NoError(t, err)
	_, err = c.p.Create(ctx, contracts.CreateRequest{
		Name: "web", Owner: ownerTeamB, Image: contracts.VMImage{Path: landed.Path, ImportedDisk: true},
	})
	require.NoError(t, err)

	sudo := splitLines(c.log("sudo"))
	for _, disk := range []string{copied, landed.Path} {
		requireCreatedWithVMDiskMode(t, sudo, disk)
	}

	// A blank disk: libvirt creates it (vol-create, the definition on stdin)
	// with the mode, and it is only chowned. A dir pool names the file after
	// the volume, WITHOUT an extension: <pool>/blank.
	c.resetLogs()
	blank := filepath.Join(c.images, "blank")
	vol, err := NewStorageProvider(c.vp).CreateVolume(ctx, "default", "blank", "qcow2", 3)
	require.NoError(t, err)
	assert.Equal(t, blank, vol.Path)
	assert.Contains(t, c.virshCalls("h1"), "vol-create default /dev/stdin")
	def, err := os.ReadFile(filepath.Join(c.root, "h1", "vol-create.xml")) //nolint:gosec // test reads its own fixture
	require.NoError(t, err)
	assert.Equal(t, "<volume><name>blank</name><capacity unit=\"G\">3</capacity>"+
		"<target><format type=\"qcow2\"></format><permissions><mode>0640</mode></permissions></target></volume>", string(def))
	sudo = splitLines(c.log("sudo"))
	assert.Equal(t, []string{"chown -h libvirt-qemu:kvm -- " + blank, "restorecon " + blank}, sudo)
}

// uefiSource creates team-a/web and gives it a per-VM UEFI varstore in a
// scratch nvram directory; it returns the source varstore and the path a clone
// named team-b.copy gets (rewriteNVRAMPath).
func (c *createHost) uefiSource() (src, target string) {
	c.t.Helper()
	_, err := c.p.Create(context.Background(), c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2")))
	require.NoError(c.t, err)
	nv := filepath.Join(c.base, "nvram")
	require.NoError(c.t, os.MkdirAll(nv, 0o750))
	src = c.file(nv, "team-a.web_VARS.fd")
	dir := filepath.Join(c.root, "h1")
	x, err := os.ReadFile(filepath.Join(dir, "dom-team-a.web.xml")) //nolint:gosec // test reads its own fixture
	require.NoError(c.t, err)
	require.Contains(c.t, string(x), "</os>")
	uefi := strings.Replace(string(x), "</os>", "<nvram>"+src+"</nvram></os>", 1)
	d, err := parseDomainDisks(uefi)
	require.NoError(c.t, err)
	for _, key := range []string{"team-a.web", d.UUID} {
		require.NoError(c.t, os.WriteFile(filepath.Join(dir, "dom-"+key+".xml"), []byte(uefi), 0o600))
	}
	c.resetLogs()
	return src, filepath.Join(nv, "team-b.copy_VARS.fd")
}

func TestClone_NVRAMCopyNeverFollowsSymlinks(t *testing.T) {
	c := newCreateHost(t)
	src, target := c.uefiSource()

	_, err := c.p.Clone(context.Background(), cloneReq())
	require.NoError(t, err)
	sudo := splitLines(c.log("sudo"))
	i := indexOf(sudo, "rm -f -- "+target)
	j := indexOf(sudo, "dd if="+src+" of="+target+" iflag=nofollow oflag=nofollow conv=excl status=none")
	require.GreaterOrEqual(t, i, 0, "any stale target is unlinked first: %v", sudo)
	require.Greater(t, j, i, "then copied with O_NOFOLLOW on both ends and an exclusive create, never `cp` as root: %v", sudo)
	assert.Contains(t, sudo, "chown -h libvirt-qemu:kvm -- "+target, "chowned without following a symlink")
	for _, l := range sudo {
		assert.False(t, strings.HasPrefix(l, "cp "), "no cp of the varstore: %q", l)
		assert.False(t, strings.HasPrefix(l, "chmod "), "the mode is set at creation (umask), never by a root chmod: %q", l)
	}
	assert.Contains(t, c.domainXML("team-b.copy"), "<nvram>"+target+"</nvram>")
}

// requireCloneRefusedBeforeWriting asserts a clone was refused as a Conflict
// before it wrote any file or defined anything.
func (c *createHost) requireCloneRefusedBeforeWriting(err error, want string) {
	c.t.Helper()
	require.Error(c.t, err)
	assert.True(c.t, contracts.IsConflict(err), "want a Conflict: %v", err)
	assert.Contains(c.t, err.Error(), want)
	assert.NotContains(c.t, err.Error(), c.base, "no host path in the message")
	assert.NoFileExists(c.t, filepath.Join(c.images, "team-b.copy-disk.qcow2"), "no clone disk was written")
	assert.Empty(c.t, c.log("sudo"), "nothing was copied, chowned or chmodded")
	for _, call := range c.virshCalls("h1") {
		assert.False(c.t, strings.HasPrefix(call, "define "), "nothing was defined: %q", call)
	}
}

func TestClone_NVRAMTargetSymlinkIsRefused(t *testing.T) {
	for name, dangling := range map[string]bool{"to a file": false, "dangling": true} {
		t.Run(name, func(t *testing.T) {
			c := newCreateHost(t)
			_, target := c.uefiSource()
			victim := filepath.Join(c.outside, "victim")
			if !dangling {
				victim = c.file(c.outside, "victim")
			}
			require.NoError(t, os.Symlink(victim, target))

			_, err := c.p.Clone(context.Background(), cloneReq())
			c.requireCloneRefusedBeforeWriting(err, "is a symbolic link")
		})
	}
}

func TestClone_NVRAMTargetInUseIsRefused(t *testing.T) {
	c := newCreateHost(t)
	_, target := c.uefiSource()
	require.NoError(t, os.WriteFile(target, []byte("vars"), 0o600))
	x := fmt.Sprintf("<domain type='kvm'><name>other</name><uuid>%s</uuid><os><nvram>%s</nvram></os><devices/></domain>", uuidB, target)
	c.define("h1", "other", uuidB, x)

	_, err := c.p.Clone(context.Background(), cloneReq())
	c.requireCloneRefusedBeforeWriting(err, "is in use by another domain")
}

func TestClone_NVRAMStaleTargetIsReplaced(t *testing.T) {
	c := newCreateHost(t)
	src, target := c.uefiSource()
	require.NoError(t, os.WriteFile(target, []byte("left by a failed clone"), 0o600))

	_, err := c.p.Clone(context.Background(), cloneReq())
	require.NoError(t, err)
	sudo := splitLines(c.log("sudo"))
	assert.Greater(t, indexOf(sudo, "dd if="+src+" of="+target+" iflag=nofollow oflag=nofollow conv=excl status=none"),
		indexOf(sudo, "rm -f -- "+target))
}

// passthroughSudo is a sudo that runs its command as the calling user (a
// leading -n is dropped), so a test can run copyClonedNVRAM's real commands.
const passthroughSudo = "#!/bin/sh\nif [ \"$1\" = \"-n\" ]; then shift; fi\nexec \"$@\"\n"

// TestCopyClonedNVRAM_RealCommands runs the varstore copy's actual commands
// (sudo passed through): a stale target that is a hard link to another file is
// replaced without rewriting that file, and the copy never writes through a
// symlink that sits at the target.
func TestCopyClonedNVRAM_RealCommands(t *testing.T) {
	requireGNURealpath(t)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "sudo"), []byte(passthroughSudo), 0o700)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	vp := NewVirshProvider(&ProviderConfig{})
	vp.uri = "test:///nvram"
	dir := t.TempDir()
	src := filepath.Join(dir, "src_VARS.fd")
	require.NoError(t, os.WriteFile(src, []byte("source vars"), 0o600))

	t.Run("hard-linked stale target", func(t *testing.T) {
		victim := filepath.Join(dir, "victim_VARS.fd")
		target := filepath.Join(dir, "hard_VARS.fd")
		require.NoError(t, os.WriteFile(victim, []byte("another domain's vars"), 0o600))
		require.NoError(t, os.Link(victim, target))

		copyClonedNVRAM(context.Background(), vp, src, target)
		got, err := os.ReadFile(target) //nolint:gosec // test reads its own scratch file
		require.NoError(t, err)
		assert.Equal(t, "source vars", string(got))
		fi, err := os.Lstat(target)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "created private to its owner (umask), not chmod'ed afterwards")
		kept, err := os.ReadFile(victim) //nolint:gosec // test reads its own scratch file
		require.NoError(t, err)
		assert.Equal(t, "another domain's vars", string(kept), "the other hard link is never truncated")
	})

	t.Run("symlink at the target", func(t *testing.T) {
		victim := filepath.Join(dir, "sym_victim")
		require.NoError(t, os.WriteFile(victim, []byte("untouched"), 0o600))
		target := filepath.Join(dir, "sym_VARS.fd")
		// The symlink appears after the stale-target unlink: the exclusive,
		// no-follow create refuses it.
		require.NoError(t, os.Symlink(victim, target))
		_, err := runHost(context.Background(), vp, "sudo", "dd", "if="+src, "of="+target,
			"iflag=nofollow", "oflag=nofollow", "conv=excl", "status=none")
		require.Error(t, err, "dd never writes through a symlink at the target")
		kept, err := os.ReadFile(victim) //nolint:gosec // test reads its own scratch file
		require.NoError(t, err)
		assert.Equal(t, "untouched", string(kept))
	})
}

// indexOf returns the index of s in list, or -1.
func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// TestClone_DiskTargetSymlinkIsRefused: a symbolic link planted at the clone's
// disk path — dangling (a write would create its target) or to an existing,
// unused file (it would look like a stale leftover to overwrite) — refuses the
// clone before any file is written.
func TestClone_DiskTargetSymlinkIsRefused(t *testing.T) {
	for name, dangling := range map[string]bool{"to a file": false, "dangling": true} {
		t.Run(name, func(t *testing.T) {
			c := newCreateHost(t)
			_, err := c.p.Create(context.Background(), c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2")))
			require.NoError(t, err)
			c.resetLogs()
			victim := filepath.Join(c.outside, "authorized_keys")
			if !dangling {
				victim = c.file(c.outside, "authorized_keys")
			}
			disk := filepath.Join(c.images, "team-b.copy-disk.qcow2")
			require.NoError(t, os.Symlink(victim, disk))

			_, err = c.p.Clone(context.Background(), cloneReq())
			require.Error(t, err)
			assert.True(t, contracts.IsConflict(err), "want a Conflict: %v", err)
			assert.Contains(t, err.Error(), "is a symbolic link")
			assert.NotContains(t, err.Error(), c.base, "no host path in the message")
			assert.NotContains(t, c.log("qemu-img"), "convert", "nothing was written")
			fi, err := os.Lstat(disk)
			require.NoError(t, err)
			assert.Equal(t, os.ModeSymlink, fi.Mode()&os.ModeSymlink, "the link is left as it was")
			if dangling {
				assert.NoFileExists(t, victim, "its target was never created")
			} else {
				b, err := os.ReadFile(victim) //nolint:gosec // test reads its own scratch file
				require.NoError(t, err)
				assert.Equal(t, "image-bytes", string(b), "its target was never written")
			}
		})
	}
}
