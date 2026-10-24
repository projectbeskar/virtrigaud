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
	stderrors "errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
)

// Removable media and libvirt's external snapshot revert (V6).
//
// libvirt (>= 9.9) reverts an external snapshot by rebuilding the domain's
// disks from the domain definition the snapshot STORED and creating a new
// overlay, <source file without its extension>.<epoch>, for every disk of that
// definition that has media — whatever the disk's snapshot mode in the
// snapshot's disk list. A VM's cloud-init seed CD-ROM (a raw ISO in its private
// seed directory) is rewritten that way to a qcow2 overlay the host's AppArmor
// profile does not let QEMU open, and the VM no longer starts. Excluding the
// CD-ROM with --diskspec does not help (libvirt already records it 'no'); only
// a stored definition whose CD-ROM is EMPTY reverts cleanly.
//
// So a disk-only snapshot of a SHUT-OFF domain is taken with its removable
// media ejected, persistently (--config), in this order:
//
//  1. record the media to eject in the domain's metadata (setMediaMarker,
//     namespace mediaMarkerURI) — the record a later call repairs from;
//  2. eject each one (`change-media <domain> <target> --eject --config`);
//  3. snapshot-create-as;
//  4. re-insert each one with its source (`change-media <domain> <target>
//     <source> --insert --config`) — also when the snapshot failed — check
//     the domain shows each source again with its driver type, and remove the
//     record.
//
// A failure (or a host lost) between 2 and 4 leaves the domain without its
// media and with the record. Every later SnapshotCreate, SnapshotRevert and
// power-on (start or reboot) of the domain first repairs it
// (restoreEjectedMedia): for each recorded target that is still empty, the
// recorded source is inserted again — only when it is provably this domain's
// own cloud-init seed (isOwnSeedISO: <seed root>/virtrigaud-cloudinit-<domain
// name>.<random>/cloud-init.iso, or an earlier release's per-name seed
// directory). Any other recorded medium (an installer ISO) is never inserted
// from the record; it is logged and left for the operator.
//
// A running domain is never ejected (it would be guest-visible); its
// snapshots store the media and cannot be reverted (snapshotRevertGate).

// Media marker metadata.
const (
	// mediaMarkerURI is the metadata namespace of the record of ejected media.
	mediaMarkerURI = "https://virtrigaud.io/xmlns/libvirt/media/v1"
	// mediaMarkerKey is the XML prefix the record is written with.
	mediaMarkerKey = "virtrigaud"
)

// mediaMarkerElem is the record of ejected media in /domain/metadata:
// <virtrigaud:ejected xmlns:virtrigaud="mediaMarkerURI"><media target=...
// source=... driver=.../></virtrigaud:ejected>. The children are matched by
// local name (libvirt may or may not carry the namespace onto them).
type mediaMarkerElem struct {
	Media []removableMedium `xml:"media"`
}

// removableMedium is one CD-ROM or floppy and the file in it.
type removableMedium struct {
	// Target is the drive's target name (hda).
	Target string `xml:"target,attr"`
	// Source is the absolute path of the file in it.
	Source string `xml:"source,attr"`
	// Driver is its driver type (raw), or "" when the definition names none.
	Driver string `xml:"driver,attr"`
}

// errMediaNotRestored marks a failure that left removable media of the domain
// ejected: the domain's definition was changed, and the record of the media
// stays for the repair (restoreEjectedMedia).
var errMediaNotRestored = stderrors.New("the domain's removable media could not be re-inserted")

// mediaMarker returns the media the domain's metadata records as ejected.
func (d *domainDisksDoc) mediaMarker() []removableMedium {
	var out []removableMedium
	for _, e := range d.Metadata.Ejected {
		out = append(out, e.Media...)
	}
	return out
}

// hasMediaMarker reports whether the domain carries a record of ejected media.
func (d *domainDisksDoc) hasMediaMarker() bool { return len(d.Metadata.Ejected) > 0 }

// diskByTarget returns the top-level <disk> element with target t.
func (d *domainDisksDoc) diskByTarget(t string) (domainDiskElem, bool) {
	for _, e := range d.Devices.Disks {
		if e.target() == t {
			return e, true
		}
	}
	return domainDiskElem{}, false
}

// driverType returns the element's own <driver type=...>, or "".
func (e domainDiskElem) driverType() string {
	if e.Driver == nil {
		return ""
	}
	return strings.TrimSpace(e.Driver.Type)
}

