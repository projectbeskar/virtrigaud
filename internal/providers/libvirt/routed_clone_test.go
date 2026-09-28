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
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the provider side of ADR-0007 Addendum A slice 3 for Clone:
// a clustered clone runs on the source VM's bound host only (target_host_id
// must equal source_host_id), reads the source only when the source VM owns
// it, is stamped with its TARGET VirtualMachine's identity (never the
// source's), never binds over a domain its target does not own, and gets its
// own cloud-init seed.

// cloneTarget is the clone's target VirtualMachine (it exists before a
// clustered clone, so it has a uid).
var cloneTarget = contracts.ObjectIdentity{UID: "7d6c5b4a-3f2e-4d1c-9b8a-0f1e2d3c4b5a", Namespace: "team-a", Name: "copy"}

// cloneTargetDomain is the domain the clone lands as.
const cloneTargetDomain = "team-a.copy"

// routedCloneReq is a routed clone of "web" on host-b into cloneTarget.
func routedCloneReq(mut ...func(*providerv1.CloneRequest)) *providerv1.CloneRequest {
	r := &providerv1.CloneRequest{
		SourceVmId: "web", SourceHostId: "host-b", TargetHostId: "host-b", SourceOwner: teamAOwner,
		TargetName: "copy", TargetVm: &providerv1.ObjectIdentity{Uid: cloneTarget.UID, Namespace: cloneTarget.Namespace, Name: cloneTarget.Name},
	}
	for _, m := range mut {
		m(r)
	}
	return r
}

// sourceWeb is host-b's source domain "web", owned by ownerTeamA, with a
// cloud-init seed.
func sourceWeb(t *testing.T, more map[string]string) *routedSCD {
	t.Helper()
	domains := map[string]string{"web": scdDomainXML("web", scdDomainOpts{owner: ownerTeamA})}
	for k, v := range more {
		domains[k] = v
	}
	return newRoutedSCD(t, map[string]map[string]string{"host-b": domains})
}

// assertNoCopy fails if the clone copied, wrote or defined anything.
func assertNoCopy(t *testing.T, calls []string) {
	t.Helper()
	for _, c := range calls {
		for _, verb := range []string{"qemu-img", "mktemp", " cp ", "define", "sudo"} {
			assert.NotContains(t, c, verb, "nothing may be copied or defined: %q", c)
		}
	}
}

// TestClustered_Clone_LinkedIsRefused: a clustered provider serves full clones
// only (v0.4.0). A linked clone is InvalidArgument before any host is touched.
func TestClustered_Clone_LinkedIsRefused(t *testing.T) {
	fx := sourceWeb(t, nil)
	_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq(func(r *providerv1.CloneRequest) { r.Linked = true }))
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
	assert.Contains(t, err.Error(), "linked clones are not supported on a clustered libvirt provider")
	assert.Empty(t, fx.calls(), "no host is touched")
}

func TestClustered_Clone_LandsOnTheSourceHostStampedWithItsTarget(t *testing.T) {
	t.Run("full", func(t *testing.T) {
		fx := sourceWeb(t, nil)
		resp, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		require.NoError(t, err)
		assert.Equal(t, cloneTargetDomain, resp.TargetVmId)
		assert.Nil(t, resp.Task, "libvirt clones are synchronous")

		calls := fx.calls()
		assert.Equal(t, []string{"host-b virsh list --all", "host-b virsh dumpxml web"}, calls[:2], "the source's owner check runs first")
		targetDisk := "/var/lib/libvirt/images/" + cloneTargetDomain + "-disk.qcow2"
		assert.Contains(t, calls, "local qemu-img convert -O qcow2 "+scdDiskPath+" "+targetDisk)
		assert.Contains(t, calls, "host-b virsh dumpxml "+routingDomainUUID, "the source is read by its checked UUID")
		for _, c := range calls {
			assert.False(t, strings.HasPrefix(c, "host-a "), "the clone never touches another host: %q", c)
			assert.NotContains(t, c, "vol-info", "nothing is looked up by volume name: %q", c)
		}
		assert.Zero(t, fx.p.virshProvider.unroutableHits.Load())

		defined := fx.definedXML(cloneTargetDomain)
		require.NotEmpty(t, defined, "the clone was defined")
		owners, err := domainOwners(strings.ReplaceAll(defined, "<staging>", fx.staging))
		require.NoError(t, err)
		assert.Equal(t, []contracts.ObjectIdentity{cloneTarget}, owners, "the clone carries its target VirtualMachine's stamp only")
		assert.NotContains(t, defined, ownerTeamA.UID, "never the source's stamp")
		assert.Contains(t, defined, "<name>"+cloneTargetDomain+"</name>")
		assert.Contains(t, defined, "file='"+targetDisk+"'")
	})
}

