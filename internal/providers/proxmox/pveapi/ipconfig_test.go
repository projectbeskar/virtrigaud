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

package pveapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIPConfigString: an ipconfigN carries the address and gateway only; PVE
// rejects a nameserver there (it is the VM's own key).
func TestIPConfigString(t *testing.T) {
	tests := []struct {
		name string
		in   IPConfig
		want string
	}{
		{"dhcp", IPConfig{DHCP: true}, "ip=dhcp"},
		{"dhcp wins", IPConfig{DHCP: true, IP: "10.0.0.5/24"}, "ip=dhcp"},
		{"static", IPConfig{IP: "10.0.0.5/24", Gateway: "10.0.0.1"}, "ip=10.0.0.5/24,gw=10.0.0.1"},
		{"static without a gateway", IPConfig{IP: "10.0.0.5/24"}, "ip=10.0.0.5/24"},
		{"DNS is not part of it", IPConfig{IP: "10.0.0.5/24", Gateway: "10.0.0.1", DNS: "10.0.0.2"}, "ip=10.0.0.5/24,gw=10.0.0.1"},
		{"IPv6", IPConfig{IP: "fd00::5/64", Gateway: "fd00::1"}, "ip6=fd00::5/64,gw6=fd00::1"},
		{"nothing", IPConfig{}, ""},
		{"only DNS", IPConfig{DNS: "10.0.0.2"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IPConfigString(tc.in))
		})
	}
}

// TestNameserverValue: the VM-wide nameserver is every config's servers in
// order, each once, space-separated.
func TestNameserverValue(t *testing.T) {
	assert.Equal(t, "", NameserverValue(nil))
	assert.Equal(t, "", NameserverValue([]IPConfig{{DHCP: true}, {IP: "10.0.0.5/24"}}))
	assert.Equal(t, "10.0.0.2 10.0.0.3 fd00::53", NameserverValue([]IPConfig{
		{DNS: "10.0.0.2, 10.0.0.3"},
		{DNS: "10.0.0.3,,fd00::53"},
	}))
}

// TestConfigToValues_Networks: a new VM's network keys, as PVE takes them.
func TestConfigToValues_Networks(t *testing.T) {
	c := &Client{}
	values := c.configToValues(&VMConfig{
		Networks: []NetworkConfig{
			{Index: 0, Model: "e1000", Bridge: "vmbr1", VLAN: 20, MAC: "02:00:00:AA:BB:CC"},
			{Index: 1},
		},
		IPConfigs: []IPConfig{
			{Index: 0, IP: "10.0.0.5/24", Gateway: "10.0.0.1", DNS: "10.0.0.2"},
			{Index: 1, DHCP: true},
		},
	})
	assert.Equal(t, "e1000,bridge=vmbr1,tag=20,macaddr=02:00:00:AA:BB:CC", values.Get("net0"))
	assert.Equal(t, "virtio,bridge=vmbr0", values.Get("net1"))
	assert.Equal(t, "ip=10.0.0.5/24,gw=10.0.0.1", values.Get("ipconfig0"))
	assert.Equal(t, "ip=dhcp", values.Get("ipconfig1"))
	assert.Equal(t, "10.0.0.2", values.Get("nameserver"))
}
