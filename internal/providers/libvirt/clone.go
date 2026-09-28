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
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	sdkerrors "github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// linkedClonesDisabledMessage is the refusal of a linked clone (CloneRequest
// with Linked=true). A linked clone's disk is a qcow2 overlay whose backing file
// is the source VM's live disk, and nothing freezes that disk: powering the
// source on while the clone is shut off writes to the file the clone reads and
// corrupts the clone — a hazard the disk dependency guard (disk_dependents.go)
// cannot catch, since no delete or snapshot is involved. Linked clones stay
// disabled until the base is frozen at clone time (an external snapshot of the
// source, or cloning only from an immutable template image). The refusal is an
// sdk InvalidSpec (gRPC InvalidArgument: non-retryable, never counted toward
// the manager's circuit breaker); GetCapabilities reports
// SupportsLinkedClones=false, so the manager refuses the VMClone
// (LinkedCloneUnsupported) before calling Clone at all.
const linkedClonesDisabledMessage = "linked clones are disabled on libvirt in this release: the source disk is not frozen, " +
	"so the source's writes would corrupt the clone; use FullClone"

// clonePoolName is the storage pool used for cloned disks. Clone is an MVP that
// operates within the provider's default pool, mirroring Create/GetDiskInfo
// (issue #153). PlacementJSON-driven pool selection is a follow-up.
const clonePoolName = "default"

// Pre-compiled regexes for the domain-XML rewrite. Compiling once at package
// init keeps Clone allocation-free on the hot path and makes the rewrite logic
// unit-testable in isolation (see clone_test.go).
var (
	// reDomainName matches the top-level <name>...</name> element. The domain
	// name is the first such element in the document; ReplaceAllString with
	// count semantics is not available, so we rely on the rewrite helper to
	// only touch the first match.
	reDomainName = regexp.MustCompile(`(?s)<name>.*?</name>`)
	// reDomainUUID matches the top-level <uuid>...</uuid> element.
	reDomainUUID = regexp.MustCompile(`(?s)<uuid>.*?</uuid>`)
	// reMACAddress matches a <mac address='..'/> element (single or double
	// quotes), capturing nothing — every occurrence is replaced with a fresh
	// address so the clone never collides with the source on the L2 segment.
	reMACAddress = regexp.MustCompile(`<mac\s+address=(?:'[^']*'|"[^"]*")\s*/>`)
	// reNVRAM matches the per-VM UEFI <nvram>...</nvram> varstore element under
	// <os>, capturing the absolute varstore path in group 1. A BIOS domain has
	// no such element, so a non-match means "no nvram to re-point" (issue #208).
	// The (?s) flag lets . span newlines; the path is captured non-greedily.
	reNVRAM = regexp.MustCompile(`(?s)<nvram[^>]*>\s*(.*?)\s*</nvram>`)
)

