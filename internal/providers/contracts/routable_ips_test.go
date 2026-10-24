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

package contracts

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRoutableIPs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil input", in: nil, want: nil},
		{name: "empty input", in: []string{}, want: nil},
		{name: "global IPv4 kept", in: []string{"192.0.2.10"}, want: []string{"192.0.2.10"}},
		{name: "private IPv4 kept", in: []string{"10.1.2.3", "172.16.0.9", "192.168.1.5"}, want: []string{"10.1.2.3", "172.16.0.9", "192.168.1.5"}},
		{name: "global IPv6 kept", in: []string{"2001:db8::1"}, want: []string{"2001:db8::1"}},
		{name: "IPv6 ULA kept", in: []string{"fd00::5", "fc00::6"}, want: []string{"fd00::5", "fc00::6"}},
		{name: "IPv4 loopback dropped", in: []string{"127.0.0.1", "127.8.9.10"}, want: nil},
		{name: "IPv6 loopback dropped", in: []string{"::1"}, want: nil},
		{name: "IPv4 unspecified dropped", in: []string{"0.0.0.0"}, want: nil},
		{name: "IPv6 unspecified dropped", in: []string{"::"}, want: nil},
		{name: "IPv4 link-local dropped", in: []string{"169.254.1.1", "169.254.255.254"}, want: nil},
		{name: "IPv6 link-local dropped", in: []string{"fe80::5054:ff:fe88:d0c1", "febf::1"}, want: nil},
		{name: "IPv6 link-local with zone dropped", in: []string{"fe80::1%eth0"}, want: nil},
		{name: "IPv4 multicast dropped", in: []string{"224.0.0.1", "239.255.255.250"}, want: nil},
		{name: "IPv6 multicast dropped", in: []string{"ff02::1", "ff05::2"}, want: nil},
		{name: "just outside link-local kept", in: []string{"169.253.0.1", "fec0::1"}, want: []string{"169.253.0.1", "fec0::1"}},
		{name: "just outside loopback kept", in: []string{"126.255.255.255", "128.0.0.1"}, want: []string{"126.255.255.255", "128.0.0.1"}},
		{
			name: "guest agent answer keeps only the reachable addresses",
			in:   []string{"127.0.0.1", "::1", "192.0.2.31", "fe80::5054:ff:fe88:d0c1"},
			want: []string{"192.0.2.31"},
		},
		{
			name: "order is the provider's order",
			in:   []string{"2001:db8::9", "192.0.2.5", "10.0.0.1", "2001:db8::1"},
			want: []string{"2001:db8::9", "192.0.2.5", "10.0.0.1", "2001:db8::1"},
		},
		{name: "duplicate dropped, first position kept", in: []string{"192.0.2.5", "10.0.0.1", "192.0.2.5"}, want: []string{"192.0.2.5", "10.0.0.1"}},
		{
			name: "IPv6 spelling variants are one address",
			in:   []string{"2001:db8::1", "2001:0DB8:0:0:0:0:0:1", "2001:DB8::1"},
			want: []string{"2001:db8::1"},
		},
		{name: "IPv4-mapped IPv6 reported as IPv4", in: []string{"::ffff:192.0.2.7", "192.0.2.7"}, want: []string{"192.0.2.7"}},
		{name: "IPv4-mapped loopback dropped", in: []string{"::ffff:127.0.0.1"}, want: nil},
		{name: "whitespace trimmed", in: []string{"  192.0.2.1\n", "\t2001:db8::2 "}, want: []string{"192.0.2.1", "2001:db8::2"}},
		{
			name: "garbage dropped",
			in:   []string{"", "   ", "N/A", "-", "host.example", "192.0.2.1/24", "2001:db8::/64", "300.1.1.1", "192.0.2", "1.2.3.4.5", "192.0.2.99"},
			want: []string{"192.0.2.99"},
		},
		{name: "only garbage and unreachable", in: []string{"", "lo", "127.0.0.1", "fe80::1"}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, RoutableIPs(tt.in))
		})
	}
}

func TestRoutableIPsDoesNotModifyInput(t *testing.T) {
	in := []string{"127.0.0.1", "192.0.2.1", "192.0.2.1", "fe80::1"}
	orig := append([]string(nil), in...)
	_ = RoutableIPs(in)
	assert.Equal(t, orig, in)
}
