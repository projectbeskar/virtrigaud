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

package libvirt

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Privileged disk copies (ADR-0007 Slice 5 lab, B1).
//
// After an external (disk-only) snapshot a VM runs on libvirt's overlay
// <pool>/<domain>-disk.<snapshot>, which libvirt creates 0600 and owned by the
// qemu user: the provider's SSH user cannot read it, even as a member of kvm,
// so a full clone or a disk export of a snapshotted VM failed with "Permission
// denied". The three copies that read a VM's whole disk chain — a full
// clone's copy (single-host and clustered) and the s3 and nfs exports' flatten
// — therefore run qemu-img as root through passwordless sudo (`sudo -n`: never
// a prompt; a refusal is immediate):
//
//   - the source is opened in the format its domain definition names (-f;
//     the exports' historical -f qcow2), by root and by the SSH user alike:
//     qemu-img never probes a disk's format, so a guest cannot make a raw
//     disk it wrote read as a qcow2 image that names another file as its
//     backing file;
//   - before either reads it, the source's image chain is read one image at a
//     time, as the disk in-use check reads it (walkBackingChainFrom: local
//     regular files only, never a protocol or json: name; a raw disk has no
//     chain). Every image must be qcow2 or raw, named with its format, without
//     an external data file, not a symbolic link, and in a directory no
//     account other than root and the SSH user can write (or a sticky one:
//     unsafeChainMemberReason) — so no other account can swap an image the
//     copy reads. Otherwise the copy is refused (checkCopySource,
//     copyRefusedError: FailedPrecondition, and VM_OPERATION_FAILED on a
//     routed call) — never retried as the SSH user, who reads every VM disk on
//     the host through the kvm group;
//   - the directory a root copy writes its local output in must be just as
//     safe (unsafeHostDirReason), or the copy is refused: root never writes
//     where another account could swap the copy's private directory;
//   - a local output is created by the SSH user, under the copy's umask,
//     inside the copy's private directory BEFORE root writes into it
//     (createCopyOutputScript): root never creates the file, so its mode does
//     not depend on sudo's umask settings, and no other account can plant a
//     link there. The finished clone disk is then renamed into place with
//     `mv -f -T` and given to the qemu user with `chown -h` (#358);
//   - on a clustered host the Slice 3 guard's flock is held by the SSH user
//     outside sudo, and timeout(1) runs INSIDE sudo, so it can stop — and
//     kill — the root qemu-img when the call's budget runs out
//     (hostCmdGuard.applyPrivileged);
//   - only when sudo itself refuses (no passwordless rule for the command, or
//     no sudo at all; sudoRefused matches sudo's exit status and exact
//     messages) does the copy run as the SSH user, with the same pinned
//     format: a disk it can read is copied as before, and a 0600 overlay
//     fails as before.
//
// The sudoers entries this needs are documented in docs/libvirt-clones.md.

// sudoNonInteractive is sudo's -n: refuse at once, never prompt, when no
// passwordless rule allows the command.
const sudoNonInteractive = "-n"

// privilegedSourceFormats are the source formats a copy opens, as root or as
// the SSH user: the formats of the VM disks VirtRigaud and libvirt create. A
// disk of any other format is not copied (checkCopySource).
var privilegedSourceFormats = map[string]bool{"qcow2": true, "raw": true}

// libvirtDefaultDiskFormat is the format libvirt's QEMU driver gives a
// file-backed disk whose definition names none (it never probes).
const libvirtDefaultDiskFormat = "raw"

// createCopyOutputScript is the fixed `sh -c` script that creates a copy's
// local output before root writes into it: "$1" is the umask, "$2" the file,
// which must not exist yet (noclobber). It runs as the SSH user.
const createCopyOutputScript = `umask "$1" && set -C && : > "$2"`

// diskCopy is one qemu-img copy that reads a VM's whole disk chain: a full
// clone's copy, or an s3/nfs export's flatten.
type diskCopy struct {
	// src is the VM disk the copy reads, and srcFormat the format its domain
	// definition opens it in — the format both the root and the SSH-user
	// copy pin with -f (a copy of a disk of any other format, or of unknown
	// format, is refused).
	src, srcFormat string
	// target is the host file the copy writes, for the clustered guard's
	// symbolic-link check ("" when it writes none).
	target string
	// output is a local file the privileged copy writes, created by the SSH
	// user under umask before root writes into it ("" for none: the output
	// already exists, or is not a local file).
	output string
	// outDir is the directory the copy's private output directory lies in,
	// which root writes below: a root copy is refused unless it is safe
	// (unsafeHostDirReason). "" when the copy writes no local file.
	outDir string
	// umask is the umask the copy runs under ("" for none).
	umask string
	// args is the qemu-img argv (after "qemu-img") the SSH user runs when
	// sudo is not available; its source format is pinned too.
	args []string
	// privArgs is the qemu-img argv the privileged copy runs; nil means the
	// copy never runs as root.
	privArgs []string
}

