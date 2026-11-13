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
	"fmt"
	"path/filepath"
	"slices"
)

// Locations of the manifests and extensions the collector reads.
const (
	// ReadOnlyManifestDir is the read-only, image-provided manifest
	// directory.
	ReadOnlyManifestDir = "/usr/share/virtrigaud/host-components.d"
	// WritableManifestDir is the writable host-configuration manifest
	// directory.
	WritableManifestDir = "/etc/virtrigaud/host-components.d"
	// ExtensionReleaseDir holds the extension-release files of the merged
	// system extensions.
	ExtensionReleaseDir = "/usr/lib/extension-release.d"
)

// ExtensionDirs are the directories system extensions are looked up in, in
// the order of the system extension tool's precedence.
var ExtensionDirs = []string{
	"/etc/extensions", "/run/extensions", "/var/lib/extensions", "/usr/lib/extensions", "/usr/local/lib/extensions",
}

// PackageDBs are the package databases the collector can read as data: the
// Debian status file and the Alpine installed database. A database in a
// binary format needs a parser in C or an external tool and is not read.
var PackageDBs = []string{"/var/lib/dpkg/status", "/lib/apk/db/installed"}

// Layout names where the collector reads the host.
type Layout struct {
	// ReadOnlyDir is the read-only manifest directory.
	ReadOnlyDir string
	// WritableDir is the writable manifest directory.
	WritableDir string
	// ExtensionDirs are the system extension directories.
	ExtensionDirs []string
	// ExtensionReleaseDir holds extension-release files.
	ExtensionReleaseDir string
	// PackageDBs are the readable package databases.
	PackageDBs []string
	// AgentExecutable is the path of the agent's own executable; empty when
	// unknown (the agent component is then missing).
	AgentExecutable string
}

// DefaultLayout returns the layout of a host. The agent's own executable is
// set by the caller.
func DefaultLayout() Layout {
	return Layout{
		ReadOnlyDir:         ReadOnlyManifestDir,
		WritableDir:         WritableManifestDir,
		ExtensionDirs:       slices.Clone(ExtensionDirs),
		ExtensionReleaseDir: ExtensionReleaseDir,
		PackageDBs:          slices.Clone(PackageDBs),
	}
}

// CatalogFile is a file of a catalog component that the agent itself
// executes or loads, hashed at each collection.
type CatalogFile struct {
	// Name is the file's name in the report (never a path).
	Name string
	// Path is the file's absolute path; empty means Layout.AgentExecutable.
	Path string
	// Self marks the agent's own executable (the path comes from the
	// layout).
	Self bool
	// Optional files are hashed when present; a missing optional file does
	// not make the component missing.
	Optional bool
}

// CatalogEntry is a component the agent knows.
type CatalogEntry struct {
	// Name is the component name (lower case letters, digits and hyphens,
	// never starting with "manifest-").
	Name string
	// Required components are reported MISSING when absent and undeclared.
	Required bool
	// Range is the version range the agent requires: comparators ">=",
	// ">", "<=", "<", "=" joined by commas.
	Range string
	// Files are the files the agent executes or loads.
	Files []CatalogFile
	// Package is the name of the component in a package database; empty when
	// it is not packaged.
	Package string
	// ELFNoteFile names the file (by CatalogFile.Name) whose ELF package note
	// holds the version; empty for none.
	ELFNoteFile string
	// SelfVersion marks the agent component, whose version is the agent's
	// own build version.
	SelfVersion bool
	// OverlayPaths are the paths, relative to the root of a directory
	// extension, whose presence means the extension overlays the component.
	OverlayPaths []string
}

// DefaultCatalog returns the built-in catalog: the hypervisor with its
// control tool, its firmware, the guest kernel and the agent itself.
func DefaultCatalog() []CatalogEntry {
	return []CatalogEntry{
		{
			Name: "cloud-hypervisor", Required: true, Range: ">=1.0.0", Package: "cloud-hypervisor", ELFNoteFile: "cloud-hypervisor",
			Files: []CatalogFile{
				{Name: "cloud-hypervisor", Path: "/usr/bin/cloud-hypervisor"},
				{Name: "ch-remote", Path: "/usr/bin/ch-remote", Optional: true},
			},
			OverlayPaths: []string{"usr/bin/cloud-hypervisor", "usr/bin/ch-remote"},
		},
		{
			Name: "guest-kernel", Range: ">=0.0.0",
			Files:        []CatalogFile{{Name: "vmlinux", Path: "/usr/share/virtrigaud/guest-kernel/vmlinux"}},
			OverlayPaths: []string{"usr/share/virtrigaud/guest-kernel/vmlinux"},
		},
		{
			Name: "hypervisor-firmware", Range: ">=0.0.0",
			Files:        []CatalogFile{{Name: "hypervisor-fw", Path: "/usr/share/virtrigaud/firmware/hypervisor-fw"}},
			OverlayPaths: []string{"usr/share/virtrigaud/firmware/hypervisor-fw"},
		},
		{
			Name: "virtrigaud-agent", Required: true, Range: ">=0.0.0", SelfVersion: true,
			Files: []CatalogFile{{Name: "vrtg-agent", Self: true}},
		},
	}
}

// validateCatalog checks that every entry obeys the name, file and range
// rules, so that a report built from it can satisfy Validate.
func validateCatalog(cat []CatalogEntry) error {
	seen := map[string]bool{}
	for _, e := range cat {
		if !componentNameRE.MatchString(e.Name) || len(e.Name) > 63 || hasInvalidPrefix(e.Name) {
			return fmt.Errorf("catalog component name %q", e.Name)
		}
		if seen[e.Name] {
			return fmt.Errorf("catalog component %q twice", e.Name)
		}
		seen[e.Name] = true
		if !rangeRE.MatchString(e.Range) {
			return fmt.Errorf("catalog component %q: range %q", e.Name, e.Range)
		}
		if _, err := parseRange(e.Range); err != nil {
			return fmt.Errorf("catalog component %q: %w", e.Name, err)
		}
		for _, f := range e.Files {
			if !fileNameRE.MatchString(f.Name) {
				return fmt.Errorf("catalog component %q: file name %q", e.Name, f.Name)
			}
			if f.Path != "" && (!filepath.IsAbs(f.Path) || filepath.Clean(f.Path) != f.Path) {
				return fmt.Errorf("catalog component %q: file path of %q", e.Name, f.Name)
			}
			if f.Path == "" && !f.Self {
				return fmt.Errorf("catalog component %q: file %q has no path", e.Name, f.Name)
			}
		}
	}
	return nil
}

func hasInvalidPrefix(name string) bool {
	return len(name) >= len(invalidManifestPrefix) && name[:len(invalidManifestPrefix)] == invalidManifestPrefix
}
