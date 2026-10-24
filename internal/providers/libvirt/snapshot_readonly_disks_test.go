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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin V6 (v0.4.0-rc6 lab validation, libvirt 10): libvirt's
// external revert rebuilds the domain's disks from the snapshot's STORED
// definition and makes a qcow2 overlay of every disk there that has media —
// the read-only cloud-init CD-ROM included — which the VM then cannot open.
// A disk-only snapshot of a shut-off VM is therefore taken with its media
// ejected (snapshot_media.go), a revert refuses a snapshot whose stored
// definition has them (snapshot_readonly_disks.go), and re-inserts them after
// a revert.

// Generic V6 fixtures: domain ns.vm, its disk in the default pool, its seed
// CD-ROM hda in a per-create seed directory of the seed root v6SeedRoot.
const (
	v6Domain      = "ns.vm"
	v6UUID        = "6e6e6e6e-0000-4000-8000-000000000006"
	v6Disk        = "/var/lib/libvirt/images/ns.vm-disk.qcow2"
	v6SeedRoot    = "/var/lib/libvirt/virtrigaud-seeds"
	v6SeedDir     = v6SeedRoot + "/virtrigaud-cloudinit-ns.vm.Ab3dE5gH9k"
	v6SeedISO     = v6SeedDir + "/cloud-init.iso"
	v6SeedOverlay = v6SeedDir + "/cloud-init.1792831471"
	v6Epoch       = "1792831471"
)

// v6Roots are the provider's seed roots in these tests.
var v6Roots = []string{v6SeedRoot}

// v6Seed is the seed CD-ROM as a removable medium.
var v6Seed = removableMedium{Target: "hda", Source: v6SeedISO, Driver: "raw"}

// mediaDisk is one disk of a mediaHost domain.
type mediaDisk struct {
	target, device, source, driver string
	readonly                       bool
}

// mediaSnapshot is a snapshot a mediaHost stored: the domain's disks and
// media record when it was taken, and its disk list.
type mediaSnapshot struct {
	disks  []mediaDisk
	marker []removableMedium
	// xml, when set, is answered by snapshot-dumpxml instead of the render.
	xml string
}

// mediaHost is an in-memory libvirt host with one domain that models what
// V6 depends on: change-media eject/insert in the persistent definition, the
// media record in <metadata>, snapshot-create-as storing the definition, and
// libvirt's external revert — rebuilding the disks from the stored
// definition with a new <file without extension>.<epoch> overlay for every
// disk there that has media.
type mediaHost struct {
	state  string
	disks  []mediaDisk
	marker []removableMedium
	snaps  map[string]*mediaSnapshot
	calls  []string
	// fail maps a call prefix to the error it fails with.
	fail map[string]error
}

// newMediaHost is a shut-off ns.vm with vda and the seed CD-ROM hda.
func newMediaHost() *mediaHost {
	return &mediaHost{
		state: domStateShutOff,
		disks: []mediaDisk{
			{target: "vda", device: "disk", source: v6Disk, driver: "qcow2"},
			{target: "hda", device: "cdrom", source: v6SeedISO, driver: "raw", readonly: true},
		},
		snaps: map[string]*mediaSnapshot{},
		fail:  map[string]error{},
	}
}

// exited is a virsh command that ended on its own with exit status 1.
func exited(msg string) error {
	return &VirshError{Command: "virsh", ExitCode: 1, Stderr: "error: " + msg, Cause: &exec.ExitError{}}
}

// dropped is a virsh command whose session dropped: its outcome is unknown.
func dropped() error {
	return &VirshError{Command: "virsh", ExitCode: -1, Stderr: "", Cause: errors.New("ssh: session dropped")}
}

func renderMediaDisks(disks []mediaDisk) string {
	var b strings.Builder
	for _, d := range disks {
		fmt.Fprintf(&b, "<disk type='file' device='%s'><driver name='qemu' type='%s'/>", d.device, d.driver)
		if d.source != "" {
			fmt.Fprintf(&b, "<source file='%s'/>", d.source)
		}
		fmt.Fprintf(&b, "<target dev='%s'/>", d.target)
		if d.readonly {
			b.WriteString("<readonly/>")
		}
		b.WriteString("</disk>")
	}
	return b.String()
}

func renderMediaDomain(disks []mediaDisk, marker []removableMedium) string {
	meta := ""
	if marker != nil {
		meta = "<metadata>" + strings.Replace(renderMediaMarker(marker), "<ejected>",
			"<virtrigaud:ejected xmlns:virtrigaud='"+mediaMarkerURI+"'>", 1)
		meta = strings.Replace(meta, "</ejected>", "</virtrigaud:ejected>", 1) + "</metadata>"
	}
	return fmt.Sprintf("<domain type='kvm'><name>%s</name><uuid>%s</uuid>%s<devices>%s</devices></domain>",
		v6Domain, v6UUID, meta, renderMediaDisks(disks))
}

func (h *mediaHost) diskIdx(t string) int {
	for i, d := range h.disks {
		if d.target == t {
			return i
		}
	}
	return -1
}

func (h *mediaHost) source(t string) string {
	if i := h.diskIdx(t); i >= 0 {
		return h.disks[i].source
	}
	return ""
}

// overlayName is libvirt's default external name: the file without its
// extension, then "." and suffix.
func overlayName(file, suffix string) string {
	return strings.TrimSuffix(file, filepath.Ext(file)) + "." + suffix
}

