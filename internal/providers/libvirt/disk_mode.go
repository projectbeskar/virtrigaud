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
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Modes and ownership of the files VirtRigaud creates for a VM.
//
// A file's final mode is set when it is CREATED, never by a later chmod, and
// its owner is changed with `chown -h`, which changes a symbolic link itself
// and never its target. Between creating a file and a privileged
// `chmod`/`chown <path>`, anyone who can write the directory can replace the
// file with a symbolic link, and root then follows it: `/etc/shadow` would
// become libvirt-qemu:kvm, group-readable. So:
//
//   - a disk that qemu-img (or cat) writes is created under vmDiskUmask
//     (withUmask): mode vmDiskMode;
//   - a blank disk that libvirt creates (vol-create) carries
//     <permissions><mode>vmDiskMode</mode> (volCreateXML);
//   - a clone's UEFI varstore is created by dd under clonedNVRAMUmask: 0600;
//   - ownership then goes to the qemu user with chownToQemu.
//
// A disk is written inside a private directory next to its final name and
// then renamed onto it (diskWriteDir), so no other account can plant a
// symbolic link the writer would follow, or swap the file while it is written;
// a symbolic link already at the final name is refused (ensureDiskTargetFree)
// and would in any case be replaced, never followed, by the rename. The
// provider still warns once per host and directory when a non-root account
// other than its own can write a directory it creates VM files in and the
// directory is not sticky (warnIfDiskDirUnsafe): that account can replace a
// finished disk. A root SSH user is not supported on such a directory.

// vmDiskMode is the mode of every VM disk VirtRigaud creates: read-write for the
// owner (the qemu user, after chownToQemu), read-only for its group (kvm), and
// nothing for anyone else. The provider's SSH user reads VM disks (the disk
// in-use checks, GetDiskInfo, s3/nfs exports, a full clone's copy) as a member
// of kvm, or as root; the disk in-use checks and the copies (a full clone's,
// the s3/nfs exports' flatten) read through passwordless `sudo -n` where the
// host allows it, which reaches libvirt's 0600 snapshot overlays too
// (privileged_copy.go); nothing writes a VM disk through the group. qemu-img
// creates files 0644 at most, so this is the most a create-time mode can give.
// Disks created by an earlier release keep their mode. Least privilege (0600,
// every read through `sudo -n`) is a tracked follow-up.
const vmDiskMode = "0640"

// vmDiskUmask is the umask a VM disk is created under: 0666 or 0644 & ^0137 =
// vmDiskMode.
const vmDiskUmask = "0137"

// clonedNVRAMUmask is the umask a clone's UEFI varstore is created under
// (0666 & ^0177 = 0600): private to its owner, the qemu user — the mode
// libvirt gives the varstores it creates.
const clonedNVRAMUmask = "0177"

// qemuFileOwner is the owner:group VirtRigaud gives a VM's disks and varstore
// (Debian/Ubuntu's qemu user and group). Elsewhere the chown fails, is logged,
// and libvirt's dynamic ownership gives the file to its qemu user at start.
const qemuFileOwner = "libvirt-qemu:kvm"

// umaskExecScript is the fixed `sh -c` script behind withUmask: "$1" is the
// umask, the rest is the command, exec'd as given (never interpolated into
// the script text).
const umaskExecScript = `umask "$1" && shift && exec "$@"`

// withUmask returns the host argv that runs argv with umask umask, so the
// files it creates get their final mode at creation. A `sudo` in argv keeps
// the umask (sudo applies the union of the caller's umask and its own).
func withUmask(umask string, argv ...string) []string {
	return append([]string{"sh", "-c", umaskExecScript, "sh", umask}, argv...)
}

// writeVMDiskScript is writeStdinToFileScript for a VM disk: the file is
// created with vmDiskMode. The path is "$1".
const writeVMDiskScript = `umask ` + vmDiskUmask + ` && cat > "$1"`

