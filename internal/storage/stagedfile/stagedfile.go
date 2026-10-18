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

// Package stagedfile opens, creates and removes the files a migration stages
// in a directory the tenant can write to (the pvc staging backend's PVC,
// mounted into the credentialed provider pods).
//
// The tenant controls everything beneath that directory. A symbolic link
// planted there — as the staged file itself or as any directory on the way to
// it — resolves in the provider pod's own filesystem, so a plain os.Open would
// read (and import into a VM the tenant owns) whatever the link names: the
// provider's credentials, its TLS key, its ServiceAccount token. A plain
// os.Create would write wherever the pod can write.
//
// Every function here therefore resolves a path strictly beneath a trusted
// root with no symbolic link in ANY component (openat2 with RESOLVE_BENEATH |
// RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS where the kernel has it, and an
// equivalent component-by-component O_NOFOLLOW walk where it does not), and
// accepts only a regular file with a single link. A refusal is an *Error that
// matches ErrUnsafe; its Error text is constant and names no path, so it is
// safe to return towards the manager and into status; Detail carries the path
// and the system error for the provider log.
package stagedfile

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// Default permissions of what Create makes; the process umask still applies.
// The staged disk is read by the target provider pod, which may run as another
// user than the source pod that wrote it, hence group/other read.
const (
	// DefaultDirPerm is the mode of the staging directories Create makes.
	DefaultDirPerm fs.FileMode = 0o755
	// DefaultFilePerm is the mode of the staged file Create makes.
	DefaultFilePerm fs.FileMode = 0o644
)

// Operations named in an *Error.
const (
	// OpOpen is a read of a staged file.
	OpOpen = "open"
	// OpCreate is a write of a staged file.
	OpCreate = "create"
	// OpRemove is a removal of a staged file.
	OpRemove = "remove"
)

// ErrUnsafe matches every refusal: a symbolic link in some component of the
// path, a path that leaves the root, or a staged entry that is not a regular
// file with exactly one link (a FIFO, a device, a socket, a directory, a
// hard-linked file).
var ErrUnsafe = errors.New("the staged file is not a regular file directly beneath the staging directory " +
	"(symbolic links, hard links and special files are refused)")

// ErrUnsupported is returned on a platform without the needed system calls.
var ErrUnsupported = errors.New("staged-file access is only supported on Linux")

// Constant Error texts for failures that are not refusals. They name no path.
const (
	msgNotFound   = "the staged file does not exist"
	msgOpenFailed = "the staged file could not be opened"
	msgCreateFail = "the staged file could not be created"
	msgRemoveFail = "the staged file could not be removed"
	msgBadPath    = "the staged file path is not a relative path beneath the staging directory"
)

// Error is a failed staged-file operation. Error() is constant per kind and
// never names a path (it may reach a VMMigration's status); Detail() names the
// root, the relative path and the underlying system error for the provider
// log. errors.Is matches ErrUnsafe for a refusal, and fs.ErrNotExist when the
// staged file (or a directory on its path) is missing.
type Error struct {
	// Op is OpOpen, OpCreate or OpRemove.
	Op string
	// Root is the trusted staging root.
	Root string
	// Rel is the path beneath Root.
	Rel string
	// Reason says why a refusal was made (empty for other failures).
	Reason string
	// Err is the underlying system error, when there is one.
	Err error
	// unsafe marks a refusal.
	unsafe bool
}

// Error returns a constant message naming no path.
func (e *Error) Error() string {
	switch {
	case e.unsafe:
		return ErrUnsafe.Error()
	case errors.Is(e.Err, fs.ErrNotExist):
		return msgNotFound
	case errors.Is(e.Err, ErrUnsupported):
		return ErrUnsupported.Error()
	}
	switch e.Op {
	case OpCreate:
		return msgCreateFail
	case OpRemove:
		return msgRemoveFail
	default:
		return msgOpenFailed
	}
}

// Detail returns the full description for the provider log: operation, root,
// relative path, refusal reason and system error. It never resolves or reads a
// symbolic link, so it cannot name what a planted link points at.
func (e *Error) Detail() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %q beneath %q", e.Op, e.Rel, e.Root)
	if e.Reason != "" {
		fmt.Fprintf(&b, ": refused: %s", e.Reason)
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

// Unwrap returns the underlying error and, for a refusal, ErrUnsafe.
func (e *Error) Unwrap() []error {
	errs := make([]error, 0, 2)
	if e.Err != nil {
		errs = append(errs, e.Err)
	}
	if e.unsafe {
		errs = append(errs, ErrUnsafe)
	}
	return errs
}

// IsUnsafe reports whether err is (or wraps) a staged-file refusal.
func IsUnsafe(err error) bool { return errors.Is(err, ErrUnsafe) }

// LogDetail returns the Detail of the *Error in err's chain, or err's own text
// when there is none. It is meant for the provider log only.
func LogDetail(err error) string {
	var se *Error
	if errors.As(err, &se) {
		return se.Detail()
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// refusal builds a refusal *Error.
func refusal(op, root, rel, reason string, err error) *Error {
	return &Error{Op: op, Root: root, Rel: rel, Reason: reason, Err: err, unsafe: true}
}

// failure builds a non-refusal *Error.
func failure(op, root, rel string, err error) *Error {
	return &Error{Op: op, Root: root, Rel: rel, Err: err}
}

// splitRel validates rel and returns its components. rel must be relative,
// non-empty, and free of "." / ".." components and empty components once
// cleaned; a ".." anywhere is refused outright rather than cleaned away.
func splitRel(op, root, rel string) ([]string, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, failure(op, root, rel, errors.New("the staging root must be an absolute path"))
	}
	if rel == "" || filepath.IsAbs(rel) {
		return nil, refusal(op, root, rel, msgBadPath, nil)
	}
	for _, c := range strings.Split(rel, "/") {
		if c == ".." {
			return nil, refusal(op, root, rel, msgBadPath, nil)
		}
	}
	clean := filepath.Clean(rel)
	if clean == "." {
		return nil, refusal(op, root, rel, msgBadPath, nil)
	}
	return strings.Split(clean, "/"), nil
}