// Clone clones the source VM identified by req.Source into a new libvirt
// domain. It is the libvirt implementation of the provider-contract Cloner
// capability (issues #153/#179).
//
// The new domain is named by cloneDomainName — "<namespace>.<name>" of the
// VirtualMachine the clone will be bound to (req.TargetVM), the same rule as
// Create, or the bare req.TargetName for an older manager — and that name is
// returned as TargetVmID, which the operator records as the VM's status.id.
// Its disk is "<domain>-disk.qcow2".
//
// Two modes are supported, selected by req.Linked:
//
//   - Full clone (Linked=false): the source's primary disk is copied into an
//     independent qcow2 volume via virsh vol-clone. The clone has no ongoing
//     dependency on the source.
//
//   - Linked clone (Linked=true): a thin qcow2 overlay is created with the
//     source's primary disk as a READ-ONLY backing file
//     (qemu-img create -f qcow2 -b <src> -F qcow2 <overlay>). This is fast and
//     space-efficient, but the clone is lifecycle-bound to the source: the
//     source disk MUST NOT be modified or deleted while the overlay exists, or
//     the clone is corrupted. The manager gates this on SupportsLinkedClones.
//     DISABLED in this release (linkedClonesDisabledMessage): a Linked request
//     is refused before any host command runs; the overlay code below stays
//     for when the base is frozen at clone time.
//
// The target domain is defined with a fresh UUID and fresh MAC address(es) by
// rewriting the source domain's XML, so the two domains never collide. The
// clone is left powered off; the manager controls power separately, matching
// Create's behavior.
func (p *Provider) Clone(ctx context.Context, req contracts.CloneRequest) (contracts.CloneResponse, error) {
	sourceID := req.Source.ID
	log.Printf("INFO Cloning VM %s -> %s (linked=%t)", sourceID, req.TargetName, req.Linked)

	if p.clustered() {
		return p.cloneClustered(ctx, req)
	}

	if p.virshProvider == nil {
		return contracts.CloneResponse{}, contracts.NewRetryableError("virsh provider not initialized", nil)
	}
	if sourceID == "" {
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError("clone source VM ID is required", nil)
	}
	if req.TargetName == "" {
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError("clone target name is required", nil)
	}
	// Linked clones are disabled (see linkedClonesDisabledMessage): refused
	// before any host command runs. Existing linked clones are untouched.
	if req.Linked {
		log.Printf("WARN Refusing linked clone of %s -> %s: linked clones are disabled on libvirt in this release", sourceID, req.TargetName)
		return contracts.CloneResponse{}, sdkerrors.NewInvalidSpec("%s", linkedClonesDisabledMessage)
	}
	// Name the target domain. A (legacy) target name virsh would resolve as a
	// domain ID/UUID would make every later by-name operation on the clone
	// address a different domain, so domainNameFor rejects it.
	domainName, err := cloneDomainName(req)
	if err != nil {
		return contracts.CloneResponse{}, err
	}

	storageProvider := NewStorageProvider(p.virshProvider)

	// 1. Resolve the source domain and reject if it does not exist. domstate
	//    accepts either a domain name or a UUID, matching how libvirt identifies
	//    a VM (Status.ID is the domain name for this provider).
	if _, err := p.virshProvider.getDomainState(ctx, sourceID); err != nil {
		return contracts.CloneResponse{}, contracts.NewNotFoundError(
			fmt.Sprintf("source VM %q not found", sourceID), err)
	}

	// Reject a target-name collision up front rather than failing mid-define:
	// a clone NEVER binds to, or redefines, an existing domain of the target
	// name, whoever owns it. (Should one appear between this check and the
	// define, the define itself fails: the clone carries a fresh random UUID and
	// libvirt refuses a same-named define under a different UUID.)
	domains, err := p.virshProvider.listDomains(ctx)
	if err != nil {
		return contracts.CloneResponse{}, contracts.NewRetryableError("failed to list domains", err)
	}
	for _, d := range domains {
		if d.Name == domainName {
			return contracts.CloneResponse{}, contracts.NewInvalidSpecError(
				fmt.Sprintf("target VM %q already exists (libvirt domain %q)", req.TargetName, domainName), nil)
		}
	}

	// 2. Resolve the source's primary disk path + format.
	srcDiskPath, srcDiskFormat, err := p.resolvePrimaryDisk(ctx, sourceID, storageProvider)
	if err != nil {
		return contracts.CloneResponse{}, err
	}
	if srcDiskFormat == "" {
		srcDiskFormat = "qcow2"
	}

	// 3. Create the target disk in the same pool.
	poolInfo, err := storageProvider.GetPoolInfo(ctx, clonePoolName)
	if err != nil {
		return contracts.CloneResponse{}, fmt.Errorf("get pool %q info: %w", clonePoolName, err)
	}
	targetVolumeName := vmDiskVolumeName(domainName)
	targetDiskPath := filepath.Join(poolInfo.Path, fmt.Sprintf("%s.qcow2", targetVolumeName))
	// Never let the overlay/copy replace a disk another domain uses.
	if err := ensureDiskTargetFree(ctx, p.virshProvider, domainDiskSubject(domainName), targetDiskPath); err != nil {
		return contracts.CloneResponse{}, err
	}

	// 4. Build the target definition by cloning the source XML and rewriting
	//    the identity (name/uuid/mac) and the primary disk source path — before
	//    any file is written, so a refused UEFI varstore path (below) leaves
	//    nothing behind.
	srcXML, err := p.virshProvider.runVirshCommand(ctx, "dumpxml", sourceID)
	if err != nil {
		return contracts.CloneResponse{}, contracts.NewRetryableError("failed to dump source domain XML", err)
	}

	targetXML, srcNvramPath, targetNvramPath, err := rewriteDomainXMLForClone(srcXML.Stdout, domainName, srcDiskPath, targetDiskPath)
	if err != nil {
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError("rewrite source domain XML for clone", err)
	}
	// Never write the clone's varstore through a symlink or over another
	// domain's varstore.
	if srcNvramPath != "" && targetNvramPath != "" {
		if err := ensureNVRAMTargetFree(ctx, p.virshProvider, domainName, targetNvramPath); err != nil {
			return contracts.CloneResponse{}, err
		}
	}

	if req.Linked {
		if err := createLinkedOverlay(ctx, p.virshProvider, srcDiskPath, srcDiskFormat, targetDiskPath); err != nil {
			return contracts.CloneResponse{}, err
		}
	} else {
		if err := createFullCopy(ctx, p.virshProvider, srcDiskPath, targetDiskPath); err != nil {
			return contracts.CloneResponse{}, err
		}
	}

	// For a UEFI source the domain XML carries a per-VM <nvram> varstore that was
	// just re-pointed to a fresh per-clone path. Copy the actual varstore file on
	// the libvirt host so the clone gets an independent set of UEFI variables
	// (issue #208); otherwise two domains would share one varstore. This is the
	// side-effecting counterpart to the pure XML rewrite, performed here next to
	// the disk copy so rewriteDomainXMLForClone stays testable without a host.
	if srcNvramPath != "" && targetNvramPath != "" {
		copyClonedNVRAM(ctx, p.virshProvider, srcNvramPath, targetNvramPath)
	}

	// Apply best-effort CPU/memory overrides from ClassJSON.
	targetXML = applyClassOverrides(targetXML, req.ClassJSON)

	// CustomizeJSON (hostname / cloud-init) is intentionally NOT applied in this
	// MVP: a faithful implementation requires regenerating the nocloud ISO and
	// re-pointing the CD-ROM, which is a follow-up. Log loudly so callers do not
	// mistake a clone for a customized VM (issue #153).
	if req.CustomizeJSON != "" {
		log.Printf("WARN Clone of %s: CustomizeJSON provided but not applied by the libvirt provider (MVP); "+
			"the clone inherits the source's hostname/cloud-init. Tracked as a follow-up to issue #153.",
			req.TargetName)
	}

	// Clone stays single-host in this slice (no CloneRequest.target_host_id yet,
	// ADR-0007 P1): define on the provider's own host.
	if err := p.defineDomainFromXML(ctx, p.virshProvider, domainName, targetXML); err != nil {
		return contracts.CloneResponse{}, fmt.Errorf("define target domain: %w", err)
	}

	log.Printf("INFO Successfully cloned VM %s -> %s (libvirt domain %s, linked=%t)", sourceID, req.TargetName, domainName, req.Linked)

	// virsh define is synchronous; no TaskRef. The clone is left powered off.
	return contracts.CloneResponse{
		TargetVmID: domainName,
	}, nil
}

