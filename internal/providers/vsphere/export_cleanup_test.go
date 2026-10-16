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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// The temporary clone ExportDisk makes next to the source disk is a full copy of
// the tenant's disk (a descriptor plus a "-flat.vmdk" extent). Every exit of
// ExportDisk must remove ALL of it: deleting only the descriptor left the
// 12 GiB flat extent behind on a real VMFS datastore.

// exportSource creates a VM from the simulator's template and returns its id,
// the datastore path of its disk and that disk's folder ("[ds] dir").
func exportSource(t *testing.T, p *Provider) (vmID, diskPath, folder string) {
	t.Helper()
	resp, err := p.Create(context.Background(), templateCreateRequest("exp-src", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)
	disks := vmDiskFiles(t, p, resp.Id)
	require.NotEmpty(t, disks)
	var dp object.DatastorePath
	require.True(t, dp.FromString(disks[0]))
	return resp.Id, disks[0], "[" + dp.Datastore + "] " + path.Dir(dp.Path)
}

// folderFiles lists the file names in the datastore folder "[ds] dir".
func folderFiles(t *testing.T, p *Provider, folder string) []string {
	t.Helper()
	var dp object.DatastorePath
	require.True(t, dp.FromString(folder))
	ctx := context.Background()
	ds, err := p.finder.Datastore(ctx, dp.Datastore)
	require.NoError(t, err)
	names, err := listDatastoreFolder(ctx, ds, dp.Path)
	require.NoError(t, err)
	return names
}

// exportLeftovers returns the names in folder that belong to an export clone.
func exportLeftovers(t *testing.T, p *Provider, folder string) []string {
	t.Helper()
	var left []string
	for _, n := range folderFiles(t, p, folder) {
		if strings.HasPrefix(n, exportTempDiskPrefix) {
			left = append(left, n)
		}
	}
	return left
}

// cloneForExport clones diskPath to a new temporary export disk in folder and
// returns its datastore path and file name.
func cloneForExport(t *testing.T, p *Provider, diskPath, folder, vmID string) (clonePath, cloneName string) {
	t.Helper()
	name, err := newExportTempDiskName(vmID, time.Now())
	require.NoError(t, err)
	clonePath = folder + "/" + name
	require.NoError(t, p.cloneDiskToStreamOptimized(context.Background(), diskPath, clonePath))
	return clonePath, name
}

func TestNewExportTempDiskName(t *testing.T) {
	now := time.Unix(1792182688, 0)
	a, err := newExportTempDiskName("vm-8040", now)
	require.NoError(t, err)
	b, err := newExportTempDiskName("vm-8040", now)
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(a, "virtrigaud-export-vm-8040-1792182688-"), a)
	assert.True(t, strings.HasSuffix(a, ".vmdk"), a)
	assert.NotEqual(t, a, b, "two exports of one VM in the same second never share a clone name")
	assert.Regexp(t, exportCloneExtentPattern(a), strings.TrimSuffix(a, ".vmdk")+"-flat.vmdk")
}

func TestExportCloneExtentPattern(t *testing.T) {
	re := exportCloneExtentPattern("virtrigaud-export-vm-1-100-ab.vmdk")
	tests := []struct {
		name  string
		match bool
	}{
		{"virtrigaud-export-vm-1-100-ab-flat.vmdk", true},
		{"virtrigaud-export-vm-1-100-ab-s001.vmdk", true},
		{"virtrigaud-export-vm-1-100-ab-s012.vmdk", true},
		{"virtrigaud-export-vm-1-100-ab-sesparse.vmdk", true},
		{"virtrigaud-export-vm-1-100-ab.vmdk", false},          // the descriptor is handled separately
		{"virtrigaud-export-vm-1-100-abc-flat.vmdk", false},    // longer base name
		{"virtrigaud-export-vm-1-100-ab-flat.vmdk.bak", false}, // suffix
		{"virtrigaud-export-vm-1-100-ab-flat.vmdkx", false},
		{"xvirtrigaud-export-vm-1-100-ab-flat.vmdk", false}, // prefix
		{"virtrigaud-export-vm-1-100-ab-s1.vmdk", false},    // not a numbered extent
		{"virtrigaud-export-vm-1-100-ab-.vmdk", false},
		{"vm-flat.vmdk", false},
		{"virtrigaud-export-vm-1-100-ab-flat.vmdk/../x", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.match, re.MatchString(tc.name))
		})
	}
	// The base name is quoted: a "." in it is not a wildcard.
	dotted := exportCloneExtentPattern("virtrigaud-export-vm.1.vmdk")
	assert.True(t, dotted.MatchString("virtrigaud-export-vm.1-flat.vmdk"))
	assert.False(t, dotted.MatchString("virtrigaud-export-vmX1-flat.vmdk"))
}

