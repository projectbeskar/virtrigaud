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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureImagesDir is the storage pool / allowed image directory of the
// routing and ops fixtures (newRoutingFixture, newOpsFixture): a scratch
// directory this test binary creates and removes, so no test runs a host
// command against the real /var/lib/libvirt/images. It is not below a
// forbiddenImageDirRoots entry (t.TempDir lives in /tmp, which the policy
// never accepts as an image directory), like scratchBase.
var fixtureImagesDir string

// TestMain creates fixtureImagesDir and the fixture disk paths inside it
// before any test runs, and removes it afterwards. It also installs the host
// guard (installHostGuard): the machine these tests run on may be a real
// libvirt host, so no test may run sudo, ssh or a real virsh, or run any host
// tool on a path under the real /var/lib/libvirt. A guard hit fails the run.
func TestMain(m *testing.M) {
	dir, err := newFixtureImagesDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "libvirt tests: %v\n", err)
		os.Exit(1)
	}
	guardDir, guardLog, err := installHostGuard()
	if err != nil {
		fmt.Fprintf(os.Stderr, "libvirt tests: install the host guard: %v\n", err)
		os.Exit(1)
	}
	fixtureImagesDir = dir
	routingDiskPath = filepath.Join(dir, "web-disk.qcow2")
	opsDiskPath = filepath.Join(dir, opsDomainName+"-disk.qcow2")
	code := m.Run()
	_ = os.RemoveAll(dir)
	if hits, _ := os.ReadFile(guardLog); len(hits) > 0 { //nolint:gosec // the test binary's own guard log
		fmt.Fprintf(os.Stderr, "libvirt tests: the host guard refused commands that would have touched the real host:\n%s", hits)
		code = 1
	}
	_ = os.RemoveAll(guardDir)
	os.Exit(code)
}

// hostGuardEnvLog names the file the host guard's shims record refused
// commands in.
const hostGuardEnvLog = "VIRTRIGAUD_TEST_HOST_GUARD_LOG"

// realHostPath is the host directory no test may run a host tool on.
const realHostPath = "/var/lib/libvirt"

// guardedHostTools are the host tools the provider (or a fixture) may run by
// name: each gets a shim first on PATH, behind every fixture's own fakes
// (fixtures prepend theirs), so a command a fixture does not fake reaches the
// shim instead of the real tool.
var guardedHostTools = []string{
	"sh", "bash", "dash", "qemu-img", "mktemp", "mv", "rm", "cp", "chmod", "chown", "cat", "stat", "realpath",
	"readlink", "ls", "mkdir", "rmdir", "touch", "ln", "dd", "test", "flock", "timeout", "id", "sha256sum",
	"restorecon", "genisoimage", "mkisofs", "xorriso", "cloud-localds", "wget", "curl", "du", "df", "truncate",
	"tee", "find", "install",
}

// neverRunHostTools are refused whatever their arguments: they would act as
// root, or on another host.
var neverRunHostTools = []string{"sudo", "ssh", "scp", "sshpass"}

// hostGuardEnvSelfTest, when set, makes a shim refuse without recording: only
// TestHostGuard_RefusesTheRealHost sets it, to prove the guard is in place.
const hostGuardEnvSelfTest = "VIRTRIGAUD_TEST_HOST_GUARD_SELFTEST"

// hostGuardRecord is the shim fragment that records a refused call (%[1]s is
// the tool's name), unless the guard's own self-test runs.
const hostGuardRecord = `[ -n "$` + hostGuardEnvSelfTest + `" ] || printf '%%s %%s\n' '%[1]s' "$*" >> "$` + hostGuardEnvLog + `"`

// hostGuardShim is the shim of a guarded tool: it refuses (records and exits
// 1) any call with an argument naming realHostPath, and otherwise runs the
// real tool (%[2]s) under its own name, so its messages read as before. %[1]s
// is the tool's name; the shell is bash for `exec -a` (the shim of bash
// itself included: its interpreter is named absolutely, never looked up).
const hostGuardShim = `#!/bin/bash
for a in "$@"; do
  case "$a" in *"` + realHostPath + `"*)
    ` + hostGuardRecord + `
    echo "virtrigaud test guard: %[1]s on the real ` + realHostPath + ` refused" >&2
    exit 1 ;;
  esac
done
exec -a '%[1]s' '%[2]s' "$@"
`