// writeVMDiskArgv returns the argv that copies the command's stdin to the VM
// disk path on the host, creating it with vmDiskMode (writeVMDiskScript).
func writeVMDiskArgv(path string) []string {
	return []string{"sh", "-c", writeVMDiskScript, "sh", path}
}

// chownToQemu gives path on vp's host to the qemu user (qemuFileOwner) with
// `sudo chown -h`: a symbolic link swapped in for the file has only its own
// ownership changed, never its target's. It first checks the permissions of
// path's directory (warnIfDiskDirUnsafe). The caller logs a failure: it is
// best-effort, as hosts differ.
func chownToQemu(ctx context.Context, vp *VirshProvider, path string) error {
	vp.warnIfDiskDirUnsafe(ctx, filepath.Dir(path))
	_, err := runHost(ctx, vp, "sudo", "chown", "-h", qemuFileOwner, "--", path)
	return err
}

// vmDiskWriteDirPrefix starts the name of the private directory a VM disk is
// written in (diskWriteDir): a dotfile, so image confinement never takes it
// for an image and a pool listing hides it.
const vmDiskWriteDirPrefix = ".virtrigaud-write-"

// diskWriteDir is a private directory made next to a VM disk that is being
// written: `mktemp -d` in the disk's own directory — an unpredictable name,
// mode 0700, owned by the provider's SSH user — so qemu-img or cat create the
// disk (and any staged input) where no other non-root account can plant a
// symbolic link for the writer to follow, or swap the file while it is
// written. The finished disk is renamed onto its final name (publish): a
// rename(2) in the same directory, which replaces whatever is at the name — a
// stale file, or a symbolic link planted there — and never follows it.
type diskWriteDir struct {
	h   hostCommandRunner
	dir string
}

// newDiskWriteDir makes a diskWriteDir in parent on the host behind h.
func newDiskWriteDir(ctx context.Context, h hostCommandRunner, parent string) (*diskWriteDir, error) {
	dir, err := makeHostTemp(ctx, h, filepath.Join(parent, vmDiskWriteDirPrefix+mktempTemplateSuffix), true)
	if err != nil {
		return nil, fmt.Errorf("create a private directory for the disk: %w", err)
	}
	return &diskWriteDir{h: h, dir: dir}, nil
}

// file returns the path of name inside the directory.
func (d *diskWriteDir) file(name string) string {
	return filepath.Join(d.dir, name)
}

// publish renames name (inside the directory) onto final with `mv -f -T`:
// whatever is at final is replaced by the rename, never followed, and a
// directory there makes it fail.
func (d *diskWriteDir) publish(ctx context.Context, name, final string) error {
	if _, err := runHost(ctx, d.h, "mv", "-f", "-T", "--", d.file(name), final); err != nil {
		return fmt.Errorf("move the new disk into place: %w", err)
	}
	return nil
}

// cleanup removes the directory and anything left in it (a failed write, a
// staged input), even when ctx is cancelled (removeHostPath).
func (d *diskWriteDir) cleanup(ctx context.Context) {
	removeHostPath(ctx, d.h, d.dir, true)
}

// cleanupWithin is cleanup bounded by within instead of the staging default:
// a routed call's cleanup must end before the manager's deadline
// (routedCleanupTimeout).
func (d *diskWriteDir) cleanupWithin(ctx context.Context, within time.Duration) {
	removeHostPathWithin(ctx, d.h, d.dir, true, within)
}

// diskDirModeScript is the fixed `sh -c` script behind warnIfDiskDirUnsafe: it
// prints the directory's octal mode and owner uid ("$1", followed if it is a
// symbolic link), then the SSH user's uid.
const diskDirModeScript = `stat -L -c '%a %u' -- "$1" && id -u`

// Permission bits warnIfDiskDirUnsafe checks.
const (
	dirStickyBit  = 0o1000
	dirOwnerWrite = 0o200
	dirGroupWrite = 0o020
	dirOtherWrite = 0o002
)