func (h *mediaHost) virsh(args []string) (*hostconn.Result, error) {
	line := strings.Join(args, " ")
	h.calls = append(h.calls, line)
	for prefix, err := range h.fail {
		if strings.HasPrefix(line, prefix) {
			return nil, err
		}
	}
	ok := func(s string) (*hostconn.Result, error) { return &hostconn.Result{Stdout: s}, nil }
	switch args[0] {
	case "dumpxml":
		return ok(renderMediaDomain(h.disks, h.marker))
	case "list":
		return ok(v6UUID + "\n")
	case "domblklist":
		var b strings.Builder
		for _, d := range h.disks {
			fmt.Fprintf(&b, "file %s %s %s\n", d.device, d.target, d.source)
		}
		return ok(b.String())
	case "metadata":
		for i, a := range args {
			switch a {
			case "--remove":
				h.marker = nil
			case "--set":
				var m mediaMarkerElem
				if err := xml.Unmarshal([]byte(args[i+1]), &m); err != nil {
					return nil, exited("bad metadata: " + err.Error())
				}
				h.marker = append([]removableMedium{}, m.Media...)
			}
		}
		return ok("")
	case "change-media":
		i := h.diskIdx(args[2])
		if i < 0 {
			return nil, exited("disk not found")
		}
		if args[3] == "--eject" {
			h.disks[i].source = ""
			return ok("Successfully ejected media.")
		}
		if h.disks[i].source != "" {
			return nil, exited("The disk device '" + args[2] + "' already has media")
		}
		h.disks[i].source = args[3]
		return ok("Successfully inserted media.")
	case "snapshot-create-as":
		h.snaps[args[2]] = &mediaSnapshot{disks: append([]mediaDisk{}, h.disks...), marker: h.marker}
		return ok("Domain snapshot " + args[2] + " created")
	case "snapshot-dumpxml":
		s, found := h.snaps[args[2]]
		if !found {
			return nil, exited("snapshot not found")
		}
		if s.xml != "" {
			return ok(s.xml)
		}
		var list strings.Builder
		for _, d := range s.disks {
			mode := "no"
			if !d.readonly && d.source != "" {
				mode = "external"
			}
			fmt.Fprintf(&list, "<disk name='%s' snapshot='%s'/>", d.target, mode)
		}
		return ok("<domainsnapshot><name>" + args[2] + "</name><disks>" + list.String() + "</disks>" +
			renderMediaDomain(s.disks, s.marker) + "</domainsnapshot>")
	case "snapshot-revert":
		s := h.snaps[args[2]]
		h.disks = nil
		for _, d := range s.disks {
			if d.source != "" {
				d.source, d.driver = overlayName(d.source, v6Epoch), "qcow2"
			}
			h.disks = append(h.disks, d)
		}
		h.marker = s.marker
		if strings.Contains(line, "--running") {
			h.state = "running"
		}
		return ok("")
	}
	return nil, exited("unsupported: " + line)
}

// mediaConn is a libvirtConn on a mediaHost.
type mediaConn struct {
	*fakeSeamConn
	h *mediaHost
}

func newMediaConn(h *mediaHost) *mediaConn {
	return &mediaConn{fakeSeamConn: &fakeSeamConn{id: "single"}, h: h}
}

func (c *mediaConn) Virsh(_ context.Context, args ...string) (*hostconn.Result, error) {
	return c.h.virsh(args)
}

func (c *mediaConn) getDomainState(_ context.Context, _ string) (string, error) {
	return c.h.state, nil
}

func (c *mediaConn) snapshotExists(_ context.Context, _, snap string) (bool, error) {
	_, ok := c.h.snaps[snap]
	return ok, nil
}

// callsFrom returns the calls from the first one starting with prefix on.
func (h *mediaHost) callsFrom(prefix string) []string {
	for i, c := range h.calls {
		if strings.HasPrefix(c, prefix) {
			return h.calls[i:]
		}
	}
	return nil
}

func (h *mediaHost) called(prefix string) bool { return h.callsFrom(prefix) != nil }

func createSnap(t *testing.T, h *mediaHost, name string, memory bool, roots []string) error {
	t.Helper()
	_, err := snapshotCreateWith(context.Background(), newMediaConn(h), v6Domain,
		&providerv1.SnapshotCreateRequest{VmId: v6Domain, NameHint: name, Description: "d", IncludeMemory: memory},
		snapshotCreateOptions{seedRoots: roots})
	return err
}

func revertSnap(t *testing.T, h *mediaHost, name string, roots []string) error {
	t.Helper()
	_, err := snapshotRevertOn(context.Background(), newMediaConn(h), v6Domain, name, roots)
	return err
}

const (
	v6EjectHDA  = "change-media " + v6Domain + " hda --eject --config"
	v6InsertHDA = "change-media " + v6Domain + " hda " + v6SeedISO + " --insert --config"
	v6SetMarker = "metadata --domain " + v6Domain + " --uri " + mediaMarkerURI + " --key virtrigaud --set"
	v6RmMarker  = "metadata --domain " + v6Domain + " --uri " + mediaMarkerURI + " --remove --config"
)

// TestSnapshotCreate_ShutOffEjectsTheSeedAroundTheSnapshot: a disk-only
// snapshot of a shut-off VM records the seed, ejects it, takes the snapshot
// (no diskspec for the empty drive), inserts the seed again, checks it, and
// removes the record — in that order. The stored definition has an empty
// CD-ROM; the domain is as before.
func TestSnapshotCreate_ShutOffEjectsTheSeedAroundTheSnapshot(t *testing.T) {
	h := newMediaHost()
	require.NoError(t, createSnap(t, h, "p3", false, v6Roots))

	seq := h.callsFrom("metadata")
	require.Len(t, seq, 6, "%v", h.calls)
	assert.True(t, strings.HasPrefix(seq[0], v6SetMarker+" <ejected><media target='hda' source='"+v6SeedISO+"' driver='raw'/></ejected> --config"), seq[0])
	assert.Equal(t, v6EjectHDA, seq[1])
	assert.Equal(t, "snapshot-create-as "+v6Domain+" p3 --description d --atomic --disk-only", seq[2], "no diskspec for an empty drive")
	assert.Equal(t, v6InsertHDA, seq[3])
	assert.Equal(t, "dumpxml "+v6Domain, seq[4], "the seed is checked back in place")
	assert.Equal(t, v6RmMarker, seq[5])

	assert.Equal(t, v6SeedISO, h.source("hda"), "the domain has its seed again")
	assert.Nil(t, h.marker)
	stored := h.snaps["p3"]
	require.NotNil(t, stored)
	assert.Equal(t, "", stored.disks[1].source, "the snapshot stores an empty CD-ROM")
}

