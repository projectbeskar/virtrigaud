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
	"encoding/xml"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"
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
//   - root opens the source in the format its domain definition names (-f;
//     the exports' historical -f qcow2): root never probes a disk's format, so
//     a guest cannot make a raw disk it wrote read as a qcow2 image that names
//     a host file as its backing file;
//   - before root opens it, the source's image chain is read one image at a
//     time, as the disk in-use check reads it (walkBackingChainFrom: local
//     regular files only, never a protocol or json: name), and every backing
//     file must be named with its format; otherwise the copy is not run as
//     root (privilegedCopyRefusal);
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
//   - when the chain cannot be verified, or sudo refuses (no passwordless rule
//     for the command, or no sudo at all), the historical command runs as the
//     SSH user, unchanged: a disk it can read is copied exactly as before, and
//     a 0600 overlay fails as before.
//
// The sudoers entries this needs are documented in docs/libvirt-clones.md.

// sudoNonInteractive is sudo's -n: refuse at once, never prompt, when no
// passwordless rule allows the command.
const sudoNonInteractive = "-n"

// privilegedSourceFormats are the source formats a copy opens as root: the
// formats of the VM disks VirtRigaud and libvirt create. Any other format is
// copied as the SSH user, as before.
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
	// src is the VM disk the copy reads, and srcFormat the format the
	// privileged copy opens it as ("" when it is not known: the copy then
	// runs as the SSH user).
	src, srcFormat string
	// target is the host file the copy writes, for the clustered guard's
	// symbolic-link check ("" when it writes none).
	target string
	// output is a local file the privileged copy writes, created by the SSH
	// user under umask before root writes into it ("" for none: the output
	// already exists, or is not a local file).
	output string
	// umask is the umask the copy runs under ("" for none).
	umask string
	// args is the historical qemu-img argv (after "qemu-img"), run as the SSH
	// user when the copy does not run as root.
	args []string
	// privArgs is the qemu-img argv the privileged copy runs; nil means the
	// copy never runs as root.
	privArgs []string
}

