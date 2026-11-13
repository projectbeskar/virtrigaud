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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Collection-side bounds (not part of the report).
const (
	// MaxHashFileBytes bounds how much of one file is hashed.
	MaxHashFileBytes = 1 << 30
	// MaxHashTotalBytes bounds how much is hashed in one collection.
	MaxHashTotalBytes = 4 << 30
	// maxExtensionEntries bounds the entries read from one extension
	// directory.
	maxExtensionEntries = 256
	// imageSuffix ends the name of a system extension image.
	imageSuffix = ".raw"
	// hashChunk is the read size of the hasher; the context is checked
	// between chunks.
	hashChunk = 256 << 10
	// invalidExtensionPrefix names an extension whose name breaks the name
	// rule.
	invalidExtensionPrefix = "invalid-"
)

// collected is the content of one finished collection.
type collected struct {
	components []*providerv1.HostComponent
	features   []*providerv1.HostFeature
	extensions []*providerv1.HostExtension
	tier       providerv1.ProcessIdentityTier
}

// abort ends a collection with a state that lists nothing.
type abort struct {
	state  providerv1.InventoryState
	reason string
	cause  error
}

func (a *abort) Error() string {
	if a.cause != nil {
		return fmt.Sprintf("collection ended %v/%s: %v", a.state, a.reason, a.cause)
	}
	return fmt.Sprintf("collection ended %v/%s", a.state, a.reason)
}

func unreadable(reason string, cause error) *abort {
	return &abort{state: providerv1.InventoryState_INVENTORY_STATE_UNREADABLE, reason: reason, cause: cause}
}

func overLimit(reason string) *abort {
	return &abort{state: providerv1.InventoryState_INVENTORY_STATE_OVER_LIMIT, reason: reason}
}

// env is what one collection reads.
type env struct {
	fs           FS
	host         Host
	layout       Layout
	catalog      []CatalogEntry
	byName       map[string]*CatalogEntry
	agentVersion string
	maxBytes     int
	probes       []probe
	// hashFile and hashTotal are the hashing limits (MaxHashFileBytes and
	// MaxHashTotalBytes; tests lower them).
	hashFile, hashTotal int64
}

// claim is one manifest that was read.
type claim struct {
	origin providerv1.ManifestOrigin
	p      parsed
}

// extension is one system extension found in an extension directory.
type extension struct {
	name  string // as listed in the directory
	key   string // the extension's name as the system extension tool takes it: without ".raw"
	path  string
	image bool // a regular file to hash; everything else is listed without a digest
}

// hasher hashes files within the per-file and per-collection limits.
type hasher struct {
	fs        FS
	perFile   int64
	remaining int64
}

// hashResult is the outcome of hashing one file.
type hashResult int

const (
	hashOK hashResult = iota
	hashLimit
	hashAbsent
)

// sum hashes the regular file at p (following a symbolic link when follow is
// set). A missing file is hashAbsent; a file above a limit is hashLimit and
// nothing of it is counted.
func (h *hasher) sum(ctx context.Context, p string, follow bool) ([]byte, hashResult, error) {
	f, err := h.fs.Open(p, follow)
	if err != nil {
		if isNotExist(err) {
			return nil, hashAbsent, nil
		}
		return nil, hashOK, err
	}
	defer func() { _ = f.Close() }()
	if f.Size() > h.perFile || f.Size() > h.remaining {
		return nil, hashLimit, nil
	}
	limit := min(h.perFile, h.remaining)
	d := sha256.New()
	buf := make([]byte, hashChunk)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			h.remaining -= n
			return nil, hashOK, err
		}
		k, rerr := f.Read(buf)
		n += int64(k)
		if n > limit {
			h.remaining -= n // what was read counts, whatever came of it
			return nil, hashLimit, nil
		}
		_, _ = d.Write(buf[:k])
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			h.remaining -= n
			return nil, hashOK, rerr
		}
	}
	h.remaining -= n
	return d.Sum(nil), hashOK, nil
}

