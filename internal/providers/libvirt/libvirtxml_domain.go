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
	"fmt"
	"regexp"
	"strings"

	"libvirt.org/go/libvirtxml"
)

// This file is the ADR-0008 D2 typed-XML entry point: it parses libvirt domain
// XML into a typed *libvirtxml.Domain (pure Go, CGO_ENABLED=0, zero transitive
// deps — the reason D1's pure-Go stack is even possible) and carries the one
// upstream bug fix D2 requires.
//
// # The carried <metadata> namespace-declaration fix (ADR-0008 Fact 6 / Q6)
//
// libvirtxml models <metadata> as `DomainMetadata{ XML string xml:",innerxml" }`.
// `,innerxml` captures the element's CHILDREN but not the <metadata> tag's OWN
// attributes — so a round-trip (Unmarshal then Marshal) DROPS the namespace
// declarations that live on the <metadata> element itself
// (xmlns:libosinfo=..., xmlns:cockpit_machines=..., ...). The children that use
// those prefixes then become namespace-invalid XML, confirmed by ADR-0008's
// verification with real xmllint errors.
//
// D2's actual mitigation is the RULE "never parse -> mutate -> serialize a domain
// definition"; PR 4b's Describe read path only PARSES, so it never hits this on
// the hot path. But D2/Q6 also require carrying the ~50-line hoist/restore fix
// now, with a round-trip test, so any future write path that does serialize is
// already protected and the carry is tracked until the fix lands upstream.
//
// Upstream follow-up: report to libvirt.org/go/libvirtxml (the fix belongs in
// DomainMetadata's (un)marshalling); remove restoreMetadataNamespaces and this
// note once merged and the dependency is bumped past it. Tracked as the ADR-0008
// D2 upstream-report follow-up.

// metadataOpenTagRE matches the opening <metadata ...> tag (not self-closing) so
// its own attribute list can be hoisted/restored. It deliberately does not match
// a self-closing <metadata/> (which has no children and so no lost namespaces).
var metadataOpenTagRE = regexp.MustCompile(`(?s)<metadata(\s[^>]*?)?>`)

// xmlnsAttrRE matches a single xmlns or xmlns:prefix declaration inside a tag's
// attribute list, capturing the whole `xmlns...="..."` (or single-quoted) token.
var xmlnsAttrRE = regexp.MustCompile(`xmlns(?::[A-Za-z_][\w.-]*)?\s*=\s*("[^"]*"|'[^']*')`)

// parseDomainLibvirtxml unmarshals a libvirt domain XML document into a typed
// *libvirtxml.Domain. It is the single go-libvirt-side parse entry point for the
// ADR-0008 PR 4b Describe shadow path; the round-trip write path (a future PR)
// must pair it with marshalDomainLibvirtxml so the carried <metadata> fix applies.
func parseDomainLibvirtxml(raw string) (*libvirtxml.Domain, error) {
	d := &libvirtxml.Domain{}
	if err := d.Unmarshal(raw); err != nil {
		return nil, fmt.Errorf("parse libvirt domain XML: %w", err)
	}
	return d, nil
}

// marshalDomainLibvirtxml serializes d back to XML and restores the <metadata>
// element's own namespace declarations that libvirtxml drops (see the file doc).
// srcXML is the original document d was parsed from; its <metadata> attributes are
// the source of truth for what to restore. If d has no <metadata> or the source
// carried no namespace declarations on it, this is exactly libvirtxml's Marshal.
//
// PR 4b does not serialize domain XML on any production path — this exists so the
// carried fix is present and tested before the first write path needs it (D2/Q6).
func marshalDomainLibvirtxml(d *libvirtxml.Domain, srcXML string) (string, error) {
	out, err := d.Marshal()
	if err != nil {
		return "", fmt.Errorf("marshal libvirt domain XML: %w", err)
	}
	return restoreMetadataNamespaces(out, srcXML), nil
}

// restoreMetadataNamespaces is the CARRIED FIX. It copies any xmlns / xmlns:prefix
// declarations present on the <metadata> open tag of src into the <metadata> open
// tag of marshaled, without duplicating ones marshaled already has. It is a pure
// string transform on the <metadata> open tag only; element content and every
// other tag are untouched.
func restoreMetadataNamespaces(marshaled, src string) string {
	srcTag := metadataOpenTagRE.FindString(src)
	if srcTag == "" {
		return marshaled // no <metadata> open tag in source (or self-closing) — nothing to restore
	}
	srcAttrs := xmlnsAttrRE.FindAllString(srcTag, -1)
	if len(srcAttrs) == 0 {
		return marshaled // source <metadata> declared no namespaces
	}

	loc := metadataOpenTagRE.FindStringIndex(marshaled)
	if loc == nil {
		return marshaled // marshaled output has no <metadata> open tag to fix
	}
	marTag := marshaled[loc[0]:loc[1]]

	// Only add declarations the marshaled tag is actually missing, so we never
	// duplicate an xmlns the library did preserve.
	existing := make(map[string]bool)
	for _, a := range xmlnsAttrRE.FindAllString(marTag, -1) {
		existing[xmlnsAttrName(a)] = true
	}
	var missing []string
	for _, a := range srcAttrs {
		if !existing[xmlnsAttrName(a)] {
			missing = append(missing, a)
		}
	}
	if len(missing) == 0 {
		return marshaled
	}

	// Insert the missing declarations right after "<metadata", before the rest of
	// the tag (attributes or the closing ">").
	const openLen = len("<metadata")
	fixedTag := marTag[:openLen] + " " + strings.Join(missing, " ") + marTag[openLen:]
	return marshaled[:loc[0]] + fixedTag + marshaled[loc[1]:]
}

// xmlnsAttrName returns the declaration name of an xmlns attribute token
// (e.g. `xmlns:libosinfo="..."` -> "xmlns:libosinfo"), used to de-duplicate
// against declarations the marshaled tag already carries.
func xmlnsAttrName(attr string) string {
	if i := strings.IndexByte(attr, '='); i >= 0 {
		return strings.TrimSpace(attr[:i])
	}
	return strings.TrimSpace(attr)
}

// domainMemoryMiB returns d's configured memory in MiB, applying the same
// contract guard domainxml.go uses: libvirt's XML formatter always emits <memory>
// in KiB (domain_conf.c hardcodes unit='KiB'), so a non-KiB unit means the XML did
// not come from libvirt and is rejected rather than silently mis-scaled by a blind
// /1024. A nil <memory> yields (0, nil): the field is simply absent, not an error.
func domainMemoryMiB(d *libvirtxml.Domain) (int64, bool, error) {
	if d.Memory == nil {
		return 0, false, nil
	}
	if u := d.Memory.Unit; u != "" && u != "KiB" {
		return 0, false, fmt.Errorf("unexpected memory unit %q (want KiB)", u)
	}
	return int64(d.Memory.Value) / 1024, true, nil
}
