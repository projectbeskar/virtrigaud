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

package vsphere

import (
	"context"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/object"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// landedReq is a DeleteLandedDisk request for namespace/vmName with the
// recorded landing path path.
func landedReq(namespace, vmName, path string) *providerv1.DeleteLandedDiskRequest {
	return &providerv1.DeleteLandedDiskRequest{
		Migration: &providerv1.ObjectIdentity{Uid: "m-uid", Namespace: namespace, Name: "mig"},
		TargetVm:  &providerv1.ObjectIdentity{Namespace: namespace, Name: vmName},
		Path:      path,
	}
}

// TestDeleteLandedDisk_RemovesAnUnusedLandingFolder: an unused landing folder
// is removed (with or without the recorded path); a second call is NOT_FOUND;
// another namespace's landing for the same VM name is untouched.
func TestDeleteLandedDisk_RemovesAnUnusedLandingFolder(t *testing.T) {
	for name, withPath := range map[string]bool{"recorded path": true, "default datastore": false} {
		t.Run(name, func(t *testing.T) {
			p, _ := newOwnershipSim(t)
			ctx := context.Background()
			makeLandingDisk(t, p, ownerTeamA.Namespace, "web")
			makeLandingDisk(t, p, ownerTeamB.Namespace, "web")
			path := ""
			if withPath {
				path = landingPathIn(ownerTeamA.Namespace, "web")
			}

			resp, err := p.DeleteLandedDisk(ctx, landedReq(ownerTeamA.Namespace, "web", path))
			require.NoError(t, err)
			assert.Equal(t, providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_DELETED, resp.GetOutcome())
			assert.False(t, datastoreFileExists(t, p, landingPathIn(ownerTeamA.Namespace, "web")))
			assert.False(t, datastoreFileExists(t, p, "["+simDatastore+"] "+landingName(ownerTeamA.Namespace, "web")),
				"the whole landing folder is removed")
			assert.True(t, datastoreFileExists(t, p, landingPathIn(ownerTeamB.Namespace, "web")),
				"another namespace's landing disk is never touched")

			resp, err = p.DeleteLandedDisk(ctx, landedReq(ownerTeamA.Namespace, "web", path))
			require.NoError(t, err)
			assert.Equal(t, providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_NOT_FOUND, resp.GetOutcome())
		})
	}
}

// TestDeleteLandedDisk_KeepsADiskAVMUses: a landing disk attached to a VM (the
// migration's target VM, created on it) is refused with VM_DISK_IN_USE and
// kept.
func TestDeleteLandedDisk_KeepsADiskAVMUses(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	_, err := p.Create(ctx, &providerv1.CreateRequest{Name: "web", ImageJson: makeOwnMigratedDisk(t, p, "web"), Owner: wireOwner(ownerTeamA)})
	require.NoError(t, err)

	_, err = p.DeleteLandedDisk(ctx, landedReq(ownerTeamA.Namespace, "web", ownMigratedDiskPath("web")))
	requireCode(t, err, codes.FailedPrecondition)
	assert.Equal(t, []string{contracts.VMDiskInUseReason}, errorReasons(err))
	assert.True(t, datastoreFileExists(t, p, ownMigratedDiskPath("web")), "a disk in use is never deleted")
}

// TestDeleteLandedDisk_KeepsAFolderAVMOrTemplateHasFilesIn (security review):
// a VM or a template whose own (non-disk) files are in the landing folder —
// its .vmx, not the landed disk — makes the delete refuse the whole folder.
func TestDeleteLandedDisk_KeepsAFolderAVMOrTemplateHasFilesIn(t *testing.T) {
	for name, template := range map[string]bool{"VM": false, "template": true} {
		t.Run(name, func(t *testing.T) {
			p, _ := newOwnershipSim(t)
			ctx := context.Background()
			id := landingName(ownerTeamA.Namespace, "web")
			plantVM(t, p, defaultVMFolder(t, p), id, nil, template) // homed in "[ds] <id>/"
			makeImportedDisk(t, p, "["+simDatastore+"] "+id, ownMigratedDiskPath("web"))

			_, err := p.DeleteLandedDisk(ctx, landedReq(ownerTeamA.Namespace, "web", ownMigratedDiskPath("web")))
			requireCode(t, err, codes.FailedPrecondition)
			assert.Equal(t, []string{contracts.VMDiskInUseReason}, errorReasons(err))
			assert.True(t, datastoreFileExists(t, p, ownMigratedDiskPath("web")))
			assert.True(t, datastoreFileExists(t, p, "["+simDatastore+"] "+id+"/"+id+".vmx"))
		})
	}
}

// TestDeleteLandedDisk_ScanFailureFailsClosed: when the inventory scan fails
// nothing is deleted and the answer is retryable.
func TestDeleteLandedDisk_ScanFailureFailsClosed(t *testing.T) {
	p, _ := newOwnershipSim(t)
	makeOwnMigratedDisk(t, p, "web")
	p.client.RoundTripper = &failVMInventoryView{RoundTripper: p.client.RoundTripper, root: p.client.ServiceContent.RootFolder}

	_, err := p.DeleteLandedDisk(context.Background(), landedReq(ownerTeamA.Namespace, "web", ""))
	requireCode(t, err, codes.Unavailable)
	assert.True(t, datastoreFileExists(t, p, ownMigratedDiskPath("web")))
}

// TestDeleteLandedDisk_RefusesAnythingButTheTargetsLanding: a path that is not
// the target VM's landing disk (another VM's, another namespace's, the legacy
// bare name, any other file) and an invalid identity are InvalidArgument;
// nothing is deleted and the message never echoes the path.
func TestDeleteLandedDisk_RefusesAnythingButTheTargetsLanding(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	makeLandingDisk(t, p, ownerTeamA.Namespace, "db")
	makeLandingDisk(t, p, ownerTeamB.Namespace, "web")

	for name, req := range map[string]*providerv1.DeleteLandedDiskRequest{
		"another VM's landing":       landedReq(ownerTeamA.Namespace, "web", landingPathIn(ownerTeamA.Namespace, "db")),
		"another namespace":          landedReq(ownerTeamA.Namespace, "web", landingPathIn(ownerTeamB.Namespace, "web")),
		"legacy bare name":           landedReq(ownerTeamA.Namespace, "web", "["+simDatastore+"] "+legacyLandingRelPath("web")),
		"any other file":             landedReq(ownerTeamA.Namespace, "web", "["+simDatastore+"] etc/passwd"),
		"traversal in the identity":  landedReq(ownerTeamA.Namespace, "../db", ""),
		"no target VM":               {Path: landingPathIn(ownerTeamA.Namespace, "db")},
		"namespace with a separator": landedReq("team_a", "web", ""),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.DeleteLandedDisk(ctx, req)
			requireCode(t, err, codes.InvalidArgument)
			if req.GetPath() != "" {
				assert.NotContains(t, status.Convert(err).Message(), req.GetPath())
			}
		})
	}
	assert.True(t, datastoreFileExists(t, p, landingPathIn(ownerTeamA.Namespace, "db")))
	assert.True(t, datastoreFileExists(t, p, landingPathIn(ownerTeamB.Namespace, "web")))
}

