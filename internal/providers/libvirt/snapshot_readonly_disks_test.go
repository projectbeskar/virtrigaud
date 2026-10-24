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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin V6 (v0.4.0-rc6 lab validation, libvirt 10): a disk-only
// snapshot used to record the read-only cloud-init CD-ROM as an external
// snapshot disk, and a later revert rewrote the CD-ROM to a qcow2 overlay in
// the private seed directory that the VM could not open
// (snapshot_readonly_disks.go).

// Generic paths of the V6 fixtures: domain ns.vm, its disk in the default
// pool and its seed CD-ROM in a per-create seed directory.
const (
	v6Domain  = "ns.vm"
	v6Disk    = "/var/lib/libvirt/images/ns.vm-disk.qcow2"
	v6SeedISO = "/var/lib/libvirt/virtrigaud-seeds/virtrigaud-cloudinit-ns.vm.X/cloud-init.iso"
	// v6SeedOverlay is the overlay libvirt's revert made of the ISO.
	v6SeedOverlay = "/var/lib/libvirt/virtrigaud-seeds/virtrigaud-cloudinit-ns.vm.X/cloud-init.1792831471"
	v6UUID        = "6e6e6e6e-0000-4000-8000-000000000006"
)

// v6RC6SnapshotXML is `virsh snapshot-dumpxml` of a snapshot v0.4.0-rc6 took
// (the shape the lab read): BOTH disks external, the CD-ROM included.
const v6RC6SnapshotXML = `<domainsnapshot><name>s1</name><state>shutoff</state>
  <disks>
    <disk name='vda' snapshot='external' type='file'><driver type='qcow2'/><source file='/var/lib/libvirt/images/ns.vm-disk.s1'/></disk>
    <disk name='hda' snapshot='external' type='file'><driver type='qcow2'/><source file='/var/lib/libvirt/virtrigaud-seeds/virtrigaud-cloudinit-ns.vm.X/cloud-init.iso'/></disk>
  </disks></domainsnapshot>`

// v6SafeSnapshotXML is the same snapshot as this release takes it: the
// CD-ROM excluded (snapshot='no').
const v6SafeSnapshotXML = `<domainsnapshot><name>s1</name><state>shutoff</state>
  <disks>
    <disk name='vda' snapshot='external' type='file'><driver type='qcow2'/><source file='/var/lib/libvirt/images/ns.vm-disk.s1'/></disk>
    <disk name='hda' snapshot='no'/>
  </disks></domainsnapshot>`

// v6DomainXML is ns.vm's `virsh dumpxml` with its CD-ROM on seed (and, when
// extra is set, the given extra <disk> elements).
func v6DomainXML(seed string, extra ...string) string {
	return fmt.Sprintf("<domain type='kvm'>\n  <name>%s</name>\n  <uuid>%s</uuid>\n  <devices>\n"+
		"    <disk type='file' device='disk'>\n      <driver name='qemu' type='qcow2'/>\n      <source file='%s'/>\n"+
		"      <target dev='vda' bus='virtio'/>\n    </disk>\n"+
		"    <disk type='file' device='cdrom'>\n      <driver name='qemu' type='raw'/>\n      <source file='%s'/>\n"+
		"      <target dev='hda' bus='ide'/>\n      <readonly/>\n    </disk>\n%s"+
		"  </devices>\n</domain>\n", v6Domain, v6UUID, v6Disk, seed, strings.Join(extra, ""))
}

