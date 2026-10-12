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

// TestNFSImportStageInner_RealShell: the NFS stage (without a uid/gid, so
// qemu-img runs as the SSH user) converts "$MNT"/rel through a temp file onto
// the stage, which passes the node check, and leaves nothing else. It needs a
// local qemu-img.
func TestNFSImportStageInner_RealShell(t *testing.T) {
	requireGNUStat(t)
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not available")
	}
	mnt := t.TempDir()
	require.NoError(t, exec.Command("qemu-img", "create", "-q", "-f", "qcow2", filepath.Join(mnt, "src.qcow2"), "1M").Run())
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
			require.NoError(t, exec.Command(shell, "-c", importStageNodeCheck(stage)).Run())
			require.NoError(t, exec.Command("qemu-img", "check", "-q", stage).Run())
		})
	}
}

// TestNFSImportStageInner_SetprivShape pins the order of the uid/gid variant
// (it needs root to run): the identity converts only inside its own work
// directory, and the result is moved into the root-only directory, checked,
// re-owned and only then renamed onto the stage.
func TestNFSImportStageInner_SetprivShape(t *testing.T) {
	uid, gid := int64(4242), int64(4343)
	s := nfsImportStageInner(proxmoxImportStageDir, "ns/web/disk.qcow2", importStagePath("team-a", "web", "qcow2"), &uid, &gid)
	steps := []string{
		"install -d -m 0700 '/var/lib/virtrigaud/import-stage'",
		`TMP=$(mktemp -p '/var/lib/virtrigaud/import-stage' .stage-XXXXXXXXXX)`,
		`W=$(mktemp -d /var/tmp/.virtrigaud-nfsimport-XXXXXX); chmod 0711 "$W"; mkdir -m 0700 "$W/out"; chown 4242:4343 "$W/out"`,
		`setpriv --reuid 4242 --regid 4343 --clear-groups qemu-img convert -O qcow2 "$MNT"/'ns/web/disk.qcow2' "$W/out/disk.qcow2"`,
		`mv -T -- "$W/out/disk.qcow2" "$TMP"`,
		`[ -f "$TMP" ] && [ ! -L "$TMP" ] && [ "$(stat -c %h -- "$TMP")" = 1 ] || exit 1`,
		`chown "$(id -u):$(id -g)" -- "$TMP"; chmod 0600 -- "$TMP"`,
		`mv -T -- "$TMP" '/var/lib/virtrigaud/import-stage/team-a_web-migrated.qcow2'`,
	}
	at := 0
	for _, step := range steps {
		i := strings.Index(s[at:], step)
		require.GreaterOrEqual(t, i, 0, "missing or out of order: %s\nin: %s", step, s)
		at += i + len(step)
	}
	assert.NotContains(t, s, "/var/tmp/.virtrigaud-import-", "never stages in /var/tmp")

	full := buildNFSImportStageScript(proxmoxImportStageDir, "nfs.example", "/export", "ns/web/disk.qcow2",
		importStagePath("team-a", "web", "qcow2"), &uid, &gid)
	assert.Contains(t, full, nfsImportStageCleanup, "the trap removes the temp and the work directory")
}
