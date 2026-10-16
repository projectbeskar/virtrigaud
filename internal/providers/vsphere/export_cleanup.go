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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
)

// exportTempDiskPrefix is the name prefix of the temporary full-disk clone
// ExportDisk makes next to the source disk. Operators looking for leftovers of
// an export that predates the cleanup fix search the VM folders of migration
// source datastores for files with this prefix.
const exportTempDiskPrefix = "virtrigaud-export-"

// exportCloneCleanupTimeout bounds the removal of the temporary export clone.
// It runs on a fresh context (the request's may be cancelled or expired) and
// has to cover deleting a multi-hundred-GiB thick extent on a busy datastore,
// which takes far longer than the tiny-file deletes the old 30s bound was
// sized for.
const exportCloneCleanupTimeout = 10 * time.Minute

// exportCloneAbortTimeout bounds waiting for a cancelled CopyVirtualDisk task
// to reach a terminal state, so the cleanup does not race a copy that is still
// writing.
const exportCloneAbortTimeout = 2 * time.Minute

// newExportTempDiskName returns a unique descriptor file name for the
// temporary export clone of VM vmID: "virtrigaud-export-<vmID>-<unix>-<rand>.vmdk".
// The random suffix guarantees two exports of the same VM in the same second
// never share a name, so one export's cleanup can never delete the other's
// clone.
func newExportTempDiskName(vmID string, now time.Time) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate export temp disk name: %w", err)
	}
	return fmt.Sprintf("%s%s-%d-%s.vmdk", exportTempDiskPrefix, vmID, now.Unix(), hex.EncodeToString(b[:])), nil
}

// exportCloneExtentPattern returns the matcher for the extent files of the
// temporary clone whose descriptor is named exactly descriptorName: its flat
// extent ("<base>-flat.vmdk"), numbered sparse extents ("<base>-s001.vmdk")
// and SE-sparse extent ("<base>-sesparse.vmdk"). The base name is quoted and
// the expression anchored, so a file of any other name (including one that
// merely starts with the same characters) never matches.
func exportCloneExtentPattern(descriptorName string) *regexp.Regexp {
	base := strings.TrimSuffix(descriptorName, ".vmdk")
	return regexp.MustCompile(`^` + regexp.QuoteMeta(base) + `-(flat|s[0-9]{3,}|sesparse)\.vmdk$`)
}

// deleteExportClone removes the temporary export clone whose descriptor is at
// the datastore path diskPath, together with all of its extents.
//
// The primary path is VirtualDiskManager.DeleteVirtualDisk, which removes the
// descriptor and every extent. Deleting only the descriptor with the datastore
// file manager (the previous behaviour) leaves the "-flat.vmdk" extent behind
// on VMFS, a full copy of the tenant's disk. If DeleteVirtualDisk fails (for
// example because the descriptor is already gone while an extent remains, or
// because a cancelled clone left only part of its files), the fallback deletes
// the files in the clone's folder whose names match the temporary name
// exactly. The function reports success when no file of the clone remains.
//
// ctx should be a fresh context bounded by exportCloneCleanupTimeout. The
// returned error names datastore objects only, never credentials.
func (p *Provider) deleteExportClone(ctx context.Context, diskPath string) error {
	if p.client == nil {
		return fmt.Errorf("delete temporary export disk: vSphere client not initialized")
	}
	dsName, filePath, err := parseDatastorePath(diskPath)
	if err != nil {
		return fmt.Errorf("delete temporary export disk: %w", err)
	}
	if !strings.HasPrefix(path.Base(filePath), exportTempDiskPrefix) {
		// Defence in depth: this function deletes disks; it never touches a
		// file that is not an export clone.
		return fmt.Errorf("delete temporary export disk %s: not a virtrigaud export clone", diskPath)
	}

	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return fmt.Errorf("delete temporary export disk %s: get datacenter: %w", diskPath, err)
	}

	task, vdmErr := object.NewVirtualDiskManager(p.client.Client).DeleteVirtualDisk(ctx, diskPath, datacenter)
	if vdmErr == nil {
		vdmErr = task.Wait(ctx)
	}
	if vdmErr == nil {
		return nil
	}
	p.logger.Info("DeleteVirtualDisk did not remove the temporary export disk, removing its files by exact name",
		"path", diskPath, "error", vdmErr)

	p.finder.SetDatacenter(datacenter)
	removed, ferr := p.removeExportCloneFiles(ctx, datacenter, dsName, filePath)
	if ferr != nil {
		return fmt.Errorf("delete temporary export disk %s: %w", diskPath, errors.Join(vdmErr, ferr))
	}
	if len(removed) == 0 {
		// Nothing of the clone exists: it was never created or is already gone.
		p.logger.Info("No temporary export disk files remain", "path", diskPath)
		return nil
	}
	p.logger.Info("Removed temporary export disk files by exact name", "path", diskPath, "files", removed)
	return nil
}

