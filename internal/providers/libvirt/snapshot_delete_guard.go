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
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Snapshot delete and the VM's other snapshots (v0.4.0-rc8 lab finding V11).
//
// A memory snapshot of a running VM (includeMemory=true) is an INTERNAL
// snapshot: libvirt stores the guest's RAM and the disks' state inside the
// qcow2 file each disk was using when it was taken. When that file is the
// overlay of an earlier disk-only (external) snapshot, deleting the external
// snapshot destroys the memory snapshot: libvirt (>= 9.0; checked against
// v10.0.0, src/qemu/qemu_snapshot.c) deletes an external snapshot by
// block-committing the file it merges away — the first child's recorded disk
// source, or the domain's current one, or the snapshot's own overlay
// (qemuSnapshotExternalGetSnapDiskSrc) — into its backing file with
// VIR_DOMAIN_BLOCK_COMMIT_DELETE, which removes that file
// (qemuSnapshotDiscardExternal). Its checks (qemuSnapshotDeleteValidate,
// qemuSnapshotDeleteExternalPrepareData) refuse an internal PARENT
// ("deleting external snapshot that has internal snapshot as parent not
// supported") but nothing about an internal snapshot stored IN the merged
// file, and it updates the recorded disk sources of external snapshots only
// ("There is no need to do it for internal snapshots as they don't create new
// disk files", qemuSnapshotUpdateDisksSingle). The memory snapshot's data is
// gone while its metadata stays, recording a file that no longer exists, and
// every later delete of it fails while the VM runs ("disk image 'vda' for
// internal snapshot 'mem' is not the same as disk image currently used by
// VM", qemuSnapshotDeleteValidate).
//
// SnapshotDelete therefore refuses, before it changes anything, to delete an
// external snapshot while any other snapshot is stored in a file the delete
// may merge away or remove (refuseIfDeleteLosesSnapshots). Those files are,
// for each of the snapshot's external disks: its overlay as recorded (its
// <disks> and <revertDisks> sources), the source each of its child snapshots
// recorded for that disk (what a merge commits when it has children), and the
// source the snapshot itself recorded for that disk (what libvirt unlinks for
// a leaf that is not the current snapshot; when the snapshot is current or has
// children this is the base a merge commits INTO, whose own internal snapshot
// libvirt refuses as an internal parent anyway). A snapshot is stored in such
// a file when:
//
//   - libvirt's metadata says so: another snapshot of the domain with an
//     internal disk whose recorded source is that file; or
//   - the file's own header says so: `qemu-img info -U -f <format>
//     --output=json` of the file — the root read of the disk in-use check,
//     qemuImgInfoOnHost — lists an internal snapshot (ground truth, whatever
//     libvirt's metadata records).
//
// The refusal is definite (FailedPrecondition, snapshotDeleteLosesSnapshotsError)
// and names the blocking snapshots — the same VM's — by name only. A check
// that cannot be completed (a snapshot or definition that cannot be read or
// parsed, an external disk whose overlay is not a file, an image that cannot
// be read) refuses the delete too, retryably (VM_DISK_CHECK_FAILED,
// snapshotDeleteCheckFailed). A file that does not exist holds nothing.
//
// A memory snapshot already orphaned this way (by v0.4.0-rc8 or earlier) is
// handled after libvirt refuses its delete with "is not the same as disk
// image currently used by VM" (a validation before anything changes): when
// the provider proves its data is gone — every file it was taken on no longer
// exists, and no image of the domain's current disk chains lists it — it
// deletes the snapshot's metadata only (`virsh snapshot-delete <domain>
// <snapshot> --metadata`), which touches no disk; otherwise it answers a
// definite refusal (snapshotDeleteStaleInternalError: power the VM off and
// delete again — libvirt then deletes an internal snapshot from the files it
// recorded with `qemu-img snapshot -d`).

// snapshotInternalMode is the snapshot-disk (and memory) mode of a snapshot
// libvirt stores inside the disk images.
const snapshotInternalMode = "internal"

// qcow2Format is the qcow2 image format: the only one that stores internal
// snapshots, and the one libvirt makes external snapshot overlays in when a
// snapshot's disk names none.
const qcow2Format = "qcow2"

