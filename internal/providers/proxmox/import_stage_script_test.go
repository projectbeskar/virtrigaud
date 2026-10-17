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

	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
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

// TestImportStageWriteExactly_RealShell (rc2 R6 audit): the s3 stage is
// published only when exactly the expected number of bytes arrived. A copy
// cut short — a cancelled call closes the node's stdin, which cat sees as end
// of input, and no hang-up reaches the non-pty command — leaves neither stage
// nor temp, and an earlier stage is kept.
func TestImportStageWriteExactly_RealShell(t *testing.T) {
	requireGNUStat(t)
	for _, shell := range localShells(t) {
		t.Run(shell, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "import-stage")
			stage := filepath.Join(dir, "team-a_web-migrated.qcow2")
			run := func(stdin string, size int64) error {
				cmd := exec.Command(shell, "-c", buildImportStageScript(dir, stage, importStageWriteExactly(size)))
				cmd.Stdin = strings.NewReader(stdin)
				return cmd.Run()
			}
			require.NoError(t, run("disk-bytes", int64(len("disk-bytes"))))
			got, err := os.ReadFile(stage)
			require.NoError(t, err)
			assert.Equal(t, "disk-bytes", string(got))

			err = run("disk", int64(len("disk-bytes-v2")))
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit)
			assert.Equal(t, importStageShortCopyExit, exit.ExitCode())
			got, err = os.ReadFile(stage)
			require.NoError(t, err)
			assert.Equal(t, "disk-bytes", string(got), "a short copy is never published")
			assert.Equal(t, []string{"team-a_web-migrated.qcow2"}, dirEntries(t, dir), "no temp left")
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
				nfsImportStageInner(dir, "src.qcow2", stage, nil, nil, testCopyLimit)
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
					nfsImportStageInner(dir, rel, stage, &uid, &gid, testCopyLimit)
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
	s := nfsImportStageInner(proxmoxImportStageDir, "ns/web/disk.qcow2", importStagePath("team-a", "web", "qcow2"), &uid, &gid, 5000)
	steps := []string{
		"install -d -m 0700 '/var/lib/virtrigaud/import-stage'",
		`TMP=$(mktemp -p '/var/lib/virtrigaud/import-stage' .stage-XXXXXXXXXX)`,
		// Measured as the NFS identity; bound and space checked by root.
		`SZ=$(setpriv --reuid 4242 --regid 4343 --clear-groups sh -c `,
		` sh "$MNT"/'ns/web/disk.qcow2'); `,
		`if [ "$SZ" -gt 5000 ]; then`,
		`AV=$(df -P -B1 -- "${TMP%/*}" | awk 'NR==2 {print $4}')`,
		`if [ "$AV" -lt $((SZ * 2)) ]; then`,
		`RC=$(mktemp -p "${TMP%/*}" .rc-XXXXXXXXXX)`,
		`{ if setpriv --reuid 4242 --regid 4343 --clear-groups cat -- "$MNT"/'ns/web/disk.qcow2' 3>&-; then echo 0 >&3; else echo 1 >&3; fi; } 3>"$RC" | head -c $((SZ + 1)) > "$TMP"`,
		`if [ "$(stat -c %s -- "$TMP")" -gt "$SZ" ]; then`,
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

	assert.NotContains(t, nfsStageCopyMeasure, "df ", "the NFS identity never measures the stage directory")
	full := buildNFSImportStageScript(proxmoxImportStageDir, "nfs.example", "/export", "ns/web/disk.qcow2",
		importStagePath("team-a", "web", "qcow2"), &uid, &gid, 5000)
	assert.Contains(t, full, nfsImportStageCleanup, "the trap removes the temp and the status file")
}

// testCopyLimit is the copy bound of the real-shell stage tests.
const testCopyLimit = int64(1) << 30

// fakeToolsDir puts fake commands first on PATH: `setpriv` drops its five
// arguments (as fakeSetprivDir) and, when it runs cat, appends grow bytes
// after the file (a server serving more than it reported); `df`, when dfAvail
// is set, reports dfAvail free bytes.
func fakeToolsDir(t *testing.T, grow, dfAvail string) string {
	t.Helper()
	bin := t.TempDir()
	setpriv := "#!/bin/sh\nshift 5\nif [ \"$1\" = cat ]; then \"$@\" || exit $?; printf %s " + proxmoxShellQuote(grow) + "; exit 0; fi\nexec \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "setpriv"), []byte(setpriv), 0o755)) //nolint:gosec // test helper must be executable
	if dfAvail != "" {
		df := "#!/bin/sh\necho 'Filesystem 1-blocks Used Available Capacity Mounted'\necho 'fake 1 1 " + dfAvail + " 1% /'\n"
		require.NoError(t, os.WriteFile(filepath.Join(bin, "df"), []byte(df), 0o755)) //nolint:gosec // test helper must be executable
	}
	return bin
}

// TestNFSStageCopy_RefusalsLeaveNothing (rebase item 6): every refusal or
// failure of the NFS stage copy exits with its status and leaves NOTHING in
// the stage directory — no stage, no temp, no status file: a symbolic link or
// a directory at the staging name (6), an object above the bound (7), one that
// grows past its measured size while it is copied (7, the copy is cut at
// size + 1), too little free space (8), a free space that is not a number
// (1), and a missing object.
func TestNFSStageCopy_RefusalsLeaveNothing(t *testing.T) {
	requireGNUStat(t)
	uid, gid := int64(4242), int64(4343)
	mnt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(mnt, "good.qcow2"), []byte("QFI\xfbdata"), 0o600))
	secret := filepath.Join(t.TempDir(), "authkey.key")
	require.NoError(t, os.WriteFile(secret, []byte("PRIVATE"), 0o600))
	require.NoError(t, os.Symlink(secret, filepath.Join(mnt, "link.qcow2")))
	require.NoError(t, os.Mkdir(filepath.Join(mnt, "dir.qcow2"), 0o700))

	for name, tc := range map[string]struct {
		rel     string
		limit   int64
		grow    string
		dfAvail string
		exit    int
		stderr  string
	}{
		"symbolic link":           {rel: "link.qcow2", limit: testCopyLimit, exit: nfsNotRegularFileExit, stderr: nfsNotRegularFileMessage},
		"directory":               {rel: "dir.qcow2", limit: testCopyLimit, exit: nfsNotRegularFileExit, stderr: nfsNotRegularFileMessage},
		"above the bound":         {rel: "good.qcow2", limit: 4, exit: nfsTooLargeExit, stderr: migration.ErrStagedObjectTooLarge.Error()},
		"grows while copied":      {rel: "good.qcow2", limit: testCopyLimit, grow: "and a great deal more", exit: nfsTooLargeExit, stderr: migration.ErrStagedObjectTooLarge.Error()},
		"no room for two copies":  {rel: "good.qcow2", limit: testCopyLimit, dfAvail: "15", exit: nfsNoSpaceExit, stderr: migration.ErrStagingSpaceExhausted.Error()},
		"free space not a number": {rel: "good.qcow2", limit: testCopyLimit, dfAvail: "-", exit: 1},
		"missing staged object":   {rel: "missing.qcow2", limit: testCopyLimit, exit: -1},
	} {
		for _, shell := range localShells(t) {
			t.Run(name+"/"+shell, func(t *testing.T) {
				bin := fakeToolsDir(t, tc.grow, tc.dfAvail)
				dir := filepath.Join(t.TempDir(), "import-stage")
				stage := filepath.Join(dir, "team-a_web-migrated.qcow2")
				script := "set -e; MNT=" + proxmoxShellQuote(mnt) + "; trap '" + nfsImportStageCleanup + "' EXIT; " +
					nfsImportStageInner(dir, tc.rel, stage, &uid, &gid, tc.limit)
				cmd := exec.Command(shell, "-c", script) //nolint:gosec // test runs the script under test
				cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
				out, err := cmd.CombinedOutput()
				var ee *exec.ExitError
				require.ErrorAs(t, err, &ee, string(out))
				if tc.exit >= 0 {
					assert.Equal(t, tc.exit, ee.ExitCode(), string(out))
				}
				if tc.stderr != "" {
					assert.Contains(t, string(out), tc.stderr)
				}
				assert.NotContains(t, string(out), "PRIVATE", "a linked file is never read")
				assert.Empty(t, dirEntries(t, dir), "no stage, temp or status file")
			})
		}
	}

	// The same copy with room and an object as reported is published.
	for _, shell := range localShells(t) {
		bin := fakeToolsDir(t, "", "64")
		dir := filepath.Join(t.TempDir(), "import-stage")
		stage := filepath.Join(dir, "team-a_web-migrated.qcow2")
		script := "set -e; MNT=" + proxmoxShellQuote(mnt) + "; trap '" + nfsImportStageCleanup + "' EXIT; " +
			nfsImportStageInner(dir, "good.qcow2", stage, &uid, &gid, 8)
		cmd := exec.Command(shell, "-c", script) //nolint:gosec // test runs the script under test
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		got, err := os.ReadFile(stage) //nolint:gosec // test reads its fixture
		require.NoError(t, err)
		assert.Equal(t, "QFI\xfbdata", string(got), "an object exactly at the bound is copied")
		assert.Equal(t, []string{"team-a_web-migrated.qcow2"}, dirEntries(t, dir))
	}
}
