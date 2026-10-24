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

// Read-only disks and snapshots (v0.4.0-rc6 lab finding V6).
//
// A VirtualMachine with cloud-init has a read-only seed CD-ROM (a raw ISO in
// its private seed directory). Without a <disks> list, `virsh
// snapshot-create-as --disk-only` makes EVERY disk that is not empty an
// external snapshot disk — the CD-ROM included: libvirt's
// virDomainSnapshotAlignDisks has no read-only or CD-ROM exception, only one
// for empty drives. Creating and deleting such a snapshot is harmless, but
// reverting it (libvirt >= 9.9 reverts external snapshots) makes a NEW qcow2
// overlay for every external disk of the snapshot, named
// <source file without its extension>.<epoch>: the CD-ROM is rewritten to a
// qcow2 overlay of the ISO inside the seed directory, which the host's
// AppArmor profile for the domain (virt-aa-helper) does not let QEMU open, so
// the VM can no longer start.
//
// Hence:
//
//   - SnapshotCreate excludes every read-only disk and every CD-ROM or floppy
//     with `--diskspec <target>,snapshot=no` (snapshotExcludedTargets), for a
//     disk-only and a memory snapshot alike. A disk that has no diskspec keeps
//     libvirt's default: external for --disk-only, with the default external
//     name <source file without its extension>.<snapshot>, so the writable
//     disks' overlays keep the <domain>-disk.<snapshot> name the overlay
//     cleanup relies on (snapshot_overlay_cleanup.go). For a memory (internal)
//     snapshot of a running domain, excluding a read-only disk is what lets
//     libvirt take it at all: an internal snapshot of a raw disk is refused,
//     and a read-only disk with snapshot=no does not count as an excluded
//     writable disk ("internal and full system snapshots require all disks to
//     be selected").
//   - SnapshotRevert refuses, before it changes anything, a snapshot that
//     records a read-only disk or removable media of the domain as an
//     external snapshot disk (one taken by v0.4.0-rc6 or earlier):
//     snapshotRevertUnsafeError, codes.FailedPrecondition.
//   - After a revert it checks that the domain's read-only disks and media
//     still have a source the domain or the snapshot had for them
//     (checkRevertKeptReadOnlyDisks); a revert that rewrote one is answered
//     as a failure (snapshotRevertBrokeDisksError) rather than a success.