// collect makes one collection. It returns an *abort for a state that lists
// nothing, and the context's error when ctx ends.
func (e *env) collect(ctx context.Context) (*collected, error) {
	claims, err := e.readManifests(ctx)
	if err != nil {
		return nil, err
	}
	exts, err := e.readExtensions(ctx)
	if err != nil {
		return nil, err
	}
	groups := map[string][]claim{}
	for _, c := range claims {
		groups[keyOf(c)] = append(groups[keyOf(c)], c)
	}
	h := &hasher{fs: e.fs, perFile: e.hashFile, remaining: e.hashTotal}
	declared := map[string]string{}
	var comps []*providerv1.HostComponent
	for i := range e.catalog {
		entry := &e.catalog[i]
		g, ok := groups[entry.Name]
		delete(groups, entry.Name)
		var c *providerv1.HostComponent
		if ok {
			c, err = e.declaredComponent(ctx, h, entry, g, exts, declared)
		} else {
			c, err = e.undeclaredComponent(ctx, h, entry)
		}
		if err != nil {
			return nil, err
		}
		if c != nil {
			comps = append(comps, c)
		}
	}
	for key, g := range groups {
		comps = append(comps, resolveInvalid(key, g))
	}
	e.markOverlays(ctx, comps, exts, declared)
	unexpected, err := e.unexpectedExtensions(ctx, h, exts, declared)
	if err != nil {
		return nil, err
	}
	features := e.runProbes(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := &collected{components: comps, features: features, extensions: unexpected, tier: e.tier(features)}
	return out, e.finish(out)
}

// keyOf is the name of the entry a claim belongs to.
func keyOf(c claim) string {
	if c.p.component != "" {
		return c.p.component
	}
	return hashedName(c.p.base)
}

// finish sorts the report and applies the bounds; it returns an *abort when
// one is exceeded. Nothing is truncated.
func (e *env) finish(c *collected) error {
	sort.Slice(c.components, func(i, j int) bool { return c.components[i].Name < c.components[j].Name })
	sort.Slice(c.features, func(i, j int) bool { return c.features[i].Name < c.features[j].Name })
	sort.Slice(c.extensions, func(i, j int) bool { return c.extensions[i].Name < c.extensions[j].Name })
	files := 0
	for _, comp := range c.components {
		if len(comp.Files) > MaxFilesPerComponent {
			return overLimit(ReasonTooManyFiles)
		}
		files += len(comp.Files)
	}
	switch {
	case len(c.components) > MaxComponents:
		return overLimit(ReasonTooManyComponents)
	case files > MaxFiles:
		return overLimit(ReasonTooManyFiles)
	case len(c.features) > MaxFeatures:
		return overLimit(ReasonTooManyFeatures)
	case len(c.extensions) > MaxExtensions:
		return overLimit(ReasonTooManyExtensions)
	}
	inv := &providerv1.HostInventory{
		State: providerv1.InventoryState_INVENTORY_STATE_COMPLETE, Components: c.components, Features: c.features,
		ProcessIdentityTier: c.tier, UnexpectedExtensions: c.extensions,
	}
	bind(inv, SupportedVersion)
	if proto.Size(inv) > e.maxBytes {
		return overLimit(ReasonInventoryTooLarge)
	}
	if err := Validate(inv, SupportedVersion); err != nil {
		if errors.Is(err, ErrTooLarge) {
			return overLimit(ReasonInventoryTooLarge)
		}
		return unreadable(ReasonProbeFailed, err)
	}
	return nil
}

// readManifests reads both manifest directories.
func (e *env) readManifests(ctx context.Context) ([]claim, error) {
	var claims []claim
	for _, d := range []struct {
		dir    string
		origin providerv1.ManifestOrigin
	}{
		{e.layout.ReadOnlyDir, providerv1.ManifestOrigin_MANIFEST_ORIGIN_READ_ONLY},
		{e.layout.WritableDir, providerv1.ManifestOrigin_MANIFEST_ORIGIN_WRITABLE},
	} {
		entries, more, err := e.fs.ReadDir(d.dir, MaxManifestEntries)
		if err != nil {
			if isNotExist(err) {
				continue
			}
			return nil, unreadable(ReasonManifestDirUnreadable, err)
		}
		if more {
			return nil, overLimit(ReasonTooManyComponents)
		}
		for _, ent := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !strings.HasSuffix(ent.Name, manifestSuffix) {
				continue
			}
			c := claim{origin: d.origin, p: parsed{base: ent.Name}}
			if ent.Mode&fs.ModeType != 0 {
				c.p.reason = ReasonParseError
				claims = append(claims, c)
				continue
			}
			f, err := e.fs.Open(path.Join(d.dir, ent.Name), false)
			switch {
			case errors.Is(err, ErrNotRegular):
				c.p.reason = ReasonParseError
			case err != nil:
				return nil, unreadable(ReasonIOError, err)
			case f.Size() > MaxManifestBytes:
				_ = f.Close()
				c.p.reason = ReasonTooLarge
			default:
				data, tooLarge, rerr := readLimited(f, MaxManifestBytes)
				_ = f.Close()
				switch {
				case rerr != nil:
					return nil, unreadable(ReasonIOError, rerr)
				case tooLarge:
					c.p.reason = ReasonTooLarge
				default:
					p := parseManifest(data, e.byName)
					p.base = ent.Name
					c.p = p
				}
			}
			claims = append(claims, c)
		}
	}
	return claims, nil
}