// v6Fixture is a single host defining ns.vm with snapshot s1, whose
// snapshot-dumpxml is snapXML.
func v6Fixture(t *testing.T, domXML, snapXML string) *scdFixture {
	t.Helper()
	fx := newSCDFixture(t, map[string]string{v6Domain: domXML})
	fx.script("single", "snaps-"+v6Domain, "s1\n")
	fx.script("single", "snapxml-s1", snapXML)
	return fx
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

func TestBuildSnapshotCreateArgs_ExcludesReadOnlyDisks(t *testing.T) {
	const vm, name, desc = "vm-1", "snap-1", "d"
	base := []string{"snapshot-create-as", vm, name, "--description", desc, "--atomic"}
	for _, tc := range []struct {
		name     string
		memory   bool
		excluded []string
		want     []string
	}{
		{"no read-only disk: no diskspec", false, nil, append(append([]string{}, base...), "--disk-only")},
		{"the seed CD-ROM", false, []string{"hda"},
			append(append([]string{}, base...), "--disk-only", "--diskspec", "hda,snapshot=no")},
		{"several read-only disks", false, []string{"hda", "sdb", "fda"},
			append(append([]string{}, base...), "--disk-only",
				"--diskspec", "hda,snapshot=no", "--diskspec", "sdb,snapshot=no", "--diskspec", "fda,snapshot=no")},
		{"a memory snapshot excludes them too", true, []string{"hda"},
			append(append([]string{}, base...), "--diskspec", "hda,snapshot=no")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, mem := buildSnapshotCreateArgs(vm, name, desc, tc.memory, true, tc.excluded)
			assert.Equal(t, tc.memory, mem)
			assert.Equal(t, tc.want, args)
		})
	}
}

