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
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	sdkerrors "github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// In-place disk confinement (security).
//
// A Create whose image carries a datastore Path attaches that file AS the new
// VM's disk, in place: no copy. The path reaches the provider from
// VirtualMachine.spec.importedDisk.path, which any tenant who may create a
// VirtualMachine can write. Unconfined, tenant B could name tenant A's disk
// ("[ds] a-vm/a-vm.vmdk"): B's VM would read and write A's data whenever A's VM
// is off, Create would grow it, and deleting B's VM (Destroy_Task) would delete
// it.
//
// Create therefore attaches a disk in place only when ALL of these hold
// (checkInPlaceDiskPath, then ensureDiskFileUnused), mirroring the libvirt
// provider (internal/providers/libvirt/imagepath.go):
//
//  1. contracts.VMImage.ImportedDisk is true: the operator verified the path as
//     the landing disk a VMMigration in the VM's own namespace imported for
//     this VM (isOwnMigrationDisk); the tenant cannot set it.
//  2. The path is exactly "[<ds>] <ns>.<vm>-migrated/<ns>.<vm>-migrated.vmdk",
//     <ns> and <vm> being the requesting VirtualMachine's namespace
//     (CreateRequest.owner, required) and name: the name ImportDisk gives the
//     disk it lands for that VirtualMachine (landingName, derived from
//     ImportDiskRequest.target_vm, never from the bare target_name). VM names
//     repeat across namespaces; the namespace in the landing name is what keeps
//     one tenant's landing disk out of another's reach. A disk landed under
//     the pre-v0.4.0 bare name ("<vm>-migrated") is refused with its own
//     message: that migration must be run again.
//  3. No VM or template in the vCenter inventory references the file, as a
//     disk, a snapshot's parent disk, or any file in its layout. The scan
//     fails closed: when it cannot read every VM's devices, nothing is
//     attached.
//
// Any other image Path is refused (InvalidArgument, non-retryable) — vSphere
// base images are templates, never datastore files. Refusals carry constant
// messages: never the caller's path, another VM's name, or a vCenter fault.
//
// The same inventory scan guards the migration import (an existing
// "[<ds>] <ns>.<vm>-migrated" folder is deleted or overwritten only when no VM
// uses a file in it, checked again right before a failed import's cleanup) and
// Delete (a disk outside the VM's own directory is detached,
// keeping its file, before Destroy_Task, unless it is the VM's own migrated
// disk and no other VM uses it; see detachForeignDisks).
//
// Work on one landing folder — an in-place attach, an import, its cleanup — is
// serialised inside the provider (Provider.landingLocks), so two such
// operations never both pass the check before either acts. The lock is per
// provider process: run one replica of a vSphere provider.

const (
	// vmdkExt is the extension of every disk file VirtRigaud places.
	vmdkExt = ".vmdk"
	// propConfigHardwareDevice lists a VM's virtual devices.
	propConfigHardwareDevice = "config.hardware.device"
	// propConfigFilesVMPathName is the datastore path of a VM's .vmx; its
	// directory is the VM's home directory.
	propConfigFilesVMPathName = "config.files.vmPathName"
	// propLayoutExFile lists every file of a VM, its snapshots' included.
	propLayoutExFile = "layoutEx.file"
	// propSnapshot is a VM's snapshot tree (nil without snapshots).
	propSnapshot = "snapshot"
)

// inPlaceDiskRejectedMessage is the only text a refused in-place disk path
// returns. It names the rule, never the caller's path.
const inPlaceDiskRejectedMessage = "imported disk rejected: the vSphere provider attaches a disk in place only when " +
	"it is the VirtualMachine's own migration landing disk ([<datastore>] <namespace>.<vm name>-migrated/" +
	"<namespace>.<vm name>-migrated.vmdk, imported by a VMMigration in the same namespace that targets this " +
	"VirtualMachine); use a VMImage (template or OVA) for any other image"

// legacyLandingMessage refuses a disk an earlier release landed under the
// bare "<vm name>-migrated" name, which does not tell namespaces apart.
const legacyLandingMessage = "imported disk rejected: it was landed by a release before v0.4.0 under " +
	"[<datastore>] <vm name>-migrated, a name that does not separate namespaces, so it cannot be proven to be this " +
	"VirtualMachine's disk; delete this VirtualMachine and its VMMigration and run the migration again (the old " +
	"landing folder is not removed automatically)"

