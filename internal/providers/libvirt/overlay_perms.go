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
	"log"
	"path/filepath"
	"strconv"
	"strings"
)

// Snapshot overlay permissions (V7, v0.4.0-rc7 lab validation).
//
// libvirt creates the overlay of an external (disk-only) snapshot, and the new
// overlay a revert to one makes, with mode 0600: owned by root (with the SSH
// user's or root's group) while the domain is shut off, and by the qemu user
// while it runs — libvirt's dynamic ownership gives a file back to the owner
// it had before the domain started once the domain stops. VirtRigaud's own VM
// disks are vmDiskMode (0640) and qemuFileOwner, so the provider's SSH user
// can read them as a member of the qemu group: what the disk in-use checks of
// every OTHER VM on the host rely on when the host gives the SSH user no sudo
// rule for qemu-img (VR_DISK_READ). An overlay that user cannot read fails
// those checks (unreadableImageError: "permission denied reading a disk image",
// retried until it is fixed).
//
// After every successful SnapshotCreate and SnapshotRevert the provider
// therefore gives the domain's OWN overlays the policy of its own disks
// (normalizeOwnOverlays) — every own overlay of its current chain, so an
// overlay an earlier snapshot, revert or release left 0600 is repaired by the
// domain's next snapshot or revert:
//
//   - only files the domain's current definition names as a disk or as a
//     nested backing store, named as libvirt names the domain's own overlays
//     (<domain>-disk.<snapshot> or <domain>-disk.<revert time>), from the top
//     of the chain down to — never including — the domain's own disk
//     (ownOverlayChain); nothing below it (a base image, another VM's disk) is
//     ever touched;
//   - only a regular file, not a symbolic link, directly inside the storage
//     pool's directory or an allowed image directory (canonicalized on the
//     host, as Delete confines its removals), in a directory no non-root
//     account other than the SSH user can write unless it is sticky
//     (unsafeDiskDirReason, the rule the root copies follow): the root chmod
//     below follows a symbolic link, so no other account may be able to swap
//     one in between the check and the chmod;
//   - a SHUT-OFF domain's overlay is given to qemuFileOwner with `sudo -n
//     chown -h` (never follows a link), then group read with `sudo -n chmod
//     g+r`; a RUNNING (or paused) domain's overlay only gets group read, and
//     only when it already belongs to qemuFileOwner. Nothing is ever taken
//     away from the owner: the running qemu keeps its access. When that
//     domain stops, libvirt may give the overlay back to root; the domain's
//     next snapshot or revert taken while it is shut off normalises it again.
//
// It is best-effort and never fails the snapshot or revert that succeeded: a
// file it cannot normalise (no sudo rule for chown/chmod, a root-squashed
// export, an unsafe directory) is logged with the file and the reason —
// provider log only, as every host path — and the next disk in-use check
// that cannot read it says so in its answer (unreadableImageError). The
// manual repair is documented at unreadableImageDoc.

// overlayGroupRead is the only mode change normalizeOwnOverlays makes: group
// read is added, nothing is removed.
const overlayGroupRead = "g+r"

// overlayGroupReadBit is the group-read permission bit.
const overlayGroupReadBit = 0o040

// What overlayStatScript prints for a path that is not a regular file.
const (
	overlayKindSymlink = "symlink"
	overlayKindAbsent  = "absent"
	overlayKindOther   = "other"
)

// overlayStatScript is the fixed `sh -c` script normalizeOverlay runs as the
// SSH user before any privileged command: for "$1" (an overlay) in directory
// "$2" it prints overlayKindSymlink, overlayKindAbsent or overlayKindOther
// when "$1" is not a regular file; otherwise the file's octal mode and
// owner:group (never following a link), then the directory's octal mode and
// owner uid (followed, as diskDirModeScript), then the SSH user's uid — one
// per line.
const overlayStatScript = `if [ -L "$1" ]; then echo ` + overlayKindSymlink + `; elif [ ! -e "$1" ]; then echo ` +
	overlayKindAbsent + `; elif [ ! -f "$1" ]; then echo ` + overlayKindOther +
	`; else stat -c '%a %U:%G' -- "$1" && stat -L -c '%a %u' -- "$2" && id -u; fi`

