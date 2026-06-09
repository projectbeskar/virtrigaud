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
)

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

// Clone clones the source VM identified by req.SourceVmID into a new libvirt
// domain named req.TargetName. It is the libvirt implementation of the
// provider-contract Cloner capability (issues #153/#179).
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
//
// The target domain is defined with a fresh UUID and fresh MAC address(es) by
// rewriting the source domain's XML, so the two domains never collide. The
// clone is left powered off; the manager controls power separately, matching
// Create's behavior.
func (p *Provider) Clone(ctx context.Context, req contracts.CloneRequest) (contracts.CloneResponse, error) {
	log.Printf("INFO Cloning VM %s -> %s (linked=%t)", req.SourceVmID, req.TargetName, req.Linked)

	if p.virshProvider == nil {
		return contracts.CloneResponse{}, contracts.NewRetryableError("virsh provider not initialized", nil)
	}
	if req.SourceVmID == "" {
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError("clone source VM ID is required", nil)
	}
	if req.TargetName == "" {
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError("clone target name is required", nil)
	}

	storageProvider := NewStorageProvider(p.virshProvider)

	// 1. Resolve the source domain and reject if it does not exist. domstate
	//    accepts either a domain name or a UUID, matching how libvirt identifies
	//    a VM (Status.ID is the domain name for this provider).
	if _, err := p.virshProvider.getDomainState(ctx, req.SourceVmID); err != nil {
		return contracts.CloneResponse{}, contracts.NewNotFoundError(
			fmt.Sprintf("source VM %q not found", req.SourceVmID), err)
	}

	// Reject a target-name collision up front rather than failing mid-define.
	domains, err := p.virshProvider.listDomains(ctx)
	if err != nil {
		return contracts.CloneResponse{}, contracts.NewRetryableError("failed to list domains", err)
	}
	for _, d := range domains {
		if d.Name == req.TargetName {
			return contracts.CloneResponse{}, contracts.NewInvalidSpecError(
				fmt.Sprintf("target VM %q already exists", req.TargetName), nil)
		}
	}

	// 2. Resolve the source's primary disk path + format.
	srcDiskPath, srcDiskFormat, err := p.resolvePrimaryDisk(ctx, req.SourceVmID, storageProvider)
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
	targetVolumeName := fmt.Sprintf("%s-disk", req.TargetName)
	targetDiskPath := filepath.Join(poolInfo.Path, fmt.Sprintf("%s.qcow2", targetVolumeName))

	if req.Linked {
		if err := p.createLinkedOverlay(ctx, srcDiskPath, srcDiskFormat, targetDiskPath); err != nil {
			return contracts.CloneResponse{}, err
		}
	} else {
		if err := p.createFullCopy(ctx, srcDiskPath, targetDiskPath); err != nil {
			return contracts.CloneResponse{}, err
		}
	}

	// 4. Define the target domain by cloning the source XML and rewriting the
	//    identity (name/uuid/mac) and the primary disk source path.
	srcXML, err := p.virshProvider.runVirshCommand(ctx, "dumpxml", req.SourceVmID)
	if err != nil {
		return contracts.CloneResponse{}, contracts.NewRetryableError("failed to dump source domain XML", err)
	}

	targetXML, srcNvramPath, targetNvramPath, err := rewriteDomainXMLForClone(srcXML.Stdout, req.TargetName, srcDiskPath, targetDiskPath)
	if err != nil {
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError("rewrite source domain XML for clone", err)
	}

	// For a UEFI source the domain XML carries a per-VM <nvram> varstore that was
	// just re-pointed to a fresh per-clone path. Copy the actual varstore file on
	// the libvirt host so the clone gets an independent set of UEFI variables
	// (issue #208); otherwise two domains would share one varstore. This is the
	// side-effecting counterpart to the pure XML rewrite, performed here next to
	// the disk copy so rewriteDomainXMLForClone stays testable without a host.
	if srcNvramPath != "" && targetNvramPath != "" {
		p.copyClonedNVRAM(ctx, srcNvramPath, targetNvramPath)
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

	if err := p.createDomainDefinition(ctx, req.TargetName, targetXML); err != nil {
		return contracts.CloneResponse{}, fmt.Errorf("create target domain definition: %w", err)
	}
	if err := p.defineDomain(ctx, req.TargetName); err != nil {
		return contracts.CloneResponse{}, fmt.Errorf("define target domain: %w", err)
	}

	log.Printf("INFO Successfully cloned VM %s -> %s (linked=%t)", req.SourceVmID, req.TargetName, req.Linked)

	// virsh define is synchronous; no TaskRef. The clone is left powered off.
	return contracts.CloneResponse{
		TargetVmID: req.TargetName,
	}, nil
}

// resolvePrimaryDisk returns the source domain's primary (boot) disk path and
// format. It prefers the live domain XML (domblklist via getDomainDiskPaths) so
// it works regardless of the volume naming convention, and falls back to the
// pool volume lookup used by GetDiskInfo for the format.
func (p *Provider) resolvePrimaryDisk(ctx context.Context, sourceVMID string, sp *StorageProvider) (path, format string, err error) {
	diskPaths, derr := p.getDomainDiskPaths(ctx, sourceVMID)
	if derr != nil {
		return "", "", contracts.NewRetryableError(
			fmt.Sprintf("failed to read disks for source VM %q", sourceVMID), derr)
	}
	if len(diskPaths) == 0 {
		return "", "", contracts.NewInvalidSpecError(
			fmt.Sprintf("source VM %q has no usable disk to clone", sourceVMID), nil)
	}
	path = diskPaths[0]

	// Best-effort format lookup via the pool volume convention; default to
	// qcow2 (the provider's standard) when unavailable.
	format = "qcow2"
	if vol, verr := sp.GetVolumeInfo(ctx, clonePoolName, fmt.Sprintf("%s-disk", sourceVMID)); verr == nil && vol.Format != "" {
		format = vol.Format
	}
	return path, format, nil
}

// createLinkedOverlay creates a copy-on-write qcow2 overlay backed by the
// source disk. The overlay is created remotely (the disk lives on the libvirt
// host), then ownership/permissions are fixed so QEMU can open it — mirroring
// CreateVolume's handling. The source disk is opened read-only as a backing
// file and is never modified here.
func (p *Provider) createLinkedOverlay(ctx context.Context, srcDiskPath, srcDiskFormat, targetDiskPath string) error {
	log.Printf("INFO Creating linked-clone overlay %s backed by %s (%s)", targetDiskPath, srcDiskPath, srcDiskFormat)

	// qemu-img create -f qcow2 -b <src> -F <srcFormat> <overlay>
	res, err := p.virshProvider.runVirshCommand(ctx, "!",
		"qemu-img", "create",
		"-f", "qcow2",
		"-b", srcDiskPath,
		"-F", srcDiskFormat,
		targetDiskPath,
	)
	if err != nil {
		return fmt.Errorf("create linked-clone overlay: %w, output: %s", err, res.Stderr)
	}

	p.finalizeClonedDisk(ctx, targetDiskPath)
	return nil
}

// createFullCopy creates an independent qcow2 copy of the source disk at
// targetDiskPath. Unlike a linked overlay, the result has no ongoing dependency
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
func (p *Provider) createFullCopy(ctx context.Context, srcDiskPath, targetDiskPath string) error {
	log.Printf("INFO Creating full-clone copy %s from %s", targetDiskPath, srcDiskPath)

	// qemu-img convert -O qcow2 <src> <target>. The source format is
	// auto-probed by qemu-img (do not force -f, which would break if the
	// resolved format is wrong); convert flattens any backing chain.
	res, err := p.virshProvider.runVirshCommand(ctx, "!",
		"qemu-img", "convert",
		"-O", "qcow2",
		srcDiskPath,
		targetDiskPath,
	)
	if err != nil {
		return fmt.Errorf("create full-clone copy: %w, output: %s", err, res.Stderr)
	}

	p.finalizeClonedDisk(ctx, targetDiskPath)
	return nil
}

// finalizeClonedDisk fixes ownership/permissions/SELinux on a freshly created
// clone disk so libvirt-qemu can open it, and refreshes the pool so the new
// volume is visible to subsequent lookups. It mirrors StorageProvider.Create-
// Volume's handling. Every step is best-effort: the host may not use these
// mechanisms (e.g. no SELinux), so failures are logged, not fatal.
func (p *Provider) finalizeClonedDisk(ctx context.Context, targetDiskPath string) {
	if _, e := p.virshProvider.runVirshCommand(ctx, "!", "sudo", "chown", "libvirt-qemu:kvm", targetDiskPath); e != nil {
		log.Printf("WARN Failed to set clone disk ownership: %v", e)
	}
	if _, e := p.virshProvider.runVirshCommand(ctx, "!", "sudo", "chmod", "777", targetDiskPath); e != nil {
		log.Printf("WARN Failed to set clone disk permissions: %v", e)
	}
	if _, e := p.virshProvider.runVirshCommand(ctx, "!", "sudo", "restorecon", targetDiskPath); e != nil {
		log.Printf("WARN Failed to restore clone disk SELinux context: %v", e)
	}
	if _, e := p.virshProvider.runVirshCommand(ctx, "pool-refresh", clonePoolName); e != nil {
		log.Printf("WARN Failed to refresh pool after clone disk create: %v", e)
	}
}

// copyClonedNVRAM copies a UEFI source domain's nvram varstore to the clone's
// fresh per-clone path on the libvirt host, so the clone boots with its own
// independent UEFI variables rather than sharing (and corrupting) the source's
// varstore (issue #208).
//
// The copy runs host-side via the "!" direct-exec convention, mirroring the
// disk copy. The nvram directory (typically /var/lib/libvirt/qemu/nvram) is
// root-owned, so sudo is used as elsewhere in this provider. The varstore is a
// small fixed-size firmware-variable image; "cp -f --" overwrites any stale
// target and stops option parsing at the paths. Failure is non-fatal but logged
// loudly: the clone may fail to boot UEFI correctly because its <nvram> now
// points at a path that was never populated.
func (p *Provider) copyClonedNVRAM(ctx context.Context, srcNvramPath, targetNvramPath string) {
	log.Printf("INFO Copying UEFI varstore %s -> %s for clone", srcNvramPath, targetNvramPath)
	if res, err := p.virshProvider.runVirshCommand(ctx, "!", "sudo", "cp", "-f", "--", srcNvramPath, targetNvramPath); err != nil {
		log.Printf("WARN Failed to copy UEFI varstore %s -> %s for clone: %v (output: %s). "+
			"The clone's <nvram> points at an unpopulated path and may fail to boot UEFI/Secure Boot correctly.",
			srcNvramPath, targetNvramPath, err, res.Stderr)
		return
	}
	// Fix ownership/SELinux so libvirt-qemu can open the varstore, mirroring the
	// clone-disk finalization. Best-effort: hosts vary in their mechanisms.
	if _, e := p.virshProvider.runVirshCommand(ctx, "!", "sudo", "chown", "libvirt-qemu:kvm", targetNvramPath); e != nil {
		log.Printf("WARN Failed to set clone varstore ownership: %v", e)
	}
	if _, e := p.virshProvider.runVirshCommand(ctx, "!", "sudo", "restorecon", targetNvramPath); e != nil {
		log.Printf("WARN Failed to restore clone varstore SELinux context: %v", e)
	}
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
	if reDomainName.MatchString(out) {
		out = replaceFirst(reDomainName, out, fmt.Sprintf("<name>%s</name>", targetName))
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
	// attributes on the opening <nvram ...> tag (e.g. template=).
	out = domainXML[:m[2]] + dstPath + domainXML[m[3]:]
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

// generateRandomUUID returns a random RFC-4122 v4 UUID string. Used to give the
// cloned domain a fresh identity (libvirt rejects a duplicate UUID on define).
func generateRandomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	// Set version (4) and variant (10xx) bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
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