// diskInUseMessage is the only text a refusal for a disk another VM uses
// returns. It never names that VM.
const diskInUseMessage = "imported disk rejected: another VM or template in the vCenter inventory uses this disk"

// diskCheckFailedMessage is the only text a failed inventory scan returns: the
// vCenter fault can name other VMs and their files, so it goes to the provider
// log only.
const diskCheckFailedMessage = "could not verify that no other VM uses the disk: the vCenter inventory scan failed " +
	"(details are in the provider log); will retry"

// foreignDiskSnapshotMessage is the refusal of a Delete that would have to
// detach a disk the VM does not own while the VM has snapshots: the snapshots
// still reference the disk, and Destroy_Task would delete it with them.
const foreignDiskSnapshotMessage = "delete refused: the VM has a disk it does not own (outside its own directory, or " +
	"used by another VM) and snapshots that reference it; deleting the VM would delete that disk. Remove the disk " +
	"from the VM in vCenter, or ask an administrator, then retry"

// statusWithReasons is a gRPC status with one VirtRigaud ErrorInfo per reason
// (contracts.ErrorInfoDomain), which the manager classifies on.
func statusWithReasons(code codes.Code, msg string, reasons ...string) *status.Status {
	st := status.New(code, msg)
	withInfo := st
	for _, r := range reasons {
		next, err := withInfo.WithDetails(&errdetails.ErrorInfo{Reason: r, Domain: contracts.ErrorInfoDomain})
		if err != nil {
			return st // unreachable in practice: ErrorInfo always marshals
		}
		withInfo = next
	}
	return withInfo
}

// errInPlaceDiskRejected is the refusal of an image Path the provider may not
// attach in place: InvalidArgument, which the manager maps to a
// non-retryable InvalidSpec.
func errInPlaceDiskRejected() error {
	return sdkerrors.NewInvalidSpec("%s", inPlaceDiskRejectedMessage)
}

// errDiskInUse is the refusal of a disk another VM uses: FailedPrecondition
// with VM_DISK_IN_USE, which the manager maps to a Conflict and keeps out of
// its circuit breaker.
func errDiskInUse(msg string) error {
	return statusWithReasons(codes.FailedPrecondition, msg, contracts.VMDiskInUseReason).Err()
}

// errDiskCheckFailed logs why the inventory scan failed and returns the
// constant, retryable error the caller sees: Unavailable with
// VM_DISK_CHECK_FAILED, kept out of the manager's circuit breaker.
func (p *Provider) errDiskCheckFailed(what string, err error) error {
	p.logger.Error("vCenter inventory disk scan failed", "operation", what, "error", err)
	return statusWithReasons(codes.Unavailable, diskCheckFailedMessage, contracts.VMDiskCheckFailedReason).Err()
}

// landingSeparator joins the namespace and the name in a landing name. A
// namespace is a DNS-1123 label (no dots), so the first dot splits the two
// unambiguously.
const landingSeparator = "."

// maxDatastoreFileNameBytes bounds one component of a datastore path (VMFS
// and NFS limit a file or directory name to 255 bytes).
const maxDatastoreFileNameBytes = 255

// landingName is the base name ImportDisk gives the disk it lands for the
// VirtualMachine namespace/name: "<ns>.<vm>-migrated". The folder and the
// .vmdk are both named after it.
func landingName(namespace, name string) string {
	return namespace + landingSeparator + name + contracts.ImportedDiskNameSuffix
}

// landingRelPath is the datastore-relative path of that landing disk:
// "<ns>.<vm>-migrated/<ns>.<vm>-migrated.vmdk".
func landingRelPath(namespace, name string) string {
	n := landingName(namespace, name)
	return n + "/" + n + vmdkExt
}

// legacyLandingRelPath is where a release before v0.4.0 landed the disk for a
// VM named name, whatever its namespace: "<vm>-migrated/<vm>-migrated.vmdk".
// Create refuses it (errLegacyLanding); Delete still recognises it as the
// VM's own disk when no other VM uses it.
func legacyLandingRelPath(name string) string {
	n := name + contracts.ImportedDiskNameSuffix
	return n + "/" + n + vmdkExt
}