// resolvePrimaryDisk returns the source domain's primary (boot) disk path and
// format. It prefers the live domain XML (domainDiskPaths) so
// it works regardless of the volume naming convention, and falls back to the
// pool volume lookup used by GetDiskInfo for the format.
func (p *Provider) resolvePrimaryDisk(ctx context.Context, sourceVMID string, sp *StorageProvider) (path, format string, err error) {
	return resolvePrimaryDiskOn(ctx, p.virshProvider, byName(sourceVMID), sp)
}

// resolvePrimaryDiskOn is resolvePrimaryDisk for domain d on vp's host (see
// resolveDomainDisksOn): the first of its disks, and their format.
func resolvePrimaryDiskOn(ctx context.Context, vp *VirshProvider, d domainTarget, sp *StorageProvider) (path, format string, err error) {
	paths, format, err := resolveDomainDisksOn(ctx, vp, d, sp)
	if err != nil {
		return "", "", err
	}
	return paths[0], format, nil
}

// defaultDiskFormat is the provider's standard disk format, assumed when no
// better answer is available.
const defaultDiskFormat = "qcow2"

// resolveDomainDisksOn returns the disk paths of domain d on vp's host, read
// from its live definition (the primary disk first; cloud-init and CD-ROM
// media excluded), and their format.
//
// On a single-host provider (byName) the format is looked up best-effort via
// the "<name>-disk" pool volume convention, exactly as before. On the
// owner-checked clustered target (d.diskByPath) nothing is looked up by name —
// a volume found by a name may belong to another domain — and the provider's
// standard format is used; the name-based lookup never yields another answer
// anyway, as GetVolumeInfo does not report a format.
func resolveDomainDisksOn(ctx context.Context, vp *VirshProvider, d domainTarget, sp *StorageProvider) ([]string, string, error) {
	diskPaths, derr := domainDiskPaths(ctx, vp, d.handle)
	if derr != nil {
		return nil, "", contracts.NewRetryableError(
			fmt.Sprintf("failed to read disks for source VM %q", d.name), derr)
	}
	if len(diskPaths) == 0 {
		return nil, "", contracts.NewInvalidSpecError(
			fmt.Sprintf("source VM %q has no usable disk to clone", d.name), nil)
	}

	// Best-effort format lookup via the pool volume convention; default to
	// qcow2 (the provider's standard) when unavailable.
	format := defaultDiskFormat
	if d.diskByPath {
		return diskPaths, format, nil
	}
	if vol, verr := sp.GetVolumeInfo(ctx, clonePoolName, vmDiskVolumeName(d.name)); verr == nil && vol.Format != "" {
		format = vol.Format
	}
	return diskPaths, format, nil
}

