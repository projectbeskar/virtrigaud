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
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
)

// fakeNode answers the remote commands of the imported-disk path. pgrepExit
// is the exit status of the importdisk check: 0 (a process matches), 1 (none),
// 255 (ssh failed).
type fakeNode struct {
	mu        sync.Mutex
	pgrepExit int
	commands  []string
}

func (n *fakeNode) run(t *testing.T) func(ctx context.Context, cmd string) (string, string, error) {
	return func(_ context.Context, cmd string) (string, string, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.commands = append(n.commands, cmd)
		switch {
		case strings.HasPrefix(cmd, "pgrep "):
			if n.pgrepExit == 0 {
				return "4242\n", "", nil
			}
			return "", "", exitErr(t, n.pgrepExit)
		case strings.Contains(cmd, "qm importdisk "):
			return "Successfully imported disk as 'unused0:local-lvm:vm-1-disk-0'", "", nil
		}
		return "", "", nil
	}
}

// ran reports whether a command containing substr was run.
func (n *fakeNode) ran(substr string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, c := range n.commands {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// TestRebind_ImportedDiskRebuildWaitsForARunningImportdisk (M-C): an
// interrupted imported-disk create is rebuilt only when no `qm importdisk` of
// it still runs on the node; a running one, or a check that cannot run, leaves
// the half-built VM in place (outcome unknown).
func TestRebind_ImportedDiskRebuildWaitsForARunningImportdisk(t *testing.T) {
	const halfBuilt = 4950
	req := createReq(testOwner(), ownStageImageJSON)
	for name, tc := range map[string]struct {
		pgrepExit   int
		wantRebuilt bool
	}{
		"importdisk still running": {0, false},
		"check failed (ssh 255)":   {255, false},
		"no importdisk running":    {1, true},
	} {
		t.Run(name, func(t *testing.T) {
			p, srv := newStampTestProvider(t)
			node := &fakeNode{pgrepExit: tc.pgrepExit}
			p.ssh = &sshTransport{host: "pve", logger: slog.Default(), runOverride: node.run(t)}
			srv.PutVM(pvefake.VM{VMID: halfBuilt, Name: testOwnerName, Node: "pve", Status: "stopped",
				Description: testStamp(testOwnerUID, ownerStampPhaseCreating).line()})

			_, err := p.Create(context.Background(), req)
			_, stillThere := srv.GetVM(halfBuilt)
			if !tc.wantRebuilt {
				assert.Equal(t, classOutcomeUnknown, classOf(t, err))
				assert.True(t, stillThere, "the half-built VM is not removed")
				assert.False(t, node.ran("qm importdisk"))
				return
			}
			require.NoError(t, err)
			assert.False(t, stillThere, "the half-built VM was removed")
			vms := srv.VMsNamed(testOwnerName)
			require.Len(t, vms, 1)
			assert.Equal(t, ownerStampPhaseReady, stampOf(t, vms[0]).phase)
			assert.True(t, node.ran("qm importdisk"))
		})
	}
}

func TestImportdiskRunningPattern(t *testing.T) {
	re := regexp.MustCompile(importdiskRunningPattern(105))
	assert.True(t, re.MatchString("/usr/bin/perl -T /usr/sbin/qm importdisk 105 /var/tmp/x.qcow2 local-lvm --format qcow2"))
	assert.True(t, re.MatchString("qm importdisk 105"))
	assert.False(t, re.MatchString("/usr/bin/perl -T /usr/sbin/qm importdisk 1050 /var/tmp/x.qcow2 local-lvm"))
	assert.False(t, re.MatchString("/usr/sbin/xqm importdisk 105 x"))
	assert.False(t, re.MatchString("bash -c pgrep -f -- '(^|[/ ])[q]m importdisk 105( |$)'"), "never matches the shell running pgrep")
}
