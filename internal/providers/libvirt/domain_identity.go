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
	"crypto/rand"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"unicode"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Domain identity and ownership.
//
// A libvirt domain used to be named after the bare VirtualMachine name — no
// namespace — so on a host shared by several namespaces/tenants two
// VirtualMachines named "web" mapped to the SAME domain name. Create used to
// treat "a domain of that name already exists" as an idempotent success and
// return its name as the VM's ID, silently binding tenant B's VirtualMachine to
// tenant A's domain (after which B could power it off, reconfigure, snapshot,
// delete it, or run in-guest commands through the guest agent).
//
// Create now stamps the requesting VirtualMachine's identity (UID, namespace,
// name) into the new domain's <metadata>, and binds to a pre-existing domain of
// the requested name ONLY when that stamp records the requester's UID — the
// retry-after-a-lost-status-write case. Anything else (no stamp, a different
// UID, an unreadable stamp, or a requester with no UID) fails closed with a
// non-retryable Conflict. A pre-existing domain is brought under management
// through the adoption flow, never by a same-named create.
//
// New domains are also named "<namespace>.<name>" (domain_naming.go), so two
// namespaces no longer share a name at all; the stamp remains the
// authorization, for legacy (bare-named) domains and for the rare collision a
// namespaced name can still meet (e.g. a legacy VM literally named
// "<namespace>.<name>").

const (
	// ownerMetadataNamespaceURI is the XML namespace of VirtRigaud's owner stamp
	// inside a libvirt domain's <metadata>. libvirt keys custom metadata by
	// namespace URI (one element per URI), so the URI — not the prefix — is what
	// identifies the stamp; it is versioned so the schema can evolve.
	ownerMetadataNamespaceURI = "https://virtrigaud.io/xmlns/libvirt/owner/v1"
	// ownerMetadataPrefix is the XML prefix the stamp is written with. Readers
	// must match on ownerMetadataNamespaceURI, never on the prefix.
	ownerMetadataPrefix = "virtrigaud"
	// ownerMetadataElement is the local name of the owner stamp element.
	ownerMetadataElement = "owner"

	// ownerAttrUID, ownerAttrNamespace and ownerAttrName are the owner stamp's
	// attributes. Only the UID authorizes a bind; namespace and name are for
	// operators reading `virsh dumpxml`.
	ownerAttrUID       = "uid"
	ownerAttrNamespace = "namespace"
	ownerAttrName      = "name"

	// domainXMLRootElement is the root element of a libvirt domain document.
	domainXMLRootElement = "domain"
	// domainXMLMetadataElement is the <domain> child holding custom metadata.
	domainXMLMetadataElement = "metadata"

	// uuidHexDigits is the number of hex digits in a UUID (128 bits).
	uuidHexDigits = 32
)

// renderOwnerMetadataXML renders the <metadata> element that stamps owner onto
// a new domain, indented to sit directly under <domain>. It returns "" for a
// zero owner (no UID): there is nothing to record, and such a domain can never
// be bound by a later same-named create anyway.
//
// Every value is CR-derived and passed through xmlEscape (issue #260), so a
// quote or angle bracket cannot break out of its attribute.
func renderOwnerMetadataXML(owner contracts.ObjectIdentity) string {
	if owner.IsZero() {
		return ""
	}
	return fmt.Sprintf("  <%s>\n    <%s:%s xmlns:%s='%s' %s='%s' %s='%s' %s='%s'/>\n  </%s>\n",
		domainXMLMetadataElement,
		ownerMetadataPrefix, ownerMetadataElement, ownerMetadataPrefix, ownerMetadataNamespaceURI,
		ownerAttrUID, xmlEscape(owner.UID),
		ownerAttrNamespace, xmlEscape(owner.Namespace),
		ownerAttrName, xmlEscape(owner.Name),
		domainXMLMetadataElement)
}

// ownerElement is one VirtRigaud owner stamp found in a domain document, with
// the byte span it occupies so it can be spliced out verbatim.
type ownerElement struct {
	identity   contracts.ObjectIdentity
	start, end int64
}