// copyRefusedError refuses a copy whose source is not safe to copy: its
// format is not one a copy opens, or its image chain could not be verified
// (checkCopySource). Nothing was copied, and the copy is not retried as the
// SSH user: a chain that is unsafe for root is unsafe for the SSH user too
// (it reads every VM disk on the host through the kvm group).
type copyRefusedError struct {
	// src is the refused source disk (provider log only).
	src string
	// reason says why (provider log only; it may name host paths).
	reason string
}

// Error is the refusal, for the provider's log and the single-host caller.
func (e *copyRefusedError) Error() string {
	return fmt.Sprintf("the source disk %s cannot be copied safely: %s", e.src, e.reason)
}

// copyRefusedWire is the requester-facing account of a copyRefusedError on a
// routed call: it names no host path.
const copyRefusedWire = "the source VM's disk image chain cannot be copied safely (details are in the provider log)"

// GRPCStatus renders the refusal as codes.FailedPrecondition: the copy is not
// retried until the source changes, and the manager never counts it toward
// its circuit breaker. status.FromError finds it through the single-host
// handlers' "failed to ..." wrapping.
func (e *copyRefusedError) GRPCStatus() *status.Status {
	return status.New(codes.FailedPrecondition, e.Error())
}

// run runs the copy on the host behind h, under guard (nil on a single host).
// The source's chain is checked first (checkCopySource): a refusal ends the
// copy, whoever would have run it. Then it runs as root through passwordless
// sudo, and as the SSH user — with the same pinned source format — ONLY when
// sudo itself refused (sudoRefused: not installed, or no passwordless rule).
func (c diskCopy) run(ctx context.Context, h hostCommandRunner, guard *hostCmdGuard) (*VirshResult, error) {
	if err := checkCopySource(ctx, h, c.src, c.srcFormat); err != nil {
		return nil, err
	}
	if c.privArgs != nil && c.outDir != "" {
		reason, err := unsafeHostDirReason(ctx, h, c.outDir)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			log.Printf("WARN Refusing to copy %s: %s", c.src, reason)
			return nil, &copyRefusedError{src: c.src, reason: reason + "; root does not write there"}
		}
	}
	if c.privArgs != nil {
		argv, err := guard.applyPrivileged(c.target, c.umask, append([]string{"qemu-img"}, c.privArgs...)...)
		if err != nil {
			return nil, err
		}
		if c.output != "" {
			if _, err := runHost(ctx, h, "sh", "-c", createCopyOutputScript, "sh", c.umask, c.output); err != nil {
				return nil, fmt.Errorf("create the copy's output file: %w", err)
			}
		}
		res, err := runHost(ctx, h, argv...)
		if err == nil || !sudoRefused(res) {
			return res, err
		}
		log.Printf("INFO Passwordless sudo is not allowed for the copy of %s (%s); copying it as the provider's SSH user, "+
			"which fails for a disk it cannot read, such as the 0600 overlay of a VM with an external snapshot "+
			"(see docs/libvirt-clones.md)", c.src, strings.TrimSpace(res.Stderr))
	}
	return c.runAsSSHUser(ctx, h, guard)
}