// ejectableMedia returns, in document order, the domain's CD-ROMs and floppies
// that hold a file this provider can eject and insert again: a file-backed
// drive with a plain target name whose source is a clean absolute path.
// Media of any other kind (a block device, a network source) are not
// returned; a snapshot keeps them (and cannot be reverted).
func (d *domainDisksDoc) ejectableMedia() []removableMedium {
	var out []removableMedium
	for _, e := range d.Devices.Disks {
		if !e.isMedia() || e.Type != diskTypeFile {
			continue
		}
		f, t := e.file(), e.target()
		if f == "" || !diskTargetRE.MatchString(t) || !isCleanAbsPath(f) {
			continue
		}
		out = append(out, removableMedium{Target: t, Source: f, Driver: e.driverType()})
	}
	return out
}

// isCleanAbsPath reports whether p is an absolute, lexically clean path with
// no control character: safe as a virsh argument (it cannot start with '-').
func isCleanAbsPath(p string) bool {
	return strings.HasPrefix(p, "/") && filepath.Clean(p) == p && !strings.ContainsFunc(p, isControlRune)
}

// isOwnSeedISO reports whether path is provably the cloud-init seed ISO of the
// domain named name: cloud-init.iso directly in a per-create seed directory
// of this domain (<root>/virtrigaud-cloudinit-<name>.<random>, isOwnSeedDirName)
// in one of roots, or in an earlier release's per-name seed directory
// (<root>/virtrigaud-cloudinit/<name>).
func isOwnSeedISO(path, name string, roots []string) bool {
	if name == "" || !isCleanAbsPath(path) || filepath.Base(path) != cloudInitISOName {
		return false
	}
	dir := filepath.Dir(path)
	if isSeedDirIn(dir, roots...) && isOwnSeedDirName(filepath.Base(dir), name) {
		return true
	}
	for _, root := range roots {
		if dir == filepath.Join(filepath.Clean(root), legacyCloudInitSeedRoot, name) {
			return true
		}
	}
	return false
}

// renderMediaMarker renders the record of media for `virsh metadata --set`.
// Every value is passed through xmlEscape.
func renderMediaMarker(media []removableMedium) string {
	var b strings.Builder
	b.WriteString("<ejected>")
	for _, m := range media {
		fmt.Fprintf(&b, "<media target='%s' source='%s' driver='%s'/>", xmlEscape(m.Target), xmlEscape(m.Source), xmlEscape(m.Driver))
	}
	b.WriteString("</ejected>")
	return b.String()
}

// setMediaMarker records media as ejected in the persistent definition of
// domain (a name or UUID) on h's host.
func setMediaMarker(ctx context.Context, h hostCommandRunner, domain string, media []removableMedium) error {
	if _, err := h.runVirshCommand(ctx, "metadata", "--domain", domain, "--uri", mediaMarkerURI,
		"--key", mediaMarkerKey, "--set", renderMediaMarker(media), "--config"); err != nil {
		return fmt.Errorf("record the removable media to eject: %w", err)
	}
	return nil
}

// clearMediaMarker removes the record of ejected media from the persistent
// definition of domain.
func clearMediaMarker(ctx context.Context, h hostCommandRunner, domain string) error {
	if _, err := h.runVirshCommand(ctx, "metadata", "--domain", domain, "--uri", mediaMarkerURI, "--remove", "--config"); err != nil {
		return fmt.Errorf("remove the record of ejected removable media: %w", err)
	}
	return nil
}

// ejectMedium ejects target's medium from the persistent definition.
func ejectMedium(ctx context.Context, h hostCommandRunner, domain, target string) error {
	if _, err := h.runVirshCommand(ctx, "change-media", domain, target, "--eject", "--config"); err != nil {
		return fmt.Errorf("eject the medium of drive %s: %w", target, err)
	}
	return nil
}

// insertMedium inserts m into its drive in the persistent definition, and in
// the running domain too when live.
func insertMedium(ctx context.Context, h hostCommandRunner, domain string, m removableMedium, live bool) error {
	args := []string{"change-media", domain, m.Target, m.Source, "--insert", "--config"}
	if live {
		args = append(args, "--live")
	}
	if _, err := h.runVirshCommand(ctx, args...); err != nil {
		return fmt.Errorf("insert the medium of drive %s: %w", m.Target, err)
	}
	return nil
}