// TestSnapshotCreate_RunningOrMemoryNeverEjects: a running domain is never
// ejected (guest-visible); the seed is excluded with a diskspec instead, and
// the snapshot stores it (it cannot be reverted — see the gate).
func TestSnapshotCreate_RunningOrMemoryNeverEjects(t *testing.T) {
	for _, memory := range []bool{false, true} {
		h := newMediaHost()
		h.state = "running"
		require.NoError(t, createSnap(t, h, "live", memory, v6Roots))
		assert.False(t, h.called("change-media"), "no eject of a running domain")
		assert.False(t, h.called("metadata"))
		want := "snapshot-create-as " + v6Domain + " live --description d --atomic --disk-only --diskspec hda,snapshot=no"
		if memory {
			want = "snapshot-create-as " + v6Domain + " live --description d --atomic --diskspec hda,snapshot=no"
		}
		assert.True(t, h.called(want), "%v", h.calls)
		assert.Equal(t, v6SeedISO, h.snaps["live"].disks[1].source, "the snapshot stores the seed")
	}
}

// TestSnapshotCreate_EjectFailures: an eject that fails definitely puts back
// what was ejected and removes the record — no snapshot, nothing changed
// ("not performed"); one whose outcome is unknown keeps the record for the
// repair and is not "nothing done".
func TestSnapshotCreate_EjectFailures(t *testing.T) {
	tools := mediaDisk{target: "hdc", device: "cdrom", source: "/srv/iso/tools.iso", driver: "raw", readonly: true}

	t.Run("definite failure of the second eject", func(t *testing.T) {
		h := newMediaHost()
		h.disks = append(h.disks, tools)
		h.fail["change-media "+v6Domain+" hdc --eject"] = exited("cannot eject")
		err := createSnap(t, h, "p", false, v6Roots)
		require.Error(t, err)
		assert.False(t, h.called("snapshot-create-as"))
		assert.Equal(t, v6SeedISO, h.source("hda"), "the first is inserted again")
		assert.Nil(t, h.marker, "the record is removed")
		stage, _, ok := stageOf(err)
		require.True(t, ok)
		assert.Equal(t, stageBefore, stage, "nothing done")
	})
	t.Run("the first eject's session drops", func(t *testing.T) {
		h := newMediaHost()
		h.fail[v6EjectHDA] = dropped()
		err := createSnap(t, h, "p", false, v6Roots)
		require.Error(t, err)
		assert.ErrorIs(t, err, errMediaNotRestored)
		_, _, staged := stageOf(err)
		assert.False(t, staged, "never answered as nothing done")
		assert.False(t, h.called("snapshot-create-as"))
		assert.Equal(t, []removableMedium{v6Seed}, h.marker, "the record stays for the repair")
	})
}

// TestSnapshotCreate_FailureStillReinserts: a snapshot-create-as that fails
// is followed by the re-insert; a failed create with the seed back in place is
// the historical stageCommand error, one with the seed still out is not.
func TestSnapshotCreate_FailureStillReinserts(t *testing.T) {
	h := newMediaHost()
	h.fail["snapshot-create-as"] = exited("scripted")
	err := createSnap(t, h, "p", false, v6Roots)
	require.Error(t, err)
	assert.True(t, h.called(v6InsertHDA), "inserted again after the failed create")
	assert.Equal(t, v6SeedISO, h.source("hda"))
	stage, _, ok := stageOf(err)
	require.True(t, ok)
	assert.Equal(t, stageCommand, stage)

	h = newMediaHost()
	h.fail["snapshot-create-as"] = exited("scripted")
	h.fail[v6InsertHDA] = exited("scripted")
	err = createSnap(t, h, "p", false, v6Roots)
	require.Error(t, err)
	_, _, staged := stageOf(err)
	assert.False(t, staged, "the domain was changed: never read as not created")
	assert.ErrorIs(t, err, errMediaNotRestored)
}

// TestSnapshotCreate_ReinsertFailureIsRepairedByTheNextCall: a snapshot made
// whose seed could not be inserted again is answered as made; the record
// stays, and the next snapshot call of the domain first inserts the seed —
// provably the domain's own — and removes the record.
func TestSnapshotCreate_ReinsertFailureIsRepairedByTheNextCall(t *testing.T) {
	h := newMediaHost()
	h.fail[v6InsertHDA] = exited("scripted")
	require.NoError(t, createSnap(t, h, "p1", false, v6Roots), "the snapshot exists")
	assert.Equal(t, "", h.source("hda"))
	assert.Equal(t, []removableMedium{v6Seed}, h.marker)

	delete(h.fail, v6InsertHDA)
	h.calls = nil
	require.NoError(t, createSnap(t, h, "p2", false, v6Roots))
	insert, set := indexOf(h.calls, v6InsertHDA), -1
	for i, c := range h.calls {
		if strings.HasPrefix(c, v6SetMarker) {
			set = i
			break
		}
	}
	require.GreaterOrEqual(t, insert, 0, "%v", h.calls)
	assert.Less(t, insert, set, "the repair inserts the seed before the new snapshot's ejection: %v", h.calls)
	assert.Equal(t, v6SeedISO, h.source("hda"))
	assert.Nil(t, h.marker)
	assert.Equal(t, "", h.snaps["p2"].disks[1].source, "p2 was taken with the seed ejected again")
}