// run runs the copy on the host behind h, under guard (nil on a single host):
// as root through passwordless sudo when privilegedCopyRefusal allows it and
// sudo does, as the SSH user (the historical command) otherwise.
func (c diskCopy) run(ctx context.Context, h hostCommandRunner, guard *hostCmdGuard) (*VirshResult, error) {
	if c.privArgs == nil {
		return c.runAsSSHUser(ctx, h, guard)
	}
	if reason := privilegedCopyRefusal(ctx, h, c.src, c.srcFormat); reason != "" {
		log.Printf("WARN Copying %s as the provider's SSH user, not as root: %s", c.src, reason)
		return c.runAsSSHUser(ctx, h, guard)
	}
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
	if err != nil && sudoRefused(res) {
		log.Printf("INFO Passwordless sudo is not allowed for the copy of %s (%s); copying it as the provider's SSH user, "+
			"which fails for a disk it cannot read, such as the 0600 overlay of a VM with an external snapshot "+
			"(see docs/libvirt-clones.md)", c.src, strings.TrimSpace(res.Stderr))
		return c.runAsSSHUser(ctx, h, guard)
	}
	return res, err
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

// runAsSSHUser runs the historical copy as the SSH user, under guard.
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

// privilegedCopyRefusal says why src must not be read by qemu-img as root in
// format, or "" when it may: the format is one root opens
// (privilegedSourceFormats), and its image chain, read one image at a time
// with the disk pinned to format, is local regular files each opened in the
// format its parent names — qemu-img, following the same chain as root, then
// never probes an image, and never opens a protocol, json: or relative
// backing name, a device or a FIFO.
func privilegedCopyRefusal(ctx context.Context, h hostCommandRunner, src, format string) string {
	if !privilegedSourceFormats[format] {
		return fmt.Sprintf("its format %q is not one a copy opens as root", format)
	}
	levels, err := walkBackingChainFrom(ctx, h, src, format)
	if err != nil {
		return fmt.Sprintf("its image chain could not be verified: %v", err)
	}
	if len(levels) == 0 {
		return "it does not exist"
	}
	for _, l := range levels {
		if l.format == "" {
			return fmt.Sprintf("the image chain names %s without its format", l.path)
		}
	}
	return ""
}

// cloneSourceDiskDoc is the part of a domain definition cloneSourceFormat
// reads: each top-level disk's type, source file and driver format.
type cloneSourceDiskDoc struct {
	XMLName xml.Name `xml:"domain"`
	Disks   []struct {
		Type   string `xml:"type,attr"`
		Driver *struct {
			Type string `xml:"type,attr"`
		} `xml:"driver"`
		Source *struct {
			File string `xml:"file,attr"`
		} `xml:"source"`
	} `xml:"devices>disk"`
}

// cloneSourceFormat returns the format domainXML opens the file-backed disk
// diskPath in — its <driver type=...>, libvirt's raw when it names none — or ""
// when the definition cannot be read or has no such disk.
func cloneSourceFormat(domainXML, diskPath string) string {
	var doc cloneSourceDiskDoc
	if err := xml.Unmarshal([]byte(domainXML), &doc); err != nil {
		return ""
	}
	for _, d := range doc.Disks {
		if d.Type != diskTypeFile || d.Source == nil || d.Source.File != diskPath {
			continue
		}
		if d.Driver != nil && strings.TrimSpace(d.Driver.Type) != "" {
			return strings.TrimSpace(d.Driver.Type)
		}
		return libvirtDefaultDiskFormat
	}
	return ""
}

// fullCloneCopy is a full clone's copy of srcDiskPath (opened as srcFormat by
// the privileged copy) into out, a file in the clone's private write
// directory, for the disk targetDiskPath: a standalone qcow2 that flattens the
// source's backing chain, created with vmDiskMode.
func fullCloneCopy(srcDiskPath, srcFormat, out, targetDiskPath string) diskCopy {
	// The historical command lets qemu-img probe the source's format (a
	// guessed format would break the copy); the privileged one pins it.
	c := diskCopy{
		src:       srcDiskPath,
		srcFormat: srcFormat,
		target:    targetDiskPath,
		output:    out,
		umask:     vmDiskUmask,
		args:      []string{"convert", "-O", "qcow2", srcDiskPath, out},
	}
	if srcFormat != "" {
		c.privArgs = []string{"convert", "-f", srcFormat, "-O", "qcow2", srcDiskPath, out}
	}
	return c
}

// posixIDRE is the shape of a uid or gid `id` prints.
var posixIDRE = regexp.MustCompile(`^[0-9]{1,10}$`)

// nfsURLWithHostIdentity returns nfsURL with the SSH user's uid and gid on the
// host behind h (`id -u`, `id -g`) as its libnfs uid and gid, unless the URL
// already names them. qemu-img run as root would otherwise reach the NFS
// server as uid 0 (squashed to the anonymous user by a root_squash export),
// not as the identity the export has always used: libnfs sends the uid and
// gid of the process by default.
func nfsURLWithHostIdentity(ctx context.Context, h hostCommandRunner, nfsURL string) (string, error) {
	u, err := url.Parse(nfsURL)
	if err != nil {
		return "", fmt.Errorf("parse the nfs destination: %w", err)
	}
	q := u.Query()
	var add []string
	for _, id := range []struct{ key, flag string }{{"uid", "-u"}, {"gid", "-g"}} {
		if q.Has(id.key) {
			continue
		}
		res, err := runHost(ctx, h, "id", id.flag)
		if err != nil {
			return "", fmt.Errorf("read the SSH user's %s on the host: %w", id.key, err)
		}
		v := strings.TrimSpace(res.Stdout)
		if !posixIDRE.MatchString(v) {
			return "", fmt.Errorf("read the SSH user's %s on the host: unexpected output %q", id.key, v)
		}
		add = append(add, id.key+"="+v)
	}
	if len(add) == 0 {
		return nfsURL, nil
	}
	sep := "?"
	if strings.Contains(nfsURL, "?") {
		sep = "&"
	}
	return nfsURL + sep + strings.Join(add, "&"), nil
}
