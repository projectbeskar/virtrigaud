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
	"regexp"
	"strings"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// VM identity and ownership.
//
// Create used to look for an existing VM with a bare-name finder search across
// the whole default datacenter and, if one matched, return its MOID as the new
// VirtualMachine's ID. A VirtualMachine named like ANY VM in the datacenter —
// another tenant's, or an unrelated production VM — therefore bound to it, and
// deleting the VirtualMachine then powered off and destroyed that VM with its
// disks. The finder error was also discarded, so an ambiguous name fell through
// to create, and govmomi's finder resolves a MOID-shaped argument ("vm-1234")
// as a managed object reference before trying it as a name, so a VirtualMachine
// named "vm-1234" bound to whatever VM had that MOID, in any datacenter.
//
// Create now:
//
//   - rejects names that are MOID-shaped or contain inventory-path characters
//     (codes.InvalidArgument) before any vCenter call;
//   - stamps the requesting VirtualMachine's identity into the new VM's
//     ExtraConfig (ownerExtraConfigKey*), on every create path;
//   - looks for an existing VM ONLY among the VirtualMachine children of the
//     exact folder the VM would be created in, comparing names exactly (no
//     finder search, no MOID resolution), and binds to it ONLY when its stamp
//     records the requester's UID. Anything else fails closed with
//     codes.AlreadyExists, which the manager maps to a non-retryable Conflict.
//
// A pre-existing VM is brought under management through the adoption flow,
// never by a same-named create.

const (
	// ownerExtraConfigKeyUID is the ExtraConfig key holding the Kubernetes UID
	// of the VirtualMachine that created this vSphere VM. It is the only stamp
	// field that authorizes binding a create to an existing VM.
	//
	// The keys deliberately do NOT use the "guestinfo." prefix: guestinfo.*
	// values are readable (and, unless isolated, writable) from inside the guest
	// through VMware Tools, whereas other ExtraConfig keys are visible only
	// through the vSphere API to principals with read access to the VM.
	ownerExtraConfigKeyUID = "virtrigaud.owner.uid"
	// ownerExtraConfigKeyNamespace is the ExtraConfig key holding the creating
	// VirtualMachine's namespace. Informational (audit, diagnostics) only.
	ownerExtraConfigKeyNamespace = "virtrigaud.owner.namespace"
	// ownerExtraConfigKeyName is the ExtraConfig key holding the creating
	// VirtualMachine's name. Informational (audit, diagnostics) only.
	ownerExtraConfigKeyName = "virtrigaud.owner.name"

	// virtualMachineMoType is the vSphere managed object type of a VM.
	virtualMachineMoType = "VirtualMachine"

	// propName, propConfigExtraConfig and propConfigTemplate are the property
	// paths the ownership check retrieves.
	propName              = "name"
	propConfigExtraConfig = "config.extraConfig"
	propConfigTemplate    = "config.template"

	// vmNameReservedChars are characters a VM name must not contain: '/' is the
	// inventory-path separator, and vSphere escapes '/', '\' and '%' in entity
	// names (as %2f, %5c, %25), so a name containing them would not compare
	// equal to the stored name; ':' is the separator of the "Type:value" managed
	// object reference form govmomi's finder resolves. None of them can appear in
	// a Kubernetes object name.
	vmNameReservedChars = `/\%:`
)

// vmMoIDPattern matches the shape of a vCenter VirtualMachine managed object ID
// ("vm-" followed by digits). govmomi's find.Finder resolves such an argument as
// a reference to the VM with that MOID (object.ReferenceFromString) before it
// tries it as a name, in any datacenter; operators' govmomi-based tooling (govc)
// does the same. A VM carrying such a name is therefore never created.
var vmMoIDPattern = regexp.MustCompile(`^vm-[0-9]+$`)

