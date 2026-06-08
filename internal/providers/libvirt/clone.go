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
		sourceVolumeName := fmt.Sprintf("%s-disk", req.SourceVmID)
		if _, err := storageProvider.CloneVolume(ctx, clonePoolName, sourceVolumeName, targetVolumeName); err != nil {
			return contracts.CloneResponse{}, fmt.Errorf("full clone of volume %q: %w", sourceVolumeName, err)
		}
		// vol-clone may emit the file at a slightly different path; trust the
		// pool convention used elsewhere in this provider.
		targetDiskPath = filepath.Join(poolInfo.Path, fmt.Sprintf("%s.qcow2", targetVolumeName))
	}

	// 4. Define the target domain by cloning the source XML and rewriting the
	//    identity (name/uuid/mac) and the primary disk source path.
	srcXML, err := p.virshProvider.runVirshCommand(ctx, "dumpxml", req.SourceVmID)
	if err != nil {
		return contracts.CloneResponse{}, contracts.NewRetryableError("failed to dump source domain XML", err)
	}

	targetXML, err := rewriteDomainXMLForClone(srcXML.Stdout, req.TargetName, srcDiskPath, targetDiskPath)
	if err != nil {
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError("rewrite source domain XML for clone", err)
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

	// Fix ownership/permissions/SELinux so libvirt-qemu can open the overlay,
	// matching StorageProvider.CreateVolume. Failures are non-fatal (the host
	// may not use these mechanisms).
	if _, e := p.virshProvider.runVirshCommand(ctx, "!", "sudo", "chown", "libvirt-qemu:kvm", targetDiskPath); e != nil {
		log.Printf("WARN Failed to set overlay ownership: %v", e)
	}
	if _, e := p.virshProvider.runVirshCommand(ctx, "!", "sudo", "chmod", "777", targetDiskPath); e != nil {
		log.Printf("WARN Failed to set overlay permissions: %v", e)
	}
	if _, e := p.virshProvider.runVirshCommand(ctx, "!", "sudo", "restorecon", targetDiskPath); e != nil {
		log.Printf("WARN Failed to restore overlay SELinux context: %v", e)
	}

	// Refresh the pool so the new overlay is visible to subsequent vol lookups.
	if _, e := p.virshProvider.runVirshCommand(ctx, "pool-refresh", clonePoolName); e != nil {
		log.Printf("WARN Failed to refresh pool after overlay create: %v", e)
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
//
// It does NOT shell out and is therefore fully unit-testable.
func rewriteDomainXMLForClone(sourceXML, targetName, srcDiskPath, targetDiskPath string) (string, error) {
	if strings.TrimSpace(sourceXML) == "" {
		return "", fmt.Errorf("source domain XML is empty")
	}

	out := sourceXML

	// Rewrite the domain name (first <name> element only — interface/source
	// elements do not use <name>...</name> so a single replacement is safe).
	if reDomainName.MatchString(out) {
		out = replaceFirst(reDomainName, out, fmt.Sprintf("<name>%s</name>", targetName))
	} else {
		return "", fmt.Errorf("source domain XML has no <name> element")
	}

	// Rewrite the domain UUID with a fresh one.
	newUUID, err := generateRandomUUID()
	if err != nil {
		return "", fmt.Errorf("generate clone UUID: %w", err)
	}
	if reDomainUUID.MatchString(out) {
		out = replaceFirst(reDomainUUID, out, fmt.Sprintf("<uuid>%s</uuid>", newUUID))
	} else {
		return "", fmt.Errorf("source domain XML has no <uuid> element")
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
			return "", fmt.Errorf("primary disk source %q not found in source domain XML", srcDiskPath)
		}
		out = replaced
	}

	return out, nil
}

// applyClassOverrides applies best-effort CPU/memory overrides from a
// JSON-encoded contracts.VMClass onto a domain XML. Unparseable or empty input
// is a no-op (the clone inherits the source's resources). Only vcpu and memory
// are adjusted; this is intentionally minimal for the MVP (issue #153).
func applyClassOverrides(domainXML, classJSON string) string {
	if strings.TrimSpace(classJSON) == "" {
		return domainXML
	}
	var class contracts.VMClass
	if err := json.Unmarshal([]byte(classJSON), &class); err != nil {
		log.Printf("WARN Clone: ignoring unparseable ClassJSON override: %v", err)
		return domainXML
	}

	out := domainXML
	if class.MemoryMiB > 0 {
		reMem := regexp.MustCompile(`<memory[^>]*>.*?</memory>`)
		reCur := regexp.MustCompile(`<currentMemory[^>]*>.*?</currentMemory>`)
		out = replaceFirst(reMem, out, fmt.Sprintf("<memory unit='MiB'>%d</memory>", class.MemoryMiB))
		out = replaceFirst(reCur, out, fmt.Sprintf("<currentMemory unit='MiB'>%d</currentMemory>", class.MemoryMiB))
	}
	if class.CPU > 0 {
		reVCPU := regexp.MustCompile(`<vcpu[^>]*>.*?</vcpu>`)
		out = replaceFirst(reVCPU, out, fmt.Sprintf("<vcpu placement='static'>%d</vcpu>", class.CPU))
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
