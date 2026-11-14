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

// Package hostinventory is the manager's component allow-list for host
// inventories: the file format, its loading (digest pin and an optional
// detached signature, verified offline) and the comparison of a reported host
// component with it.
//
// The allow-list is read once, when the manager starts, from a file whose
// SHA-256 is pinned by a manager flag. A change needs a new file and a new
// flag value, which is a change of the manager Deployment and a rollout.
//
// It imports no Kubernetes package.
package hostinventory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// FormatVersion is the only allow-list format version.
const FormatVersion = 1

// Bounds of an allow-list file.
const (
	// MaxFileBytes bounds the file.
	MaxFileBytes = 1 << 20
	// MaxEntries bounds the (component, version) entries.
	MaxEntries = 512
	// MaxAlternatives bounds the alternatives of one entry.
	MaxAlternatives = 16
	// MaxFiles bounds the files of one alternative: the most a host
	// component may report.
	MaxFiles = 8
	// MaxRequired bounds the required components.
	MaxRequired = 32
)

// NoArtifact is the artifactDigest value of an alternative whose component
// is delivered without an artifact.
const NoArtifact = "none"

// digestPrefix starts every digest in the file.
const digestPrefix = "sha256:"

// Errors of loading an allow-list.
var (
	// ErrInvalid is wrapped when the file is not a valid allow-list.
	ErrInvalid = errors.New("invalid component allow-list")
	// ErrDigestMismatch is wrapped when the file's SHA-256 is not the pinned
	// one, or no digest is pinned.
	ErrDigestMismatch = errors.New("the component allow-list does not have the pinned digest")
	// ErrUnverified is wrapped when a signature is required and is missing,
	// malformed, or not made by a configured signer.
	ErrUnverified = errors.New("the component allow-list signature does not verify")
)

// Result is the comparison of a reported host component with the allow-list.
type Result string

// Results of Match.
const (
	// Allowed: the artifact digest and the complete file set match an
	// alternative of the component's entry for its version.
	Allowed Result = "Allowed"
	// NotAllowed: an entry exists for the component and version, but no
	// alternative matches.
	NotAllowed Result = "NotAllowed"
	// NotListed: no entry exists for the component and version.
	NotListed Result = "NotListed"
	// NotChecked: the component reports neither an artifact digest nor files,
	// so it cannot be compared.
	NotChecked Result = "NotChecked"
)

