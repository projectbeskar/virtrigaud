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
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Routed Clone of a CLUSTERED provider (ADR-0007 Addendum A, A1, slice 3).
//
// Disks are host-local and a linked clone depends on its source disk, so in v1
// a clone lands on its source VM's host: the operator names that host twice —
// source_host_id (routing) and target_host_id (landing, the target
// VirtualMachine's pendingHost) — and the provider refuses anything else. It
// never picks a landing host itself.
//
// The target VirtualMachine exists before the clone on a clustered provider,
// so the clone's domain is stamped with ITS identity (namespace, name, uid):
// the finalizer's owner-checked Delete can then remove a clone whose status
// write was lost, and a retried clone is an idempotent success. The source's
// stamp is never copied.
//
// Unlike the single-host clone, the clustered clone gives the new domain its
// own copy of the source's cloud-init seed ISO: sharing one seed would let the
// clone's Delete remove the source's seed directory (and the source's Delete
// the clone's), across VirtualMachines and possibly namespaces.

// linkedCloneClusteredRefusal is the InvalidArgument answer to a linked clone
// on a clustered provider. A linked clone's overlay keeps reading its source's
// disk for its whole life, and the source's disk is not frozen at clone time
// (the reason #358 disables single-host linked clones too), so only full
// clones are served there (v0.4.0), and the provider does not advertise linked
// clones. The source's lifecycle calls (Delete, SnapshotCreate/Delete/Revert)
// refuse while any overlay depends on its disk (disk_dependents.go) on both
// paths.
const linkedCloneClusteredRefusal = "linked clones are not supported on a clustered libvirt provider: " +
	"a linked clone depends on its source's disk for its whole life; use a full clone"

// cloneClustered is the CLUSTERED Clone. It validates the request, then runs
// on the source's bound host with the source domain owner-checked
// (withOwnedDomain) and addressed by its UUID (cloneOnHost).
func (p *Provider) cloneClustered(ctx context.Context, req contracts.CloneRequest) (contracts.CloneResponse, error) {
	source := strings.TrimSpace(req.Source.HostID)
	landing := strings.TrimSpace(req.TargetHostID)
	switch {
	case source == "":
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError(emptyTargetHostMessage+" (clone: source_host_id)", nil)
	case landing == "":
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError(
			"clustered libvirt clone requires target_host_id: the host the clone lands on (the target VM's pending host)", nil)
	case landing != source:
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError(fmt.Sprintf(
			"clustered libvirt clone must land on its source VM's host (source_host_id %q, target_host_id %q): "+
				"disks are host-local and a cross-host clone is not supported (ADR-0007 Addendum A)", source, landing), nil)
	case req.Linked:
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError(linkedCloneClusteredRefusal, nil)
	case req.Source.ID == "":
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError("clone source VM ID is required", nil)
	case req.TargetName == "":
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError("clone target name is required", nil)
	case req.TargetVM.IsZero() || !hasNamingIdentity(req.TargetVM):
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError(
			"clustered libvirt clone requires the target VirtualMachine's identity (namespace, name and uid): "+
				"the clone is stamped with it", nil)
	}
	domainName, err := cloneDomainName(req)
	if err != nil {
		return contracts.CloneResponse{}, err
	}

	// The clone's disk copy is long: run it under the routed budget and guard
	// (routed_budget.go).
	bctx, cancel := withRoutedBudget(ctx)
	defer cancel()
	var resp contracts.CloneResponse
	err = p.withOwnedDomain(bctx, req.Source, "clone", func(c libvirtConn, d domainTarget) error {
		vp, err := virshOf(c)
		if err != nil {
			return err
		}
		r, err := p.cloneOnHost(bctx, vp, c, d, req, domainName)
		if err != nil {
			return err
		}
		r.TaskRef = encodeHostTaskRef(c.HostID(), r.TaskRef)
		resp = r
		return nil
	})
	return resp, classifyRoutedFailure("clone VM", source, ctx, bctx, err)
}