// validLandingIdentity reports whether namespace/name can form a landing name:
// a DNS-1123 label namespace, a DNS-1123 subdomain name, and a name that fits
// one datastore path component.
func validLandingIdentity(namespace, name string) error {
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		return fmt.Errorf("namespace %q: %s", namespace, strings.Join(errs, "; "))
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return fmt.Errorf("name %q: %s", name, strings.Join(errs, "; "))
	}
	if len(landingName(namespace, name))+len(vmdkExt) > maxDatastoreFileNameBytes {
		return fmt.Errorf("landing name for %s/%s is longer than %d bytes", namespace, name, maxDatastoreFileNameBytes)
	}
	return nil
}

// importLandingName is the landing name of a migration import: landingName
// over req.target_vm. The bare target_name the manager also sends cannot tell
// namespaces apart and never decides the location; when set it must be the
// same VM's "<vm>-migrated". A request without target_vm (a manager older
// than the provider) is refused.
func importLandingName(req *providerv1.ImportDiskRequest) (string, error) {
	tv := req.GetTargetVm()
	if tv.GetNamespace() == "" || tv.GetName() == "" {
		return "", sdkerrors.NewInvalidSpec("%s", "vSphere disk import requires the target VirtualMachine's namespace "+
			"and name (ImportDiskRequest.target_vm); upgrade the manager")
	}
	if err := validLandingIdentity(tv.GetNamespace(), tv.GetName()); err != nil {
		return "", sdkerrors.NewInvalidSpec("import target cannot name a vSphere landing disk: %s", err.Error())
	}
	if req.GetTargetName() != "" && req.GetTargetName() != tv.GetName()+contracts.ImportedDiskNameSuffix {
		return "", sdkerrors.NewInvalidSpec("import target name %q does not match its target VM name %q",
			req.GetTargetName(), tv.GetName())
	}
	return landingName(tv.GetNamespace(), tv.GetName()), nil
}

// errLegacyLanding refuses a disk landed under the pre-v0.4.0 bare name.
func errLegacyLanding() error {
	return sdkerrors.NewInvalidSpec("%s", legacyLandingMessage)
}

// checkInPlaceDiskPath enforces rules 1 and 2 of the in-place confinement for
// the VM named vmName, requested by owner, and returns the datastore the disk
// is on. diskPath is accepted only when imported is set, owner carries a
// namespace (and, when set, owner.Name is vmName), and diskPath is
// byte-for-byte "[<ds>] <ns>.<vm>-migrated/<ns>.<vm>-migrated.vmdk" with a
// plain datastore name.
func checkInPlaceDiskPath(diskPath string, owner contracts.ObjectIdentity, vmName string, imported bool) (string, error) {
	if !imported || vmName == "" || owner.Namespace == "" || (owner.Name != "" && owner.Name != vmName) {
		return "", errInPlaceDiskRejected()
	}
	if validLandingIdentity(owner.Namespace, vmName) != nil || !strings.HasPrefix(diskPath, "[") {
		return "", errInPlaceDiskRejected()
	}
	end := strings.Index(diskPath, "]")
	if end < 2 {
		return "", errInPlaceDiskRejected()
	}
	ds := diskPath[1:end]
	if strings.TrimSpace(ds) != ds || strings.ContainsAny(ds, "[]/*?") || strings.ContainsFunc(ds, isControlRune) {
		return "", errInPlaceDiskRejected()
	}
	switch diskPath[end+1:] {
	case " " + landingRelPath(owner.Namespace, vmName):
		return ds, nil
	case " " + legacyLandingRelPath(vmName):
		return "", errLegacyLanding()
	default:
		return "", errInPlaceDiskRejected()
	}
}

// landingLockKey is the Provider.landingLocks key of the landing folder
// "[<ds>] <folder>" (case-folded: datastore names and paths may compare
// case-insensitively).
func landingLockKey(dsName, folder string) string {
	return strings.ToLower("[" + dsName + "] " + folder)
}

// landingLockKeyOfPath is landingLockKey for the folder of a datastore path.
func landingLockKeyOfPath(diskPath string) string {
	var dp object.DatastorePath
	dp.FromString(diskPath)
	return landingLockKey(dp.Datastore, path.Dir(dp.Path))
}

// importCleanupTimeout bounds a failed import's landing-folder cleanup, which
// runs on a fresh context (the request's may be done).
const importCleanupTimeout = 120 * time.Second

