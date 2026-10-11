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
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/vmware/govmomi/fault"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
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
//  2. The path is exactly "[<ds>] <ns>_<vm>-migrated/<ns>_<vm>-migrated.vmdk",
//     <ns> and <vm> being the requesting VirtualMachine's namespace
//     (CreateRequest.owner, required) and name: the name ImportDisk gives the
//     disk it lands for that VirtualMachine (landingName, derived from
//     ImportDiskRequest.target_vm, never from the bare target_name). VM names
//     repeat across namespaces; the namespace in the landing name is what keeps
//     one tenant's landing disk out of another's reach. A disk landed under
//     the pre-v0.4.0 bare name ("<vm>-migrated") is refused with its own
//     message: that migration must be run again.
//  3. No VM or template with files on the disk's datastore (or on the same
//     storage under another datastore name) uses the disk — as a disk, a
//     snapshot's or linked clone's parent, or a file in its layout — compared
//     by path and by disk UUID (disk_scan.go). The check fails closed: when
//     the scan fails, a VM there cannot be read or has an unidentifiable disk,
//     or the account lacks System.Read, nothing is attached.
//
// Any other image Path is refused (InvalidArgument, non-retryable) — vSphere
// base images are templates, never datastore files. Refusals carry constant
// messages: never the caller's path, another VM's name, or a vCenter fault.
//
// The same check guards the migration import (an existing
// "[<ds>] <ns>_<vm>-migrated" folder is deleted or overwritten only when no VM
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
	"it is the VirtualMachine's own migration landing disk ([<datastore>] <namespace>_<vm name>-migrated/" +
	"<namespace>_<vm name>-migrated.vmdk, imported by a VMMigration in the same namespace that targets this " +
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

// inventoryNotReadableMessage is the refusal when the provider's vCenter
// account cannot be shown to see every VM the check must read.
const inventoryNotReadableMessage = "could not verify that no other VM uses the disk: the provider's vCenter " +
	"account lacks the System.Read privilege on the vCenter root folder or on the disk's datastore, so VMs hidden " +
	"from it cannot be ruled out; grant it read-only access on the vCenter root, propagated (details are in the " +
	"provider log)"

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
	msg := diskCheckFailedMessage
	if errors.Is(err, errInventoryNotReadable) {
		msg = inventoryNotReadableMessage
	}
	return statusWithReasons(codes.Unavailable, msg, contracts.VMDiskCheckFailedReason).Err()
}

// landingSeparator joins the namespace and the name in a landing name. It is
// "_", which no Kubernetes namespace or object name can contain: the landing
// name splits unambiguously, and no VirtualMachine — which vSphere names, and
// homes in a directory named, after its bare name — can be named like a
// landing folder (a VM named "ns-a.web-migrated" in another namespace would
// squat "ns-a"'s landing if the separator were a dot). vCenter's own
// directory suffix ("<vm>_<digits>") never ends in "-migrated".
const landingSeparator = "_"

// maxDatastoreFileNameBytes bounds one component of a datastore path (VMFS
// and NFS limit a file or directory name to 255 bytes).
const maxDatastoreFileNameBytes = 255

// landingName is the base name ImportDisk gives the disk it lands for the
// VirtualMachine namespace/name: "<ns>_<vm>-migrated". The folder and the
// .vmdk are both named after it.
func landingName(namespace, name string) string {
	return namespace + landingSeparator + name + contracts.ImportedDiskNameSuffix
}