// scanOwnerElements walks a libvirt domain document and returns every owner
// stamp that is a direct child of /domain/metadata, matched by namespace URI
// (so the prefix libvirt re-serializes it with does not matter, and an element
// in any other namespace is ignored).
//
// It only TOKENIZES the document — it never re-serializes it — so a caller that
// removes a stamp splices the returned byte span out of the original text and
// every other byte (including other tools' metadata and its namespace
// declarations) is preserved exactly (see the ADR-0008 D2 note in
// libvirtxml_domain.go on why domain XML is never parse->mutate->serialized).
func scanOwnerElements(domainXML string) ([]ownerElement, error) {
	dec := xml.NewDecoder(strings.NewReader(domainXML))

	var (
		found      []ownerElement
		depth      int
		rootSeen   bool
		inMetadata bool
		// ownerDepth is the depth of the owner element currently being read
		// (0 when not inside one) and ownerStart its opening byte offset.
		ownerDepth int
		ownerStart int64
		current    contracts.ObjectIdentity
	)
	for {
		offset := dec.InputOffset()
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse domain XML: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case depth == 1 && rootSeen:
				// libvirt never emits two root elements; Go's decoder tolerates
				// them, so refuse rather than read a stamp from a second root.
				return nil, fmt.Errorf("parse domain XML: more than one root element")
			case depth == 1 && t.Name.Local != domainXMLRootElement:
				return nil, fmt.Errorf("parse domain XML: root element is <%s>, not <%s>", t.Name.Local, domainXMLRootElement)
			case depth == 2 && t.Name.Space == "" && t.Name.Local == domainXMLMetadataElement:
				inMetadata = true
			case depth == 3 && inMetadata &&
				t.Name.Space == ownerMetadataNamespaceURI && t.Name.Local == ownerMetadataElement:
				id, aerr := ownerFromAttrs(t.Attr)
				if aerr != nil {
					return nil, aerr
				}
				ownerDepth, ownerStart, current = depth, offset, id
			}
			if depth == 1 {
				rootSeen = true
			}
		case xml.EndElement:
			if ownerDepth != 0 && depth == ownerDepth {
				found = append(found, ownerElement{identity: current, start: ownerStart, end: dec.InputOffset()})
				ownerDepth = 0
			}
			if depth == 2 && inMetadata {
				inMetadata = false
			}
			depth--
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("parse domain XML: unexpected end of document")
	}
	return found, nil
}

// ownerFromAttrs reads the owner stamp's unqualified uid/namespace/name
// attributes. Attributes in any namespace are ignored. A repeated attribute is
// an error: XML forbids it and libvirt never emits it, but Go's decoder accepts
// it (last one wins), so it is refused rather than silently resolved.
func ownerFromAttrs(attrs []xml.Attr) (contracts.ObjectIdentity, error) {
	var id contracts.ObjectIdentity
	seen := make(map[string]bool, len(attrs))
	for _, a := range attrs {
		if a.Name.Space != "" {
			continue
		}
		switch a.Name.Local {
		case ownerAttrUID, ownerAttrNamespace, ownerAttrName:
			if seen[a.Name.Local] {
				return contracts.ObjectIdentity{}, fmt.Errorf("parse domain XML: owner stamp repeats attribute %q", a.Name.Local)
			}
			seen[a.Name.Local] = true
		}
		switch a.Name.Local {
		case ownerAttrUID:
			id.UID = a.Value
		case ownerAttrNamespace:
			id.Namespace = a.Value
		case ownerAttrName:
			id.Name = a.Value
		}
	}
	return id, nil
}

// domainOwners returns the owner identities stamped on a domain document (see
// scanOwnerElements). A domain VirtRigaud never created has none.
func domainOwners(domainXML string) ([]contracts.ObjectIdentity, error) {
	elems, err := scanOwnerElements(domainXML)
	if err != nil {
		return nil, err
	}
	owners := make([]contracts.ObjectIdentity, 0, len(elems))
	for _, e := range elems {
		owners = append(owners, e.identity)
	}
	return owners, nil
}

// domainOwnerUIDs returns the non-empty owner UIDs stamped on a domain document,
// comma-separated in document order, for VMInfo.ProviderRaw
// (contracts.VMInfoOwnerUIDKey). An unstamped or unreadable document yields "".
// It is informational (adoption uses it to skip domains live VirtualMachines
// own); it never authorizes anything.
func domainOwnerUIDs(domainXML string) string {
	owners, err := domainOwners(domainXML)
	if err != nil {
		return ""
	}
	uids := make([]string, 0, len(owners))
	for _, o := range owners {
		if o.UID != "" {
			uids = append(uids, o.UID)
		}
	}
	return strings.Join(uids, ",")
}

