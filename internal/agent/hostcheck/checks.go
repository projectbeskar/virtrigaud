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

package hostcheck

import (
	"bytes"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
)

// Initial namespace inode numbers: the fixed inode numbers Linux gives the
// namespaces of the initial (host) set.
const (
	initUserNSInode   uint64 = 0xEFFFFFFD
	initPIDNSInode    uint64 = 0xEFFFFFFC
	initCgroupNSInode uint64 = 0xEFFFFFFB
	initTimeNSInode   uint64 = 0xEFFFFFFA
)

// CgroupRoot is where a unified cgroup v2 hierarchy is mounted.
const CgroupRoot = "/sys/fs/cgroup"

// cgroup2FSType is the file-system type of a cgroup v2 hierarchy.
const cgroup2FSType = "cgroup2"

// HostMarkerFile is the provisioning marker in the state directory: it holds
// the host's URI identity, written when the host was provisioned. Its
// presence, with the right content, is positive evidence that the state
// directory is the host's provisioned persistent state rather than an empty
// stand-in mounted in its place.
const HostMarkerFile = "host-id"

// hostKeyFileName is the host private key's file name.
const hostKeyFileName = "host.key"

// maxMarkerBytes bounds the provisioning marker.
const maxMarkerBytes = 4096

// persistentFSTypes are the file systems a state directory may be on: local,
// disk-backed file systems with the fsync, flock and O_EXCL semantics the
// store relies on.
var persistentFSTypes = map[string]bool{
	"ext4": true, "ext3": true, "xfs": true, "btrfs": true, "f2fs": true, "zfs": true,
}

// loopDevicePrefix names loop devices: a file posing as a block device. Its
// backing file can live on tmpfs, so a loop-backed mount proves nothing about
// persistence and is refused.
const loopDevicePrefix = "/dev/loop"

// datasetFSTypes are persistent file systems whose mount source is a dataset
// name rather than a device path.
var datasetFSTypes = map[string]bool{"zfs": true}

// Config names what Run checks.
type Config struct {
	// StateDir is the agent's state directory.
	StateDir string
	// PKIDir is the directory of the host's key and certificate.
	PKIDir string
	// TrustDir is the trust configuration directory.
	TrustDir string
	// UID is the agent user's id: the owner the state and key material
	// directories must have.
	UID uint32
	// SignerKeyFile is the path of the host private key file, which belongs
	// to the signer process; the agent must not be able to read it.
	SignerKeyFile string
	// SignerSocketDir is the directory of the signer's socket.
	SignerSocketDir string
	// SignerUID is the signer user's id: the owner of SignerSocketDir.
	SignerUID uint32
}