// TestRestoreEjectedMedia_OnlyTheDomainsOwnSeed: the repair inserts only a
// recorded medium that is provably the domain's own seed, into a drive that
// is still empty; anything else is left (and its record kept).
func TestRestoreEjectedMedia_OnlyTheDomainsOwnSeed(t *testing.T) {
	for name, tc := range map[string]struct {
		marker     []removableMedium
		hda        string
		roots      []string
		wantInsert bool
		wantMarker bool
	}{
		"the domain's own seed":                {[]removableMedium{v6Seed}, "", v6Roots, true, false},
		"no seed roots":                        {[]removableMedium{v6Seed}, "", nil, false, true},
		"an installer ISO":                     {[]removableMedium{{Target: "hda", Source: "/srv/iso/installer.iso"}}, "", v6Roots, false, true},
		"another domain's seed":                {[]removableMedium{{Target: "hda", Source: v6SeedRoot + "/virtrigaud-cloudinit-ns.other.Ab3dE5gH9k/cloud-init.iso"}}, "", v6Roots, false, true},
		"a seed outside the roots":             {[]removableMedium{{Target: "hda", Source: "/tmp/virtrigaud-cloudinit-ns.vm.Ab3dE5gH9k/cloud-init.iso"}}, "", v6Roots, false, true},
		"a relative path":                      {[]removableMedium{{Target: "hda", Source: "virtrigaud-cloudinit-ns.vm.Ab3dE5gH9k/cloud-init.iso"}}, "", v6Roots, false, true},
		"the drive holds a medium again":       {[]removableMedium{v6Seed}, v6SeedISO, v6Roots, false, false},
		"an earlier release's per-name seed":   {[]removableMedium{{Target: "hda", Source: v6SeedRoot + "/virtrigaud-cloudinit/ns.vm/cloud-init.iso"}}, "", v6Roots, true, false},
		"a recorded target that is not a disk": {[]removableMedium{{Target: "hdz", Source: v6SeedISO}}, v6SeedISO, v6Roots, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newMediaHost()
			h.disks[1].source = tc.hda
			h.marker = tc.marker
			doc, err := parseDomainDisks(renderMediaDomain(h.disks, h.marker))
			require.NoError(t, err)
			changed, err := restoreEjectedMedia(context.Background(), newMediaConnRunner(h), v6Domain, doc, tc.roots)
			require.NoError(t, err)
			assert.Equal(t, tc.wantInsert, changed)
			assert.Equal(t, tc.wantInsert, h.called("change-media"), "%v", h.calls)
			assert.Equal(t, tc.wantMarker, h.marker != nil)
		})
	}
}

// newMediaConnRunner is a hostCommandRunner on h.
func newMediaConnRunner(h *mediaHost) hostCommandRunner { return hostConnRunner{conn: newMediaConn(h)} }

// TestSnapshotRevert_NewStyleSnapshotRevertsWithTheSeedBack: a snapshot taken
// with the seed ejected is reverted; libvirt leaves the CD-ROM empty (no
// overlay of the seed), and the provider inserts the pre-revert seed again
// (recorded first), checks it, removes the record, and answers success.
func TestSnapshotRevert_NewStyleSnapshotRevertsWithTheSeedBack(t *testing.T) {
	h := newMediaHost()
	require.NoError(t, createSnap(t, h, "p3", false, v6Roots))
	h.calls = nil
	require.NoError(t, revertSnap(t, h, "p3", v6Roots))

	seq := h.callsFrom("snapshot-revert")
	require.GreaterOrEqual(t, len(seq), 6, "%v", h.calls)
	assert.Equal(t, "snapshot-revert "+v6Domain+" p3 --force", seq[0])
	assert.Equal(t, "dumpxml "+v6Domain, seq[1])
	assert.True(t, strings.HasPrefix(seq[2], v6SetMarker), seq[2])
	assert.Equal(t, v6InsertHDA, seq[3])
	assert.Equal(t, "dumpxml "+v6Domain, seq[4])
	assert.Equal(t, v6RmMarker, seq[5])

	assert.Equal(t, v6SeedISO, h.source("hda"), "the raw seed ISO, no overlay")
	assert.Equal(t, overlayName(v6Disk, v6Epoch), h.source("vda"), "the disk is the revert's overlay")
	assert.Equal(t, "raw", h.disks[1].driver)
	assert.Nil(t, h.marker)
}

// TestSnapshotRevert_RunningRevertInsertsLiveToo: a revert that keeps the VM
// running inserts the seed into the running domain too.
func TestSnapshotRevert_RunningRevertInsertsLiveToo(t *testing.T) {
	h := newMediaHost()
	require.NoError(t, createSnap(t, h, "p3", false, v6Roots))
	h.state = "running"
	require.NoError(t, revertSnap(t, h, "p3", v6Roots))
	assert.True(t, h.called(v6InsertHDA+" --live"), "%v", h.calls)
}

// TestSnapshotRevert_ReinsertFailure: a revert after which the seed cannot be
// inserted again is answered FailedPrecondition (the revert happened; it is
// not sent again), with the record kept so the next power-on inserts it.
func TestSnapshotRevert_ReinsertFailure(t *testing.T) {
	h := newMediaHost()
	require.NoError(t, createSnap(t, h, "p3", false, v6Roots))
	h.fail[v6InsertHDA] = exited("scripted")
	err := revertSnap(t, h, "p3", v6Roots)
	var me *snapshotRevertMediaError
	require.True(t, errors.As(err, &me), "%v", err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), "reverted, but CD-ROM hda could not be re-inserted")
	assert.NotContains(t, st.Message(), "/", "no host path crosses the wire")
	assert.Equal(t, []removableMedium{v6Seed}, h.marker, "recorded for the power-on repair")
}

// rc6AsCreated is `virsh snapshot-dumpxml` of a snapshot v0.4.0-rc6 took of
// the shut-off VM: libvirt already records the read-only CD-ROM 'no', but the
// stored definition has the seed in it.
var rc6AsCreated = "<domainsnapshot><name>s1</name><state>shutoff</state><disks>" +
	"<disk name='vda' snapshot='external' type='file'><driver type='qcow2'/><source file='/var/lib/libvirt/images/ns.vm-disk.s1'/></disk>" +
	"<disk name='hda' snapshot='no'/></disks>" +
	renderMediaDomain([]mediaDisk{
		{target: "vda", device: "disk", source: v6Disk, driver: "qcow2"},
		{target: "hda", device: "cdrom", source: v6SeedISO, driver: "raw", readonly: true},
	}, nil) + "</domainsnapshot>"