// staleInternalSnapshotMarker is the text (lower case) of libvirt's refusal
// to delete an internal snapshot of a running domain whose disk is no longer
// the file the snapshot was taken on (qemuSnapshotDeleteValidate, libvirt
// >= 9.0). The validation runs before anything is changed.
const staleInternalSnapshotMarker = "is not the same as disk image currently used by vm"

// snapshotDeleteMetadataOnly is virsh snapshot-delete's flag that removes a
// snapshot's metadata and leaves every disk file alone.
const snapshotDeleteMetadataOnly = "--metadata"

// snapshotKeepsOthersCheck is what snapshotDeleteCheckFailed says could not be
// verified (it follows "could not verify").
const snapshotKeepsOthersCheck = "that deleting the snapshot keeps the VM's other snapshots"

// maxNamedSnapshots is how many blocking snapshots a refusal names; the rest
// are counted.
const maxNamedSnapshots = 3

// maxSnapshotNameBytes bounds a snapshot name in a refusal (VirtRigaud names
// are at most 64 bytes; a name read from an image header may be longer).
const maxSnapshotNameBytes = 64

// snapshotTreeDoc is the part of `virsh snapshot-dumpxml` the delete guard
// reads: the snapshot's name and parent, its memory and disk modes, and the
// domain definition it recorded.
type snapshotTreeDoc struct {
	XMLName xml.Name `xml:"domainsnapshot"`
	Name    string   `xml:"name"`
	Parent  *struct {
		Name string `xml:"name"`
	} `xml:"parent"`
	Memory *struct {
		Snapshot string `xml:"snapshot,attr"`
	} `xml:"memory"`
	Disks       []snapshotTreeDisk `xml:"disks>disk"`
	RevertDisks []snapshotTreeDisk `xml:"revertDisks>disk"`
	Domain      *domainDisksDoc    `xml:"domain"`
}

// snapshotTreeDisk is one <disk> of a snapshot's disk lists.
type snapshotTreeDisk struct {
	Name     string `xml:"name,attr"`
	Snapshot string `xml:"snapshot,attr"`
	Driver   *struct {
		Type string `xml:"type,attr"`
	} `xml:"driver"`
	Source *struct {
		File     string `xml:"file,attr"`
		Dev      string `xml:"dev,attr"`
		Protocol string `xml:"protocol,attr"`
		Name     string `xml:"name,attr"`
	} `xml:"source"`
}

// file returns the disk's source file, or "".
func (d snapshotTreeDisk) file() string {
	if d.Source == nil {
		return ""
	}
	return strings.TrimSpace(d.Source.File)
}

// overlayFormat returns the format of an external disk's overlay: the one its
// <driver> names, else qcow2 (libvirt's default for an external snapshot).
func (d snapshotTreeDisk) overlayFormat() string {
	if d.Driver != nil && strings.TrimSpace(d.Driver.Type) != "" {
		return strings.TrimSpace(d.Driver.Type)
	}
	return qcow2Format
}

// parent returns the name of the snapshot's parent, or "".
func (s *snapshotTreeDoc) parent() string {
	if s.Parent == nil {
		return ""
	}
	return strings.TrimSpace(s.Parent.Name)
}

// externalDisks returns the snapshot's external disks, from its disk list and
// from the disks a revert recorded (<revertDisks>, libvirt >= 9.9).
func (s *snapshotTreeDoc) externalDisks() []snapshotTreeDisk {
	var out []snapshotTreeDisk
	for _, d := range append(append([]snapshotTreeDisk(nil), s.Disks...), s.RevertDisks...) {
		if d.Snapshot == snapshotExternalOverlay {
			out = append(out, d)
		}
	}
	return out
}

// internalDisks returns the names of the snapshot's internal disks.
func (s *snapshotTreeDoc) internalDisks() []string {
	var out []string
	for _, d := range s.Disks {
		if d.Snapshot == snapshotInternalMode {
			out = append(out, strings.TrimSpace(d.Name))
		}
	}
	return out
}