// unsafeNameReason returns why name cannot be used as the name of a vSphere VM
// or template that VirtRigaud creates or looks up by name, or "" when it can.
// It performs no vCenter call.
func unsafeNameReason(name string) string {
	switch {
	case strings.TrimSpace(name) == "":
		return "it is empty"
	case strings.ContainsAny(name, vmNameReservedChars):
		return fmt.Sprintf("it contains one of the characters %q, which vSphere inventory paths and "+
			"managed object references treat specially", vmNameReservedChars)
	case vmMoIDPattern.MatchString(name):
		return "it has the shape of a vCenter VirtualMachine managed object ID (\"vm-\" followed only by digits), " +
			"which govmomi-based lookups resolve as a reference to a different VM before trying it as a name"
	}
	return ""
}

// invalidVMNameError returns a codes.InvalidArgument status error when name
// cannot safely be used as the name of a vSphere VM VirtRigaud creates, or nil
// when it can. It performs no vCenter call, so it runs before any lookup.
//
// VirtualMachine CRD validation is deliberately NOT tightened: it is shared by
// every provider and is released v1beta1 API.
func invalidVMNameError(name string) error {
	if reason := unsafeNameReason(name); reason != "" {
		return status.Errorf(codes.InvalidArgument,
			"name %q cannot be used as a vSphere VM name: %s; rename the VirtualMachine", name, reason)
	}
	return nil
}

// ownerFromCreateRequest reads CreateRequest.owner. An absent owner (a manager
// older than this provider) yields the zero identity, which never authorizes a
// bind to an existing VM.
func ownerFromCreateRequest(req *providerv1.CreateRequest) contracts.ObjectIdentity {
	o := req.GetOwner()
	if o == nil {
		return contracts.ObjectIdentity{}
	}
	return contracts.ObjectIdentity{
		UID:       o.GetUid(),
		Namespace: o.GetNamespace(),
		Name:      o.GetName(),
	}
}

// ownerExtraConfig returns the ExtraConfig entries that stamp owner onto a VM
// being created or cloned.
//
// For a zero owner (no UID) it returns the same three keys with empty values.
// vSphere removes an ExtraConfig key set to "", so on a clone (from a template
// or from another VM, which copies the source's ExtraConfig) this strips any
// stamp inherited from the source instead of letting the new VM claim the
// source's owner.
func ownerExtraConfig(owner contracts.ObjectIdentity) []types.BaseOptionValue {
	if owner.IsZero() {
		// A namespace/name without a UID proves nothing; do not record it.
		owner = contracts.ObjectIdentity{}
	}
	return []types.BaseOptionValue{
		&types.OptionValue{Key: ownerExtraConfigKeyUID, Value: owner.UID},
		&types.OptionValue{Key: ownerExtraConfigKeyNamespace, Value: owner.Namespace},
		&types.OptionValue{Key: ownerExtraConfigKeyName, Value: owner.Name},
	}
}

// ownerFromExtraConfig reads the owner stamp from a VM's ExtraConfig. Keys are
// matched case-insensitively (VMX keys are case-insensitive). A VM without a
// stamp, or with an empty UID, yields the zero identity. It fails closed on a
// key that appears more than once or has a non-string value — VirtRigaud never
// writes either, so such a stamp was edited by hand and is not trusted.
func ownerFromExtraConfig(extraConfig []types.BaseOptionValue) (contracts.ObjectIdentity, error) {
	var id contracts.ObjectIdentity
	seen := make(map[string]bool, 3)
	for _, bov := range extraConfig {
		if bov == nil {
			continue
		}
		ov := bov.GetOptionValue()
		if ov == nil {
			continue
		}
		key := strings.ToLower(ov.Key)
		var dst *string
		switch key {
		case ownerExtraConfigKeyUID:
			dst = &id.UID
		case ownerExtraConfigKeyNamespace:
			dst = &id.Namespace
		case ownerExtraConfigKeyName:
			dst = &id.Name
		default:
			continue
		}
		if seen[key] {
			return contracts.ObjectIdentity{}, fmt.Errorf("owner stamp repeats ExtraConfig key %q", key)
		}
		seen[key] = true
		value, ok := ov.Value.(string)
		if !ok {
			return contracts.ObjectIdentity{}, fmt.Errorf("owner stamp ExtraConfig key %q has a non-string value (%T)", key, ov.Value)
		}
		*dst = value
	}
	return id, nil
}