// warnIfDiskDirUnsafe logs a warning, once per directory and provider
// process, when dir on vp's host can be written by a non-root account other
// than the provider's SSH user (a non-root owner other than it, its group, or
// everyone) and does not have the sticky bit. Such an account can plant a
// symbolic link at the name of a VM file before it is created; the create-time
// modes and `chown -h` keep root from following one swapped in afterwards.
// It never refuses: existing hosts keep working. A check that cannot run is
// logged and not retried.
func (v *VirshProvider) warnIfDiskDirUnsafe(ctx context.Context, dir string) {
	if _, done := v.checkedDiskDirs.LoadOrStore(dir, true); done {
		return
	}
	res, err := runHost(ctx, v, "sh", "-c", diskDirModeScript, "sh", dir)
	if err != nil {
		log.Printf("WARN Could not check the permissions of %s, where VM disks are created: %v", dir, err)
		return
	}
	if reason := unsafeDiskDirReason(res.Stdout); reason != "" {
		log.Printf("WARN %s, where VM disks are created, %s and is not sticky: that account can plant a symbolic link "+
			"at a VM file's name before it is created. Make it writable only by root and the provider's SSH user "+
			"(e.g. root:root 0755, or owned by the SSH user 0755), or set the sticky bit (chmod +t)", dir, reason)
	}
}

// unsafeDiskDirReason reads diskDirModeScript's output and says who other than
// root and the SSH user can write the directory, or "" when no one can or the
// directory is sticky (or the output cannot be read).
func unsafeDiskDirReason(out string) string {
	lines := strings.Fields(out)
	if len(lines) != 3 {
		return ""
	}
	mode, err := strconv.ParseUint(lines[0], 8, 32)
	if err != nil || mode&dirStickyBit != 0 {
		return ""
	}
	owner, self := lines[1], lines[2]
	var who []string
	if mode&dirOwnerWrite != 0 && owner != "0" && owner != self {
		who = append(who, "its owner (uid "+owner+")")
	}
	if mode&dirGroupWrite != 0 {
		who = append(who, "its group")
	}
	if mode&dirOtherWrite != 0 {
		who = append(who, "every user")
	}
	if len(who) == 0 {
		return ""
	}
	return "is writable by " + strings.Join(who, " and ")
}

// volCreateXML is the definition of a blank qcow2 volume vol-create makes: the
// same volume vol-create-as would, created by libvirt with vmDiskMode.
type volCreateXML struct {
	XMLName  xml.Name `xml:"volume"`
	Name     string   `xml:"name"`
	Capacity struct {
		Unit  string `xml:"unit,attr"`
		Value int    `xml:",chardata"`
	} `xml:"capacity"`
	Target struct {
		Format struct {
			Type string `xml:"type,attr"`
		} `xml:"format"`
		Permissions struct {
			Mode string `xml:"mode"`
		} `xml:"permissions"`
	} `xml:"target"`
}

// blankVolumeXML returns the vol-create definition of a blank volume name of
// sizeGB GiB in format, created with vmDiskMode. Every value is escaped by the
// XML encoder.
func blankVolumeXML(name, format string, sizeGB int) ([]byte, error) {
	var v volCreateXML
	v.Name = name
	v.Capacity.Unit = "G"
	v.Capacity.Value = sizeGB
	v.Target.Format.Type = format
	v.Target.Permissions.Mode = vmDiskMode
	b, err := xml.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode volume %s: %w", name, err)
	}
	return b, nil
}

// runVirshStdin runs `virsh <args>` against v's libvirt with stdin as its
// standard input — ON the host for an SSH connection (pinned to the same
// libvirtd, like runRemoteVirshCommand), locally otherwise. It lets a
// subcommand read a definition from /dev/stdin instead of a host file.
func (v *VirshProvider) runVirshStdin(ctx context.Context, stdin []byte, args ...string) (*VirshResult, error) {
	argv := []string{"virsh"}
	if v.isSSHTransport() {
		if uri := remoteVirshConnectURI(v.uri); uri != "" {
			argv = append(argv, "-c", uri)
		}
	} else {
		argv = append(argv, "-c", v.uri)
	}
	return v.runHostStdin(ctx, stdin, append(argv, args...)...)
}
