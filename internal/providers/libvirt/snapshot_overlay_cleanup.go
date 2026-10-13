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
)

// Snapshot overlay cleanup (v0.4.0-rc1 F2). VirtRigaud's snapshots are
// external and disk-only: snapshot-create-as --disk-only adds an overlay file
// next to the disk it covers, named after that disk with its suffix replaced
// by the snapshot's name (<domain>-disk.<snapshot> for the primary disk). On
// `virsh snapshot-delete`, libvirt 10 commits the overlay back into its base
// and pivots the domain onto the base, but LEAVES the overlay file in the
// pool — and a later Delete of the VM removes only overlays still in the
// domain's chain, so it stayed forever.
//
// SnapshotDelete therefore records the snapshot's overlay files before the
// delete and, once the delete succeeded, removes each one only if ALL hold:
//
//   - the snapshot names it as an external overlay, and its name is the
//     domain's own primary-disk overlay name, <domain>-disk.<snapshot>;
//   - it is no longer in any backing chain of the domain's current disks;
//   - no other snapshot of the domain references it (in its disk list or the
//     domain definition it recorded);
//   - it is a regular file, not a symbolic link, directly inside the storage
//     pool or an allowed image directory (deletableDiskFiles, as Delete);
//   - no other domain on the host uses it as a disk or backing file, and — on
//     a clustered provider — no domain on another host of the Provider does
//     (checkDeletionAcrossHosts, as Delete).
//
// Anything that cannot be established keeps the file. A failure to remove it
// is logged provider-side only (no path reaches any status) and never fails
// the SnapshotDelete RPC: the snapshot itself is gone, and a retry would find
// no snapshot left to clean up after.

// snapshotDisksDoc is the part of `virsh snapshot-dumpxml` the cleanup reads:
// the snapshot's own disk list (not the domain definition it embeds).
type snapshotDisksDoc struct {
	XMLName xml.Name `xml:"domainsnapshot"`
	Disks   []struct {
		Snapshot string `xml:"snapshot,attr"`
		Source   struct {
			File string `xml:"file,attr"`
		} `xml:"source"`
	} `xml:"disks>disk"`
}

// snapshotExternalOverlay is the snapshot-disk mode of an external snapshot.
const snapshotExternalOverlay = "external"

// parseSnapshotOverlays returns the overlay files an external snapshot's XML
// (`virsh snapshot-dumpxml`) names: the source files of its disks whose
// snapshot mode is external.
func parseSnapshotOverlays(snapshotXML string) ([]string, error) {
	var doc snapshotDisksDoc
	if err := xml.Unmarshal([]byte(snapshotXML), &doc); err != nil {
		return nil, err
	}
	var out []string
	for _, d := range doc.Disks {
		if d.Snapshot == snapshotExternalOverlay && strings.TrimSpace(d.Source.File) != "" {
			out = append(out, strings.TrimSpace(d.Source.File))
		}
	}
	return out, nil
}

// sourceFilesIn returns every <source file='...'> anywhere in an XML
// document: what a snapshot's XML references, in its disk list and in the
// domain definition it recorded.
func sourceFilesIn(doc string) ([]string, error) {
	dec := xml.NewDecoder(strings.NewReader(doc))
	var out []string
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "source" {
			continue
		}
		for _, a := range se.Attr {
			if a.Name.Local == "file" && strings.TrimSpace(a.Value) != "" {
				out = append(out, strings.TrimSpace(a.Value))
			}
		}
	}
}

// snapshotOverlaysOf reads the overlay files snapshot snapshotID of domain
// (a name or UUID) names on conn's host, before it is deleted. A snapshot
// that cannot be read yields none: the delete proceeds, and nothing is
// cleaned up after it.
func snapshotOverlaysOf(ctx context.Context, conn libvirtConn, domain, snapshotID string) []string {
	res, err := conn.Virsh(ctx, "snapshot-dumpxml", domain, snapshotID)
	if err != nil {
		log.Printf("WARN Could not read snapshot %s of domain %s; its overlay files are left in place after the delete: %v",
			snapshotID, domain, err)
		return nil
	}
	overlays, err := parseSnapshotOverlays(res.Stdout)
	if err != nil {
		log.Printf("WARN Could not parse snapshot %s of domain %s; its overlay files are left in place after the delete: %v",
			snapshotID, domain, err)
		return nil
	}
	return overlays
}

