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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

const (
	// SourceDigestVersion is the version of the canonical form SourceDigest
	// hashes. It is part of the hashed envelope, so changing it changes every
	// digest and therefore every artifact name: every image is prepared again
	// once, under a new name. Bump it only as a deliberate migration.
	SourceDigestVersion = 1

	// SourceDigestPrefix starts every source digest; the rest is the lowercase
	// hex SHA-256 of the canonical form.
	SourceDigestPrefix = "sha256:"

	// SourceDigestPattern is the syntax of a source digest. It is the pattern
	// of VMImage status.providerStatus[].sourceDigest in the CRD (kept
	// identical, see TestSourceDigestPatternMatchesCRD).
	SourceDigestPattern = `^sha256:[0-9a-f]{64}$`

	// sourceDigestHexLen is the number of hex digits after SourceDigestPrefix.
	sourceDigestHexLen = sha256.Size * 2
)

// sourceDigestEnvelope is the versioned document SourceDigest hashes.
type sourceDigestEnvelope struct {
	// Version is SourceDigestVersion.
	Version int `json:"v"`
	// Source is the VMImage spec.source with the transport-only fields
	// cleared (see SourceDigest).
	Source infravirtrigaudiov1beta1.ImageSource `json:"source"`
}

// SourceDigest returns the digest of a VMImage's spec.source (ADR-0009 D2):
//
//	"sha256:" + hex(sha256(canonical({"source": <spec.source>, "v": 1})))
//
// It identifies WHICH content an artifact was prepared from. The prepared
// artifact's name and stamp carry it, so a changed spec.source gets a new
// artifact and is never served the old one.
//
// What it covers: every field of spec.source that says WHAT the image is or
// WHERE it is placed — URLs and paths, the expected checksum and its
// algorithm, formats, and the location fields such as libvirt storagePool and
// Proxmox storage and node (ADR-0009 Q7: "location yes, transport no").
//
// What it excludes, because each changes how the bytes are fetched or who
// fetches them, never which bytes are expected or where they land (so
// changing one must not orphan multi-GB artifacts):
//
//   - source.http.timeout: how long a download may take;
//   - source.http.headers: request headers, which may carry inline tokens;
//   - source.http.authentication: the credential references of the download;
//   - source.registry.pullSecretRef: the credential reference of the pull;
//   - source.vsphere.providerRef: which Provider imports the image (routing
//     and credentials);
//   - the userinfo ("user:password@") of every URL field — source.http.url,
//     source.libvirt.url and source.vsphere.ovaURL: it is a credential, so
//     hashing it would orphan artifacts on every rotation and put a hash of
//     guessable credential material into stamps on the hypervisor. It must be
//     percent-encoded per RFC 3986 to be recognized ('/', '?' and '#' end the
//     authority).
//
// A URL's scheme, host, path, query and fragment are kept: a query string may
// select the content. A presigned or token-bearing query string is therefore
// part of the digest: rotating it gets a new artifact (one re-import). Put
// credentials in source.http.authentication (a Secret reference) or in
// headers instead of the URL.
//
// Nothing outside spec.source is covered:
// not spec.metadata, spec.distribution or spec.consumerNamespaceSelector, and
// not spec.prepare, which no provider reads today. A spec.prepare field that a
// provider starts honouring must be added to the digest in the same change,
// together with a bump of SourceDigestVersion or an argument why existing
// digests stay correct.
//
// Canonical form: the typed ImageSource is encoded with encoding/json (the
// field names are the CRD's JSON names; omitempty fields that are unset are
// absent) inside the versioned envelope, then re-encoded with the keys of
// every object sorted by byte order, numbers written exactly as the first
// encoding wrote them, no insignificant whitespace, and no HTML escaping.
// Sorting makes the form independent of Go struct field order, so reordering
// the fields of an API type cannot silently change every digest. Values the
// API server defaults (e.g. checksumType) are part of the object the manager
// reads, so they are covered; a future DEFAULTED field of ImageSource would
// change every digest once, and must be left out here or carry no default.
//
// A golden vector (TestSourceDigestGolden) pins the form. src is not modified.
func SourceDigest(src infravirtrigaudiov1beta1.ImageSource) (string, error) {
	canonical, err := canonicalSource(src)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return SourceDigestPrefix + hex.EncodeToString(sum[:]), nil
}

// canonicalSource returns the canonical form SourceDigest hashes.
func canonicalSource(src infravirtrigaudiov1beta1.ImageSource) ([]byte, error) {
	// Clear the transport-only fields (ADR-0009 D2, Q7): how or by whom the
	// bytes are fetched, not which bytes are expected or where they land. See
	// SourceDigest for the list and the reasons.
	source := src.DeepCopy()
	if source.HTTP != nil {
		source.HTTP.URL = stripURLUserinfo(source.HTTP.URL)
		source.HTTP.Timeout = nil
		source.HTTP.Headers = nil
		source.HTTP.Authentication = nil
	}
	if source.Registry != nil {
		source.Registry.PullSecretRef = nil
	}
	if source.VSphere != nil {
		source.VSphere.OVAURL = stripURLUserinfo(source.VSphere.OVAURL)
		source.VSphere.ProviderRef = nil
	}
	if source.Libvirt != nil {
		source.Libvirt.URL = stripURLUserinfo(source.Libvirt.URL)
	}
	typed, err := json.Marshal(sourceDigestEnvelope{Version: SourceDigestVersion, Source: *source})
	if err != nil {
		return nil, fmt.Errorf("encode VMImage spec.source for its digest: %w", err)
	}
	return canonicalJSON(typed)
}

// stripURLUserinfo returns raw without the userinfo of its authority: for
// "scheme://user:password@host/path?query#fragment" it returns
// "scheme://host/path?query#fragment". The authority is what follows "://"
// up to the first '/', '?' or '#', and the userinfo is everything in it up to
// its LAST '@' (as net/url splits it). A string without "://" or without an
// '@' in its authority is returned unchanged. It never fails, so an
// unparseable URL still has a digest.
func stripURLUserinfo(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return raw
	}
	authority, tail := rest, ""
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		authority, tail = rest[:end], rest[end:]
	}
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return raw
	}
	return scheme + "://" + authority[at+1:] + tail
}

// canonicalJSON re-encodes the JSON document raw with the keys of every
// object sorted (encoding/json sorts map keys), numbers kept verbatim
// (json.Number), no insignificant whitespace and no HTML escaping.
func canonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode VMImage spec.source for its digest: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("canonicalize VMImage spec.source for its digest: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// ValidateSourceDigest returns an error unless digest has the syntax of a
// source digest (SourceDigestPattern): "sha256:" and 64 lowercase hex digits.
// A provider checks only this syntax; it never recomputes the digest.
func ValidateSourceDigest(digest string) error {
	hexPart, ok := strings.CutPrefix(digest, SourceDigestPrefix)
	if !ok || len(hexPart) != sourceDigestHexLen || !isLowerHex(hexPart) {
		return fmt.Errorf("source digest %q is not %q followed by %d lowercase hex digits",
			digest, SourceDigestPrefix, sourceDigestHexLen)
	}
	return nil
}

// isLowerHex reports whether s consists only of lowercase hex digits.
func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