// isExternal reports whether libvirt deletes the snapshot as an external one
// (virDomainSnapshotIsExternal): its memory or any disk is external.
func (s *snapshotTreeDoc) isExternal() bool {
	if s.Memory != nil && s.Memory.Snapshot == snapshotExternalOverlay {
		return true
	}
	return len(s.externalDisks()) > 0
}

// storedDisk returns the disk of the snapshot's recorded domain definition a
// snapshot disk name refers to: by its target, or by its source file (libvirt
// also accepts a disk's path as its name).
func (s *snapshotTreeDoc) storedDisk(name string) (domainDiskElem, bool) {
	if s.Domain == nil || name == "" {
		return domainDiskElem{}, false
	}
	for _, e := range s.Domain.Devices.Disks {
		if e.target() == name || (e.file() != "" && e.file() == name) {
			return e, true
		}
	}
	return domainDiskElem{}, false
}

// readSnapshotTree reads and parses snapshot snapshotID of domain (a name or
// UUID) on h's host. It also returns the raw document.
func readSnapshotTree(ctx context.Context, h hostCommandRunner, domain, snapshotID string) (*snapshotTreeDoc, string, error) {
	res, err := h.runVirshCommand(ctx, "snapshot-dumpxml", domain, snapshotID)
	if err != nil {
		return nil, "", fmt.Errorf("read snapshot %s: %w", snapshotID, err)
	}
	var doc snapshotTreeDoc
	if err := xml.Unmarshal([]byte(res.Stdout), &doc); err != nil {
		return nil, "", fmt.Errorf("parse snapshot %s: %w", snapshotID, err)
	}
	doc.Name = strings.TrimSpace(doc.Name)
	if doc.Name == "" {
		doc.Name = snapshotID
	}
	return &doc, res.Stdout, nil
}