// removeSnapshotOverlays removes, after snapshot snapshotID of domain (a name
// or UUID) was deleted on c's host, those of its recorded overlay files that
// are safe to remove (see the file comment). It never fails: whatever cannot
// be established keeps the file, and is logged.
func (p *Provider) removeSnapshotOverlays(ctx context.Context, c libvirtConn, domain, snapshotID string, overlays []string) {
	if len(overlays) == 0 {
		return
	}
	keep := func(why string, err error) {
		if err != nil {
			log.Printf("WARN Keeping the overlay file(s) %v of deleted snapshot %s of domain %s: %s: %v", overlays, snapshotID, domain, why, err)
			return
		}
		log.Printf("WARN Keeping the overlay file(s) %v of deleted snapshot %s of domain %s: %s", overlays, snapshotID, domain, why)
	}
	vp, err := virshOf(c)
	if err != nil {
		keep("no host connection", err)
		return
	}
	files, err := p.removableSnapshotOverlays(ctx, vp, domain, snapshotID, overlays)
	if err != nil {
		keep("their use could not be established", err)
		return
	}
	if len(files) > 0 && p.clustered() {
		if _, err := p.checkDeletionAcrossHosts(ctx, c.HostID(), domainDeletionPlan{name: domain, disks: files}); err != nil {
			keep("they may be used on another host of the Provider", err)
			return
		}
	}
	for _, f := range files {
		if err := deleteDiskFile(ctx, vp, f); err != nil {
			log.Printf("WARN Failed to remove overlay file %s of deleted snapshot %s of domain %s: %v", f, snapshotID, domain, err)
			continue
		}
		log.Printf("INFO Removed overlay file %s of deleted snapshot %s of domain %s", f, snapshotID, domain)
	}
}

// removableSnapshotOverlays returns the canonical host paths among overlays
// that removeSnapshotOverlays may remove on vp's host: named
// <domain>-disk.<snapshotID>, out of the domain's current chains, referenced
// by no other snapshot of the domain, regular files inside the storage
// directories, and used by no other domain on the host. An error means the
// answer could not be established; nothing may be removed then.
func (p *Provider) removableSnapshotOverlays(ctx context.Context, vp *VirshProvider, domain, snapshotID string, overlays []string) ([]string, error) {
	doc, err := domainDisksOf(ctx, vp, domain)
	if err != nil {
		return nil, err
	}
	name := doc.Name
	if name == "" {
		name = domain
	}
	want := vmDiskVolumeName(name) + "." + snapshotID
	var candidates []string
	for _, o := range overlays {
		if filepath.Base(o) != want {
			log.Printf("WARN Keeping overlay file %s of deleted snapshot %s of domain %s: it is not the domain's own overlay %s",
				o, snapshotID, name, want)
			continue
		}
		candidates = append(candidates, o)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Every file the domain still references — its disks' current chains —
	// and every file its other snapshots reference.
	inUse, err := domainChainFiles(ctx, vp, doc)
	if err != nil {
		return nil, err
	}
	snapRefs, err := otherSnapshotFiles(ctx, vp, domain, snapshotID)
	if err != nil {
		return nil, err
	}
	inUse = append(inUse, snapRefs...)
	canon, err := canonicalizeOnHost(ctx, vp, append(append([]string(nil), candidates...), inUse...))
	if err != nil {
		return nil, err
	}
	referenced := map[string]bool{}
	for i, f := range inUse {
		referenced[f] = true
		referenced[canon[len(candidates)+i]] = true
	}
	var free []string
	for i, o := range candidates {
		if referenced[o] || referenced[canon[i]] {
			log.Printf("INFO Keeping overlay file %s of deleted snapshot %s of domain %s: the domain or another of its snapshots still references it",
				o, snapshotID, name)
			continue
		}
		free = append(free, o)
	}
	if len(free) == 0 {
		return nil, nil
	}

	cleared, err := p.deletableDiskFiles(ctx, vp, name, free)
	if err != nil {
		return nil, err
	}
	if len(cleared) == 0 {
		return nil, nil
	}
	others, err := otherDomainsOnHost(ctx, vp, doc.UUID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range cleared {
		if others.using(name, []string{f}, []string{f}) > 0 {
			log.Printf("WARN Keeping overlay file %s of deleted snapshot %s of domain %s: another domain uses it", f, snapshotID, name)
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// domainChainFiles returns every file in the backing chains of the domain's
// current disks (doc), the disks included. A chain that cannot be read is an
// error: what it holds cannot be known.
func domainChainFiles(ctx context.Context, vp *VirshProvider, doc *domainDisksDoc) ([]string, error) {
	var out []string
	for _, disk := range doc.diskFiles() {
		out = append(out, disk)
		format := doc.diskFormat(disk)
		if format == rawDiskFormat {
			continue
		}
		if format == "" {
			return nil, fmt.Errorf("the format of disk %s is not known from the definition", disk)
		}
		levels, err := walkBackingChainFrom(ctx, vp, disk, format)
		if err != nil {
			return nil, err
		}
		for _, l := range levels {
			out = append(out, l.path)
			out = append(out, l.refs...)
			if l.dataFile != "" {
				out = append(out, l.dataFile)
			}
		}
	}
	return out, nil
}

// otherSnapshotFiles returns every file the snapshots of domain other than
// except reference (sourceFilesIn: their disk lists and recorded domain
// definitions). A snapshot list or definition that cannot be read is an
// error.
func otherSnapshotFiles(ctx context.Context, vp *VirshProvider, domain, except string) ([]string, error) {
	res, err := vp.runVirshCommand(ctx, "snapshot-list", domain, "--name")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, snap := range strings.Split(res.Stdout, "\n") {
		snap = strings.TrimSpace(snap)
		if snap == "" || snap == except {
			continue
		}
		x, err := vp.runVirshCommand(ctx, "snapshot-dumpxml", domain, snap)
		if err != nil {
			return nil, err
		}
		files, err := sourceFilesIn(x.Stdout)
		if err != nil {
			return nil, err
		}
		out = append(out, files...)
	}
	return out, nil
}