// TestClustered_Clone_GetsItsOwnCloudInitSeed: the clone's CD-ROM points at a
// copy of the source's seed in a per-clone seed directory (which the clone's
// Delete removes), so deleting either VM can never remove the other's seed.
func TestClustered_Clone_GetsItsOwnCloudInitSeed(t *testing.T) {
	fx := sourceWeb(t, nil)
	_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
	require.NoError(t, err)

	seedDir := "<staging>/" + cloudInitSeedDirPrefix + cloneTargetDomain + ".0000000000"
	iso := seedDir + "/" + cloudInitISOName
	calls := fx.calls()
	assert.Contains(t, calls, "local mktemp -d <staging>/"+cloudInitSeedDirPrefix+cloneTargetDomain+".XXXXXXXXXX")
	assert.Contains(t, calls, "local cp -- "+scdSeedISO+" "+iso)
	assert.Contains(t, calls, "local chmod "+cloudInitISOMode+" -- "+iso)
	assert.Contains(t, calls, "local chmod "+cloudInitSeedDirMode+" -- "+seedDir)

	defined := fx.definedXML(cloneTargetDomain)
	assert.Contains(t, defined, "file='"+iso+"'", "the clone's CD-ROM is its own seed")
	assert.NotContains(t, defined, scdSeedISO, "the source's seed is not shared")
	doc, err := parseDomainDisks(strings.ReplaceAll(defined, "<staging>", fx.staging))
	require.NoError(t, err)
	assert.Equal(t, strings.ReplaceAll(seedDir, "<staging>", fx.staging), doc.cloudInitSeedDir(fx.staging),
		"Delete of the clone finds (and removes) its own seed directory")
	for _, c := range calls {
		assert.NotContains(t, c, "rm -rf -- "+scdSeedISO[:strings.LastIndex(scdSeedISO, "/")], "the source's seed is never removed")
	}
}

// TestClustered_Clone_DefineFailureAndTheClonesSeed: when the define failed
// and the domain provably does not exist, the clone's own seed copy is removed;
// when that cannot be established the domain may exist and reference it, so it
// is kept (as Create does with its seed).
func TestClustered_Clone_DefineFailureAndTheClonesSeed(t *testing.T) {
	seedRemoval := "local rm -rf -- <staging>/" + cloudInitSeedDirPrefix + cloneTargetDomain + ".0000000000"
	for name, absent := range map[string]bool{"domain provably absent": true, "outcome unknown": false} {
		t.Run(name, func(t *testing.T) {
			fx := sourceWeb(t, nil)
			fx.script("local", "fail-define", "")
			if absent {
				answerDomuuidNoDomain(t, fx.dir)
			}
			_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
			st, ok := status.FromError(err)
			require.True(t, ok)
			assert.Equal(t, contracts.VMOperationFailedReason, errorInfoReason(st), "got %v", err)
			if absent {
				assert.Contains(t, fx.calls(), seedRemoval)
			} else {
				assert.NotContains(t, fx.calls(), seedRemoval)
			}
		})
	}
}