// Field shapes, as in the host inventory.
var (
	componentNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	fileNameRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	versionRE       = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+~_-]{0,63}$`)
	hexDigestRE     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// fileFormat is the JSON allow-list file.
type fileFormat struct {
	Version    int              `json:"version"`
	Required   []string         `json:"required,omitempty"`
	Components []componentEntry `json:"components"`
}

// componentEntry is one (component, version) entry.
type componentEntry struct {
	Name         string        `json:"name"`
	Version      string        `json:"version"`
	Alternatives []alternative `json:"alternatives"`
}

// alternative is one accepted build of a component version.
type alternative struct {
	ArtifactDigest string      `json:"artifactDigest"`
	Files          []fileEntry `json:"files"`
}

// fileEntry is one file of an alternative.
type fileEntry struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// entryKey names one entry.
type entryKey struct{ name, version string }

// match is one parsed alternative.
type match struct {
	artifact []byte
	files    map[string][]byte
}

// AllowList is a parsed component allow-list.
type AllowList struct {
	sha      [sha256.Size]byte
	required []string
	entries  map[entryKey][]match
}

// SHA256 returns the SHA-256 of the allow-list file, as 64 lowercase
// hexadecimal characters.
func (a *AllowList) SHA256() string { return hex.EncodeToString(a.sha[:]) }

// Required returns the components the allow-list requires to be reported
// ready, sorted.
func (a *AllowList) Required() []string { return slices.Clone(a.required) }

// invalidf wraps ErrInvalid.
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// parseDigest decodes "sha256:<64 hex>".
func parseDigest(s string) ([]byte, bool) {
	if !hexDigestRE.MatchString(s) {
		return nil, false
	}
	d, err := hex.DecodeString(strings.TrimPrefix(s, digestPrefix))
	return d, err == nil
}

// Parse parses an allow-list file: JSON with no unknown field, format
// version 1, every name, version and digest in its shape, no repeated entry,
// alternative file name or required component, and within the bounds.
func Parse(data []byte) (*AllowList, error) {
	if len(data) > MaxFileBytes {
		return nil, invalidf("the file is larger than %d bytes", MaxFileBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f fileFormat
	if err := dec.Decode(&f); err != nil {
		return nil, invalidf("decode: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, invalidf("trailing data after the allow-list")
	}
	if f.Version != FormatVersion {
		return nil, invalidf("format version %d is not %d", f.Version, FormatVersion)
	}
	if len(f.Components) > MaxEntries || len(f.Required) > MaxRequired {
		return nil, invalidf("more than %d entries or %d required components", MaxEntries, MaxRequired)
	}
	a := &AllowList{sha: sha256.Sum256(data), entries: make(map[entryKey][]match, len(f.Components))}
	for _, r := range f.Required {
		if !componentNameRE.MatchString(r) || slices.Contains(a.required, r) {
			return nil, invalidf("required component %q is malformed or repeated", r)
		}
		a.required = append(a.required, r)
	}
	slices.Sort(a.required)
	for _, e := range f.Components {
		k := entryKey{e.Name, e.Version}
		switch {
		case !componentNameRE.MatchString(e.Name) || !versionRE.MatchString(e.Version):
			return nil, invalidf("entry %q version %q: malformed name or version", e.Name, e.Version)
		case a.entries[k] != nil:
			return nil, invalidf("entry %q version %q is repeated", e.Name, e.Version)
		case len(e.Alternatives) == 0 || len(e.Alternatives) > MaxAlternatives:
			return nil, invalidf("entry %q version %q has %d alternatives", e.Name, e.Version, len(e.Alternatives))
		}
		ms := make([]match, 0, len(e.Alternatives))
		for _, alt := range e.Alternatives {
			m, err := parseAlternative(alt)
			if err != nil {
				return nil, fmt.Errorf("entry %q version %q: %w", e.Name, e.Version, err)
			}
			ms = append(ms, m)
		}
		a.entries[k] = ms
	}
	return a, nil
}

// parseAlternative parses one alternative. It must name at least one digest:
// an alternative with neither an artifact nor files would match nothing a
// checked component reports.
func parseAlternative(alt alternative) (match, error) {
	m := match{files: make(map[string][]byte, len(alt.Files))}
	if alt.ArtifactDigest != NoArtifact {
		d, ok := parseDigest(alt.ArtifactDigest)
		if !ok {
			return match{}, invalidf("artifactDigest is neither %q nor sha256:<64 lowercase hex>", NoArtifact)
		}
		m.artifact = d
	}
	if len(alt.Files) > MaxFiles {
		return match{}, invalidf("%d files", len(alt.Files))
	}
	for _, f := range alt.Files {
		d, ok := parseDigest(f.Digest)
		if !fileNameRE.MatchString(f.Name) || !ok {
			return match{}, invalidf("file %q: malformed name or digest", f.Name)
		}
		if _, dup := m.files[f.Name]; dup {
			return match{}, invalidf("file %q is repeated", f.Name)
		}
		m.files[f.Name] = d
	}
	if m.artifact == nil && len(m.files) == 0 {
		return match{}, invalidf("an alternative without an artifact digest or files")
	}
	return m, nil
}

// Match compares a reported host component with the allow-list: NotChecked
// when it reports neither an artifact digest nor files; NotListed when no
// entry names its name and observed version; Allowed when its artifact digest
// equals an alternative's (both absent when the alternative says none) and
// its file names are exactly the alternative's, each with the same digest;
// NotAllowed otherwise.
func (a *AllowList) Match(c *providerv1.HostComponent) Result {
	if len(c.GetArtifactSha256()) == 0 && len(c.GetFiles()) == 0 {
		return NotChecked
	}
	ms, ok := a.entries[entryKey{c.GetName(), c.GetObservedVersion()}]
	if !ok {
		return NotListed
	}
	for _, m := range ms {
		if matches(m, c) {
			return Allowed
		}
	}
	return NotAllowed
}

// matches reports whether c matches the alternative m exactly.
func matches(m match, c *providerv1.HostComponent) bool {
	if !bytes.Equal(m.artifact, c.GetArtifactSha256()) || len(m.files) != len(c.GetFiles()) {
		return false
	}
	for _, f := range c.GetFiles() {
		d, ok := m.files[f.GetName()]
		if !ok || !bytes.Equal(d, f.GetSha256()) {
			return false
		}
	}
	return true
}