// requesterOwnsDomain reports whether a create by requester may bind to an
// existing domain whose stamped owners are recorded. It is the whole
// authorization decision and fails closed:
//
//   - a requester without a UID (e.g. an older manager that does not send one)
//     owns nothing — it must never bind to an existing domain;
//   - a domain with no stamp (never created by VirtRigaud, or created before
//     stamping existed) is owned by nobody who can prove it;
//   - more than one stamp is ambiguous (libvirt keeps one element per namespace,
//     so this is a hand-edited definition) and is refused;
//   - otherwise the single stamp's UID must equal the requester's exactly.
//     Namespace and name are informational and never consulted.
func requesterOwnsDomain(requester contracts.ObjectIdentity, recorded []contracts.ObjectIdentity) bool {
	if requester.IsZero() || len(recorded) != 1 {
		return false
	}
	return recorded[0].UID == requester.UID
}

// bindExistingDomain decides a create whose domain — domainName, the name
// createDomainName gave this request — already exists on vp's host. It reads
// the domain's owner stamp and returns domainName as the VM ID (idempotent
// success) ONLY when requesterOwnsDomain says the stamp records req.Owner's
// UID — the case of a create retried after the manager lost its Status.ID
// write, which derives the same (namespaced) name again. Otherwise it returns a
// non-retryable Conflict and never binds.
//
// The error message is deliberately uniform and names only the requested
// domain: it lands in the requesting VirtualMachine's status, so it must not
// disclose which other namespace/VirtualMachine (if any) owns the domain. The
// details go to the provider log for the operator.
//
// domainName has already passed ambiguousDomainNameError (domainNameFor), so
// `virsh dumpxml <name>` cannot resolve to a different domain by ID or UUID.
func bindExistingDomain(ctx context.Context, vp *VirshProvider, req contracts.CreateRequest, domainName, state string) (contracts.CreateResponse, error) {
	res, err := vp.runVirshCommand(ctx, "dumpxml", domainName)
	if err != nil {
		// Transient (connection) or the domain vanished since the list — both
		// resolve on retry, which re-lists and re-decides.
		return contracts.CreateResponse{}, contracts.NewRetryableError(
			fmt.Sprintf("read owner metadata of existing domain %q", domainName), err)
	}

	recorded, perr := domainOwners(res.Stdout)
	if perr == nil && requesterOwnsDomain(req.Owner, recorded) {
		log.Printf("INFO Domain %s already exists (state: %s) and is owned by this VirtualMachine (uid %s); treating create as idempotent",
			domainName, state, req.Owner.UID)
		return contracts.CreateResponse{ID: domainName}, nil
	}

	switch {
	case perr != nil:
		log.Printf("WARN Refusing to bind VirtualMachine %s/%s (uid %q) to existing domain %s: its owner metadata could not be read: %v",
			req.Owner.Namespace, req.Owner.Name, req.Owner.UID, domainName, perr)
	case req.Owner.IsZero():
		log.Printf("WARN Refusing to bind existing domain %s: the create request carries no owner UID (manager older than the provider?), "+
			"so ownership cannot be proven", domainName)
	case len(recorded) == 0:
		log.Printf("WARN Refusing to bind VirtualMachine %s/%s (uid %s) to existing domain %s: it has no VirtRigaud owner metadata "+
			"(not created by VirtRigaud, or created before ownership stamping); adopt it instead",
			req.Owner.Namespace, req.Owner.Name, req.Owner.UID, domainName)
	default:
		log.Printf("WARN Refusing to bind VirtualMachine %s/%s (uid %s) to existing domain %s: it is owned by %v",
			req.Owner.Namespace, req.Owner.Name, req.Owner.UID, domainName, recorded)
	}
	return contracts.CreateResponse{}, contracts.NewConflictError(fmt.Sprintf(
		"libvirt domain %q already exists on the host and is not owned by this VirtualMachine; "+
			"refusing to bind to it. Bring the existing domain under management through the adoption flow, "+
			"remove it, or rename the VirtualMachine", domainName), nil)
}

