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
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

// TestBoundGuestInfo_CapsGuestControlledData pins the limits applied to
// guest-agent answers: the agent is controlled by whoever runs the guest, so a
// flood of interfaces, filesystems, addresses, or very long names must not
// translate into unbounded ProviderRaw/status keys.
func TestBoundGuestInfo_CapsGuestControlledData(t *testing.T) {
	info := &GuestAgentInfo{}
	for i := 0; i < maxGuestInterfaces+10; i++ {
		iface := GuestNetworkInterface{Name: fmt.Sprintf("eth%d-%s", i, strings.Repeat("x", 200))}
		for j := 0; j < maxGuestIPsPerInterface+5; j++ {
			iface.IPAddresses = append(iface.IPAddresses, fmt.Sprintf("10.0.%d.%d", i, j))
		}
		info.NetworkInterfaces = append(info.NetworkInterfaces, iface)
	}
	for i := 0; i < maxGuestFilesystems+10; i++ {
		info.Filesystems = append(info.Filesystems, GuestFilesystem{Mountpoint: "/mnt/" + strings.Repeat("m", 200)})
	}

	boundGuestInfo(info)

	assert.Len(t, info.NetworkInterfaces, maxGuestInterfaces)
	assert.Len(t, info.Filesystems, maxGuestFilesystems)
	for _, iface := range info.NetworkInterfaces {
		assert.LessOrEqual(t, len(iface.Name), maxGuestNameLen)
		assert.Len(t, iface.IPAddresses, maxGuestIPsPerInterface)
	}
	for _, fs := range info.Filesystems {
		assert.LessOrEqual(t, len(fs.Mountpoint), maxGuestNameLen)
	}
	// Order is preserved: the first entries are the ones kept.
	assert.True(t, strings.HasPrefix(info.NetworkInterfaces[0].Name, "eth0-"))
}

// TestBoundGuestInfo_LeavesSmallAnswersUntouched proves the caps are a no-op
// for a normal guest.
func TestBoundGuestInfo_LeavesSmallAnswersUntouched(t *testing.T) {
	info := &GuestAgentInfo{
		NetworkInterfaces: []GuestNetworkInterface{{Name: "eth0", IPAddresses: []string{"10.0.0.5", "fe80::1"}}},
		Filesystems:       []GuestFilesystem{{Mountpoint: "/"}, {Mountpoint: "/boot"}},
	}
	boundGuestInfo(info)
	assert.Equal(t, "eth0", info.NetworkInterfaces[0].Name)
	assert.Equal(t, []string{"10.0.0.5", "fe80::1"}, info.NetworkInterfaces[0].IPAddresses)
	assert.Equal(t, []string{"/", "/boot"}, []string{info.Filesystems[0].Mountpoint, info.Filesystems[1].Mountpoint})
}

// TestTruncateGuestName_KeepsValidUTF8 proves truncation never splits a
// multi-byte rune.
func TestTruncateGuestName_KeepsValidUTF8(t *testing.T) {
	s := strings.Repeat("é", maxGuestNameLen) // 2 bytes per rune
	got := truncateGuestName(s)
	assert.LessOrEqual(t, len(got), maxGuestNameLen)
	assert.True(t, utf8.ValidString(got))
	assert.Equal(t, "short", truncateGuestName("short"))
}
