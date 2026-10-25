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
