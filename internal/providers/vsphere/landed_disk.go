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
	stderrors "errors"
	"fmt"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	sdkerrors "github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Landed-disk delete (rc2 R7).
//
// A migration whose import landed a disk on this provider — the landing folder
// "[<ds>] <ns>_<vm>-migrated" holding "<ns>_<vm>-migrated.vmdk" — and that then
// failed for good, or was deleted, before a target VM attached the disk, asks
// this provider to remove it (DeleteLandedDisk). The folder is derived from the
// request's target VM with landingName, exactly as ImportDisk names it; the
// recorded landing path, when sent, only chooses the datastore and must be
// exactly that VM's landing disk. Under the folder's landing lock, the folder
// is removed only when — checked right before the delete, without the scan
// cache — no VM or template references any file in it (ensureImportFolderUnused,
// which fails closed). Nothing outside that one folder is ever removed.
//
// Limits: the scan sees the VMs and templates of this vCenter only, so the
// landing datastore must not be shared with hypervisors or vCenters this
// Provider does not manage; and the landing lock is per provider process, so
// run one replica of a vSphere provider (the controller does: Recreate, one
// replica).

const (
	// landedDiskRequestInvalidMessage refuses a request without a valid
	// target VM identity.
	landedDiskRequestInvalidMessage = "DeleteLandedDisk: the target VirtualMachine's namespace and name do not name a " +
		"vSphere landing disk"
	// landedDiskPathMismatchMessage refuses a recorded path that is not the
	// target VM's landing disk. It never echoes the path.
	landedDiskPathMismatchMessage = "DeleteLandedDisk: the recorded landing path is not the target VirtualMachine's " +
		"landing disk ([<datastore>] <namespace>_<vm name>-migrated/<namespace>_<vm name>-migrated.vmdk)"
	// landedDiskNoDatastoreMessage refuses a request without a recorded path
	// when the provider's default datastore is not a plain datastore (unset,
	// or a datastore cluster whose member the import chose by free space).
	landedDiskNoDatastoreMessage = "DeleteLandedDisk: without the recorded landing path the landing datastore cannot " +
		"be determined (the provider's default datastore is not a plain datastore)"
	// landedDiskFailedMessage is the retryable answer of a lookup or delete
	// that failed; the vCenter fault goes to the provider log only.
	landedDiskFailedMessage = "DeleteLandedDisk: the landing folder could not be checked or removed; the cause is in " +
		"the provider log; will retry"
)

// DeleteLandedDisk removes the landing folder of the disk a migration's
// ImportDisk landed for req.target_vm (see above). A folder that does not
// exist is NOT_FOUND; one a VM uses is refused with VM_DISK_IN_USE and kept.
func (p *Provider) DeleteLandedDisk(ctx context.Context, req *providerv1.DeleteLandedDiskRequest) (*providerv1.DeleteLandedDiskResponse, error) {
	if p.client == nil {
		return nil, sdkerrors.NewUnavailable("vSphere client not configured", nil)
	}
	tv := req.GetTargetVm()
	if tv.GetNamespace() == "" || tv.GetName() == "" || validLandingIdentity(tv.GetNamespace(), tv.GetName()) != nil {
		return nil, sdkerrors.NewInvalidSpec("%s", landedDiskRequestInvalidMessage)
	}
	ns, name := tv.GetNamespace(), tv.GetName()
	id := landingName(ns, name)
	migrationRef := req.GetMigration().GetNamespace() + "/" + req.GetMigration().GetName()

	dsName, err := p.landedDiskDatastoreName(ctx, req.GetPath(), ns, name)
	if err != nil {
		return nil, err
	}

	unlock, err := p.landingLocks.lock(ctx, landingLockKey(dsName, id))
	if err != nil {
		return nil, sdkerrors.NewInternal("wait for other work on the landing folder", err)
	}
	defer unlock()
	defer p.diskScans.invalidate(diskScanKey(dsName))

	exists, err := p.landingFolderExists(ctx, dsName, id)
	if err != nil {
		return nil, p.errLandedDisk("look up the landing folder", err)
	}
	if !exists {
		p.logger.Info("Migration landed disk already gone", "migration", migrationRef, "datastore", dsName, "folder", id)
		return &providerv1.DeleteLandedDiskResponse{Outcome: providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_NOT_FOUND}, nil
	}
	// Fresh scan (no cache), right before the delete: no VM or template may
	// reference any file in the folder. Fails closed.
	if err := p.ensureImportFolderUnused(ctx, dsName, id, true); err != nil {
		p.logger.Warn("Not deleting a migration's landed disk: it is used, or could not be shown unused",
			"migration", migrationRef, "datastore", dsName, "folder", id, "reason", status.Convert(err).Message())
		return nil, err
	}
	if err := NewDatastoreFileManager(p).DeleteFile(ctx, fmt.Sprintf("[%s] %s", dsName, id)); err != nil {
		return nil, p.errLandedDisk("delete the landing folder", err)
	}
	p.logger.Info("Migration landed disk deleted", "migration", migrationRef, "datastore", dsName, "folder", id)
	return &providerv1.DeleteLandedDiskResponse{Outcome: providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_DELETED}, nil
}