// cleanupFailedImport deletes a failed import's landing folder
// "[<dsName>] <id>" — but only after checking again, right before the delete,
// that no VM uses a file in it. The caller holds the folder's landing lock.
func (p *Provider) cleanupFailedImport(dsName, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), importCleanupTimeout)
	defer cancel()
	folderPath := fmt.Sprintf("[%s] %s", dsName, id)
	if err := p.ensureImportFolderUnused(ctx, dsName, id); err != nil {
		p.logger.Warn("Not deleting a failed import's landing folder: it could not be confirmed unused",
			"path", folderPath, "error", err)
		return
	}
	if err := NewDatastoreFileManager(p).DeleteFile(ctx, folderPath); err != nil {
		p.logger.Warn("Failed to delete partial import folder after failure (manual cleanup may be needed)",
			"path", folderPath, "error", err)
	}
}

// isControlRune reports C0 controls (including NUL), DEL and C1 controls.
func isControlRune(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r <= 0x9f)
}

// datastoreFile is a datastore path in comparable form: datastore name and
// cleaned relative path, both lower-cased so that a case variant of a file
// never escapes the in-use check (the check errs towards "in use").
type datastoreFile struct {
	datastore string
	path      string
}

// datastoreFileOf parses "[<ds>] <path>" into a datastoreFile; ok is false for
// anything else (e.g. an empty or "/vmfs/volumes/..." name).
func datastoreFileOf(s string) (datastoreFile, bool) {
	var dp object.DatastorePath
	if !dp.FromString(s) || dp.Datastore == "" {
		return datastoreFile{}, false
	}
	rel := strings.TrimPrefix(path.Clean("/"+dp.Path), "/")
	return datastoreFile{datastore: strings.ToLower(dp.Datastore), path: strings.ToLower(rel)}, true
}

// dir is the directory f is in ("" for the datastore root).
func (f datastoreFile) dir() datastoreFile {
	d := path.Dir(f.path)
	if d == "." || d == "/" {
		d = ""
	}
	return datastoreFile{datastore: f.datastore, path: d}
}

// inDir reports whether f lies (at any depth) inside directory d.
func (f datastoreFile) inDir(d datastoreFile) bool {
	if f.datastore != d.datastore {
		return false
	}
	if d.path == "" {
		return true
	}
	return strings.HasPrefix(f.path, d.path+"/")
}

// diskBackingChain returns the file names of a disk backing and of every
// parent in its chain (snapshot deltas and linked-clone bases).
func diskBackingChain(b types.BaseVirtualDeviceBackingInfo) []string {
	var files []string
	for b != nil {
		fb, ok := b.(types.BaseVirtualDeviceFileBackingInfo)
		if !ok {
			return files
		}
		files = append(files, fb.GetVirtualDeviceFileBackingInfo().FileName)
		switch t := b.(type) {
		case *types.VirtualDiskFlatVer2BackingInfo:
			b = backingOrNil(t.Parent)
		case *types.VirtualDiskSeSparseBackingInfo:
			b = backingOrNil(t.Parent)
		case *types.VirtualDiskSparseVer2BackingInfo:
			b = backingOrNil(t.Parent)
		case *types.VirtualDiskRawDiskMappingVer1BackingInfo:
			b = backingOrNil(t.Parent)
		case *types.VirtualDiskFlatVer1BackingInfo:
			b = backingOrNil(t.Parent)
		case *types.VirtualDiskSparseVer1BackingInfo:
			b = backingOrNil(t.Parent)
		default:
			b = nil
		}
	}
	return files
}

// backingOrNil returns parent as a backing interface, or a nil interface when
// the typed pointer is nil (a typed nil would not end the chain walk).
func backingOrNil[T types.BaseVirtualDeviceBackingInfo](parent T) types.BaseVirtualDeviceBackingInfo {
	var zero T
	if any(parent) == any(zero) {
		return nil
	}
	return parent
}

// vmFileRefs returns every datastore file vm references: the backing chain of
// each file-backed device, and every file in its layout (snapshot disks
// included).
func vmFileRefs(vm *mo.VirtualMachine) []datastoreFile {
	var names []string
	if vm.Config != nil {
		for _, dev := range vm.Config.Hardware.Device {
			names = append(names, diskBackingChain(dev.GetVirtualDevice().Backing)...)
		}
	}
	if vm.LayoutEx != nil {
		for _, f := range vm.LayoutEx.File {
			names = append(names, f.Name)
		}
	}
	refs := make([]datastoreFile, 0, len(names))
	for _, n := range names {
		if f, ok := datastoreFileOf(n); ok {
			refs = append(refs, f)
		}
	}
	return refs
}