// createCloneDisk creates the clone's disk at targetDiskPath on vp's host: a
// linked overlay (linked) or a full copy of srcDiskPath.
func createCloneDisk(ctx context.Context, vp *VirshProvider, linked bool, srcDiskPath, srcDiskFormat, targetDiskPath string) error {
	if linked {
		return createLinkedOverlay(ctx, vp, srcDiskPath, srcDiskFormat, targetDiskPath)
	}
	return createFullCopy(ctx, vp, srcDiskPath, targetDiskPath)
}

// createLinkedOverlay creates a copy-on-write qcow2 overlay backed by the
// source disk on vp's host. The overlay is created remotely (the disk lives on
// the libvirt host) with vmDiskMode (withUmask) in a private directory and
// renamed onto targetDiskPath (diskWriteDir), then given to the qemu user so
// QEMU can open it — mirroring CreateVolume's handling. The source disk is
// opened read-only as a backing file (by its absolute path) and is never
// modified here.
func createLinkedOverlay(ctx context.Context, vp *VirshProvider, srcDiskPath, srcDiskFormat, targetDiskPath string) error {
	log.Printf("INFO Creating linked-clone overlay %s backed by %s (%s)", targetDiskPath, srcDiskPath, srcDiskFormat)
	wd, err := newDiskWriteDir(ctx, vp, filepath.Dir(targetDiskPath))
	if err != nil {
		return fmt.Errorf("create linked-clone overlay: %w", err)
	}
	defer wd.cleanup(ctx)
	name := filepath.Base(targetDiskPath)

	// qemu-img create -f qcow2 -b <src> -F <srcFormat> <overlay>
	res, err := runHost(ctx, vp, withUmask(vmDiskUmask,
		"qemu-img", "create",
		"-f", "qcow2",
		"-b", srcDiskPath,
		"-F", srcDiskFormat,
		wd.file(name),
	)...)
	if err != nil {
		return fmt.Errorf("create linked-clone overlay: %w, output: %s", err, res.Stderr)
	}
	if err := wd.publish(ctx, name, targetDiskPath); err != nil {
		return fmt.Errorf("create linked-clone overlay: %w", err)
	}

	finalizeClonedDisk(ctx, vp, targetDiskPath)
	return nil
}

// createFullCopy creates an independent qcow2 copy of the source disk at
// targetDiskPath on vp's host. Unlike a linked overlay, the result has no ongoing dependency
// on the source: qemu-img convert reads through any backing chain the source
// disk may have (e.g. a provider-created overlay on a base image) and writes a
// standalone, flattened qcow2.
//
// The copy is performed on the libvirt host (the disk lives there) by the real
// disk PATH resolved from the live domain (domblklist), NOT by a guessed
// "<vmid>-disk" pool-volume name. The earlier vol-clone approach assumed the
// provider names every disk "<vmid>-disk" inside the default pool; that does
// not hold for VMs whose disk is a plain file or follows a different naming
// convention, so full clone failed with "storage volume not found". Operating
// on the resolved path mirrors the linked-clone path and is naming-agnostic
// (issue #153, surfaced by libvirt clone E2E validation).
func createFullCopy(ctx context.Context, vp *VirshProvider, srcDiskPath, targetDiskPath string) error {
	log.Printf("INFO Creating full-clone copy %s from %s", targetDiskPath, srcDiskPath)

	// qemu-img convert -O qcow2 <src> <target>. The source format is
	// auto-probed by qemu-img (do not force -f, which would break if the
	// resolved format is wrong); convert flattens any backing chain. The copy
	// is created with vmDiskMode (withUmask) in a private directory and
	// renamed onto targetDiskPath (diskWriteDir).
	wd, err := newDiskWriteDir(ctx, vp, filepath.Dir(targetDiskPath))
	if err != nil {
		return fmt.Errorf("create full-clone copy: %w", err)
	}
	defer wd.cleanup(ctx)
	name := filepath.Base(targetDiskPath)
	res, err := runHost(ctx, vp, withUmask(vmDiskUmask,
		"qemu-img", "convert",
		"-O", "qcow2",
		srcDiskPath,
		wd.file(name),
	)...)
	if err != nil {
		return fmt.Errorf("create full-clone copy: %w, output: %s", err, res.Stderr)
	}
	if err := wd.publish(ctx, name, targetDiskPath); err != nil {
		return fmt.Errorf("create full-clone copy: %w", err)
	}

	finalizeClonedDisk(ctx, vp, targetDiskPath)
	return nil
}