// extensionKey is the name the system extension tool gives an extension: the
// entry's name without the ".raw" suffix, so "x" and "x.raw" are one extension.
func extensionKey(name string) string { return strings.TrimSuffix(name, imageSuffix) }

// masked reports whether a symbolic link entry masks an extension: the tool
// treats a link to /dev/null as not merged.
func (e *env) masked(p string) bool {
	target, err := e.fs.Readlink(p)
	return err == nil && target == "/dev/null"
}

// classify makes the extension of one directory entry, or reports that the
// entry is not an extension (a hidden entry, or a masked one). Nothing else is
// left out: an entry that cannot be told apart from a plain extension (a
// dangling link, a device node, a socket, a versioned directory, a file
// without the image suffix) is listed as a directory extension, which carries
// no digest and is valid under the field rules. The report errs towards
// showing a stray entry rather than hiding it; one bad entry never turns the
// whole snapshot into an error. Hidden entries (a leading dot) are skipped:
// the system extension tool's image discovery ignores them, so they are never
// merged (stated from the tool's source, not checked against a local manual).
func (e *env) classify(dir string, ent DirEntry) (extension, bool) {
	if ent.Name == "" || strings.HasPrefix(ent.Name, ".") {
		return extension{}, false
	}
	x := extension{name: ent.Name, key: extensionKey(ent.Name), path: path.Join(dir, ent.Name)}
	isImage := strings.HasSuffix(ent.Name, imageSuffix)
	switch {
	case ent.Mode&fs.ModeSymlink != 0:
		if e.masked(x.path) {
			return extension{}, false
		}
		mode, _, err := e.fs.Stat(x.path)
		x.image = err == nil && mode.IsRegular() && isImage
	case ent.Mode.IsRegular():
		x.image = isImage
	}
	return x, true
}