// createFullCopyGuarded is createFullCopy for a clustered clone, on the same
// write path (#358): qemu-img convert writes the copy with vmDiskMode
// (withUmask) inside a private directory next to the disk's name
// (diskWriteDir), the finished copy is renamed onto the name (`mv -f -T`,
// which replaces a symbolic link there instead of following it), and the disk
// is then finalized as for any clone (chown -h, never chmod'ed:
// finalizeClonedDisk). The convert alone runs under flock(1) on the clone's
// lock (a retry while an earlier copy still runs is "in progress", never a
// second copy) and timeout(1) (it is stopped on the host when the call's
// budget runs out); the guard refuses a lock, or a disk name, that is a
// symbolic link.
//
// The source is read as root (privileged_copy.go) when its chain is safe for
// root to read and passwordless sudo allows it — the SSH user holds the lock
// outside sudo, and timeout(1) runs inside sudo so it can stop the root
// qemu-img, which writes into a file the SSH user created under vmDiskUmask:
//
//	sh -c <hostGuardScript> … flock -n -E 75 <lock> sh -c <umask> 0137 sudo -n timeout … qemu-img convert -f <fmt> … <private dir>/<name>
//
// and otherwise as the SSH user, exactly as before:
//
//	sh -c <hostGuardScript> … flock -n -E 75 <lock> timeout … sh -c <umask> 0137 qemu-img convert … <private dir>/<name>
//
// A copy that fails or is stopped never reaches the disk's name: its partial
// file is removed with the private directory, whatever happens, within
// routedCleanupTimeout. When the rename itself fails, the name may already
// hold the copy (the answer was lost), so it is removed again under the lock
// (removeClonedDisk).
func createFullCopyGuarded(ctx context.Context, vp *VirshProvider, lock hostLock, srcDiskPath, srcFormat, targetDiskPath string) error {
	log.Printf("INFO Creating full-clone copy %s from %s (guarded)", targetDiskPath, srcDiskPath)
	wd, err := newDiskWriteDir(ctx, vp, filepath.Dir(targetDiskPath))
	if err != nil {
		return fmt.Errorf("create full-clone copy: %w", err)
	}
	defer wd.cleanupWithin(ctx, routedCleanupTimeout)
	name := filepath.Base(targetDiskPath)
	if res, err := fullCloneCopy(srcDiskPath, srcFormat, wd.file(name), targetDiskPath).run(ctx, vp, guardFor(ctx, lock)); err != nil {
		var roe *routedOpError
		if errors.As(err, &roe) {
			return err
		}
		stderr := ""
		if res != nil {
			stderr = res.Stderr
		}
		return fmt.Errorf("create full-clone copy: %w, output: %s", err, stderr)
	}
	if err := wd.publish(ctx, name, targetDiskPath); err != nil {
		removeClonedDisk(ctx, vp, lock, targetDiskPath)
		return fmt.Errorf("create full-clone copy: %w", err)
	}
	finalizeClonedDisk(ctx, vp, targetDiskPath)
	return nil
}

// removeClonedDisk removes a failed clustered clone's disk, best-effort, only
// when no domain on the host uses it (pathInUseOnHost), under the clone's lock (waiting routedCleanupLockWait for a copy that
// timeout(1) just stopped): a lock still held after that belongs to another
// attempt, whose disk is left alone. It runs through the guard (never through
// a symbolic link) with sudo, as the disk may already be owned by
// libvirt-qemu, and even when the call's budget ran out
// (context.WithoutCancel, bounded by routedCleanupTimeout).
func removeClonedDisk(ctx context.Context, vp *VirshProvider, lock hostLock, targetDiskPath string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), routedCleanupTimeout)
	defer cancel()
	// Never remove a file some domain on the host uses (a define whose error
	// hid a success, a domain defined over the path meanwhile): check first,
	// and leave the file when it is in use or the check itself fails.
	switch inUse, err := pathInUseOnHost(cctx, vp, targetDiskPath); {
	case err != nil:
		log.Printf("WARN Leaving the disk %s of a failed clone: could not check whether a domain uses it: %v", targetDiskPath, err)
		return
	case inUse:
		log.Printf("WARN Leaving the disk %s of a failed clone: a domain on the host uses it", targetDiskPath)
		return
	}
	argv := guardArgv(lock, targetDiskPath, []string{"-w", routedCleanupLockWait},
		[]string{"sudo", "rm", "-f", "--", targetDiskPath})
	if _, err := runHost(cctx, vp, argv...); err != nil {
		log.Printf("WARN Could not remove the disk %s of a failed clone (another attempt may own it; "+
			"the next attempt overwrites it): %v", targetDiskPath, err)
		return
	}
	log.Printf("INFO Removed the disk %s of a failed clone", targetDiskPath)
}