// readDiskInfoOnHost runs `qemu-img info -U ... <path>` on the host behind h
// for GetDiskInfo (ADR-0007 Slice 5 lab follow-up). A disk named by the
// domain's own definition (ownFormat: its definition format, "" for any other
// path) is read in that format (-f; never probed, so a guest's raw disk is
// never read as a qcow2 header it wrote) and, for qcow2 and raw, the way the
// disk in-use check reads it: once it is confirmed not to be a device, FIFO or
// other non-regular file (checkChainFileKind), through passwordless sudo,
// falling back to the SSH user when sudo refuses (qemuImgInfoOnHost; the
// documented `qemu-img info` rule). So the sizes of a snapshotted VM's 0600
// libvirt-qemu overlay are read during an export instead of reported as 0.
// qemu-img info opens no backing file. A disk of another format is read as
// the SSH user, its format still pinned. Any other path — an explicit disk
// path a single-host caller passed — is read as the SSH user, as before: root
// never opens a caller-supplied path.
func readDiskInfoOnHost(ctx context.Context, h hostCommandRunner, path, ownFormat string) (*VirshResult, error) {
	if ownFormat == "" {
		return runHost(ctx, h, "qemu-img", "info", "-U", "--output=json", path)
	}
	args := []string{"-U", "-f", ownFormat, "--output=json", "--", path}
	if privilegedSourceFormats[ownFormat] {
		if reason, err := rootReadableDiskReason(ctx, h, path); err != nil {
			return nil, err
		} else if reason != "" {
			log.Printf("WARN Reading disk info of %s as the provider's SSH user, not as root: %s", path, reason)
		} else {
			return qemuImgInfoOnHost(ctx, h, args...)
		}
	}
	return runHost(ctx, h, append([]string{"qemu-img", "info"}, args...)...)
}

// rootReadableDiskReason says why GetDiskInfo does not read path as root, or
// "" when it may: path must be a regular file (checkChainFileKind) that no
// account other than root and the SSH user could swap
// (unsafeChainMemberReason). A host that could not be reached is the error.
func rootReadableDiskReason(ctx context.Context, h hostCommandRunner, path string) (string, error) {
	if err := checkChainFileKind(ctx, h, path); err != nil {
		if isHostTransportFailure(err) || ctx.Err() != nil {
			return "", err
		}
		return err.Error(), nil
	}
	return unsafeChainMemberReason(ctx, h, path)
}

// copyStderr formats a failed copy's stderr for the provider's log (" (qemu-img
// stderr: ...)"), or "" when there is none.
func copyStderr(res *VirshResult) string {
	if res == nil {
		return ""
	}
	if s := strings.TrimSpace(res.Stderr); s != "" {
		return fmt.Sprintf(" (qemu-img stderr: %s)", s)
	}
	return ""
}

// runAsSSHUser runs the copy as the SSH user, under guard.
func (c diskCopy) runAsSSHUser(ctx context.Context, h hostCommandRunner, guard *hostCmdGuard) (*VirshResult, error) {
	cmd := append([]string{"qemu-img"}, c.args...)
	if c.umask != "" {
		cmd = withUmask(c.umask, cmd...)
	}
	argv, err := guard.apply(c.target, cmd...)
	if err != nil {
		return nil, err
	}
	return runHost(ctx, h, argv...)
}

// checkCopySource verifies, before qemu-img reads src in format — as root or
// as the SSH user — that the copy may run: the format is one a copy opens
// (privilegedSourceFormats), and src's image chain, read one image at a time
// with the disk pinned to format (a raw disk has no chain), is local regular
// files each opened in the format its parent names — qemu-img, following the
// same chain, then never probes an image, and never opens a protocol, json:
// or relative backing name, a device or a FIFO — each qcow2 or raw, without
// an external data file, and none of them one another account could swap: a
// symbolic link, or an image in a directory an account other than root and
// the SSH user can write and that is not sticky (unsafeChainMemberReason).
// Anything else is a copyRefusedError: the copy is refused, not retried as
// the SSH user, who reads every VM disk through the kvm group. A host that
// could not be reached while the chain was read is that host failure.
func checkCopySource(ctx context.Context, h hostCommandRunner, src, format string) error {
	refuse := func(reason string) error {
		log.Printf("WARN Refusing to copy %s: %s", src, reason)
		return &copyRefusedError{src: src, reason: reason}
	}
	if !privilegedSourceFormats[format] {
		return refuse(fmt.Sprintf("its format %q (from its domain definition) is not one a copy opens (qcow2, raw)", format))
	}
	levels, err := walkBackingChainFrom(ctx, h, src, format)
	if err != nil {
		if isHostTransportFailure(err) || ctx.Err() != nil {
			return err
		}
		return refuse(fmt.Sprintf("its image chain could not be read and verified (for a disk the SSH user cannot read, "+
			"allow passwordless sudo for qemu-img info; see docs/libvirt-clones.md): %v", err))
	}
	if len(levels) == 0 {
		return refuse("it does not exist")
	}
	for _, l := range levels {
		switch {
		case l.format == "":
			return refuse(fmt.Sprintf("the image chain names %s without its format", l.path))
		case !privilegedSourceFormats[l.format]:
			return refuse(fmt.Sprintf("the image chain opens %s as %q, not qcow2 or raw", l.path, l.format))
		case l.dataFile != "":
			return refuse(fmt.Sprintf("%s has an external data file", l.path))
		}
		reason, err := unsafeChainMemberReason(ctx, h, l.path)
		if err != nil {
			return err
		}
		if reason != "" {
			return refuse(reason + ": another account could swap the image the copy reads")
		}
	}
	return nil
}