// readExtensions indexes the system extensions of every extension directory;
// an earlier directory wins over a later one for the same name.
func (e *env) readExtensions(ctx context.Context) (map[string]extension, error) {
	out := map[string]extension{}
	for _, dir := range e.layout.ExtensionDirs {
		inDir := map[string]bool{}
		entries, more, err := e.fs.ReadDir(dir, maxExtensionEntries)
		if err != nil {
			if isNotExist(err) {
				continue
			}
			return nil, unreadable(ReasonIOError, err)
		}
		if more {
			return nil, overLimit(ReasonTooManyExtensions)
		}
		for _, ent := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			x, ok := e.classify(dir, ent)
			if !ok {
				continue
			}
			if inDir[x.key] {
				// "x" and "x.raw" in one directory: which one the system
				// extension tool takes is not defined, and neither may be
				// dropped.
				return nil, unreadable(ReasonProbeFailed, errors.New("two entries of one extension directory name the same extension"))
			}
			inDir[x.key] = true
			if _, seen := out[x.key]; !seen {
				out[x.key] = x
			}
		}
	}
	return out, nil
}

// resolveInvalid makes the MANIFEST_INVALID component of a group of claims
// that cannot be used: a named group with several claims is a duplicate
// declaration; a hash-named group (the same base name in both directories) is
// one entry. The origin is the read-only directory when a claim comes from it.
func resolveInvalid(key string, g []claim) *providerv1.HostComponent {
	origin := g[0].origin
	for _, c := range g {
		if c.origin == providerv1.ManifestOrigin_MANIFEST_ORIGIN_READ_ONLY {
			origin = c.origin
			break
		}
	}
	reason := g[0].p.reason
	if len(g) > 1 && !strings.HasPrefix(key, invalidManifestPrefix) {
		reason = ReasonDuplicateDeclaration
	}
	if reason == "" {
		reason = ReasonParseError
	}
	return &providerv1.HostComponent{
		Name: key, State: providerv1.ComponentState_COMPONENT_STATE_MANIFEST_INVALID, StateReason: reason, ManifestOrigin: origin,
	}
}

// fileSums is what hashing a component's catalog files found.
type fileSums struct {
	files   []*providerv1.ComponentFile
	digests map[string][]byte
	missing bool // a non-optional file is absent
	present bool // some file is present
	limit   bool // some file exceeded a hashing limit
}

// inspectFiles hashes the catalog files of entry.
func (e *env) inspectFiles(ctx context.Context, h *hasher, entry *CatalogEntry) (fileSums, error) {
	s := fileSums{digests: map[string][]byte{}}
	for _, f := range entry.Files {
		p := f.Path
		if f.Self {
			p = e.layout.AgentExecutable
		}
		if p == "" {
			if !f.Optional {
				s.missing = true
			}
			continue
		}
		sum, res, err := h.sum(ctx, p, true)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return s, ctx.Err()
			}
			return s, unreadable(ReasonIOError, err)
		case res == hashAbsent:
			if !f.Optional {
				s.missing = true
			}
		case res == hashLimit:
			s.present, s.limit = true, true
		default:
			s.present = true
			s.digests[f.Name] = sum
			s.files = append(s.files, &providerv1.ComponentFile{Name: f.Name, Sha256: sum})
		}
	}
	sort.Slice(s.files, func(i, j int) bool { return s.files[i].Name < s.files[j].Name })
	return s, nil
}

// observedVersion reads the installed version of entry as data, from the
// first source that has one: the agent's own build, an extension-release
// file, an ELF package note, a package database. Nothing is executed.
func (e *env) observedVersion(entry *CatalogEntry, artifactName string) string {
	if entry.SelfVersion {
		if versionRE.MatchString(e.agentVersion) {
			return e.agentVersion
		}
		return ""
	}
	if artifactName != "" {
		p := path.Join(e.layout.ExtensionReleaseDir, "extension-release."+strings.TrimSuffix(artifactName, imageSuffix))
		if f, err := e.fs.Open(p, false); err == nil {
			data, tooLarge, rerr := readLimited(f, maxExtensionReleaseBytes)
			_ = f.Close()
			if rerr == nil && !tooLarge {
				if v, ok := extensionReleaseVersion(data); ok {
					return v
				}
			}
		}
	}
	if entry.ELFNoteFile != "" {
		for _, f := range entry.Files {
			if f.Name != entry.ELFNoteFile || f.Path == "" {
				continue
			}
			if file, err := e.fs.Open(f.Path, true); err == nil {
				v, ok := elfPackageVersion(file, file.Size())
				_ = file.Close()
				if ok {
					return v
				}
			}
		}
	}
	if entry.Package != "" {
		for _, db := range e.layout.PackageDBs {
			f, err := e.fs.Open(db, false)
			if err != nil {
				continue
			}
			v, ok := packageDBVersion(f, entry.Package)
			_ = f.Close()
			if ok {
				return v
			}
		}
	}
	return ""
}

