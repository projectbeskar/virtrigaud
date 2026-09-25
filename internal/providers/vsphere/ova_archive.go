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

package vsphere

import (
	"archive/tar"
	stderrors "errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/vmware/govmomi/ovf"

	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// OVF packages without filesystem access.
//
// govmomi's importer opens every file an OVF references through an
// importer.Archive. Its FileArchive (used for a bare .ovf) resolves each
// <References><File ovf:href> as a path on the local filesystem — absolute
// hrefs as-is, relative ones joined to the staging directory, so "../"
// escapes — and its TapeArchive matches hrefs against tar members with
// path.Match. A tenant-written OVF could therefore make the provider upload
// any file the provider pod can read (its vCenter credentials, its TLS key) as
// a disk or ISO of the template, readable from every VM cloned from it.
//
// Now every href is validated before vCenter sees the descriptor
// (validateOVFFileRefs), and the importer reads the package only through
// ovfPackage, which serves exactly the descriptor and the validated hrefs, by
// exact name, from the staged download — never a file named by the OVF. A
// bare .ovf (a descriptor with no container for its files) can import only a
// package that references no file.
//
// The package is also bounded, because the provider is shared by every tenant
// of its Provider: govmomi reads the whole descriptor into memory (and vCenter
// receives it as one string), so a descriptor larger than maxOVFDescriptorBytes
// is refused from its tar header before a byte of it is read; an OVF may
// reference at most maxOVFFileRefs files; and the members those references
// name are located in one pass over the tar, not one pass per reference.

const (
	// maxOVFHrefBytes bounds the length of an OVF file reference.
	maxOVFHrefBytes = 255
	// maxOVFDescriptorBytes bounds the OVF descriptor. Real descriptors are a
	// few hundred KiB; govmomi's importer reads the whole descriptor into
	// memory (several times over while parsing it), so an unbounded one could
	// exhaust the memory of the provider pod every tenant shares.
	maxOVFDescriptorBytes = 16 << 20
	// maxOVFFileRefs bounds how many files an OVF may reference.
	maxOVFFileRefs = 256
	// ovfHrefForbiddenChars are characters an OVF file reference must not
	// contain: path separators ('/', '\'), the URL-scheme / drive separator
	// (':') and the glob metacharacters path.Match interprets ('*', '?', '[').
	ovfHrefForbiddenChars = `/\:*?[`
	// ovaCurrentDirPrefix is the "./" some tar writers put before root-level
	// member names.
	ovaCurrentDirPrefix = "./"
	// appleDoublePrefix starts the name of a macOS AppleDouble sidecar
	// ("._disk.vmdk"), never a package member.
	appleDoublePrefix = "._"
	// ovfDescriptorExt is the extension of an OVF descriptor.
	ovfDescriptorExt = ".ovf"
	// gnuSparsePAXPrefix starts the PAX records of a GNU sparse file, whose
	// stored bytes are not its contents: such an entry is never a member.
	gnuSparsePAXPrefix = "GNU.sparse."
)

// errUnreadableOVA marks a staged OVA that is not a readable tar archive.
var errUnreadableOVA = stderrors.New("the downloaded OVA is not a readable tar archive")

// ovfHrefError returns why href cannot name a file of an OVF package, or ""
// when it can: a non-empty, plain file name of at most maxOVFHrefBytes bytes —
// no path separator, no "." or "..", no URL scheme, no glob metacharacter and
// no control character.
func ovfHrefError(href string) string {
	switch {
	case href == "":
		return "it is empty"
	case len(href) > maxOVFHrefBytes:
		return fmt.Sprintf("it is longer than %d bytes", maxOVFHrefBytes)
	case href == "." || href == "..":
		return "it names a directory"
	case strings.ContainsAny(href, ovfHrefForbiddenChars):
		return fmt.Sprintf("it contains one of %q: only a plain file name inside the package is allowed", ovfHrefForbiddenChars)
	}
	for _, r := range href {
		if r < 0x20 || r == 0x7f {
			return "it contains a control character"
		}
	}
	return ""
}

// validateOVFFileRefs returns the ovf:href of every <References><File> of env
// after checking each with ovfHrefError, or an InvalidSpec error: for more than
// maxOVFFileRefs references, or for the first reference that fails. It runs
// before the descriptor reaches vCenter, so a package that names a file
// outside itself is never imported. The message quotes no href: the details
// are the caller's to log.
func validateOVFFileRefs(env *ovf.Envelope) ([]string, error) {
	if len(env.References) > maxOVFFileRefs {
		return nil, errors.NewInvalidSpec(
			"ImagePrepare: the OVF references %d files; at most %d are allowed", len(env.References), maxOVFFileRefs)
	}
	hrefs := make([]string, 0, len(env.References))
	for i, f := range env.References {
		if reason := ovfHrefError(f.Href); reason != "" {
			return nil, errors.NewInvalidSpec(
				"ImagePrepare: the OVF's file reference #%d cannot be used: %s", i+1, reason)
		}
		hrefs = append(hrefs, f.Href)
	}
	return hrefs, nil
}

// descriptorTooLargeError is the InvalidSpec of an OVF descriptor larger than
// maxOVFDescriptorBytes.
func descriptorTooLargeError() error {
	return errors.NewInvalidSpec("ImagePrepare: the OVF descriptor is larger than %d MiB",
		maxOVFDescriptorBytes>>20)
}

// ovaMemberLoc is where a package member's bytes lie in the staged download.
type ovaMemberLoc struct {
	// offset is the member's first byte.
	offset int64
	// size is the member's length in bytes.
	size int64
}

// ovfPackage is the importer.Archive an OVA or OVF import reads the package
// through. It serves only members named in its allow list — the descriptor,
// then the validated file references (index, permit) — by exact name, from
// the staged download at path, which the provider created itself. It never
// opens a file named by the OVF, never matches a pattern, and has no remote
// access. Members are located once (newTarPackage, index) and then read at
// their offset.
type ovfPackage struct {
	// path is the staged download (the OVA tar, or the bare .ovf itself).
	path string
	// bare is true when path is a bare .ovf descriptor, not a tar.
	bare bool
	// descriptor is the descriptor's member name.
	descriptor string
	// members are the located members: the descriptor, then the names index
	// found.
	members map[string]ovaMemberLoc
	// allowed are the member names Open serves.
	allowed map[string]bool
	// scans counts the passes over the tar (tests).
	scans int
}

// newTarPackage returns the package of the OVA tar staged at staged. Its
// descriptor is the first package member (ovaMemberName) whose name ends in
// .ovf; one larger than maxOVFDescriptorBytes is refused (InvalidSpec) from
// its tar header, before it is read. A tar that cannot be read wraps
// errUnreadableOVA.
func newTarPackage(staged string) (*ovfPackage, error) {
	pkg := &ovfPackage{path: staged, members: map[string]ovaMemberLoc{}, allowed: map[string]bool{}}
	err := pkg.scan(func(name string, loc ovaMemberLoc) bool {
		if !strings.EqualFold(path.Ext(name), ovfDescriptorExt) {
			return false
		}
		pkg.descriptor = name
		pkg.members[name] = loc
		return true
	})
	if err != nil {
		return nil, err
	}
	if pkg.descriptor == "" {
		return nil, errors.NewInvalidSpec("OVA contains no .ovf descriptor (after skipping macOS sidecar files)")
	}
	if pkg.members[pkg.descriptor].size > maxOVFDescriptorBytes {
		return nil, descriptorTooLargeError()
	}
	pkg.allowed[pkg.descriptor] = true
	return pkg, nil
}

// newBareOVFPackage returns the package of the bare .ovf descriptor staged at
// staged. It serves the descriptor only: a bare .ovf cannot carry its files. A
// descriptor larger than maxOVFDescriptorBytes is refused (InvalidSpec).
func newBareOVFPackage(staged string) (*ovfPackage, error) {
	st, err := os.Stat(filepath.Clean(staged))
	if err != nil {
		return nil, fmt.Errorf("stat staged OVF: %w", err)
	}
	if st.Size() > maxOVFDescriptorBytes {
		return nil, descriptorTooLargeError()
	}
	name := filepath.Base(staged)
	return &ovfPackage{
		path:       staged,
		bare:       true,
		descriptor: name,
		members:    map[string]ovaMemberLoc{name: {offset: 0, size: st.Size()}},
		allowed:    map[string]bool{name: true},
	}, nil
}

// scan passes over the tar once and calls visit with every package member
// (ovaMemberName, not a sparse file) and where its bytes lie, until visit
// returns true. A tar read error wraps errUnreadableOVA.
func (a *ovfPackage) scan(visit func(name string, loc ovaMemberLoc) bool) error {
	a.scans++
	f, err := os.Open(filepath.Clean(a.path))
	if err != nil {
		return fmt.Errorf("open staged OVA: %w", err)
	}
	defer func() { _ = f.Close() }()
	cr := &countingReader{r: f}
	tr := tar.NewReader(cr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// The file is the complete body the source served (a truncated
			// download fails in downloadOVA), so an unreadable archive is a
			// property of the source.
			return fmt.Errorf("%w: %v", errUnreadableOVA, err)
		}
		name := ovaMemberName(h)
		if name == "" || isSparse(h) {
			continue
		}
		// After Next the underlying reader stands at the entry's first data
		// byte: archive/tar reads headers block by block and never ahead.
		if visit(name, ovaMemberLoc{offset: cr.n, size: h.Size}) {
			return nil
		}
	}
}

