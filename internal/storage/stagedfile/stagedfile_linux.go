//go:build linux

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

package stagedfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// useOpenat2 selects openat2 for reads. Tests clear it to exercise the
// component-by-component fallback that kernels before 5.6 (or a seccomp
// profile that denies openat2) get.
var useOpenat2 = true

// openat2Fn is the openat2 system call; tests replace it to inject ENOSYS,
// EPERM and EAGAIN.
var openat2Fn = unix.Openat2

// openat2Attempts bounds the openat2 retries on EAGAIN, which RESOLVE_BENEATH
// returns when a rename or mount raced the resolution; after the last one the
// open fails closed.
const openat2Attempts = 8

// resolveStrict confines an openat2 resolution beneath its dirfd and refuses
// every symbolic link (and /proc magic link) on the way.
const resolveStrict = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS

// readFlags open a staged file for reading. O_NONBLOCK keeps a planted FIFO
// from blocking the open (it is refused by the fstat check right after);
// O_NOCTTY keeps a planted terminal from becoming the controlling terminal.
const readFlags = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_NOCTTY

// dirFlags open one directory component without following a link.
const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// createFlags make a new staged file; O_EXCL never follows a link and fails
// when anything already has the name.
const createFlags = unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC

// Refusal reasons (Error.Reason; provider log only).
const (
	reasonSymlink    = "a symbolic link on the path"
	reasonEscapes    = "the path leaves the staging directory"
	reasonNotDir     = "a non-directory where a directory is expected"
	reasonNotRegular = "not a regular file"
	reasonHardLinked = "the file has more than one hard link"
	reasonDirAtName  = "a directory has the staged file's name"
	reasonRecreated  = "the staged file's name was re-created while it was being replaced"
)

// OpenRead opens root/rel for reading. Every component of rel is resolved
// beneath root without following a symbolic link, and the result must be a
// regular file with exactly one link. root itself is trusted (it is the pod's
// mount point) and is opened normally.
//
// The final component is opened O_RDONLY|O_NONBLOCK|O_NOCTTY before its type
// is checked (fstat on the open descriptor, so the check and the read are on
// the same inode): a planted FIFO therefore cannot block the open, and is
// refused right after it. Opening a device node the tenant planted is
// possible only for one the container's device cgroup allows; it is refused
// just the same, before any read.
func OpenRead(root, rel string) (*os.File, error) {
	comps, err := splitRel(OpOpen, root, rel)
	if err != nil {
		return nil, err
	}
	rootfd, err := openRoot(OpOpen, root, rel)
	if err != nil {
		return nil, err
	}
	defer closeFD(rootfd)

	fd, err := openBeneath(rootfd, comps)
	if err != nil {
		return nil, classify(OpOpen, root, rel, err)
	}
	if regErr := checkRegular(fd); regErr != nil {
		closeFD(fd)
		return nil, refusal(OpOpen, root, rel, regErr.Error(), nil)
	}
	if nbErr := unix.SetNonblock(fd, false); nbErr != nil {
		closeFD(fd)
		return nil, failure(OpOpen, root, rel, nbErr)
	}
	return os.NewFile(uintptr(fd), filepath.Join(root, rel)), nil
}