// removeExportCloneFiles deletes, with the datastore file manager, every file
// in the folder of descriptorPath whose name is exactly the descriptor's name
// or one of its extent names (see exportCloneExtentPattern). The extents are
// deleted before the descriptor so an interrupted run still leaves a
// descriptor that DeleteVirtualDisk can finish. It returns the datastore paths
// it deleted. A missing folder is not an error.
func (p *Provider) removeExportCloneFiles(ctx context.Context, datacenter *object.Datacenter, dsName, descriptorPath string) ([]string, error) {
	ds, err := p.finder.Datastore(ctx, dsName)
	if err != nil {
		return nil, fmt.Errorf("find datastore %s: %w", dsName, err)
	}
	dir := path.Dir(descriptorPath)
	descriptorName := path.Base(descriptorPath)
	extents := exportCloneExtentPattern(descriptorName)

	names, err := listDatastoreFolder(ctx, ds, dir)
	if err != nil {
		return nil, err
	}
	var victims []string
	descriptorPresent := false
	for _, name := range names {
		switch {
		case name == descriptorName:
			descriptorPresent = true
		case extents.MatchString(name):
			victims = append(victims, name)
		}
	}
	sort.Strings(victims)
	if descriptorPresent {
		victims = append(victims, descriptorName)
	}

	fileManager := object.NewFileManager(p.client.Client)
	var removed []string
	var errs []error
	for _, name := range victims {
		dsPath := ds.Path(path.Join(dir, name))
		delTask, derr := fileManager.DeleteDatastoreFile(ctx, dsPath, datacenter)
		if derr == nil {
			derr = delTask.Wait(ctx)
		}
		if derr != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", dsPath, derr))
			continue
		}
		removed = append(removed, dsPath)
	}
	return removed, errors.Join(errs...)
}

// listDatastoreFolder returns the names of the entries of the datastore folder
// dir ("" or "." for the datastore root). A folder that does not exist yields
// no entries and no error.
func listDatastoreFolder(ctx context.Context, ds *object.Datastore, dir string) ([]string, error) {
	browser, err := ds.Browser(ctx)
	if err != nil {
		return nil, fmt.Errorf("get datastore browser for %s: %w", ds.Name(), err)
	}
	dsPath := ds.Path(dir)
	task, err := browser.SearchDatastore(ctx, dsPath, &types.HostDatastoreBrowserSearchSpec{
		MatchPattern: []string{"*"},
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dsPath, err)
	}
	info, err := task.WaitForResult(ctx, nil)
	if err != nil {
		if types.IsFileNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s: %w", dsPath, err)
	}
	res, ok := info.Result.(types.HostDatastoreBrowserSearchResults)
	if !ok {
		return nil, fmt.Errorf("list %s: unexpected browser result %T", dsPath, info.Result)
	}
	names := make([]string, 0, len(res.File))
	for _, f := range res.File {
		if fi := f.GetFileInfo(); fi != nil {
			names = append(names, fi.Path)
		}
	}
	return names, nil
}

// cleanupExportClone removes the temporary export clone at diskPath on a fresh
// bounded context and logs the outcome (provider-side only; the error never
// reaches the caller of ExportDisk). It is safe to call when the clone was
// never created or only partially created.
func (p *Provider) cleanupExportClone(diskPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), exportCloneCleanupTimeout)
	defer cancel()
	if err := p.deleteExportClone(ctx, diskPath); err != nil {
		p.logger.Error("Failed to delete temporary export disk on the datastore; it is a full copy of the VM disk and must be deleted manually",
			"path", diskPath, "error", err)
		return
	}
	p.logger.Info("Cleaned up temporary export disk from datastore", "path", diskPath)
}