// hostGuardRefuseShim is the shim of a tool that never runs (%[1]s).
const hostGuardRefuseShim = `#!/bin/sh
` + hostGuardRecord + `
echo "virtrigaud test guard: %[1]s must not run in tests" >&2
exit 1
`

// hostGuardVirshShim runs the real virsh (%[2]s) only on libvirt's in-process
// test driver (-c test:///...), and refuses anything else: qemu:///system on
// this machine may be a real host's. %[1]s is "virsh".
const hostGuardVirshShim = `#!/bin/bash
case "$1 $2" in "-c test:///"*) exec -a '%[1]s' '%[2]s' "$@" ;; esac
` + hostGuardRecord + `
echo "virtrigaud test guard: a real virsh outside test:/// must not run in tests" >&2
exit 1
`

// installHostGuard writes the guard's shims to a new directory, puts it first
// on PATH and points hostGuardEnvLog at a new log. It returns the directory
// and the log.
func installHostGuard() (string, string, error) {
	dir, err := os.MkdirTemp("", "virtrigaud-host-guard-")
	if err != nil {
		return "", "", err
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		return "", "", err
	}
	logPath := filepath.Join(dir, "refused.log")
	_, bashErr := os.Stat("/bin/bash")
	write := func(name, script string) error {
		if bashErr != nil {
			// No bash for `exec -a`: a POSIX shim runs the real tool by path.
			script = strings.Replace(script, "#!/bin/bash", "#!/bin/sh", 1)
			script = strings.ReplaceAll(script, "exec -a '"+name+"' ", "exec ")
		}
		return os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700) //nolint:gosec // test shim must be executable
	}
	for _, tool := range guardedHostTools {
		real, err := exec.LookPath(tool)
		if err != nil {
			continue // not installed: nothing to guard
		}
		if err := write(tool, fmt.Sprintf(hostGuardShim, tool, real)); err != nil {
			return "", "", err
		}
	}
	for _, tool := range neverRunHostTools {
		if err := write(tool, fmt.Sprintf(hostGuardRefuseShim, tool)); err != nil {
			return "", "", err
		}
	}
	virsh := "/nonexistent/virsh"
	if real, err := exec.LookPath("virsh"); err == nil {
		virsh = real
	}
	if err := write("virsh", fmt.Sprintf(hostGuardVirshShim, "virsh", virsh)); err != nil {
		return "", "", err
	}
	if err := os.Setenv(hostGuardEnvLog, logPath); err != nil {
		return "", "", err
	}
	if err := os.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		return "", "", err
	}
	return dir, logPath, nil
}

// newFixtureImagesDir makes a canonical scratch directory under the package
// directory (or the home directory) that the image policy accepts.
func newFixtureImagesDir() (string, error) {
	var candidates []string
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, home)
	}
	for _, parent := range candidates {
		dir, err := os.MkdirTemp(parent, "_fixture_images_")
		if err != nil {
			continue
		}
		canon, err := filepath.EvalSymlinks(dir)
		if err == nil && !isForbiddenImageDir(canon) {
			return canon, nil
		}
		_ = os.RemoveAll(dir)
	}
	return "", fmt.Errorf("no writable scratch directory outside the forbidden image-dir roots")
}

// useFixtureImages points the image-directory policy at fixtureImagesDir and
// creates the fixture disks (an existing, regular file is what Delete removes).
func useFixtureImages(t *testing.T) {
	t.Helper()
	t.Setenv(EnvImageDirs, fixtureImagesDir)
	t.Setenv("FAKE_POOL_DIR", fixtureImagesDir)
	for _, p := range []string{routingDiskPath, opsDiskPath} {
		if err := os.WriteFile(p, []byte("disk"), 0o600); err != nil {
			t.Fatalf("create fixture disk %s: %v", p, err)
		}
	}
}