// Create creates root/rel for writing, making the missing directories on its
// path (dirPerm) without following a symbolic link in any of them. An earlier
// regular file at the name is unlinked (never opened) and a new regular file
// (filePerm) is created exclusively in its place; a symbolic link, FIFO,
// device node, socket or directory at the name is refused and left alone.
func Create(root, rel string, dirPerm, filePerm fs.FileMode) (*os.File, error) {
	comps, err := splitRel(OpCreate, root, rel)
	if err != nil {
		return nil, err
	}
	rootfd, err := openRoot(OpCreate, root, rel)
	if err != nil {
		return nil, err
	}
	defer closeFD(rootfd)

	dirfd, err := walkParent(rootfd, comps, true, dirPerm)
	if err != nil {
		return nil, classify(OpCreate, root, rel, err)
	}
	defer closeFD(dirfd)
	base := comps[len(comps)-1]

	// An earlier regular file at the name (a retried export) is replaced: its
	// name is unlinked, never opened, so no inode the tenant prepared is ever
	// written. Anything else at the name — a planted symbolic link, FIFO,
	// device node, socket or directory — is refused.
	var st unix.Stat_t
	err = retryEINTR(func() error { return unix.Fstatat(dirfd, base, &st, unix.AT_SYMLINK_NOFOLLOW) })
	switch {
	case errors.Is(err, unix.ENOENT):
	case err != nil:
		return nil, failure(OpCreate, root, rel, err)
	case st.Mode&unix.S_IFMT == unix.S_IFLNK:
		return nil, refusal(OpCreate, root, rel, reasonSymlink, nil)
	case st.Mode&unix.S_IFMT == unix.S_IFDIR:
		return nil, refusal(OpCreate, root, rel, reasonDirAtName, nil)
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		return nil, refusal(OpCreate, root, rel, reasonNotRegular, nil)
	default:
		unlinkErr := retryEINTR(func() error { return unix.Unlinkat(dirfd, base, 0) })
		switch {
		case unlinkErr == nil, errors.Is(unlinkErr, unix.ENOENT):
		case errors.Is(unlinkErr, unix.EISDIR):
			// Swapped for a directory after the fstatat.
			return nil, refusal(OpCreate, root, rel, reasonDirAtName, unlinkErr)
		default:
			return nil, failure(OpCreate, root, rel, unlinkErr)
		}
	}
	var fd int
	err = retryEINTR(func() error {
		var oerr error
		fd, oerr = unix.Openat(dirfd, base, createFlags, uint32(filePerm.Perm()))
		return oerr
	})
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil, refusal(OpCreate, root, rel, reasonRecreated, err)
		}
		return nil, classify(OpCreate, root, rel, err)
	}
	return os.NewFile(uintptr(fd), filepath.Join(root, rel)), nil
}

// Remove removes root/rel. No component of rel is followed; a symbolic link at
// the name is removed itself (unlinkat never follows the last component). A
// missing file is an *Error matching fs.ErrNotExist.
func Remove(root, rel string) error {
	comps, err := splitRel(OpRemove, root, rel)
	if err != nil {
		return err
	}
	rootfd, err := openRoot(OpRemove, root, rel)
	if err != nil {
		return err
	}
	defer closeFD(rootfd)

	dirfd, err := walkParent(rootfd, comps, false, 0)
	if err != nil {
		return classify(OpRemove, root, rel, err)
	}
	defer closeFD(dirfd)
	base := comps[len(comps)-1]
	if unlinkErr := retryEINTR(func() error { return unix.Unlinkat(dirfd, base, 0) }); unlinkErr != nil {
		if errors.Is(unlinkErr, unix.EISDIR) {
			return refusal(OpRemove, root, rel, reasonDirAtName, unlinkErr)
		}
		return failure(OpRemove, root, rel, unlinkErr)
	}
	return nil
}

// openRoot opens the trusted root directory. A missing root is reported (in
// Detail, for the provider log) as a missing staging mount: the PVC is not
// mounted where the provider expects it.
func openRoot(op, root, rel string) (int, error) {
	var fd int
	err := retryEINTR(func() error {
		var oerr error
		fd, oerr = unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		return oerr
	})
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			err = fmt.Errorf("the staging mount %q is missing (is the PVC mounted into this pod?): %w", root, err)
		}
		return -1, failure(op, root, rel, err)
	}
	return fd, nil
}

// openBeneath opens comps beneath rootfd with readFlags: openat2 with
// resolveStrict, or — on a kernel without openat2 (ENOSYS) or under a seccomp
// profile that denies it (EPERM) — the equivalent O_NOFOLLOW walk. EAGAIN
// (a racing rename or mount) is retried openat2Attempts times, then the open
// fails closed.
func openBeneath(rootfd int, comps []string) (int, error) {
	if useOpenat2 {
		fd, err := openat2Beneath(rootfd, strings.Join(comps, "/"))
		if err == nil {
			return fd, nil
		}
		if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EPERM) {
			return -1, err
		}
	}
	dirfd, err := walkParent(rootfd, comps, false, 0)
	if err != nil {
		return -1, err
	}
	defer closeFD(dirfd)
	var fd int
	err = retryEINTR(func() error {
		var oerr error
		fd, oerr = unix.Openat(dirfd, comps[len(comps)-1], readFlags, 0)
		return oerr
	})
	if err != nil {
		return -1, err
	}
	return fd, nil
}

