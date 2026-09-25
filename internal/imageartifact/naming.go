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

package imageartifact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// NameRule selects a hypervisor's artifact-name budget and separator
// (ADR-0009 D1). The zero value is not a valid rule.
type NameRule int

const (
	// NameRuleVSphere names a vSphere template: at most 80 bytes (the vSphere
	// entity-name limit), separator '_'.
	NameRuleVSphere NameRule = iota + 1
	// NameRuleLibvirt names a libvirt pool file's base name (before
	// ".qcow2"): at most 200 bytes (#339's bound, leaving room under the
	// 255-byte file-name limit), separator '_'.
	NameRuleLibvirt
	// NameRuleProxmox names a Proxmox template: at most 80 bytes, separator
	// '-', because PVE validates a VM name as a DNS name, which has no '_'.
	//
	// The name is cosmetic on PVE, and it is NOT disjoint from Kubernetes
	// names: a Proxmox artifact name is a valid DNS name, so it may equal a
	// legacy bare VMImage name or another object's name, and PVE names are
	// not unique anyway. A Proxmox provider must therefore never resolve,
	// reuse or adopt an artifact by name: only by its vr-img-<h16> tag and a
	// matching stamp (with its own VMID) inside the provider's own PVE pool
	// (ADR-0009 D3, D5). It must never serve a legacy (identity-less) request
	// by name either (ADR-0009 D10). PVE's per-label length limit, if any, is
	// verified in Slice 6.
	NameRuleProxmox
)

const (
	// nameSchemeVersion prefixes the hash input, so a future naming scheme can
	// never produce a name of this one for a different identity.
	nameSchemeVersion = "v1"
	// nameHashInputSeparator joins the parts of the hash input. The digest is
	// fixed-length and '/' never occurs in it, so the input is unambiguous.
	nameHashInputSeparator = "/"
	// nameHashHexLen is how many hex digits (64 bits) of the hash every
	// artifact name ends with.
	nameHashHexLen = 16
	// nameNamespaceSeparator joins the namespace and name in the readable
	// prefix. A namespace (DNS-1123 label) never contains it.
	nameNamespaceSeparator = "."
	// namePrefixTrim are trimmed from the end of a cut prefix, so no segment
	// before the separator ends in '.' or '-'.
	namePrefixTrim = ".-"
	// maxImageUIDBytes bounds an identity's UID (a Kubernetes UID is a
	// 36-byte UUID).
	maxImageUIDBytes = 128
)

// MaxBytes is the longest artifact name r allows, in bytes (0 for an unknown
// rule).
func (r NameRule) MaxBytes() int {
	switch r {
	case NameRuleVSphere, NameRuleProxmox:
		return 80
	case NameRuleLibvirt:
		return 200
	}
	return 0
}

// Separator is the string between the readable prefix and the hash of an
// artifact name under r ("" for an unknown rule).
func (r NameRule) Separator() string {
	switch r {
	case NameRuleVSphere, NameRuleLibvirt:
		return "_"
	case NameRuleProxmox:
		return "-"
	}
	return ""
}

// String returns the rule's hypervisor name.
func (r NameRule) String() string {
	switch r {
	case NameRuleVSphere:
		return "vsphere"
	case NameRuleLibvirt:
		return "libvirt"
	case NameRuleProxmox:
		return "proxmox"
	}
	return fmt.Sprintf("NameRule(%d)", int(r))
}

// ArtifactName is THE prepared-image artifact naming rule (ADR-0009 D1). It
// returns the hypervisor-side name of the artifact for the VMImage image
// prepared from the source whose digest is sourceDigest:
//
//	h16    = hex(sha256("v1/" + image.UID + "/" + sourceDigest))[:16]
//	prefix = image.Namespace + "." + image.Name, cut to
//	         (rule.MaxBytes() - len(rule.Separator()) - 16) bytes, then
//	         trailing '.' and '-' trimmed
//	name   = prefix + rule.Separator() + h16
//
// Only a provider calls it: the naming rule belongs to the provider, and the
// manager never derives an artifact name. Properties (tested):
//
//   - the UID and the digest are in the name (through h16): a re-created
//     VMImage or a changed spec.source gets a new artifact, never an old one;
//   - every name has the same shape and ends in the separator plus 16 lowercase
//     hex digits; there is no separate untruncated form;
//   - the namespace (at most 63 bytes) always fits whole, so two namespaces
//     never share a prefix;
//   - under the '_' rules (vSphere, libvirt) a name never equals a Kubernetes
//     name, a legacy bare VMImage name or a #339 "<namespace>.<name>" domain
//     name, since none can contain '_'. This does NOT hold under
//     NameRuleProxmox, whose names are valid DNS names: Proxmox never looks
//     an artifact up by name (see NameRuleProxmox);
//   - it cannot be predicted before the VMImage exists (its UID is assigned by
//     the API server). This is not access control: names are not secrets.
//
// image.UID must be a plausible UID (letters, digits and '-', at most 128
// bytes), image.Namespace a DNS-1123 label, image.Name a DNS-1123 subdomain,
// and sourceDigest a valid source digest; otherwise an error is returned.
func ArtifactName(rule NameRule, image contracts.ObjectIdentity, sourceDigest string) (string, error) {
	budget, sep := rule.MaxBytes(), rule.Separator()
	if budget == 0 {
		return "", fmt.Errorf("unknown artifact name rule %s", rule)
	}
	if err := validateImageIdentity(image); err != nil {
		return "", err
	}
	if err := ValidateSourceDigest(sourceDigest); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(nameSchemeVersion + nameHashInputSeparator + image.UID +
		nameHashInputSeparator + sourceDigest))
	h16 := hex.EncodeToString(sum[:])[:nameHashHexLen]

	prefix := image.Namespace + nameNamespaceSeparator + image.Name
	if limit := budget - len(sep) - nameHashHexLen; len(prefix) > limit {
		// Namespace and name are DNS-1123 (ASCII), so a byte cut never splits
		// a character.
		prefix = prefix[:limit]
	}
	prefix = strings.TrimRight(prefix, namePrefixTrim)
	return prefix + sep + h16, nil
}

// validateImageIdentity returns an error unless image can name and stamp an
// artifact: a plausible UID, a DNS-1123 label namespace and a DNS-1123
// subdomain name.
func validateImageIdentity(image contracts.ObjectIdentity) error {
	if err := validateUID(image.UID); err != nil {
		return fmt.Errorf("image identity: %w", err)
	}
	if errs := validation.IsDNS1123Label(image.Namespace); len(errs) > 0 {
		return fmt.Errorf("image identity: namespace %q: %s", image.Namespace, strings.Join(errs, "; "))
	}
	if errs := validation.IsDNS1123Subdomain(image.Name); len(errs) > 0 {
		return fmt.Errorf("image identity: name %q: %s", image.Name, strings.Join(errs, "; "))
	}
	return nil
}

// validateUID returns an error unless uid is non-empty, at most
// maxImageUIDBytes long and made only of ASCII letters, digits and '-' (the
// alphabet of a Kubernetes UID). A UID is written into stamps on the
// hypervisor, so it is never an arbitrary string.
func validateUID(uid string) error {
	if uid == "" {
		return fmt.Errorf("uid is empty")
	}
	if len(uid) > maxImageUIDBytes {
		return fmt.Errorf("uid is longer than %d bytes", maxImageUIDBytes)
	}
	for i := 0; i < len(uid); i++ {
		c := uid[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '-' {
			return fmt.Errorf("uid %q contains a character other than a letter, digit or '-'", uid)
		}
	}
	return nil
}
