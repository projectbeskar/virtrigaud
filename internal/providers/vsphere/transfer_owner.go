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
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Owner transfer (single-host adoption).
//
// The manager's adoption flow creates a VirtualMachine for a VM it listed and
// then hands the VM to it with TransferOwner: the VM's owner stamp
// (ownerExtraConfigKey*, the keys Create writes) is set to the adopting
// VirtualMachine's identity. Without it an adopted VM kept whatever stamp it
// had — none, or the stamp of a VirtualMachine detached with
// orphan-on-delete — so ListVMs never reported it as owned, and another
// Provider object pointing at the same vCenter could adopt it a second time.
//
// It is a compare-and-swap, atomic in vCenter:
//
//   - the VM is read (name, config.template, config.extraConfig,
//     config.changeVersion, config.instanceUuid) in one property read;
//   - only an ordinary VM may be stamped: a template, an object carrying a
//     prepared-image stamp key, or a VM whose owner stamp cannot be read is
//     refused (listExclusionFor, the rule that keeps them out of ListVMs);
//   - the recorded stamp must be the new owner's (an idempotent retry,
//     nothing written), absent, or one the manager verified belongs to no
//     existing VirtualMachine (replaceable_owner_uids,
//     contracts.OwnerTransferDecision); anything else is refused;
//   - the stamp is written by ReconfigVM_Task with the changeVersion read
//     above: vCenter applies the spec only when the VM's configuration is
//     unchanged since that read (ConcurrentAccess otherwise), so a concurrent
//     re-stamp — another adopter, an administrator's edit — is never
//     overwritten. A refused write is a retryable error; the manager re-lists
//     and decides again;
//   - the stamp is read back.
//
// MOIDs are never reused within a vCenter, so the VM addressed by id is the
// VM the manager listed; expected_uuid, when sent, must equal the VM's
// instance UUID as an extra check. Writing ExtraConfig needs the privilege
// Create already needs (VirtualMachine.Config.AdvancedConfig).

// The constant messages of TransferOwner's answers. They name no other owner
// and no inventory path; the details go to the provider log.
const (
	// transferRefusedMessage refuses a VM that may not be handed over.
	transferRefusedMessage = "vSphere VM %q is owned by another VirtualMachine, is a template or image artifact, " +
		"or its owner cannot be verified; its owner was not changed"
	// transferGoneMessage answers a VM that is gone or is not the VM listed.
	transferGoneMessage = "vSphere VM %q no longer exists or is not the VM that was listed; its owner was not changed"
	// transferRetryMessage answers a transfer that may succeed when retried
	// (a vCenter error, or a concurrent change of the VM's configuration).
	transferRetryMessage = "the owner of vSphere VM %q could not be changed now (a vCenter error or a concurrent " +
		"change of its configuration; details in the provider log); retry"
)

