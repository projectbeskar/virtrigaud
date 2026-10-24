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
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The outputs below are the shapes of `virsh domifaddr` on libvirt 10.0 for a
// guest on the NAT "default" network (192.168.122.0/24 is libvirt's well-known
// default range), with the lab's addresses replaced.
const (
	domifaddrAgentEmpty = ""

	// The lease and arp sources name the row after the NIC's host-side device
	// (vnetN), and the arp source prints a /0 prefix length.
	domifaddrLeaseNAT = ` Name       MAC address          Protocol     Address
-------------------------------------------------------------------------------
 vnet7      52:54:00:00:00:01    ipv4         192.168.122.50/24
`
	domifaddrARPNAT = ` Name       MAC address          Protocol     Address
-------------------------------------------------------------------------------
 vnet7      52:54:00:00:00:01    ipv4         192.168.122.50/0
`
	// The agent names the rows after the guest's interfaces and reports lo.
	domifaddrAgentLoEth0 = ` Name       MAC address          Protocol     Address
-------------------------------------------------------------------------------
 lo         00:00:00:00:00:00    ipv4         127.0.0.1/8
 -          -                    ipv6         ::1/128
 eth0       52:54:00:00:00:01    ipv4         192.0.2.31/24
 -          -                    ipv6         fe80::5054:ff:fe00:1/64
`
	// Two NICs on the default network, the second with an IPv4 and an IPv6
	// address (the second address of a NIC is a "-" / "-" continuation row).
	domifaddrLeaseTwoNICs = ` Name       MAC address          Protocol     Address
-------------------------------------------------------------------------------
 vnet7      52:54:00:00:00:01    ipv4         192.168.122.50/24
 vnet8      52:54:00:00:00:02    ipv4         192.168.122.51/24
 -          -                    ipv6         2001:db8::51/64
`
)

func TestParseDomifaddr(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{name: "agent source with no rows", out: domifaddrAgentEmpty, want: nil},
		{name: "header only", out: " Name       MAC address          Protocol     Address\n---------------\n", want: nil},
		{name: "lease row on a vnetN device, prefix removed", out: domifaddrLeaseNAT, want: []string{"192.168.122.50"}},
		{name: "arp row on a vnetN device, /0 removed", out: domifaddrARPNAT, want: []string{"192.168.122.50"}},
		{name: "agent output: lo and link-local dropped by address", out: domifaddrAgentLoEth0, want: []string{"192.0.2.31"}},
		{name: "two NICs and a continuation row keep every address in order", out: domifaddrLeaseTwoNICs, want: []string{"192.168.122.50", "192.168.122.51", "2001:db8::51"}},
		{
			name: "docker-style guest interface names are not filtered by name",
			out: ` Name       MAC address          Protocol     Address
-------------------------------------------------------------------------------
 ens3       52:54:00:00:00:01    ipv4         198.51.100.7/24
 br0        52:54:00:00:00:02    ipv4         198.51.100.8/24
`,
			want: []string{"198.51.100.7", "198.51.100.8"},
		},
		{
			name: "N/A and - addresses and a repeated address are dropped",
			out: ` Name       MAC address          Protocol     Address
-------------------------------------------------------------------------------
 vnet7      52:54:00:00:00:01    ipv4         N/A
 vnet7      52:54:00:00:00:01    ipv4         -
 vnet7      52:54:00:00:00:01    ipv4         192.0.2.9/24
 vnet7      52:54:00:00:00:01    ipv4         192.0.2.9/0
`,
			want: []string{"192.0.2.9"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseDomifaddr(tt.out))
		})
	}
}

// fakeVirsh puts a `virsh` shell script on PATH that answers `domifaddr` with
// the given output per source (the default source is "lease"), or with an
// error for a source in failing. It records the sources asked, in order, in the
// returned log file.
func fakeVirsh(t *testing.T, outputs map[string]string, failing map[string]bool) (logFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script stub")
	}
	dir := t.TempDir()
	logFile = filepath.Join(dir, "sources.log")
	script := "#!/bin/sh\nsource=lease\nwhile [ $# -gt 0 ]; do [ \"$1\" = \"--source\" ] && source=$2; shift; done\n" +
		"echo \"$source\" >> " + logFile + "\n"
	for _, source := range []string{"agent", "lease", "arp"} {
		out := filepath.Join(dir, source+".out")
		require.NoError(t, os.WriteFile(out, []byte(outputs[source]), 0o600))
		if failing[source] {
			script += "[ \"$source\" = " + source + " ] && { echo 'error: Guest agent is not responding' >&2; exit 1; }\n"
		} else {
			script += "[ \"$source\" = " + source + " ] && { cat " + out + "; exit 0; }\n"
		}
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "virsh"), []byte(script), 0o700)) //nolint:gosec // test stub must be executable
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logFile
}

func TestGetDomainIPAddresses_Sources(t *testing.T) {
	tests := []struct {
		name        string
		outputs     map[string]string
		failing     map[string]bool
		want        string
		wantSources []string
	}{
		{
			name:        "NAT guest without the agent shows its lease",
			outputs:     map[string]string{"agent": domifaddrAgentEmpty, "lease": domifaddrLeaseNAT, "arp": domifaddrARPNAT},
			want:        "192.168.122.50",
			wantSources: []string{"agent", "lease"},
		},
		{
			name:        "an agent that is not running is skipped",
			outputs:     map[string]string{"lease": domifaddrLeaseNAT},
			failing:     map[string]bool{"agent": true},
			want:        "192.168.122.50",
			wantSources: []string{"agent", "lease"},
		},
		{
			name:        "the agent wins and ends the search",
			outputs:     map[string]string{"agent": domifaddrAgentLoEth0, "lease": domifaddrLeaseNAT},
			want:        "192.0.2.31",
			wantSources: []string{"agent"},
		},
		{
			name:        "an agent that reports only loopback and link-local falls through to the lease",
			outputs:     map[string]string{"agent": " Name MAC address Protocol Address\n---\n lo 00:00:00:00:00:00 ipv4 127.0.0.1/8\n eth0 52:54:00:00:00:01 ipv6 fe80::1/64\n", "lease": domifaddrLeaseNAT},
			want:        "192.168.122.50",
			wantSources: []string{"agent", "lease"},
		},
		{
			name:        "a lease of two NICs returns every address",
			outputs:     map[string]string{"lease": domifaddrLeaseTwoNICs},
			want:        "192.168.122.50,192.168.122.51,2001:db8::51",
			wantSources: []string{"agent", "lease"},
		},
		{
			name:        "a guest on a host bridge has no lease: the arp table is the last resort",
			outputs:     map[string]string{"arp": domifaddrARPNAT},
			want:        "192.168.122.50",
			wantSources: []string{"agent", "lease", "arp"},
		},
		{
			name:        "no source has an address",
			outputs:     map[string]string{},
			want:        "",
			wantSources: []string{"agent", "lease", "arp"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logFile := fakeVirsh(t, tt.outputs, tt.failing)
			v := &VirshProvider{uri: "qemu:///system"}

			got, err := v.getDomainIPAddresses(context.Background(), "dom")
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)

			raw, readErr := os.ReadFile(logFile) //nolint:gosec // test-owned temp file
			require.NoError(t, readErr)
			asked := strings.Fields(string(raw))
			assert.Equal(t, tt.wantSources, asked, "the sources are asked in order, and the first with an address wins")
		})
	}
}
