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

package inventory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
)

// Collection-side bounds on manifests.
const (
	// MaxManifestBytes bounds one manifest file.
	MaxManifestBytes = 16 << 10
	// MaxManifestEntries bounds the entries of a manifest directory.
	MaxManifestEntries = 64
	// manifestSchemaVersion is the only manifest schema.
	manifestSchemaVersion = 1
	// manifestSuffix ends the name of a manifest file.
	manifestSuffix = ".json"
	// digestPrefix starts every digest a manifest declares.
	digestPrefix = "sha256:"
)

// digestRE is the one digest form a manifest may declare.
var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// declaration is what a valid manifest declares.
type declaration struct {
	component      string
	version        string
	source         string
	artifactName   string
	artifactDigest []byte
	fileDigests    map[string][]byte
}

// manifestDoc is the JSON shape of a manifest.
type manifestDoc struct {
	SchemaVersion int    `json:"schemaVersion"`
	Component     string `json:"component"`
	Version       string `json:"version"`
	Source        string `json:"source"`
	Artifact      *struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	} `json:"artifact"`
	Files map[string]string `json:"files"`
}

// parsed is the outcome of reading one manifest.
type parsed struct {
	// base is the manifest file's base name.
	base string
	// component is the catalog component the manifest names, when it names
	// one; empty otherwise.
	component string
	// reason is why the manifest cannot be used; empty when it can.
	reason string
	decl   declaration
}

// hashedName names a manifest that names no catalog component: the prefix
// and the first 16 hex digits of the SHA-256 of its base name. This is
// charset safety and stability, not confidentiality.
func hashedName(base string) string {
	sum := sha256.Sum256([]byte(base))
	return invalidManifestPrefix + hex.EncodeToString(sum[:8])
}

// parseManifest parses the content of one manifest against the catalog. The
// input is at most MaxManifestBytes; unknown fields, trailing data and any
// field outside its rule make the manifest invalid.
func parseManifest(data []byte, catalog map[string]*CatalogEntry) parsed {
	if len(data) > MaxManifestBytes {
		return parsed{reason: ReasonTooLarge}
	}
	var doc manifestDoc
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return parsed{reason: ReasonParseError}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return parsed{reason: ReasonParseError}
	}
	if doc.SchemaVersion != manifestSchemaVersion {
		return parsed{reason: ReasonParseError}
	}
	if !componentNameRE.MatchString(doc.Component) {
		return parsed{reason: ReasonInvalidName}
	}
	entry := catalog[doc.Component]
	if entry == nil {
		return parsed{reason: ReasonUnknownComponent}
	}
	p := parsed{component: doc.Component}
	p.decl.component = doc.Component
	fail := func(reason string) parsed {
		p.reason = reason
		p.decl = declaration{}
		return p
	}
	switch {
	case len(doc.Version) > 64:
		return fail(ReasonVersionTooLong)
	case !versionRE.MatchString(doc.Version):
		return fail(ReasonParseError)
	}
	if doc.Source != "" && !sourceRE.MatchString(doc.Source) {
		return fail(ReasonParseError)
	}
	p.decl.version, p.decl.source = doc.Version, doc.Source
	if doc.Artifact != nil {
		if !fileNameRE.MatchString(doc.Artifact.Name) {
			return fail(ReasonInvalidName)
		}
		p.decl.artifactName = doc.Artifact.Name
		if doc.Artifact.SHA256 != "" {
			d, ok := parseDigest(doc.Artifact.SHA256)
			if !ok {
				return fail(ReasonUnsupportedDigest)
			}
			p.decl.artifactDigest = d
		}
	}
	if len(doc.Files) > 0 {
		p.decl.fileDigests = map[string][]byte{}
	}
	for name, digest := range doc.Files {
		if !catalogHasFile(entry, name) {
			return fail(ReasonParseError)
		}
		d, ok := parseDigest(digest)
		if !ok {
			return fail(ReasonUnsupportedDigest)
		}
		p.decl.fileDigests[name] = d
	}
	return p
}

// parseDigest decodes "sha256:<64 lower-case hex digits>".
func parseDigest(s string) ([]byte, bool) {
	if !digestRE.MatchString(s) {
		return nil, false
	}
	b, err := hex.DecodeString(strings.TrimPrefix(s, digestPrefix))
	if err != nil || len(b) != sha256.Size {
		return nil, false
	}
	return b, true
}

// catalogHasFile reports whether the entry has a file called name.
func catalogHasFile(e *CatalogEntry, name string) bool {
	for _, f := range e.Files {
		if f.Name == name {
			return true
		}
	}
	return false
}