// rc6AfterRevert is the same snapshot's metadata after libvirt reverted it:
// the CD-ROM rewritten to an external overlay of the ISO.
var rc6AfterRevert = "<domainsnapshot><name>s1</name><state>shutoff</state><disks>" +
	"<disk name='vda' snapshot='external' type='file'><driver type='qcow2'/><source file='/var/lib/libvirt/images/ns.vm-disk." + v6Epoch + "'/></disk>" +
	"<disk name='hda' snapshot='external' type='file'><driver type='qcow2'/><source file='" + v6SeedOverlay + "'/></disk></disks>" +
	renderMediaDomain([]mediaDisk{
		{target: "vda", device: "disk", source: v6Disk, driver: "qcow2"},
		{target: "hda", device: "cdrom", source: v6SeedISO, driver: "raw", readonly: true},
	}, nil) + "</domainsnapshot>"

// newStyle is a snapshot this release takes: the stored CD-ROM is empty.
var newStyle = "<domainsnapshot><name>s1</name><state>shutoff</state><disks>" +
	"<disk name='vda' snapshot='external' type='file'><driver type='qcow2'/><source file='/var/lib/libvirt/images/ns.vm-disk.s1'/></disk>" +
	"<disk name='hda' snapshot='no'/></disks>" +
	renderMediaDomain([]mediaDisk{
		{target: "vda", device: "disk", source: v6Disk, driver: "qcow2"},
		{target: "hda", device: "cdrom", driver: "raw", readonly: true},
	}, []removableMedium{v6Seed}) + "</domainsnapshot>"

// TestSnapshotRevertGate: the three real shapes. Both rc6 shapes are refused
// (the stored definition has the seed; after a revert the CD-ROM is also
// recorded external); this release's is accepted. A snapshot without a
// stored definition cannot be checked.
func TestSnapshotRevertGate(t *testing.T) {
	doc, err := parseDomainDisks(renderMediaDomain(newMediaHost().disks, nil))
	require.NoError(t, err)
	for name, tc := range map[string]struct {
		xml     string
		want    []string
		wantErr bool
	}{
		"rc6, as created":      {xml: rc6AsCreated, want: []string{"hda"}},
		"rc6, after a revert":  {xml: rc6AfterRevert, want: []string{"hda"}},
		"this release's":       {xml: newStyle},
		"no stored definition": {xml: "<domainsnapshot><name>s1</name></domainsnapshot>", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := parseSnapshotDiskModes(tc.xml)
			require.NoError(t, err)
			got, err := s.snapshotRevertGate(doc)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			if len(tc.want) == 0 {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// requireRevertRefused asserts the V6 refusal: FailedPrecondition, guidance
// first, no host path, and no snapshot-revert sent.
func requireRevertRefused(t *testing.T, err error, reverted bool) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "a gRPC status: %v", err)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	for _, want := range []string{"revert refused, nothing changed: take a new snapshot with the VM powered off",
		"taken while it ran or by an earlier release", "cannot revert it safely"} {
		assert.Contains(t, st.Message(), want)
	}
	assert.NotContains(t, st.Message(), "/", "no host path crosses the wire")
	assert.False(t, reverted, "no snapshot-revert is sent")
}

// TestSnapshotRevert_RefusesUnsafeSnapshotsBeforeTouchingTheHost: an rc6
// snapshot (either shape) and one taken while the VM ran are refused before
// anything changes; the VM keeps its seed.
func TestSnapshotRevert_RefusesUnsafeSnapshotsBeforeTouchingTheHost(t *testing.T) {
	for name, setup := range map[string]func(h *mediaHost){
		"rc6, as created":     func(h *mediaHost) { h.snaps["s1"] = &mediaSnapshot{xml: rc6AsCreated} },
		"rc6, after a revert": func(h *mediaHost) { h.snaps["s1"] = &mediaSnapshot{xml: rc6AfterRevert} },
		"taken while running": func(h *mediaHost) {
			h.state = "running"
			require.NoError(t, createSnap(t, h, "s1", false, v6Roots))
			h.state = domStateShutOff
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newMediaHost()
			setup(h)
			h.calls = nil
			err := revertSnap(t, h, "s1", v6Roots)
			requireRevertRefused(t, err, h.called("snapshot-revert"))
			assert.False(t, h.called("change-media"))
			assert.Equal(t, v6SeedISO, h.source("hda"))
		})
	}
}

// TestSnapshotRevert_UnreadableSnapshotIsNotReverted: fail closed.
func TestSnapshotRevert_UnreadableSnapshotIsNotReverted(t *testing.T) {
	for name, setup := range map[string]func(h *mediaHost){
		"the read fails":       func(h *mediaHost) { h.fail["snapshot-dumpxml"] = exited("scripted") },
		"it does not parse":    func(h *mediaHost) { h.snaps["s1"] = &mediaSnapshot{xml: "<domainsnapshot><disks>"} },
		"no stored definition": func(h *mediaHost) { h.snaps["s1"] = &mediaSnapshot{xml: "<domainsnapshot/>"} },
		"the domain read fails": func(h *mediaHost) {
			h.fail["dumpxml"] = exited("scripted")
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newMediaHost()
			h.snaps["s1"] = &mediaSnapshot{xml: newStyle}
			setup(h)
			require.Error(t, revertSnap(t, h, "s1", v6Roots))
			assert.False(t, h.called("snapshot-revert"))
		})
	}
}

// TestSnapshotRevert_RepairsAnInterruptedEjectionFirst: a domain left with
// its seed ejected (and recorded) gets it back before the revert, so the
// revert re-inserts it afterwards too.
func TestSnapshotRevert_RepairsAnInterruptedEjectionFirst(t *testing.T) {
	h := newMediaHost()
	require.NoError(t, createSnap(t, h, "p3", false, v6Roots))
	h.disks[1].source = ""
	h.marker = []removableMedium{v6Seed}
	h.calls = nil
	require.NoError(t, revertSnap(t, h, "p3", v6Roots))
	before := h.calls[:len(h.calls)-len(h.callsFrom("snapshot-revert"))]
	assert.Contains(t, before, v6InsertHDA, "repaired before the revert")
	assert.Equal(t, v6SeedISO, h.source("hda"))
	assert.Nil(t, h.marker)
}