// cloneOnHost clones the owner-checked source domain d into domainName on vp's
// host (c is the same host's connection, for its id). The command sequence
// mirrors the single-host Clone, with three differences: an existing domain of
// the target name that the target VirtualMachine owns is an idempotent
// success (a retry) and any other is a Conflict; the definition is stamped
// with the target VirtualMachine's identity; and the cloud-init seed ISO is
// copied for the clone (cloneSeedISO). It keeps every protection of the
// single-host Clone (disk_dependents.go, call-site checklist): linked clones
// are refused before any host is touched (cloneClustered); the definition is
// built and the disk and UEFI varstore paths checked before any file is
// written — across every host of the Provider when a file is already there
// (clusterDiskGuard, ADR-0007 A6 R3; a varstore in the host-local NVRAM
// directory keeps the host-local ensureNVRAMTargetFree check, symlink and
// in-use refusals included: ensureVarstoreFree); the disk is written on #358's path
// (createFullCopyGuarded: withUmask, private directory, `mv -T`,
// finalizeClonedDisk) and the varstore by copyClonedNVRAM. A target name held
// by a previous incarnation of the target VirtualMachine is answered
// VM_PREVIOUS_INCARNATION (R2). A source that is not shut off is refused
// before anything is read for the copy (VM_SOURCE_RUNNING,
// clone_source_state.go), and the copy reads the source as root when it may
// (privileged_copy.go).
func (p *Provider) cloneOnHost(ctx context.Context, vp *VirshProvider, c libvirtConn, d domainTarget, req contracts.CloneRequest, domainName string) (contracts.CloneResponse, error) {
	host := c.HostID()

	// The target name's lock, held from the checks to the define (ADR-0007
	// A6.1): a retried clone never checks and copies next to an earlier
	// attempt still running.
	unlock, err := p.lockDomain(ctx, domainName, guardOpClone)
	if err != nil {
		return contracts.CloneResponse{}, err
	}
	defer unlock()

	// The target name: never bound to, or redefined over, a domain the target
	// VirtualMachine does not own. One it owns was defined by an earlier
	// attempt whose answer was lost (the stamp is written by the define, the
	// last step): report it as the clone.
	target, err := checkDomainOwner(ctx, vp, host, domainName, req.TargetVM, "clone target")
	if err != nil {
		return contracts.CloneResponse{}, err
	}
	switch {
	case target.present && target.owned:
		log.Printf("INFO Clone target domain %s on host %s is already owned by VirtualMachine %s/%s (uid %s); treating the clone as done",
			domainName, host, req.TargetVM.Namespace, req.TargetVM.Name, req.TargetVM.UID)
		return contracts.CloneResponse{TargetVmID: domainName}, nil
	case target.present && target.namesOwner:
		// A previous incarnation of the target VirtualMachine (stamped for its
		// namespace and name under another UID, ADR-0007 A6 R2): the manager
		// holds the clone instead of excluding the host.
		return contracts.CloneResponse{}, &previousIncarnationError{op: guardOpClone, domain: domainName}
	case target.present:
		return contracts.CloneResponse{}, contracts.NewConflictError(fmt.Sprintf(
			"libvirt domain %q already exists on host %s and is not owned by the clone's target VirtualMachine; nothing was cloned",
			domainName, host), nil)
	}

	// A full clone requires a powered-off source (clone_source_state.go):
	// checked after the target name (a clone an earlier attempt already made
	// is reported whatever the source's state now) and before anything is
	// read for the copy or written.
	if err := refuseRunningCloneSource(ctx, vp, d.handle, d.name); err != nil {
		return contracts.CloneResponse{}, err
	}

	// The clone is defined from the source's PERSISTENT definition
	// (--inactive), not its live one (which carries runtime-only state), and
	// without any <backingStore>: that element describes the SOURCE disk's
	// backing chain, while the clone's disk is a standalone full copy. It is
	// read, and checked to be a source this clone can copy whole, before
	// anything is copied.
	srcXML, err := vp.runVirshCommand(ctx, "dumpxml", d.handle, "--inactive")
	if err != nil {
		return contracts.CloneResponse{}, contracts.NewRetryableError("failed to dump source domain XML", err)
	}
	if err := checkCloneableSource(srcXML.Stdout); err != nil {
		return contracts.CloneResponse{}, err
	}
	srcDoc, err := parseDomainDisks(srcXML.Stdout)
	if err != nil {
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError("read source domain XML for clone", err)
	}
	srcSeedISO := srcDoc.cloudInitSeedISO()
	cloneXML, err := stripBackingStores(srcXML.Stdout)
	if err != nil {
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError("read source domain XML for clone", err)
	}

	storageProvider := NewStorageProvider(vp)
	srcDiskPath, _, err := resolvePrimaryDiskOn(ctx, vp, d, storageProvider)
	if err != nil {
		return contracts.CloneResponse{}, err
	}

	poolInfo, err := storageProvider.GetPoolInfo(ctx, clonePoolName)
	if err != nil {
		return contracts.CloneResponse{}, fmt.Errorf("get pool %q info: %w", clonePoolName, err)
	}
	vp.warnIfDiskDirUnsafe(ctx, poolInfo.Path)
	targetDiskPath := filepath.Join(poolInfo.Path, fmt.Sprintf("%s.qcow2", vmDiskVolumeName(domainName)))
	// Never let the copy replace a disk a domain on ANY host of the Provider
	// uses (the pool may be shared, ADR-0007 A6 R3), or write through a
	// symbolic link.
	legacy, _ := legacyNameOf(req.TargetVM, req.TargetName, domainName)
	guard := p.newClusterDiskGuard(host, req.TargetVM, domainName, legacy, guardOpClone)
	if err := guard.ensureDiskFree(ctx, vp, domainDiskSubject(domainName), poolInfo.Path, targetDiskPath); err != nil {
		return contracts.CloneResponse{}, err
	}

	// The target definition is built — and a UEFI varstore path checked —
	// before any file is written, as on the single host, so a refusal leaves
	// nothing behind.
	targetXML, srcNvramPath, targetNvramPath, err := rewriteDomainXMLForClone(cloneXML, domainName, srcDiskPath, targetDiskPath)
	if err != nil {
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError("rewrite source domain XML for clone", err)
	}
	// rewriteDomainXMLForClone dropped the source's stamp; the clone carries
	// its own VirtualMachine's.
	targetXML, err = stampOwnerMetadata(targetXML, req.TargetVM)
	if err != nil {
		return contracts.CloneResponse{}, contracts.NewInvalidSpecError("stamp the clone with its target VirtualMachine", err)
	}
	uefi := srcNvramPath != "" && targetNvramPath != ""
	// Never write the clone's varstore through a symbolic link or over another
	// domain's varstore. In the host-local NVRAM directory the same path on
	// another host is another file, so the host-local check is the whole
	// check; a varstore that resolves anywhere else (the source's directory
	// may be shared storage) is checked on every host (ADR-0007 A6.1).
	if uefi {
		if err := guard.ensureVarstoreFree(ctx, vp, targetNvramPath); err != nil {
			return contracts.CloneResponse{}, err
		}
	}

	lock, err := p.hostLockFor(cloneLockKind, domainName)
	if err != nil {
		return contracts.CloneResponse{}, err
	}
	// Full clones only on a clustered provider (cloneClustered refuses
	// linked). A copy that fails or is stopped never reaches the disk's name:
	// it is written in a private directory, removed with it
	// (createFullCopyGuarded).
	if err := createFullCopyGuarded(ctx, vp, lock, srcDiskPath, cloneSourceFormat(srcXML.Stdout, srcDiskPath), targetDiskPath); err != nil {
		return contracts.CloneResponse{}, err
	}
	// From here the disk is a complete copy of the source's: any failure
	// before the clone's domain is defined removes it again.
	fail := func(err error) (contracts.CloneResponse, error) {
		removeClonedDisk(ctx, vp, lock, targetDiskPath)
		return contracts.CloneResponse{}, err
	}

	targetXML, seedDir, err := p.cloneSeedISO(ctx, vp, targetXML, srcSeedISO, domainName)
	if err != nil {
		return fail(err)
	}

	// The varstore is copied with #358's single-host helper: the stale target
	// unlinked, then `dd` with O_NOFOLLOW on both ends and O_EXCL on the
	// target, under a 0600 umask, and chown -h to the qemu user.
	if uefi {
		copyClonedNVRAM(ctx, vp, srcNvramPath, targetNvramPath)
	}
	targetXML = applyClassOverrides(targetXML, req.ClassJSON)
	if req.CustomizeJSON != "" {
		log.Printf("WARN Clone of %s: CustomizeJSON provided but not applied by the libvirt provider (MVP); "+
			"the clone inherits the source's hostname/cloud-init. Tracked as a follow-up to issue #153.", req.TargetName)
	}

	if err := p.defineDomainFromXML(ctx, vp, domainName, targetXML); err != nil {
		// The clone's own seed copy and its disk are removed unless the
		// domain may exist after all (it would reference them, and its
		// owner-checked Delete removes them with it), as Create does with its
		// seed.
		if !errors.Is(err, errDefineOutcomeUnknown) {
			if seedDir != "" {
				removeHostPathWithin(ctx, vp, seedDir, true, routedCleanupTimeout)
			}
			removeClonedDisk(ctx, vp, lock, targetDiskPath)
		}
		return contracts.CloneResponse{}, fmt.Errorf("define target domain: %w", err)
	}

	log.Printf("INFO Successfully cloned VM %s -> %s (libvirt domain %s on host %s, linked=%t)",
		d.name, req.TargetName, domainName, host, req.Linked)
	return contracts.CloneResponse{TargetVmID: domainName}, nil
}

