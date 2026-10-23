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

package pvefake

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateNetValue: the fake reads a netN the way PVE does, so a provider
// that writes a key PVE rejects (the old "mac=") fails against it too.
func TestValidateNetValue(t *testing.T) {
	valid := []string{
		"virtio",
		"virtio,bridge=vmbr0",
		"virtio=BC:24:11:00:00:01,bridge=vmbr0,tag=30,firewall=1",
		"e1000=bc:24:11:00:00:01,bridge=vmbr1",
		"model=virtio,macaddr=BC:24:11:00:00:01,bridge=vmbr0,rate=10,mtu=1500",
		"virtio,bridge=vmbr0,macaddr=BC:24:11:00:00:01,link_down=1",
	}
	for _, v := range valid {
		assert.NoError(t, validateNetValue(v), v)
	}

	invalid := map[string]string{
		"mac= is not a key":      "virtio,bridge=vmbr0,mac=BC:24:11:00:00:01",
		"no model":               "bridge=vmbr0",
		"unknown model":          "virtio-9000,bridge=vmbr0",
		"two models":             "virtio,e1000,bridge=vmbr0",
		"bad MAC":                "virtio=BC:24:11,bridge=vmbr0",
		"bad macaddr":            "virtio,macaddr=nope",
		"tag too high":           "virtio,bridge=vmbr0,tag=4095",
		"tag zero":               "virtio,bridge=vmbr0,tag=0",
		"tag not a number":       "virtio,bridge=vmbr0,tag=x",
		"empty bridge":           "virtio,bridge=",
		"option without a value": "virtio,bridge",
		"unknown option":         "virtio,bridge=vmbr0,evil=1",
	}
	for name, v := range invalid {
		assert.Error(t, validateNetValue(v), name)
	}
}

// TestValidateIPConfig: only ip, gw, ip6 and gw6; a nameserver inside an
// ipconfigN is rejected, as PVE does.
func TestValidateIPConfig(t *testing.T) {
	for _, v := range []string{"ip=dhcp", "ip=10.0.0.5/24", "ip=10.0.0.5/24,gw=10.0.0.1", "ip6=fd00::5/64,gw6=fd00::1", "ip6=auto", "ip=manual"} {
		assert.NoError(t, validateIPConfig(v), v)
	}
	for _, v := range []string{"ip=10.0.0.5", "ip=10.0.0.5/24,nameserver=10.0.0.2", "ip=10.0.0.5/24,gw=fd00::1", "ip6=10.0.0.5/24", "gw=nope", "dns=10.0.0.2"} {
		assert.Error(t, validateIPConfig(v), v)
	}
}

// TestNetworkConfigFromForm: the network keys of a request are validated and
// kept; others are ignored.
func TestNetworkConfigFromForm(t *testing.T) {
	got, err := networkConfigFromForm(url.Values{
		"net0":       {"virtio,bridge=vmbr1"},
		"ipconfig0":  {"ip=10.0.0.5/24"},
		"nameserver": {"10.0.0.2 10.0.0.3"},
		"cores":      {"2"},
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"net0": "virtio,bridge=vmbr1", "ipconfig0": "ip=10.0.0.5/24", "nameserver": "10.0.0.2 10.0.0.3"}, got)

	for name, form := range map[string]url.Values{
		"bad net":        {"net0": {"virtio,mac=BC:24:11:00:00:01"}},
		"NIC index":      {"net32": {"virtio"}},
		"bad ipconfig":   {"ipconfig1": {"ip=dhcp,nameserver=10.0.0.2"}},
		"bad nameserver": {"nameserver": {"not-an-ip"}},
	} {
		_, err := networkConfigFromForm(form)
		assert.Error(t, err, name)
	}
}

// TestCloneNetworkMACs: a clone gets a new MAC on every NIC and keeps the
// other options; CloneMAC names it.
func TestCloneNetworkMACs(t *testing.T) {
	config := map[string]string{
		"net0":  "virtio=AA:BB:CC:00:00:01,bridge=vmbr3,tag=30",
		"net1":  "model=e1000,macaddr=AA:BB:CC:00:00:02,bridge=vmbr4",
		"net2":  "rtl8139,bridge=vmbr5",
		"scsi0": "tank:vm-1-disk-0",
	}
	cloneNetworkMACs(config, 1234)

	assert.Equal(t, "virtio="+CloneMAC(1234, 0)+",bridge=vmbr3,tag=30", config["net0"])
	assert.Equal(t, "model=e1000,macaddr="+CloneMAC(1234, 1)+",bridge=vmbr4", config["net1"])
	assert.Equal(t, "rtl8139="+CloneMAC(1234, 2)+",bridge=vmbr5", config["net2"], "a NIC with no MAC gets one: PVE gives every NIC a MAC")
	assert.Equal(t, "tank:vm-1-disk-0", config["scsi0"])
	assert.NotEqual(t, CloneMAC(1234, 0), CloneMAC(1234, 1))
	assert.NotEqual(t, CloneMAC(1234, 0), CloneMAC(1235, 0))
	assert.NoError(t, validateNetValue(config["net0"]))
}
