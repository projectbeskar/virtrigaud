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

// Package hostcheck holds the start-up checks a per-host agent runs before it
// opens its operation store: that it runs in the host's initial namespaces on
// a unified cgroup v2 hierarchy, that its state directory is private, owned
// by the agent's user and on a persistent, block-device-backed file system
// that carries the host's provisioning marker, that its key material is
// private and that the host private key is not readable by the agent at all
// (it is held by the separate signer process), that the signer's socket
// directory is reachable by only the signer and the agent, and that its trust configuration comes from a location the agent
// cannot write. A failed check refuses the start with a Failure naming the
// reason; the agent changes nothing on the host.
//
// Every check reads the host through a Probe, so that tests can stand in a
// fake host. Linux is the only supported system: on any other, the real
// Probe fails every check.
//
// It is used only by development builds of the agent and imports no
// Kubernetes package.
package hostcheck

import (
	"errors"
	"fmt"
	"io/fs"
)

// The reasons a check refuses the start with. A reason is a stable word for
// the system journal and, later, for the host's status; it never carries a
// path or a secret.
const (
	// ReasonRunsAsRoot: the agent runs as root. It needs no privilege and
	// must run as its own unprivileged user.
	ReasonRunsAsRoot = "RunsAsRoot"
	// ReasonUnsupportedHost: the agent does not run in the host's initial
	// namespaces, or the host has no unified cgroup v2 hierarchy.
	ReasonUnsupportedHost = "UnsupportedHost"
	// ReasonStateDirInsecure: the state directory is not a private directory
	// owned by the agent's user.
	ReasonStateDirInsecure = "StateDirInsecure"
	// ReasonStateNotPersistent: the state directory is not on a persistent,
	// block-device-backed file system, or it does not carry the host's
	// provisioning marker.
	ReasonStateNotPersistent = "StateNotPersistent"
	// ReasonPKIInsecure: the key material directory or a file in it is not
	// private to the agent's user, or is a symbolic link.
	ReasonPKIInsecure = "PKIInsecure"
	// ReasonHostKeyReadable: the agent can read the host private key file,
	// or a file of that name is in its own key material directory. The key
	// belongs to the signer process, whose user only may read it.
	ReasonHostKeyReadable = "HostKeyReadable"
	// ReasonSignerSocketInsecure: the signer's socket directory is not owned
	// by the signer's user, or grants others any access or its group write
	// access.
	ReasonSignerSocketInsecure = "SignerSocketInsecure"
	// ReasonTrustConfigInsecure: the trust configuration is writable by the
	// agent, by a user other than root, or by group or others, or reached
	// through a symbolic link.
	ReasonTrustConfigInsecure = "TrustConfigInsecure"
)

// ErrRefused is wrapped by every Failure.
var ErrRefused = errors.New("host check refused the start")

// Failure is a refused check.
type Failure struct {
	// Reason is one of the Reason constants.
	Reason string
	// Check names the check that refused.
	Check string
	// Detail says what was found. It may name a path of the agent's own
	// configuration, never a secret.
	Detail string
}

// Error implements error.
func (f *Failure) Error() string {
	return fmt.Sprintf("%s (%s): %s", f.Reason, f.Check, f.Detail)
}

// Unwrap returns ErrRefused.
func (f *Failure) Unwrap() error { return ErrRefused }

// AsFailure returns the Failure in err's chain, if any.
func AsFailure(err error) (*Failure, bool) {
	var f *Failure
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

func refuse(reason, check, format string, args ...any) *Failure {
	return &Failure{Reason: reason, Check: check, Detail: fmt.Sprintf(format, args...)}
}

// FileInfo is what a Probe reports about one path, without following a
// symbolic link.
type FileInfo struct {
	// Mode is the file's mode, including its type bits.
	Mode fs.FileMode
	// UID is the owner's user id.
	UID uint32
}

// Mount is the mount a path lies on.
type Mount struct {
	// MountPoint is the mount point.
	MountPoint string
	// FSType is the file-system type, as the kernel names it.
	FSType string
	// Source is the mount source (for a block-device-backed file system, the
	// device path).
	Source string
}

// Namespace kinds a Probe reports.
const (
	// NamespacePID is the PID namespace.
	NamespacePID = "pid"
	// NamespaceCgroup is the cgroup namespace.
	NamespaceCgroup = "cgroup"
	// NamespaceUser is the user namespace.
	NamespaceUser = "user"
	// NamespaceTime is the time namespace.
	NamespaceTime = "time"
)

// Probe reads the host. Every method is a read; none changes anything.
type Probe interface {
	// Lstat reports on path without following a symbolic link.
	Lstat(path string) (FileInfo, error)
	// ReadDir returns the names of the entries of a directory, sorted.
	ReadDir(path string) ([]string, error)
	// ReadFile returns the content of a small file, at most limit bytes; a
	// larger file is an error.
	ReadFile(path string, limit int64) ([]byte, error)
	// MountOf returns the mount path lies on, after resolving symbolic
	// links.
	MountOf(path string) (Mount, error)
	// NamespaceInode returns the inode number of the process's namespace of
	// kind. An error matching fs.ErrNotExist means the kernel has no such
	// namespace.
	NamespaceInode(kind string) (uint64, error)
	// Writable reports whether the process may write path (access(2) with
	// W_OK, which accounts for a read-only mount).
	Writable(path string) (bool, error)
	// Readable reports whether the process may read path: true when an
	// open for reading succeeds, false when it is refused by permission
	// (EACCES, EPERM), an error for anything else (a missing path, a
	// symbolic link, a loop): a path the process cannot be shown to be
	// refused is not one it is known not to read.
	Readable(path string) (bool, error)
	// EUID is the process's effective user id.
	EUID() uint32
}
