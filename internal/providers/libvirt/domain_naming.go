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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Domain naming.
//
// A libvirt domain name is host-global: every namespace (tenant) whose
// VirtualMachines land on a host shares that host's one set of domain names,
// and every host-side artifact of a VM (its "<domain>-disk" volume, its
// imported "<domain>-migrated.qcow2" disk, its cloud-init seed and its
// domain-XML staging file) is named after the domain. Naming a domain after the
// bare VirtualMachine name therefore let two namespaces' "web" VMs share one
// name: they squatted each other's name on every host, and two concurrent
// creates raced on the same staging files and disk.
//
// A domain created for a VirtualMachine is now named "<namespace>.<name>"
// (e.g. "team-a.web"), derived by domainNameFor — the single naming rule for
// Create (single-host and clustered), Clone and the migration import. It is
// unambiguous because a Kubernetes namespace is a DNS-1123 label, which cannot
// contain '.': the first '.' always ends the namespace.
//
// Only NEW domains get the namespaced name. An existing VM keeps its domain
// name — every later operation addresses the domain by the VirtualMachine's
// status.id, which is the name Create returned — so nothing is renamed.
//
// A request without a naming identity (an older manager that sends no owner)
// falls back to the bare name it carries, exactly as before; the #333 owner
// stamp and the ID/UUID ambiguity guard still apply to it.

const (
	// domainNameSeparator joins a VirtualMachine's namespace and name into its
	// libvirt domain name, "<namespace>.<name>". A namespace (DNS-1123 label)
	// never contains it, so the split is unambiguous.
	domainNameSeparator = "."

	// maxDomainNameBytes bounds a namespaced domain name. A namespace (at most
	// 63 bytes) plus the separator plus a name (at most 253 bytes) can reach
	// 317 bytes, and host-side files append suffixes to the domain name
	// ("-disk.qcow2", "-migrated.qcow2", "-disk-temp.img", a mktemp suffix),
	// which must stay within the 255-byte file-name limit of Linux
	// filesystems. 200 bytes leaves room for the longest suffix VirtRigaud
	// uses with margin. A longer name is shortened deterministically
	// (boundDomainName).
	maxDomainNameBytes = 200

	// domainNameHashHexLen is how many hex digits of sha256("<namespace>/<name>")
	// a shortened domain name ends with, after a '-'. The hash keeps two long
	// names that share a prefix distinct and makes the shortened name stable
	// across retries.
	domainNameHashHexLen = 8

	// domainNameHashInputSeparator joins namespace and name in the hash input.
	// '/' is valid in neither, so distinct identities never hash the same input.
	domainNameHashInputSeparator = "/"

	// domainNameTruncationTrim are the characters trimmed from the end of a
	// truncated prefix before the hash suffix is appended, so a shortened name
	// never ends a segment with '.' or '-' (".-1a2b3c4d").
	domainNameTruncationTrim = ".-"
)

// hasNamingIdentity reports whether id carries the namespace AND name a domain
// name can be derived from. The UID is not needed to name a domain (a clone or
// migration target does not exist yet when it is named) and never consulted.
func hasNamingIdentity(id contracts.ObjectIdentity) bool {
	return id.Namespace != "" && id.Name != ""
}

// domainNameFor is THE libvirt domain naming rule. It returns the domain name
// for the VirtualMachine that owner identifies:
//
//   - with a naming identity (namespace and name, hasNamingIdentity):
//     "<namespace>.<name>", shortened by boundDomainName when longer than
//     maxDomainNameBytes. The namespace must be a DNS-1123 label and the name a
//     DNS-1123 subdomain (what Kubernetes admits), or the request is rejected
//     as InvalidSpec: the name reaches host file paths and virsh arguments, so
//     it must never carry a '/', a leading '-' or a character outside the DNS
//     alphabet.
//   - without one (an older manager that sends no owner): fallback, the bare
//     name the request carries — the legacy naming.
//
// Either way, a name virsh would resolve as a domain ID or UUID is rejected as
// InvalidSpec (ambiguousDomainNameError). Only the legacy fallback can hit
// that: a namespaced name always contains '.', which neither form contains.
func domainNameFor(owner contracts.ObjectIdentity, fallback string) (string, error) {
	name := fallback
	if hasNamingIdentity(owner) {
		if errs := validation.IsDNS1123Label(owner.Namespace); len(errs) > 0 {
			return "", contracts.NewInvalidSpecError(fmt.Sprintf(
				"namespace %q cannot name a libvirt domain: %s", owner.Namespace, strings.Join(errs, "; ")), nil)
		}
		if errs := validation.IsDNS1123Subdomain(owner.Name); len(errs) > 0 {
			return "", contracts.NewInvalidSpecError(fmt.Sprintf(
				"name %q cannot name a libvirt domain: %s", owner.Name, strings.Join(errs, "; ")), nil)
		}
		name = boundDomainName(owner.Namespace, owner.Name)
	}
	if err := ambiguousDomainNameError(name); err != nil {
		return "", err
	}
	return name, nil
}

// boundDomainName returns "<namespace>.<name>", or — when that is longer than
// maxDomainNameBytes — its first bytes followed by "-" and the first
// domainNameHashHexLen hex digits of sha256("<namespace>/<name>"), exactly
// maxDomainNameBytes long at most. The result is deterministic (a retried
// create finds the domain it made) and keeps two long names that share a
// prefix distinct. The namespace (at most 63 bytes) is always kept whole, so a
// shortened name still starts with "<namespace>.".
//
// Namespace and name are DNS-1123 (ASCII), so slicing by byte never splits a
// character.
func boundDomainName(namespace, name string) string {
	full := namespace + domainNameSeparator + name
	if len(full) <= maxDomainNameBytes {
		return full
	}
	sum := sha256.Sum256([]byte(namespace + domainNameHashInputSeparator + name))
	suffix := "-" + hex.EncodeToString(sum[:])[:domainNameHashHexLen]
	prefix := strings.TrimRight(full[:maxDomainNameBytes-len(suffix)], domainNameTruncationTrim)
	return prefix + suffix
}

// createDomainName is the domain name a Create request makes: domainNameFor
// over req.Owner, falling back to req.Name. When the owner carries a naming
// identity its name must be req.Name — the manager sets both from the same
// VirtualMachine, so a mismatch is a malformed request and is refused rather
// than resolved in favour of either.
func createDomainName(req contracts.CreateRequest) (string, error) {
	if hasNamingIdentity(req.Owner) && req.Owner.Name != req.Name {
		return "", contracts.NewInvalidSpecError(fmt.Sprintf(
			"create request name %q does not match its owner name %q", req.Name, req.Owner.Name), nil)
	}
	return domainNameFor(req.Owner, req.Name)
}

// pendingCreateDomainName returns the domain name Create gives the VM owner
// identifies, when a per-VM request addresses that VM by its bare name id —
// which is how the operator addresses a clustered VM whose create is still in
// flight (no status.id yet; the finalizer's cleanup Delete). ok is false when
// the request carries no naming identity, id is not the owner's name, or the
// name is already the domain name, so callers try it only as a second,
// owner-checked lookup.
func pendingCreateDomainName(id string, owner contracts.ObjectIdentity) (string, bool) {
	if !hasNamingIdentity(owner) || owner.Name != id {
		return "", false
	}
	name, err := domainNameFor(owner, id)
	if err != nil || name == id {
		return "", false
	}
	return name, true
}