// TransferOwner stamps the VM req.id with req.owner, compare-and-swap (see
// above). Answers: InvalidArgument (a request without an id, an owner UID,
// namespace or name, or one naming a host — vSphere is not clustered),
// NotFound (the VM is gone, or its instance UUID is not expected_uuid),
// AlreadyExists (refused; nothing changed), Unavailable with
// VM_OPERATION_FAILED (retryable; kept out of the manager's circuit breaker).
func (p *Provider) TransferOwner(ctx context.Context, req *providerv1.TransferOwnerRequest) (*providerv1.TransferOwnerResponse, error) {
	if p.client == nil {
		return nil, status.Error(codes.Unavailable, "vSphere client not configured")
	}
	owner := contracts.ObjectIdentity{UID: req.GetOwner().GetUid(), Namespace: req.GetOwner().GetNamespace(), Name: req.GetOwner().GetName()}
	if err := validateTransferRequest(req, owner); err != nil {
		return nil, err
	}
	id := req.GetId()
	ref := types.ManagedObjectReference{Type: virtualMachineMoType, Value: id}
	logArgs := []any{"vm_id", id, "owner_uid", owner.UID, "owner_namespace", owner.Namespace, "owner_name", owner.Name}

	var vm mo.VirtualMachine
	if err := property.DefaultCollector(p.client.Client).RetrieveOne(ctx, ref,
		[]string{propName, propConfigTemplate, propConfigExtraConfig, "config.changeVersion", "config.instanceUuid"}, &vm); err != nil {
		if isNotFound(err, ref) {
			return nil, status.Errorf(codes.NotFound, transferGoneMessage, id)
		}
		p.logger.Error("Reading a VM before transferring its owner failed", append(logArgs, "error", err)...)
		return nil, transferRetryError(id)
	}
	if reason := listExclusionFor(vm.Config); reason != listedVM {
		p.logger.Warn("Refusing to transfer the owner of a vSphere object that is not an adoptable VM",
			append(logArgs, "reason", string(reason))...)
		return nil, status.Errorf(codes.AlreadyExists, transferRefusedMessage, id)
	}
	if want := strings.TrimSpace(req.GetExpectedUuid()); want != "" && !strings.EqualFold(want, vm.Config.InstanceUuid) {
		p.logger.Warn("Refusing to transfer the owner of a VM that is not the VM listed (instance UUID differs)",
			append(logArgs, "instance_uuid", vm.Config.InstanceUuid, "expected_uuid", want)...)
		return nil, status.Errorf(codes.NotFound, transferGoneMessage, id)
	}
	// listExclusionFor already proved the stamp readable.
	recorded, _ := ownerFromExtraConfig(vm.Config.ExtraConfig)
	var stamps []contracts.ObjectIdentity
	if !recorded.IsZero() {
		stamps = append(stamps, recorded)
	}
	already, err := contracts.OwnerTransferDecision(owner, stamps, req.GetReplaceableOwnerUids())
	if err != nil {
		p.logger.Warn("Refusing to transfer the owner of a VM", append(logArgs, "reason", err.Error(),
			"recorded_uid", recorded.UID, "recorded_namespace", recorded.Namespace, "recorded_name", recorded.Name)...)
		return nil, status.Errorf(codes.AlreadyExists, transferRefusedMessage, id)
	}
	if already {
		return &providerv1.TransferOwnerResponse{}, nil
	}

	// The changeVersion makes the write a compare-and-swap in vCenter.
	spec := types.VirtualMachineConfigSpec{
		ChangeVersion: vm.Config.ChangeVersion,
		ExtraConfig:   ownerExtraConfig(owner),
	}
	task, err := object.NewVirtualMachine(p.client.Client, ref).Reconfigure(ctx, spec)
	if err == nil {
		err = task.Wait(ctx)
	}
	if err != nil {
		if isNotFound(err, ref) {
			return nil, status.Errorf(codes.NotFound, transferGoneMessage, id)
		}
		p.logger.Warn("Writing the new owner stamp failed (a concurrent change of the VM's configuration is refused by vCenter)",
			append(logArgs, "error", err)...)
		return nil, transferRetryError(id)
	}

	var after mo.VirtualMachine
	if err := property.DefaultCollector(p.client.Client).RetrieveOne(ctx, ref, []string{propConfigExtraConfig}, &after); err != nil || after.Config == nil {
		p.logger.Error("Reading the owner stamp back after transferring it failed", append(logArgs, "error", err)...)
		return nil, transferRetryError(id)
	}
	if got, err := ownerFromExtraConfig(after.Config.ExtraConfig); err != nil || got != owner {
		p.logger.Error("The VM does not carry exactly its new owner's stamp after the transfer", logArgs...)
		return nil, transferRetryError(id)
	}
	p.logger.Info("Transferred the owner of a VM", append(logArgs, "vm_name", vm.Name, "replaced_uid", recorded.UID)...)
	return &providerv1.TransferOwnerResponse{}, nil
}

// validateTransferRequest refuses, before any vCenter call, a request that
// names no VM (or one that is not a VM MOID), no complete owner (UID,
// namespace, name — what a Kubernetes object carries), a replaceable UID that
// is empty, or a host (vSphere is not clustered).
func validateTransferRequest(req *providerv1.TransferOwnerRequest, owner contracts.ObjectIdentity) error {
	switch {
	case !vmMoIDPattern.MatchString(req.GetId()):
		return status.Error(codes.InvalidArgument, "TransferOwner requires the id of a vSphere VM (vm-<n>)")
	case strings.TrimSpace(req.GetTargetHostId()) != "":
		return status.Error(codes.InvalidArgument, "TransferOwner: the vSphere provider is not clustered and takes no target host")
	case strings.TrimSpace(owner.UID) == "" || len(validation.IsDNS1123Label(owner.Namespace)) > 0 ||
		len(validation.IsDNS1123Subdomain(owner.Name)) > 0:
		return status.Error(codes.InvalidArgument, "TransferOwner requires the new owner's uid, namespace and name")
	}
	for _, uid := range req.GetReplaceableOwnerUids() {
		if strings.TrimSpace(uid) == "" {
			return status.Error(codes.InvalidArgument, "TransferOwner: a replaceable owner uid is empty")
		}
	}
	return nil
}

// transferRetryError is TransferOwner's retryable answer for VM id, marked
// VM_OPERATION_FAILED so one VM's failure does not count toward the manager's
// circuit breaker.
func transferRetryError(id string) error {
	return statusWithReasons(codes.Unavailable, fmt.Sprintf(transferRetryMessage, id),
		contracts.VMOperationFailedReason).Err()
}