// cloneSeedISO gives a clone its own copy of the source's cloud-init seed ISO
// (srcISO, "" when the source has none): a fresh per-clone seed directory in
// the host staging directory — named like Create's, so the clone's Delete
// removes it with the clone — holding cloud-init.iso, and the clone's CD-ROM
// re-pointed at it. It returns the rewritten definition and the seed directory
// ("" when there was nothing to copy). The copy runs as the SSH user, as the
// seed was written.
func (p *Provider) cloneSeedISO(ctx context.Context, vp *VirshProvider, domainXML, srcISO, domainName string) (string, string, error) {
	if srcISO == "" {
		return domainXML, "", nil
	}
	dir, err := makeHostTemp(ctx, vp, filepath.Join(p.stagingDir(), cloudInitSeedDirPrefix+domainName+"."+mktempTemplateSuffix), true)
	if err != nil {
		return "", "", fmt.Errorf("create the clone's cloud-init seed directory: %w", err)
	}
	iso := filepath.Join(dir, cloudInitISOName)
	fail := func(step string, err error) (string, string, error) {
		removeHostPathWithin(ctx, vp, dir, true, routedCleanupTimeout)
		return "", "", fmt.Errorf("%s: %w", step, err)
	}
	if _, err := runHost(ctx, vp, "cp", "--", srcISO, iso); err != nil {
		return fail("copy the cloud-init seed for the clone", err)
	}
	if _, err := runHost(ctx, vp, "chmod", cloudInitISOMode, "--", iso); err != nil {
		return fail("set the clone's cloud-init seed mode", err)
	}
	if _, err := runHost(ctx, vp, "chmod", cloudInitSeedDirMode, "--", dir); err != nil {
		return fail("set the clone's cloud-init seed directory mode", err)
	}
	out, ok := replaceSourceFile(domainXML, srcISO, iso)
	if !ok {
		return fail("re-point the clone's cloud-init CD-ROM", fmt.Errorf("seed %q not found in the clone's definition", srcISO))
	}
	return out, dir, nil
}

