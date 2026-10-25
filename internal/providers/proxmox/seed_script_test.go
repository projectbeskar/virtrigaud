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
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The seed script (buildSeedScript) is run here with the local POSIX sh and
// bash, GNU coreutils/findutils/tar as on a PVE node, and stub qm,
// genisoimage and pgrep commands that model a node: `qm config` prints a
// config file, `qm importdisk` adds an unusedN volume, `qm set` attaches a
// volume (dropping its unusedN entry, as qemu-server does) or deletes an
// unusedN key, genisoimage copies the files it is given so the test can read
// them. PATH holds only those stubs and links to the real tools the script
// needs.

// seedScriptTools are the real commands the seed script runs.
var seedScriptTools = []string{"install", "stat", "id", "find", "rm", "mktemp", "mkdir", "tar", "sed", "cat", "cp", "mv", "grep", "sh", "echo", "flock", "sleep"}

const stubQM = `#!/bin/sh
echo "$*" >> "$QM_LOG"
case "$1" in
config) cat "$QM_STATE" ;;
importdisk)
  [ -f "$3" ] || { echo "no such file $3" >&2; exit 2; }
  [ -z "$QM_IMPORT_FAIL" ] || exit 255
  n=0; while grep -q "^unused$n:" "$QM_STATE"; do n=$((n+1)); done
  echo "unused$n: $4:vm-$2-disk-$QM_DISK_INDEX" >> "$QM_STATE"
  [ -z "$QM_IMPORT_TWICE" ] || { n=$((n+1)); echo "unused$n: $4:vm-$2-disk-99" >> "$QM_STATE"; }
  ;;
set)
  if [ "$3" = "--delete" ]; then
    grep -v "^$4:" "$QM_STATE" > "$QM_STATE.new"; mv "$QM_STATE.new" "$QM_STATE"; exit 0
  fi
  [ -z "$QM_SET_FAIL" ] || exit 255
  slot=${3#--}; vol=${4%%,*}; del=x
  [ "$5" != "--delete" ] || del=$6
  grep -v "^unused[0-9]*: $vol\$" "$QM_STATE" | grep -v "^$slot:" | grep -v "^$del:" > "$QM_STATE.new"; mv "$QM_STATE.new" "$QM_STATE"
  echo "$slot: $4" >> "$QM_STATE"
  ;;
esac
`

const stubGenisoimage = `#!/bin/sh
out=""; last=""
while [ $# -gt 0 ]; do
  case "$1" in -output) out=$2; shift ;; esac
  last=$1; shift
done
cp -R "$last/." "$ISO_CAPTURE/"
echo ISO > "$out"
`

const stubPgrep = `#!/bin/sh
exit ${PGREP_EXIT:-1}
`

// seedScriptEnv is one run's node: a stub bin directory, the qm state and
// log, the genisoimage capture directory, and the stage directory.
type seedScriptEnv struct {
	bin, state, log, capture, stage, conf string
	extra                                 []string
	// alsoDelete is the script's alsoDelete argument.
	alsoDelete string
}

// stageEntries lists the stage directory without the VMs' lock files.
func (e *seedScriptEnv) stageEntries(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, name := range dirEntries(t, e.stage) {
		if !strings.HasPrefix(name, ".lock-") {
			out = append(out, name)
		}
	}
	return out
}

