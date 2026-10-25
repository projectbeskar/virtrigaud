/*
Copyright 2025.

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
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// The fake checks the network keys of a VM config the way PVE's schema does
// (pve-qemu-server's PVE::QemuServer::Network, PVE::JSONSchema), so a
// provider that writes a key PVE would reject (net0 "virtio,mac=...",
// "nameserver=" inside an ipconfigN) fails here too. It does not model every
// PVE rule; it models the ones a provider can get wrong.

var (
	netKeyPattern      = regexp.MustCompile(`^net(\d+)$`)
	ipconfigKeyPattern = regexp.MustCompile(`^ipconfig(\d+)$`)
	macPattern         = regexp.MustCompile(`^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$`)

	// nicModels are the models a netN value can start with.
	nicModels = map[string]bool{
		"e1000": true, "e1000-82540em": true, "e1000-82544gc": true, "e1000-82545em": true,
		"e1000e": true, "i82551": true, "i82557b": true, "i82559er": true,
		"ne2k_isa": true, "ne2k_pci": true, "pcnet": true, "rtl8139": true,
		"virtio": true, "vmxnet3": true,
	}
	// netOptions are the other keys of a netN value.
	netOptions = map[string]bool{
		"bridge": true, "firewall": true, "link_down": true, "macaddr": true,
		"model": true, "mtu": true, "queues": true, "rate": true, "tag": true,
		"trunks": true, "host-tunnel": true,
	}
	// bridgeIDPattern is PVE's pve-bridge-id format.
	bridgeIDPattern = regexp.MustCompile(`^[-_.\w]+$`)
)

// validMAC reports whether mac is a unicast MAC address in PVE's format: PVE
// refuses a multicast one (first octet odd).
func validMAC(mac string) bool {
	if !macPattern.MatchString(mac) {
		return false
	}
	first, err := strconv.ParseUint(mac[:2], 16, 8)
	return err == nil && first&1 == 0
}

// maxNICs is the number of NICs of a PVE VM (net0 to net31).
const maxNICs = 32

// validateNetValue checks a netN value: a model (alone, as "<model>=<MAC>",
// or as "model=<model>"), then known options with valid values. A "mac=" key
// is not a PVE key.
func validateNetValue(value string) error {
	hasModel := false
	for _, part := range strings.Split(value, ",") {
		key, val, hasVal := strings.Cut(strings.TrimSpace(part), "=")
		switch {
		case nicModels[key]:
			if hasModel {
				return fmt.Errorf("duplicate model %q", key)
			}
			hasModel = true
			if hasVal && !validMAC(val) {
				return fmt.Errorf("invalid MAC address %q for model %q", val, key)
			}
		case !netOptions[key]:
			return fmt.Errorf("unknown key %q", key)
		case !hasVal:
			return fmt.Errorf("key %q needs a value", key)
		case key == "model":
			if hasModel {
				return fmt.Errorf("duplicate model %q", val)
			}
			if !nicModels[val] {
				return fmt.Errorf("invalid model %q", val)
			}
			hasModel = true
		case key == "macaddr" && !validMAC(val):
			return fmt.Errorf("invalid MAC address %q", val)
		case key == "tag":
			if n, err := strconv.Atoi(val); err != nil || n < 1 || n > 4094 {
				return fmt.Errorf("invalid VLAN tag %q", val)
			}
		case key == "bridge" && !bridgeIDPattern.MatchString(val):
			return fmt.Errorf("invalid bridge ID %q", val)
		}
	}
	if !hasModel {
		return fmt.Errorf("a model is required")
	}
	return nil
}

// validateIPConfig checks an ipconfigN value: only ip, gw, ip6 and gw6, with
// ip and ip6 as an address with its prefix length (or dhcp/manual, auto/dhcp
// for ip6), and a gateway only beside an address of its family (PVE's
// ipconfig format makes gw require ip, and gw6 require ip6).
func validateIPConfig(value string) error {
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		key, val, _ := strings.Cut(strings.TrimSpace(part), "=")
		seen[key] = true
		switch key {
		case "ip":
			if val == "dhcp" || val == "manual" {
				continue
			}
			prefix, err := netip.ParsePrefix(val)
			if err != nil || !prefix.Addr().Is4() {
				return fmt.Errorf("invalid ip %q", val)
			}
		case "ip6":
			if val == "dhcp" || val == "auto" || val == "manual" {
				continue
			}
			prefix, err := netip.ParsePrefix(val)
			if err != nil || !prefix.Addr().Is6() {
				return fmt.Errorf("invalid ip6 %q", val)
			}
		case "gw":
			if addr, err := netip.ParseAddr(val); err != nil || !addr.Is4() {
				return fmt.Errorf("invalid gw %q", val)
			}
		case "gw6":
			if addr, err := netip.ParseAddr(val); err != nil || !addr.Is6() {
				return fmt.Errorf("invalid gw6 %q", val)
			}
		default:
			return fmt.Errorf("unknown key %q", key)
		}
	}
	if seen["gw"] && !seen["ip"] {
		return fmt.Errorf("gw requires ip")
	}
	if seen["gw6"] && !seen["ip6"] {
		return fmt.Errorf("gw6 requires ip6")
	}
	return nil
}

// validateNameserver checks a VM's nameserver value: IP addresses separated
// by spaces (or commas or semicolons).
func validateNameserver(value string) error {
	for _, server := range strings.FieldsFunc(value, func(r rune) bool { return r == ' ' || r == ',' || r == ';' }) {
		if _, err := netip.ParseAddr(server); err != nil {
			return fmt.Errorf("invalid DNS server %q", server)
		}
	}
	return nil
}

// networkConfigFromForm validates the network keys of a create or config
// request (netN, ipconfigN, nameserver) and returns them. The error is what
// PVE answers a bad parameter with.
func networkConfigFromForm(form url.Values) (map[string]string, error) {
	out := map[string]string{}
	for key, values := range form {
		if len(values) == 0 {
			continue
		}
		value := values[0]
		switch {
		case netKeyPattern.MatchString(key):
			if n, _ := strconv.Atoi(netKeyPattern.FindStringSubmatch(key)[1]); n >= maxNICs {
				return nil, fmt.Errorf("parameter verification failed: %s: unknown NIC index", key)
			}
			if err := validateNetValue(value); err != nil {
				return nil, fmt.Errorf("parameter verification failed: %s: %v", key, err)
			}
		case ipconfigKeyPattern.MatchString(key):
			if err := validateIPConfig(value); err != nil {
				return nil, fmt.Errorf("parameter verification failed: %s: %v", key, err)
			}
		case key == "nameserver":
			if err := validateNameserver(value); err != nil {
				return nil, fmt.Errorf("parameter verification failed: %s: %v", key, err)
			}
		default:
			continue
		}
		out[key] = value
	}
	return out, nil
}

// CloneMAC is the MAC address the fake gives NIC index of the VM it clones to
// vmid: PVE draws a new one for every netN of a clone, and the fake derives it
// from the clone's VMID and the NIC's index, so it is stable, distinct from
// the source's, and a test can name it.
func CloneMAC(vmid, index int) string {
	mac := net.HardwareAddr{0xBC, 0x24, 0x11, byte((vmid >> 16) & 0xFF), byte((vmid >> 8) & 0xFF), byte(vmid & 0xFF)}
	mac[5] ^= byte(index & 0xFF)
	return strings.ToUpper(mac.String())
}

// cloneNetworkMACs gives the clone of a VM config new MAC addresses, as PVE
// does for every netN of a clone: the model and the other options stay, the
// MAC is CloneMAC's.
func cloneNetworkMACs(config map[string]string, vmid int) {
	for key, value := range config {
		m := netKeyPattern.FindStringSubmatch(key)
		if m == nil {
			continue
		}
		index, _ := strconv.Atoi(m[1])
		mac := CloneMAC(vmid, index)

		parts := strings.Split(value, ",")
		for i, part := range parts {
			name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			switch {
			case nicModels[name]:
				parts[i] = name + "=" + mac
			case name == "macaddr":
				parts[i] = "macaddr=" + mac
			}
		}
		config[m[0]] = strings.Join(parts, ",")
	}
}

// SetAssignCreateMACs makes a create give every netN that names no MAC one
// (CloneMAC's, for the new VMID), as PVE does; off by default, so a test can
// read a NIC back as it was sent. Safe for concurrent use.
func (s *Server) SetAssignCreateMACs(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.assignCreateMACs = on
}

// assignNetworkMACs gives each netN of config that names no MAC the MAC
// CloneMAC draws for VM vmid, written as "<model>=<MAC>" as PVE writes it.
func assignNetworkMACs(config map[string]string, vmid int) {
	for key, value := range config {
		m := netKeyPattern.FindStringSubmatch(key)
		if m == nil {
			continue
		}
		parts := strings.Split(value, ",")
		hasMAC := false
		for _, part := range parts {
			name, _, hasVal := strings.Cut(strings.TrimSpace(part), "=")
			if (nicModels[name] && hasVal) || name == "macaddr" {
				hasMAC = true
			}
		}
		if hasMAC {
			continue
		}
		index, _ := strconv.Atoi(m[1])
		for i, part := range parts {
			if name := strings.TrimSpace(part); nicModels[name] {
				parts[i] = name + "=" + CloneMAC(vmid, index)
			}
		}
		config[key] = strings.Join(parts, ",")
	}
}