// answerDomuuidNoDomain puts a virsh in front of scdFakeTool that answers
// `domuuid` as libvirt does for a domain that does not exist, and hands every
// other call on.
func answerDomuuidNoDomain(t *testing.T, fakeDir string) {
	t.Helper()
	real, err := exec.LookPath("virsh")
	require.NoError(t, err)
	script := "#!/bin/sh\ncase \"$*\" in *domuuid*)\n" +
		"  printf 'local virsh %s\\n' \"$*\" >> \"" + fakeDir + "/calls.log\"\n" +
		"  echo \"error: failed to get domain 'x'\" >&2; exit 1 ;;\nesac\nexec " + real + " \"$@\"\n"
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "virsh"), []byte(script), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestClustered_Clone_SourceWithoutSeed(t *testing.T) {
	noSeed := strings.Replace(scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}), scdSeedISO, "/var/lib/libvirt/images/installer.iso", 1)
	fx := newRoutedSCD(t, map[string]map[string]string{"host-b": {"web": noSeed}})
	resp, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
	require.NoError(t, err)
	assert.Equal(t, cloneTargetDomain, resp.TargetVmId)
	for _, c := range fx.calls() {
		assert.NotContains(t, c, "mktemp -d", "no seed to copy")
	}
}

// TestClustered_Clone_SourceNotOwnedIsNotFoundAndNeverRead: the source is read
// (and its disk copied) only when the source VirtualMachine owns it.
func TestClustered_Clone_SourceNotOwnedIsNotFoundAndNeverRead(t *testing.T) {
	for name, owner := range map[string]*providerv1.ObjectIdentity{
		"another tenant": teamBOwner,
		"no owner":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			fx := sourceWeb(t, nil)
			_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq(func(r *providerv1.CloneRequest) { r.SourceOwner = owner }))
			assert.Equal(t, codes.NotFound, status.Code(err), "got %v", err)
			assert.Equal(t, []string{"host-b virsh list --all", "host-b virsh dumpxml web"}, fx.calls(), "only the ownership check ran")
		})
	}
}

// TestClustered_Clone_TargetNameTaken: a domain of the target name that the
// target VirtualMachine does not own is a Conflict (AlreadyExists) and nothing
// is copied; one it owns is an earlier attempt's clone and is reported as done.
func TestClustered_Clone_TargetNameTaken(t *testing.T) {
	t.Run("by another domain", func(t *testing.T) {
		fx := sourceWeb(t, map[string]string{cloneTargetDomain: strings.Replace(
			scdDomainXML(cloneTargetDomain, scdDomainOpts{owner: ownerTeamB}), routingDomainUUID, "22222222-3333-4444-8555-666666666666", 1)})
		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		assert.Equal(t, codes.AlreadyExists, status.Code(err), "got %v", err)
		assert.NotContains(t, err.Error(), ownerTeamB.UID, "the message never discloses the other owner")
		assertNoCopy(t, fx.calls())
	})
	t.Run("by the target's own earlier clone", func(t *testing.T) {
		fx := sourceWeb(t, map[string]string{cloneTargetDomain: strings.Replace(
			scdDomainXML(cloneTargetDomain, scdDomainOpts{owner: cloneTarget}), routingDomainUUID, "22222222-3333-4444-8555-666666666666", 1)})
		resp, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		require.NoError(t, err)
		assert.Equal(t, cloneTargetDomain, resp.TargetVmId, "a retried clone is an idempotent success")
		assertNoCopy(t, fx.calls())
	})
}

func TestClustered_Clone_RequestRefusals(t *testing.T) {
	cases := map[string]struct {
		mut  func(*providerv1.CloneRequest)
		code codes.Code
	}{
		"no source host":         {func(r *providerv1.CloneRequest) { r.SourceHostId = "" }, codes.InvalidArgument},
		"no landing host":        {func(r *providerv1.CloneRequest) { r.TargetHostId = "" }, codes.InvalidArgument},
		"cross-host clone":       {func(r *providerv1.CloneRequest) { r.TargetHostId = "host-a" }, codes.InvalidArgument},
		"target VM without uid":  {func(r *providerv1.CloneRequest) { r.TargetVm.Uid = "" }, codes.InvalidArgument},
		"no target VM":           {func(r *providerv1.CloneRequest) { r.TargetVm = nil }, codes.InvalidArgument},
		"target name mismatch":   {func(r *providerv1.CloneRequest) { r.TargetName = "other" }, codes.InvalidArgument},
		"unknown host (retried)": {func(r *providerv1.CloneRequest) { r.SourceHostId, r.TargetHostId = "host-zzz", "host-zzz" }, codes.Unavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fx := sourceWeb(t, nil)
			_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq(tc.mut))
			st, ok := status.FromError(err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, tc.code, st.Code(), "got %v", err)
			if tc.code == codes.Unavailable {
				assert.True(t, hasHostUnavailableInfo(st), "an unknown host is host-scoped")
			}
			assert.Empty(t, fx.calls(), "a refused clone never reaches a host")
			assert.Zero(t, fx.p.virshProvider.unroutableHits.Load())
		})
	}
}