// TestSingleHost_SnapshotRevert_RefusalOnTheWire: through the Server, the
// refusal is FailedPrecondition (terminal RevertFailed in the manager).
func TestSingleHost_SnapshotRevert_RefusalOnTheWire(t *testing.T) {
	h := newMediaHost()
	h.snaps["s1"] = &mediaSnapshot{xml: rc6AsCreated}
	s := &Server{provider: &fakeSeamProvider{hostConn: newMediaConn(h)}}
	_, err := s.SnapshotRevert(context.Background(), &providerv1.SnapshotRevertRequest{VmId: v6Domain, SnapshotId: "s1"})
	requireRevertRefused(t, err, h.called("snapshot-revert"))
	st, _ := status.FromError(err)
	assert.Contains(t, st.Message(), `"s1"`)
	assert.Contains(t, st.Message(), "disk hda")
}

// TestSingleHost_SnapshotRevert_RewrittenSeedIsAFailure: a revert after which
// a read-only disk is on a file neither the domain nor the snapshot had for it
// is answered as a failure (reverted, the VM may not start), never a success.
func TestSingleHost_SnapshotRevert_RewrittenSeedIsAFailure(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
	fx.script("single", "snaps-"+scdDomain, "s1\n")
	overlay := strings.Replace(scdSeedISO, "cloud-init.iso", "cloud-init."+v6Epoch, 1)
	fx.script("single", "revert-rewrites", strings.Replace(scdDomainXML(scdDomain, scdDomainOpts{}), scdSeedISO, overlay, 1))
	_, err := NewServer(fx.p).SnapshotRevert(context.Background(), &providerv1.SnapshotRevertRequest{VmId: scdDomain, SnapshotId: "s1"})
	require.Error(t, err)
	assert.True(t, called(fx.calls(), "single virsh snapshot-revert"), "the revert was sent")
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), "reverted, but the VM may not start: libvirt moved read-only disk sda")
	assert.NotContains(t, st.Message(), "/", "no host path crosses the wire")
}

// called reports whether a logged call starts with prefix.
func called(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// TestClustered_Snapshots_MediaHandlingIsRouted: on a clustered provider the
// create ejects and re-inserts the seed on the leased host by the
// owner-checked UUID, and the revert refuses an rc6 snapshot there —
// FailedPrecondition with VM_OPERATION_FAILED — without reverting.
func TestClustered_Snapshots_MediaHandlingIsRouted(t *testing.T) {
	uuid := routingDomainUUID
	ctx := context.Background()

	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	_, err := NewServer(fx.p).SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "pre", Description: "d",
	})
	require.NoError(t, err)
	calls := fx.calls()
	for _, want := range []string{
		"host-b virsh change-media " + uuid + " sda --eject --config",
		"host-b virsh snapshot-create-as " + uuid + " pre --description d --atomic --disk-only",
		"host-b virsh change-media " + uuid + " sda " + scdSeedISO + " --insert --config",
		"host-b virsh metadata --domain " + uuid + " --uri " + mediaMarkerURI + " --remove --config",
	} {
		assert.Contains(t, calls, want)
	}

	fx = ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	fx.script("host-b", "snapxml-s2", strings.ReplaceAll(rc6AsCreated, "'hda'", "'sda'"))
	_, err = NewServer(fx.p).SnapshotRevert(ctx, &providerv1.SnapshotRevertRequest{
		VmId: "web", SnapshotId: "s2", TargetHostId: "host-b", Owner: teamAOwner,
	})
	requireRevertRefused(t, err, called(fx.calls(), "host-b virsh snapshot-revert"))
	st, _ := status.FromError(err)
	assert.True(t, errorInfoReasons(st)[contracts.VMOperationFailedReason], "a per-VM failure on its host")
	assert.Contains(t, fx.calls(), "host-b virsh snapshot-dumpxml "+uuid+" s2", "read by the owner-checked UUID")
}

// TestPowerOn_ReinsertsAnEjectedSeedFirst: a start of a domain an interrupted
// snapshot left without its seed (recorded) first inserts the seed — provably
// the domain's own, in the provider's seed root — then starts it; a seed that
// cannot be inserted fails the start, which is not attempted.
func TestPowerOn_ReinsertsAnEjectedSeedFirst(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*scdFixture, string) {
		t.Helper()
		// The seed root is the fixture's staging directory (hostSeedDir).
		staging := t.TempDir()
		seed := filepath.Join(staging, cloudInitSeedDirPrefix+scdDomain+".Ab3dE5gH9k", cloudInitISOName)
		marker := "<metadata><virtrigaud:ejected xmlns:virtrigaud='" + mediaMarkerURI + "'>" +
			"<media target='sda' source='" + seed + "' driver='raw'/></virtrigaud:ejected></metadata>"
		ejected := strings.Replace(strings.Replace(scdDomainXML(scdDomain, scdDomainOpts{}), "<source file='"+scdSeedISO+"'/>", "", 1),
			"<devices>", marker+"<devices>", 1)
		fx := newSCDFixture(t, map[string]string{scdDomain: ejected})
		fx.p.hostSeedDir, fx.p.hostStagingDir = staging, staging
		fx.script("single", "after-change-media", strings.Replace(scdDomainXML(scdDomain, scdDomainOpts{}), scdSeedISO, seed, 1))
		return fx, seed
	}

	t.Run("inserted, then started", func(t *testing.T) {
		fx, seed := setup(t)
		_, err := fx.p.Power(ctx, contracts.VMRef{ID: scdDomain}, contracts.PowerOpOn)
		require.NoError(t, err)
		calls := fx.calls()
		insert := "single virsh change-media " + scdDomain + " sda " + seed + " --insert --config"
		require.Contains(t, calls, insert)
		assert.Less(t, indexOf(calls, insert), indexOf(calls, "single virsh start "+scdDomain), "before the start")
	})
	t.Run("not inserted: not started", func(t *testing.T) {
		fx, _ := setup(t)
		fx.script("single", "fail-change-media", "")
		_, err := fx.p.Power(ctx, contracts.VMRef{ID: scdDomain}, contracts.PowerOpOn)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "start of domain "+scdDomain+" not performed")
		assert.False(t, called(fx.calls(), "single virsh start"))
	})
}

