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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// swapSudo stands in for sudo in the real-command tests below and plays the
// attacker: right before a privileged chown or chmod of a path, it replaces
// the file with a symbolic link to $SWAP_DIR/victim (what anyone able to write
// the directory can do between the create and the chown). It then runs the
// command as the calling user, with chown's owner rewritten to that user (the
// tests are not root). restorecon is a no-op; everything is logged to
// $SWAP_DIR/sudo.log.
const swapSudo = `#!/bin/sh
if [ "$1" = "-n" ]; then shift; fi
case "$1" in sudo|*/sudo) echo "swap sudo: never runs sudo" >&2; exit 1 ;; esac
printf '%s\n' "$*" >> "$SWAP_DIR/sudo.log"
for last in "$@"; do :; done
case "$1" in
  restorecon) exit 0 ;;
  chown|chmod)
    if [ ! -L "$last" ]; then rm -f -- "$last" && ln -s -- "$SWAP_DIR/victim" "$last"; fi ;;
esac
if [ "$1" = "chown" ]; then
  shift
  h=""
  if [ "$1" = "-h" ]; then h="-h"; shift; fi
  shift
  exec chown $h "$(id -u):$(id -g)" "$@"
fi
exec "$@"
`

// swapHost is a local provider whose sudo is swapSudo and whose virsh is a
// no-op, with a victim file (0644) in the swap directory.
type swapHost struct {
	vp     *VirshProvider
	dir    string
	victim string
}

func newSwapHost(t *testing.T) *swapHost {
	t.Helper()
	requireGNURealpath(t)
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not available")
	}
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "sudo"), []byte(swapSudo), 0o700))               //nolint:gosec // test shim must be executable
	require.NoError(t, os.WriteFile(filepath.Join(bin, "virsh"), []byte("#!/bin/sh\nexit 0\n"), 0o700)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	t.Setenv("SWAP_DIR", dir)
	victim := filepath.Join(dir, "victim")
	require.NoError(t, os.WriteFile(victim, []byte("root's file"), 0o600))
	require.NoError(t, os.Chmod(victim, 0o644)) //nolint:gosec // the victim's mode is what the test watches
	vp := NewVirshProvider(&ProviderConfig{})
	vp.uri = "test:///swap"
	return &swapHost{vp: vp, dir: dir, victim: victim}
}

// requireVictimUntouched asserts the victim kept its content and mode.
func (s *swapHost) requireVictimUntouched(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(s.victim)
	require.NoError(t, err)
	assert.Equal(t, "root's file", string(b))
	fi, err := os.Stat(s.victim)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), fi.Mode().Perm(), "the swapped-in symlink's target keeps its mode")
}

// sudoLog returns the commands swapSudo ran.
func (s *swapHost) sudoLog(t *testing.T) []string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(s.dir, "sudo.log")) //nolint:gosec // test reads its own log
	return splitLines(string(b))
}

// TestCreatedVMFiles_SwappedSymlinkIsNeverFollowed runs the real commands of a
// clone's disk copy and varstore copy while a symbolic link to another file is
// swapped in for the new file right before its privileged chown: the other
// file's mode and content are untouched (the mode was set at creation, and
// `chown -h` changes only the link). The first case checks the harness: the
// commands used before (`sudo chown` + `sudo chmod`) do follow the link.
func TestCreatedVMFiles_SwappedSymlinkIsNeverFollowed(t *testing.T) {
	ctx := context.Background()

	t.Run("the earlier chown+chmod follow the link", func(t *testing.T) {
		s := newSwapHost(t)
		path := filepath.Join(s.dir, "old.qcow2")
		require.NoError(t, os.WriteFile(path, []byte("disk"), 0o600))
		_, _ = runHost(ctx, s.vp, "sudo", "chown", "libvirt-qemu:kvm", path)
		_, _ = runHost(ctx, s.vp, "sudo", "chmod", "0660", path)
		fi, err := os.Stat(s.victim)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o660), fi.Mode().Perm(), "the swap reaches the victim through a root chmod")
	})

	t.Run("full-clone disk", func(t *testing.T) {
		s := newSwapHost(t)
		src := filepath.Join(s.dir, "src.qcow2")
		out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", src, "1M").CombinedOutput() //nolint:gosec // test-controlled paths
		require.NoError(t, err, "%s", out)
		dst := filepath.Join(s.dir, "team-b.copy-disk.qcow2")

		require.NoError(t, createFullCopy(ctx, s.vp, src, "qcow2", dst))
		s.requireVictimUntouched(t)
		fi, err := os.Lstat(dst)
		require.NoError(t, err)
		assert.Equal(t, os.ModeSymlink, fi.Mode()&os.ModeSymlink, "only the link itself was chowned")
		log := s.sudoLog(t)
		assert.Contains(t, log, "chown -h libvirt-qemu:kvm -- "+dst)
		for _, l := range log {
			assert.False(t, strings.HasPrefix(l, "chmod"), "no chmod: %q", l)
		}
	})

	t.Run("UEFI varstore", func(t *testing.T) {
		s := newSwapHost(t)
		src := filepath.Join(s.dir, "src_VARS.fd")
		require.NoError(t, os.WriteFile(src, []byte("vars"), 0o600))
		dst := filepath.Join(s.dir, "team-b.copy_VARS.fd")

		copyClonedNVRAM(ctx, s.vp, src, dst)
		s.requireVictimUntouched(t)
		log := s.sudoLog(t)
		assert.Contains(t, log, "chown -h libvirt-qemu:kvm -- "+dst)
		for _, l := range log {
			assert.False(t, strings.HasPrefix(l, "chmod"), "no chmod: %q", l)
		}
	})
}