// overlayDirsFunc returns the directories, on the host behind h, that a
// domain's own overlay may be normalised in (Provider.storageDirsOn); nil
// normalises nothing.
type overlayDirsFunc func(ctx context.Context, h hostCommandRunner) []string

// overlayDirsOf returns backend's overlayDirsFunc (Provider.storageDirsOn),
// or nil for a backend without one.
func overlayDirsOf(backend providerBackend) overlayDirsFunc {
	if b, ok := backend.(interface {
		storageDirsOn(ctx context.Context, h hostCommandRunner) []string
	}); ok {
		return b.storageDirsOn
	}
	return nil
}

// overlayChainDoc is what normalizeOwnOverlays reads of a domain definition:
// whether it runs, its name, and each disk's chain as the definition lists it.
type overlayChainDoc struct {
	XMLName xml.Name `xml:"domain"`
	// ID is the running domain's id; an inactive definition has none.
	ID      string `xml:"id,attr"`
	Name    string `xml:"name"`
	Devices struct {
		Disks []overlayChainLevel `xml:"disk"`
	} `xml:"devices"`
}

// overlayChainLevel is a <disk>, or a <backingStore> nested in one, with its
// own file source and the backing store nested in it.
type overlayChainLevel struct {
	// Device is the <disk>'s device ("disk", "cdrom", ...); empty for a
	// backing store.
	Device string `xml:"device,attr"`
	Source *struct {
		File string `xml:"file,attr"`
	} `xml:"source"`
	BackingStore *overlayChainLevel `xml:"backingStore"`
}

// live reports whether the definition is a running (or paused) domain's.
func (d *overlayChainDoc) live() bool {
	id := strings.TrimSpace(d.ID)
	return id != "" && id != "-1"
}

// ownOverlayChain returns the domain's own overlays its definition lists, top
// first, per disk (device "disk"): from the disk's source down its nested
// backing stores while each is named <domain>-disk.<snapshot> or
// <domain>-disk.<revert time>, stopping at the domain's own disk
// (<domain>-disk.qcow2, or <domain>-disk) — which is not returned — or at
// the first other name, below which nothing is the domain's own (as
// ownChainMembers).
func (d *overlayChainDoc) ownOverlayChain() []string {
	if d.Name == "" {
		return nil
	}
	stem := vmDiskVolumeName(d.Name)
	seen := map[string]bool{}
	var out []string
	for i := range d.Devices.Disks {
		disk := &d.Devices.Disks[i]
		if disk.Device != "" && disk.Device != "disk" {
			continue
		}
		for l := disk; l != nil && l.Source != nil && strings.TrimSpace(l.Source.File) != ""; l = l.BackingStore {
			file := strings.TrimSpace(l.Source.File)
			name := filepath.Base(file)
			if name == stem+qcow2Ext || name == stem {
				break
			}
			rest, ok := strings.CutPrefix(name, stem+".")
			if !ok || !snapshotOverlaySuffixRE.MatchString(rest) {
				break
			}
			if !seen[file] {
				seen[file] = true
				out = append(out, file)
			}
		}
	}
	return out
}