// inventoryFileUsers maps each datastore file referenced by a VM or template
// anywhere in the vCenter inventory to the VMs that reference it.
type inventoryFileUsers map[datastoreFile][]types.ManagedObjectReference

// usedByOtherThan reports whether a VM other than self references f.
func (u inventoryFileUsers) usedByOtherThan(f datastoreFile, self types.ManagedObjectReference) bool {
	for _, ref := range u[f] {
		if ref != self {
			return true
		}
	}
	return false
}

// anyInDirUsedBy reports whether a VM other than self references a file
// inside directory d.
func (u inventoryFileUsers) anyInDirUsedBy(d datastoreFile, self types.ManagedObjectReference) bool {
	for f := range u {
		if f.inDir(d) && u.usedByOtherThan(f, self) {
			return true
		}
	}
	return false
}

// scanInventoryFileUsers reads every VirtualMachine (templates included) in
// the vCenter inventory, across all datacenters, and records which datastore
// files each references. It fails — and callers fail closed — when the view or
// any VM's properties cannot be read, or a VM's configuration is unavailable
// (an inaccessible or orphaned VM, whose disks cannot be known).
func (p *Provider) scanInventoryFileUsers(ctx context.Context) (inventoryFileUsers, error) {
	v, err := view.NewManager(p.client.Client).CreateContainerView(ctx, p.client.ServiceContent.RootFolder,
		[]string{virtualMachineMoType}, true)
	if err != nil {
		return nil, fmt.Errorf("create inventory view: %w", err)
	}
	defer func() { _ = v.Destroy(context.WithoutCancel(ctx)) }()

	var vms []mo.VirtualMachine
	if err := v.Retrieve(ctx, []string{virtualMachineMoType},
		[]string{propName, propConfigHardwareDevice, propLayoutExFile}, &vms); err != nil {
		return nil, fmt.Errorf("retrieve VM devices: %w", err)
	}
	users := inventoryFileUsers{}
	for i := range vms {
		vm := &vms[i]
		if vm.Config == nil {
			return nil, fmt.Errorf("VM %s (%q) has no readable configuration", vm.Self.Value, vm.Name)
		}
		for _, f := range vmFileRefs(vm) {
			users[f] = append(users[f], vm.Self)
		}
	}
	return users, nil
}

// ensureDiskFileUnused is rule 3 of the in-place confinement: diskPath must not
// be referenced by any VM or template in the inventory. Create calls it only
// after confirming no VM of the requested name exists, so the new VM is not
// among those scanned.
func (p *Provider) ensureDiskFileUnused(ctx context.Context, diskPath string) error {
	f, ok := datastoreFileOf(diskPath)
	if !ok {
		return errInPlaceDiskRejected()
	}
	users, err := p.scanInventoryFileUsers(ctx)
	if err != nil {
		return p.errDiskCheckFailed("in-place disk check", err)
	}
	if len(users[f]) > 0 {
		p.logger.Warn("Refusing to attach an imported disk in place: another VM references it",
			"disk_path", diskPath, "users", len(users[f]))
		return errDiskInUse(diskInUseMessage)
	}
	return nil
}

// ensureImportFolderUnused refuses (fail closed) a migration import whose
// landing folder "[<dsName>] <id>" holds a file some VM or template references.
// The import deletes or overwrites that folder's content, and its name derives
// from the target VirtualMachine's name only, so a same-named VM in another
// namespace could otherwise reach another tenant's migrated disk.
func (p *Provider) ensureImportFolderUnused(ctx context.Context, dsName, id string) error {
	d, ok := datastoreFileOf(fmt.Sprintf("[%s] %s/%s", dsName, id, id+vmdkExt))
	if !ok {
		return sdkerrors.NewInvalidSpec("invalid import target name")
	}
	users, err := p.scanInventoryFileUsers(ctx)
	if err != nil {
		return p.errDiskCheckFailed("import folder check", err)
	}
	if users.anyInDirUsedBy(d.dir(), types.ManagedObjectReference{}) {
		p.logger.Warn("Refusing to import a disk: a VM uses a file in the landing folder",
			"datastore", dsName, "folder", id)
		return errDiskInUse("disk import refused: a VM in the vCenter inventory uses a disk in the landing folder " +
			"for this VirtualMachine's name; delete or rename the VirtualMachine that holds it, or migrate to " +
			"another VirtualMachine name")
	}
	return nil
}

