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
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// These tests pin R36 / ADR-0014 Slice 0a: the manager sends a VM's networks
// as json.Marshal([]contracts.NetworkAttachment) (the struct has no JSON tags,
// so the keys are "Bridge", "VLAN", "Model", "StaticIP", ...), and the
// provider used to read lower-case keys that never matched, so every Proxmox
// VM got virtio on vmbr0 and no static IP. They marshal the real contracts
// struct, never hand-written JSON. Validated against pvefake only, not a real
// PVE.

const (
	// templateNet0 and templateNet1 are the NICs of the test template: a
	// non-default bridge, a VLAN tag, a firewall flag and a MAC.
	templateNet0 = "virtio=AA:BB:CC:00:00:01,bridge=vmbr3,tag=30,firewall=1"
	templateNet1 = "e1000=AA:BB:CC:00:00:02,bridge=vmbr4"
	// templateMAC0 is the MAC of the template's net0.
	templateMAC0 = "AA:BB:CC:00:00:01"
)

// networksJSON marshals attachments as the manager does.
func networksJSON(t *testing.T, attachments ...contracts.NetworkAttachment) string {
	t.Helper()
	data, err := json.Marshal(attachments)
	require.NoError(t, err)
	return string(data)
}

// netTestProvider returns a provider on a fake PVE whose template (9000) has
// two NICs.
func netTestProvider(t *testing.T) (*Provider, *pvefake.Server) {
	t.Helper()
	p, srv := newStampTestProvider(t)
	tmpl, ok := srv.GetVM(seededTemplateID)
	require.True(t, ok)
	tmpl.Config = map[string]string{
		"scsi0": "tank-ssd:vm-9000-disk-0,size=32G",
		"net0":  templateNet0,
		"net1":  templateNet1,
	}
	srv.PutVM(tmpl)
	return p, srv
}

// createNet creates a VM named name with the given image, networks and user
// data, and returns the fake's record of it.
func createNet(t *testing.T, p *Provider, srv *pvefake.Server, imageJSON, networks string, userData []byte) pvefake.VM {
	t.Helper()
	ctx := context.Background()
	resp, err := p.Create(ctx, &providerv1.CreateRequest{
		Name:         "net-vm",
		ClassJson:    `{"CPU": 2, "MemoryMiB": 2048}`,
		ImageJson:    imageJSON,
		NetworksJson: networks,
		UserData:     userData,
	})
	require.NoError(t, err)
	if resp.Task != nil {
		require.NoError(t, waitForTask(ctx, p, resp.Task.Id))
	}
	vmid, err := strconv.Atoi(resp.Id)
	require.NoError(t, err)
	vm, ok := srv.GetVM(vmid)
	require.True(t, ok)
	return vm
}

// TestCreate_NewVM_AppliesTheAttachments: the bridge, model, VLAN, static IP
// (with its prefix), gateway and DNS of each attachment reach the VM; an
// attachment that names only a bridge gets virtio and DHCP.
func TestCreate_NewVM_AppliesTheAttachments(t *testing.T) {
	p, srv := netTestProvider(t)
	vm := createNet(t, p, srv, "", networksJSON(t,
		contracts.NetworkAttachment{
			Name: "lan", Bridge: "vmbr1", VLAN: 20, Model: "e1000",
			StaticIP: "10.1.2.3", Prefix: 24, Gateway: "10.1.2.1", DNS: "10.1.2.53, 10.1.2.54",
		},
		contracts.NetworkAttachment{Name: "dmz", Bridge: "vmbr2"},
		contracts.NetworkAttachment{Name: "mgmt", Bridge: "vmbr7", Model: "VIRTIO", VLAN: 4094,
			StaticIP: "10.9.0.4", Prefix: 16, DNS: "10.1.2.53"},
	), nil)

	assert.Equal(t, "e1000,bridge=vmbr1,tag=20", vm.Config["net0"])
	assert.Equal(t, "virtio,bridge=vmbr2", vm.Config["net1"])
	assert.Equal(t, "virtio,bridge=vmbr7,tag=4094", vm.Config["net2"])
	assert.Equal(t, "ip=10.1.2.3/24,gw=10.1.2.1", vm.Config["ipconfig0"])
	assert.Equal(t, "ip=dhcp", vm.Config["ipconfig1"])
	assert.Equal(t, "ip=10.9.0.4/16", vm.Config["ipconfig2"])
	assert.Equal(t, "10.1.2.53 10.1.2.54", vm.Config["nameserver"], "DNS is VM-wide, each server once")
}