// bindOwnedLegacyDomain decides a namespaced create when a domain with the
// request's LEGACY (bare) name, legacyName (legacyCreateDomainName), exists on
// vp's host. It binds that domain — the VM ID is legacyName — ONLY when its
// owner stamp records req.Owner's UID: this VirtualMachine created it before
// domains were namespaced and the manager lost the status.id write, so
// creating "<namespace>.<name>" would leave a second domain (and its disk and
// seed) behind. bound is true then.
//
// A bare-named domain with a missing, foreign or unreadable stamp is someone
// else's legacy domain (e.g. another namespace's pre-upgrade "web"): it is not
// a conflict for the namespaced create, which proceeds (bound false, nil
// error). Only a failure to read the domain is an error (retryable), because
// proceeding then could leave this VM's own legacy domain behind.
func bindOwnedLegacyDomain(ctx context.Context, vp *VirshProvider, req contracts.CreateRequest, legacyName, state string) (contracts.CreateResponse, bool, error) {
	res, err := vp.runVirshCommand(ctx, "dumpxml", legacyName)
	if err != nil {
		return contracts.CreateResponse{}, false, contracts.NewRetryableError(
			fmt.Sprintf("read owner metadata of existing domain %q", legacyName), err)
	}
	recorded, perr := domainOwners(res.Stdout)
	if perr == nil && requesterOwnsDomain(req.Owner, recorded) {
		log.Printf("INFO Domain %s (legacy, un-namespaced name; state: %s) is owned by this VirtualMachine (uid %s); "+
			"binding to it instead of creating a namespaced domain", legacyName, state, req.Owner.UID)
		return contracts.CreateResponse{ID: legacyName}, true, nil
	}
	log.Printf("INFO Domain %s (legacy, un-namespaced name) exists but is not owned by VirtualMachine %s/%s (uid %s); "+
		"creating its namespaced domain", legacyName, req.Owner.Namespace, req.Owner.Name, req.Owner.UID)
	return contracts.CreateResponse{}, false, nil
}

// stripOwnerMetadata removes every VirtRigaud owner stamp from a domain
// document by splicing out exactly the bytes each stamp occupies, leaving the
// rest of the document byte-identical. The clone path uses it so a cloned
// domain does not inherit (and falsely claim) the SOURCE VirtualMachine's owner.
func stripOwnerMetadata(domainXML string) (string, error) {
	elems, err := scanOwnerElements(domainXML)
	if err != nil {
		return "", err
	}
	out := domainXML
	// Splice from the last span backwards so earlier offsets stay valid.
	for i := len(elems) - 1; i >= 0; i-- {
		out = out[:elems[i].start] + out[elems[i].end:]
	}
	return out, nil
}

// ambiguousDomainNameError returns a non-retryable InvalidSpec error when name
// cannot be used safely as a libvirt domain name, or nil when it can.
//
// virsh resolves a domain argument by ID first (if it parses as a non-negative
// integer), then by UUID (if it parses as one), and only then by name. A domain
// named "12" or "1b4e28ba-2fa1-11d2-883f-0016d3cca427" would therefore make
// every later by-name operation (power, delete, snapshot, guest-agent exec)
// address a DIFFERENT domain whenever one with that ID/UUID exists. Such names
// are rejected at create time instead. VirtualMachine CRD validation is
// deliberately NOT tightened: it is shared by every provider and is released
// v1beta1 API.
func ambiguousDomainNameError(name string) error {
	if looksLikeVirshDomainID(name) || looksLikeUUID(name) {
		return contracts.NewInvalidSpecError(fmt.Sprintf(
			"name %q cannot be used as a libvirt domain name: virsh would resolve it as a domain ID or UUID "+
				"before trying it as a name, so later operations could address a different domain; "+
				"rename the VirtualMachine (for example, start it with a letter)", name), nil)
	}
	return nil
}

// looksLikeVirshDomainID mirrors virsh's by-ID lookup (virStrToLong_i on the
// whole argument, base 10, then id >= 0): optional leading whitespace, an
// optional sign, then only decimal digits. "-0" is included because it parses
// to ID 0. Negative values are rejected too — they are never valid DNS names,
// so being conservative costs nothing.
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

// looksLikeUUID reports whether name would satisfy libvirt's lenient UUID
// parser (virUUIDParse): exactly 32 hex digits, optionally separated by '-' or
// whitespace. It is slightly MORE permissive than virUUIDParse (which only
// skips separators between digit pairs), which errs toward rejecting a name.
func looksLikeUUID(name string) bool {
	digits := 0
	for _, r := range name {
		switch {
		case r == '-' || unicode.IsSpace(r):
			// Separator: skipped by virUUIDParse.
		case (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'):
			digits++
		default:
			return false
		}
	}
	return digits == uuidHexDigits
}

// generateRandomUUID returns a random RFC 4122 version-4 UUID drawn from
// crypto/rand. Every new domain (create and clone) gets one, so a domain UUID is
// neither predictable nor reused.
//
// It is not what keeps concurrent creates apart: libvirt does refuse a define
// whose name exists under a different UUID, but the staged files are kept
// apart by the namespaced domain name (domain_naming.go) and by per-create
// staging paths (staging.go).
func generateRandomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random bytes for domain UUID: %w", err)
	}
	// Set version (4) and variant (10xx) bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
