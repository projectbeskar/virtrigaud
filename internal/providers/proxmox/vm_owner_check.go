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

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Owner checks of the per-VM calls other than Delete (Describe, Power,
// Reconfigure, SnapshotCreate, SnapshotDelete, SnapshotRevert, GetDiskInfo,
// ExportDisk, and the source of a Clone, checked against
// CloneRequest.source_owner).
//
// Proxmox VMIDs are reused: once a VM is destroyed (outside VirtRigaud, say),
// /cluster/nextid may hand its VMID to the next VM anyone creates. A
// VirtualMachine whose status.id still records that VMID would otherwise
// read, power, resize, snapshot or revert the unrelated VM that took it over.
// When the request names its owner (the manager sends the VirtualMachine's
// identity on every one of these calls), the VM's owner stamp is checked
// against it BEFORE anything of the VM is read for the answer or changed — the
// same rule as Delete's (checkDelete):
//
//   - stamped with the requester's UID: the call goes on;
//   - unstamped (created before owner stamps, adopted, or cloned without a
//     target UID) and named like the owner (unstampedNamedLikeOwner: the
//     name Create or adoption gave it): the call goes on, as before;
//   - stamped with another UID, or unstamped with any other name: NotFound +
//     VM_NOT_OWNED, and the VM is neither read nor changed;
//   - a stamp that cannot be read or records more than one owner:
//     FailedPrecondition + OWNER_STAMP_UNREADABLE (fail closed);
//   - the config cannot be read: Unavailable (retried), nothing done.
//
// A request without an owner (a manager older than this check) is not
// checked, exactly as before. Templates keep their handling: only Delete
// refuses them (VM_IS_IMAGE_ARTIFACT); for the calls here a template is owner
// checked like any VM.

// The per-VM calls an owner check names in its answer and log line.
const (
	ownerCheckOpDescribe       = "describe"
	ownerCheckOpPower          = "power operation"
	ownerCheckOpReconfigure    = "reconfigure"
	ownerCheckOpSnapshotCreate = "snapshot create"
	ownerCheckOpSnapshotDelete = "snapshot delete"
	ownerCheckOpSnapshotRevert = "snapshot revert"
	ownerCheckOpGetDiskInfo    = "disk read"
	ownerCheckOpExportDisk     = "disk export"
	ownerCheckOpCloneSource    = "clone"
)

// vmNotOwnedMessage is the constant answer (per call) to a request for a VM
// that is not the requester's (VM_NOT_OWNED). It names no VM, stamp or owner:
// it lands in the requesting object's status, and the details go to the
// provider log only.
func vmNotOwnedMessage(op string) string {
	return fmt.Sprintf("the Proxmox VM at this VMID is not this VirtualMachine's (it carries another owner's stamp, or "+
		"it is unstamped and named differently: its VMID was reused, or the VirtualMachine was re-created under another "+
		"UID); the %s was not performed and none of the VM's state was read", op)
}

// ownerStampUnreadableMessage is the constant answer (per call) to a request
// for a VM whose owner stamp cannot be read or records more than one owner
// (OWNER_STAMP_UNREADABLE).
func ownerStampUnreadableMessage(op string) string {
	return fmt.Sprintf("refused: the VM carries an owner stamp (%s line of its description) that cannot be read or "+
		"records more than one owner, so who owns it cannot be told; the %s was not performed. Fix the stamp, or "+
		"remove the VM by hand", ownerStampMarker, op)
}

// requestOwner reads the owner of a per-VM request; the zero identity when
// it is absent or carries no UID (a manager older than the owner checks).
func requestOwner(o *providerv1.ObjectIdentity) contracts.ObjectIdentity {
	return contracts.ObjectIdentity{UID: o.GetUid(), Namespace: o.GetNamespace(), Name: o.GetName()}
}

// checkVMOwner reads the config of VM vmid on node and decides, with
// checkOwnerInConfig, whether the per-VM call op made for owner may go on. It
// returns nil to go on, and the answer otherwise. A request without an owner
// is not checked and costs no API call.
//
// A VM the config read cannot find on node is left to the call itself: each
// call already handles a VM that is not on the node asked (Describe confirms
// cluster-wide whether it is gone; the others fail on PVE's answer), and there
// is no VM there whose state it could read or change.
func (p *Provider) checkVMOwner(ctx context.Context, node string, vmid int, owner contracts.ObjectIdentity, op string) error {
	if owner.IsZero() {
		return nil
	}
	cfg, err := p.client.GetVMConfig(ctx, node, vmid)
	if stderrors.Is(err, pveapi.ErrVMNotFound) {
		return nil
	}
	if err != nil {
		p.logger.Error("Reading the VM configuration to check its owner failed; not performing the call",
			"op", op, "vmid", vmid, "node", node, "error", err)
		return errors.NewUnavailable(fmt.Sprintf("cannot read the configuration of Proxmox VM %d to check its owner; "+
			"the %s was not performed (will retry)", vmid, op), nil)
	}
	return p.checkOwnerInConfig(cfg, node, vmid, owner, op)
}

// checkRequestVMOwner is checkVMOwner for a call that has only the VM's
// provider id (ExportDisk): it resolves the id to a VMID and node only when
// the request carries an owner, so a request without one is unchanged.
func (p *Provider) checkRequestVMOwner(ctx context.Context, id string, owner contracts.ObjectIdentity, op string) error {
	if owner.IsZero() {
		return nil
	}
	if p.client == nil {
		return errors.NewUnavailable("PVE client not configured", nil)
	}
	vmid, node, err := p.parseVMReference(id)
	if err != nil {
		return errors.NewInvalidSpec("invalid VM reference: %v", err)
	}
	return p.checkVMOwner(ctx, node, vmid, owner, op)
}

// isOwnerRefusal reports whether err is an owner check's refusal
// (checkOwnerInConfig): a status carrying VM_NOT_OWNED or
// OWNER_STAMP_UNREADABLE. A caller that wraps the errors of an inner call
// returns such a refusal unwrapped, so its code and reason reach the manager.
func isOwnerRefusal(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetDomain() == contracts.ErrorInfoDomain &&
			(info.GetReason() == contracts.VMNotOwnedReason || info.GetReason() == contracts.OwnerStampUnreadableReason) {
			return true
		}
	}
	return false
}