// TestCreate_NewVM_NamesAreNotBridges: the attachment's Name is a label. The
// old name-to-bridge heuristic (dmz to vmbr1, mgmt to vmbr2) is gone.
func TestCreate_NewVM_NamesAreNotBridges(t *testing.T) {
	p, srv := netTestProvider(t)
	vm := createNet(t, p, srv, "", networksJSON(t,
		contracts.NetworkAttachment{Name: "dmz"},
		contracts.NetworkAttachment{Name: "mgmt"},
		contracts.NetworkAttachment{Name: "vmbr5"},
	), nil)

	assert.Equal(t, "virtio,bridge=vmbr0", vm.Config["net0"])
	assert.Equal(t, "virtio,bridge=vmbr0", vm.Config["net1"])
	assert.Equal(t, "virtio,bridge=vmbr0", vm.Config["net2"])
}

// TestCreate_NewVM_DefaultNIC: a request with no networks still gets one
// virtio NIC on vmbr0 with DHCP; "null" is what the manager sends for none.
func TestCreate_NewVM_DefaultNIC(t *testing.T) {
	for name, networks := range map[string]string{"empty": "", "null": "null", "empty list": "[]"} {
		t.Run(name, func(t *testing.T) {
			p, srv := netTestProvider(t)
			vm := createNet(t, p, srv, "", networks, nil)
			assert.Equal(t, "virtio,bridge=vmbr0", vm.Config["net0"])
			assert.Equal(t, "ip=dhcp", vm.Config["ipconfig0"])
			assert.NotContains(t, vm.Config, "nameserver")
		})
	}
}

// TestCreate_NewVM_AppliesAForwardedMAC: a MAC an attachment carries (the
// manager does not forward one yet, Slice 0b) is written as macaddr= for a new
// VM, a key PVE has, which the fake accepts and the old "mac=" it would not.
func TestCreate_NewVM_AppliesAForwardedMAC(t *testing.T) {
	p, srv := netTestProvider(t)
	vm := createNet(t, p, srv, "", networksJSON(t,
		contracts.NetworkAttachment{Bridge: "vmbr1", MacAddress: "02:00:00:aa:bb:cc"},
	), nil)
	assert.Equal(t, "virtio,bridge=vmbr1,macaddr=02:00:00:AA:BB:CC", vm.Config["net0"])
}

// TestCreate_Clone_KeepsTheTemplateNICsWhenTheSpecNamesNone: the clone of a
// template with user data and SSH keys used to get every netN overwritten with
// virtio on vmbr0. A spec that names no NIC (only a static IP) leaves the
// template's bridge, VLAN and options, and the MAC PVE gave the clone.
func TestCreate_Clone_KeepsTheTemplateNICsWhenTheSpecNamesNone(t *testing.T) {
	userData := []byte("#cloud-config\nssh_authorized_keys:\n  - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample key@example\n")
	for name, networks := range map[string]string{
		"no networks": "",
		"static IP only": networksJSON(t,
			contracts.NetworkAttachment{Name: "lan", StaticIP: "10.1.2.3", Prefix: 24, Gateway: "10.1.2.1", DNS: "10.1.2.53"}),
	} {
		t.Run(name, func(t *testing.T) {
			p, srv := netTestProvider(t)
			vm := createNet(t, p, srv, `{"TemplateName": "9000"}`, networks, userData)

			clone0 := pvefake.CloneMAC(vm.VMID, 0)
			assert.Equal(t, "virtio="+clone0+",bridge=vmbr3,tag=30,firewall=1", vm.Config["net0"],
				"the template's bridge, VLAN and options, the clone's own MAC")
			assert.NotEqual(t, templateMAC0, clone0)
			assert.Equal(t, "e1000="+pvefake.CloneMAC(vm.VMID, 1)+",bridge=vmbr4", vm.Config["net1"])
			assert.Contains(t, vm.Config, "ide2", "the cloud-init drive is still attached")

			if networks == "" {
				assert.Equal(t, "ip=dhcp", vm.Config["ipconfig0"])
				assert.NotContains(t, vm.Config, "nameserver")
			} else {
				assert.Equal(t, "ip=10.1.2.3/24,gw=10.1.2.1", vm.Config["ipconfig0"])
				assert.Equal(t, "10.1.2.53", vm.Config["nameserver"])
			}
		})
	}
}