func newSeedScriptEnv(t *testing.T, withGenisoimage bool) *seedScriptEnv {
	t.Helper()
	requireGNUStat(t)
	dir := t.TempDir()
	e := &seedScriptEnv{
		bin:     filepath.Join(dir, "bin"),
		state:   filepath.Join(dir, "qm.conf"),
		log:     filepath.Join(dir, "qm.log"),
		capture: filepath.Join(dir, "capture"),
		stage:   filepath.Join(dir, "seed-stage"),
		conf:    filepath.Join(dir, "qemu-server"),
	}
	require.NoError(t, os.MkdirAll(e.conf, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(e.conf, "4242.conf"), nil, 0o600))
	require.NoError(t, os.MkdirAll(e.bin, 0o755))
	require.NoError(t, os.MkdirAll(e.capture, 0o755))
	for _, tool := range seedScriptTools {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not available", tool)
		}
		require.NoError(t, os.Symlink(path, filepath.Join(e.bin, tool)))
	}
	if path, err := exec.LookPath("bash"); err == nil {
		require.NoError(t, os.Symlink(path, filepath.Join(e.bin, "bash")))
	}
	stubs := map[string]string{"qm": stubQM, "pgrep": stubPgrep}
	if withGenisoimage {
		stubs["genisoimage"] = stubGenisoimage
	}
	for name, body := range stubs {
		require.NoError(t, os.WriteFile(filepath.Join(e.bin, name), []byte(body), 0o755))
	}
	require.NoError(t, os.WriteFile(e.state, []byte("boot: order=scsi0\nscsi0: tank:vm-4242-disk-0,size=8G\nunused0: tank:vm-4242-disk-7\n"), 0o600))
	return e
}

// run runs the seed script for VM 4242 (slot ide2, storage tank) with shell,
// archive on stdin.
func (e *seedScriptEnv) run(t *testing.T, shell string, archive []byte) (string, string, error) {
	t.Helper()
	cmd := exec.Command(filepath.Join(e.bin, shell), "-c", buildSeedScript(e.stage, e.conf, 4242, "tank", "ide2", e.alsoDelete))
	cmd.Env = append([]string{"PATH=" + e.bin, "QM_STATE=" + e.state, "QM_LOG=" + e.log,
		"ISO_CAPTURE=" + e.capture, "QM_DISK_INDEX=1"}, e.extra...)
	cmd.Stdin = bytes.NewReader(archive)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	return out.String(), errOut.String(), err
}

func (e *seedScriptEnv) qmState(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(e.state)
	require.NoError(t, err)
	return string(data)
}

func seedScriptShells(t *testing.T) []string {
	t.Helper()
	shells := []string{"sh"}
	if _, err := exec.LookPath("bash"); err == nil {
		shells = append(shells, "bash")
	}
	return shells
}

