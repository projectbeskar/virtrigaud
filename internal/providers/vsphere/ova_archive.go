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

const (
	// maxOVFHrefBytes bounds the length of an OVF file reference.
	maxOVFHrefBytes = 255
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
)

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
// after checking each with ovfHrefError, or an InvalidSpec error for the first
// that fails. It runs before the descriptor reaches vCenter, so a package that
// names a file outside itself is never imported. The message quotes no href:
// the details are the caller's to log.
func validateOVFFileRefs(env *ovf.Envelope) ([]string, error) {
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

// ovfPackage is the importer.Archive an OVA or OVF import reads the package
// through. It serves only members named in its allow list — the descriptor,
// then the validated file references (permit) — by exact name, from the staged
// download at path, which the provider created itself. It never opens a file
// named by the OVF, never matches a pattern, and has no remote access.
type ovfPackage struct {
	// path is the staged download (the OVA tar, or the bare .ovf itself).
	path string
	// bare is true when path is a bare .ovf descriptor, not a tar.
	bare bool
	// descriptor is the descriptor's member name.
	descriptor string
	// allowed are the member names Open serves.
	allowed map[string]bool
}

// newTarPackage returns the package of the OVA tar at path, whose descriptor
// is the member descriptor (findOVADescriptorName).
func newTarPackage(path, descriptor string) *ovfPackage {
	return &ovfPackage{path: path, descriptor: descriptor, allowed: map[string]bool{descriptor: true}}
}

// newBareOVFPackage returns the package of the bare .ovf descriptor staged at
// path. It serves the descriptor only: a bare .ovf cannot carry its files.
func newBareOVFPackage(path string) *ovfPackage {
	name := filepath.Base(path)
	return &ovfPackage{path: path, bare: true, descriptor: name, allowed: map[string]bool{name: true}}
}

// contains reports whether the package has a member named exactly name. A
// bare .ovf contains nothing but its descriptor.
func (a *ovfPackage) contains(name string) (bool, error) {
	if a.bare {
		return name == a.descriptor, nil
	}
	f, err := os.Open(filepath.Clean(a.path))
	if err != nil {
		return false, fmt.Errorf("open staged OVA: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := seekOVAMember(tar.NewReader(f), name); err != nil {
		if stderrors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// permit adds names (validated file references) to the members Open serves.
func (a *ovfPackage) permit(names []string) {
	for _, n := range names {
		a.allowed[n] = true
	}
}

// Open implements importer.Archive. It returns the member named exactly name
// when name is allowed, and os.ErrNotExist otherwise.
func (a *ovfPackage) Open(name string) (io.ReadCloser, int64, error) {
	if !a.allowed[name] {
		return nil, 0, fmt.Errorf("OVF package member %q is not permitted: %w", name, os.ErrNotExist)
	}
	f, err := os.Open(filepath.Clean(a.path))
	if err != nil {
		return nil, 0, fmt.Errorf("open staged OVF package: %w", err)
	}
	if a.bare {
		if name != a.descriptor {
			_ = f.Close()
			return nil, 0, os.ErrNotExist
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, 0, fmt.Errorf("stat staged OVF: %w", err)
		}
		return f, st.Size(), nil
	}
	tr := tar.NewReader(f)
	size, err := seekOVAMember(tr, name)
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return &ovaMember{Reader: tr, f: f}, size, nil
}

// ovaMember is an open OVA member: reads come from the tar stream, Close
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

// seekOVAMember advances tr to the member named exactly name and returns its
// size, or os.ErrNotExist when there is none. A tar read error is returned
// wrapped.
func seekOVAMember(tr *tar.Reader, name string) (int64, error) {
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return 0, os.ErrNotExist
		}
		if err != nil {
			return 0, fmt.Errorf("read OVA tar: %w", err)
		}
		if ovaMemberName(h) == name {
			return h.Size, nil
		}
	}
}
