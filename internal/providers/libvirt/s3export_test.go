/*
Copyright 2025.

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
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestExportDiskToS3_NilProvider verifies the export path fails cleanly when no
// provider/virshProvider is wired rather than panicking. This fires before any
// host interaction, so it is host-independent.
func TestExportDiskToS3_NilProvider(t *testing.T) {
	s := &Server{}

	resp, err := s.exportDiskToS3(context.Background(), &providerv1.ExportDiskRequest{
		VmId:           "vm-1",
		DestinationUrl: "s3://bucket/disk.qcow2",
		BackendType:    "s3",
	})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "not initialized")
}

// TestExportDiskToS3_RequiresSSHTransport verifies the host-side flatten +
// stream-out flow refuses a non-ssh:// libvirt transport: the flatten
// (`qemu-img convert`) and the stream (`cat <hostTmp>`) both need an SSH
// connection to the libvirt host. The guard fires before any S3 client is built,
// so it is host-independent.
func TestExportDiskToS3_RequiresSSHTransport(t *testing.T) {
	vp := &VirshProvider{uri: "qemu:///system"}
	reg, err := hostconn.NewRegistry(newVirshConn("host-a", vp))
	require.NoError(t, err)
	s := &Server{
		provider: &Provider{registry: reg, hostID: "host-a"},
	}

	resp, err := s.exportDiskToS3(context.Background(), &providerv1.ExportDiskRequest{
		VmId:           "vm-1",
		DestinationUrl: "s3://bucket/disk.qcow2",
		BackendType:    "s3",
	})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "ssh://",
		"a local transport must be rejected: host-side flatten+stream needs SSH")
}

// TestHostExportStageName verifies the transient flattened qcow2's name inside
// its private staging directory: dot-prefixed (hidden from a casual directory
// listing), carrying the VM id, ending in the staged object's format.
func TestHostExportStageName(t *testing.T) {
	const vm = "demo-ubuntu-libvirt"

	base := hostExportStageName(vm)

	assert.True(t, strings.HasPrefix(base, ".virtrigaud-export-"),
		"stage file must be dot-prefixed and namespaced; got base %q", base)
	assert.Contains(t, base, vm, "stage file name must carry the VM id; got base %q", base)
	assert.True(t, strings.HasSuffix(base, hostExportStageSuffix), "the staged object's format; got base %q", base)
	assert.Equal(t, ".qcow2", hostExportStageSuffix, "the staged-object suffix")
}

// TestHostExportStageName_SanitizedNameStaysContained verifies that a hostile
// VM id cannot escape the private staging directory: sanitizeVolumeName
// neutralizes path separators and "..", so the name is one path component.
func TestHostExportStageName_SanitizedNameStaysContained(t *testing.T) {
	stage := hostExportStageName("../../etc/evil")

	assert.False(t, strings.Contains(stage, ".."),
		"sanitized stage name must not contain a parent-dir traversal; got %q", stage)
	assert.NotContains(t, stage, "/", "the stage name is one path component; got %q", stage)
}

// fakeSudoAllow stands in for sudo in the real-command export tests: it logs
// its argv to $FAKE_SUDO_LOG and runs the command as the calling user (the
// tests are never root and never run the real sudo). fakeSudoRefuse answers
// as sudo does when no passwordless rule allows the command.
const (
	fakeSudoAllow = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_SUDO_LOG"
if [ "$1" = "-n" ]; then shift; fi
case "$1" in qemu-img|timeout) ;; *) echo "fake sudo: only qemu-img and timeout are run" >&2; exit 1 ;; esac
exec "$@"
`
	fakeSudoRefuse = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_SUDO_LOG"
echo "sudo: a password is required" >&2
exit 1
`
)

// installFakeSudo puts script first on PATH as sudo and returns the path of
// its log.
func installFakeSudo(t *testing.T, script string) string {
	t.Helper()
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "sudo"), []byte(script), 0o700)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	logPath := filepath.Join(t.TempDir(), "sudo.log")
	t.Setenv("FAKE_SUDO_LOG", logPath)
	return logPath
}

// TestFlattenForExport runs the export's flatten with the real commands
// (mktemp, qemu-img, rm; sudo is a fake that runs the command as the test
// user, or refuses): the staging file is made for this export alone, inside a
// private (0700) directory next to the source, is private (0600), holds the
// flattened disk, and is removed with its directory by its cleanup — even
// when the request was cancelled; a failed flatten leaves nothing behind. The
// flatten runs through `sudo -n` with the source format pinned when sudo
// allows it, and as the SSH user (the historical command) when it refuses.
func TestFlattenForExport(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not available")
	}
	for name, sudo := range map[string]string{
		"sudo allows the flatten": fakeSudoAllow,
		"sudo refuses":            fakeSudoRefuse,
	} {
		t.Run(name, func(t *testing.T) {
			sudoLog := installFakeSudo(t, sudo)
			vp := NewVirshProvider(&ProviderConfig{})
			vp.uri = "test:///export"
			dir := t.TempDir()
			// Writable by the test user (the SSH user) alone, whatever the
			// umask: a root copy refuses a group-writable directory.
			require.NoError(t, os.Chmod(dir, 0o700))
			src := filepath.Join(dir, "disk0.qcow2")
			out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", src, "1M").CombinedOutput() //nolint:gosec // test-controlled paths
			require.NoError(t, err, "%s", out)
			staged := func() []string {
				m, err := filepath.Glob(filepath.Join(dir, vmDiskWriteDirPrefix+"*"))
				require.NoError(t, err)
				return m
			}

			ctx, cancel := context.WithCancel(context.Background())
			tmp, cleanup, err := flattenForExport(ctx, vp, src, "qcow2", "vm-1", nil)
			require.NoError(t, err)
			stageDir := filepath.Dir(tmp)
			assert.Equal(t, dir, filepath.Dir(stageDir), "the private directory is next to the source disk")
			assert.Regexp(t, `^`+regexp.QuoteMeta(vmDiskWriteDirPrefix)+`[A-Za-z0-9]{10}$`, filepath.Base(stageDir))
			di, err := os.Stat(stageDir)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o700), di.Mode().Perm(), "the staging directory is private to the SSH user")
			assert.Equal(t, ".virtrigaud-export-vm-1.qcow2", filepath.Base(tmp))
			fi, err := os.Stat(tmp)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "a full copy of a VM's disk is private")
			info, err := exec.Command("qemu-img", "info", "--output=json", tmp).Output() //nolint:gosec // test-controlled path
			require.NoError(t, err)
			assert.Contains(t, string(info), `"format": "qcow2"`)

			// The flatten is always asked of sudo first (the chain was
			// verified); when sudo refuses, the historical flatten ran as
			// the SSH user and produced the same private, flattened file.
			b, _ := os.ReadFile(sudoLog) //nolint:gosec // test reads its own log
			assert.Contains(t, splitLines(string(b)), "-n qemu-img convert -U -f qcow2 -O qcow2 "+src+" "+tmp)

			tmp2, cleanup2, err := flattenForExport(ctx, vp, src, "qcow2", "vm-1", nil)
			require.NoError(t, err)
			assert.NotEqual(t, tmp, tmp2, "one staging file per export")
			cleanup2()

			cancel() // the request is gone: the cleanup still runs
			cleanup()
			assert.Empty(t, staged(), "removed")

			_, _, err = flattenForExport(context.Background(), vp, filepath.Join(dir, "missing.qcow2"), "qcow2", "vm-1", nil)
			require.Error(t, err)
			assert.Empty(t, staged(), "a failed flatten leaves nothing behind")
		})
	}
}

// TestStreamCmdQuotesPath verifies the export stream (conn.Stream(ctx, "cat",
// hostTmp)) reaches the remote shell with the host temp path shell-quoted, so a
// path with spaces (or shell metacharacters) is read from exactly the intended
// file. Stream flattens its argv with shellJoin.
func TestStreamCmdQuotesPath(t *testing.T) {
	hostTmp := "/var/lib/libvirt/images/.virtrigaud-export-my vm-1.qcow2"
	streamCmd, err := shellJoin([]string{"cat", hostTmp})
	require.NoError(t, err)

	assert.Equal(t, "cat '/var/lib/libvirt/images/.virtrigaud-export-my vm-1.qcow2'", streamCmd,
		"the cat source must be single-quoted so spaces don't split the path")
}