// declaredComponent evaluates a component some manifest claims.
func (e *env) declaredComponent(ctx context.Context, h *hasher, entry *CatalogEntry, g []claim,
	exts map[string]extension, declared map[string]string) (*providerv1.HostComponent, error) {
	if len(g) > 1 || g[0].p.reason != "" {
		return resolveInvalid(entry.Name, g), nil
	}
	decl := g[0].p.decl
	c := &providerv1.HostComponent{
		Name: entry.Name, ManifestOrigin: g[0].origin, DeclaredVersion: decl.version, Source: decl.source,
	}
	var ext *extension
	if decl.artifactName != "" {
		key := extensionKey(decl.artifactName)
		declared[key] = entry.Name
		if x, ok := exts[key]; ok {
			ext = &x
		}
	}
	sums, err := e.inspectFiles(ctx, h, entry)
	if err != nil {
		return nil, err
	}
	var artifact []byte
	artifactLimit := false
	if ext != nil && ext.image {
		sum, res, herr := h.sum(ctx, ext.path, true)
		switch {
		case herr != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, unreadable(ReasonIOError, herr)
		case res == hashLimit:
			artifactLimit = true
		case res == hashOK:
			artifact = sum
		}
	}
	if (decl.artifactName != "" && ext == nil) || sums.missing {
		c.State, c.StateReason = providerv1.ComponentState_COMPONENT_STATE_MISSING, ReasonNotInstalled
		return c, nil
	}
	c.ObservedVersion = e.observedVersion(entry, decl.artifactName)
	c.ArtifactSha256, c.Files = artifact, sums.files
	// The first defect, in the order of the states.
	rng, err := parseRange(entry.Range)
	if err != nil {
		return nil, unreadable(ReasonProbeFailed, err)
	}
	_, numeric := parseNumeric(c.ObservedVersion)
	switch {
	case c.ObservedVersion == "" || !numeric:
		c.State, c.StateReason, c.RequiredRange = providerv1.ComponentState_COMPONENT_STATE_INCOMPATIBLE, ReasonVersionUnreadable, entry.Range
	case !rng.contains(c.ObservedVersion):
		c.State, c.StateReason, c.RequiredRange = providerv1.ComponentState_COMPONENT_STATE_INCOMPATIBLE, ReasonVersionOutOfRange, entry.Range
	default:
		if reason := unhealthyReason(decl, artifact, sums, artifactLimit); reason != "" {
			c.State, c.StateReason = providerv1.ComponentState_COMPONENT_STATE_UNHEALTHY, reason
		} else {
			c.State = providerv1.ComponentState_COMPONENT_STATE_READY
		}
	}
	return c, nil
}

// unhealthyReason returns the first UNHEALTHY reason that applies, in the
// order of the closed set (the overlay reason is applied by markOverlays),
// or "".
func unhealthyReason(decl declaration, artifact []byte, sums fileSums, artifactLimit bool) string {
	if decl.artifactDigest != nil && !artifactLimit && !equalDigest(decl.artifactDigest, artifact) {
		return ReasonArtifactDigestMismatch
	}
	for name, want := range decl.fileDigests {
		if got, ok := sums.digests[name]; ok && !equalDigest(got, want) {
			return ReasonFileDigestMismatch
		}
	}
	if artifactLimit || sums.limit {
		return ReasonDigestLimitExceeded
	}
	return ""
}