// What chainMemberScript prints for a symbolic link.
const chainMemberSymlink = "symlink"

// chainMemberScript is the fixed `sh -c` script behind
// unsafeChainMemberReason: "$1" is an image of a disk's chain, "$2" its
// directory. It prints chainMemberSymlink when the image is a symbolic link,
// and otherwise what diskDirModeScript prints for the directory.
const chainMemberScript = `if [ -L "$1" ]; then echo ` + chainMemberSymlink + `; else stat -L -c '%a %u' -- "$2" && id -u; fi`

// dirModeOutputRE is the shape of diskDirModeScript's output: the octal mode
// and owner uid of the directory, then the SSH user's uid.
var dirModeOutputRE = regexp.MustCompile(`^[0-7]{3,4} [0-9]{1,10}\s+[0-9]{1,10}$`)

// unsafeChainMemberReason says why root must not open path, an image of a
// disk's chain on the host behind h, or "" when it may: path is a symbolic
// link, or its directory is not safe (dirModeReason). Either would let an
// account other than root and the SSH user swap the image root opens. A check
// that fails or cannot be read is a reason too (fail closed); a host that
// could not be reached is the error.
func unsafeChainMemberReason(ctx context.Context, h hostCommandRunner, path string) (string, error) {
	dir := filepath.Dir(path)
	res, err := runHost(ctx, h, "sh", "-c", chainMemberScript, "sh", path, dir)
	if err != nil {
		if isHostTransportFailure(err) || ctx.Err() != nil {
			return "", err
		}
		return fmt.Sprintf("%s and its directory could not be checked: %v", path, err), nil
	}
	out := strings.TrimSpace(res.Stdout)
	if out == chainMemberSymlink {
		return fmt.Sprintf("%s is a symbolic link", path), nil
	}
	return dirModeReason(dir, out), nil
}

// unsafeHostDirReason says why root must not write below dir on the host
// behind h (diskDirModeScript, dirModeReason), or "" when it may. A check
// that fails is a reason too (fail closed); a host that could not be reached
// is the error.
func unsafeHostDirReason(ctx context.Context, h hostCommandRunner, dir string) (string, error) {
	res, err := runHost(ctx, h, "sh", "-c", diskDirModeScript, "sh", dir)
	if err != nil {
		if isHostTransportFailure(err) || ctx.Err() != nil {
			return "", err
		}
		return fmt.Sprintf("the permissions of %s could not be checked: %v", dir, err), nil
	}
	return dirModeReason(dir, strings.TrimSpace(res.Stdout)), nil
}

// dirModeReason reads diskDirModeScript's output for dir: "" when only root
// and the SSH user can write dir, or it is sticky; otherwise why not
// (unsafeDiskDirReason) — including output it cannot read.
func dirModeReason(dir, out string) string {
	if !dirModeOutputRE.MatchString(out) {
		return fmt.Sprintf("the permissions of %s could not be read", dir)
	}
	if reason := unsafeDiskDirReason(out); reason != "" {
		return fmt.Sprintf("the directory %s %s and is not sticky", dir, reason)
	}
	return ""
}

// definitionDiskFormat returns the format domainXML opens the file-backed
// disk diskPath in — its <driver type=...>, libvirt's raw when it names none
// (domainDisksDoc.diskFormat) — or "" when the definition cannot be read or
// has no such disk.
func definitionDiskFormat(domainXML, diskPath string) string {
	doc, err := parseDomainDisks(domainXML)
	if err != nil {
		return ""
	}
	return doc.diskFormat(diskPath)
}

