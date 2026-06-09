/*
Copyright 2025.

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
	"strconv"
	"strings"
)

// bytesPerGiB is the number of bytes in one GiB, used to convert the
// provider-agnostic GiB disk-size contract into byte-accurate comparisons
// against virsh's domblkinfo Capacity (reported in bytes).
const bytesPerGiB = int64(1024) * 1024 * 1024

// growThresholdBytes is the minimum delta (in bytes) between the desired and
// current block-device capacity that justifies a live blockresize. virsh and
// qcow2 round capacities to alignment boundaries, so a sub-threshold positive
// delta is treated as "already at target" to keep the operation idempotent and
// avoid no-op blockresize churn on every reconcile.
const growThresholdBytes = int64(1024) * 1024 // 1 MiB

// parseDomblklistPrimaryTarget extracts the primary (boot) disk's target device
// name (e.g. "vda") from `virsh domblklist <dom>` output. It skips the header,
// separator, and any non-primary devices (cloud-init ISOs, CD-ROMs) using the
// same heuristics as getDomainDiskPaths so the result is naming-convention
// agnostic — it relies on the live domain's real block topology, NOT on the
// fragile "<vmid>-disk" volume-name guess that bit the clone full-path bug
// (#207).
//
// domblklist output looks like:
//
//	Target   Source
//	---------------------------------------------
//	vda      /var/lib/libvirt/images/vm-foo.qcow2
//	hda      /var/lib/libvirt/images/vm-foo-cidata.iso
//
// The first row whose source is a real disk (not a cloud-init/cdrom ISO) wins.
func parseDomblklistPrimaryTarget(domblklistStdout string) (string, error) {
	lines := strings.Split(strings.TrimSpace(domblklistStdout), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Skip the header row and the dashed separator.
		if i == 0 || strings.HasPrefix(trimmed, "-") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 1 {
			continue
		}
		target := fields[0]
		source := ""
		if len(fields) >= 2 {
			source = fields[1]
		}
		// Skip devices with no backing source (e.g. an empty CD-ROM shows "-").
		if source == "" || source == "-" {
			continue
		}
		// Skip cloud-init ISOs / CD-ROMs; mirror getDomainDiskPaths.
		if strings.HasSuffix(source, "-cidata.iso") || strings.HasSuffix(source, "cloud-init.iso") {
			continue
		}
		return target, nil
	}
	return "", fmt.Errorf("no primary disk target found in domblklist output")
}

// parseDomblkinfoCapacity extracts the "Capacity:" value (in bytes) from
// `virsh domblkinfo <dom> <target>` output. domblkinfo reports:
//
//	Capacity:       10737418240
//	Allocation:     2147483648
//	Physical:       2147483648
//
// Capacity is the virtual (guest-visible) block size, which is exactly what a
// grow-only guard must compare against.
func parseDomblkinfoCapacity(domblkinfoStdout string) (int64, error) {
	lines := strings.Split(domblkinfoStdout, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "Capacity:") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			return 0, fmt.Errorf("malformed Capacity line in domblkinfo: %q", trimmed)
		}
		capBytes, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse domblkinfo Capacity %q: %w", fields[1], err)
		}
		return capBytes, nil
	}
	return 0, fmt.Errorf("no Capacity line found in domblkinfo output")
}

// blockresizeSizeArg formats a desired GiB size into the size argument virsh
// `blockresize` expects. The file's existing convention (ResizeVolume's
// vol-resize) passes a scaled "<n>G" value, and blockresize accepts the same
// scaled-suffix form, so we mirror it for consistency. The unsuffixed default
// for blockresize would be KiB, which is why an explicit "G" suffix is
// mandatory here.
func blockresizeSizeArg(desiredGiB int) string {
	return fmt.Sprintf("%dG", desiredGiB)
}

// shouldGrowDisk implements the grow-only + idempotency guard. It returns true
// only when the desired capacity exceeds the current capacity by more than the
// rounding threshold. libvirt/qcow2 cannot shrink a live block device, so any
// desired ≤ current request is reported as a no-op (false) and must be skipped
// by the caller — never attempted as a shrink.
func shouldGrowDisk(currentBytes, desiredBytes int64) bool {
	return desiredBytes-currentBytes > growThresholdBytes
}

// fsGrowCommands returns the best-effort, in-guest filesystem-grow shell
// commands to run via the guest agent after a live blockresize, given the
// guest's primary block target (e.g. "vda"). The block device is already
// larger at the QEMU level; these commands extend the partition table and then
// the filesystem so the guest actually sees the new space without a reboot.
//
// Sequence:
//  1. growpart /dev/<target> 1 — grow partition 1 to fill the enlarged device
//     (from cloud-guest-utils; the canonical online partition-grow tool).
//  2. resize2fs /dev/<target>1 — grow an ext2/3/4 filesystem online.
//  3. xfs_growfs / — grow an XFS filesystem (XFS grows by mountpoint, not
//     device); "/" is the overwhelmingly common root mountpoint for the boot
//     disk this provider manages.
//
// We return all three because cheaply detecting the FS type inside the guest is
// not always reliable; each command is harmless on a mismatched FS (resize2fs
// refuses a non-ext volume, xfs_growfs refuses a non-XFS mount) and the whole
// sequence is best-effort: any failure is logged WARN and does NOT fail the
// reconfigure. The "1" partition assumption matches standard cloud images
// (single root partition); exotic layouts simply fall back to user/cloud-init
// completion, which is the documented #201 caveat.
func fsGrowCommands(target string) []string {
	dev := "/dev/" + target
	return []string{
		fmt.Sprintf("growpart %s 1", dev),
		fmt.Sprintf("resize2fs %s1", dev),
		"xfs_growfs /",
	}
}

// growDiskOnline grows a running domain's primary disk live. It is the online
// branch of Reconfigure's disk handling (#201):
//
//  1. Resolve the primary disk's target device and current capacity from the
//     live domain (domblklist + domblkinfo) — NOT from the volume-name guess.
//  2. Apply the grow-only / idempotency guard.
//  3. Resize the backing qcow2 volume so the larger size persists across reboot.
//  4. `virsh blockresize` so live QEMU sees the new block size immediately.
//  5. Best-effort in-guest filesystem grow via the guest agent (non-fatal).
//
// A failure in steps 1–4 is fatal to the disk step and returned to the caller.
// Step 5 never fails the operation. It returns true when the live block device
// was actually grown (so the caller can record a change), false when the
// request was a no-op.
func (p *Provider) growDiskOnline(ctx context.Context, id string, desiredDiskGB int, sp *StorageProvider) (grew bool, err error) {
	// Resolve the primary disk target from the live domain topology.
	blkResult, err := p.virshProvider.runVirshCommand(ctx, "domblklist", id)
	if err != nil {
		return false, fmt.Errorf("list block devices for domain %s: %w", id, err)
	}
	target, err := parseDomblklistPrimaryTarget(blkResult.Stdout)
	if err != nil {
		return false, fmt.Errorf("resolve primary disk target for domain %s: %w", id, err)
	}

	// Read the current virtual capacity (bytes) for the grow-only guard.
	infoResult, err := p.virshProvider.runVirshCommand(ctx, "domblkinfo", id, target)
	if err != nil {
		return false, fmt.Errorf("read block info for domain %s target %s: %w", id, target, err)
	}
	currentBytes, err := parseDomblkinfoCapacity(infoResult.Stdout)
	if err != nil {
		return false, fmt.Errorf("parse current capacity for domain %s target %s: %w", id, target, err)
	}

	desiredBytes := int64(desiredDiskGB) * bytesPerGiB
	if !shouldGrowDisk(currentBytes, desiredBytes) {
		// desired ≤ current (or within rounding): no-op. Never shrink live.
		log.Printf("INFO Disk grow skipped for domain %s target %s: desired %d bytes ≤ current %d bytes (grow-only)",
			id, target, desiredBytes, currentBytes)
		return false, nil
	}

	log.Printf("INFO Growing disk for running domain %s target %s: %d bytes -> %d bytes (%dGB)",
		id, target, currentBytes, desiredBytes, desiredDiskGB)

	// Persist the larger size to the backing volume first so the size survives a
	// reboot and the qcow2 file is large enough for blockresize to expose. This
	// uses the existing best-effort volume convention; if it fails we still try
	// blockresize, but log the volume failure. ResizeVolume is grow-only-safe
	// for files (qemu-img/vol-resize grows the file).
	if verr := sp.ResizeVolume(ctx, clonePoolName, fmt.Sprintf("%s-disk", id), desiredDiskGB); verr != nil {
		log.Printf("WARN Backing volume resize for domain %s did not apply (continuing to blockresize): %v", id, verr)
	}

	// Grow the live block device so QEMU exposes the new size to the guest.
	if _, rerr := p.virshProvider.runVirshCommand(ctx, "blockresize", id, target, blockresizeSizeArg(desiredDiskGB)); rerr != nil {
		return false, fmt.Errorf("blockresize domain %s target %s to %dGB: %w", id, target, desiredDiskGB, rerr)
	}
	log.Printf("INFO Successfully grew live block device for domain %s target %s to %dGB", id, target, desiredDiskGB)

	// Best-effort in-guest filesystem grow. Non-fatal: the block device is
	// already larger, and cloud-init / a user can finish the FS grow. Gated on
	// guest-agent availability (#201).
	p.growGuestFilesystemBestEffort(ctx, id, target)

	return true, nil
}

// growGuestFilesystemBestEffort attempts to extend the in-guest partition and
// filesystem after a live blockresize. It is entirely best-effort: it requires
// the QEMU guest agent, and every command failure is logged WARN and swallowed
// so it never fails the surrounding Reconfigure. This is the documented #201
// caveat — without the guest agent (or for exotic partition layouts) the
// operator must finish the FS grow via cloud-init or manually.
func (p *Provider) growGuestFilesystemBestEffort(ctx context.Context, id, target string) {
	ga := NewGuestAgentProvider(p.virshProvider)
	if !ga.isGuestAgentAvailable(ctx, id) {
		log.Printf("WARN In-guest filesystem grow skipped for domain %s: guest agent unavailable; "+
			"block device is grown but the guest filesystem must be extended via cloud-init or manually (#201)", id)
		return
	}

	for _, cmd := range fsGrowCommands(target) {
		out, err := ga.ExecuteGuestCommand(ctx, id, cmd)
		if err != nil {
			log.Printf("WARN In-guest filesystem grow step %q failed for domain %s (non-fatal): %v", cmd, id, err)
			continue
		}
		log.Printf("INFO In-guest filesystem grow step %q succeeded for domain %s: %s", cmd, id, strings.TrimSpace(out))
	}
	log.Printf("INFO In-guest filesystem grow (best-effort) completed for domain %s", id)
}