// isSparse reports whether the tar entry h is a PAX sparse file.
func isSparse(h *tar.Header) bool {
	for k := range h.PAXRecords {
		if strings.HasPrefix(k, gnuSparsePAXPrefix) {
			return true
		}
	}
	return false
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

// Read implements io.Reader.
func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// index locates the members named in names in one pass over the tar (the
// first of equal names wins). A bare .ovf has no members to locate.
func (a *ovfPackage) index(names []string) error {
	if a.bare {
		return nil
	}
	wanted := make(map[string]bool, len(names))
	for _, n := range names {
		if _, ok := a.members[n]; !ok {
			wanted[n] = true
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	return a.scan(func(name string, loc ovaMemberLoc) bool {
		if wanted[name] {
			a.members[name] = loc
			delete(wanted, name)
		}
		return len(wanted) == 0
	})
}

// contains reports whether the package has a located member named exactly
// name (after index). A bare .ovf contains nothing but its descriptor.
func (a *ovfPackage) contains(name string) bool {
	_, ok := a.members[name]
	return ok
}

// permit adds names (validated file references) to the members Open serves.
func (a *ovfPackage) permit(names []string) {
	for _, n := range names {
		a.allowed[n] = true
	}
}

// Open implements importer.Archive. It returns the member named exactly name
// when name is allowed and located, and os.ErrNotExist otherwise.
func (a *ovfPackage) Open(name string) (io.ReadCloser, int64, error) {
	loc, located := a.members[name]
	if !a.allowed[name] || !located {
		return nil, 0, fmt.Errorf("OVF package member %q is not permitted: %w", name, os.ErrNotExist)
	}
	f, err := os.Open(filepath.Clean(a.path))
	if err != nil {
		return nil, 0, fmt.Errorf("open staged OVF package: %w", err)
	}
	if _, err := f.Seek(loc.offset, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("seek to OVF package member %q: %w", name, err)
	}
	return &ovaMember{Reader: io.LimitReader(f, loc.size), f: f}, loc.size, nil
}

// ovaMember is an open package member: reads are limited to the member, Close
// closes the staged file.
type ovaMember struct {
	io.Reader
	f *os.File
}

// Close implements io.Closer.
func (m *ovaMember) Close() error { return m.f.Close() }

// ovaMemberName returns the package member name of the tar entry h, or ""
// when h is not a package member: only a regular file at the root of the
// archive (a leading "./" is ignored) whose name is not a macOS AppleDouble
// sidecar is one. The OVF specification puts every file of an OVA at its root.
func ovaMemberName(h *tar.Header) string {
	if h.Typeflag != tar.TypeReg {
		return ""
	}
	name := strings.TrimPrefix(h.Name, ovaCurrentDirPrefix)
	if name == "" || strings.Contains(name, "/") || strings.HasPrefix(name, appleDoublePrefix) {
		return ""
	}
	return name
}