// checkCloneSourceOwner is the owner check of a Clone's source VM (vmid on
// node) against CloneRequest.source_owner, with Clone's error classes: a
// config that cannot be read is VM_OPERATION_FAILED + CREATE_NOT_PERFORMED
// (nothing was cloned; retry), never an outcome-unknown Unavailable. A source
// not on node is left to Clone's own read, which reports a gone source
// NotFound.
func (p *Provider) checkCloneSourceOwner(ctx context.Context, node string, vmid int, owner contracts.ObjectIdentity) error {
	if owner.IsZero() {
		return nil
	}
	cfg, err := p.client.GetVMConfig(ctx, node, vmid)
	if stderrors.Is(err, pveapi.ErrVMNotFound) {
		return nil
	}
	if err != nil {
		return p.vmCreateNotPerformed(fmt.Sprintf("cannot read clone source %d to check its owner; nothing was cloned "+
			"(will retry)", vmid), err)
	}
	return p.checkOwnerInConfig(cfg, node, vmid, owner, ownerCheckOpCloneSource)
}

// checkOwnerInConfig decides from cfg, the already-read config of VM vmid on
// node, whether the per-VM call op made for owner may go on (see the rules
// above). A request without an owner always may.
func (p *Provider) checkOwnerInConfig(cfg map[string]interface{}, node string, vmid int, owner contracts.ObjectIdentity, op string) error {
	if owner.IsZero() {
		return nil
	}
	read := readOwnerStamp(configString(cfg, pveConfigKeyDescription))
	pveName := configString(cfg, pveConfigKeyName)
	logArgs := []any{"op", op, "vmid", vmid, "node", node,
		"requester_uid", owner.UID, "requester_namespace", owner.Namespace, "requester_name", owner.Name}
	switch {
	case read.state != "":
		p.logger.Warn("Refusing the call: the VM's owner stamp cannot be relied on",
			append(logArgs, "owner_stamp_state", read.state, "problem", read.problem)...)
		return statusWithReasons(codes.FailedPrecondition, ownerStampUnreadableMessage(op), contracts.OwnerStampUnreadableReason)
	case read.stamp == nil && !unstampedNamedLikeOwner(pveName, vmid, owner):
		p.logger.Warn("Refusing the call: the VM is unstamped and not named like this VirtualMachine (its VMID was reused?)",
			append(logArgs, "vm_name", pveName)...)
		return statusWithReasons(codes.NotFound, vmNotOwnedMessage(op), contracts.VMNotOwnedReason)
	case read.stamp == nil:
		// A VM created before owner stamps, adopted, or cloned without a
		// target UID, named like this VirtualMachine: managed as before.
		p.logger.Debug("The VM carries no owner stamp and is named like this VirtualMachine; performing the call as before",
			logArgs...)
		return nil
	case read.stamp.owner.UID != owner.UID:
		p.logger.Warn("Refusing the call: the VM is stamped with another owner (its VMID was reused, or the requester was re-created)",
			append(logArgs, "owner_uid", read.stamp.owner.UID, "owner_namespace", read.stamp.owner.Namespace,
				"owner_name", read.stamp.owner.Name)...)
		return statusWithReasons(codes.NotFound, vmNotOwnedMessage(op), contracts.VMNotOwnedReason)
	}
	return nil
}
