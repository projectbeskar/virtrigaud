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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRealpath is the realpath of the fixtures whose host paths are
// /var/lib/libvirt strings (newSCDFixture, newRoutedSCD): such a path is
// answered lexically — as given with -m, missing with -e — and never
// resolved on the machine running the test, which may be a real libvirt
// host; any other path (a test's scratch directory) is resolved by the real
// realpath ($FAKE_REAL_REALPATH, the host guard's shim). It does not log, so
// the pinned call sequences are unchanged.
const fakeRealpath = `#!/bin/sh
z=0; e=0
while [ $# -gt 0 ]; do
  case "$1" in
    -z) z=1; shift ;;
    -e) e=1; shift ;;
    --) shift; break ;;
    -*) shift ;;
    *) break ;;
  esac
done
rc=0
for p in "$@"; do
  case "$p" in
    ` + realHostPath + `|` + realHostPath + `/*)
      if [ "$e" = 1 ]; then echo "realpath: $p: No such file or directory" >&2; rc=1; continue; fi
      out="$p" ;;
    *)
      if [ "$e" = 1 ]; then out=$("$FAKE_REAL_REALPATH" -e -- "$p") || { rc=1; continue; }
      else out=$("$FAKE_REAL_REALPATH" -m -- "$p") || { rc=1; continue; }; fi ;;
  esac
  if [ "$z" = 1 ]; then printf '%s\0' "$out"; else printf '%s\n' "$out"; fi
done
exit $rc
`

// installFakeRealpath puts fakeRealpath in bin (a fixture directory that goes
// first on PATH) and points it at the realpath found now — the host guard's
// shim, which refuses the real /var/lib/libvirt too.
func installFakeRealpath(t *testing.T, bin string) {
	t.Helper()
	real, err := exec.LookPath("realpath")
	require.NoError(t, err)
	require.NotEqual(t, bin, filepath.Dir(real), "the real realpath is resolved before the fixture's bin is on PATH")
	require.NoError(t, os.WriteFile(filepath.Join(bin, "realpath"), []byte(fakeRealpath), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("FAKE_REAL_REALPATH", real)
}

// TestHostGuard_RefusesTheRealHost proves the TestMain host guard is in
// place: sudo, ssh and a real virsh never run, and a host tool is refused on
// any path under the real /var/lib/libvirt, while a scratch path passes. The
// self-test switch keeps these deliberate refusals out of the guard's log.
func TestHostGuard_RefusesTheRealHost(t *testing.T) {
	t.Setenv(hostGuardEnvSelfTest, "1")
	refused := func(name string, args ...string) {
		t.Helper()
		out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // the guard's own shims
		require.Error(t, err, "%s %v must be refused: %s", name, args, out)
		assert.Contains(t, string(out), "virtrigaud test guard", "%s %v", name, args)
	}
	refused("sudo", "-n", "true")
	refused("sudo", "-n", "qemu-img", "info", "-U", "--output=json", "/tmp/x")
	refused("ssh", "localhost", "true")
	refused("virsh", "-c", "qemu:///system", "list", "--all")
	refused("virsh", "list")
	refused("realpath", "-m", "-z", "--", realHostPath+"/images/x.qcow2")
	refused("stat", "-c", "%F", "--", realHostPath+"/images")
	refused("qemu-img", "info", "-U", "--output=json", "--", realHostPath+"/images/x.qcow2")
	refused("sh", "-c", `[ -e "$1" ]`, "sh", realHostPath)
	refused("mktemp", "-d", realHostPath+"/images/.virtrigaud-write-XXXXXXXXXX")

	for _, tool := range []string{"sudo", "virsh", "realpath", "sh", "qemu-img"} {
		p, err := exec.LookPath(tool)
		require.NoError(t, err)
		assert.Contains(t, p, "virtrigaud-host-guard-", "%s resolves to the guard's shim first", tool)
	}
	for _, tool := range guardedHostTools {
		p, err := exec.LookPath(tool)
		require.NoError(t, err, "every guarded tool has a shim, installed on this machine or not")
		assert.Contains(t, p, "virtrigaud-host-guard-", "%s resolves to the guard's shim first", tool)
	}
	scratch := t.TempDir()
	out, err := exec.Command("realpath", "-m", "--", scratch).CombinedOutput()
	require.NoError(t, err, "%s", out)
	assert.Equal(t, scratch, strings.TrimSpace(string(out)), "a scratch path runs the real tool")
	out, err = exec.Command("sh", "-c", "echo ok").CombinedOutput()
	require.NoError(t, err, "%s", out)
	assert.Equal(t, "ok", strings.TrimSpace(string(out)))
}

// TestHostGuard_MissingToolIsGuardedToo writes the guard's shims as for a
// machine that has none of the guarded tools (the CI runner has no qemu-img,
// for one): each tool still gets a shim that refuses the real host, and that
// otherwise fails as a missing command does (exit 127) — never a silent
// success, and never a real tool found further down PATH.
func TestHostGuard_MissingToolIsGuardedToo(t *testing.T) {
	t.Setenv(hostGuardEnvSelfTest, "1")
	bin := t.TempDir()
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	real, err := writeHostGuardShims(bin, missing)
	require.NoError(t, err)
	for _, tool := range guardedHostTools {
		assert.Empty(t, real[tool], "%s is reported missing", tool)
	}
	for _, tool := range []string{"qemu-img", "genisoimage", "flock", "curl"} {
		shim := filepath.Join(bin, tool)
		out, err := exec.Command(shim, "info", "--", realHostPath+"/images/x.qcow2").CombinedOutput() //nolint:gosec // the guard's own shim
		require.Error(t, err, "%s on the real host is refused: %s", tool, out)
		assert.Contains(t, string(out), "virtrigaud test guard: "+tool+" on the real "+realHostPath+" refused")

		out, err = exec.Command(shim, "--version").CombinedOutput() //nolint:gosec // the guard's own shim
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr, "%s: %s", tool, out)
		assert.Equal(t, hostGuardMissingExit, exitErr.ExitCode(), "a missing command's status")
		assert.Contains(t, string(out), "virtrigaud test guard: "+tool+" is not installed on this machine")
	}
	for _, tool := range neverRunHostTools {
		out, err := exec.Command(filepath.Join(bin, tool), "-n", "true").CombinedOutput() //nolint:gosec // the guard's own shim
		require.Error(t, err)
		assert.Contains(t, string(out), "virtrigaud test guard: "+tool+" must not run in tests")
	}
}
