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

package proxmox

import (
	"context"
	stderrors "errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Owner transfer (single-host adoption).
//
// The manager's adoption flow creates a VirtualMachine for a VM it listed and
// then hands the VM to it with TransferOwner: the VM's owner stamp (the
// description line vm_ownership.go writes) is set to the adopting
// VirtualMachine's identity, phase=ready. Without it an adopted VM kept
// whatever stamp it had, and a VM stamped by a VirtualMachine that was
// detached with orphan-on-delete and then adopted again was answered
// VM_NOT_OWNED by its new VirtualMachine's Delete (checkDelete) and left on
// the cluster.
//
// It is a compare-and-swap, atomic in PVE:
//
//   - the VM's config is read once (description, name, lock, template and
//     the config digest);
//   - a template is refused, and a VM locked by a PVE task (clone, create,
//     backup, migrate...) is answered retryably — it is still being made or
//     changed;
//   - the VM must still be the VM the manager listed: its PVE name must be the
//     new owner's (unstampedNamedLikeOwner, the rule Delete applies to an
//     unstamped VM — adoption names the VirtualMachine after the VM), so a
//     VMID reused by another VM since the listing is never stamped (NotFound);
//   - the recorded stamp must be the new owner's (an idempotent retry,
//     nothing written), absent, or one the manager verified belongs to no
//     existing VirtualMachine (replaceable_owner_uids,
//     contracts.OwnerTransferDecision); a stamp that cannot be read, that
//     names more than one owner, or that names anyone else is refused;
//   - the description is written with the digest read above: PVE applies the
//     update only to an unchanged config ("detected modified configuration"
//     otherwise), so a concurrent re-stamp is never overwritten. A refused
//     write is retryable; the manager re-lists and decides again;
//   - the stamp is read back.
//
// The other lines of the description (the VM's notes) are kept.

// pveConfigKeyDigest is the config digest PVE returns with a VM's config and
// accepts on an update, which it then applies only to that config.
const pveConfigKeyDigest = "digest"

// vmReferenceRE is the shape of a Proxmox VM id: a VMID, optionally prefixed
// with its node ("<node>:<vmid>").
var vmReferenceRE = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9.-]*:)?[0-9]+$`)

// The constant messages of TransferOwner's answers. They name no other owner;
// the details go to the provider log.
const (
	// transferRefusedMessage refuses a VM that may not be handed over.
	transferRefusedMessage = "Proxmox VM %d is owned by another VirtualMachine, is a template, or its owner cannot " +
		"be verified; its owner was not changed"
	// transferGoneMessage answers a VM that is gone, or is not the VM listed.
	transferGoneMessage = "Proxmox VM %d no longer exists or is not the VM that was listed (its name differs); " +
		"its owner was not changed"
	// transferRetryMessage answers a transfer that may succeed when retried.
	transferRetryMessage = "the owner of Proxmox VM %d could not be changed now (the VM is locked or being " +
		"changed, or PVE could not be reached; details in the provider log); retry"
)

// TransferOwner stamps the VM req.id with req.owner, compare-and-swap (see
// above). Answers: InvalidArgument (a request without a valid id or a
// stampable owner, with a malformed replaceable UID, or naming a host — the
// provider is not clustered), NotFound (gone, or not the VM listed),
// AlreadyExists (refused; nothing changed), Unavailable with
// VM_OPERATION_FAILED (retryable; kept out of the manager's circuit breaker).
func (p *Provider) TransferOwner(ctx context.Context, req *providerv1.TransferOwnerRequest) (*providerv1.TransferOwnerResponse, error) {
	if p.client == nil {
		return nil, statusWithReasons(codes.Unavailable, "PVE client not configured")
	}
	owner, err := p.validateTransferRequest(req)
	if err != nil {
		return nil, err
	}
	vmid, node, err := p.parseVMReference(req.GetId())
	if err != nil {
		p.logger.Error("Resolving the node of a VM before transferring its owner failed", "vm_id", req.GetId(), "error", err)
		return nil, transferRetryError(0)
	}
	logArgs := []any{"vmid", vmid, "owner_uid", owner.UID, "owner_namespace", owner.Namespace, "owner_name", owner.Name}

	cfg, node, err := p.transferTargetConfig(ctx, node, vmid)
	if err != nil {
		return nil, err
	}
	logArgs = append(logArgs, "node", node)
	pveName := configString(cfg, pveConfigKeyName)
	digest := configString(cfg, pveConfigKeyDigest)
	description := configString(cfg, pveConfigKeyDescription)
	read := readOwnerStamp(description)

	switch {
	case configIsTemplate(cfg):
		p.logger.Warn("Refusing to transfer the owner of a template", logArgs...)
		return nil, statusWithReasons(codes.AlreadyExists, fmt.Sprintf(transferRefusedMessage, vmid))
	case configString(cfg, pveConfigKeyLock) != "":
		p.logger.Info("Not transferring the owner of a VM locked by a PVE task; retry",
			append(logArgs, "lock", configString(cfg, pveConfigKeyLock))...)
		return nil, transferRetryError(vmid)
	case !unstampedNamedLikeOwner(pveName, vmid, owner):
		p.logger.Warn("Refusing to transfer the owner of a VM not named like its new owner (its VMID was reused?)",
			append(logArgs, "vm_name", pveName)...)
		return nil, statusWithReasons(codes.NotFound, fmt.Sprintf(transferGoneMessage, vmid))
	case read.state != "":
		p.logger.Warn("Refusing to transfer the owner of a VM whose owner stamp cannot be relied on",
			append(logArgs, "owner_stamp_state", read.state, "problem", read.problem)...)
		return nil, statusWithReasons(codes.AlreadyExists, fmt.Sprintf(transferRefusedMessage, vmid))
	}

	var recorded []contracts.ObjectIdentity
	if read.stamp != nil {
		if read.stamp.phase != ownerStampPhaseReady {
			// A Create or Clone for that owner has not finished: never taken
			// over, not even by the same owner (adoption never creates).
			p.logger.Warn("Refusing to transfer the owner of a VM whose create has not finished",
				append(logArgs, "recorded_uid", read.stamp.owner.UID)...)
			return nil, statusWithReasons(codes.AlreadyExists, fmt.Sprintf(transferRefusedMessage, vmid))
		}
		recorded = append(recorded, read.stamp.owner)
	}
	already, err := contracts.OwnerTransferDecision(owner, recorded, req.GetReplaceableOwnerUids())
	if err != nil {
		p.logger.Warn("Refusing to transfer the owner of a VM", append(logArgs, "reason", err.Error(), "recorded_uids", read.uids)...)
		return nil, statusWithReasons(codes.AlreadyExists, fmt.Sprintf(transferRefusedMessage, vmid))
	}
	if already {
		return &providerv1.TransferOwnerResponse{}, nil
	}
	if digest == "" {
		// Without the digest the write could not be a compare-and-swap.
		p.logger.Error("PVE returned the VM's config without a digest; not transferring its owner", logArgs...)
		return nil, transferRetryError(vmid)
	}

	stamp := ownerStamp{owner: owner, phase: ownerStampPhaseReady}
	p.warnIfTruncated(vmid, description, &stamp)
	vals := url.Values{}
	vals.Set(pveConfigKeyDescription, composeDescription(description, &stamp))
	vals.Set(pveConfigKeyDigest, digest)
	task, err := p.client.ReconfigureVMRaw(ctx, node, vmid, vals)
	if err == nil && task != "" {
		err = p.client.WaitForTask(ctx, node, task)
	}
	if err != nil {
		p.logger.Warn("Writing the new owner stamp failed (PVE refuses a write to a config changed since it was read)",
			append(logArgs, "error", err)...)
		return nil, transferRetryError(vmid)
	}
	if err := p.verifyOwnerStamp(ctx, node, vmid, &stamp); err != nil {
		p.logger.Error("The VM does not carry exactly its new owner's stamp after the transfer", append(logArgs, "error", err)...)
		return nil, transferRetryError(vmid)
	}
	p.logger.Info("Transferred the owner of a VM", append(logArgs, "vm_name", pveName, "replaced_uids", read.uids)...)
	return &providerv1.TransferOwnerResponse{}, nil
}

// validateTransferRequest refuses, before any PVE call, a request without a
// VM id of the Proxmox shape, without a stampable owner (UID, namespace and
// name in the stamp's charset), with a replaceable UID outside that charset,
// or naming a host. It returns the owner.
func (p *Provider) validateTransferRequest(req *providerv1.TransferOwnerRequest) (contracts.ObjectIdentity, error) {
	if !vmReferenceRE.MatchString(req.GetId()) {
		return contracts.ObjectIdentity{}, statusWithReasons(codes.InvalidArgument,
			"TransferOwner requires the id of a Proxmox VM (<vmid> or <node>:<vmid>)")
	}
	if strings.TrimSpace(req.GetTargetHostId()) != "" {
		return contracts.ObjectIdentity{}, statusWithReasons(codes.InvalidArgument,
			"TransferOwner: the Proxmox provider is not clustered and takes no target host")
	}
	owner, err := stampableOwner(req.GetOwner(), "owner")
	if err != nil {
		return contracts.ObjectIdentity{}, err
	}
	if owner.IsZero() {
		return contracts.ObjectIdentity{}, statusWithReasons(codes.InvalidArgument,
			"TransferOwner requires the new owner's uid, namespace and name")
	}
	for _, uid := range req.GetReplaceableOwnerUids() {
		if !ownerIdentityFieldRE.MatchString(uid) || len(uid) > ownerIdentityFieldMaxLen {
			return contracts.ObjectIdentity{}, statusWithReasons(codes.InvalidArgument,
				"TransferOwner: a replaceable owner uid is malformed")
		}
	}
	return owner, nil
}

// transferTargetConfig reads the config of VM vmid on node and returns it
// with the node it was read on. A VM that is not on node (a bare VMID is
// looked up on the default node) is looked up cluster-wide: on another node
// it is read there; gone from the whole cluster it is NotFound. Any read
// failure is retryable.
func (p *Provider) transferTargetConfig(ctx context.Context, node string, vmid int) (map[string]interface{}, string, error) {
	cfg, err := p.client.GetVMConfig(ctx, node, vmid)
	if stderrors.Is(err, pveapi.ErrVMNotFound) {
		vms, lerr := p.client.ListClusterVMs(ctx)
		if lerr != nil {
			p.logger.Error("Listing the cluster to find a VM before transferring its owner failed", "vmid", vmid, "error", lerr)
			return nil, "", transferRetryError(vmid)
		}
		other := ""
		for _, vm := range vms {
			if vm.VMID == vmid && vm.Node != node {
				other = vm.Node
				break
			}
		}
		if other == "" {
			gone, gerr := p.client.VMIDFree(ctx, vmid)
			if gerr != nil || !gone {
				p.logger.Error("Cannot confirm that a VM is gone before transferring its owner", "vmid", vmid, "error", gerr)
				return nil, "", transferRetryError(vmid)
			}
			return nil, "", statusWithReasons(codes.NotFound, fmt.Sprintf(transferGoneMessage, vmid))
		}
		node = other
		cfg, err = p.client.GetVMConfig(ctx, node, vmid)
	}
	if err != nil {
		p.logger.Error("Reading a VM's config before transferring its owner failed", "vmid", vmid, "node", node, "error", err)
		return nil, "", transferRetryError(vmid)
	}
	return cfg, node, nil
}

// transferRetryError is TransferOwner's retryable answer for VM vmid, marked
// VM_OPERATION_FAILED so one VM's failure does not count toward the manager's
// circuit breaker.
func transferRetryError(vmid int) error {
	return statusWithReasons(codes.Unavailable, fmt.Sprintf(transferRetryMessage, vmid), contracts.VMOperationFailedReason)
}
