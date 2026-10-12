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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The stage-writing scripts are run here against a local directory with the
// local POSIX sh and bash (GNU coreutils, as on a PVE node). Only the stage
// directory differs from production (a parameter of the builders).

// requireGNUStat skips a test that runs node-side shell on a system without
// GNU stat (-c).
func requireGNUStat(t *testing.T) {
	t.Helper()
	if err := exec.Command("stat", "-c", "%h", "/").Run(); err != nil {
		t.Skip("GNU stat not available")
	}
}

// localShells are the shells the node-side scripts are run with.
func localShells(t *testing.T) []string {
	t.Helper()
	var shells []string
	for _, s := range []string{"sh", "bash"} {
		if _, err := exec.LookPath(s); err == nil {
			shells = append(shells, s)
		}
	}
	if len(shells) == 0 {
		t.Skip("no local shell")
	}
	return shells
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	require.NoError(t, err)
	return fi.Mode().Perm()
}

// TestBuildImportStageScript_RealShell: the S3 stage script creates the stage
// directory 0700 (resetting an existing one), writes stdin through a mktemp
// file, renames it onto the stage (0600, passing the node check), and leaves
// nothing else; a failed write leaves neither stage nor temp; a stage
// directory that is a symbolic link is refused before anything is written.
func TestBuildImportStageScript_RealShell(t *testing.T) {
	requireGNUStat(t)
	for _, shell := range localShells(t) {
		t.Run(shell, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "import-stage")
			stage := filepath.Join(dir, "team-a_web-migrated.qcow2")
			cmd := exec.Command(shell, "-c", buildImportStageScript(dir, stage, `cat > "$TMP"`))
			cmd.Stdin = strings.NewReader("disk-bytes")
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, string(out))

			got, err := os.ReadFile(stage)
			require.NoError(t, err)
			assert.Equal(t, "disk-bytes", string(got))
			assert.Equal(t, os.FileMode(0o700), modeOf(t, dir))
			assert.Equal(t, os.FileMode(0o600), modeOf(t, stage))
			assert.Equal(t, []string{"team-a_web-migrated.qcow2"}, dirEntries(t, dir), "no temp left")
			require.NoError(t, exec.Command(shell, "-c", importStageNodeCheck(stage)).Run(), "the stage passes the node check")

			// A re-run (retry) replaces the stage; an existing 0755 directory is reset to 0700.
			require.NoError(t, os.Chmod(dir, 0o755))
			cmd = exec.Command(shell, "-c", buildImportStageScript(dir, stage, `cat > "$TMP"`))
			cmd.Stdin = strings.NewReader("retry")
			require.NoError(t, cmd.Run())
			got, err = os.ReadFile(stage)
			require.NoError(t, err)
			assert.Equal(t, "retry", string(got))
			assert.Equal(t, os.FileMode(0o700), modeOf(t, dir))

			// A failed write leaves no stage and no temp.
			fresh := filepath.Join(t.TempDir(), "import-stage")
			freshStage := filepath.Join(fresh, "team-a_db-migrated.qcow2")
			cmd = exec.Command(shell, "-c", buildImportStageScript(fresh, freshStage, `cat > "$TMP"; false`))
			cmd.Stdin = strings.NewReader("partial")
			require.Error(t, cmd.Run())
			assert.Empty(t, dirEntries(t, fresh), "neither stage nor temp")

			// A stage directory that is a symbolic link is refused.
			target := t.TempDir()
			link := filepath.Join(t.TempDir(), "import-stage")
			require.NoError(t, os.Symlink(target, link))
			cmd = exec.Command(shell, "-c", buildImportStageScript(link, filepath.Join(link, "x_y-migrated.qcow2"), `cat > "$TMP"`))
			cmd.Stdin = strings.NewReader("x")
			require.Error(t, cmd.Run())
			assert.Empty(t, dirEntries(t, target), "nothing is written through a linked stage directory")
		})
	}
}

// TestNFSImportStageInner_RealShell: the NFS stage without a uid/gid (the root
// shell reads the export) copies "$MNT"/rel byte for byte through a temp file
// onto the stage, which passes the node check, and leaves nothing else.
func TestNFSImportStageInner_RealShell(t *testing.T) {
	requireGNUStat(t)
	mnt := t.TempDir()
	src := filepath.Join(mnt, "src.qcow2")
	require.NoError(t, os.WriteFile(src, []byte("QFI\xfb staged bytes"), 0o644))
	for _, shell := range localShells(t) {
		t.Run(shell, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "import-stage")
			stage := filepath.Join(dir, "team-a_web-migrated.qcow2")
			script := "set -e; MNT=" + proxmoxShellQuote(mnt) + "; trap '" + nfsImportStageCleanup + "' EXIT; " +
				nfsImportStageInner(dir, "src.qcow2", stage, nil, nil)
			out, err := exec.Command(shell, "-c", script).CombinedOutput()
			require.NoError(t, err, string(out))
			assert.Equal(t, []string{"team-a_web-migrated.qcow2"}, dirEntries(t, dir))
			assert.Equal(t, os.FileMode(0o600), modeOf(t, stage))
			got, err := os.ReadFile(stage)
			require.NoError(t, err)
			assert.Equal(t, "QFI\xfb staged bytes", string(got), "a byte-for-byte copy")
			require.NoError(t, exec.Command(shell, "-c", importStageNodeCheck(stage)).Run())
		})
	}
}