// Run runs every start-up check that needs no certificate, in order: the
// process is not root, the namespaces, cgroup v2, the state directory (owner,
// mode, persistence), the key material, that the agent cannot read the host
// private key, the signer's socket directory and the trust configuration. It returns the first Failure.
// The provisioning marker is checked separately (CheckHostMarker), once the
// host's identity is known.
func Run(p Probe, cfg Config) error {
	for _, check := range []func() error{
		func() error { return CheckNotRoot(p) },
		func() error { return CheckNamespaces(p) },
		func() error { return CheckCgroupV2(p) },
		func() error { return CheckStateDir(p, cfg.StateDir, cfg.UID) },
		func() error { return CheckPersistent(p, cfg.StateDir) },
		func() error { return CheckPKI(p, cfg.PKIDir, cfg.UID) },
		func() error { return CheckHostKeyUnreadable(p, cfg.PKIDir, cfg.SignerKeyFile) },
		func() error { return CheckSignerSocketDir(p, cfg.SignerSocketDir, cfg.SignerUID) },
		func() error { return CheckTrustDir(p, cfg.TrustDir) },
	} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// CheckNotRoot refuses a process whose effective uid is 0: the agent holds
// no privilege and runs as its own user, which also keeps its trust
// configuration out of its reach.
func CheckNotRoot(p Probe) error {
	if p.EUID() == 0 {
		return refuse(ReasonRunsAsRoot, "user", "the agent runs as root; run it as its own unprivileged user")
	}
	return nil
}

// CheckNamespaces refuses a process outside the host's initial PID, cgroup,
// user or time namespace. A kernel without time namespaces passes the time
// check.
func CheckNamespaces(p Probe) error {
	const check = "namespaces"
	for _, ns := range []struct {
		kind     string
		want     uint64
		optional bool
	}{
		{NamespacePID, initPIDNSInode, false},
		{NamespaceCgroup, initCgroupNSInode, false},
		{NamespaceUser, initUserNSInode, false},
		{NamespaceTime, initTimeNSInode, true},
	} {
		ino, err := p.NamespaceInode(ns.kind)
		if err != nil {
			if ns.optional && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return refuse(ReasonUnsupportedHost, check, "cannot read the %s namespace: %v", ns.kind, err)
		}
		if ino != ns.want {
			return refuse(ReasonUnsupportedHost, check, "the agent does not run in the host's initial %s namespace", ns.kind)
		}
	}
	return nil
}

// CheckCgroupV2 refuses a host whose cgroup root is not a unified cgroup v2
// hierarchy (a legacy or hybrid layout).
func CheckCgroupV2(p Probe) error {
	const check = "cgroup"
	m, err := p.MountOf(CgroupRoot)
	if err != nil {
		return refuse(ReasonUnsupportedHost, check, "cannot read the mount of %s: %v", CgroupRoot, err)
	}
	if m.MountPoint != CgroupRoot || m.FSType != cgroup2FSType {
		return refuse(ReasonUnsupportedHost, check, "%s is not a unified cgroup v2 hierarchy (found %q on %s)", CgroupRoot, m.FSType, m.MountPoint)
	}
	return nil
}

// privateDir refuses a path that is not a directory, is a symbolic link, is
// not owned by uid, or grants any permission to group or others.
func privateDir(p Probe, reason, check, dir string, uid uint32) error {
	if dir == "" || !filepath.IsAbs(dir) {
		return refuse(reason, check, "%q is not an absolute path", dir)
	}
	fi, err := p.Lstat(dir)
	if err != nil {
		return refuse(reason, check, "cannot stat %s: %v", dir, err)
	}
	switch {
	case fi.Mode&fs.ModeSymlink != 0:
		return refuse(reason, check, "%s is a symbolic link", dir)
	case !fi.Mode.IsDir():
		return refuse(reason, check, "%s is not a directory", dir)
	case fi.UID != uid:
		return refuse(reason, check, "%s is owned by uid %d, not the agent's uid %d", dir, fi.UID, uid)
	case fi.Mode.Perm()&0o077 != 0:
		return refuse(reason, check, "%s has mode %04o; it must grant nothing to group or others", dir, fi.Mode.Perm())
	}
	return nil
}

// CheckStateDir refuses a state directory that is not a private directory
// (no permission for group or others) owned by the agent's user uid, or that
// is a symbolic link.
func CheckStateDir(p Probe, dir string, uid uint32) error {
	return privateDir(p, ReasonStateDirInsecure, "state-directory", dir, uid)
}

// CheckPersistent refuses a state directory that is not positively on a
// persistent file system: its mount must be one of the local, disk-backed
// file-system types, mounted from a block device that is not a loop device
// (or a dataset, for ZFS). A tmpfs, ramfs or overlay mount, a loop-backed
// mount and a network file system are refused,
// which also catches a persistent partition that failed to mount and left
// an in-memory directory in its place.
func CheckPersistent(p Probe, dir string) error {
	const check = "persistent-state"
	m, err := p.MountOf(dir)
	if err != nil {
		return refuse(ReasonStateNotPersistent, check, "cannot read the mount of %s: %v", dir, err)
	}
	if !persistentFSTypes[m.FSType] {
		return refuse(ReasonStateNotPersistent, check, "%s is on a %q file system mounted at %s, not a persistent disk-backed one", dir, m.FSType, m.MountPoint)
	}
	if !datasetFSTypes[m.FSType] && !strings.HasPrefix(m.Source, "/dev/") {
		return refuse(ReasonStateNotPersistent, check, "%s is on a %q mount at %s that is not backed by a block device", dir, m.FSType, m.MountPoint)
	}
	if strings.HasPrefix(m.Source, loopDevicePrefix) {
		return refuse(ReasonStateNotPersistent, check, "%s is on a loop device (%s), whose backing file may itself be in memory", dir, m.Source)
	}
	return nil
}

// CheckHostMarker refuses a state directory whose provisioning marker
// (HostMarkerFile) is missing, is not a private regular file owned by uid, or
// does not name hostURI.
func CheckHostMarker(p Probe, stateDir, hostURI string, uid uint32) error {
	const check = "host-marker"
	path := filepath.Join(stateDir, HostMarkerFile)
	fi, err := p.Lstat(path)
	if err != nil {
		return refuse(ReasonStateNotPersistent, check, "the provisioning marker %s is missing or unreadable: %v", path, err)
	}
	if !fi.Mode.IsRegular() || fi.UID != uid || fi.Mode.Perm()&0o022 != 0 {
		return refuse(ReasonStateNotPersistent, check, "the provisioning marker %s is not a regular file owned by the agent and writable only by it", path)
	}
	b, err := p.ReadFile(path, maxMarkerBytes)
	if err != nil {
		return refuse(ReasonStateNotPersistent, check, "cannot read the provisioning marker %s: %v", path, err)
	}
	if string(bytes.TrimSpace(b)) != hostURI {
		return refuse(ReasonStateNotPersistent, check, "the provisioning marker %s names another host", path)
	}
	return nil
}

// CheckPKI refuses a key material directory that is not a private directory
// owned by uid, and any entry in it that is not a regular file owned by uid
// with no permission for group or others (a symbolic link included).
func CheckPKI(p Probe, dir string, uid uint32) error {
	const check = "pki"
	if err := privateDir(p, ReasonPKIInsecure, check, dir, uid); err != nil {
		return err
	}
	names, err := p.ReadDir(dir)
	if err != nil {
		return refuse(ReasonPKIInsecure, check, "cannot list %s: %v", dir, err)
	}
	for _, n := range names {
		path := filepath.Join(dir, n)
		fi, err := p.Lstat(path)
		if err != nil {
			return refuse(ReasonPKIInsecure, check, "cannot stat %s: %v", path, err)
		}
		switch {
		case !fi.Mode.IsRegular():
			return refuse(ReasonPKIInsecure, check, "%s is not a regular file", path)
		case fi.UID != uid:
			return refuse(ReasonPKIInsecure, check, "%s is owned by uid %d, not the agent's uid %d", path, fi.UID, uid)
		case fi.Mode.Perm()&0o077 != 0:
			return refuse(ReasonPKIInsecure, check, "%s has mode %04o; it must grant nothing to group or others", path, fi.Mode.Perm())
		}
	}
	return nil
}

// CheckHostKeyUnreadable refuses a host that lets the agent read the host
// private key. The key is held by the separate signer process so that the
// TLS-facing agent never has its bytes; a key file the agent can read (at
// keyFile, the signer's file) defeats that, and so does a file of that name
// in the agent's own key material directory, readable or not, which is a
// stale copy in the wrong place.
func CheckHostKeyUnreadable(p Probe, pkiDir, keyFile string) error {
	const check = "host-key"
	if keyFile == "" || !filepath.IsAbs(keyFile) {
		return refuse(ReasonHostKeyReadable, check, "%q is not an absolute path to the signer's key file", keyFile)
	}
	own := filepath.Join(pkiDir, hostKeyFileName)
	if _, err := p.Lstat(own); err == nil {
		return refuse(ReasonHostKeyReadable, check, "%s exists: the host private key belongs to the signer process, not the agent", own)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return refuse(ReasonHostKeyReadable, check, "cannot tell whether %s exists: %v", own, err)
	}
	if guard.Off(guard.HostKeyUnreadable) {
		return nil
	}
	for _, path := range []string{keyFile, own} {
		ok, err := p.Readable(path)
		if err != nil {
			return refuse(ReasonHostKeyReadable, check, "cannot tell whether %s is readable: %v", path, err)
		}
		if ok {
			return refuse(ReasonHostKeyReadable, check, "the agent can read the host private key file %s; only the signer's user may", path)
		}
	}
	return nil
}

// CheckSignerSocketDir refuses a signer socket directory that is a symbolic
// link or not a directory, is not owned by the signer's user uid, grants
// anything to others or grants write permission to its group: only the
// signer's user and the group the directory names (the agent's) can reach the
// socket, and that group cannot replace it.
func CheckSignerSocketDir(p Probe, dir string, uid uint32) error {
	const check = "signer-socket"
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return refuse(ReasonSignerSocketInsecure, check, "%q is not a clean absolute path", dir)
	}
	fi, err := p.Lstat(dir)
	if err != nil {
		return refuse(ReasonSignerSocketInsecure, check, "cannot stat %s: %v", dir, err)
	}
	switch {
	case fi.Mode&fs.ModeSymlink != 0 || !fi.Mode.IsDir():
		return refuse(ReasonSignerSocketInsecure, check, "%s is not a directory", dir)
	case fi.UID != uid:
		return refuse(ReasonSignerSocketInsecure, check, "%s is owned by uid %d, not the signer's uid %d", dir, fi.UID, uid)
	case fi.Mode.Perm()&0o027 != 0:
		return refuse(ReasonSignerSocketInsecure, check, "%s has mode %04o; it must grant nothing to others and no write to its group", dir, fi.Mode.Perm())
	}
	return nil
}