// The simulator, like VMFS, stores a copied disk as a descriptor plus a flat
// extent file: the precondition that makes the folder assertions meaningful.
func TestCloneForExport_CreatesDescriptorAndExtent(t *testing.T) {
	p, _ := newOwnershipSim(t)
	vmID, disk, folder := exportSource(t, p)

	_, name := cloneForExport(t, p, disk, folder, vmID)

	files := folderFiles(t, p, folder)
	assert.Contains(t, files, name)
	assert.Contains(t, files, strings.TrimSuffix(name, ".vmdk")+"-flat.vmdk")
}

func TestDeleteExportClone_RemovesDescriptorAndExtent(t *testing.T) {
	p, _ := newOwnershipSim(t)
	vmID, disk, folder := exportSource(t, p)
	before := folderFiles(t, p, folder)
	clonePath, _ := cloneForExport(t, p, disk, folder, vmID)

	require.NoError(t, p.deleteExportClone(context.Background(), clonePath))

	assert.Empty(t, exportLeftovers(t, p, folder))
	assert.ElementsMatch(t, before, folderFiles(t, p, folder), "the source VM's own files are untouched")
}

// The vCenter failure mode behind this fix: the descriptor is gone but the flat
// extent remains. DeleteVirtualDisk reports FileNotFound; the fallback deletes
// the extent by its exact name and nothing else.
func TestDeleteExportClone_DescriptorGoneStillRemovesExtent(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	vmID, disk, folder := exportSource(t, p)
	clonePath, name := cloneForExport(t, p, disk, folder, vmID)
	base := strings.TrimSuffix(name, ".vmdk")

	// Decoys that share the clone's prefix but are NOT its files.
	decoys := []string{
		base + "X-flat.vmdk",       // longer base name
		base + "-flat.vmdk.bak",    // suffixed
		base + "-notes.txt",        // other extension
		exportTempDiskPrefix + "z", // another export's file
	}
	for _, d := range decoys {
		makeFolderFile(t, p, folder+"/"+d)
	}
	// Remove only the descriptor, as the old datastore-file-manager delete did
	// for the descriptor-only case.
	require.NoError(t, NewDatastoreFileManager(p).DeleteFile(ctx, clonePath))
	require.Contains(t, folderFiles(t, p, folder), base+"-flat.vmdk")

	require.NoError(t, p.deleteExportClone(ctx, clonePath))

	files := folderFiles(t, p, folder)
	assert.NotContains(t, files, base+"-flat.vmdk", "the orphaned extent must be deleted")
	assert.NotContains(t, files, name)
	for _, d := range decoys {
		assert.Contains(t, files, d, "only files matching the temp name exactly may be deleted")
	}
	assert.Contains(t, files, path.Base(disk), "the source disk is untouched")
}

func TestDeleteExportClone_NothingToDelete(t *testing.T) {
	p, _ := newOwnershipSim(t)
	_, _, folder := exportSource(t, p)
	name, err := newExportTempDiskName("vm-1", time.Now())
	require.NoError(t, err)

	assert.NoError(t, p.deleteExportClone(context.Background(), folder+"/"+name), "never created / already gone is clean")
	assert.NoError(t, p.deleteExportClone(context.Background(), "[LocalDS_0] no-such-folder/"+name), "a missing folder is clean")
}

func TestDeleteExportClone_RefusesNonExportDisk(t *testing.T) {
	p, _ := newOwnershipSim(t)
	_, disk, folder := exportSource(t, p)

	err := p.deleteExportClone(context.Background(), disk)

	require.Error(t, err)
	assert.Contains(t, folderFiles(t, p, folder), path.Base(disk), "a tenant disk is never deleted by the export cleanup")
}

