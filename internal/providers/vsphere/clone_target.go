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

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// The Clone target (rc2 R4).
//
// A Clone is re-sent when the manager never recorded its answer (a manager
// restart, a status write lost after the RPC, a call that ran past its
// deadline). A manager that knows the target VirtualMachine's UID sends it in
// CloneRequest.target_vm, and the clone is stamped with it in the same
// CloneVM_Task that creates it (the owner ExtraConfig keys, as a Create's).
// Before cloning, the provider looks for the target name among the direct
// children of the folder it clones into, exactly as Create does
// (vmsNamedInFolder: byte-for-byte names, no finder, no MOID resolution):
//
//   - no VM of that name: the clone is made, stamped;
//   - one, stamped with the target's UID and not a template: an earlier
//     attempt made it; its MOID is returned (idempotent success) and nothing
//     is cloned;
//   - anything else — unstamped, stamped for another VirtualMachine, an
//     unreadable stamp, a template, more than one, or a request without a UID
//     (a manager older than this provider) — is refused with AlreadyExists, the
//     class Create uses, before the source is read or changed (a linked
//     clone's base snapshot included).
//
// A request without a UID clones unstamped, as before.

// cloneTargetOwner reads CloneRequest.target_vm when it carries a UID: the
// VirtualMachine the clone will be bound to, which the clone is stamped with.
// Without a UID it is the zero identity, which stamps nothing and owns
// nothing.
func cloneTargetOwner(req *providerv1.CloneRequest) contracts.ObjectIdentity {
	o := req.GetTargetVm()
	if o.GetUid() == "" {
		return contracts.ObjectIdentity{}
	}
	return contracts.ObjectIdentity{UID: o.GetUid(), Namespace: o.GetNamespace(), Name: o.GetName()}
}

// vmCloneConflictError is the codes.AlreadyExists answer to a Clone whose
// target name is taken in the target folder by a VM the clone's target
// VirtualMachine does not own. Like vmConflictError it names only the
// requested VM: the other VM's owner and location go to the provider log.
func vmCloneConflictError(name string) error {
	return status.Errorf(codes.AlreadyExists,
		"vSphere VM %q already exists in the target folder and is not owned by the clone's target VirtualMachine; "+
			"nothing was cloned. Remove or rename that VM, or clone to another target name", name)
}

// existingCloneTarget decides a Clone's target name in folder before anything
// is cloned. It returns the MOID of the VM an earlier attempt of this Clone
// made (stamped with owner's UID), "" when the name is free, or an error: a
// Conflict for any VM of that name the target does not own, and a generic,
// retryable error when the folder or the VM's stamp cannot be read (the
// vCenter detail goes to the provider log only).
func (p *Provider) existingCloneTarget(ctx context.Context, folder *object.Folder, name string, owner contracts.ObjectIdentity) (string, error) {
	existing, err := p.vmsNamedInFolder(ctx, folder, name)
	if err != nil {
		p.logger.Error("Clone target check in the target folder failed",
			"vm_name", name, "folder", folder.Reference().Value, "error", err)
		return "", fmt.Errorf("check for an existing VM named %q in the target folder: "+
			"a vCenter error occurred (details in the provider log); will retry", name)
	}
	switch len(existing) {
	case 0:
		return "", nil
	case 1:
	default:
		p.logger.Warn("Refusing the clone: more than one VM with the target name exists in the target folder",
			"vm_name", name, "folder", folder.Reference().Value, "count", len(existing))
		return "", vmCloneConflictError(name)
	}

	ref := existing[0]
	var vm mo.VirtualMachine
	if err := property.DefaultCollector(p.client.Client).RetrieveOne(ctx, ref,
		[]string{propConfigExtraConfig, propConfigTemplate}, &vm); err != nil {
		p.logger.Error("Reading the owner stamp of the clone's existing target failed",
			"vm_name", name, "vm_id", ref.Value, "error", err)
		return "", fmt.Errorf("read owner of existing VM %q: a vCenter error occurred (details in the provider log); will retry", name)
	}
	var (
		recorded   contracts.ObjectIdentity
		parseErr   error
		isTemplate bool
	)
	if vm.Config != nil {
		// A VM whose configuration cannot be read (e.g. still being made by
		// an earlier attempt's clone) has no provable owner yet.
		recorded, parseErr = ownerFromExtraConfig(vm.Config.ExtraConfig)
		isTemplate = vm.Config.Template
	}
	if parseErr == nil && !isTemplate && requesterOwnsVM(owner, recorded) {
		p.logger.Info("The clone's target already exists in the target folder, stamped with its target VirtualMachine; "+
			"an earlier attempt of this clone made it: treating the clone as done",
			"vm_name", name, "vm_id", ref.Value, "owner_uid", owner.UID)
		return ref.Value, nil
	}

	logArgs := []any{"vm_name", name, "vm_id", ref.Value,
		"target_namespace", owner.Namespace, "target_name", owner.Name, "target_uid", owner.UID}
	switch {
	case parseErr != nil:
		p.logger.Warn("Refusing the clone: the existing target's owner stamp could not be read", append(logArgs, "error", parseErr.Error())...)
	case isTemplate:
		p.logger.Warn("Refusing the clone: the existing target is a template", logArgs...)
	case owner.IsZero():
		p.logger.Warn("Refusing the clone: the request carries no target UID (manager older than the provider?), "+
			"so the existing target cannot be proven to be its own", logArgs...)
	case recorded.IsZero():
		p.logger.Warn("Refusing the clone: the existing target has no VirtRigaud owner stamp", logArgs...)
	default:
		p.logger.Warn("Refusing the clone: the existing target is owned by another VirtualMachine",
			append(logArgs, "owner_namespace", recorded.Namespace, "owner_name", recorded.Name, "owner_uid", recorded.UID)...)
	}
	return "", vmCloneConflictError(name)
}

// cloneConfigSpec is the ConfigSpec a Clone sends with CloneVM_Task: the
// target's owner stamp (or, without a UID, the owner keys emptied, so the
// clone never inherits the source's stamp) and clearImage, the removal of
// every prepared-image key the source carries.
func cloneConfigSpec(owner contracts.ObjectIdentity, clearImage []types.BaseOptionValue) *types.VirtualMachineConfigSpec {
	return &types.VirtualMachineConfigSpec{ExtraConfig: append(ownerExtraConfig(owner), clearImage...)}
}
