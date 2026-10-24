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
	"net/netip"
	"strings"
)

// RoutableIPs returns the addresses of ips a user can reach a VM on, in the
// order the provider reported them, for VirtualMachine status.ips.
//
// Each entry is trimmed and parsed with net/netip; an entry that does not parse
// (an empty string, a hostname, an address with a prefix length, garbage) is
// dropped. An IPv4-mapped IPv6 address is reported as the IPv4 address it
// carries, and an IPv6 zone is removed. The following are dropped:
//
//   - loopback (127.0.0.0/8, ::1)
//   - the unspecified address (0.0.0.0, ::)
//   - IPv4 link-local (169.254.0.0/16)
//   - IPv6 link-local (fe80::/10)
//   - multicast (224.0.0.0/4, ff00::/8)
//
// Private (RFC 1918), IPv6 unique-local (fc00::/7) and global addresses are
// kept. Each kept address is returned in its canonical text form, and a
// duplicate (including one that differs only in spelling, such as an expanded
// or upper-case IPv6 address) is dropped, keeping its first position.
//
// The result is nil when nothing is left, so a status field tagged omitempty
// stays absent. The input slice is never modified.
func RoutableIPs(ips []string) []string {
	var out []string
	seen := make(map[netip.Addr]struct{}, len(ips))
	for _, raw := range ips {
		addr, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		addr = addr.Unmap().WithZone("")
		if !isRoutable(addr) {
			continue
		}
		if _, dup := seen[addr]; dup {
			continue
		}
		seen[addr] = struct{}{}
		out = append(out, addr.String())
	}
	return out
}

// isRoutable reports whether addr is an address a user can reach a VM on: not
// loopback, unspecified, link-local, or multicast.
func isRoutable(addr netip.Addr) bool {
	return addr.IsValid() &&
		!addr.IsLoopback() &&
		!addr.IsUnspecified() &&
		!addr.IsLinkLocalUnicast() &&
		!addr.IsMulticast()
}