func TestStampOwnerMetadata(t *testing.T) {
	base := "<domain type='kvm'>\n  <name>d</name>\n  <uuid>" + routingDomainUUID + "</uuid>\n</domain>\n"
	withMeta := "<domain type='kvm'>\n  <name>d</name>\n  <metadata>\n    <x:y xmlns:x='urn:other'/>\n  </metadata>\n</domain>\n"
	selfClosing := "<domain type='kvm'><name>d</name><metadata/></domain>"
	for name, doc := range map[string]string{"no metadata": base, "foreign metadata kept": withMeta, "self-closing metadata": selfClosing} {
		t.Run(name, func(t *testing.T) {
			out, err := stampOwnerMetadata(doc, cloneTarget)
			require.NoError(t, err)
			owners, err := domainOwners(out)
			require.NoError(t, err)
			assert.Equal(t, []contracts.ObjectIdentity{cloneTarget}, owners)
			if doc == withMeta {
				assert.Contains(t, out, "<x:y xmlns:x='urn:other'/>", "other tools' metadata is kept")
			}
			_, err = parseDomainLibvirtxml(out)
			require.NoError(t, err)
		})
	}

	_, err := stampOwnerMetadata(scdDomainXML("d", scdDomainOpts{owner: ownerTeamA}), cloneTarget)
	assert.Error(t, err, "a document that already carries a stamp is never stamped twice")
	_, err = stampOwnerMetadata(base, contracts.ObjectIdentity{Namespace: "ns", Name: "n"})
	assert.Error(t, err, "an owner without a uid is never stamped")
}

// TestClustered_Clone_DefinedFromThePersistentDefinitionWithoutBackingChain:
// the clone is defined from the source's --inactive definition, and the
// source disk's <backingStore> chain is not copied (the clone's disk is a
// standalone full copy).
func TestClustered_Clone_DefinedFromThePersistentDefinitionWithoutBackingChain(t *testing.T) {
	src := scdDomainXML("web", scdDomainOpts{owner: ownerTeamA})
	chain := "\n      <backingStore type='file'>\n        <format type='qcow2'/>\n        <source file='/var/lib/libvirt/images/base.qcow2'/>\n" +
		"        <backingStore/>\n      </backingStore>"
	src = strings.Replace(src, "<source file='"+scdDiskPath+"'/>", "<source file='"+scdDiskPath+"'/>"+chain, 1)
	fx := newRoutedSCD(t, map[string]map[string]string{"host-b": {"web": src}})

	_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
	require.NoError(t, err)
	assert.Contains(t, fx.calls(), "host-b virsh dumpxml "+routingDomainUUID+" --inactive", "the persistent definition is cloned")
	defined := fx.definedXML(cloneTargetDomain)
	require.NotEmpty(t, defined)
	assert.NotContains(t, defined, "backingStore", "the source's backing chain is not copied")
	assert.NotContains(t, defined, "base.qcow2")
	assert.Contains(t, defined, "file='/var/lib/libvirt/images/"+cloneTargetDomain+"-disk.qcow2'")
}