// TestCreate_Clone_AppliesTheNamedNICKeepingItsMAC: a spec that names a NIC
// gets its bridge, VLAN and model, on the clone's own MAC (not drawn again),
// whether or not the clone has cloud-init data. The NIC it does not name is
// untouched.
func TestCreate_Clone_AppliesTheNamedNICKeepingItsMAC(t *testing.T) {
	for name, userData := range map[string][]byte{
		"with cloud-init": []byte("#cloud-config\nhostname: net-vm\n"),
		"plain clone":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			p, srv := netTestProvider(t)
			vm := createNet(t, p, srv, `{"TemplateName": "9000"}`, networksJSON(t,
				contracts.NetworkAttachment{Name: "lan", Bridge: "vmbr9", VLAN: 77, Model: "e1000"},
			), userData)

			clone0 := pvefake.CloneMAC(vm.VMID, 0)
			assert.Equal(t, "e1000="+clone0+",bridge=vmbr9,tag=77,firewall=1", vm.Config["net0"],
				"model, bridge and VLAN from the spec; MAC and firewall as the clone had them")
			assert.Equal(t, "e1000="+pvefake.CloneMAC(vm.VMID, 1)+",bridge=vmbr4", vm.Config["net1"],
				"the NIC the spec does not name is as cloned")
		})
	}
}

// TestCreate_Clone_UnreadableConfigFailsClosed: when the clone's config cannot
// be read, a NIC the spec names is not merged blind (that would lose the
// clone's MAC and options): the call fails and writes no network key.
func TestCreate_Clone_UnreadableConfigFailsClosed(t *testing.T) {
	p, srv := netTestProvider(t)
	vm := createNet(t, p, srv, `{"TemplateName": "9000"}`, "", nil)
	before := len(srv.MutatingRequests())

	srv.InjectFault(pvefake.Fault{Method: http.MethodGet, PathContains: "/qemu/" + strconv.Itoa(vm.VMID) + "/config",
		Status: http.StatusInternalServerError, Body: "boom"})
	networks, err := parseNetworksForTest(t, contracts.NetworkAttachment{Bridge: "vmbr9"})
	require.NoError(t, err)
	err = p.configureClonedVM(context.Background(), &providerv1.CreateRequest{}, &pveapi.VMConfig{VMID: vm.VMID, Networks: networks}, "pve")
	require.Error(t, err)
	assert.Equal(t, before, len(srv.MutatingRequests()), "nothing was changed")
}

// parseNetworksForTest parses attachments for a template clone.
func parseNetworksForTest(t *testing.T, attachments ...contracts.NetworkAttachment) ([]pveapi.NetworkConfig, error) {
	t.Helper()
	nets, _, err := parseNetworks(networksJSON(t, attachments...), true)
	return nets, err
}

// TestCreate_Clone_BridgeWithoutVLANDropsTheTemplateTag: a spec that names a
// bridge names the NIC's whole L2 placement, so the template's tag does not
// carry over to it.
func TestCreate_Clone_BridgeWithoutVLANDropsTheTemplateTag(t *testing.T) {
	p, srv := netTestProvider(t)
	vm := createNet(t, p, srv, `{"TemplateName": "9000"}`, networksJSON(t,
		contracts.NetworkAttachment{Bridge: "vmbr9"},
	), nil)
	assert.Equal(t, "virtio="+pvefake.CloneMAC(vm.VMID, 0)+",bridge=vmbr9,firewall=1", vm.Config["net0"])
}

// TestCreate_Clone_PlainCloneWithNoNetworksChangesNothing: a clone with no
// cloud-init data and no networks is not reconfigured at all, and keeps the
// NICs PVE cloned.
func TestCreate_Clone_PlainCloneWithNoNetworksChangesNothing(t *testing.T) {
	p, srv := netTestProvider(t)
	vm := createNet(t, p, srv, `{"TemplateName": "9000"}`, "", nil)

	assert.Equal(t, "virtio="+pvefake.CloneMAC(vm.VMID, 0)+",bridge=vmbr3,tag=30,firewall=1", vm.Config["net0"])
	assert.NotContains(t, vm.Config, "ipconfig0")
	assert.NotContains(t, vm.Config, "nameserver")
}