func TestIsOwnSeedISO(t *testing.T) {
	for path, want := range map[string]bool{
		v6SeedISO: true,
		v6SeedRoot + "/virtrigaud-cloudinit/ns.vm/cloud-init.iso":                 true,
		v6SeedRoot + "/virtrigaud-cloudinit-ns.vm.short/cloud-init.iso":           false,
		v6SeedRoot + "/virtrigaud-cloudinit-ns.vmx.Ab3dE5gH9k/cloud-init.iso":     false,
		v6SeedRoot + "/virtrigaud-cloudinit-ns.vm.Ab3dE5gH9k/user-data":           false,
		v6SeedRoot + "/virtrigaud-cloudinit-ns.vm.Ab3dE5gH9k/../x/cloud-init.iso": false,
		v6SeedRoot + "/sub/virtrigaud-cloudinit-ns.vm.Ab3dE5gH9k/cloud-init.iso":  false,
		"/srv/iso/installer.iso": false,
	} {
		assert.Equal(t, want, isOwnSeedISO(path, v6Domain, v6Roots), path)
	}
	assert.False(t, isOwnSeedISO(v6SeedISO, "", v6Roots), "no domain name: nothing is provable")
}

func TestBuildSnapshotCreateArgs_ExcludesReadOnlyDisks(t *testing.T) {
	const vm, name, desc = "vm-1", "snap-1", "d"
	base := []string{"snapshot-create-as", vm, name, "--description", desc, "--atomic"}
	for _, tc := range []struct {
		name     string
		memory   bool
		excluded []string
		want     []string
	}{
		{"no read-only disk with media: no diskspec", false, nil, append(append([]string{}, base...), "--disk-only")},
		{"a drive that keeps its medium", false, []string{"hda"},
			append(append([]string{}, base...), "--disk-only", "--diskspec", "hda,snapshot=no")},
		{"a memory snapshot", true, []string{"hda", "sdb"},
			append(append([]string{}, base...), "--diskspec", "hda,snapshot=no", "--diskspec", "sdb,snapshot=no")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, mem := buildSnapshotCreateArgs(vm, name, desc, tc.memory, true, tc.excluded)
			assert.Equal(t, tc.memory, mem)
			assert.Equal(t, tc.want, args)
		})
	}
}

func TestSnapshotExcludedTargetsAndEjectableMedia(t *testing.T) {
	disks := append(newMediaHost().disks,
		mediaDisk{target: "hdc", device: "cdrom", driver: "raw", readonly: true},                                        // empty
		mediaDisk{target: "sdb", device: "disk", source: "/srv/ref.img", driver: "raw", readonly: true},                 // read-only data disk
		mediaDisk{target: "fda", device: "floppy", source: "/srv/f.img", driver: "raw"},                                 // floppy
		mediaDisk{target: "vdb", device: "disk", source: "/var/lib/libvirt/images/ns.vm-disk-1.qcow2", driver: "qcow2"}) // writable
	doc, err := parseDomainDisks(renderMediaDomain(disks, nil))
	require.NoError(t, err)

	media := doc.ejectableMedia()
	assert.Equal(t, []string{"hda", "fda"}, targetsOf(media), "file-backed drives with a medium")
	excluded, err := doc.snapshotExcludedTargets(media)
	require.NoError(t, err)
	assert.Equal(t, []string{"sdb"}, excluded, "what is not ejected and still has a source")
	all, err := doc.snapshotExcludedTargets(nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"hda", "sdb", "fda"}, all, "never an empty drive or a writable disk")

	bad, err := parseDomainDisks(renderMediaDomain(append(newMediaHost().disks,
		mediaDisk{target: "h,x", device: "cdrom", source: "/srv/x.iso", readonly: true}), nil))
	require.NoError(t, err)
	assert.NotContains(t, targetsOf(bad.ejectableMedia()), "h,x", "never put in an argument")
	_, err = bad.snapshotExcludedTargets(bad.ejectableMedia())
	assert.Error(t, err, "a medium that can be neither ejected nor excluded: no snapshot")
}

// managerDetailBytes is how much of a provider's answer the manager keeps in
// a VM's status ("snapshotRevert failed: " + the message; maxProviderDetailBytes).
const managerDetailBytes = 256

// TestSnapshotRevertMessages_GuidanceSurvivesTheStatusCap: the guidance comes
// first, so it survives the cap with the longest snapshot name; a
// default-named snapshot's message fits whole, routed prefix included.
func TestSnapshotRevertMessages_GuidanceSurvivesTheStatusCap(t *testing.T) {
	const prefix = "snapshotRevert failed: failed to revert to snapshot: "
	long := strings.Repeat("s", 64)
	for _, tc := range []struct {
		err   error
		guide string
	}{
		{&snapshotRevertUnsafeError{snapshot: long, disks: []string{"hda", "hdc"}}, "take a new snapshot with the VM powered off"},
		{&snapshotRevertBrokeDisksError{snapshot: long, disks: []string{"hda", "hdc"}}, "repair it before another revert"},
		{&snapshotRevertMediaError{snapshot: long, drives: []string{"hda", "hdc"}}, "the next power-on re-inserts a cloud-init seed"},
	} {
		kept := prefix + tc.err.Error()
		if len(kept) > managerDetailBytes {
			kept = kept[:managerDetailBytes]
		}
		assert.Contains(t, kept, tc.guide)
	}
	for _, err := range []error{
		&snapshotRevertUnsafeError{snapshot: "snapshot-1792831471", disks: []string{"hda"}},
		&snapshotRevertBrokeDisksError{snapshot: "snapshot-1792831471", disks: []string{"hda"}},
		&snapshotRevertMediaError{snapshot: "snapshot-1792831471", drives: []string{"hda"}},
	} {
		assert.LessOrEqual(t, len(prefix+err.Error()), managerDetailBytes, "fits whole: %q", err.Error())
	}
}