// TestVMFiles_CreatedWithTheirFinalMode runs the create commands for real:
// qemu-img (which creates 0644 at most) and cat under vmDiskUmask give
// vmDiskMode, dd under clonedNVRAMUmask gives 0600 — whatever the caller's
// umask.
func TestVMFiles_CreatedWithTheirFinalMode(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not available")
	}
	ctx := context.Background()
	vp := NewVirshProvider(&ProviderConfig{})
	vp.uri = "test:///modes"
	dir := t.TempDir()
	src := filepath.Join(dir, "src.qcow2")
	out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", src, "1M").CombinedOutput() //nolint:gosec // test-controlled paths
	require.NoError(t, err, "%s", out)

	mode := func(p string) os.FileMode {
		fi, err := os.Lstat(p)
		require.NoError(t, err)
		return fi.Mode().Perm()
	}
	converted := filepath.Join(dir, "converted.qcow2")
	_, err = runHost(ctx, vp, withUmask(vmDiskUmask, "qemu-img", "convert", "-O", "qcow2", src, converted)...)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), mode(converted))

	created := filepath.Join(dir, "created.qcow2")
	_, err = runHost(ctx, vp, withUmask(vmDiskUmask, "qemu-img", "create", "-q", "-f", "qcow2", created, "1M")...)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), mode(created))

	streamed := filepath.Join(dir, "streamed.qcow2")
	_, err = vp.runHostStdin(ctx, []byte("disk"), writeVMDiskArgv(streamed)...)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), mode(streamed))

	vars := filepath.Join(dir, "vars.fd")
	_, err = runHost(ctx, vp, withUmask(clonedNVRAMUmask, "dd", "if="+src, "of="+vars, "conv=excl", "status=none")...)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), mode(vars))
}

func TestUnsafeDiskDirReason(t *testing.T) {
	for _, tc := range []struct {
		out, want string
	}{
		{"755 0\n1000\n", ""},
		{"711 0\n0\n", ""},
		{"755 1000\n1000\n", ""}, // owned by the SSH user
		{"1777 0\n1000\n", ""},   // sticky, like /tmp
		{"3770 0\n1000\n", ""},   // sticky + setgid, group-writable
		{"775 0\n1000\n", "is writable by its group"},
		{"757 0\n1000\n", "is writable by every user"},
		{"755 64055\n1000\n", "is writable by its owner (uid 64055)"},
		{"777 64055\n1000\n", "is writable by its owner (uid 64055) and its group and every user"},
		{"garbage", ""},
		{"", ""},
	} {
		assert.Equal(t, tc.want, unsafeDiskDirReason(tc.out), "%q", tc.out)
	}
}