// detachForeignDisks runs before Delete's Destroy_Task, which deletes every
// disk attached to the VM wherever its file is. It detaches — keeping the
// file — every disk the VM does not own:
//
//   - a disk in the VM's home directory (the directory of its .vmx) is its own;
//   - a disk in a directory of the same name on another datastore, or the VM's
//     own migration landing disk ("<vm>-migrated/<vm>-migrated.vmdk"), is its
//     own unless another VM or template references the file (an inventory
//     scan, failing closed);
//   - any other disk is foreign.
//
// A VM created by this release never has a foreign disk (Create's in-place
// confinement); one created before it may. When a foreign disk must be
// detached while the VM has snapshots, the delete is refused
// (FailedPrecondition, VM_DISK_IN_USE): the snapshots keep referencing the
// file, and Destroy_Task would delete it with them. A VM whose configuration
// cannot be read keeps the historical behavior (Destroy_Task alone).
func (p *Provider) detachForeignDisks(ctx context.Context, vm *object.VirtualMachine) error {
	var vmMo mo.VirtualMachine
	if err := property.DefaultCollector(p.client.Client).RetrieveOne(ctx, vm.Reference(),
		[]string{propName, propConfigHardwareDevice, propConfigFilesVMPathName, propSnapshot, propConfigExtraConfig}, &vmMo); err != nil {
		return fmt.Errorf("read the disks of VM %s: %w", vm.Reference().Value, err)
	}
	if vmMo.Config == nil {
		p.logger.Warn("VM configuration unavailable; cannot check its disks before delete", "vm_id", vm.Reference().Value)
		return nil
	}
	home, ok := datastoreFileOf(vmMo.Config.Files.VmPathName)
	if !ok {
		p.logger.Warn("VM home path unreadable; cannot check its disks before delete", "vm_id", vm.Reference().Value)
		return nil
	}
	homeDir := home.dir()
	// The VM's own landing disk: under its namespaced name (the namespace from
	// its owner stamp) or, for a VM created before v0.4.0, the bare name.
	ownImported := []string{strings.ToLower(legacyLandingRelPath(vmMo.Name))}
	if owner, err := ownerFromExtraConfig(vmMo.Config.ExtraConfig); err == nil && owner.Namespace != "" {
		ownImported = append(ownImported, strings.ToLower(landingRelPath(owner.Namespace, vmMo.Name)))
	}

	var (
		users   inventoryFileUsers
		foreign []types.BaseVirtualDevice
	)
	for _, dev := range vmMo.Config.Hardware.Device {
		disk, isDisk := dev.(*types.VirtualDisk)
		if !isDisk {
			continue
		}
		fb, isFile := disk.Backing.(types.BaseVirtualDeviceFileBackingInfo)
		if !isFile {
			continue
		}
		name := fb.GetVirtualDeviceFileBackingInfo().FileName
		f, parsed := datastoreFileOf(name)
		if parsed && f.inDir(homeDir) {
			continue // the VM's own directory
		}
		mayOwn := parsed && homeDir.path != "" && (f.dir().path == homeDir.path || slices.Contains(ownImported, f.path))
		if mayOwn {
			if users == nil {
				var err error
				if users, err = p.scanInventoryFileUsers(ctx); err != nil {
					return p.errDiskCheckFailed("delete disk check", err)
				}
			}
			if !users.usedByOtherThan(f, vm.Reference()) {
				continue
			}
		}
		p.logger.Warn("Detaching a disk the VM does not own before delete; its file is kept",
			"vm_id", vm.Reference().Value, "disk_path", name)
		foreign = append(foreign, disk)
	}
	if len(foreign) == 0 {
		return nil
	}
	if vmMo.Snapshot != nil {
		return errDiskInUse(foreignDiskSnapshotMessage)
	}
	if err := vm.RemoveDevice(ctx, true, foreign...); err != nil {
		return fmt.Errorf("detach %d disk(s) VM %s does not own before delete: %w", len(foreign), vm.Reference().Value, err)
	}
	return nil
}