// landedDiskDatastoreName is the datastore of the landed disk of ns/name: the
// datastore of recordedPath, which must be exactly that VM's landing disk
// (checkInPlaceDiskPath's byte-for-byte rule), or — when no path was recorded —
// the provider's DefaultDatastore, which must then be a plain datastore.
func (p *Provider) landedDiskDatastoreName(ctx context.Context, recordedPath, ns, name string) (string, error) {
	if recordedPath != "" {
		ds, err := checkInPlaceDiskPath(recordedPath, contracts.ObjectIdentity{Namespace: ns, Name: name}, name, true)
		if err != nil {
			return "", sdkerrors.NewInvalidSpec("%s", landedDiskPathMismatchMessage)
		}
		return ds, nil
	}
	dsName := p.config.DefaultDatastore
	if dsName == "" {
		return "", sdkerrors.NewInvalidSpec("%s", landedDiskNoDatastoreMessage)
	}
	if _, err := p.datastoreByName(ctx, dsName); err != nil {
		var nf *find.NotFoundError
		if stderrors.As(err, &nf) {
			return "", sdkerrors.NewInvalidSpec("%s", landedDiskNoDatastoreMessage)
		}
		return "", p.errLandedDisk("find the default datastore", err)
	}
	return dsName, nil
}

// datastoreByName finds the plain datastore dsName in the default datacenter.
func (p *Provider) datastoreByName(ctx context.Context, dsName string) (*object.Datastore, error) {
	finder, _, err := p.dcFinder(ctx)
	if err != nil {
		return nil, fmt.Errorf("find datacenter: %w", err)
	}
	ds, err := finder.Datastore(ctx, dsName)
	p.noteInventoryError(err)
	return ds, err
}

// landingFolderExists reports whether the landing folder "[<dsName>] <id>"
// exists.
func (p *Provider) landingFolderExists(ctx context.Context, dsName, id string) (bool, error) {
	ds, err := p.datastoreByName(ctx, dsName)
	if err != nil {
		return false, fmt.Errorf("find datastore %q: %w", dsName, err)
	}
	if _, err := ds.Stat(ctx, id); err != nil {
		var noFile object.DatastoreNoSuchFileError
		var noDir object.DatastoreNoSuchDirectoryError
		if stderrors.As(err, &noFile) || stderrors.As(err, &noDir) {
			return false, nil
		}
		return false, fmt.Errorf("stat landing folder: %w", err)
	}
	return true, nil
}

// errLandedDisk logs a failed landed-disk step and returns the constant,
// retryable error the manager sees.
func (p *Provider) errLandedDisk(step string, err error) error {
	p.logger.Error("Migration landed disk delete step failed", "step", step, "error", err)
	return status.Error(codes.Unavailable, landedDiskFailedMessage)
}