// TestWarnIfDiskDirUnsafe_ChecksEachDirectoryOnce: the permission check runs
// on the host once per directory and provider process.
func TestWarnIfDiskDirUnsafe_ChecksEachDirectoryOnce(t *testing.T) {
	realID, err := exec.LookPath("id")
	require.NoError(t, err)
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "id.log")
	shim := "#!/bin/sh\necho id >> '" + calls + "'\nexec '" + realID + "' \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "id"), []byte(shim), 0o700)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	vp := NewVirshProvider(&ProviderConfig{})
	vp.uri = "test:///once"
	a, b := t.TempDir(), t.TempDir()
	for _, dir := range []string{a, a, b, a} {
		vp.warnIfDiskDirUnsafe(context.Background(), dir)
	}
	got, err := os.ReadFile(calls) //nolint:gosec // test reads its own log
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(got), "id\n"), "one check per directory")
}

// TestDiskWriteDir_ReplacesALinkPlantedAtTheFinalName runs a disk write with
// the real commands after a symbolic link was planted at its final name (as if
// between the target check and the write): the disk is written in a private
// directory (mode 0700) and renamed onto the name, which replaces the link
// without following it — the link's target is never written — and the private
// directory is removed.
func TestDiskWriteDir_ReplacesALinkPlantedAtTheFinalName(t *testing.T) {
	s := newSwapHost(t)
	ctx := context.Background()
	src := filepath.Join(s.dir, "src.qcow2")
	out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", src, "1M").CombinedOutput() //nolint:gosec // test-controlled paths
	require.NoError(t, err, "%s", out)
	pool := filepath.Join(s.dir, "pool")
	require.NoError(t, os.Mkdir(pool, 0o750))
	dst := filepath.Join(pool, "team-b.copy-disk.qcow2")
	require.NoError(t, os.Symlink(s.victim, dst))

	wd, err := newDiskWriteDir(ctx, s.vp, pool)
	require.NoError(t, err)
	fi, err := os.Stat(wd.dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm(), "private to the SSH user")
	assert.Equal(t, pool, filepath.Dir(wd.dir), "next to the final name: the rename never crosses a filesystem")
	_, err = runHost(ctx, s.vp, withUmask(vmDiskUmask, "qemu-img", "convert", "-O", "qcow2", src, wd.file("d.qcow2"))...)
	require.NoError(t, err)
	require.NoError(t, wd.publish(ctx, "d.qcow2", dst))
	wd.cleanup(ctx)

	fi, err = os.Lstat(dst)
	require.NoError(t, err)
	assert.True(t, fi.Mode().IsRegular(), "the link was replaced by the disk")
	assert.Equal(t, os.FileMode(0o640), fi.Mode().Perm())
	b, err := os.ReadFile(s.victim)
	require.NoError(t, err)
	assert.Equal(t, "root's file", string(b), "the link's target was never written")
	_, err = os.Stat(wd.dir)
	assert.True(t, os.IsNotExist(err), "the private directory is removed")

	// The whole full-clone path: the same, through createFullCopy.
	dst2 := filepath.Join(pool, "team-c.copy-disk.qcow2")
	require.NoError(t, os.Symlink(s.victim, dst2))
	require.NoError(t, createFullCopy(ctx, s.vp, src, "qcow2", dst2))
	s.requireVictimUntouched(t)
	entries, err := os.ReadDir(pool)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), vmDiskWriteDirPrefix, "no private directory left behind")
	}
}

// TestEnsureDiskTargetFree_Kinds: a free path passes, an unused regular file
// is a leftover to replace, a symbolic link — dangling or not — is refused.
func TestEnsureDiskTargetFree_Kinds(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	ctx := context.Background()
	subject := domainDiskSubject("team-a.web")

	require.NoError(t, ensureDiskTargetFree(ctx, vp, subject, filepath.Join(h.images, "free.qcow2")))
	require.NoError(t, ensureDiskTargetFree(ctx, vp, subject, h.file(h.images, "stale.qcow2")))

	live := filepath.Join(h.images, "live.qcow2")
	require.NoError(t, os.Symlink(h.file(h.outside, "keys"), live))
	dangling := filepath.Join(h.images, "dangling.qcow2")
	require.NoError(t, os.Symlink(filepath.Join(h.outside, "nothing"), dangling))
	for _, p := range []string{live, dangling} {
		err := ensureDiskTargetFree(ctx, vp, subject, p)
		require.Error(t, err)
		assert.True(t, contracts.IsConflict(err), "%v", err)
		assert.NotContains(t, err.Error(), h.base)
	}
}