// makeFolderFile creates an empty file at the datastore path (a decoy).
func makeFolderFile(t *testing.T, p *Provider, dsPath string) {
	t.Helper()
	var dp object.DatastorePath
	require.True(t, dp.FromString(dsPath))
	ds, err := p.finder.Datastore(context.Background(), dp.Datastore)
	require.NoError(t, err)
	require.NoError(t, ds.Upload(context.Background(), strings.NewReader("x"), dp.Path, nil))
}

// failCopyAfterRunning lets CopyVirtualDisk_Task run (so vCenter creates the
// clone's files) and then reports the call as failed, like a transport error
// after the task started.
type failCopyAfterRunning struct {
	soap.RoundTripper
}

func (r *failCopyAfterRunning) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if err := r.RoundTripper.RoundTrip(ctx, req, res); err != nil {
		return err
	}
	if _, ok := req.(*methods.CopyVirtualDisk_TaskBody); ok {
		return errors.New("fixture: connection lost after the copy started")
	}
	return nil
}

// failDatastoreHTTP fails the datastore file transfer (HTTP /folder/ URLs) and
// lets every other request through.
type failDatastoreHTTP struct {
	http.RoundTripper
}

func (r *failDatastoreHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasPrefix(req.URL.Path, "/folder/") {
		return nil, errors.New("fixture: datastore download failed")
	}
	return r.RoundTripper.RoundTrip(req)
}

func TestExportDisk_CloneFailsAfterCreatingFiles_NothingLeaks(t *testing.T) {
	p, _ := newOwnershipSim(t)
	vmID, _, folder := exportSource(t, p)
	p.client.RoundTripper = &failCopyAfterRunning{RoundTripper: p.client.RoundTripper}

	_, err := p.ExportDisk(context.Background(), s3ExportRequest(t, vmID, "http://127.0.0.1:1"))

	require.Error(t, err)
	assert.Empty(t, exportLeftovers(t, p, folder), "a partial clone must be removed")
}

func TestExportDisk_DownloadFailure_NothingLeaks(t *testing.T) {
	p, _ := newOwnershipSim(t)
	vmID, _, folder := exportSource(t, p)
	p.client.Transport = &failDatastoreHTTP{RoundTripper: p.client.Transport}

	_, err := p.ExportDisk(context.Background(), s3ExportRequest(t, vmID, "http://127.0.0.1:1"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "download", "the download is what failed")
	assert.Empty(t, exportLeftovers(t, p, folder), "the clone must be removed after a failed download")
}

func TestExportDisk_Success_NothingLeaks(t *testing.T) {
	// vcsim stores the clone as a descriptor plus extent, which the success
	// path flattens with qemu-img before uploading.
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	p, _ := newOwnershipSim(t)
	vmID, _, folder := exportSource(t, p)
	before := folderFiles(t, p, folder)
	s3 := fakeS3(t)

	resp, err := p.ExportDisk(context.Background(), s3ExportRequest(t, vmID, s3.URL))

	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.NotEmpty(t, resp.ExportId)
	assert.Empty(t, exportLeftovers(t, p, folder), "the clone's descriptor AND extent must be removed after a successful export")
	assert.ElementsMatch(t, before, folderFiles(t, p, folder), "the VM folder is as it was before the export")
}

// s3ExportRequest is an S3-relay ExportDisk request for vmID against the S3
// endpoint.
func s3ExportRequest(t *testing.T, vmID, endpoint string) *providerv1.ExportDiskRequest {
	t.Helper()
	return &providerv1.ExportDiskRequest{
		VmId:               vmID,
		BackendType:        "s3",
		TransferMode:       "relay",
		DestinationUrl:     "s3://bucket/exp/disk.vmdk",
		StorageOptionsJson: `{"backend":"s3","bucket":"bucket","endpoint":"` + endpoint + `","region":"us-east-1","usePathStyle":true}`,
		Credentials:        map[string]string{"accessKeyID": "AK", "secretAccessKey": "SK"},
	}
}

// fakeS3 is an S3 endpoint that accepts every PutObject.
func fakeS3(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}