// requesterOwnsVM reports whether a create by requester may bind to an existing
// VM whose recorded owner stamp is recorded. It is the whole authorization
// decision and fails closed:
//
//   - a requester without a UID (e.g. an older manager) owns nothing;
//   - a VM with no stamp (not created by VirtRigaud, or created before stamping
//     existed) is owned by nobody who can prove it;
//   - otherwise the stamped UID must equal the requester's exactly. Namespace
//     and name are informational and never consulted.
func requesterOwnsVM(requester, recorded contracts.ObjectIdentity) bool {
	if requester.IsZero() || recorded.IsZero() {
		return false
	}
	return recorded.UID == requester.UID
}

// vmConflictError is the codes.AlreadyExists error returned when the requested
// VM name is taken in the target folder by a VM the requester does not own.
//
// The message is deliberately uniform and names only the requested VM: it lands
// in the requesting VirtualMachine's status, so it must not disclose which other
// namespace/VirtualMachine (if any) owns the VM, nor the inventory path. The
// details go to the provider log for the operator.
func vmConflictError(name string) error {
	return status.Errorf(codes.AlreadyExists,
		"vSphere VM %q already exists in the target folder and is not owned by this VirtualMachine; "+
			"refusing to bind to it. Bring the existing VM under management through the adoption flow, "+
			"remove or rename it, or rename the VirtualMachine", name)
}

// resolveVMFolder resolves the folder a VM is created (and looked up) in:
// folderOverride (VirtualMachine placement) or the provider DefaultFolder, and
// the datacenter's default VM folder when neither is set or the named folder
// does not exist. The finder must already be scoped to the datacenter.
//
// Only a definitive NotFound or MultipleFound falls back to the default VM
// folder (both deterministic, so every retry resolves the same folder). Any
// other error — e.g. a transient vCenter failure — is returned: falling back
// then would make the existing-VM check look in a different folder than a
// previous attempt created the VM in.
func (p *Provider) resolveVMFolder(ctx context.Context, folderOverride string) (*object.Folder, error) {
	folderName := p.config.DefaultFolder
	if folderOverride != "" {
		folderName = folderOverride
		p.logger.Info("Using placement override for folder", "folder", folderName)
	}

	if folderName != "" {
		folder, err := p.finder.Folder(ctx, folderName)
		if err == nil {
			return folder, nil
		}
		var notFound *find.NotFoundError
		var multiple *find.MultipleFoundError
		if !stderrors.As(err, &notFound) && !stderrors.As(err, &multiple) {
			return nil, fmt.Errorf("find folder %q: %w", folderName, err)
		}
		p.logger.Warn("Failed to find folder, using datacenter default VM folder", "folder", folderName, "error", err)
	}

	folder, err := p.finder.DefaultFolder(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to find datacenter VM folder: %w", err)
	}
	return folder, nil
}

// vmsNamedInFolder returns the VirtualMachines (VMs and templates) that are
// DIRECT children of folder and whose name is exactly name.
//
// It never uses find.Finder: it lists the folder's childEntity and compares the
// retrieved names byte-for-byte, so a name is never resolved as a managed
// object reference, a glob, or a path, and a same-named VM in any other folder,
// vApp or datacenter is not considered. vCenter keeps VM names unique within a
// folder, so more than one match means the inventory is not what VirtRigaud
// expects and the caller must fail closed.
func (p *Provider) vmsNamedInFolder(ctx context.Context, folder *object.Folder, name string) ([]types.ManagedObjectReference, error) {
	children, err := folder.Children(ctx)
	if err != nil {
		return nil, fmt.Errorf("list children of folder %s: %w", folder.Reference().Value, err)
	}

	var vmRefs []types.ManagedObjectReference
	for _, child := range children {
		if ref := child.Reference(); ref.Type == virtualMachineMoType {
			vmRefs = append(vmRefs, ref)
		}
	}
	if len(vmRefs) == 0 {
		return nil, nil
	}

	var vms []mo.VirtualMachine
	if err := property.DefaultCollector(p.client.Client).Retrieve(ctx, vmRefs, []string{propName}, &vms); err != nil {
		return nil, fmt.Errorf("retrieve names of VMs in folder %s: %w", folder.Reference().Value, err)
	}

	var matches []types.ManagedObjectReference
	for _, vm := range vms {
		if vm.Name == name {
			matches = append(matches, vm.Self)
		}
	}
	return matches, nil
}

