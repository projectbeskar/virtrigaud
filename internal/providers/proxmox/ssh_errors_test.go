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
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
)

// exitErr returns the *exec.ExitError of a shell that exits with code.
func exitErr(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var ee *exec.ExitError
	require.True(t, errors.As(err, &ee))
	return fmt.Errorf("ssh pve: %w (stderr: )", err)
}

// TestSSHMayHaveRun (L4): an ssh that exited 255 (connection failed or broke),
// was cut off by its context, or was killed may have left the remote command
// running; a remote command's own exit status, or an ssh that never started,
// did not.
func TestSSHMayHaveRun(t *testing.T) {
	ctx := context.Background()
	assert.True(t, sshMayHaveRun(ctx, exitErr(t, 255)))
	assert.False(t, sshMayHaveRun(ctx, exitErr(t, 1)))
	assert.False(t, sshMayHaveRun(ctx, exitErr(t, 2)))
	assert.False(t, sshMayHaveRun(ctx, errors.New("ssh: no credentials")))

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	assert.True(t, sshMayHaveRun(canceled, exitErr(t, 1)))
}

// TestRebind_ImportedDiskRebuildChecksPrerequisites (L4): an interrupted
// imported-disk create is removed for a rebuild only when it can be rebuilt;
// without the SSH data plane it is left in place (outcome unknown).
func TestRebind_ImportedDiskRebuildChecksPrerequisites(t *testing.T) {
	p, srv := newStampTestProvider(t)
	p.ssh = nil
	srv.PutVM(pvefake.VM{VMID: 4800, Name: testOwnerName, Node: "pve", Status: "stopped",
		Description: testStamp(testOwnerUID, ownerStampPhaseCreating).line()})

	req := createReq(testOwner(), `{"Path":"/var/tmp/.virtrigaud-import-web.qcow2","Format":"qcow2"}`)
	_, err := p.Create(context.Background(), req)
	assert.Equal(t, classOutcomeUnknown, classOf(t, err))
	_, exists := srv.GetVM(4800)
	assert.True(t, exists, "the half-built VM is not removed when it cannot be rebuilt")

}