// TestDeleteLandedDisk_DefaultDatastoreMustBePlain: without a recorded path a
// default datastore that is not a plain datastore (a datastore cluster) is
// InvalidArgument: the import chose its member by free space.
func TestDeleteLandedDisk_DefaultDatastoreMustBePlain(t *testing.T) {
	p, _ := newOwnershipSim(t)
	p.config.DefaultDatastore = "SomeStoragePod"
	_, err := p.DeleteLandedDisk(context.Background(), landedReq(ownerTeamA.Namespace, "web", ""))
	requireCode(t, err, codes.InvalidArgument)
}

// TestCapabilities_LandedDiskDelete: the provider reports DeleteLandedDisk.
func TestCapabilities_LandedDiskDelete(t *testing.T) {
	p, _ := newOwnershipSim(t)
	caps, err := p.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsLandedDiskDelete())
}

// --- transient import VM stubs (Part B) ---------------------------------------

// TestImportVMStubNames: only the transient VM's own non-disk files directly
// in its landing folder are named, never a .vmdk, never another folder.
func TestImportVMStubNames(t *testing.T) {
	id := landingName("team-a", "web")
	got := importVMStubNames(simDatastore, id, []string{
		"[" + simDatastore + "] " + id + "/" + id + ".vmx",
		"[" + simDatastore + "] " + id + "/vmware.log",
		"[" + simDatastore + "] " + id + "/" + id + ".vmdk",
		"[" + simDatastore + "] " + id + "/" + id + "-flat.VMDK",
		"[" + simDatastore + "] other/other.vmx",
		"[" + simDatastore + "] " + id + "/sub/x.vmx",
		"[" + simDatastore + "] " + id + "/../other/x.vmx",
		"[OtherDS] " + id + "/" + id + ".nvram",
	})
	assert.ElementsMatch(t, []string{
		id + "/" + id + ".vmx", id + "/" + id + ".vmsd", id + "/" + id + ".hlog",
		id + "/" + id + ".nvram", id + "/" + id + ".vmxf", id + "/vmware.log",
	}, got)
}

