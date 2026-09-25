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
// What it covers: every field of spec.source (URLs and paths, the expected
// checksum and its algorithm, formats, and the location fields such as
// libvirt storagePool and Proxmox storage and node), except the transport-only
// fields of source.http — timeout, headers and authentication — which change
// how the bytes are fetched, not which bytes are expected (rotating a token
// must not orphan multi-GB artifacts). Nothing outside spec.source is covered:
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
	source := src.DeepCopy()
	if source.HTTP != nil {
		// Transport-only (ADR-0009 D2, Q7): how the bytes are fetched, not
		// which bytes are expected.
		source.HTTP.Timeout = nil
		source.HTTP.Headers = nil
		source.HTTP.Authentication = nil
	}
	typed, err := json.Marshal(sourceDigestEnvelope{Version: SourceDigestVersion, Source: *source})
	if err != nil {
		return nil, fmt.Errorf("encode VMImage spec.source for its digest: %w", err)
	}
	return canonicalJSON(typed)
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