// normalizeOwnOverlays gives the own overlays of domain (a name or UUID) on
// the host behind h the mode and owner of VirtRigaud's own VM disks, under
// the rules of the file comment, and returns how many of them it could not
// normalise (logged; 0 also when there is nothing to do). dirsOf names the
// directories they may be in; nil normalises nothing. It never fails.
func normalizeOwnOverlays(ctx context.Context, h hostCommandRunner, domain string, dirsOf overlayDirsFunc) int {
	if dirsOf == nil {
		return 0
	}
	res, err := h.runVirshCommand(ctx, "dumpxml", domain)
	if err != nil {
		log.Printf("WARN Could not read domain %s to give its snapshot overlays the mode of a VM disk: %v (see %s)",
			domain, err, unreadableImageDoc)
		return 1
	}
	var doc overlayChainDoc
	if err := xml.Unmarshal([]byte(res.Stdout), &doc); err != nil {
		log.Printf("WARN Could not parse domain %s to give its snapshot overlays the mode of a VM disk: %v (see %s)",
			domain, err, unreadableImageDoc)
		return 1
	}
	overlays := doc.ownOverlayChain()
	if len(overlays) == 0 {
		return 0
	}
	dirs := dirsOf(ctx, h)
	if len(dirs) == 0 {
		log.Printf("WARN Not normalising the snapshot overlays %v of domain %s: no storage directory is known (see %s)",
			overlays, domain, unreadableImageDoc)
		return len(overlays)
	}
	canon, err := canonicalizeOnHost(ctx, h, append(append([]string(nil), overlays...), dirs...))
	if err != nil {
		log.Printf("WARN Not normalising the snapshot overlays %v of domain %s: %v (see %s)",
			overlays, domain, err, unreadableImageDoc)
		return len(overlays)
	}
	allowed := map[string]bool{}
	for _, d := range canon[len(overlays):] {
		if !isForbiddenImageDir(d) {
			allowed[d] = true
		}
	}
	failed := 0
	for i, o := range overlays {
		c := canon[i]
		if !allowed[filepath.Dir(c)] {
			log.Printf("WARN Not normalising snapshot overlay %s of domain %s: it is not a file directly inside the storage "+
				"pool directory or an allowed image directory %v (see %s)", o, domain, dirs, unreadableImageDoc)
			failed++
			continue
		}
		if !normalizeOverlay(ctx, h, domain, c, doc.live()) {
			failed++
		}
	}
	return failed
}

// normalizeOverlay normalises one own overlay, path (canonical, inside an
// allowed directory), of domain under the rules of the file comment, and
// reports whether it has (or already had) its mode and owner — or, for a
// live domain, is left as the rules require.
func normalizeOverlay(ctx context.Context, h hostCommandRunner, domain, path string, live bool) bool {
	warn := func(format string, args ...any) bool {
		log.Printf("WARN Not normalising snapshot overlay %s of domain %s: "+format+" (see %s)",
			append(append([]any{path, domain}, args...), unreadableImageDoc)...)
		return false
	}
	res, err := runHost(ctx, h, "sh", "-c", overlayStatScript, "sh", path, filepath.Dir(path))
	if err != nil {
		return warn("it could not be checked: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	switch lines[0] {
	case overlayKindSymlink:
		return warn("it is a symbolic link")
	case overlayKindAbsent:
		return true // gone: nothing for any check to read
	case overlayKindOther:
		return warn("it is not a regular file")
	}
	fields := strings.Fields(lines[0])
	if len(lines) != 3 || len(fields) != 2 {
		return warn("unexpected check output %q", res.Stdout)
	}
	mode, err := strconv.ParseUint(fields[0], 8, 32)
	if err != nil {
		return warn("unexpected mode %q", fields[0])
	}
	owner := fields[1]
	needChown := owner != qemuFileOwner
	needChmod := mode&overlayGroupReadBit == 0
	if live && needChown {
		// The running qemu's file: never given away under it.
		log.Printf("INFO Snapshot overlay %s of running domain %s belongs to %s, not %s: left as is; it is normalised by "+
			"the domain's next snapshot or revert taken while it is shut off", path, domain, owner, qemuFileOwner)
		return true
	}
	if !needChown && !needChmod {
		return true
	}
	if reason := unsafeDiskDirReason(lines[1] + " " + lines[2]); reason != "" {
		return warn("its directory %s and is not sticky, so a root chmod could be made to follow a symbolic link", reason)
	}
	if needChown {
		if _, err := runHost(ctx, h, "sudo", "-n", "chown", "-h", qemuFileOwner, "--", path); err != nil {
			return warn("sudo -n chown -h %s failed: %v", qemuFileOwner, err)
		}
	}
	if needChmod {
		if _, err := runHost(ctx, h, "sudo", "-n", "chmod", overlayGroupRead, "--", path); err != nil {
			return warn("sudo -n chmod %s failed: %v", overlayGroupRead, err)
		}
	}
	log.Printf("INFO Snapshot overlay %s of domain %s now belongs to %s with group read (it was %s, mode %s)",
		path, domain, qemuFileOwner, owner, fields[0])
	return true
}