// otherSnapshotTrees reads every snapshot of domain except except. A list or
// snapshot that cannot be read is an error.
func otherSnapshotTrees(ctx context.Context, h hostCommandRunner, domain, except string) ([]*snapshotTreeDoc, error) {
	res, err := h.runVirshCommand(ctx, "snapshot-list", domain, "--name")
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	var out []*snapshotTreeDoc
	for _, name := range strings.Split(res.Stdout, "\n") {
		name = strings.TrimSpace(name)
		if name == "" || name == except {
			continue
		}
		doc, _, err := readSnapshotTree(ctx, h, domain, name)
		if err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, nil
}

// imageInternalSnapshots reads the internal snapshots listed by the header of
// the image at path, opened in format, on h's host: `qemu-img info -U -f
// <format> --output=json -- <path>` through passwordless sudo when the host
// allows it (qemuImgInfoOnHost, the documented rule), after the file-kind
// check (a device or FIFO is never opened). exists is false only when
// qemu-img reported that the file does not exist. A raw image stores no
// snapshot and is not opened (exists is then reported true: unknown). Any
// other format, or an image that cannot be read, is an error.
func imageInternalSnapshots(ctx context.Context, h hostCommandRunner, path, format string) (names []string, exists bool, err error) {
	if format == rawDiskFormat {
		return nil, true, nil
	}
	if format != qcow2Format {
		return nil, true, fmt.Errorf("%s: image format %q is not read for internal snapshots", path, format)
	}
	if err := checkChainFileKind(ctx, h, path); err != nil {
		return nil, true, err
	}
	res, err := qemuImgInfoOnHost(ctx, h, "-U", "-f", format, "--output=json", "--", path)
	if err != nil {
		if res != nil && res.ExitCode == qemuImgFailureExitCode && strings.Contains(res.Stderr, qemuImgMissingFile) {
			return nil, false, nil
		}
		if res != nil && res.ExitCode == qemuImgFailureExitCode && strings.Contains(res.Stderr, qemuImgPermissionDenied) {
			return nil, true, &unreadableImageError{path: path, cause: err}
		}
		return nil, true, fmt.Errorf("read image %s: %w", path, err)
	}
	var info qemuImgInfo
	if err := json.Unmarshal([]byte(res.Stdout), &info); err != nil {
		return nil, true, fmt.Errorf("parse qemu-img info of %s: %w", path, err)
	}
	return info.snapshotNames(), true, nil
}

// snapshotDeleteCheckFailed is guardCheckFailed for a delete check that could
// not be completed: nothing was done; retryable (VM_DISK_CHECK_FAILED, or the
// host failure when the host could not be reached). err stays in the provider
// log.
func snapshotDeleteCheckFailed(domain string, err error) error {
	log.Printf("ERROR libvirt snapshot delete check of domain %s failed; nothing was deleted: %v", domain, err)
	if isHostTransportFailure(err) {
		return &guardHostUnreachableError{op: guardOpSnapshotDelete, domain: domain, cause: err}
	}
	return &diskCheckFailedError{op: guardOpSnapshotDelete, domain: domain, unreadable: isUnreadableImage(err),
		check: snapshotKeepsOthersCheck}
}

// atRiskFile is one file a snapshot delete may merge away or remove, and the
// format it is read in.
type atRiskFile struct {
	path   string
	format string
}

// snapshotDeleteFiles returns the files deleting external snapshot snap may
// merge away or remove (see the file comment), keyed by cleaned path; others
// are the domain's other snapshots. An external disk whose overlay is not a
// file is an error: what it holds cannot be read.
func snapshotDeleteFiles(snap *snapshotTreeDoc, others []*snapshotTreeDoc) (map[string]atRiskFile, error) {
	files := map[string]atRiskFile{}
	add := func(path, format string) {
		if path = strings.TrimSpace(path); path == "" {
			return
		}
		key := filepath.Clean(path)
		if _, seen := files[key]; !seen {
			files[key] = atRiskFile{path: path, format: format}
		}
	}
	addStored := func(doc *snapshotTreeDoc, name string) {
		if e, ok := doc.storedDisk(name); ok && e.file() != "" {
			add(e.file(), e.format())
		}
	}
	for _, d := range snap.externalDisks() {
		if d.file() == "" {
			return nil, fmt.Errorf("external disk %q of snapshot %s has no file source", d.Name, snap.Name)
		}
		add(d.file(), d.overlayFormat())
		name := strings.TrimSpace(d.Name)
		addStored(snap, name)
		for _, o := range others {
			if o.parent() == snap.Name {
				addStored(o, name)
			}
		}
	}
	return files, nil
}

// refuseIfDeleteLosesSnapshots refuses (snapshotDeleteLosesSnapshotsError)
// to delete external snapshot snap (snapshotID) of domain (a name or UUID)
// while another snapshot is stored in a file the delete may merge away or
// remove — by libvirt's metadata or by the file's own header (see the file
// comment). A snapshot that is not external merges nothing and passes. A
// check that cannot be completed refuses retryably
// (snapshotDeleteCheckFailed).
func refuseIfDeleteLosesSnapshots(ctx context.Context, h hostCommandRunner, domain, snapshotID string, snap *snapshotTreeDoc) error {
	if !snap.isExternal() {
		return nil
	}
	others, err := otherSnapshotTrees(ctx, h, domain, snapshotID)
	if err != nil {
		return snapshotDeleteCheckFailed(domain, err)
	}
	files, err := snapshotDeleteFiles(snap, others)
	if err != nil {
		return snapshotDeleteCheckFailed(domain, err)
	}
	blocking := map[string]bool{}
	// libvirt's metadata: another snapshot whose internal disk it recorded
	// on one of those files.
	for _, o := range others {
		for _, n := range o.internalDisks() {
			if e, ok := o.storedDisk(n); ok && e.file() != "" {
				if _, atRisk := files[filepath.Clean(e.file())]; atRisk {
					blocking[o.Name] = true
				}
			}
		}
	}
	// The files' own headers.
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f := files[k]
		names, _, err := imageInternalSnapshots(ctx, h, f.path, f.format)
		if err != nil {
			return snapshotDeleteCheckFailed(domain, err)
		}
		for _, n := range names {
			blocking[n] = true
		}
	}
	if len(blocking) == 0 {
		return nil
	}
	names := make([]string, 0, len(blocking))
	for n := range blocking {
		names = append(names, n)
	}
	sort.Strings(names)
	log.Printf("WARN Refusing to delete snapshot %s of domain %s: snapshot(s) %v are stored in a file the delete would merge away or remove (files %v)",
		snapshotID, domain, names, keys)
	return &snapshotDeleteLosesSnapshotsError{snapshot: snapshotID, blocking: names}
}