// cloudInitSeedISO returns the cloud-init seed ISO the domain's removable
// media reference — the first top-level file-backed cdrom/floppy source named
// by a seed convention (isCloudInitSeedName) — or "" when there is none. A
// <backingStore> source is never considered (parseDomainDisks).
func (d *domainDisksDoc) cloudInitSeedISO() string {
	for _, e := range d.Devices.Disks {
		if f := e.file(); e.isMedia() && f != "" && isCloudInitSeedName(f) {
			return f
		}
	}
	return ""
}

// backingStoreElement is libvirt's element describing a disk's backing chain.
const backingStoreElement = "backingStore"

// backingStorePath is where stripBackingStores removes backingStore elements:
// the direct children of a domain's disks, /domain/devices/disk/backingStore,
// none in an XML namespace.
var backingStorePath = []string{"domain", "devices", "disk"}

// stripBackingStores removes every /domain/devices/disk/backingStore element
// (with the chain nested inside it, self-closing ones included) from a domain
// document by splicing out exactly the bytes each occupies; every other byte
// is kept (the document is tokenized, never re-serialized). An element of that
// name anywhere else — in another tool's <metadata>, or in an XML namespace —
// is left alone.
func stripBackingStores(domainXML string) (string, error) {
	dec := xml.NewDecoder(strings.NewReader(domainXML))
	type span struct{ start, end int64 }
	var spans []span
	var stack []xml.Name
	inStore := 0
	var start int64
	for {
		offset := dec.InputOffset()
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("parse domain XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if inStore == 0 && t.Name == (xml.Name{Local: backingStoreElement}) && atUnqualifiedPath(stack, backingStorePath) {
				inStore, start = len(stack)+1, offset
			}
			stack = append(stack, t.Name)
		case xml.EndElement:
			if inStore != 0 && len(stack) == inStore {
				spans = append(spans, span{start: start, end: dec.InputOffset()})
				inStore = 0
			}
			stack = stack[:len(stack)-1]
		}
	}
	out := domainXML
	for i := len(spans) - 1; i >= 0; i-- {
		out = out[:spans[i].start] + out[spans[i].end:]
	}
	return out, nil
}