func scriptExitCode(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

// TestBuildSeedScript_RealShell: the files reach genisoimage byte for byte,
// the seed is imported once and attached as a CD-ROM in its slot (its
// unusedN entry gone, the VM's other unused disk kept), the volume is
// printed, the stage directory is 0700 and no build directory is left.
func TestBuildSeedScript_RealShell(t *testing.T) {
	for _, shell := range seedScriptShells(t) {
		for name, network := range map[string][]byte{"with network-config": []byte("version: 2\n"), "without": nil} {
			t.Run(shell+"/"+name, func(t *testing.T) {
				e := newSeedScriptEnv(t, true)
				content := seedContent{userData: seedUserData, metaData: []byte("instance-id: x\n"), networkConfig: network}
				archive, err := seedArchive(content)
				require.NoError(t, err)
				out, stderr, err := e.run(t, shell, archive)
				require.NoError(t, err, stderr)
				assert.Equal(t, "tank:vm-4242-disk-1\n", out)

				got, err := os.ReadFile(filepath.Join(e.capture, seedFileUserData))
				require.NoError(t, err)
				assert.Equal(t, seedUserData, got)
				got, err = os.ReadFile(filepath.Join(e.capture, seedFileMetaData))
				require.NoError(t, err)
				assert.Equal(t, "instance-id: x\n", string(got))
				_, statErr := os.Stat(filepath.Join(e.capture, seedFileNetworkConfig))
				assert.Equal(t, network == nil, os.IsNotExist(statErr))

				state := e.qmState(t)
				assert.Contains(t, state, "ide2: tank:vm-4242-disk-1,media=cdrom\n")
				assert.NotContains(t, state, "unused1:", "the seed's unused entry is gone")
				assert.Contains(t, state, "unused0: tank:vm-4242-disk-7\n", "another unused disk is kept")
				assert.Equal(t, os.FileMode(0o700), modeOf(t, e.stage))
				assert.Empty(t, e.stageEntries(t), "the build directory is removed")
			})
		}
	}
}

// TestBuildSeedScript_Failures: each refusal and failure exits with its code
// and leaves neither a build directory nor an imported volume behind.
func TestBuildSeedScript_Failures(t *testing.T) {
	archive, err := seedArchive(seedContent{userData: seedUserData, metaData: []byte("instance-id: x\n")})
	require.NoError(t, err)
	for _, shell := range seedScriptShells(t) {
		for name, tc := range map[string]struct {
			genisoimage bool
			notOnNode   bool
			env         []string
			stdin       []byte
			wantExit    int
			// wantUnusedLeft: an unidentified import is never deleted (it is
			// freed with the VM).
			wantUnusedLeft bool
		}{
			"genisoimage missing":      {genisoimage: false, stdin: archive, wantExit: seedExitNoGenisoimage},
			"VM on another node":       {genisoimage: true, notOnNode: true, stdin: archive, wantExit: seedExitNotOnNode},
			"importdisk still running": {genisoimage: true, env: []string{"PGREP_EXIT=0"}, stdin: archive, wantExit: seedExitImportBusy},
			"import failed (qm 255)":   {genisoimage: true, env: []string{"QM_IMPORT_FAIL=1"}, stdin: archive, wantExit: seedExitQMImport},
			"import added two volumes": {genisoimage: true, env: []string{"QM_IMPORT_TWICE=1"}, stdin: archive, wantExit: seedExitImportUnclear, wantUnusedLeft: true},
			"attach failed (qm 255)":   {genisoimage: true, env: []string{"QM_SET_FAIL=1"}, stdin: archive, wantExit: seedExitQMSet},
			"truncated archive":        {genisoimage: true, stdin: archive[:700], wantExit: -2},
		} {
			t.Run(shell+"/"+name, func(t *testing.T) {
				e := newSeedScriptEnv(t, tc.genisoimage)
				e.extra = tc.env
				if tc.notOnNode {
					require.NoError(t, os.Remove(filepath.Join(e.conf, "4242.conf")))
				}
				_, stderr, err := e.run(t, shell, tc.stdin)
				require.Error(t, err)
				if tc.wantExit == -2 {
					assert.NotZero(t, scriptExitCode(err), stderr)
				} else {
					assert.Equal(t, tc.wantExit, scriptExitCode(err), stderr)
				}
				state := e.qmState(t)
				if tc.wantUnusedLeft {
					assert.Contains(t, state, "unused1: tank:vm-4242-disk-1\n", "an unidentified import is not deleted")
					assert.Contains(t, state, "unused2: tank:vm-4242-disk-99\n")
				} else {
					assert.NotContains(t, state, "vm-4242-disk-1", "no seed volume is left on the VM")
				}
				assert.NotContains(t, state, "ide2:")
				assert.Contains(t, state, "unused0: tank:vm-4242-disk-7\n", "the VM's own unused disk is never deleted")
				if _, statErr := os.Stat(e.stage); statErr == nil {
					assert.Empty(t, e.stageEntries(t), "the build directory is removed")
				}
				noSecrets(t, stderr)
			})
		}
	}
}

// TestBuildSeedScript_SweepsStaleBuilds: a build directory a killed script
// left behind for more than an hour is removed; a recent one is kept.
func TestBuildSeedScript_SweepsStaleBuilds(t *testing.T) {
	e := newSeedScriptEnv(t, true)
	require.NoError(t, os.MkdirAll(filepath.Join(e.stage, ".seed-stale"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(e.stage, ".seed-recent"), 0o700))
	require.NoError(t, os.Chmod(e.stage, 0o700))
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(e.stage, ".seed-stale"), old, old))
	archive, err := seedArchive(seedContent{userData: []byte("#cloud-config\n"), metaData: []byte("instance-id: x\n")})
	require.NoError(t, err)
	_, stderr, err := e.run(t, "sh", archive)
	require.NoError(t, err, stderr)
	assert.Equal(t, []string{".seed-recent"}, e.stageEntries(t))
	assert.False(t, strings.Contains(e.qmState(t), "unused1"))
}