// finalizeClonedDisk fixes ownership/SELinux on a freshly created clone disk
// on vp's host so libvirt-qemu can open it, and refreshes the pool so the new
// volume is visible to subsequent lookups. It mirrors StorageProvider.Create-
// Volume's handling. The disk already has its mode (vmDiskMode, set when it
// was created): it is never chmod'ed, and it is chowned without following a
// symbolic link (chownToQemu). Every step is best-effort: the host may not use
// these mechanisms (e.g. no SELinux), so failures are logged, not fatal.
func finalizeClonedDisk(ctx context.Context, vp *VirshProvider, targetDiskPath string) {
	if e := chownToQemu(ctx, vp, targetDiskPath); e != nil {
		log.Printf("WARN Failed to set clone disk ownership: %v", e)
	}
	if _, e := vp.runVirshCommand(ctx, "!", "sudo", "restorecon", targetDiskPath); e != nil {
		log.Printf("WARN Failed to restore clone disk SELinux context: %v", e)
	}
	if _, e := vp.runVirshCommand(ctx, "pool-refresh", clonePoolName); e != nil {
		log.Printf("WARN Failed to refresh pool after clone disk create: %v", e)
	}
}

// copyClonedNVRAM copies a UEFI source domain's nvram varstore to the clone's
// fresh per-clone path on vp's host, so the clone boots with its own
// independent UEFI variables rather than sharing (and corrupting) the source's
// varstore (issue #208).
//
// The copy runs host-side via the "!" direct-exec convention, mirroring the
// disk copy. The nvram directory (typically /var/lib/libvirt/qemu/nvram) is
// root-owned, so sudo is used as elsewhere in this provider. The varstore is a
// small fixed-size firmware-variable image. Any stale target is unlinked, then
// it is copied with `dd` opening the source with O_NOFOLLOW and creating the
// target with O_CREAT|O_EXCL|O_NOFOLLOW (oflag=nofollow, conv=excl): running
// as root, a symlink at either path makes the copy fail instead of being
// followed to another file, and a pre-existing target (a hard link to another
// file among them) is never truncated (ensureNVRAMTargetFree has refused a
// symlinked or in-use target before any file of the clone was written; this
// closes the race after that check). The target stays in the source
// varstore's directory (rewriteNVRAMPath). dd creates it under
// clonedNVRAMUmask — private to its owner (0600) from the start, never
// chmod'ed — and it is then given to the qemu user without following a
// symbolic link (chownToQemu). Failure is non-fatal but
// logged loudly: the clone may fail to boot UEFI correctly because its <nvram>
// now points at a path that was never populated.
func copyClonedNVRAM(ctx context.Context, vp *VirshProvider, srcNvramPath, targetNvramPath string) {
	log.Printf("INFO Copying UEFI varstore %s -> %s for clone", srcNvramPath, targetNvramPath)
	// A stale file there (an earlier failed clone; ensureNVRAMTargetFree
	// verified no domain uses it) is unlinked first — never truncated in place,
	// which would also rewrite any other hard link to it — and the copy then
	// creates the target exclusively (conv=excl: O_CREAT|O_EXCL), so anything
	// that appears at that path in between makes the copy fail.
	if _, err := runHost(ctx, vp, "sudo", "rm", "-f", "--", targetNvramPath); err != nil {
		log.Printf("WARN Failed to remove the stale UEFI varstore %s for clone: %v", targetNvramPath, err)
	}
	if res, err := runHost(ctx, vp, withUmask(clonedNVRAMUmask, "sudo", "dd", "if="+srcNvramPath, "of="+targetNvramPath,
		"iflag=nofollow", "oflag=nofollow", "conv=excl", "status=none")...); err != nil {
		stderr := ""
		if res != nil {
			stderr = res.Stderr
		}
		log.Printf("WARN Failed to copy UEFI varstore %s -> %s for clone: %v (output: %s). "+
			"The clone's <nvram> points at an unpopulated path and may fail to boot UEFI/Secure Boot correctly.",
			srcNvramPath, targetNvramPath, err, stderr)
		return
	}
	// Fix ownership/SELinux so libvirt-qemu (only) can open the varstore,
	// mirroring the clone-disk finalization. Best-effort: hosts vary in their
	// mechanisms.
	if e := chownToQemu(ctx, vp, targetNvramPath); e != nil {
		log.Printf("WARN Failed to set clone varstore ownership: %v", e)
	}
	if _, e := vp.runVirshCommand(ctx, "!", "sudo", "restorecon", targetNvramPath); e != nil {
		log.Printf("WARN Failed to restore clone varstore SELinux context: %v", e)
	}
}

