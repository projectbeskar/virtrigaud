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
	"archive/tar"
	"bytes"
	"context"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
)

// seedNode is a fake PVE node behind the provider's SSH data plane for the
// seed tests: it answers the genisoimage pre-flight and runs the seed script
// against a pvefake.Server (pvefake.AttachSeedVolume), keeping each seed's
// files as they arrived on stdin.
type seedNode struct {
	t   *testing.T
	srv *pvefake.Server

	mu sync.Mutex
	// noGenisoimage makes the pre-flight (and the seed script) answer that
	// genisoimage is missing.
	noGenisoimage bool
	// seedExit, when not 0, is the exit status the seed script fails with
	// (nothing is attached).
	seedExit int
	// commands are the remote commands run, in order.
	commands []string
	// seeds are the seeds attached, in order.
	seeds []seedCapture
}

// seedCapture is one seed the fake node attached.
type seedCapture struct {
	vmid    int
	storage string
	slot    string
	volume  string
	files   map[string][]byte
}

var (
	seedScriptVMID    = regexp.MustCompile(`VMID=(\d+);`)
	seedScriptStorage = regexp.MustCompile(`qm importdisk "\$VMID" "\$W/seed\.iso" '([^']*)' --format raw`)
	seedScriptSlot    = regexp.MustCompile(`qm set "\$VMID" --([a-z]+[0-9]+) "\$V,media=cdrom"`)
)

func newSeedNode(t *testing.T, srv *pvefake.Server) *seedNode {
	t.Helper()
	return &seedNode{t: t, srv: srv}
}

// transport is an SSH data plane with credentials (password) and host-key
// verification off, so checkSeedPrerequisites passes, that runs every remote
// command on n.
func (n *seedNode) transport() *sshTransport {
	return &sshTransport{
		host:             "pve.example.com",
		creds:            sshCredentials{User: "root", Password: "not-a-real-password"},
		hostKey:          proxmoxHostKeyPolicy{insecure: true},
		logger:           slog.Default(),
		runOverride:      n.run,
		runInputOverride: n.runInput,
	}
}

func (n *seedNode) run(_ context.Context, cmd string) (string, string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.commands = append(n.commands, cmd)
	if cmd == seedPreflightCommand && n.noGenisoimage {
		return "", "", exitErr(n.t, seedExitNoGenisoimage)
	}
	return "", "", nil
}

func (n *seedNode) runInput(_ context.Context, stdin []byte, cmd string) (string, string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.commands = append(n.commands, cmd)
	if n.noGenisoimage {
		return "", "", exitErr(n.t, seedExitNoGenisoimage)
	}
	if n.seedExit != 0 {
		return "", "", exitErr(n.t, n.seedExit)
	}
	vm := seedScriptVMID.FindStringSubmatch(cmd)
	st := seedScriptStorage.FindStringSubmatch(cmd)
	sl := seedScriptSlot.FindStringSubmatch(cmd)
	require.NotNil(n.t, vm, "the seed script names the VMID")
	require.NotNil(n.t, st, "the seed script names the storage")
	require.NotNil(n.t, sl, "the seed script names the slot")
	vmid, err := strconv.Atoi(vm[1])
	require.NoError(n.t, err)
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(stdin))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(n.t, err)
		require.Equal(n.t, byte(tar.TypeReg), hdr.Typeflag)
		data, err := io.ReadAll(tr)
		require.NoError(n.t, err)
		files[hdr.Name] = data
	}
	volume, err := n.srv.AttachSeedVolume(vmid, st[1], sl[1])
	if err != nil {
		return "", err.Error(), exitErr(n.t, 2)
	}
	n.seeds = append(n.seeds, seedCapture{vmid: vmid, storage: st[1], slot: sl[1], volume: volume, files: files})
	return volume + "\n", "", nil
}

// seedsOf returns the seeds attached to VM vmid.
func (n *seedNode) seedsOf(vmid int) []seedCapture {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []seedCapture
	for _, s := range n.seeds {
		if s.vmid == vmid {
			out = append(out, s)
		}
	}
	return out
}

// allCommands returns the remote commands run so far.
func (n *seedNode) allCommands() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.commands...)
}