// snapshotDeleteLosesSnapshotsError refuses to delete an external snapshot
// while other snapshots of the same VM are stored in a file the delete would
// merge away or remove (refuseIfDeleteLosesSnapshots). Nothing was changed.
// Its message names only snapshot names, never a host path.
type snapshotDeleteLosesSnapshotsError struct {
	// snapshot is the refused snapshot's name.
	snapshot string
	// blocking are the names of the snapshots that would be lost, sorted.
	blocking []string
}

// Error is the refusal, safe for the requesting VM's status. The manager
// keeps 256 bytes of a provider's answer, so the guidance — which snapshots
// to delete first — comes first.
func (e *snapshotDeleteLosesSnapshotsError) Error() string {
	subject, verb := "snapshot", "it is"
	if len(e.blocking) > 1 {
		subject, verb = "snapshots", "they are"
	}
	return fmt.Sprintf("delete refused, nothing changed: delete %s %s first; %s stored in a disk file that deleting "+
		"snapshot %s would merge away, and would be lost", subject, quotedSnapshotNames(e.blocking), verb,
		quoteSnapshotName(e.snapshot))
}

// GRPCStatus renders the refusal as codes.FailedPrecondition (no ErrorInfo):
// a definite answer, kept out of the circuit breaker; the manager keeps the
// VMSnapshot and re-checks with a backoff. status.FromError finds it through
// any fmt.Errorf wrapping.
func (e *snapshotDeleteLosesSnapshotsError) GRPCStatus() *status.Status {
	return status.New(codes.FailedPrecondition, e.Error())
}

// snapshotDeleteStaleInternalError refuses to delete an internal (memory)
// snapshot that libvirt cannot delete while the VM runs, because the VM no
// longer runs on the disk file the snapshot was taken on, and whose data the
// provider cannot prove gone (deleteStaleInternalSnapshot). Nothing was
// changed. Its message names only the snapshot.
type snapshotDeleteStaleInternalError struct {
	// snapshot is the refused snapshot's name.
	snapshot string
}

// Error is the refusal, safe for the requesting VM's status (guidance first).
func (e *snapshotDeleteStaleInternalError) Error() string {
	return fmt.Sprintf("delete refused, nothing changed: power the VM off and delete it again; "+
		"libvirt cannot delete snapshot %s while the VM runs on a disk file other than the one it is stored in",
		quoteSnapshotName(e.snapshot))
}

// GRPCStatus renders the refusal as codes.FailedPrecondition (no ErrorInfo),
// as snapshotDeleteLosesSnapshotsError's.
func (e *snapshotDeleteStaleInternalError) GRPCStatus() *status.Status {
	return status.New(codes.FailedPrecondition, e.Error())
}

// quoteSnapshotName quotes a snapshot name for a refusal, cut to
// maxSnapshotNameBytes (at a rune boundary).
func quoteSnapshotName(name string) string {
	name = strings.ToValidUTF8(name, "?")
	if len(name) > maxSnapshotNameBytes {
		cut := maxSnapshotNameBytes
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = name[:cut] + "…"
	}
	return fmt.Sprintf("%q", name)
}

// quotedSnapshotNames renders up to maxNamedSnapshots names, quoted and
// comma-separated, and counts the rest.
func quotedSnapshotNames(names []string) string {
	shown := names
	if len(shown) > maxNamedSnapshots {
		shown = shown[:maxNamedSnapshots]
	}
	parts := make([]string, 0, len(shown))
	for _, n := range shown {
		parts = append(parts, quoteSnapshotName(n))
	}
	out := strings.Join(parts, ", ")
	if rest := len(names) - len(shown); rest > 0 {
		out += fmt.Sprintf(" and %d more", rest)
	}
	return out
}