// mediaInPlace reports which of media the domain (doc) does not show in its
// drive with its source and (when recorded) its driver type.
func mediaInPlace(doc *domainDisksDoc, media []removableMedium) (missing []string) {
	for _, m := range media {
		e, ok := doc.diskByTarget(m.Target)
		if !ok || e.file() != m.Source || (m.Driver != "" && e.driverType() != m.Driver) {
			missing = append(missing, m.Target)
		}
	}
	return missing
}

// ejectMediaForSnapshot ejects media from the persistent definition of the
// shut-off domain, after recording them (setMediaMarker), and returns them.
// When an eject fails it inserts again those already ejected and removes the
// record; the error is then the eject's (the domain is as before), or carries
// errMediaNotRestored when that failed too.
func ejectMediaForSnapshot(ctx context.Context, h hostCommandRunner, domain string, media []removableMedium) ([]removableMedium, error) {
	if len(media) == 0 {
		return nil, nil
	}
	if err := setMediaMarker(ctx, h, domain, media); err != nil {
		return nil, err
	}
	var ejected []removableMedium
	for _, m := range media {
		if err := ejectMedium(ctx, h, domain, m.Target); err != nil {
			if !hostCommandEnded(ctx, err) {
				// The eject may have taken effect (the session dropped, the
				// request ended): those surely ejected are inserted again,
				// and the record stays (whole) for the repair.
				for _, done := range ejected {
					if ierr := insertMedium(ctx, h, domain, done, false); ierr != nil {
						log.Printf("WARN %v", ierr)
					}
				}
				return nil, fmt.Errorf("%w: the eject of drive %s may have taken effect: %w", errMediaNotRestored, m.Target, err)
			}
			if rerr := reinsertMedia(ctx, h, domain, ejected, false); rerr != nil {
				return nil, fmt.Errorf("%w; %w", err, rerr)
			}
			if len(ejected) == 0 {
				// Nothing was ejected: the record is stale, and harmless (the
				// repair skips a drive that holds media), but removed.
				if cerr := clearMediaMarker(ctx, h, domain); cerr != nil {
					log.Printf("WARN %v", cerr)
				}
			}
			return nil, err
		}
		ejected = append(ejected, m)
	}
	log.Printf("INFO Ejected removable media %v of domain %s for its snapshot", targetsOf(ejected), domain)
	return ejected, nil
}

// reinsertMedia inserts media (all of them, whatever fails) into the domain's
// drives, checks that the domain shows each again with its source and driver
// type, and removes the record of ejected media. A failure carries
// errMediaNotRestored, and the record stays for the repair.
func reinsertMedia(ctx context.Context, h hostCommandRunner, domain string, media []removableMedium, live bool) error {
	if len(media) == 0 {
		return nil
	}
	var errs []error
	for _, m := range media {
		if err := insertMedium(ctx, h, domain, m, live); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		doc, err := readDomainDisks(ctx, h, domain)
		switch {
		case err != nil:
			errs = append(errs, err)
		default:
			if missing := mediaInPlace(doc, media); len(missing) > 0 {
				errs = append(errs, fmt.Errorf("drive(s) %v do not show their medium again", missing))
			}
		}
	}
	if len(errs) > 0 {
		log.Printf("ERROR Removable media %v of domain %s could not be re-inserted; the record of them stays for the repair: %v",
			targetsOf(media), domain, errs)
		return fmt.Errorf("%w (%v): %w", errMediaNotRestored, targetsOf(media), stderrors.Join(errs...))
	}
	if err := clearMediaMarker(ctx, h, domain); err != nil {
		// The media are in place; a stale record is harmless (the repair
		// skips a drive that holds media) and is removed by the next repair.
		log.Printf("WARN %v", err)
	}
	log.Printf("INFO Re-inserted removable media %v of domain %s", targetsOf(media), domain)
	return nil
}