// TestRemoveImportVMStubs_KeepsTheLandedDisk (vcsim): the transient import VM
// homed in the landing folder is unregistered; its own files are removed and
// the landed .vmdk stays.
func TestRemoveImportVMStubs_KeepsTheLandedDisk(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	id := landingName(ownerTeamA.Namespace, "web")
	ref := plantVM(t, p, defaultVMFolder(t, p), id, nil, false)
	vm := object.NewVirtualMachine(p.client.Client, ref)
	landed := ownMigratedDiskPath("web")
	makeImportedDisk(t, p, "["+simDatastore+"] "+id, landed)

	stubs := p.importVMStubFiles(ctx, vm, simDatastore, id)
	require.Contains(t, stubs, id+"/"+id+".vmx", "the transient VM's .vmx is a stub")
	require.True(t, datastoreFileExists(t, p, "["+simDatastore+"] "+id+"/"+id+".vmx"), "setup: vcsim wrote the .vmx")

	// vCenter's "<id>-<hex>.hlog" history log (rc2 R23) is in neither the
	// derived names nor the layout; vcsim does not write one, so plant it,
	// next to look-alikes that must stay.
	folder := "[" + simDatastore + "] " + id + "/"
	hlog := folder + id + "-3fc8ea8a.hlog"
	keep := []string{
		folder + id + ".hlog.bak",
		folder + "other-abc.hlog",
		folder + id + "-zz.hlog",
		folder + id + "-3fc8ea8a.hlog.bak",
		folder + id + "-3fc8ea8a.vmdk",
	}
	makeFolderFile(t, p, hlog)
	for _, f := range keep {
		makeFolderFile(t, p, f)
	}

	require.NoError(t, vm.Unregister(ctx))
	ds, err := p.datastoreByName(ctx, simDatastore)
	require.NoError(t, err)
	p.removeImportVMStubs(ctx, ds, testDatacenter(t, p), id, stubs)

	assert.False(t, datastoreFileExists(t, p, "["+simDatastore+"] "+id+"/"+id+".vmx"), "the stub .vmx is removed")
	assert.False(t, datastoreFileExists(t, p, hlog), "the history log is removed")
	assert.True(t, datastoreFileExists(t, p, landed), "the landed disk is never removed")
	for _, f := range keep {
		assert.True(t, datastoreFileExists(t, p, f), "an unrelated name is never removed: %s", f)
	}
}

// TestRemoveImportVMStubs_NoStubsRemovesNothing (vcsim): when the import VM was
// not verified (no stubs) the history log is not removed either.
func TestRemoveImportVMStubs_NoStubsRemovesNothing(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	id := landingName(ownerTeamA.Namespace, "web")
	hlog := "[" + simDatastore + "] " + id + "/" + id + "-3fc8ea8a.hlog"
	makeFolderFile(t, p, hlog)

	ds, err := p.datastoreByName(ctx, simDatastore)
	require.NoError(t, err)
	p.removeImportVMStubs(ctx, ds, testDatacenter(t, p), id, nil)
	assert.True(t, datastoreFileExists(t, p, hlog))
}

// TestIsImportVMHistoryLog: exactly `^<QuoteMeta(id)>-[0-9a-f]+\.hlog$`.
func TestIsImportVMHistoryLog(t *testing.T) {
	id := "mig-s3_vs2-back-migrated"
	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(id) + `-[0-9a-f]+\.hlog$`)
	for _, name := range []string{
		id + "-3fc8ea8a.hlog", id + "-0.hlog", id + "-abcdef0123456789.hlog",
		id + ".hlog", id + "-.hlog", id + "-zz.hlog", id + "-3FC8EA8A.hlog", id + "-3fc8ea8a.hlog.bak",
		id + "-3fc8ea8a.vmdk", id + "-3fc8ea8a-flat.vmdk", "other-abc.hlog", "x" + id + "-ab.hlog",
		id + "-ab/../x.hlog", id + "-ab.HLOG", id + "-a b.hlog", "",
	} {
		assert.Equal(t, pattern.MatchString(name), isImportVMHistoryLog(id, name), name)
	}
	// A dot in the id is literal (QuoteMeta), not "any character".
	assert.False(t, isImportVMHistoryLog("a.b", "axb-12.hlog"))
	assert.True(t, isImportVMHistoryLog("a.b", "a.b-12.hlog"))

	assert.Equal(t, []string{id + "/" + id + "-3fc8ea8a.hlog"},
		importVMHistoryLogNames(id, []string{id + ".vmdk", id + "-3fc8ea8a.hlog", id + ".hlog.bak", "other-abc.hlog"}))
}

// TestImportVMStubFiles_OnlyTheImportVM (vcsim): a VM whose .vmx is not
// "[<ds>] <id>/<id>.vmx" yields no stubs (fail-soft: nothing is removed).
func TestImportVMStubFiles_OnlyTheImportVM(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ref := plantVM(t, p, defaultVMFolder(t, p), "some-other-vm", nil, false)
	stubs := p.importVMStubFiles(context.Background(), object.NewVirtualMachine(p.client.Client, ref),
		simDatastore, landingName(ownerTeamA.Namespace, "web"))
	assert.Empty(t, stubs)
}