// ensureNVRAMTargetFree refuses to let a clone of domainName write its UEFI
// varstore to target — as root — when target is a symbolic link (the copy
// would write through it to another file) or the varstore (or any other file)
// of ANY domain defined on the host behind h (the clone would share, and
// overwrite, another VM's firmware variables). It mirrors ensureDiskTargetFree:
// a regular file no domain uses is left over from an earlier, failed clone to
// the same name and is overwritten. The refusal is a Conflict whose message
// names only the clone's own domain.
func ensureNVRAMTargetFree(ctx context.Context, h hostCommandRunner, domainName, target string) error {
	res, err := runHost(ctx, h, "sh", "-c", targetKindScript, "sh", target)
	if err != nil {
		log.Printf("ERROR Could not check the UEFI varstore path %s for clone %s: %v", target, domainName, err)
		return contracts.NewRetryableError(fmt.Sprintf(
			"could not check the UEFI varstore path of libvirt domain %q on the host (details are in the provider log)", domainName), nil)
	}
	switch strings.TrimSpace(res.Stdout) {
	case targetKindSymlink:
		log.Printf("WARN Refusing to write the UEFI varstore of clone %s: %s is a symbolic link", domainName, target)
		return contracts.NewConflictError(fmt.Sprintf(
			"the UEFI varstore path of libvirt domain %q is a symbolic link on the host; refusing to write through it", domainName), nil)
	case pathExistsMarker:
		inUse, err := pathInUseOnHost(ctx, h, target)
		if err != nil {
			return err
		}
		if inUse {
			log.Printf("WARN Refusing to write the UEFI varstore of clone %s: %s is in use by another domain", domainName, target)
			return contracts.NewConflictError(fmt.Sprintf(
				"the UEFI varstore path of libvirt domain %q is in use by another domain on the host; refusing to overwrite it", domainName), nil)
		}
		log.Printf("INFO UEFI varstore %s of clone %s exists but no domain uses it (left by an earlier failed clone); overwriting it",
			target, domainName)
	}
	return nil
}

// rewriteDomainXMLForClone produces a new domain XML from the source domain XML
// with a fresh identity so the clone never collides with the source:
//
//   - <name> is set to targetName.
//   - <uuid> is replaced with a freshly generated v4 UUID. Libvirt rejects a
//     define whose UUID is already in use, so a stale UUID would fail the clone.
//   - every <mac address=.../> is replaced with a fresh locally-administered
//     unicast address (so the clone gets new NICs on the same L2 segment).
//   - the primary disk <source file='<srcDiskPath>'/> is re-pointed at
//     targetDiskPath (the cloned/overlay volume). Only the matching source path
//     is rewritten, so a cloud-init CD-ROM source is left untouched.
//   - for a UEFI source, the per-VM <nvram>...</nvram> varstore path is
//     re-pointed to a fresh per-clone path derived from the SOURCE varstore's
//     directory and the target name (issue #208). The returned srcNvramPath /
//     targetNvramPath let the caller copy the actual varstore file on the host
//     (the XML rewrite stays pure and side-effect-free). A BIOS source has no
//     <nvram> element: both returned paths are empty and the XML is unchanged.
//
// It does NOT shell out and is therefore fully unit-testable.
func rewriteDomainXMLForClone(sourceXML, targetName, srcDiskPath, targetDiskPath string) (targetXML, srcNvramPath, targetNvramPath string, err error) {
	if strings.TrimSpace(sourceXML) == "" {
		return "", "", "", fmt.Errorf("source domain XML is empty")
	}

	out := sourceXML

	// Rewrite the domain name (first <name> element only — interface/source
	// elements do not use <name>...</name> so a single replacement is safe).
	// targetName is CR-derived (VMClone spec.target.name); it carries a
	// Kubernetes object-name validation pattern but not an XML-safety one, so
	// it is escaped as defense in depth (issue #260).
	if reDomainName.MatchString(out) {
		out = replaceFirst(reDomainName, out, fmt.Sprintf("<name>%s</name>", xmlEscape(targetName)))
	} else {
		return "", "", "", fmt.Errorf("source domain XML has no <name> element")
	}

	// Rewrite the domain UUID with a fresh one.
	newUUID, gerr := generateRandomUUID()
	if gerr != nil {
		return "", "", "", fmt.Errorf("generate clone UUID: %w", gerr)
	}
	if reDomainUUID.MatchString(out) {
		out = replaceFirst(reDomainUUID, out, fmt.Sprintf("<uuid>%s</uuid>", newUUID))
	} else {
		return "", "", "", fmt.Errorf("source domain XML has no <uuid> element")
	}

	// Replace every NIC MAC with a fresh locally-administered address. Each
	// occurrence gets its own address so multi-NIC sources stay collision-free.
	out = reMACAddress.ReplaceAllStringFunc(out, func(string) string {
		mac, merr := generateRandomMAC()
		if merr != nil {
			// Fall back to leaving the element unchanged on the (practically
			// impossible) RNG failure; the define will surface any conflict.
			return "<mac address='52:54:00:00:00:00'/>"
		}
		return fmt.Sprintf("<mac address='%s'/>", mac)
	})

	// Re-point the primary disk source path. Match both quote styles.
	if srcDiskPath != "" && targetDiskPath != "" {
		replaced := strings.Replace(out, fmt.Sprintf("file='%s'", srcDiskPath), fmt.Sprintf("file='%s'", targetDiskPath), 1)
		if replaced == out {
			replaced = strings.Replace(out, fmt.Sprintf("file=\"%s\"", srcDiskPath), fmt.Sprintf("file=\"%s\"", targetDiskPath), 1)
		}
		if replaced == out {
			return "", "", "", fmt.Errorf("primary disk source %q not found in source domain XML", srcDiskPath)
		}
		out = replaced
	}

	// Re-point the per-VM UEFI <nvram> varstore (issue #208). On a UEFI source
	// the <nvram> element holds an absolute varstore path
	// (e.g. /var/lib/libvirt/qemu/nvram/<domain>_VARS.fd); if the clone kept it,
	// both domains would share one varstore -> define conflict or corrupted boot
	// order / Secure Boot state. Derive the new path from the SOURCE varstore's
	// directory (do not hardcode the default dir) and the target name. A BIOS
	// source has no <nvram>: this is a no-op and both nvram paths stay empty.
	out, srcNvramPath, targetNvramPath = rewriteNVRAMPath(out, targetName)

	// Drop the SOURCE VirtualMachine's owner stamp: the clone is a different
	// domain, bound to a VirtualMachine the clone controller creates afterwards,
	// and must not claim the source's owner. The stamp's bytes are spliced out
	// verbatim; the rest of the document is untouched.
	out, err = stripOwnerMetadata(out)
	if err != nil {
		return "", "", "", fmt.Errorf("strip source owner metadata: %w", err)
	}

	return out, srcNvramPath, targetNvramPath, nil
}