// TestCreate_Clone_NICBeyondTheTemplateIsAdded: a spec that names more NICs
// than the template has adds them (virtio on the named bridge).
func TestCreate_Clone_NICBeyondTheTemplateIsAdded(t *testing.T) {
	p, srv := netTestProvider(t)
	vm := createNet(t, p, srv, `{"TemplateName": "9000"}`, networksJSON(t,
		contracts.NetworkAttachment{},
		contracts.NetworkAttachment{},
		contracts.NetworkAttachment{Bridge: "vmbr6", VLAN: 5},
	), nil)

	assert.Equal(t, "virtio,bridge=vmbr6,tag=5", vm.Config["net2"])
	assert.Contains(t, vm.Config["net0"], "bridge=vmbr3", "an empty attachment leaves the template's NIC")
}

// TestCreate_NewVM_DNSOnADHCPEntryIsApplied: PVE's nameserver is VM-wide, so a
// DHCP entry's DNS servers are applied too; its prefix and gateway are not.
func TestCreate_NewVM_DNSOnADHCPEntryIsApplied(t *testing.T) {
	p, srv := netTestProvider(t)
	vm := createNet(t, p, srv, "", networksJSON(t,
		contracts.NetworkAttachment{Bridge: "vmbr1", DNS: "10.1.2.53", Prefix: 24, Gateway: "10.1.2.1"},
	), nil)
	assert.Equal(t, "ip=dhcp", vm.Config["ipconfig0"])
	assert.Equal(t, "10.1.2.53", vm.Config["nameserver"])
}

// TestCreate_Clone_NICsAreMatchedByIndex: the spec's entries are matched to the
// template's NICs by index. With net0 and net2 in the template and two
// entries, entry 1 adds net1, and net2 is kept: the clone has three NICs.
func TestCreate_Clone_NICsAreMatchedByIndex(t *testing.T) {
	p, srv := netTestProvider(t)
	tmpl, ok := srv.GetVM(seededTemplateID)
	require.True(t, ok)
	tmpl.Config = map[string]string{
		"scsi0": "tank-ssd:vm-9000-disk-0,size=32G",
		"net0":  templateNet0,
		"net2":  "virtio=AA:BB:CC:00:00:03,bridge=vmbr5",
	}
	srv.PutVM(tmpl)

	vm := createNet(t, p, srv, `{"TemplateName": "9000"}`, networksJSON(t,
		contracts.NetworkAttachment{Bridge: "vmbr9"},
		contracts.NetworkAttachment{Bridge: "vmbr6", VLAN: 7},
	), nil)

	assert.Equal(t, "virtio="+pvefake.CloneMAC(vm.VMID, 0)+",bridge=vmbr9,firewall=1", vm.Config["net0"])
	assert.Equal(t, "virtio,bridge=vmbr6,tag=7", vm.Config["net1"], "a new NIC: the template has no net1")
	assert.Equal(t, "virtio="+pvefake.CloneMAC(vm.VMID, 2)+",bridge=vmbr5", vm.Config["net2"], "a template NIC beyond the spec is kept")
}