// isStaleInternalSnapshotRefusal reports whether err is a virsh snapshot-delete
// that ended with an exit status of its own and one of whose "error:" lines
// is libvirt's refusal to delete an internal snapshot of a running domain
// whose disk is no longer the file the snapshot was taken on
// (staleInternalSnapshotMarker). virsh prints its own "error: Failed to delete
// snapshot <name>" first and libvirt's error after it.
func isStaleInternalSnapshotRefusal(err error) bool {
	var ve *VirshError
	if !errors.As(err, &ve) || ve.ExitCode <= 0 || ve.ExitCode >= 128 {
		return false
	}
	for _, l := range strings.Split(ve.Stderr, "\n") {
		l = strings.ToLower(strings.TrimSpace(l))
		if strings.HasPrefix(l, "error:") && strings.Contains(l, staleInternalSnapshotMarker) {
			return true
		}
	}
	return false
}

// staleInternalSnapshotGone reports whether the provider can prove that
// internal snapshot snap (snapshotID) of domain (a name or UUID) holds no
// data any more: it is internal, every file it recorded for its internal
// disks is a qcow2 file that no longer exists (qemu-img: "No such file or
// directory"), and no image of the domain's current disk chains lists a
// snapshot of that name. Anything it cannot establish is false (not proven);
// a read that fails is the error.
func staleInternalSnapshotGone(ctx context.Context, h hostCommandRunner, domain, snapshotID string, snap *snapshotTreeDoc) (bool, error) {
	if snap == nil || snap.isExternal() || snap.Domain == nil {
		return false, nil
	}
	disks := snap.internalDisks()
	if len(disks) == 0 {
		return false, nil
	}
	for _, n := range disks {
		e, ok := snap.storedDisk(n)
		if !ok || e.file() == "" || e.format() != qcow2Format {
			return false, nil
		}
		_, exists, err := imageInternalSnapshots(ctx, h, e.file(), qcow2Format)
		if err != nil {
			return false, err
		}
		if exists {
			return false, nil
		}
	}
	cur, err := readDomainDisks(ctx, h, domain)
	if err != nil {
		return false, err
	}
	for _, e := range cur.Devices.Disks {
		if e.sourceRef() == "" || e.format() == rawDiskFormat {
			continue
		}
		if e.file() == "" {
			return false, nil // a block or network qcow2 disk: not read
		}
		levels, err := walkBackingChainFrom(ctx, h, e.file(), e.format())
		if err != nil {
			return false, err
		}
		for _, l := range levels {
			for _, s := range l.snapshots {
				if s == snapshotID {
					return false, nil
				}
			}
		}
	}
	return true, nil
}

// deleteStaleInternalSnapshot answers a snapshot-delete libvirt refused
// because internal snapshot snapshotID is stored in a disk file the running
// domain no longer uses (isStaleInternalSnapshotRefusal; nothing was
// changed). When its data is provably gone (staleInternalSnapshotGone) only
// its metadata is deleted, which touches no disk file; otherwise the answer is
// the definite snapshotDeleteStaleInternalError, or a retryable check failure
// when the proof could not be completed.
func deleteStaleInternalSnapshot(ctx context.Context, conn libvirtConn, domain, snapshotID string, snap *snapshotTreeDoc, refusal error) (*providerv1.TaskResponse, error) {
	log.Printf("WARN libvirt refused to delete internal snapshot %s of domain %s: %v", snapshotID, domain, refusal)
	gone, err := staleInternalSnapshotGone(ctx, hostConnRunner{conn: conn}, domain, snapshotID, snap)
	if err != nil {
		return nil, fmt.Errorf("failed to delete snapshot: %w", snapshotDeleteCheckFailed(domain, err))
	}
	if !gone {
		return nil, fmt.Errorf("failed to delete snapshot: %w", &snapshotDeleteStaleInternalError{snapshot: snapshotID})
	}
	if _, err := conn.Virsh(ctx, "snapshot-delete", domain, snapshotID, snapshotDeleteMetadataOnly); err != nil {
		return nil, fmt.Errorf("failed to delete snapshot metadata: %w", err)
	}
	log.Printf("WARN Deleted the metadata of snapshot %s of domain %s: its data was already gone (the file it was stored in "+
		"no longer exists and no current disk image lists it)", snapshotID, domain)
	return &providerv1.TaskResponse{}, nil
}