// CheckTrustDir refuses a trust configuration the agent could change, or
// that anyone but root could change: every directory from the root down to
// dir, and every entry of dir, must be owned by root, grant no write
// permission to group or others and not be a symbolic link; dir and its
// entries must be regular files and directories that the agent's process
// cannot write (access(2) refuses the write, for example on a read-only
// mount or for a non-root agent). Trust anchors therefore come from the
// host's signed, read-only system image, never from a writable location.
func CheckTrustDir(p Probe, dir string) error {
	const check = "trust-config"
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return refuse(ReasonTrustConfigInsecure, check, "%q is not a clean absolute path", dir)
	}
	// Every ancestor, from "/" down.
	var chain []string
	for d := dir; ; d = filepath.Dir(d) {
		chain = append([]string{d}, chain...)
		if d == "/" {
			break
		}
	}
	for _, d := range chain {
		if err := rootOwned(p, check, d, true); err != nil {
			return err
		}
	}
	names, err := p.ReadDir(dir)
	if err != nil {
		return refuse(ReasonTrustConfigInsecure, check, "cannot list %s: %v", dir, err)
	}
	paths := []string{dir}
	for _, n := range names {
		path := filepath.Join(dir, n)
		if err := rootOwned(p, check, path, false); err != nil {
			return err
		}
		paths = append(paths, path)
	}
	for _, path := range paths {
		w, err := p.Writable(path)
		if err != nil {
			return refuse(ReasonTrustConfigInsecure, check, "cannot tell whether %s is writable: %v", path, err)
		}
		if w {
			return refuse(ReasonTrustConfigInsecure, check, "%s is writable by the agent", path)
		}
	}
	return nil
}

// rootOwned refuses a path that is a symbolic link, is not owned by root, or
// that group or others may write; wantDir says whether it must be a
// directory (otherwise a regular file).
func rootOwned(p Probe, check, path string, wantDir bool) error {
	fi, err := p.Lstat(path)
	if err != nil {
		return refuse(ReasonTrustConfigInsecure, check, "cannot stat %s: %v", path, err)
	}
	switch {
	case fi.Mode&fs.ModeSymlink != 0:
		return refuse(ReasonTrustConfigInsecure, check, "%s is a symbolic link", path)
	case wantDir && !fi.Mode.IsDir():
		return refuse(ReasonTrustConfigInsecure, check, "%s is not a directory", path)
	case !wantDir && !fi.Mode.IsRegular():
		return refuse(ReasonTrustConfigInsecure, check, "%s is not a regular file", path)
	case fi.UID != 0:
		return refuse(ReasonTrustConfigInsecure, check, "%s is owned by uid %d, not root", path, fi.UID)
	case fi.Mode.Perm()&0o022 != 0:
		return refuse(ReasonTrustConfigInsecure, check, "%s has mode %04o; group or others may write it", path, fi.Mode.Perm())
	}
	return nil
}