// landingRelPath is the datastore-relative path of that landing disk:
// "<ns>_<vm>-migrated/<ns>_<vm>-migrated.vmdk".
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
// byte-for-byte "[<ds>] <ns>_<vm>-migrated/<ns>_<vm>-migrated.vmdk" with a
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
	if err := p.ensureImportFolderUnused(ctx, dsName, id, true); err != nil {
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
// cleaned relative path. For "is it in use" questions both are lower-cased
// (datastoreFileOf), so a case variant of a file never escapes the check (it
// errs towards "in use"). For "is it this VM's own" questions they keep their
// case (exactDatastoreFileOf): VMFS is case-sensitive, and "WEB/" is not the
// home directory "web/".
type datastoreFile struct {
	datastore string
	path      string
}

// datastoreFileOf parses "[<ds>] <path>" into a case-folded datastoreFile, for
// in-use matching; ok is false for anything else (e.g. an empty or
// "/vmfs/volumes/..." name).
func datastoreFileOf(s string) (datastoreFile, bool) {
	return parseDatastoreFile(s, true)
}

// exactDatastoreFileOf is datastoreFileOf keeping the case, for ownership
// decisions.
func exactDatastoreFileOf(s string) (datastoreFile, bool) {
	return parseDatastoreFile(s, false)
}

// parseDatastoreFile parses "[<ds>] <path>", lower-casing both when fold.
func parseDatastoreFile(s string, fold bool) (datastoreFile, bool) {
	var dp object.DatastorePath
	if !dp.FromString(s) || dp.Datastore == "" {
		return datastoreFile{}, false
	}
	f := datastoreFile{datastore: dp.Datastore, path: strings.TrimPrefix(path.Clean("/"+dp.Path), "/")}
	if fold {
		f = datastoreFile{datastore: strings.ToLower(f.datastore), path: strings.ToLower(f.path)}
	}
	return f, true
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

// chainLink is one disk file of a backing chain: its datastore path as vCenter
// reports it, and its disk UUID (empty for backings that carry none).
type chainLink struct {
	file string
	uuid string
}

// diskBackingChain returns a disk backing and every parent in its chain
// (snapshot deltas and linked-clone bases).
func diskBackingChain(b types.BaseVirtualDeviceBackingInfo) []chainLink {
	var links []chainLink
	for b != nil {
		fb, ok := b.(types.BaseVirtualDeviceFileBackingInfo)
		if !ok {
			return links
		}
		link := chainLink{file: fb.GetVirtualDeviceFileBackingInfo().FileName}
		switch t := b.(type) {
		case *types.VirtualDiskFlatVer2BackingInfo:
			link.uuid, b = t.Uuid, backingOrNil(t.Parent)
		case *types.VirtualDiskSeSparseBackingInfo:
			link.uuid, b = t.Uuid, backingOrNil(t.Parent)
		case *types.VirtualDiskSparseVer2BackingInfo:
			link.uuid, b = t.Uuid, backingOrNil(t.Parent)
		case *types.VirtualDiskRawDiskMappingVer1BackingInfo:
			link.uuid, b = t.Uuid, backingOrNil(t.Parent)
		case *types.VirtualDiskFlatVer1BackingInfo:
			b = backingOrNil(t.Parent)
		case *types.VirtualDiskSparseVer1BackingInfo:
			b = backingOrNil(t.Parent)
		default:
			b = nil
		}
		links = append(links, link)
	}
	return links
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

// normalizeDiskUUID puts a disk UUID in comparable form. vCenter spells the
// same UUID "6000C29a-..." in a backing and "60 00 C2 9a ..." from
// QueryVirtualDiskUuid.
func normalizeDiskUUID(u string) string {
	return strings.NewReplacer(" ", "", "-", "").Replace(strings.ToLower(strings.TrimSpace(u)))
}

// inventoryFileUsers records which VMs (with files on a datastore,
// scanDatastoreFileUsers) use which disks. A path names a file through one
// datastore name and one directory spelling; the same disk can also be
// reached through another (an NFS export mounted under two names, a vSAN or
// vVols namespace directory by UUID or friendly name, a "[] /vmfs/volumes/..."
// path). Disks are therefore matched by path (case-insensitively, erring
// towards "in use") AND by disk UUID, which is stored in the disk itself; and
// a disk whose path cannot be parsed and that carries no UUID cannot be
// identified at all, so its VM is recorded as unknown and the check fails
// closed.
type inventoryFileUsers struct {
	byFile  map[datastoreFile][]types.ManagedObjectReference
	byUUID  map[string][]types.ManagedObjectReference
	unknown []types.ManagedObjectReference
}

// newInventoryFileUsers returns an empty inventoryFileUsers.
func newInventoryFileUsers() *inventoryFileUsers {
	return &inventoryFileUsers{
		byFile: map[datastoreFile][]types.ManagedObjectReference{},
		byUUID: map[string][]types.ManagedObjectReference{},
	}
}

// addVM records the files and disks vm uses: the backing chain (path and
// UUID) of each file-backed device, and every file in its layout. vm must
// have a configuration.
func (u *inventoryFileUsers) addVM(vm *mo.VirtualMachine) {
	unknown := false
	for _, dev := range vm.Config.Hardware.Device {
		_, isDisk := dev.(*types.VirtualDisk)
		for _, link := range diskBackingChain(dev.GetVirtualDevice().Backing) {
			f, parsed := datastoreFileOf(link.file)
			if parsed {
				u.byFile[f] = append(u.byFile[f], vm.Self)
			}
			id := normalizeDiskUUID(link.uuid)
			if id != "" {
				u.byUUID[id] = append(u.byUUID[id], vm.Self)
			}
			if isDisk && !parsed && id == "" {
				unknown = true
			}
		}
	}
	if vm.LayoutEx != nil {
		for _, lf := range vm.LayoutEx.File {
			if f, ok := datastoreFileOf(lf.Name); ok {
				u.byFile[f] = append(u.byFile[f], vm.Self)
			}
		}
	}
	if unknown {
		u.unknown = append(u.unknown, vm.Self)
	}
}

// otherThan reports whether refs holds a VM other than self.
func otherThan(refs []types.ManagedObjectReference, self types.ManagedObjectReference) bool {
	for _, ref := range refs {
		if ref != self {
			return true
		}
	}
	return false
}

// usedBy reports whether a VM other than self uses the disk at f or, when
// uuid is not empty, the disk with that UUID under any path.
func (u *inventoryFileUsers) usedBy(f datastoreFile, uuid string, self types.ManagedObjectReference) bool {
	if otherThan(u.byFile[f], self) {
		return true
	}
	if id := normalizeDiskUUID(uuid); id != "" && otherThan(u.byUUID[id], self) {
		return true
	}
	return false
}

// unknownOtherThan reports whether a VM other than self has a disk that
// cannot be identified (unparseable path, no UUID).
func (u *inventoryFileUsers) unknownOtherThan(self types.ManagedObjectReference) bool {
	return otherThan(u.unknown, self)
}

// anyInDirUsedBy reports whether a VM other than self references a file
// inside directory d.
func (u *inventoryFileUsers) anyInDirUsedBy(d datastoreFile, self types.ManagedObjectReference) bool {
	for f, refs := range u.byFile {
		if f.inDir(d) && otherThan(refs, self) {
			return true
		}
	}
	return false
}

// diskUUID returns the UUID of the disk at datastore path name (vCenter reads
// it from the disk), and whether the disk exists.
func (p *Provider) diskUUID(ctx context.Context, name string) (string, bool, error) {
	dc, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return "", false, fmt.Errorf("find datacenter: %w", err)
	}
	uuid, err := object.NewVirtualDiskManager(p.client.Client).QueryVirtualDiskUuid(ctx, name, dc)
	if err != nil {
		if fault.Is(err, &types.FileNotFound{}) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("query the UUID of disk %s: %w", name, err)
	}
	return uuid, true, nil
}

// ensureDiskFileUnused is rule 3 of the in-place confinement: diskPath must not
// be referenced by any VM or template with files on its datastore
// (scanDatastoreFileUsers). Create calls it only after confirming no VM of the
// requested name exists, so the new VM is not among those scanned.
func (p *Provider) ensureDiskFileUnused(ctx context.Context, diskPath string) error {
	f, ok := datastoreFileOf(diskPath)
	if !ok {
		return errInPlaceDiskRejected()
	}
	var dp object.DatastorePath
	dp.FromString(diskPath)
	uuid, exists, err := p.diskUUID(ctx, diskPath)
	if err == nil && !exists {
		err = fmt.Errorf("disk %s not found", diskPath)
	}
	if err != nil {
		return p.errDiskCheckFailed("in-place disk check", err)
	}
	users, err := p.datastoreFileUsers(ctx, dp.Datastore, false)
	if err != nil {
		return p.errDiskCheckFailed("in-place disk check", err)
	}
	none := types.ManagedObjectReference{}
	if users.usedBy(f, uuid, none) {
		p.logger.Warn("Refusing to attach an imported disk in place: another VM uses it", "disk_path", diskPath)
		return errDiskInUse(diskInUseMessage)
	}
	if users.unknownOtherThan(none) {
		return p.errDiskCheckFailed("in-place disk check",
			fmt.Errorf("a VM on datastore %q has a disk that can be identified by neither path nor UUID", dp.Datastore))
	}
	return nil
}

// ensureImportFolderUnused refuses (fail closed) a migration import whose
// landing folder "[<dsName>] <id>" holds a file some VM or template references.
// The import deletes or overwrites that folder's content, and its name derives
// from the target VirtualMachine's name only, so a same-named VM in another
// namespace could otherwise reach another tenant's migrated disk. fresh skips
// the scan cache (the re-check right before a failed import's cleanup).
func (p *Provider) ensureImportFolderUnused(ctx context.Context, dsName, id string, fresh bool) error {
	d, ok := datastoreFileOf(fmt.Sprintf("[%s] %s/%s", dsName, id, id+vmdkExt))
	if !ok {
		return sdkerrors.NewInvalidSpec("invalid import target name")
	}
	users, err := p.datastoreFileUsers(ctx, dsName, fresh)
	if err != nil {
		return p.errDiskCheckFailed("import folder check", err)
	}
	// The landing disk itself may also be in use under another path (an
	// alias): compare by UUID when it exists.
	landing := fmt.Sprintf("[%s] %s/%s", dsName, id, id+vmdkExt)
	uuid, exists, err := p.diskUUID(ctx, landing)
	if err != nil {
		return p.errDiskCheckFailed("import folder check", err)
	}
	none := types.ManagedObjectReference{}
	if exists && users.unknownOtherThan(none) {
		return p.errDiskCheckFailed("import folder check",
			fmt.Errorf("a VM on datastore %q has a disk that can be identified by neither path nor UUID", dsName))
	}
	if users.anyInDirUsedBy(d.dir(), none) || (exists && users.usedBy(d, uuid, none)) {
		p.logger.Warn("Refusing to import a disk: a VM uses a file in the landing folder",
			"datastore", dsName, "folder", id)
		return errDiskInUse("disk import refused: a VM in the vCenter inventory uses a disk in the landing folder " +
			"for this VirtualMachine's name; delete or rename the VirtualMachine that holds it, or migrate to " +
			"another VirtualMachine name")
	}
	return nil
}

// deleteDiskStepMessage is the only text a failed delete-time disk step
// returns; the vCenter fault can name other VMs' files.
const deleteDiskStepMessage = "could not prepare the VM's disks for delete (details are in the provider log); will retry"

// errDeleteDiskStep logs a failed delete-time disk step and returns the
// constant, retryable error the caller sees: Unavailable with
// VM_OPERATION_FAILED, kept out of the manager's circuit breaker.
func (p *Provider) errDeleteDiskStep(vmID, step string, err error) error {
	p.logger.Error("Delete-time disk step failed; the VM is kept and the delete is retried",
		"vm_id", vmID, "step", step, "error", err)
	return statusWithReasons(codes.Unavailable, deleteDiskStepMessage, contracts.VMOperationFailedReason).Err()
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
		return p.errDeleteDiskStep(vm.Reference().Value, "reading the VM's disks", err)
	}
	// FAIL-OPEN, deliberately: a VM whose configuration (or home path) vCenter
	// cannot load — inaccessible, orphaned — has no disk list to check, and
	// refusing its delete would keep it forever. It is deleted as before this
	// check existed (Destroy_Task alone), which removes whatever files vCenter
	// still associates with it; the log line records that it was not checked.
	if vmMo.Config == nil {
		p.logger.Warn("Deleting a VM whose disks cannot be checked (configuration unavailable): "+
			"Destroy_Task alone, without detaching disks it may not own", "vm_id", vm.Reference().Value)
		return nil
	}
	home, ok := exactDatastoreFileOf(vmMo.Config.Files.VmPathName)
	if !ok {
		p.logger.Warn("Deleting a VM whose disks cannot be checked (home path unreadable): "+
			"Destroy_Task alone, without detaching disks it may not own", "vm_id", vm.Reference().Value)
		return nil
	}
	homeDir := home.dir()
	// The VM's own landing disk: under its namespaced name (the namespace from
	// its owner stamp) or, for a VM created before v0.4.0, the bare name.
	// Ownership compares exact case (exactDatastoreFileOf); only "used by
	// another VM" folds case.
	ownImported := []string{legacyLandingRelPath(vmMo.Name)}
	if owner, err := ownerFromExtraConfig(vmMo.Config.ExtraConfig); err == nil && owner.Namespace != "" {
		ownImported = append(ownImported, landingRelPath(owner.Namespace, vmMo.Name))
	}

	var foreign []types.BaseVirtualDevice
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
		f, parsed := exactDatastoreFileOf(name)
		if parsed && f.inDir(homeDir) {
			continue // the VM's own directory
		}
		if !parsed {
			// A disk whose file cannot be identified is never presumed the VM's
			// own: it is detached and its file kept (possibly left orphaned).
			p.logger.Warn("A disk of the VM has an unrecognised path; detaching it before delete and keeping its file",
				"vm_id", vm.Reference().Value, "disk_path", name)
		}
		mayOwn := parsed && homeDir.path != "" && (f.dir().path == homeDir.path || slices.Contains(ownImported, f.path))
		if mayOwn {
			var dp object.DatastorePath
			dp.FromString(name)
			users, err := p.datastoreFileUsers(ctx, dp.Datastore, false)
			if err != nil {
				return p.errDiskCheckFailed("delete disk check", err)
			}
			ff, _ := datastoreFileOf(name)
			var diskID string
			if links := diskBackingChain(disk.Backing); len(links) > 0 {
				diskID = links[0].uuid
			}
			if !users.usedBy(ff, diskID, vm.Reference()) && !users.unknownOtherThan(vm.Reference()) {
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
		return p.errDeleteDiskStep(vm.Reference().Value, fmt.Sprintf("detaching %d disk(s) it does not own", len(foreign)), err)
	}
	return nil
}
