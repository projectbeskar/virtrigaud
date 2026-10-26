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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildSeedScript_LockedWhileAnotherBuildRuns: a seed build of the same
// VM still running (holding its lock) makes the script exit busy before it
// runs any qm command.
func TestBuildSeedScript_LockedWhileAnotherBuildRuns(t *testing.T) {
	e := newSeedScriptEnv(t, true)
	require.NoError(t, os.MkdirAll(e.stage, 0o700))
	lock := filepath.Join(e.stage, seedLockName(4242))
	holder := exec.Command(filepath.Join(e.bin, "flock"), lock, filepath.Join(e.bin, "sleep"), "30")
	require.NoError(t, holder.Start())
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
	require.Eventually(t, func() bool {
		return exec.Command(filepath.Join(e.bin, "flock"), "-n", lock, filepath.Join(e.bin, "echo")).Run() != nil
	}, 5*time.Second, 20*time.Millisecond, "the holder takes the lock")

	archive, err := seedArchive(seedContent{userData: []byte("#cloud-config\n"), metaData: []byte("instance-id: x\n")})
	require.NoError(t, err)
	_, stderr, err := e.run(t, "sh", archive)
	require.Error(t, err)
	assert.Equal(t, seedExitImportBusy, scriptExitCode(err), stderr)
	_, statErr := os.Stat(e.log)
	assert.True(t, os.IsNotExist(statErr), "no qm command ran")
}

// TestBuildSeedScript_ReplacesACloudInitDriveInAnotherSlot: alsoDelete
// removes a PVE cloud-init drive the seed did not replace in its slot.
func TestBuildSeedScript_ReplacesACloudInitDriveInAnotherSlot(t *testing.T) {
	e := newSeedScriptEnv(t, true)
	require.NoError(t, os.WriteFile(e.state, []byte("scsi0: tank:vm-4242-disk-0\nide3: tank:vm-4242-cloudinit,media=cdrom\n"), 0o600))
	e.alsoDelete = "ide3"
	archive, err := seedArchive(seedContent{userData: []byte("#cloud-config\n"), metaData: []byte("instance-id: x\n")})
	require.NoError(t, err)
	_, stderr, err := e.run(t, "sh", archive)
	require.NoError(t, err, stderr)
	state := e.qmState(t)
	assert.Contains(t, state, "ide2: tank:vm-4242-disk-1,media=cdrom\n")
	assert.NotContains(t, state, "ide3:")
}

// runLockRemove runs seedLockRemoveScript for VM 4242 against e's stage
// directory with shell.
func (e *seedScriptEnv) runLockRemove(t *testing.T, shell string) error {
	t.Helper()
	cmd := exec.Command(filepath.Join(e.bin, shell), "-c", seedLockRemoveScript(e.stage, 4242))
	cmd.Env = []string{"PATH=" + e.bin}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("output: %s", out)
	}
	return err
}

// TestSeedLockRemoveScript pins rc1 finding O5: the lock file of a VM whose
// seed build is not running is removed; a held lock, a symbolic link and a
// missing file are left alone (exit 0 each time); a build after the removal
// works.
func TestSeedLockRemoveScript(t *testing.T) {
	for _, shell := range seedScriptShells(t) {
		t.Run(shell, func(t *testing.T) {
			e := newSeedScriptEnv(t, true)
			lock := filepath.Join(e.stage, seedLockName(4242))

			// Missing stage directory and file: nothing is created.
			require.NoError(t, e.runLockRemove(t, shell))
			_, err := os.Stat(e.stage)
			assert.True(t, os.IsNotExist(err), "nothing is created")

			// A build leaves its lock file behind.
			archive, err := seedArchive(seedContent{userData: []byte(seedEmptyUserData), metaData: []byte("instance-id: x\n")})
			require.NoError(t, err)
			_, stderr, err := e.run(t, shell, archive)
			require.NoError(t, err, stderr)
			require.FileExists(t, lock)

			// Held: left alone. (-o: the lock is held by flock itself, not by
			// sleep, so killing flock releases it.)
			holder := exec.Command(filepath.Join(e.bin, "flock"), "-o", lock, filepath.Join(e.bin, "sleep"), "30")
			require.NoError(t, holder.Start())
			require.Eventually(t, func() bool {
				return exec.Command(filepath.Join(e.bin, "flock"), "-n", lock, filepath.Join(e.bin, "echo")).Run() != nil
			}, 5*time.Second, 20*time.Millisecond, "the holder takes the lock")
			require.NoError(t, e.runLockRemove(t, shell))
			assert.FileExists(t, lock, "a held lock is not removed")
			_ = holder.Process.Kill()
			_ = holder.Wait()

			// Not held: removed.
			require.NoError(t, e.runLockRemove(t, shell))
			assert.NoFileExists(t, lock)

			// A symbolic link is left alone.
			target := filepath.Join(t.TempDir(), "target")
			require.NoError(t, os.WriteFile(target, nil, 0o600))
			require.NoError(t, os.Symlink(target, lock))
			require.NoError(t, e.runLockRemove(t, shell))
			assert.FileExists(t, target)
			_, err = os.Lstat(lock)
			require.NoError(t, err, "the link is kept")
			require.NoError(t, os.Remove(lock))

			// A build after the removal makes a new lock and works.
			_, stderr, err = e.run(t, shell, archive)
			require.NoError(t, err, stderr)
			assert.FileExists(t, lock)
		})
	}
}

// TestBuildSeedScript_LockFileReplacedWhileWaiting: a build whose lock file
// was removed (and made again by another build) between its open and its
// flock holds a lock on a file no other build opens: it stops as busy
// before any qm command. The stub flock replaces the lock file first.
func TestBuildSeedScript_LockFileReplacedWhileWaiting(t *testing.T) {
	for _, shell := range seedScriptShells(t) {
		t.Run(shell, func(t *testing.T) {
			e := newSeedScriptEnv(t, true)
			realFlock, err := os.Readlink(filepath.Join(e.bin, "flock"))
			require.NoError(t, err)
			require.NoError(t, os.Remove(filepath.Join(e.bin, "flock")))
			lock := filepath.Join(e.stage, seedLockName(4242))
			stub := "#!/bin/sh\nrm -f -- '" + lock + "'; : > '" + lock + "'; exec '" + realFlock + "' \"$@\"\n"
			require.NoError(t, os.WriteFile(filepath.Join(e.bin, "flock"), []byte(stub), 0o755))

			archive, err := seedArchive(seedContent{userData: []byte(seedEmptyUserData), metaData: []byte("instance-id: x\n")})
			require.NoError(t, err)
			_, stderr, err := e.run(t, shell, archive)
			require.Error(t, err)
			assert.Equal(t, seedExitImportBusy, scriptExitCode(err), stderr)
			assert.Contains(t, stderr, "the seed build lock was removed meanwhile")
			_, statErr := os.Stat(e.log)
			assert.True(t, os.IsNotExist(statErr), "no qm command ran")
		})
	}
}