func TestSnapshotExcludedTargets(t *testing.T) {
	readOnlyData := "    <disk type='file' device='disk'><driver type='raw'/><source file='/srv/ref.img'/>" +
		"<target dev='sdb' bus='scsi'/><readonly/></disk>\n"
	floppy := "    <disk type='file' device='floppy'><source file='/srv/f.img'/><target dev='fda'/></disk>\n"
	emptyCD := "    <disk type='file' device='cdrom'><target dev='hdc' bus='ide'/><readonly/></disk>\n"
	writable := "    <disk type='file' device='disk'><driver type='qcow2'/><source file='/srv/data.qcow2'/><target dev='vdb'/></disk>\n"

	for _, tc := range []struct {
		name string
		xml  string
		want []string
	}{
		{"no read-only disk", snapOverlayDomainXML("web", overlayDomainUUID, "/p/web-disk.qcow2"), nil},
		{"the seed CD-ROM", v6DomainXML(v6SeedISO), []string{"hda"}},
		{"CD-ROMs, a floppy and a read-only disk, never a writable one",
			v6DomainXML(v6SeedISO, readOnlyData, writable, floppy, emptyCD), []string{"hda", "sdb", "fda", "hdc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := parseDomainDisks(tc.xml)
			require.NoError(t, err)
			got, err := doc.snapshotExcludedTargets()
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("a read-only disk whose target cannot be put in a diskspec", func(t *testing.T) {
		for _, bad := range []string{"", "hd,a", "-x", "HDA", "hd a"} {
			doc, err := parseDomainDisks(v6DomainXML(v6SeedISO,
				fmt.Sprintf("<disk type='file' device='cdrom'><source file='/srv/x.iso'/><target dev='%s'/></disk>", bad)))
			require.NoError(t, err)
			_, err = doc.snapshotExcludedTargets()
			assert.Error(t, err, "target %q", bad)
		}
	})
}

func TestExternalReadOnlyDisks(t *testing.T) {
	dom, err := parseDomainDisks(v6DomainXML(v6SeedISO))
	require.NoError(t, err)

	for _, tc := range []struct {
		name   string
		snap   string
		domain *domainDisksDoc
		want   []string
	}{
		{"a snapshot rc6 took records the CD-ROM", v6RC6SnapshotXML, dom, []string{"hda"}},
		{"a snapshot this release takes does not", v6SafeSnapshotXML, dom, nil},
		{"an internal snapshot has no external disk", `<domainsnapshot><name>s1</name></domainsnapshot>`, dom, nil},
		{"the CD-ROM known only from the definition the snapshot recorded",
			strings.Replace(v6RC6SnapshotXML, "</disks>", "</disks>"+v6DomainXML(v6SeedISO), 1),
			&domainDisksDoc{}, []string{"hda"}},
		{"a disk named by its path",
			strings.Replace(v6RC6SnapshotXML, "name='hda'", "name='"+v6SeedISO+"'", 1), dom, []string{v6SeedISO}},
		{"revert disks are checked too",
			`<domainsnapshot><name>s1</name><disks><disk name='vda' snapshot='external'/></disks>` +
				`<revertDisks><disk name='hda' snapshot='external'/></revertDisks></domainsnapshot>`, dom, []string{"hda"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := parseSnapshotDiskModes(tc.snap)
			require.NoError(t, err)
			got := s.externalReadOnlyDisks(tc.domain)
			if len(tc.want) == 0 {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestSingleHost_SnapshotCreate_ExcludesReadOnlyDisks: the create reads the
// domain and passes one --diskspec <target>,snapshot=no per read-only disk —
// none for a VM without one — and a read-only disk that cannot be excluded
// safely means no snapshot is taken.
func TestSingleHost_SnapshotCreate_ExcludesReadOnlyDisks(t *testing.T) {
	ctx := context.Background()
	create := func(t *testing.T, fx *scdFixture) ([]string, error) {
		t.Helper()
		_, err := NewServer(fx.p).SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{VmId: v6Domain, NameHint: "s2", Description: "d"})
		var creates []string
		for _, c := range fx.calls() {
			if strings.HasPrefix(c, "single virsh snapshot-create-as ") {
				creates = append(creates, c)
			}
		}
		return creates, err
	}
	const prefix = "single virsh snapshot-create-as " + v6Domain + " s2 --description d --atomic --disk-only"

	t.Run("the seed CD-ROM", func(t *testing.T) {
		got, err := create(t, v6Fixture(t, v6DomainXML(v6SeedISO), v6SafeSnapshotXML))
		require.NoError(t, err)
		assert.Equal(t, []string{prefix + " --diskspec hda,snapshot=no"}, got)
	})
	t.Run("no read-only disk", func(t *testing.T) {
		got, err := create(t, v6Fixture(t, snapOverlayDomainXML(v6Domain, v6UUID, v6Disk), v6SafeSnapshotXML))
		require.NoError(t, err)
		assert.Equal(t, []string{prefix}, got)
	})
	t.Run("several read-only disks", func(t *testing.T) {
		got, err := create(t, v6Fixture(t, v6DomainXML(v6SeedISO,
			"<disk type='file' device='cdrom'><source file='/srv/tools.iso'/><target dev='hdc'/><readonly/></disk>",
			"<disk type='file' device='disk'><source file='/srv/ref.img'/><target dev='vdb'/><readonly/></disk>"), v6SafeSnapshotXML))
		require.NoError(t, err)
		assert.Equal(t, []string{prefix + " --diskspec hda,snapshot=no --diskspec hdc,snapshot=no --diskspec vdb,snapshot=no"}, got)
	})
	t.Run("a read-only disk that cannot be excluded", func(t *testing.T) {
		got, err := create(t, v6Fixture(t, v6DomainXML(v6SeedISO,
			"<disk type='file' device='cdrom'><source file='/srv/x.iso'/><target dev='h,x'/></disk>"), v6SafeSnapshotXML))
		require.Error(t, err)
		assert.Empty(t, got, "no snapshot is taken")
		st, ok := status.FromError(err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, codes.Unavailable, st.Code(), "answered as not performed")
		assert.True(t, errorInfoReasons(st)[contracts.VMOperationFailedReason], "answered as not performed")
	})
}

// requireRevertRefused asserts the V6 refusal: FailedPrecondition, naming the
// CD-ROM's target and what to do, no host path, and no snapshot-revert sent.
func requireRevertRefused(t *testing.T, err error, calls []string, revertPrefix string) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "a gRPC status: %v", err)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	for _, want := range []string{"revert refused", "earlier release", "read-only disk", "unable to start", "Nothing changed",
		"Take a new snapshot; delete this VMSnapshot"} {
		assert.Contains(t, st.Message(), want)
	}
	assert.NotContains(t, st.Message(), "/", "no host path crosses the wire")
	// The manager keeps 256 bytes of the answer ("snapshotRevert failed: "
	// + the message) in the VM's status: the guidance must fit.
	assert.LessOrEqual(t, len("snapshotRevert failed: "+st.Message()), 256, "fits the status detail: %q", st.Message())
	assert.False(t, called(calls, revertPrefix), "no snapshot-revert is sent: %v", calls)
}

func TestSingleHost_SnapshotRevert_RefusesASnapshotRecordingTheSeed(t *testing.T) {
	ctx := context.Background()
	fx := v6Fixture(t, v6DomainXML(v6SeedISO), v6RC6SnapshotXML)
	_, err := NewServer(fx.p).SnapshotRevert(ctx, &providerv1.SnapshotRevertRequest{VmId: v6Domain, SnapshotId: "s1"})
	requireRevertRefused(t, err, fx.calls(), "single virsh snapshot-revert")
	st, _ := status.FromError(err)
	assert.Contains(t, st.Message(), "hda")
	assert.Contains(t, st.Message(), `"s1"`)

	// The contracts.Provider path refuses it the same way.
	_, err = fx.p.SnapshotRevert(ctx, contracts.VMRef{ID: v6Domain}, "s1")
	var unsafe *snapshotRevertUnsafeError
	require.True(t, errors.As(err, &unsafe), "%v", err)
	assert.Equal(t, []string{"hda"}, unsafe.disks)
	assert.False(t, called(fx.calls(), "single virsh snapshot-revert"))
}

func TestSingleHost_SnapshotRevert_AcceptsASnapshotWithoutTheSeed(t *testing.T) {
	fx := v6Fixture(t, v6DomainXML(v6SeedISO), v6SafeSnapshotXML)
	_, err := NewServer(fx.p).SnapshotRevert(context.Background(), &providerv1.SnapshotRevertRequest{VmId: v6Domain, SnapshotId: "s1"})
	require.NoError(t, err)
	assert.Contains(t, fx.calls(), "single virsh snapshot-revert "+v6Domain+" s1 --force")
}

// TestSingleHost_SnapshotRevert_RewrittenSeedIsAFailure: a revert after which
// the CD-ROM is on a file neither the domain nor the snapshot had for it is
// answered as a failure (reverted, the VM may not start), never a success; a
// CD-ROM put back on the source the snapshot recorded is a success.
func TestSingleHost_SnapshotRevert_RewrittenSeedIsAFailure(t *testing.T) {
	ctx := context.Background()

	fx := v6Fixture(t, v6DomainXML(v6SeedISO), v6SafeSnapshotXML)
	fx.script("single", "revert-rewrites", strings.Replace(v6DomainXML(v6SeedOverlay), "type='raw'", "type='qcow2'", 1))
	_, err := NewServer(fx.p).SnapshotRevert(ctx, &providerv1.SnapshotRevertRequest{VmId: v6Domain, SnapshotId: "s1"})
	require.Error(t, err)
	assert.True(t, called(fx.calls(), "single virsh snapshot-revert"), "the revert was sent")
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), `reverted to snapshot "s1", but libvirt moved read-only disk hda`)
	assert.NotContains(t, st.Message(), "/", "no host path crosses the wire")
	assert.LessOrEqual(t, len("snapshotRevert failed: "+st.Message()), 256, "fits the status detail")

	// The snapshot recorded another seed (e.g. the VM's seed changed since):
	// the revert restoring it is not a rewrite.
	recorded := "/var/lib/libvirt/virtrigaud-seeds/virtrigaud-cloudinit-ns.vm.Y/cloud-init.iso"
	fx = v6Fixture(t, v6DomainXML(v6SeedISO), strings.Replace(v6SafeSnapshotXML, "</disks>", "</disks>"+v6DomainXML(recorded), 1))
	fx.script("single", "revert-rewrites", v6DomainXML(recorded))
	_, err = NewServer(fx.p).SnapshotRevert(ctx, &providerv1.SnapshotRevertRequest{VmId: v6Domain, SnapshotId: "s1"})
	require.NoError(t, err)
}

// TestSingleHost_SnapshotRevert_UnreadableSnapshotIsNotReverted: a snapshot
// definition that cannot be read means the revert is not made (fail closed).
func TestSingleHost_SnapshotRevert_UnreadableSnapshotIsNotReverted(t *testing.T) {
	for name, setup := range map[string]func(fx *scdFixture){
		"the read fails":     func(fx *scdFixture) { fx.script("single", "fail-snapshot-dumpxml", "") },
		"it does not parse":  func(fx *scdFixture) { fx.script("single", "snapxml-s1", "<domainsnapshot><disks>") },
		"the domain is gone": func(fx *scdFixture) { fx.script("single", "fail-dumpxml", "") },
	} {
		t.Run(name, func(t *testing.T) {
			fx := v6Fixture(t, v6DomainXML(v6SeedISO), v6SafeSnapshotXML)
			setup(fx)
			_, err := NewServer(fx.p).SnapshotRevert(context.Background(), &providerv1.SnapshotRevertRequest{VmId: v6Domain, SnapshotId: "s1"})
			require.Error(t, err)
			assert.False(t, called(fx.calls(), "single virsh snapshot-revert"))
		})
	}
}

// TestClustered_SnapshotRevert_RefusesASnapshotRecordingTheSeed: the routed,
// owner-checked revert refuses the same snapshot on the leased host —
// FailedPrecondition with VM_OPERATION_FAILED — and sends no revert; one
// without the seed is reverted.
func TestClustered_SnapshotRevert_RefusesASnapshotRecordingTheSeed(t *testing.T) {
	uuid := routingDomainUUID
	ctx := context.Background()
	rc6 := strings.ReplaceAll(v6RC6SnapshotXML, "name='hda'", "name='sda'") // scdDomainXML's CD-ROM is sda

	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	fx.script("host-b", "snapxml-s2", rc6)
	_, err := NewServer(fx.p).SnapshotRevert(ctx, &providerv1.SnapshotRevertRequest{
		VmId: "web", SnapshotId: "s2", TargetHostId: "host-b", Owner: teamAOwner,
	})
	requireRevertRefused(t, err, fx.calls(), "host-b virsh snapshot-revert")
	st, _ := status.FromError(err)
	assert.True(t, errorInfoReasons(st)[contracts.VMOperationFailedReason], "a per-VM failure on its host")
	assert.Contains(t, st.Message(), "sda")
	assert.Contains(t, fx.calls(), "host-b virsh snapshot-dumpxml "+uuid+" s2", "read by the owner-checked UUID")

	fx = ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	fx.script("host-b", "snapxml-s2", strings.ReplaceAll(v6SafeSnapshotXML, "name='hda'", "name='sda'"))
	_, err = NewServer(fx.p).SnapshotRevert(ctx, &providerv1.SnapshotRevertRequest{
		VmId: "web", SnapshotId: "s2", TargetHostId: "host-b", Owner: teamAOwner,
	})
	require.NoError(t, err)
	assert.Contains(t, fx.calls(), "host-b virsh snapshot-revert "+uuid+" s2 --force")
}

// TestClustered_SnapshotCreate_ExcludesTheSeed: the routed create excludes
// the read-only CD-ROM it reads on the leased host by the owner-checked UUID.
func TestClustered_SnapshotCreate_ExcludesTheSeed(t *testing.T) {
	uuid := routingDomainUUID
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	_, err := NewServer(fx.p).SnapshotCreate(context.Background(), &providerv1.SnapshotCreateRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "pre", Description: "d",
	})
	require.NoError(t, err)
	assert.Contains(t, fx.calls(), "host-b virsh snapshot-create-as "+uuid+" pre --description d --atomic --disk-only --diskspec sda,snapshot=no")
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
		o := filepath.Join(fixtureImagesDir, "web-disk.1792831471")
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
		c.script("snapxml-s2", externalSnapshotXML("s2", s2, filepath.Join(fixtureImagesDir, "web-disk.1792831471")))
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
			reverted := c.file(c.images, "team-a.web-disk.1792831471")
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