func TestIsOwnOverlayName(t *testing.T) {
	for _, tc := range []struct {
		base string
		want bool
	}{
		{"web-disk.s1", true},
		{"web-disk.1792831471", true}, // a revert's overlay, recorded by the snapshot
		{"web-disk.s2", false},        // another snapshot's
		{"web-disk.qcow2", false},     // the base
		{"web-disk.0792831471", false},
		{"web-disk.17928", false},
		{"web-disk.1792831471.x", false},
		{"other-disk.1792831471", false},
		{"cloud-init.1792831471", false}, // the seed's overlay is never the disk's
		{"web-disk1792831471", false},
	} {
		assert.Equal(t, tc.want, isOwnOverlayName(tc.base, "web-disk", "s1"), tc.base)
	}
}

func TestParseSnapshotOverlays_AfterARevert(t *testing.T) {
	got, err := parseSnapshotOverlays(`<domainsnapshot><name>s1</name>` +
		`<disks><disk name='vda' snapshot='external'><source file='/pool/web-disk.s1'/></disk></disks>` +
		`<revertDisks><disk name='vda' snapshot='external'><source file='/pool/web-disk.1792831471'/></disk></revertDisks>` +
		`</domainsnapshot>`)
	require.NoError(t, err)
	assert.Equal(t, []string{"/pool/web-disk.s1", "/pool/web-disk.1792831471"}, got)
}

// TestSnapshotDelete_AfterARevert: once the domain was reverted to s1, libvirt
// records the revert's overlay, <domain>-disk.<epoch>, as s1's; deleting s1
// removes it once it is out of the chain and nobody uses it, and keeps it
// while the domain (or another snapshot) still reads it.
func TestSnapshotDelete_AfterARevert(t *testing.T) {
	epochOverlay := func(t *testing.T, c *overlayCase) {
		t.Helper()
		o := filepath.Join(fixtureImagesDir, "web-disk."+v6Epoch)
		require.NoError(t, os.WriteFile(o, []byte("overlay"), 0o600))
		t.Cleanup(func() { _ = os.Remove(o) })
		c.overlay = o
		c.script("snapxml-s1", externalSnapshotXML("s1", o, c.base))
	}

	t.Run("removed once out of the chain", func(t *testing.T) {
		c := newOverlayCase(t, nil)
		epochOverlay(t, c)
		c.deleteS1(t)
		assert.True(t, c.removed(), "the revert's overlay is removed: %v", c.fx.calls())
	})
	t.Run("kept while the domain still reads it", func(t *testing.T) {
		c := newOverlayCase(t, nil)
		epochOverlay(t, c)
		x := snapOverlayDomainXML("web", overlayDomainUUID, c.overlay)
		c.script("dom-web.xml", x)
		c.script("dom-"+overlayDomainUUID+".xml", x)
		c.deleteS1(t)
		assert.False(t, c.removed(), "kept: %v", c.fx.calls())
	})
	t.Run("kept while a later snapshot records it", func(t *testing.T) {
		c := newOverlayCase(t, nil)
		epochOverlay(t, c)
		c.script("snaps", "s1\ns2\n")
		c.script("snapxml-s2", externalSnapshotXML("s2", filepath.Join(fixtureImagesDir, "web-disk.s2"), c.overlay))
		c.deleteS1(t)
		assert.False(t, c.removed(), "kept: %v", c.fx.calls())
	})
	t.Run("a later snapshot's overlay over the revert's is its own", func(t *testing.T) {
		// s2 was taken after the revert: libvirt names its overlay after the
		// disk without its extension — <domain>-disk.<epoch> loses ".<epoch>"
		// — so it is <domain>-disk.s2, the name the cleanup expects.
		c := newOverlayCase(t, nil)
		s2 := filepath.Join(fixtureImagesDir, "web-disk.s2")
		require.NoError(t, os.WriteFile(s2, []byte("overlay"), 0o600))
		t.Cleanup(func() { _ = os.Remove(s2) })
		c.overlay = s2
		c.script("snaps", "s2\n")
		c.script("snapxml-s2", externalSnapshotXML("s2", s2, filepath.Join(fixtureImagesDir, "web-disk."+v6Epoch)))
		c.deleteSnap(t, "s2")
		assert.True(t, c.removed(), "removed: %v", c.fx.calls())
	})
}

// TestDelete_SingleHost_RemovesARevertedChain: a VM whose disk is a revert's
// overlay (<vm>-disk.<epoch> over <vm>-disk.qcow2 over a base image), with or
// without a later snapshot's overlay above it, is deleted with its whole own
// chain; the base image is kept.
func TestDelete_SingleHost_RemovesARevertedChain(t *testing.T) {
	for _, laterSnapshot := range []bool{false, true} {
		t.Run(fmt.Sprintf("later snapshot=%v", laterSnapshot), func(t *testing.T) {
			c := newCreateHost(t)
			reverted := c.file(c.images, "team-a.web-disk."+v6Epoch)
			own := c.file(c.images, "team-a.web-disk.qcow2")
			base := c.file(c.images, "golden.qcow2")
			top := reverted
			want := []string{reverted, own}
			if laterSnapshot {
				top = c.file(c.images, "team-a.web-disk.s2")
				c.chainSidecar(top, reverted, own, base)
				want = []string{top, reverted, own}
			} else {
				c.chainSidecar(reverted, own, base)
			}
			c.define("h1", "team-a.web", uuidSource, depDomainXML("team-a.web", uuidSource, top, ""))
			require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "names"), []byte("team-a.web\n"), 0o600))

			_, err := c.p.Delete(context.Background(), contracts.VMRef{ID: "team-a.web"})
			require.NoError(t, err)
			assert.Equal(t, want, c.removals(), "its own chain, the revert's overlay included; never the base image")
			assert.FileExists(t, base)
		})
	}
}
