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

package grpc

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
)

// TestConvertCreateRequest_NetworksReachProxmox pins R36 / ADR-0014 Slice 0a
// end to end on the manager side of the wire: the networks the manager builds
// (contracts.NetworkAttachment), converted by the real convertCreateRequest,
// handed to the real Proxmox provider on a fake PVE, land on the bridge,
// VLAN, model and static IP the attachments name. The contract struct has no
// JSON tags, so its wire keys are its Go field names, and the provider reads
// them back into the same struct. Validated against pvefake only.
func TestConvertCreateRequest_NetworksReachProxmox(t *testing.T) {
	srv, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	srv.SetTaskDelay(0) // a task ends at once
	t.Setenv("PROVIDER_ENDPOINT", endpoint)
	t.Setenv("PROVIDER_TOKEN_ID", "test@pve!token")
	t.Setenv("PROVIDER_TOKEN_SECRET", "secret")
	t.Setenv("TLS_INSECURE_SKIP_VERIFY", "true")
	provider := proxmox.New()

	attachments := []contracts.NetworkAttachment{
		{Name: "lan", Bridge: "vmbr1", VLAN: 20, Model: "e1000",
			StaticIP: "10.1.2.3", Prefix: 24, Gateway: "10.1.2.1", DNS: "10.1.2.53"},
		{Name: "dmz", Bridge: "vmbr2"},
	}
	c := &Client{}
	grpcReq, err := c.convertCreateRequest(contracts.CreateRequest{Name: "net-vm", Networks: attachments})
	require.NoError(t, err)

	// The wire keys are the contract's Go field names. Renaming one (or tagging
	// the struct) changes what every provider, in or out of tree, receives.
	var wire []map[string]any
	require.NoError(t, json.Unmarshal([]byte(grpcReq.NetworksJson), &wire))
	require.Len(t, wire, 2)
	for _, key := range []string{"Name", "Bridge", "VLAN", "Model", "StaticIP", "Prefix", "Gateway", "DNS"} {
		assert.Contains(t, wire[0], key)
	}

	ctx := context.Background()
	resp, err := provider.Create(ctx, grpcReq)
	require.NoError(t, err)
	vmid, err := strconv.Atoi(resp.Id)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		vm, ok := srv.GetVM(vmid)
		return ok && vm.Lock == "" && vm.Name == "net-vm"
	}, 5*time.Second, 10*time.Millisecond)
	vm, _ := srv.GetVM(vmid)
	assert.Equal(t, "e1000,bridge=vmbr1,tag=20", vm.Config["net0"])
	assert.Equal(t, "virtio,bridge=vmbr2", vm.Config["net1"])
	assert.Equal(t, "ip=10.1.2.3/24,gw=10.1.2.1", vm.Config["ipconfig0"])
	assert.Equal(t, "ip=dhcp", vm.Config["ipconfig1"])
	assert.Equal(t, "10.1.2.53", vm.Config["nameserver"])
}