// diskTargetRE is the shape of a libvirt disk target name (vda, hdc, sdb,
// xvdaa, fda): what a --diskspec may carry without quoting. A target of any
// other shape is never put in an argument.
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
// domain's read-only disks, CD-ROMs and floppies: the disks a snapshot must
// exclude (--diskspec <target>,snapshot=no). Such a disk whose target is not
// a plain libvirt target name is an error: it cannot be excluded safely, so no
// snapshot is taken.
func (d *domainDisksDoc) snapshotExcludedTargets() ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, e := range d.Devices.Disks {
		if !e.excludedFromSnapshots() {
			continue
		}
		t := e.target()
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
// must leave in place (checkRevertKeptReadOnlyDisks). A source is the file,
// the device, or pool/volume of the element's own <source>.
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

// sourceRef is the element's own source as one comparable string: its file,
// its device, or pool/volume; "" when it has none.
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

// snapshotExcludedTargetsOf reads the domain's definition on h's host and
// returns its snapshotExcludedTargets.
func snapshotExcludedTargetsOf(ctx context.Context, h hostCommandRunner, domain string) ([]string, error) {
	doc, err := readDomainDisks(ctx, h, domain)
	if err != nil {
		return nil, err
	}
	return doc.snapshotExcludedTargets()
}

// snapshotDiskModes is the part of `virsh snapshot-dumpxml` the revert check
// reads: the snapshot's disk list, the disks a revert with children recorded
// (<revertDisks>, libvirt >= 9.9), and the domain definition the snapshot
// recorded.
type snapshotDiskModes struct {
	XMLName     xml.Name           `xml:"domainsnapshot"`
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
// (domain) or as the snapshot recorded it. A name matches a disk by its
// target or by its source file (libvirt also accepts a disk's path as its
// name).
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

// recordedReadOnlySources returns the read-only disk sources (by target) of
// the domain definition the snapshot recorded, or nil when it recorded none.
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

// snapshotRevertUnsafeError refuses a revert to a snapshot that records a
// read-only disk or removable media of the domain as an external snapshot
// disk (a snapshot taken by v0.4.0-rc6 or earlier): libvirt would rewrite
// that disk to a new overlay the VM cannot open. Nothing was changed. Its
// message names only the snapshot and the disks' target names (never a host
// path).
type snapshotRevertUnsafeError struct {
	// snapshot is the refused snapshot's name.
	snapshot string
	// disks are the read-only disks the snapshot records as external.
	disks []string
}

// Error is the refusal, safe for the requesting VM's status. It is short on
// purpose: the manager keeps 256 bytes of a provider's answer in the VM's
// status, so the guidance comes first (a long snapshot name only cuts the
// tail) and the domain (known from the VirtualMachine) is left to the
// provider log.
func (e *snapshotRevertUnsafeError) Error() string {
	return fmt.Sprintf("revert refused, nothing changed: take a new snapshot and delete this VMSnapshot. "+
		"Snapshot %q is from an earlier release and includes read-only disk %s (the VM would not start)",
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
// a definite, non-retried answer (the revert must not be sent again) that the
// manager reports as RevertFailed, out of the circuit breaker.
func (e *snapshotRevertBrokeDisksError) GRPCStatus() *status.Status {
	return status.New(codes.FailedPrecondition, e.Error())
}

// revertReadOnlyBaseline is what checkSnapshotRevertSafe read before a
// revert, for checkRevertKeptReadOnlyDisks after it.
type revertReadOnlyBaseline struct {
	// before are the domain's read-only disk sources before the revert.
	before map[string]string
	// recorded are those of the domain definition the snapshot recorded.
	recorded map[string]string
}

// checkSnapshotRevertSafe reads, before a revert of domain to snapshotID on
// h's host, the snapshot's definition and the domain's, and refuses
// (snapshotRevertUnsafeError) a snapshot that records a read-only disk or
// removable media as external. A definition that cannot be read or parsed is
// an error too: whether the revert is safe cannot be known, so it is not
// made. On success it returns the read-only disk sources the revert must
// leave in place.
func checkSnapshotRevertSafe(ctx context.Context, h hostCommandRunner, domain, snapshotID string) (*revertReadOnlyBaseline, error) {
	res, err := h.runVirshCommand(ctx, "snapshot-dumpxml", domain, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("failed to revert to snapshot: read the snapshot definition: %w", err)
	}
	snap, err := parseSnapshotDiskModes(res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("failed to revert to snapshot: %w", err)
	}
	doc, err := readDomainDisks(ctx, h, domain)
	if err != nil {
		return nil, fmt.Errorf("failed to revert to snapshot: %w", err)
	}
	if unsafe := snap.externalReadOnlyDisks(doc); len(unsafe) > 0 {
		e := &snapshotRevertUnsafeError{snapshot: snapshotID, disks: unsafe}
		log.Printf("WARN Revert of libvirt domain %s refused: snapshot %s records its read-only disk(s) %v as external "+
			"snapshot disks (taken by v0.4.0-rc6 or earlier); libvirt would rewrite them to new overlays the VM cannot open",
			domain, snapshotID, unsafe)
		return nil, e
	}
	return &revertReadOnlyBaseline{before: doc.readOnlyDiskSources(), recorded: snap.recordedReadOnlySources()}, nil
}

// checkRevertKeptReadOnlyDisks checks, after a successful revert of domain to
// snapshotID, that each of the domain's read-only disks and removable media
// has a source it had before the revert or that the snapshot recorded for it
// (or none). A disk on any other source is a disk libvirt rewrote:
// snapshotRevertBrokeDisksError. A definition that cannot be read is logged
// and the revert is answered as the success it was: the snapshot was checked
// before the revert (checkSnapshotRevertSafe), so this check is a second line
// of defence, not the gate.
func checkRevertKeptReadOnlyDisks(ctx context.Context, h hostCommandRunner, domain, snapshotID string, base *revertReadOnlyBaseline) error {
	if base == nil {
		return nil
	}
	doc, err := readDomainDisks(ctx, h, domain)
	if err != nil {
		log.Printf("WARN Could not read domain %s after its revert to snapshot %s; its read-only disks were not re-checked: %v",
			domain, snapshotID, err)
		return nil
	}
	var rewritten []string
	for target, src := range doc.readOnlyDiskSources() {
		if src == "" {
			continue
		}
		if prev, ok := base.before[target]; ok && prev == src {
			continue
		}
		if rec, ok := base.recorded[target]; ok && rec == src {
			continue
		}
		rewritten = append(rewritten, target)
	}
	if len(rewritten) == 0 {
		return nil
	}
	sort.Strings(rewritten)
	e := &snapshotRevertBrokeDisksError{snapshot: snapshotID, disks: rewritten}
	log.Printf("ERROR Libvirt domain %s was reverted to snapshot %s, but its read-only disk(s) %v are no longer on a "+
		"source the domain or the snapshot had for them: %v", domain, snapshotID, rewritten, doc.readOnlyDiskSources())
	return e
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
