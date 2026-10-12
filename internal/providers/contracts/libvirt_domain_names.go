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

package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

// The libvirt domain naming rule, shared by the libvirt provider (which makes
// the names, internal/providers/libvirt/domain_naming.go) and the manager
// (which looks for a domain a Create may have made). A drift-guard test in
// the libvirt package pins it to the provider's own rule.
const (
	libvirtDomainNameSeparator  = "."
	libvirtMaxDomainNameBytes   = 200
	libvirtDomainNameHashHexLen = 16
	libvirtDomainNameHashSep    = "_"
	libvirtDomainNameHashInput  = "/"
	libvirtDomainNameTrimCutset = ".-"
	libvirtUUIDHexDigits        = 32
)

// LibvirtBoundDomainName returns the libvirt domain name of the
// VirtualMachine namespace/name: "<namespace>.<name>", or — when that is
// longer than 200 bytes — its first bytes followed by "_" and the first 16
// hex digits of sha256("<namespace>/<name>"), 200 bytes long at most.
// Namespace and name are DNS-1123 (as Kubernetes admits them).
func LibvirtBoundDomainName(namespace, name string) string {
	full := namespace + libvirtDomainNameSeparator + name
	if len(full) <= libvirtMaxDomainNameBytes {
		return full
	}
	sum := sha256.Sum256([]byte(namespace + libvirtDomainNameHashInput + name))
	suffix := libvirtDomainNameHashSep + hex.EncodeToString(sum[:])[:libvirtDomainNameHashHexLen]
	return strings.TrimRight(full[:libvirtMaxDomainNameBytes-len(suffix)], libvirtDomainNameTrimCutset) + suffix
}

// LibvirtCreateDomainNames returns every libvirt domain name a Create for the
// VirtualMachine namespace/name could have used: its bound name
// (LibvirtBoundDomainName) and, when usable, the bare legacy name it had
// before domains were namespaced — name itself, unless it contains a '.' or
// virsh would resolve it as a domain ID or UUID. name must be a DNS-1123
// subdomain (a VirtualMachine name).
func LibvirtCreateDomainNames(namespace, name string) []string {
	bound := LibvirtBoundDomainName(namespace, name)
	names := []string{bound}
	if name != bound && !strings.Contains(name, libvirtDomainNameSeparator) &&
		!looksLikeVirshDomainID(name) && !looksLikeVirshUUID(name) {
		names = append(names, name)
	}
	return names
}

// looksLikeVirshDomainID reports whether virsh would resolve name as a domain
// ID: optional leading whitespace, an optional sign, then only decimal digits.
func looksLikeVirshDomainID(name string) bool {
	s := strings.TrimLeftFunc(name, unicode.IsSpace)
	if strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// looksLikeVirshUUID reports whether virsh would resolve name as a domain
// UUID: exactly 32 hex digits, optionally separated by '-' or whitespace.
func looksLikeVirshUUID(name string) bool {
	digits := 0
	for _, r := range name {
		switch {
		case r == '-' || unicode.IsSpace(r):
		case (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'):
			digits++
		default:
			return false
		}
	}
	return digits == libvirtUUIDHexDigits
}
