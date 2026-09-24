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

package libvirt

import (
	"bytes"
	"encoding/xml"
)

// xmlEscape returns s with the XML special characters (& < > ' ") replaced by
// their entity equivalents, making it safe to interpolate into a generated
// libvirt XML document — whether the destination is a single-quoted attribute
// value (e.g. `<source network='%s'/>`) or element text content (e.g.
// `<name>%s</name>`).
//
// # Why this exists (issue #260)
//
// This package builds domain/pool XML with fmt.Sprintf, interpolating
// CR-derived strings — network/bridge names, NIC models, MAC addresses,
// VM/clone target names, disk and cloud-init ISO paths — directly into the
// template. None of the corresponding CRD fields (e.g.
// LibvirtNetworkConfig.NetworkName, BridgeConfig.Name) carried a
// kubebuilder Pattern ruling out XML metacharacters, so any principal with
// RBAC to create a VMNetworkAttachment or VirtualMachine could reach two
// distinct failure modes:
//
//   - A value containing a bare `'` breaks out of the attribute, producing
//     malformed XML that `virsh define` rejects — a define-time denial of
//     service.
//   - A value containing `'/><disk type='block'><source dev='/dev/sda'/></disk><x a='`
//     splices a sibling element into the document — e.g. a disk pointing at
//     a host block device, letting a tenant read hypervisor-local storage
//     from inside their own VM.
//
// xmlEscape closes both by being the single choke point every XML-generating
// call site in this package funnels a CR-derived value through, rather than
// each call site hand-rolling its own replacer (and inevitably missing one).
// It escapes the single quote (') and double quote (") in addition to the
// standard `& < >` set: every attribute this package emits is single-quoted,
// so an un-escaped `'` is exactly the injection primitive above, and escaping
// `"` too keeps the helper correct if a call site ever uses double quotes.
//
// It is deliberately implemented via encoding/xml.EscapeText (which already
// escapes all five characters, plus raw control characters and invalid
// UTF-8) rather than a hand-written strings.Replacer, so the escaping rules
// stay in lock-step with the standard library's own understanding of the XML
// spec instead of a second, hand-maintained copy.
//
// For a value that contains none of these characters — the overwhelmingly
// common case (hostnames, bridge names, MAC addresses) — xmlEscape is a
// no-op and the generated document is byte-identical to the unescaped
// output. This is what makes applying it uniformly to every user/CR-derived
// interpolation a behavior-preserving change for all legitimate inputs.
func xmlEscape(s string) string {
	var buf bytes.Buffer
	// encoding/xml.EscapeText only ever fails if the io.Writer it is given
	// fails; bytes.Buffer's Write never returns an error, so this error is
	// unreachable and is safely discarded rather than plumbed through every
	// call site of a function that cannot otherwise fail.
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