// rewriteNVRAMPath re-points a UEFI domain's <nvram> varstore path to a fresh
// per-clone path and returns (rewrittenXML, srcNvramPath, targetNvramPath).
//
// The new path keeps the SOURCE varstore's directory (so a non-default nvram
// location is preserved) and uses "<targetName>_VARS.fd" as the basename — the
// libvirt/QEMU convention. For a BIOS source (no <nvram> element, or an empty
// path) it returns the XML unchanged and empty paths, signalling the caller to
// skip the host-side varstore copy.
func rewriteNVRAMPath(domainXML, targetName string) (out, srcPath, dstPath string) {
	m := reNVRAM.FindStringSubmatchIndex(domainXML)
	if m == nil {
		return domainXML, "", "" // BIOS: no nvram element.
	}
	// Group 1 is the captured varstore path (indices m[2]:m[3]).
	srcPath = strings.TrimSpace(domainXML[m[2]:m[3]])
	if srcPath == "" {
		// Templated <nvram/> with no inline path (libvirt fills it from the
		// firmware feature). Nothing to copy or re-point.
		return domainXML, "", ""
	}
	dstPath = filepath.Join(filepath.Dir(srcPath), fmt.Sprintf("%s_VARS.fd", targetName))
	if dstPath == srcPath {
		// Degenerate: target basename already equals source. Leave as-is.
		return domainXML, srcPath, dstPath
	}
	// Splice the new path into the captured path span only, preserving any
	// attributes on the opening <nvram ...> tag (e.g. template=). dstPath
	// embeds targetName (CR-derived; see rewriteDomainXMLForClone) so it is
	// escaped before insertion as defense in depth (issue #260); the
	// (unescaped) dstPath is still what the caller uses for the actual
	// host-side varstore file copy.
	out = domainXML[:m[2]] + xmlEscape(dstPath) + domainXML[m[3]:]
	return out, srcPath, dstPath
}