// atUnqualifiedPath reports whether the open elements stack are exactly path,
// every one without an XML namespace.
func atUnqualifiedPath(stack []xml.Name, path []string) bool {
	if len(stack) != len(path) {
		return false
	}
	for i, name := range stack {
		if name != (xml.Name{Local: path[i]}) {
			return false
		}
	}
	return true
}

// cloneSourceDisks is the part of a domain document checkCloneableSource
// reads: its disks, their device kind and whether their source names an
// external data file.
type cloneSourceDisks struct {
	XMLName xml.Name `xml:"domain"`
	Disks   []struct {
		Device string `xml:"device,attr"`
		Source *struct {
			DataStore *struct{} `xml:"dataStore"`
		} `xml:"source"`
	} `xml:"devices>disk"`
}

// checkCloneableSource refuses (InvalidSpec) a clustered clone of a source
// whose definition the clone could not copy whole: rewriteDomainXMLForClone
// re-points only the PRIMARY disk at the clone's copy, so any other writable
// medium (of any kind: file, block or network; device disk, lun or floppy)
// would be shared by the source and the clone, and a disk whose <source>
// names an external data file (<dataStore>) would keep reading the source's
// data. Multi-disk clones are a later slice. Only CD-ROM media are not
// counted: they are read-only, and the cloud-init seed among them is copied
// for the clone.
func checkCloneableSource(domainXML string) error {
	var doc cloneSourceDisks
	if err := xml.Unmarshal([]byte(domainXML), &doc); err != nil {
		return contracts.NewInvalidSpecError("read source domain XML for clone", err)
	}
	disks := 0
	for _, disk := range doc.Disks {
		if disk.Device == diskDeviceCDROM {
			continue
		}
		disks++
		if disk.Source != nil && disk.Source.DataStore != nil {
			return contracts.NewInvalidSpecError(
				"clustered libvirt clone does not support a source disk with an external data file (<dataStore>)", nil)
		}
	}
	if disks > 1 {
		return contracts.NewInvalidSpecError(fmt.Sprintf(
			"clustered libvirt clone supports a source VM with one disk; this one has %d writable disk or floppy "+
				"devices (multi-disk clone is not supported yet)", disks), nil)
	}
	return nil
}

// replaceSourceFile re-points the first <source file=...> attribute naming from
// (either quote style) at to, XML-escaped. ok is false when none names from.
func replaceSourceFile(domainXML, from, to string) (string, bool) {
	for _, q := range []string{"'", `"`} {
		old := "file=" + q + from + q
		if strings.Contains(domainXML, old) {
			return strings.Replace(domainXML, old, "file="+q+xmlEscape(to)+q, 1), true
		}
	}
	return domainXML, false
}