// TestCreate_RefusesAnInvalidAttachment: every invalid attachment is refused
// as InvalidSpec before a VM is made, and the message does not echo the
// tenant's value.
func TestCreate_RefusesAnInvalidAttachment(t *testing.T) {
	const secret = "sentinel-value"
	tooMany := make([]contracts.NetworkAttachment, pveMaxNICs+1)
	tests := []struct {
		name     string
		networks string
	}{
		{"bridge with a comma", networksJSON(t, contracts.NetworkAttachment{Bridge: "vmbr1,firewall=0"})},
		{"bridge too long", networksJSON(t, contracts.NetworkAttachment{Bridge: "vmbr0123456789012"})},
		{"bridge with a space", networksJSON(t, contracts.NetworkAttachment{Bridge: "vmbr 1"})},
		{"unknown model", networksJSON(t, contracts.NetworkAttachment{Model: secret})},
		{"model with a comma", networksJSON(t, contracts.NetworkAttachment{Model: "virtio,bridge=vmbr9"})},
		{"VLAN too high", networksJSON(t, contracts.NetworkAttachment{VLAN: 4095})},
		{"negative VLAN", networksJSON(t, contracts.NetworkAttachment{VLAN: -3})},
		{"short MAC", networksJSON(t, contracts.NetworkAttachment{MacAddress: "02:00:00:aa:bb"})},
		{"multicast MAC", networksJSON(t, contracts.NetworkAttachment{MacAddress: "01:00:5e:00:00:01"})},
		{"zero MAC", networksJSON(t, contracts.NetworkAttachment{MacAddress: "00:00:00:00:00:00"})},
		{"MAC with a comma", networksJSON(t, contracts.NetworkAttachment{MacAddress: "02:00:00:aa:bb:cc,bridge=vmbr9"})},
		{"invalid static IP", networksJSON(t, contracts.NetworkAttachment{StaticIP: secret, Prefix: 24})},
		{"static IP with a comma", networksJSON(t, contracts.NetworkAttachment{StaticIP: "10.0.0.5,gw=10.0.0.1", Prefix: 24})},
		{"static IP 0.0.0.0", networksJSON(t, contracts.NetworkAttachment{StaticIP: "0.0.0.0", Prefix: 24})},
		{"static IP multicast", networksJSON(t, contracts.NetworkAttachment{StaticIP: "224.0.0.9", Prefix: 24})},
		{"static IP without a prefix", networksJSON(t, contracts.NetworkAttachment{StaticIP: "10.0.0.5"})},
		{"prefix too long for IPv4", networksJSON(t, contracts.NetworkAttachment{StaticIP: "10.0.0.5", Prefix: 33})},
		{"negative prefix", networksJSON(t, contracts.NetworkAttachment{StaticIP: "10.0.0.5", Prefix: -1})},
		{"CIDR contradicting the prefix", networksJSON(t, contracts.NetworkAttachment{StaticIP: "10.0.0.5/24", Prefix: 16})},
		{"invalid gateway", networksJSON(t, contracts.NetworkAttachment{StaticIP: "10.0.0.5", Prefix: 24, Gateway: secret})},
		{"IPv6 gateway for an IPv4 address", networksJSON(t, contracts.NetworkAttachment{StaticIP: "10.0.0.5", Prefix: 24, Gateway: "fd00::1"})},
		{"invalid DNS", networksJSON(t, contracts.NetworkAttachment{StaticIP: "10.0.0.5", Prefix: 24, DNS: "10.0.0.2," + secret})},
		{"invalid DNS on a DHCP entry", networksJSON(t, contracts.NetworkAttachment{DNS: secret + ".example.com"})},
		{"DNS as a space-separated list", networksJSON(t, contracts.NetworkAttachment{DNS: "10.0.0.2 10.0.0.3"})},
		{"too many attachments", networksJSON(t, tooMany...)},
		{"not JSON", secret},
		{"wrong JSON shape", `{"Bridge": "vmbr1"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, srv := netTestProvider(t)
			for _, image := range []string{"", `{"TemplateName": "9000"}`} {
				_, err := p.Create(context.Background(), &providerv1.CreateRequest{
					Name:         "net-vm",
					ClassJson:    `{"CPU": 2, "MemoryMiB": 2048}`,
					ImageJson:    image,
					NetworksJson: tc.networks,
				})
				require.Error(t, err)
				var perr *errors.ProviderError
				require.ErrorAs(t, err, &perr)
				assert.Equal(t, codes.InvalidArgument, perr.Code, "an invalid attachment is the spec's fault")
				assert.NotContains(t, perr.Message, secret)
				assert.Empty(t, srv.VMsNamed("net-vm"), "nothing is created")
			}
		})
	}
}

// TestParseNetworks_AcceptsIPv6AndCIDR: an IPv6 static IP becomes ip6=/gw6=,
// and an address written in CIDR form carries its own prefix.
func TestParseNetworks_AcceptsIPv6AndCIDR(t *testing.T) {
	nets, ips, err := parseNetworks(networksJSON(t,
		contracts.NetworkAttachment{StaticIP: "fd00:1::10", Prefix: 64, Gateway: "fd00:1::1", DNS: "fd00:1::53"},
		contracts.NetworkAttachment{StaticIP: "192.168.7.9/24", Gateway: "192.168.7.1"},
		contracts.NetworkAttachment{StaticIP: "192.168.8.9/24", Prefix: 24},
	), false)
	require.NoError(t, err)
	require.Len(t, nets, 3)
	require.Len(t, ips, 3)

	assert.Equal(t, "ip6=fd00:1::10/64,gw6=fd00:1::1", pveapi.IPConfigString(ips[0]))
	assert.Equal(t, "ip=192.168.7.9/24,gw=192.168.7.1", pveapi.IPConfigString(ips[1]))
	assert.Equal(t, "ip=192.168.8.9/24", pveapi.IPConfigString(ips[2]))
	assert.Equal(t, "fd00:1::53", pveapi.NameserverValue(ips))
}

// TestParseNetworks_TemplateKeepsTheEmptyFields: for a clone, fields an
// attachment leaves empty stay empty (the clone's own value stays); for a new
// VM they default.
func TestParseNetworks_TemplateKeepsTheEmptyFields(t *testing.T) {
	wire := networksJSON(t, contracts.NetworkAttachment{Name: "lan"})

	clone, _, err := parseNetworks(wire, true)
	require.NoError(t, err)
	require.Len(t, clone, 1)
	assert.False(t, netSpecified(&clone[0]))

	fresh, _, err := parseNetworks(wire, false)
	require.NoError(t, err)
	require.Len(t, fresh, 1)
	assert.Equal(t, pveapi.NetworkConfig{Index: 0, Model: "virtio", Bridge: "vmbr0"}, fresh[0])
}

// TestMergeNetValue: how a request's NIC combines with the NIC PVE cloned.
func TestMergeNetValue(t *testing.T) {
	tests := []struct {
		name    string
		current string
		nic     pveapi.NetworkConfig
		want    string
	}{
		{"nothing there", "", pveapi.NetworkConfig{Bridge: "vmbr1", VLAN: 10}, "virtio,bridge=vmbr1,tag=10"},
		{"nothing there, model only", "", pveapi.NetworkConfig{Model: "e1000"}, "e1000,bridge=vmbr0"},
		{"model only keeps bridge, tag and MAC",
			"virtio=AA:BB:CC:00:00:01,bridge=vmbr3,tag=30", pveapi.NetworkConfig{Model: "vmxnet3"},
			"vmxnet3=AA:BB:CC:00:00:01,bridge=vmbr3,tag=30"},
		{"VLAN only keeps the bridge",
			"virtio=AA:BB:CC:00:00:01,bridge=vmbr3,tag=30", pveapi.NetworkConfig{VLAN: 40},
			"virtio=AA:BB:CC:00:00:01,bridge=vmbr3,tag=40"},
		{"bridge replaces the placement",
			"virtio=AA:BB:CC:00:00:01,bridge=vmbr3,tag=30", pveapi.NetworkConfig{Bridge: "vmbr1"},
			"virtio=AA:BB:CC:00:00:01,bridge=vmbr1"},
		{"a request MAC replaces the clone's",
			"virtio=AA:BB:CC:00:00:01,bridge=vmbr3", pveapi.NetworkConfig{MAC: "02:00:00:AA:BB:CC"},
			"virtio=02:00:00:AA:BB:CC,bridge=vmbr3"},
		{"options are kept in order, bridge and tag first",
			"virtio=AA:BB:CC:00:00:01,firewall=1,bridge=vmbr3,rate=10,tag=30", pveapi.NetworkConfig{Model: "virtio"},
			"virtio=AA:BB:CC:00:00:01,bridge=vmbr3,tag=30,firewall=1,rate=10"},
		{"the macaddr and model= spellings are read",
			"model=e1000,macaddr=AA:BB:CC:00:00:01,bridge=vmbr3", pveapi.NetworkConfig{Bridge: "vmbr3"},
			"e1000=AA:BB:CC:00:00:01,bridge=vmbr3"},
		{"a model change away from virtio drops the virtio-only options",
			"virtio=AA:BB:CC:00:00:01,bridge=vmbr3,mtu=1400,queues=4,host-tunnel=1,firewall=1", pveapi.NetworkConfig{Model: "e1000"},
			"e1000=AA:BB:CC:00:00:01,bridge=vmbr3,firewall=1"},
		{"virtio keeps them",
			"virtio=AA:BB:CC:00:00:01,bridge=vmbr3,mtu=1400,queues=4", pveapi.NetworkConfig{Model: "virtio"},
			"virtio=AA:BB:CC:00:00:01,bridge=vmbr3,mtu=1400,queues=4"},
		{"a bridgeless NIC named only by model gets vmbr0", "virtio=AA:BB:CC:00:00:01", pveapi.NetworkConfig{Model: "virtio"},
			"virtio=AA:BB:CC:00:00:01,bridge=vmbr0"},
		{"a bare model has no MAC", "rtl8139,bridge=vmbr2", pveapi.NetworkConfig{VLAN: 9}, "rtl8139,bridge=vmbr2,tag=9"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mergeNetValue(tc.current, &tc.nic))
			assert.NotContains(t, mergeNetValue(tc.current, &tc.nic), "mac=", "mac= is not a PVE key")
		})
	}
}

// TestNetworkInfosFromConfig: the NICs of a config in index order, with the
// bridge as the name and the MAC from the "<model>=<MAC>" form (and from
// macaddr=).
func TestNetworkInfosFromConfig(t *testing.T) {
	infos := networkInfosFromConfig(map[string]interface{}{
		"net10":      "virtio=BC:24:11:00:00:0A,bridge=vmbr10",
		"net2":       "e1000=BC:24:11:00:00:02,bridge=vmbr2,tag=5",
		"net0":       "model=virtio,macaddr=BC:24:11:00:00:00,bridge=vmbr0",
		"net1":       "virtio,bridge=vmbr1",
		"net3":       "",
		"net":        "virtio,bridge=vmbrX",
		"netx":       "virtio,bridge=vmbrY",
		"net04":      "virtio,bridge=vmbrZ",
		"nameserver": "10.0.0.2",
		"scsi0":      "tank:vm-1-disk-0",
	})
	require.Len(t, infos, 4)
	assert.Equal(t, []string{"vmbr0", "vmbr1", "vmbr2", "vmbr10"},
		[]string{infos[0].Name, infos[1].Name, infos[2].Name, infos[3].Name})
	assert.Equal(t, []string{"BC:24:11:00:00:00", "", "BC:24:11:00:00:02", "BC:24:11:00:00:0A"},
		[]string{infos[0].Mac, infos[1].Mac, infos[2].Mac, infos[3].Mac})
}

// TestListVMs_ReportsNICsAndMACs: ListVMs reports each NIC's bridge and MAC.
func TestListVMs_ReportsNICsAndMACs(t *testing.T) {
	p, srv := newStampTestProvider(t)
	srv.PutVM(pvefake.VM{
		VMID: 610, Name: "nics", Status: "stopped", Node: "pve",
		Config: map[string]string{
			"net0": "virtio=BC:24:11:AA:00:01,bridge=vmbr1,tag=20",
			"net1": "e1000=BC:24:11:AA:00:02,bridge=vmbr2",
		},
	})

	resp, err := p.ListVMs(context.Background(), &providerv1.ListVMsRequest{})
	require.NoError(t, err)
	var found *providerv1.VMInfo
	for _, vm := range resp.Vms {
		if vm.Id == "610" {
			found = vm
		}
	}
	require.NotNil(t, found)
	require.Len(t, found.Networks, 2)
	assert.Equal(t, "vmbr1", found.Networks[0].Name)
	assert.Equal(t, "BC:24:11:AA:00:01", found.Networks[0].Mac)
	assert.Equal(t, "vmbr2", found.Networks[1].Name)
	assert.Equal(t, "BC:24:11:AA:00:02", found.Networks[1].Mac)
}

// TestWriterAndReaderAgree: what mergeNetValue writes, networkInfosFromConfig
// reads back, MAC included.
func TestWriterAndReaderAgree(t *testing.T) {
	value := mergeNetValue("", &pveapi.NetworkConfig{Bridge: "vmbr1", VLAN: 12, MAC: "02:00:00:AA:BB:CC"})
	require.False(t, strings.Contains(value, "mac="))
	infos := networkInfosFromConfig(map[string]interface{}{"net0": value})
	require.Len(t, infos, 1)
	assert.Equal(t, "02:00:00:AA:BB:CC", infos[0].Mac)
	assert.Equal(t, "vmbr1", infos[0].Name)
}