// cloneClassOverride is the subset of a JSON-encoded VM class that the clone
// path consumes. The clone controller (VMCloneReconciler.classJSON) marshals the
// **v1beta1 VMClassSpec**, so the field names and shapes here mirror that type
// exactly: `cpu` (int), `memory` (a resource.Quantity string such as "8Gi"), and
// `performanceProfile.{cpuHotAddEnabled,memoryHotAddEnabled}`. In particular,
// memory is a quantity — NOT an int `memoryMiB` — so it must be parsed via
// resource.Quantity and converted to MiB (matching the manager's
// `Memory.Value() / (1024*1024)` convention); a plain int field would silently
// fail to bind and the memory override (and its #221 headroom) would never fire.
type cloneClassOverride struct {
	// CPU is the target vCPU count (0 = inherit the source's).
	CPU int32 `json:"cpu"`
	// Memory is the target memory as a resource.Quantity (e.g. "8Gi"); the
	// zero value means "inherit the source's". Converted to MiB via memoryMiB().
	Memory resource.Quantity `json:"memory"`
	// PerformanceProfile carries the hot-add flags that decide whether the clone
	// keeps online-reconfigure headroom (#221).
	PerformanceProfile *cloneClassPerfProfile `json:"performanceProfile,omitempty"`
}

// memoryMiB converts the class's memory quantity to MiB, matching the manager's
// conversion (Memory.Value() bytes / 1 MiB). Returns 0 when unset, which the
// caller treats as "inherit the source's memory".
func (c cloneClassOverride) memoryMiB() int64 {
	return c.Memory.Value() / (1024 * 1024)
}

// cloneClassPerfProfile is the slice of a class's performance profile that the
// clone path needs: the CPU/memory hot-add toggles (#221).
type cloneClassPerfProfile struct {
	// CPUHotAddEnabled requests online CPU grow headroom on the clone.
	CPUHotAddEnabled bool `json:"cpuHotAddEnabled,omitempty"`
	// MemoryHotAddEnabled requests online memory grow headroom on the clone.
	MemoryHotAddEnabled bool `json:"memoryHotAddEnabled,omitempty"`
}

// applyClassOverrides applies best-effort CPU/memory overrides from a
// JSON-encoded VM class onto a domain XML. Unparseable or empty input is a
// no-op (the clone inherits the source's resources). Only vcpu and memory are
// adjusted; this is intentionally minimal for the MVP (issue #153).
//
// When the override class opts into CPU/memory hot-add (performanceProfile.
// {cpuHotAddEnabled,memoryHotAddEnabled}), the resource elements are rendered
// via buildCPUMemoryXML so the clone keeps online-reconfigure headroom — a
// vcpu current=<initial> ceiling and a <memory> balloon maximum above
// <currentMemory> — instead of the plain no-headroom form that would silently
// strip the clone of live-grow capability (#221). When the flags are
// absent/false the plain form is emitted, byte-identical to the historical
// behavior (no regression).
func applyClassOverrides(domainXML, classJSON string) string {
	if strings.TrimSpace(classJSON) == "" {
		return domainXML
	}
	var class cloneClassOverride
	if err := json.Unmarshal([]byte(classJSON), &class); err != nil {
		log.Printf("WARN Clone: ignoring unparseable ClassJSON override: %v", err)
		return domainXML
	}

	cpuHotAdd := class.PerformanceProfile != nil && class.PerformanceProfile.CPUHotAddEnabled
	memHotAdd := class.PerformanceProfile != nil && class.PerformanceProfile.MemoryHotAddEnabled

	// Render the resource elements once, reusing the create-path headroom logic
	// (#203) so the policy lives in exactly one place. The fields are only used
	// when the corresponding override value is set below.
	memMiB := class.memoryMiB()
	res := buildCPUMemoryXML(class.CPU, memMiB, cpuHotAdd, memHotAdd)

	out := domainXML
	if memMiB > 0 {
		reMem := regexp.MustCompile(`<memory[^>]*>.*?</memory>`)
		reCur := regexp.MustCompile(`<currentMemory[^>]*>.*?</currentMemory>`)
		out = replaceFirst(reMem, out, res.Memory)
		out = replaceFirst(reCur, out, res.CurrentMemory)
	}
	if class.CPU > 0 {
		reVCPU := regexp.MustCompile(`<vcpu[^>]*>.*?</vcpu>`)
		out = replaceFirst(reVCPU, out, res.VCPU)
	}
	return out
}

// replaceFirst replaces only the first match of re in s with repl. regexp has
// no built-in count-limited replace, so we locate the first match and splice.
func replaceFirst(re *regexp.Regexp, s, repl string) string {
	loc := re.FindStringIndex(s)
	if loc == nil {
		return s
	}
	return s[:loc[0]] + repl + s[loc[1]:]
}

// generateRandomMAC returns a random locally-administered, unicast MAC address
// in the QEMU 52:54:00 OUI space. The first octet is fixed at 0x52 (locally
// administered + unicast), matching libvirt/QEMU convention, with the remaining
// octets randomized to avoid collisions with the source NIC.
func generateRandomMAC() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", b[0], b[1], b[2]), nil
}