// exportSourceOn resolves the disk an s3/nfs export of domain d on vp's host
// reads, and the format its definition opens it in: the primary disk, or —
// when diskID names a path — that path, which must be one of the domain's
// own disks (any other file on the host is never read, as root or not). The
// export pins that format (-f) and is refused for a format other than qcow2
// or raw (checkCopySource).
func exportSourceOn(ctx context.Context, vp *VirshProvider, d domainTarget, diskID string) (string, string, error) {
	doc, err := domainDisksOf(ctx, vp, d.handle)
	if err != nil {
		return "", "", contracts.NewRetryableError(fmt.Sprintf("failed to read disks for source VM %q", d.name), err)
	}
	disks := doc.diskFiles()
	if len(disks) == 0 {
		return "", "", contracts.NewInvalidSpecError(fmt.Sprintf("source VM %q has no usable disk to export", d.name), nil)
	}
	path := disks[0]
	if strings.Contains(diskID, "/") {
		if !slices.Contains(disks, diskID) {
			return "", "", contracts.NewInvalidSpecError(fmt.Sprintf("disk %q is not a disk of VM %q", diskID, d.name), nil)
		}
		path = diskID
	}
	return path, doc.diskFormat(path), nil
}

// fullCloneCopy is a full clone's copy of srcDiskPath (opened as srcFormat by
// the privileged copy) into out, a file in the clone's private write
// directory, for the disk targetDiskPath: a standalone qcow2 that flattens the
// source's backing chain, created with vmDiskMode.
func fullCloneCopy(srcDiskPath, srcFormat, out, targetDiskPath string) diskCopy {
	// Both the root and the SSH-user copy open the source in the format its
	// definition names: qemu-img never probes it (checkCopySource refuses a
	// format that is not qcow2 or raw, or unknown).
	args := []string{"convert", "-f", srcFormat, "-O", "qcow2", srcDiskPath, out}
	return diskCopy{
		src:       srcDiskPath,
		srcFormat: srcFormat,
		target:    targetDiskPath,
		output:    out,
		outDir:    filepath.Dir(targetDiskPath),
		umask:     vmDiskUmask,
		args:      args,
		privArgs:  args,
	}
}

// posixIDRE is the shape of a uid or gid `id` prints.
var posixIDRE = regexp.MustCompile(`^[0-9]{1,10}$`)

// nfsRootBaseRE is the shape of the server-and-path part of an nfs:// URL a
// root export writes to: one host and an absolute path, none of the URL
// delimiters or whitespace the manager's NFS URL builder rejects
// (migration.NFSURL).
var nfsRootBaseRE = regexp.MustCompile(`^nfs://[^/?#&\s]+/[^?#&\s]+$`)

// nfsURLForRoot returns the nfs:// URL an nfs export writes to when it runs
// as root: nfsURL's server and path with EXACTLY the SSH user's uid and gid
// on the host behind h (`id -u`, `id -g`) as its libnfs query —
// `?uid=<uid>&gid=<gid>`, the shape the sudoers rule pins — and every other
// query parameter of nfsURL dropped, so no libnfs option (a port, a debug
// level, ...) reaches a root qemu-img. libnfs presents the uid and gid of
// the process by default: root would reach the NFS server as uid 0 (squashed
// to the anonymous user by a root_squash export, NOT squashed by a
// no_root_squash one), not as the identity the export has always used.
//
// An error means the export must not run as root: nfsURL cannot be read, or
// names a uid or gid other than the SSH user's (a VMMigration's
// spec.storage.nfs.uid/gid, which the SSH user's own qemu-img presents, as
// it always has), or the SSH user's ids cannot be read.
func nfsURLForRoot(ctx context.Context, h hostCommandRunner, nfsURL string) (string, error) {
	base, rawQuery, _ := strings.Cut(nfsURL, "?")
	if !nfsRootBaseRE.MatchString(base) || strings.Contains(rawQuery, "#") {
		return "", fmt.Errorf("the nfs destination is not a plain nfs://<server>/<path> URL")
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", fmt.Errorf("parse the nfs destination's query: %w", err)
	}
	ids := make(map[string]string, 2)
	for _, id := range []struct{ key, flag string }{{"uid", "-u"}, {"gid", "-g"}} {
		res, err := runHost(ctx, h, "id", id.flag)
		if err != nil {
			return "", fmt.Errorf("read the SSH user's %s on the host: %w", id.key, err)
		}
		v := strings.TrimSpace(res.Stdout)
		if !posixIDRE.MatchString(v) {
			return "", fmt.Errorf("read the SSH user's %s on the host: unexpected output %q", id.key, v)
		}
		for _, named := range q[id.key] {
			if named != v {
				return "", fmt.Errorf("the nfs destination names %s %q, not the SSH user's %s", id.key, named, v)
			}
		}
		ids[id.key] = v
	}
	return base + "?uid=" + ids["uid"] + "&gid=" + ids["gid"], nil
}