// restoreEjectedMedia is the repair of a snapshot's interrupted ejection: for
// each medium the domain's metadata records as ejected (doc, its current
// definition) whose drive is still empty and whose recorded source is provably
// the domain's own cloud-init seed (isOwnSeedISO), it inserts the source again
// (persistently). A recorded medium that is not provably the domain's seed is
// never inserted: it is logged, and its record kept. A drive that holds a
// medium again needs nothing. The record is removed once nothing is left to
// restore. It reports whether it inserted anything; an error carries
// errMediaNotRestored.
func restoreEjectedMedia(ctx context.Context, h hostCommandRunner, domain string, doc *domainDisksDoc, roots []string) (bool, error) {
	if !doc.hasMediaMarker() {
		return false, nil
	}
	var restore []removableMedium
	unresolved := 0
	for _, m := range doc.mediaMarker() {
		e, ok := doc.diskByTarget(m.Target)
		if !ok || !e.isMedia() || e.sourceRef() != "" {
			continue // gone, not a drive, or it holds a medium again
		}
		if !diskTargetRE.MatchString(m.Target) || !isOwnSeedISO(m.Source, doc.Name, roots) {
			log.Printf("WARN Domain %s records that drive %s was ejected for a snapshot and is still empty, but its recorded "+
				"medium %q is not provably the domain's own cloud-init seed: it is not re-inserted; insert it by hand "+
				"(virsh change-media ... --insert --config) and remove the record (virsh metadata --uri %s --remove --config)",
				domain, m.Target, m.Source, mediaMarkerURI)
			unresolved++
			continue
		}
		restore = append(restore, m)
	}
	if len(restore) > 0 {
		log.Printf("WARN Domain %s has cloud-init seed drive(s) %v left ejected by an interrupted snapshot; re-inserting them",
			domain, targetsOf(restore))
		var errs []error
		for _, m := range restore {
			if err := insertMedium(ctx, h, domain, m, false); err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) == 0 {
			after, err := readDomainDisks(ctx, h, domain)
			switch {
			case err != nil:
				errs = append(errs, err)
			default:
				if missing := mediaInPlace(after, restore); len(missing) > 0 {
					errs = append(errs, fmt.Errorf("drive(s) %v do not show their seed again", missing))
				}
			}
		}
		if len(errs) > 0 {
			return false, fmt.Errorf("%w (%v): %w", errMediaNotRestored, targetsOf(restore), stderrors.Join(errs...))
		}
	}
	if unresolved == 0 {
		if err := clearMediaMarker(ctx, h, domain); err != nil {
			log.Printf("WARN %v", err)
		}
	}
	return len(restore) > 0, nil
}

// repairEjectedMediaOf reads domain's definition on h's host and runs
// restoreEjectedMedia; it returns the definition as it is afterwards. A
// definition that cannot be read is an error.
func repairEjectedMediaOf(ctx context.Context, h hostCommandRunner, domain string, roots []string) (*domainDisksDoc, error) {
	doc, err := readDomainDisks(ctx, h, domain)
	if err != nil {
		return nil, err
	}
	changed, err := restoreEjectedMedia(ctx, h, domain, doc, roots)
	if err != nil {
		return nil, err
	}
	if changed {
		return readDomainDisks(ctx, h, domain)
	}
	return doc, nil
}

// restoreEjectedSeedBeforeStart runs, before domain (a name or UUID) is
// started on vp's host, the repair of a snapshot's interrupted ejection
// (restoreEjectedMedia), so the guest never boots without its cloud-init seed
// (cloud-init without its datasource may re-run its per-instance steps). A
// definition that cannot be read is logged and the start proceeds as before
// (it fails on its own for a domain that is not there); a seed that cannot be
// re-inserted fails the start, which is not attempted.
func (p *Provider) restoreEjectedSeedBeforeStart(ctx context.Context, vp *VirshProvider, domain string) error {
	doc, err := readDomainDisks(ctx, vp, domain)
	if err != nil {
		log.Printf("WARN Could not read domain %s before starting it; an ejected cloud-init seed is not checked for: %v", domain, err)
		return nil
	}
	if _, err := restoreEjectedMedia(ctx, vp, domain, doc, p.seedRoots()); err != nil {
		return fmt.Errorf("start of domain %s not performed: the cloud-init seed an interrupted snapshot ejected could not be re-inserted: %w",
			domain, err)
	}
	return nil
}

// targetsOf returns the targets of media.
func targetsOf(media []removableMedium) []string {
	out := make([]string, 0, len(media))
	for _, m := range media {
		out = append(out, m.Target)
	}
	return out
}

// seedRootsOf returns backend's seed roots (Provider.seedRoots), or nil for a
// backend that has none (a test fake): with no roots no seed is provable, so
// the repair inserts nothing.
func seedRootsOf(backend providerBackend) []string {
	if b, ok := backend.(interface{ seedRoots() []string }); ok {
		return b.seedRoots()
	}
	return nil
}