func TestStripBackingStores(t *testing.T) {
	doc := "<domain><devices>\n<disk><source file='a'/><backingStore type='file'><source file='b'/><backingStore type='file'>" +
		"<source file='c'/><backingStore/></backingStore></backingStore><target dev='vda'/></disk>\n" +
		"<disk><source file='d'/><backingStore/><target dev='vdb'/></disk>\n</devices></domain>"
	out, err := stripBackingStores(doc)
	require.NoError(t, err)
	assert.Equal(t, "<domain><devices>\n<disk><source file='a'/><target dev='vda'/></disk>\n"+
		"<disk><source file='d'/><target dev='vdb'/></disk>\n</devices></domain>", out, "every other byte is kept")

	same, err := stripBackingStores(scdDomainXML("web", scdDomainOpts{}))
	require.NoError(t, err)
	assert.Equal(t, scdDomainXML("web", scdDomainOpts{}), same, "a document without a chain is unchanged")

	// Only /domain/devices/disk/backingStore, without a namespace: another
	// tool's <metadata> (namespaced or not) and any other path are kept.
	elsewhere := "<domain><metadata><x:backingStore xmlns:x='urn:tool'><x:source file='m'/></x:backingStore>" +
		"<backingStore>kept</backingStore></metadata><devices>" +
		"<disk><source file='a'/><backingStore type='file'><source file='b'/></backingStore>" +
		"<y:backingStore xmlns:y='urn:other'/></disk>" +
		"<controller><backingStore/></controller></devices></domain>"
	out, err = stripBackingStores(elsewhere)
	require.NoError(t, err)
	assert.Equal(t, "<domain><metadata><x:backingStore xmlns:x='urn:tool'><x:source file='m'/></x:backingStore>"+
		"<backingStore>kept</backingStore></metadata><devices>"+
		"<disk><source file='a'/><y:backingStore xmlns:y='urn:other'/></disk>"+
		"<controller><backingStore/></controller></devices></domain>", out)

	_, err = stripBackingStores("<domain><disk>")
	assert.Error(t, err)
}

func TestCheckCloneableSource(t *testing.T) {
	disk := func(device, extra string) string {
		return "<disk type='file' device='" + device + "'><source file='/p/" + device + ".img'>" + extra + "</source></disk>"
	}
	doc := func(disks ...string) string {
		return "<domain><name>d</name><devices>" + strings.Join(disks, "") + "</devices></domain>"
	}
	for name, tc := range map[string]struct {
		xml     string
		refused string
	}{
		"one disk and the seed CD-ROM":      {doc(disk("disk", ""), disk("cdrom", "")), ""},
		"the routed fixture's source":       {scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}), ""},
		"one disk, device defaulted":        {"<domain><devices><disk type='file'><source file='/p/a'/></disk></devices></domain>", ""},
		"two file disks":                    {doc(disk("disk", ""), disk("disk", "")), "one disk; this one has 2"},
		"a file disk and a block disk":      {doc(disk("disk", ""), "<disk type='block' device='disk'><source dev='/dev/sdb'/></disk>"), "has 2"},
		"a file disk and a LUN":             {doc(disk("disk", ""), "<disk type='block' device='lun'><source dev='/dev/sdc'/></disk>"), "has 2"},
		"a file disk and a floppy image":    {doc(disk("disk", ""), disk("floppy", "")), "has 2 writable"},
		"one disk and two CD-ROMs":          {doc(disk("disk", ""), disk("cdrom", ""), disk("cdrom", "")), ""},
		"a disk with an external data file": {doc(disk("disk", "<dataStore type='file'><format type='raw'/><source file='/p/data.raw'/></dataStore>")), "dataStore"},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkCloneableSource(tc.xml)
			if tc.refused == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, contracts.IsInvalidSpec(err), "got %v", err)
			assert.Contains(t, err.Error(), tc.refused)
		})
	}
}

// TestClustered_Clone_MultiDiskSourceIsRefusedBeforeTheCopy: a source the
// clone could not copy whole (a second disk would be shared by source and
// clone) is InvalidArgument, and nothing is copied or defined.
func TestClustered_Clone_MultiDiskSourceIsRefusedBeforeTheCopy(t *testing.T) {
	src := scdDomainXML("web", scdDomainOpts{owner: ownerTeamA})
	second := "    <disk type='file' device='disk'>\n      <driver name='qemu' type='qcow2'/>\n" +
		"      <source file='/var/lib/libvirt/images/web-data.qcow2'/>\n      <target dev='vdb' bus='virtio'/>\n    </disk>\n"
	src = strings.Replace(src, "  <devices>\n", "  <devices>\n"+second, 1)
	fx := newRoutedSCD(t, map[string]map[string]string{"host-b": {"web": src}})

	_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
	assert.Contains(t, err.Error(), "supports a source VM with one disk")
	assertNoCopy(t, fx.calls())
}
