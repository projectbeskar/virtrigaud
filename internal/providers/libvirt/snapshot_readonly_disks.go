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
	"regexp"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Read-only disks and snapshot revert (v0.4.0-rc6 lab finding V6).
//
// libvirt (>= 9.9) reverts an external snapshot by rebuilding the domain's
// disks from the domain definition the snapshot stored and creating a new
// qcow2 overlay, <source file without its extension>.<epoch>, for every disk
// of that definition that has media — the read-only cloud-init seed CD-ROM
// included, whatever its snapshot mode (libvirt records a read-only CD-ROM
// 'no' by itself). The CD-ROM is then a qcow2 overlay in the VM's private seed
// directory that the host's AppArmor profile does not let QEMU open, and the
// VM no longer starts; libvirt also rewrites the snapshot's metadata to record
// that overlay as an external snapshot disk.
//
// Hence:
//
//   - SnapshotCreate of a shut-off domain, disk-only, takes the snapshot with
//     its removable media ejected (snapshot_media.go), so the stored
//     definition's drives are empty. A read-only disk or drive that still has
//     a source (a running domain, a memory snapshot, a block or network
//     medium, a read-only data disk) is excluded with `--diskspec
//     <target>,snapshot=no` (snapshotExcludedTargets): harmless, and it keeps
//     the disk out of the snapshot's disk list — but the snapshot stores its
//     media and cannot be reverted. For a memory (internal) snapshot the
//     exclusion is what lets libvirt take it: an internal snapshot of a raw
//     disk is refused, and a read-only disk with snapshot=no does not count
//     as an excluded writable disk. Writable disks get no diskspec and keep
//     libvirt's defaults (for --disk-only: external, named
//     <domain>-disk.<snapshot>, which the overlay cleanup relies on).
//   - SnapshotRevert refuses, before it changes anything, an external
//     snapshot whose stored definition has a read-only disk or drive with a
//     source, or a snapshot that records one as an external snapshot disk
//     (snapshotRevertGate, snapshotRevertUnsafeError,
//     codes.FailedPrecondition): a disk-only one taken while the VM ran, or by
//     v0.4.0-rc6 or earlier. An internal (memory) snapshot is reverted in
//     place by libvirt, without overlays, and is not refused for its media.
//   - After a revert it re-inserts each medium the domain had before it into
//     its now empty drive (finishSnapshotRevert), and checks that no read-only
//     disk was moved to a new file (snapshotRevertBrokeDisksError).