// bindExistingVM decides a create whose requested name is already taken, in the
// target folder, by the VM ref. It returns ref's MOID as the VM ID (idempotent
// success) ONLY when requesterOwnsVM says the VM's owner stamp records the
// requester's UID — the case of a create retried after the manager lost its
// Status.ID write — and the VM is not a template. Otherwise it returns
// vmConflictError and never binds. A failure to read the stamp is returned as a
// plain (retryable) error: the next attempt re-lists and re-decides.
func (p *Provider) bindExistingVM(ctx context.Context, ref types.ManagedObjectReference, name string, requester contracts.ObjectIdentity) (*providerv1.CreateResponse, error) {
	var vm mo.VirtualMachine
	if err := property.DefaultCollector(p.client.Client).RetrieveOne(ctx, ref,
		[]string{propConfigExtraConfig, propConfigTemplate}, &vm); err != nil {
		// ref may be another tenant's VM: its MOID and the vCenter fault text go
		// to the provider log only; the caller gets a generic, retryable error.
		p.logger.Error("Reading the owner stamp of an existing VM failed",
			"vm_name", name, "vm_id", ref.Value, "error", err)
		return nil, fmt.Errorf("read owner of existing VM %q: a vCenter error occurred (details in the provider log); will retry", name)
	}

	var (
		recorded   contracts.ObjectIdentity
		parseErr   error
		isTemplate bool
	)
	if vm.Config != nil {
		// An inaccessible VM has no config and therefore no provable owner.
		recorded, parseErr = ownerFromExtraConfig(vm.Config.ExtraConfig)
		isTemplate = vm.Config.Template
	}

	if parseErr == nil && !isTemplate && requesterOwnsVM(requester, recorded) {
		p.logger.Info("VM already exists in the target folder and is owned by this VirtualMachine; treating create as idempotent",
			"vm_name", name, "vm_id", ref.Value, "owner_uid", requester.UID)
		return &providerv1.CreateResponse{Id: ref.Value}, nil
	}

	logArgs := []any{
		"vm_name", name, "vm_id", ref.Value,
		"requester_namespace", requester.Namespace, "requester_name", requester.Name, "requester_uid", requester.UID,
	}
	switch {
	case parseErr != nil:
		p.logger.Warn("Refusing to bind existing VM: its owner stamp could not be read",
			append(logArgs, "error", parseErr.Error())...)
	case isTemplate:
		p.logger.Warn("Refusing to bind existing VM: it is a template", logArgs...)
	case requester.IsZero():
		p.logger.Warn("Refusing to bind existing VM: the create request carries no owner UID "+
			"(manager older than the provider?), so ownership cannot be proven", logArgs...)
	case recorded.IsZero():
		p.logger.Warn("Refusing to bind existing VM: it has no VirtRigaud owner stamp "+
			"(not created by VirtRigaud, or created before ownership stamping); adopt it instead", logArgs...)
	default:
		p.logger.Warn("Refusing to bind existing VM: it is owned by another VirtualMachine",
			append(logArgs, "owner_namespace", recorded.Namespace, "owner_name", recorded.Name, "owner_uid", recorded.UID)...)
	}
	return nil, vmConflictError(name)
}