// undeclaredComponent evaluates a catalog component no manifest declares:
// UNMANIFESTED when present, MISSING (not declared) when the catalog requires
// it, nothing otherwise.
func (e *env) undeclaredComponent(ctx context.Context, h *hasher, entry *CatalogEntry) (*providerv1.HostComponent, error) {
	sums, err := e.inspectFiles(ctx, h, entry)
	if err != nil {
		return nil, err
	}
	none := providerv1.ManifestOrigin_MANIFEST_ORIGIN_NONE
	if sums.present {
		return &providerv1.HostComponent{
			Name: entry.Name, State: providerv1.ComponentState_COMPONENT_STATE_UNMANIFESTED, StateReason: ReasonPresentUndeclared,
			ManifestOrigin: none, ObservedVersion: e.observedVersion(entry, ""), Files: sums.files,
		}, nil
	}
	if entry.Required {
		return &providerv1.HostComponent{
			Name: entry.Name, State: providerv1.ComponentState_COMPONENT_STATE_MISSING, StateReason: ReasonNotDeclared, ManifestOrigin: none,
		}, nil
	}
	return nil, nil
}

// markOverlays makes a manifested component UNHEALTHY (unexpected_extension)
// when an undeclared directory extension holds one of its files; that reason
// comes first in the UNHEALTHY set, so it replaces another. A component in an
// earlier state of the precedence keeps its state.
func (e *env) markOverlays(ctx context.Context, comps []*providerv1.HostComponent, exts map[string]extension, declared map[string]string) {
	for _, c := range comps {
		if c.State != providerv1.ComponentState_COMPONENT_STATE_READY && c.State != providerv1.ComponentState_COMPONENT_STATE_UNHEALTHY {
			continue
		}
		entry := e.byName[c.Name]
		if entry == nil {
			continue
		}
		for _, x := range exts {
			// An extension the component declares itself is not an overlay of it;
			// one another component declares is.
			if x.image || declared[x.key] == c.Name || ctx.Err() != nil {
				continue
			}
			for _, rel := range entry.OverlayPaths {
				if _, _, err := e.fs.Lstat(path.Join(x.path, rel)); !isNotExist(err) {
					// Present, or unreadable: an overlay that cannot be ruled out is one.
					c.State, c.StateReason = providerv1.ComponentState_COMPONENT_STATE_UNHEALTHY, ReasonUnexpectedExtension
				}
			}
		}
	}
}

// unexpectedExtensions lists the extensions no manifest declares. An image
// that cannot be hashed (a limit, a read error) is listed without a digest, as
// a directory extension, instead of being hidden.
func (e *env) unexpectedExtensions(ctx context.Context, h *hasher, exts map[string]extension, declared map[string]string) ([]*providerv1.HostExtension, error) {
	keys := make([]string, 0, len(exts))
	for k := range exts {
		if _, ok := declared[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	seen := map[string]bool{}
	var out []*providerv1.HostExtension
	for _, k := range keys {
		x := exts[k]
		display := x.name
		if !fileNameRE.MatchString(display) {
			sum := sha256.Sum256([]byte(display))
			display = invalidExtensionPrefix + hex.EncodeToString(sum[:8])
		}
		if seen[display] {
			// A literal name equal to a hashed one: two entries cannot share a
			// name, and neither may be dropped.
			return nil, unreadable(ReasonProbeFailed, errors.New("two extensions have the same reported name"))
		}
		seen[display] = true
		he := &providerv1.HostExtension{Name: display, Kind: providerv1.ExtensionKind_EXTENSION_KIND_DIRECTORY}
		if x.image {
			sum, res, err := h.sum(ctx, x.path, true)
			if err != nil && ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err == nil && res == hashOK {
				he.Kind, he.ImageSha256 = providerv1.ExtensionKind_EXTENSION_KIND_UNDECLARED_IMAGE, sum
			}
		}
		out = append(out, he)
	}
	return out, nil
}