// diskTargetRE is the shape of a libvirt disk target name (vda, hdc, sdb,
// xvdaa, fda): what a --diskspec or change-media may carry without quoting. A
// target of any other shape is never put in an argument.
var diskTargetRE = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`)

// diskspecNoSnapshot is the --diskspec suffix that excludes a disk from a
// snapshot.
const diskspecNoSnapshot = ",snapshot=no"

// excludedFromSnapshots reports whether the element must never be part of a
// snapshot: a CD-ROM or floppy, or a disk marked <readonly/>.
func (e domainDiskElem) excludedFromSnapshots() bool {
	return e.isMedia() || e.ReadOnly != nil
}

// target returns the element's own <target dev=...>, or "".
func (e domainDiskElem) target() string {
	if e.Target == nil {
		return ""
	}
	return strings.TrimSpace(e.Target.Dev)
}

// snapshotExcludedTargets returns the target names, in document order, of the
// domain's read-only disks, CD-ROMs and floppies that have a source and are
// not in ejected: the disks a snapshot excludes with --diskspec
// <target>,snapshot=no. Such a disk whose target is not a plain libvirt target
// name is an error: it cannot be excluded safely, so no snapshot is taken.
// Empty drives need no diskspec (libvirt never snapshots an empty drive).
func (d *domainDisksDoc) snapshotExcludedTargets(ejected []removableMedium) ([]string, error) {
	skip := map[string]bool{}
	for _, m := range ejected {
		skip[m.Target] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, e := range d.Devices.Disks {
		if !e.excludedFromSnapshots() || e.sourceRef() == "" {
			continue
		}
		t := e.target()
		if skip[t] {
			continue
		}
		if !diskTargetRE.MatchString(t) {
			return nil, fmt.Errorf("a read-only or removable disk of the domain has target %q, which cannot be excluded from a snapshot", t)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, nil
}

// readOnlyDiskSources returns, keyed by target, the source of every read-only
// disk, CD-ROM and floppy of the domain ("" for empty media): what a revert
// must leave in place (checkRevertKeptReadOnlyDisks).
func (d *domainDisksDoc) readOnlyDiskSources() map[string]string {
	out := map[string]string{}
	for _, e := range d.Devices.Disks {
		if !e.excludedFromSnapshots() || e.target() == "" {
			continue
		}
		out[e.target()] = e.sourceRef()
	}
	return out
}

// loadedReadOnlyDisks returns, sorted, the targets (or, without one, the
// source) of the read-only disks, CD-ROMs and floppies that have a source.
func (d *domainDisksDoc) loadedReadOnlyDisks() []string {
	var out []string
	for _, e := range d.Devices.Disks {
		if !e.excludedFromSnapshots() || e.sourceRef() == "" {
			continue
		}
		name := e.target()
		if name == "" {
			name = e.sourceRef()
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sourceRef is the element's own source as one comparable string: its file,
// its device, pool/volume, or protocol:name of a network source; "" when it
// has none (an empty drive).
func (e domainDiskElem) sourceRef() string {
	if e.Source == nil {
		return ""
	}
	switch {
	case e.Source.File != "":
		return e.Source.File
	case e.Source.Dev != "":
		return e.Source.Dev
	case e.Source.Pool != "" || e.Source.Volume != "":
		return e.Source.Pool + "/" + e.Source.Volume
	case e.Source.Protocol != "" || e.Source.Name != "":
		return e.Source.Protocol + ":" + e.Source.Name
	}
	return ""
}

// readDomainDisks reads the definition of domain (a name or UUID) on h's host.
func readDomainDisks(ctx context.Context, h hostCommandRunner, domain string) (*domainDisksDoc, error) {
	res, err := h.runVirshCommand(ctx, "dumpxml", domain)
	if err != nil {
		return nil, fmt.Errorf("read the domain definition: %w", err)
	}
	return parseDomainDisks(res.Stdout)
}

// snapshotDiskModes is the part of `virsh snapshot-dumpxml` the revert gate
// reads: the snapshot's disk list, the disks a revert with children recorded
// (<revertDisks>, libvirt >= 9.9), and the domain definition the snapshot
// stored.
type snapshotDiskModes struct {
	XMLName xml.Name `xml:"domainsnapshot"`
	// Memory is the snapshot's <memory snapshot=...> mode.
	Memory *struct {
		Snapshot string `xml:"snapshot,attr"`
	} `xml:"memory"`
	Disks       []snapshotDiskMode `xml:"disks>disk"`
	RevertDisks []snapshotDiskMode `xml:"revertDisks>disk"`
	Domain      *domainDisksDoc    `xml:"domain"`
}

// snapshotDiskMode is one <disk> of a snapshot's disk list.
type snapshotDiskMode struct {
	Name     string `xml:"name,attr"`
	Snapshot string `xml:"snapshot,attr"`
	Source   struct {
		File string `xml:"file,attr"`
	} `xml:"source"`
}

// externalReadOnlyDisks returns, sorted, the names of the snapshot's external
// disks that are read-only disks or removable media of the domain — current
// (domain) or as the snapshot stored it. A name matches a disk by its target
// or by its source file (libvirt also accepts a disk's path as its name).
func (s *snapshotDiskModes) externalReadOnlyDisks(domain *domainDisksDoc) []string {
	readOnly := map[string]bool{}
	for _, doc := range []*domainDisksDoc{domain, s.Domain} {
		if doc == nil {
			continue
		}
		for _, e := range doc.Devices.Disks {
			if !e.excludedFromSnapshots() {
				continue
			}
			if t := e.target(); t != "" {
				readOnly[t] = true
			}
			if f := e.file(); f != "" {
				readOnly[f] = true
			}
		}
	}
	found := map[string]bool{}
	for _, d := range append(append([]snapshotDiskMode(nil), s.Disks...), s.RevertDisks...) {
		name := strings.TrimSpace(d.Name)
		if d.Snapshot == snapshotExternalOverlay && readOnly[name] {
			found[name] = true
		}
	}
	out := make([]string, 0, len(found))
	for n := range found {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// external reports whether libvirt reverts the snapshot as an external one
// (virDomainSnapshotIsExternal): its memory, or any disk of its disk lists, is
// external. An internal snapshot (a memory snapshot of a running VM) is
// reverted in place, without new overlays.
func (s *snapshotDiskModes) external() bool {
	if s.Memory != nil && s.Memory.Snapshot == snapshotExternalOverlay {
		return true
	}
	for _, d := range append(append([]snapshotDiskMode(nil), s.Disks...), s.RevertDisks...) {
		if d.Snapshot == snapshotExternalOverlay {
			return true
		}
	}
	return false
}

// snapshotRevertGate returns, sorted and without duplicates, the read-only
// disks and drives that make the snapshot unsafe to revert with libvirt: for
// an external snapshot, those its stored definition shows with a source
// (libvirt would make an overlay of each); and those its disk lists record as
// external snapshot disks. An external snapshot without a stored definition
// cannot be checked: an error.
func (s *snapshotDiskModes) snapshotRevertGate(domain *domainDisksDoc) ([]string, error) {
	set := map[string]bool{}
	if s.external() {
		if s.Domain == nil {
			return nil, fmt.Errorf("the snapshot's stored domain definition cannot be read")
		}
		for _, n := range s.Domain.loadedReadOnlyDisks() {
			set[n] = true
		}
	}
	for _, n := range s.externalReadOnlyDisks(domain) {
		set[n] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// recordedReadOnlySources returns the read-only disk sources (by target) of
// the domain definition the snapshot stored, or nil when it stored none.
func (s *snapshotDiskModes) recordedReadOnlySources() map[string]string {
	if s.Domain == nil {
		return nil
	}
	return s.Domain.readOnlyDiskSources()
}

// parseSnapshotDiskModes decodes a `virsh snapshot-dumpxml` document.
func parseSnapshotDiskModes(snapshotXML string) (*snapshotDiskModes, error) {
	var s snapshotDiskModes
	if err := xml.Unmarshal([]byte(snapshotXML), &s); err != nil {
		return nil, fmt.Errorf("parse snapshot definition: %w", err)
	}
	return &s, nil
}

// snapshotRevertUnsafeError refuses a revert to a snapshot that libvirt
// cannot revert without making an overlay of a read-only disk or drive the VM
// then cannot open (snapshotRevertGate): one taken while the VM was running,
// or by v0.4.0-rc6 or earlier. Nothing was changed. Its message names only the
// snapshot and the disks' target names (never a host path).
type snapshotRevertUnsafeError struct {
	// snapshot is the refused snapshot's name.
	snapshot string
	// disks are the read-only disks and drives that make it unsafe.
	disks []string
}

// Error is the refusal, safe for the requesting VM's status. It is short on
// purpose: the manager keeps 256 bytes of a provider's answer in the VM's
// status, so the guidance comes first (a long snapshot name only cuts the
// tail) and the domain (known from the VirtualMachine) is left to the
// provider log.
func (e *snapshotRevertUnsafeError) Error() string {
	return fmt.Sprintf("revert refused, nothing changed: take a new snapshot with the VM powered off. "+
		"Snapshot %q was taken while it ran or by an earlier release; libvirt cannot revert it safely (disk %s)",
		e.snapshot, strings.Join(e.disks, ","))
}

// GRPCStatus renders the refusal as codes.FailedPrecondition (no ErrorInfo):
// the manager maps it to a terminal, non-retried failure (RevertFailed) kept
// out of the circuit breaker. status.FromError finds it through any
// fmt.Errorf wrapping.
func (e *snapshotRevertUnsafeError) GRPCStatus() *status.Status {
	return status.New(codes.FailedPrecondition, e.Error())
}

// snapshotRevertBrokeDisksError reports a revert that libvirt performed but
// that left one of the domain's read-only disks or removable media on a
// source neither the domain nor the snapshot had for it: the VM was reverted,
// and may not start. Its message names only the snapshot and the disks'
// target names.
type snapshotRevertBrokeDisksError struct {
	// snapshot is the snapshot reverted to.
	snapshot string
	// disks are the rewritten disks' targets.
	disks []string
}

// Error is the failure, safe for the requesting VM's status (short, as
// snapshotRevertUnsafeError's).
func (e *snapshotRevertBrokeDisksError) Error() string {
	return fmt.Sprintf("reverted, but the VM may not start: libvirt moved read-only disk %s to a new file; repair it "+
		"before another revert (see the snapshot revert documentation). Snapshot %q", strings.Join(e.disks, ","), e.snapshot)
}

// GRPCStatus renders the failure as codes.FailedPrecondition (no ErrorInfo):
// a definite, non-retried answer that the manager reports as RevertFailed,
// out of the circuit breaker.
func (e *snapshotRevertBrokeDisksError) GRPCStatus() *status.Status {
	return status.New(codes.FailedPrecondition, e.Error())
}

// snapshotRevertMediaError reports a revert that libvirt performed after
// which the domain's removable media could not be re-inserted: the VM was
// reverted, and its drives (its cloud-init seed) are empty. The record of the
// media stays, so the next power-on or snapshot call re-inserts a cloud-init
// seed (restoreEjectedMedia).
type snapshotRevertMediaError struct {
	// snapshot is the snapshot reverted to.
	snapshot string
	// drives are the drives left empty.
	drives []string
	// cause is logged, never sent.
	cause error
}

// Error is the failure, safe for the requesting VM's status.
func (e *snapshotRevertMediaError) Error() string {
	return fmt.Sprintf("reverted, but CD-ROM %s could not be re-inserted; the next power-on re-inserts a cloud-init "+
		"seed, or insert it by hand (see the snapshot revert documentation). Snapshot %q", strings.Join(e.drives, ","), e.snapshot)
}

// Unwrap exposes the cause, for errors.Is/As only.
func (e *snapshotRevertMediaError) Unwrap() error { return e.cause }

// GRPCStatus renders the failure as codes.FailedPrecondition (no ErrorInfo):
// the revert happened and must not be sent again.
func (e *snapshotRevertMediaError) GRPCStatus() *status.Status {
	return status.New(codes.FailedPrecondition, e.Error())
}

// revertPlan is what prepareSnapshotRevert read before a revert, for
// finishSnapshotRevert after it.
type revertPlan struct {
	// before are the domain's read-only disk sources before the revert.
	before map[string]string
	// recorded are those of the domain definition the snapshot stored.
	recorded map[string]string
	// media are the domain's removable media before the revert, to insert
	// again into the drives the revert leaves empty.
	media []removableMedium
	// sources are all the domain's disk sources before the revert
	// (diskSourcesKey): a refused revert is proven to have changed nothing
	// only while they are unchanged (classifyRevertFailure).
	sources string
	// internal reports that the snapshot is an internal one (its memory or a
	// disk recorded 'internal'): libvirt reverts such disks in place, one at
	// a time, so unchanged disk sources prove nothing (classifyRevertFailure).
	internal bool
}

// prepareSnapshotRevert runs, before a revert of domain to snapshotID on h's
// host, the repair of an interrupted snapshot's ejection
// (repairEjectedMediaOf), then reads the snapshot's definition and refuses
// (snapshotRevertUnsafeError) a snapshot snapshotRevertGate finds unsafe. A
// definition that cannot be read or parsed is an error too: whether the
// revert is safe cannot be known, so it is not made. It returns what the
// revert must restore and leave in place.
func prepareSnapshotRevert(ctx context.Context, h hostCommandRunner, domain, snapshotID string, roots []string) (*revertPlan, error) {
	doc, err := repairEjectedMediaOf(ctx, h, domain, roots)
	if err != nil {
		return nil, fmt.Errorf("failed to revert to snapshot: %w", err)
	}
	res, err := h.runVirshCommand(ctx, "snapshot-dumpxml", domain, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("failed to revert to snapshot: read the snapshot definition: %w", err)
	}
	snap, err := parseSnapshotDiskModes(res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("failed to revert to snapshot: %w", err)
	}
	unsafe, err := snap.snapshotRevertGate(doc)
	if err != nil {
		return nil, fmt.Errorf("failed to revert to snapshot: %w", err)
	}
	if len(unsafe) > 0 {
		log.Printf("WARN Revert of libvirt domain %s refused: snapshot %s stores read-only disk(s) %v with their media "+
			"(taken while the VM ran, or by v0.4.0-rc6 or earlier); libvirt would rewrite them to new overlays the VM cannot open",
			domain, snapshotID, unsafe)
		return nil, &snapshotRevertUnsafeError{snapshot: snapshotID, disks: unsafe}
	}
	return &revertPlan{before: doc.readOnlyDiskSources(), recorded: snap.recordedReadOnlySources(), media: doc.ejectableMedia(),
		sources: doc.diskSourcesKey(), internal: snap.isInternal()}, nil
}

// isInternal reports whether the snapshot is internal: its memory, or any of
// its disks, is recorded snapshot='internal'.
func (s *snapshotDiskModes) isInternal() bool {
	if s.Memory != nil && strings.TrimSpace(s.Memory.Snapshot) == snapshotInternalMode {
		return true
	}
	for _, d := range s.Disks {
		if strings.TrimSpace(d.Snapshot) == snapshotInternalMode {
			return true
		}
	}
	return false
}

// finishSnapshotRevert runs after a successful revert of domain to
// snapshotID: it re-inserts each medium the domain had before the revert
// (plan.media) whose drive the revert left empty — recording them first
// (setMediaMarker), so a failure is repaired later — in the persistent
// definition, and in the running domain too when live; it removes a record
// the revert restored from the snapshot's stored definition when nothing is
// left to insert; and it checks that no read-only disk was moved to a new
// file (checkRevertKeptReadOnlyDisks). A domain that cannot be read after the
// revert gets the record of every pre-revert medium, so the repair inserts a
// cloud-init seed into a drive left empty, and the answer says so.
func finishSnapshotRevert(ctx context.Context, h hostCommandRunner, domain, snapshotID string, plan *revertPlan, live bool) error {
	after, err := readDomainDisks(ctx, h, domain)
	if err != nil {
		if len(plan.media) == 0 {
			log.Printf("WARN Could not read domain %s after its revert to snapshot %s; its read-only disks were not re-checked: %v",
				domain, snapshotID, err)
			return nil
		}
		if merr := setMediaMarker(ctx, h, domain, plan.media); merr != nil {
			log.Printf("ERROR %v", merr)
		}
		return &snapshotRevertMediaError{snapshot: snapshotID, drives: targetsOf(plan.media), cause: err}
	}
	var empty []removableMedium
	for _, m := range plan.media {
		if e, ok := after.diskByTarget(m.Target); ok && e.isMedia() && e.sourceRef() == "" {
			empty = append(empty, m)
		}
	}
	switch {
	case len(empty) > 0:
		if err := setMediaMarker(ctx, h, domain, empty); err != nil {
			log.Printf("ERROR %v", err)
		}
		if err := reinsertMedia(ctx, h, domain, empty, live); err != nil {
			return &snapshotRevertMediaError{snapshot: snapshotID, drives: targetsOf(empty), cause: err}
		}
	case after.hasMediaMarker():
		// The record came back with the snapshot's stored definition, and
		// nothing is left to insert.
		if err := clearMediaMarker(ctx, h, domain); err != nil {
			log.Printf("WARN %v", err)
		}
	}
	return checkRevertKeptReadOnlyDisks(after, domain, snapshotID, plan)
}

// checkRevertKeptReadOnlyDisks checks the domain's definition read after a
// revert of domain to snapshotID: each read-only disk and removable medium
// must have a source it had before the revert or that the snapshot stored for
// it (or none: a drive re-inserted by finishSnapshotRevert). A disk on any
// other source is a disk libvirt rewrote: snapshotRevertBrokeDisksError.
func checkRevertKeptReadOnlyDisks(after *domainDisksDoc, domain, snapshotID string, plan *revertPlan) error {
	var rewritten []string
	for target, src := range after.readOnlyDiskSources() {
		if src == "" {
			continue
		}
		if prev, ok := plan.before[target]; ok && prev == src {
			continue
		}
		if rec, ok := plan.recorded[target]; ok && rec == src {
			continue
		}
		rewritten = append(rewritten, target)
	}
	if len(rewritten) == 0 {
		return nil
	}
	sort.Strings(rewritten)
	log.Printf("ERROR Libvirt domain %s was reverted to snapshot %s, but its read-only disk(s) %v are no longer on a "+
		"source the domain or the snapshot had for them: %v", domain, snapshotID, rewritten, after.readOnlyDiskSources())
	return &snapshotRevertBrokeDisksError{snapshot: snapshotID, disks: rewritten}
}

// snapshotDiskspecArgs returns the --diskspec arguments that exclude targets
// from a snapshot.
func snapshotDiskspecArgs(targets []string) []string {
	out := make([]string, 0, 2*len(targets))
	for _, t := range targets {
		out = append(out, "--diskspec", t+diskspecNoSnapshot)
	}
	return out
}