// openat2Beneath runs openat2 with readFlags and resolveStrict, retrying
// EINTR, and EAGAIN up to openat2Attempts times.
func openat2Beneath(rootfd int, path string) (int, error) {
	how := &unix.OpenHow{Flags: uint64(readFlags), Resolve: resolveStrict}
	var (
		fd  int
		err error
	)
	for range openat2Attempts {
		err = retryEINTR(func() error {
			var oerr error
			fd, oerr = openat2Fn(rootfd, path, how)
			return oerr
		})
		if !errors.Is(err, unix.EAGAIN) {
			return fd, err
		}
	}
	return -1, err
}

// walkParent opens, one component at a time and without following a link,
// every directory of comps but the last, beneath rootfd; with create it makes
// the missing ones (dirPerm). It returns a new descriptor of the last
// directory, which the caller closes. comps holds no "." or ".." (splitRel),
// so no step can leave the root.
func walkParent(rootfd int, comps []string, create bool, dirPerm fs.FileMode) (int, error) {
	dirfd, err := retryDup(rootfd)
	if err != nil {
		return -1, err
	}
	for _, c := range comps[:len(comps)-1] {
		next, openErr := openDirNoFollow(dirfd, c)
		if errors.Is(openErr, unix.ENOENT) && create {
			if merr := retryEINTR(func() error { return unix.Mkdirat(dirfd, c, uint32(dirPerm.Perm())) }); merr != nil && !errors.Is(merr, unix.EEXIST) {
				closeFD(dirfd)
				return -1, merr
			}
			next, openErr = openDirNoFollow(dirfd, c)
		}
		closeFD(dirfd)
		if openErr != nil {
			return -1, openErr
		}
		dirfd = next
	}
	return dirfd, nil
}

// openDirNoFollow opens the single directory component name beneath dirfd.
func openDirNoFollow(dirfd int, name string) (int, error) {
	var fd int
	err := retryEINTR(func() error {
		var oerr error
		fd, oerr = unix.Openat(dirfd, name, dirFlags, 0)
		return oerr
	})
	return fd, err
}

// retryDup duplicates fd (close-on-exec).
func retryDup(fd int) (int, error) {
	var nfd int
	err := retryEINTR(func() error {
		var derr error
		nfd, derr = unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
		return derr
	})
	return nfd, err
}

// checkRegular refuses anything but a regular file with exactly one link.
// A same-filesystem hard link cannot reach the provider's credentials (they
// are on other filesystems, and link(2) cannot cross one), but a staged disk
// the source provider wrote always has a single link, so anything else is
// refused rather than reasoned about.
func checkRegular(fd int) error {
	var st unix.Stat_t
	if err := retryEINTR(func() error { return unix.Fstat(fd, &st) }); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New(reasonNotRegular)
	}
	if st.Nlink != 1 {
		return errors.New(reasonHardLinked)
	}
	return nil
}

// classify turns a system error from resolving a path into an *Error: a
// link, an escape or a non-directory on the path is a refusal; anything else
// (a missing file included) is a plain failure.
func classify(op, root, rel string, err error) *Error {
	switch {
	case errors.Is(err, unix.ELOOP):
		return refusal(op, root, rel, reasonSymlink, err)
	case errors.Is(err, unix.EXDEV):
		return refusal(op, root, rel, reasonEscapes, err)
	case errors.Is(err, unix.ENOTDIR):
		return refusal(op, root, rel, reasonNotDir, err)
	default:
		return failure(op, root, rel, err)
	}
}

// retryEINTR runs fn until it does not fail with EINTR.
func retryEINTR(fn func() error) error {
	for {
		err := fn()
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// closeFD closes fd, ignoring the error (read-only or already-flushed fds).
func closeFD(fd int) { _ = unix.Close(fd) }