// fakeSetprivDir puts a fake `setpriv` first on PATH: it records the file
// descriptors it holds (its /proc/self/fd links) in $FDLOG, drops the five
// setpriv arguments nfsSetprivPrefix passes (--reuid U --regid G
// --clear-groups) and runs the rest. A real setpriv needs root.
func fakeSetprivDir(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("no /proc")
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nfor f in /proc/$$/fd/*; do readlink \"$f\"; done > \"$FDLOG\" 2>/dev/null\nshift 5\nexec \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "setpriv"), []byte(script), 0o755)) //nolint:gosec // test helper must be executable
	return bin
}

// TestNFSStageCopy_SetprivHoldsOnlyAPipe (review N1): with a uid/gid, the
// reader (setpriv) holds no file descriptor on the copy or anything in the
// stage directory — only the pipe — and the root side writes the copy, owned
// by the running user. A failed read fails the script and leaves neither the
// copy, its status file nor a stage.
func TestNFSStageCopy_SetprivHoldsOnlyAPipe(t *testing.T) {
	requireGNUStat(t)
	bin := fakeSetprivDir(t)
	uid, gid := int64(4242), int64(4343)
	mnt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(mnt, "src.qcow2"), []byte("tenant bytes"), 0o644))

	for _, shell := range localShells(t) {
		t.Run(shell, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "import-stage")
			stage := filepath.Join(dir, "team-a_web-migrated.qcow2")
			fdlog := filepath.Join(t.TempDir(), "fds")
			run := func(rel string) ([]byte, error) {
				script := "set -e; MNT=" + proxmoxShellQuote(mnt) + "; trap '" + nfsImportStageCleanup + "' EXIT; " +
					nfsImportStageInner(dir, rel, stage, &uid, &gid)
				cmd := exec.Command(shell, "-c", script)
				cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FDLOG="+fdlog)
				return cmd.CombinedOutput()
			}

			out, err := run("src.qcow2")
			require.NoError(t, err, string(out))
			got, err := os.ReadFile(stage)
			require.NoError(t, err)
			assert.Equal(t, "tenant bytes", string(got))
			assert.Equal(t, []string{"team-a_web-migrated.qcow2"}, dirEntries(t, dir), "no temp or status file left")
			require.NoError(t, exec.Command(shell, "-c", importStageNodeCheck(stage)).Run())

			fds, err := os.ReadFile(fdlog)
			require.NoError(t, err)
			require.NotEmpty(t, strings.TrimSpace(string(fds)))
			assert.NotContains(t, string(fds), dir, "the reader holds no descriptor in the stage directory")
			assert.Contains(t, string(fds), "pipe:", "the reader writes into a pipe")

			// A failed read (missing object) fails closed and leaves nothing.
			require.NoError(t, os.Remove(stage))
			out, err = run("missing.qcow2")
			require.Error(t, err, string(out))
			assert.Empty(t, dirEntries(t, dir), "no stage, temp or status file after a failed read")
		})
	}
}

// TestNFSStageCopy_Shape pins the uid/gid copy: the setpriv'd reader writes
// only into the pipe (fd 3 closed for it), the root side opens the copy, and
// the status check precedes the rename onto the stage.
func TestNFSStageCopy_Shape(t *testing.T) {
	uid, gid := int64(4242), int64(4343)
	s := nfsImportStageInner(proxmoxImportStageDir, "ns/web/disk.qcow2", importStagePath("team-a", "web", "qcow2"), &uid, &gid)
	steps := []string{
		"install -d -m 0700 '/var/lib/virtrigaud/import-stage'",
		`TMP=$(mktemp -p '/var/lib/virtrigaud/import-stage' .stage-XXXXXXXXXX)`,
		`RC=$(mktemp -p "${TMP%/*}" .rc-XXXXXXXXXX)`,
		`{ if setpriv --reuid 4242 --regid 4343 --clear-groups cat -- "$MNT"/'ns/web/disk.qcow2' 3>&-; then echo 0 >&3; else echo 1 >&3; fi; } 3>"$RC" | cat > "$TMP"`,
		`[ "$(cat -- "$RC")" = 0 ] || exit 1`,
		`mv -T -- "$TMP" '/var/lib/virtrigaud/import-stage/team-a_web-migrated.qcow2'`,
	}
	at := 0
	for _, step := range steps {
		i := strings.Index(s[at:], step)
		require.GreaterOrEqual(t, i, 0, "missing or out of order: %s\nin: %s", step, s)
		at += i + len(step)
	}
	assert.NotContains(t, s, "/var/tmp/", "nothing is written outside the stage directory")
	assert.NotContains(t, s, "chown", "no inode is adopted from the NFS identity")
	assert.NotContains(t, s, "qemu-img", "no qemu-img runs on the tenant's bytes in the copy")

	full := buildNFSImportStageScript(proxmoxImportStageDir, "nfs.example", "/export", "ns/web/disk.qcow2",
		importStagePath("team-a", "web", "qcow2"), &uid, &gid)
	assert.Contains(t, full, nfsImportStageCleanup, "the trap removes the temp and the status file")
}
