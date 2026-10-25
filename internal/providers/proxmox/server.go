/*
Copyright 2025.

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
	"encoding/json"
	stderrors "errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/diskutil"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/storage"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/capabilities"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Provider implements the Proxmox VE provider
type Provider struct {
	providerv1.UnimplementedProviderServer
	client       *pveapi.Client
	capabilities *capabilities.Manager
	logger       *slog.Logger

	// ssh is the migration DATA-plane transport to the PVE node (ADR-0006
	// Slice 3). It is nil on a token-only deployment with no SSH credentials or
	// no derivable host; the S3 export/import paths return an actionable error in
	// that case rather than panicking. The control plane (Create/Delete/etc.)
	// never depends on it.
	ssh *sshTransport

	// inflight records the create/clone tasks this provider started for an
	// owner and has not seen end (inflight.go).
	inflight inflightTracker
	// ownerLocks serializes the Creates and Clones of one owner (inflight.go).
	ownerLocks ownerLocks
}

// readCredentialFile reads a credential from a mounted secret file
func readCredentialFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// New creates a new Proxmox provider
func New() *Provider {
	// Get capabilities for Proxmox VE
	caps := GetProviderCapabilities()

	// Create PVE client from environment
	// Support both PVE_* (legacy) and PROVIDER_* (new standard) env vars
	endpoint := os.Getenv("PROVIDER_ENDPOINT")
	if endpoint == "" {
		endpoint = os.Getenv("PVE_ENDPOINT")
	}

	// Read credentials from mounted secret files (primary method)
	tokenID := readCredentialFile("/etc/virtrigaud/credentials/token_id")
	tokenSecret := readCredentialFile("/etc/virtrigaud/credentials/token_secret")
	username := readCredentialFile("/etc/virtrigaud/credentials/username")
	password := readCredentialFile("/etc/virtrigaud/credentials/password")

	// Fallback to environment variables if files don't exist
	if tokenID == "" {
		tokenID = os.Getenv("PROVIDER_TOKEN_ID")
		if tokenID == "" {
			tokenID = os.Getenv("PVE_TOKEN_ID")
		}
	}

	if tokenSecret == "" {
		tokenSecret = os.Getenv("PROVIDER_TOKEN_SECRET")
		if tokenSecret == "" {
			tokenSecret = os.Getenv("PVE_TOKEN_SECRET")
		}
	}

	if username == "" {
		username = os.Getenv("PROVIDER_USERNAME")
		if username == "" {
			username = os.Getenv("PVE_USERNAME")
		}
	}

	if password == "" {
		password = os.Getenv("PROVIDER_PASSWORD")
		if password == "" {
			password = os.Getenv("PVE_PASSWORD")
		}
	}

	insecureSkipVerify := os.Getenv("TLS_INSECURE_SKIP_VERIFY") == "true" || os.Getenv("PVE_INSECURE_SKIP_VERIFY") == "true"

	config := &pveapi.Config{
		Endpoint:           endpoint,
		TokenID:            tokenID,
		TokenSecret:        tokenSecret,
		Username:           username,
		Password:           password,
		InsecureSkipVerify: insecureSkipVerify,
	}

	// Default PVE storage for disk-landing operations (e.g. the migration
	// `qm importdisk` target) when the request/VM config names none.
	defaultStorage := os.Getenv("PROVIDER_DEFAULT_STORAGE")
	if defaultStorage == "" {
		defaultStorage = os.Getenv("PVE_DEFAULT_STORAGE")
	}
	config.DefaultStorage = strings.TrimSpace(defaultStorage)
	if config.DefaultStorage != "" && !pveapi.ValidStorageID(config.DefaultStorage) {
		// Not fatal here (the control plane does not need it): Validate
		// fails with a clear message, and a Create that would use it is
		// refused. The value is not logged: it is not a storage ID.
		slog.Error("PROVIDER_DEFAULT_STORAGE / PVE_DEFAULT_STORAGE is not a valid PVE storage ID; Validate will fail until it is fixed")
	}

	// Parse node selector
	nodeSelector := os.Getenv("PROVIDER_NODE_SELECTOR")
	if nodeSelector == "" {
		nodeSelector = os.Getenv("PVE_NODE_SELECTOR")
	}
	if nodeSelector != "" {
		config.NodeSelector = strings.Split(nodeSelector, ",")
		for i := range config.NodeSelector {
			config.NodeSelector[i] = strings.TrimSpace(config.NodeSelector[i])
		}
	}

	// Parse CA bundle
	caBundle := os.Getenv("PROVIDER_CA_BUNDLE")
	if caBundle == "" {
		caBundle = os.Getenv("PVE_CA_BUNDLE")
	}
	if caBundle != "" {
		config.CABundle = []byte(caBundle)
	}

	client, err := pveapi.NewClient(config)
	if err != nil {
		// Log error but continue - validation will catch connection issues
		slog.Error("Failed to create PVE client", "error", err)
	}

	// Build the migration DATA-plane SSH transport to the PVE node (ADR-0006
	// Slice 3). The SSH host is the SAME host as the API endpoint. Failure to
	// derive a host (e.g. no endpoint set) is non-fatal: the control plane is
	// unaffected, and the S3 export/import paths surface an actionable error if
	// the transport is needed but unavailable.
	sshTransport, sshErr := newSSHTransport(endpoint, slog.Default())
	if sshErr != nil {
		slog.Warn("Migration SSH data-plane transport unavailable (S3 migration disabled until configured)", "error", sshErr)
	}

	return &Provider{
		client:       client,
		capabilities: caps,
		logger:       slog.Default(),
		ssh:          sshTransport,
	}
}

// Validate validates the provider configuration and connectivity
func (p *Provider) Validate(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
	if p.client == nil {
		return &providerv1.ValidateResponse{
			Ok:      false,
			Message: "PVE client not configured",
		}, nil
	}

	// A default storage that is not a PVE storage ID would refuse every Create
	// that uses it: say so now, not at the first Create.
	if _, err := p.defaultStorage(); err != nil {
		p.logger.Error(invalidDefaultStorageMessage)
		return &providerv1.ValidateResponse{
			Ok:      false,
			Message: invalidDefaultStorageMessage,
		}, nil
	}

	// Test connectivity by trying to find a node
	node, err := p.client.FindNode(ctx)
	if err != nil {
		return &providerv1.ValidateResponse{
			Ok:      false,
			Message: fmt.Sprintf("Failed to connect to Proxmox VE: %v", err),
		}, nil
	}

	return &providerv1.ValidateResponse{
		Ok:      true,
		Message: fmt.Sprintf("Proxmox VE provider is ready (node: %s)", node),
	}, nil
}

// Create creates a new virtual machine.
//
// When the request carries an owner (CreateRequest.owner with a UID), the VM
// is stamped with it (vm_ownership.go) by the very request that makes it, and
// a VM the cluster already has with this name and this owner's UID is
// re-bound instead of making a second one: a Create re-sent after an unknown
// outcome returns the VM the first one made, finishing it first if it was
// interrupted. Failures are classified as described in create_errors.go.
func (p *Provider) Create(ctx context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	if p.client == nil {
		return nil, p.createNotPerformed("PVE client not configured; nothing was created", nil)
	}

	owner, err := ownerFromCreateRequest(req)
	if err != nil {
		return nil, err
	}
	unlock, err := p.lockOwner(owner.UID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Parse the request (node discovery and VMID allocation happen here; their
	// failures are already classified).
	vmConfig, node, err := p.parseCreateRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	var stamp *ownerStamp
	if !owner.IsZero() {
		// A task an earlier Create for this owner started and that has not
		// ended makes a VM the owner lookup cannot see yet (inflight.go).
		if err := p.checkInflight(ctx, owner.UID); err != nil {
			return nil, err
		}
		existing, err := p.findOwnedVM(ctx, req.Name, owner)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			resp, recreate, err := p.rebindCreate(ctx, req, vmConfig, existing)
			if err != nil || !recreate {
				return resp, err
			}
		}
		stamp = &ownerStamp{owner: owner, phase: ownerStampPhaseCreating}
	}

	// ADR-0006 Proxmox TARGET: when the request carries a pre-staged imported
	// disk (from a migration's ImportDisk), the create path builds a bare VM shell
	// and attaches the staged disk via `qm importdisk` instead of cloning a
	// template. Handled entirely by createFromImportedDisk and returns early.
	if vmConfig.ImportedDiskPath != "" {
		return p.createFromImportedDisk(ctx, req, vmConfig, node, stamp)
	}

	if vmConfig.Custom == nil {
		vmConfig.Custom = make(map[string]string)
	}

	if vmConfig.Template == "" {
		// A new VM (not from a template): the whole configuration, the owner
		// stamp included (already "ready": nothing follows), is in the one
		// create request.
		if stamp != nil {
			ready := stamp.withPhase(ownerStampPhaseReady)
			vmConfig.Custom[pveConfigKeyDescription] = composeDescription("", &ready)
		}
		if len(req.UserData) > 0 {
			// No template, so no boot disk to follow: the request's or the
			// operator's storage, else the last resort (R33).
			storage, err := p.disklessCloudInitStorage(vmConfig.Storage)
			if err != nil {
				return nil, err
			}
			vmConfig.IDE2 = cloudInitDrive(storage)
		}
		p.logger.Info("Creating new VM", "vmid", vmConfig.VMID)
		p.trackTask(stamp, node, vmConfig.VMID, "")
		taskID, err := p.client.CreateVM(ctx, node, vmConfig)
		if err != nil {
			return nil, p.submitFailed(stamp, vmConfig.VMID, p.classifySubmitError(ctx, "create VM", vmConfig.VMID, stamp, err))
		}
		// The task is not awaited: it stays recorded until a later Create for
		// this owner sees it end.
		p.trackTask(stamp, node, vmConfig.VMID, taskID)
		result := &providerv1.CreateResponse{Id: fmt.Sprintf("%d", vmConfig.VMID)}
		if taskID != "" {
			result.Task = &providerv1.TaskRef{Id: taskID}
		}
		return result, nil
	}

	// Clone from a template.
	templateID, parseErr := strconv.Atoi(vmConfig.Template)
	if parseErr != nil {
		return nil, errors.NewInvalidSpec("invalid template ID '%s': %v", vmConfig.Template, parseErr)
	}
	// The clone inherits the template's description: keep it, minus any owner
	// stamp, and add this VM's own.
	base, sourceHas, err := p.inheritedDescription(ctx, node, templateID)
	if stderrors.Is(err, pveapi.ErrVMNotFound) {
		return nil, errors.NewInvalidSpec("template %d not found on node %s", templateID, node)
	}
	if err != nil {
		return nil, p.vmCreateNotPerformed(fmt.Sprintf("cannot read template %d; nothing was created (will retry)", templateID), err)
	}
	p.warnIfTruncated(vmConfig.VMID, base, stamp)
	if desc := cloneRequestDescription(base, sourceHas, stamp); desc != "" {
		vmConfig.Custom[pveConfigKeyDescription] = desc
	}
	vmConfig.Custom["full"] = "1" // Full clone by default

	p.logger.Info("Cloning VM from template", "template_id", templateID, "new_vmid", vmConfig.VMID)
	p.trackTask(stamp, node, vmConfig.VMID, "")
	taskID, err := p.client.CloneVM(ctx, node, templateID, vmConfig)
	if err != nil {
		return nil, p.submitFailed(stamp, vmConfig.VMID, p.classifySubmitError(ctx, "clone template", vmConfig.VMID, stamp, err))
	}
	p.trackTask(stamp, node, vmConfig.VMID, taskID)
	if err := p.client.WaitForTask(ctx, node, taskID); err != nil {
		if stderrors.Is(err, pveapi.ErrTaskFailed) {
			p.untrackTask(stamp, vmConfig.VMID)
		}
		return nil, p.failAfterTask(ctx, node, vmConfig.VMID, req.Name, stamp, "the template clone task failed", err)
	}
	p.untrackTask(stamp, vmConfig.VMID)
	if err := p.ensureOwnerStamp(ctx, node, vmConfig.VMID, base, stamp); err != nil {
		return nil, p.failAfterSubmit(ctx, node, vmConfig.VMID, req.Name, stamp, true, "stamping the cloned VM with its owner failed", err)
	}
	if err := p.configureClonedVM(ctx, req, vmConfig, node); err != nil {
		return nil, p.failAfterSubmit(ctx, node, vmConfig.VMID, req.Name, stamp, true, "configuring the cloned VM failed", err)
	}
	if stamp != nil {
		ready := stamp.withPhase(ownerStampPhaseReady)
		if err := p.writeOwnerStamp(ctx, node, vmConfig.VMID, base, &ready); err != nil {
			return nil, p.failAfterSubmit(ctx, node, vmConfig.VMID, req.Name, &ready, true, "marking the cloned VM ready failed", err)
		}
	}
	return &providerv1.CreateResponse{Id: fmt.Sprintf("%d", vmConfig.VMID)}, nil
}

// configureClonedVM applies, to a VM cloned from a template, what the clone
// does not carry: the VMClass sizing, the network settings the request names
// (applyCloneNetworks) and the cloud-init settings. Every value
// is set with an idempotent config update, so a re-sent Create that finds its
// VM interrupted runs it again.
func (p *Provider) configureClonedVM(ctx context.Context, req *providerv1.CreateRequest, vmConfig *pveapi.VMConfig, node string) error {
	// Apply the VMClass sizing on top of the cloned template. A PVE clone
	// inherits the template's cores/memory, so they MUST be set explicitly or
	// the VM comes up at the template's size, not the class's (#261 P1-1). This
	// runs for EVERY clone, independent of cloud-init — the cloud-init
	// reconfigure below is gated and would otherwise skip a plain VM. (The
	// diskless create path applies sizing via configToValues at create time.)
	if err := p.applyVMClassSizing(ctx, node, vmConfig.VMID, vmConfig.CPUs, vmConfig.Memory); err != nil {
		return fmt.Errorf("apply VMClass sizing to cloned VM %d: %w", vmConfig.VMID, err)
	}

	// Read the clone's config once: the NICs the request names are merged into
	// the clone's own, and the cloud-init step needs its primary disk and its
	// cloud-init drive and storage. A failed read is an error (the caller rolls
	// back or retries): choosing the drive's storage without it could put the
	// drive on `local` again (R33), and a NIC merged without it would lose the
	// clone's MAC.
	cloudInit := len(req.UserData) > 0 || vmConfig.SSHKeys != ""
	var cfg map[string]interface{}
	if cloudInit || anyNICSpecified(vmConfig.Networks) {
		var err error
		cfg, err = p.client.GetVMConfig(ctx, node, vmConfig.VMID)
		if err != nil {
			return fmt.Errorf("read the config of cloned VM %d: %w", vmConfig.VMID, err)
		}
	}

	// The NICs and IP settings the request names. A NIC it does not name keeps
	// what the template made (bridge, VLAN and the MAC PVE gave the clone); this
	// also runs for a clone with no cloud-init data (R36).
	if err := p.applyCloneNetworks(ctx, node, vmConfig, cfg, cloudInit); err != nil {
		return err
	}

	// After cloning, reconfigure the VM with the cloud-init settings.
	if !cloudInit {
		return nil
	}
	p.logger.Info("Reconfiguring cloned VM with cloud-init", "vmid", vmConfig.VMID)

	// Build reconfiguration values for cloud-init. Sizing (cores/memory) is
	// already applied above for every clone, so it is not repeated here.
	//
	// The cloud-init settings (user-data, SSH keys) reach the guest only
	// through a cloud-init drive, so the clone gets one whenever user-data or
	// SSH keys are set: on the request's storage, else the primary disk's, else the
	// operator's (R33); then what the clone's own cloud-init drive, if the
	// template had one, needs (cloudInitChange keeps the slot the drive is
	// really in).
	reconfigValues := url.Values{}
	storage, err := p.diskfulCloudInitStorage(vmConfig.Storage, cfg, vmConfig.VMID)
	if err != nil {
		return err
	}
	slot, value, set := cloudInitChange(cfg, storage)
	if set {
		reconfigValues.Set(slot, value)
	}
	p.logger.Info("Cloud-init drive", "vmid", vmConfig.VMID, "storage", storage, "slot", slot, "set", set)
	if vmConfig.SSHKeys != "" {
		reconfigValues.Set("sshkeys", vmConfig.SSHKeys)
	}
	if vmConfig.CIUser != "" {
		reconfigValues.Set("ciuser", vmConfig.CIUser)
	}
	// Boot from the boot disk only (cloneBootOrder); a clone whose config
	// shows no disk keeps the boot order the template gave it.
	if boot := cloneBootOrder(cfg); boot != "" {
		reconfigValues.Set(pveConfigKeyBoot, boot)
		p.logger.Info("Setting boot order", "vmid", vmConfig.VMID, "boot", boot)
	} else {
		p.logger.Info("The cloned VM's config shows no disk; its boot order is left as the template made it", "vmid", vmConfig.VMID)
	}

	taskID, err := p.client.ReconfigureVMRaw(ctx, node, vmConfig.VMID, reconfigValues)
	if err != nil {
		return fmt.Errorf("reconfigure cloned VM %d with cloud-init: %w", vmConfig.VMID, err)
	}
	if err := p.client.WaitForTask(ctx, node, taskID); err != nil {
		return fmt.Errorf("reconfigure cloned VM %d with cloud-init: %w", vmConfig.VMID, err)
	}
	return nil
}

// rebindCreate answers a Create that found the VM vm, stamped with this
// request's owner, which an earlier Create for the same VirtualMachine made.
// A "ready" VM is returned as it is. A "creating" one was interrupted: it is
// finished (a template clone: sizing and cloud-init applied again; a plain
// create: nothing to add) and marked ready, or — for a migration's
// imported-disk create, whose steps cannot be re-run on the half-built VM —
// removed (recreate=true), so the caller builds it again from the staged
// disk. A VM a PVE task is still working on (locked) is not touched: the
// answer is retryable and leaves the outcome unknown.
func (p *Provider) rebindCreate(ctx context.Context, req *providerv1.CreateRequest, vmConfig *pveapi.VMConfig, vm *ownedVM) (resp *providerv1.CreateResponse, recreate bool, err error) {
	id := p.vmIDOn(ctx, vm.node, vm.vmid)
	logArgs := []any{"name", req.Name, "vmid", vm.vmid, "node", vm.node, "owner_uid", vm.stamp.owner.UID}
	if vm.stamp.phase == ownerStampPhaseReady {
		p.logger.Info("A VM stamped with this VirtualMachine's UID already exists; treating the create as idempotent", logArgs...)
		return &providerv1.CreateResponse{Id: id}, false, nil
	}
	if vm.lock != "" {
		return nil, false, p.outcomeUnknown(fmt.Sprintf("Proxmox VM %d, made by an earlier Create for this VirtualMachine, "+
			"is still being worked on (lock %q); will retry", vm.vmid, vm.lock), nil)
	}
	if vmConfig.ImportedDiskPath != "" {
		if vm.status != pveStatusStopped {
			return nil, false, p.outcomeUnknown(fmt.Sprintf("Proxmox VM %d, an interrupted imported-disk create for this "+
				"VirtualMachine, is %s, not stopped; not rebuilding it", vm.vmid, vm.status), nil)
		}
		// Remove the half-built VM only when it can be built again (the SSH
		// data plane is configured and the staged disk is still on the node)
		// and no `qm importdisk` of the interrupted create still runs.
		err := p.importStageReady(ctx, vmConfig.ImportedDiskPath)
		if err == nil {
			err = p.importdiskIdle(ctx, vm.vmid)
		}
		if err != nil {
			return nil, false, p.outcomeUnknown(fmt.Sprintf("Proxmox VM %d, an interrupted imported-disk create for this "+
				"VirtualMachine, cannot be rebuilt (%v); not removing it", vm.vmid, err), nil)
		}
		p.logger.Warn("An imported-disk create for this VirtualMachine was interrupted; removing its half-built VM to build it again", logArgs...)
		if err := p.rollbackCreatedVM(ctx, vm.node, vm.vmid, req.Name, &vm.stamp, true); err != nil {
			return nil, false, p.outcomeUnknown(fmt.Sprintf("removing the half-built VM %d of an interrupted imported-disk create failed", vm.vmid), err)
		}
		return nil, true, nil
	}
	p.logger.Info("A VM stamped with this VirtualMachine's UID was left half-created; finishing it", logArgs...)
	if vmConfig.Template != "" {
		finishing := *vmConfig
		finishing.VMID = vm.vmid
		if err := p.configureClonedVM(ctx, req, &finishing, vm.node); err != nil {
			return nil, false, p.outcomeUnknown(fmt.Sprintf("finishing Proxmox VM %d failed", vm.vmid), err)
		}
	}
	ready := vm.stamp.withPhase(ownerStampPhaseReady)
	if err := p.writeOwnerStamp(ctx, vm.node, vm.vmid, vm.description, &ready); err != nil {
		return nil, false, p.outcomeUnknown(fmt.Sprintf("marking Proxmox VM %d ready failed", vm.vmid), err)
	}
	return &providerv1.CreateResponse{Id: id}, false, nil
}

// Delete deletes a virtual machine
func (p *Provider) Delete(ctx context.Context, req *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	vmid, node, err := p.parseVMReference(req.Id)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	// A template is never destroyed; a request that names its owner destroys
	// the VM only when the VM is not stamped with another owner (its VMID
	// reused) and its stamp can be read (checkDelete).
	if err := p.checkDelete(ctx, node, vmid, deleteOwner(req)); err != nil {
		return nil, err
	}

	// Proxmox refuses to destroy a running VM ("VM <id> is running - destroy
	// failed"), so stop it first and wait for the stop to complete — mirroring
	// vSphere (power-off → Destroy) and libvirt (destroyDomain → undefine). A VM
	// that no node of the cluster knows is already deleted, so it is not an
	// error; one only missing from this node is (see below).
	vm, err := p.client.GetVM(ctx, node, vmid)
	if err == nil && vm != nil {
		switch vm.Status {
		case "running", "paused", "suspended":
			stopTask, stopErr := p.client.PowerOperation(ctx, node, vmid, "stop")
			if stopErr != nil {
				return nil, errors.NewInternal("failed to stop VM before delete", stopErr)
			}
			if stopTask != "" {
				if werr := p.client.WaitForTask(ctx, node, stopTask); werr != nil {
					return nil, errors.NewInternal("failed waiting for VM to stop before delete", werr)
				}
			}
		}
	}

	// Destroy with purge=1 so the config, disk volumes, and any backup/replication
	// references are reclaimed (a bare DELETE leaves disks behind). Wait for the
	// destroy task so a successful Delete RPC means the VM is actually gone — the
	// controller removes the finalizer on success, so returning success while the
	// VM still exists would orphan it.
	taskID, err := p.client.DeleteVM(ctx, node, vmid, true)
	if stderrors.Is(err, pveapi.ErrVMNotFound) {
		// Not on this node: already deleted only when no node has the VMID
		// (a VM moved to another node is not deleted, and success would
		// release the VirtualMachine's finalizer).
		gone, goneErr := p.vmGone(ctx, vmid)
		switch {
		case goneErr != nil:
			p.logCause("cannot confirm that the VM is gone", goneErr)
			return nil, errors.NewUnavailable(fmt.Sprintf("Proxmox VM %d is not on node %s and the cluster could not be checked for it; not deleting", vmid, node), nil)
		case !gone:
			return nil, errors.NewUnavailable(fmt.Sprintf("Proxmox VM %d is not on node %s but exists elsewhere in the cluster; not deleting", vmid, node), nil)
		}
		return &providerv1.TaskResponse{}, nil
	}
	if err != nil {
		return nil, errors.NewInternal("failed to delete VM", err)
	}
	if taskID != "" {
		if werr := p.client.WaitForTask(ctx, node, taskID); werr != nil {
			return nil, errors.NewInternal("failed waiting for VM destroy task", werr)
		}
	}

	return &providerv1.TaskResponse{}, nil
}

// Power performs power operations on a virtual machine
func (p *Provider) Power(ctx context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	vmid, node, err := p.parseVMReference(req.Id)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	var operation string
	switch req.Op {
	case providerv1.PowerOp_POWER_OP_ON:
		operation = "start"
	case providerv1.PowerOp_POWER_OP_OFF:
		operation = "stop"
	case providerv1.PowerOp_POWER_OP_REBOOT:
		operation = "reboot"
	case providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL:
		operation = "shutdown" // Proxmox supports graceful shutdown
	default:
		return nil, errors.NewInvalidSpec("unsupported power operation: %v", req.Op)
	}

	// A VM that is not the requester's (its VMID reused) is never started,
	// stopped or rebooted — not even read for the idempotency check below
	// (vm_owner_check.go).
	if err := p.checkVMOwner(ctx, node, vmid, requestOwner(req.GetOwner()), ownerCheckOpPower); err != nil {
		return nil, err
	}

	// Power is idempotent: a VM already in the requested state is success,
	// with no task. PVE fails a start of a running VM ("already running"), and
	// a stop or shutdown of a stopped one may fail too — also when another
	// actor won a race to the same change.
	want := pveRequestedStatus(req.Op)
	if p.pveAlreadyInStatus(ctx, node, vmid, want) {
		return &providerv1.TaskResponse{}, nil
	}

	var taskID string
	if req.Op == providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL && req.GracefulTimeoutSeconds > 0 {
		// Honor the caller's graceful-shutdown deadline and escalate to a hard
		// stop when it elapses (PVE `forceStop`), instead of ignoring the timeout
		// and waiting on the guest indefinitely (#261 P1-4).
		params := url.Values{}
		params.Set("timeout", strconv.Itoa(int(req.GracefulTimeoutSeconds)))
		params.Set("forceStop", "1")
		taskID, err = p.client.PowerOperationWithParams(ctx, node, vmid, operation, params)
	} else {
		taskID, err = p.client.PowerOperation(ctx, node, vmid, operation)
	}
	if err != nil {
		if p.pveAlreadyInStatus(ctx, node, vmid, want) {
			return &providerv1.TaskResponse{}, nil
		}
		return nil, errors.NewInternal("failed to perform power operation", err)
	}

	result := &providerv1.TaskResponse{}
	if taskID != "" {
		result.Task = &providerv1.TaskRef{Id: taskID}
	}

	return result, nil
}

// pveStatusRunning is the status (status/current "status") of a running VM;
// pveStatusStopped (vm_ownership.go) is that of a stopped one.
const pveStatusRunning = "running"

// pveRequestedStatus is the PVE status op leaves the VM in — running for On,
// stopped for Off and ShutdownGraceful — or "" for an op (Reboot) that no
// status satisfies.
func pveRequestedStatus(op providerv1.PowerOp) string {
	switch op {
	case providerv1.PowerOp_POWER_OP_ON:
		return pveStatusRunning
	case providerv1.PowerOp_POWER_OP_OFF, providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL:
		return pveStatusStopped
	default:
		return ""
	}
}

// pveAlreadyInStatus reports whether VM vmid on node is already in status
// want (never for an empty want). A status that cannot be read is not want.
func (p *Provider) pveAlreadyInStatus(ctx context.Context, node string, vmid int, want string) bool {
	if want == "" {
		return false
	}
	vm, err := p.client.GetVM(ctx, node, vmid)
	if err != nil || vm == nil || vm.Status != want {
		return false
	}
	p.logger.Info("Power operation: the VM is already in the requested state", "vmid", vmid, "status", want)
	return true
}

// Reconfigure reconfigures a virtual machine: it sets its CPU count and
// memory and changes no disk — neither a spec.disks size (the Proxmox Create
// attaches none) nor the VMClass disk size — see reconfigure.go. The VM must
// be the requester's (vm_owner_check.go).
func (p *Provider) Reconfigure(ctx context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	vmid, node, err := p.parseVMReference(req.Id)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	// Get current VM configuration to check what can be changed online
	currentConfig, err := p.client.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return nil, errors.NewInternal("failed to get current VM config", err)
	}
	// The same config decides whether the VM is the requester's: one that is
	// not (its VMID reused) is never resized (vm_owner_check.go).
	if err := p.checkOwnerInConfig(currentConfig, node, vmid, requestOwner(req.GetOwner()), ownerCheckOpReconfigure); err != nil {
		return nil, err
	}

	// Apply CPU/memory (reconfigure.go): no size goes to a disk.
	desired, err := parseReconfigureDesired(req.DesiredJson)
	if err != nil {
		return nil, err
	}
	return p.reconfigureVM(ctx, node, vmid, desired)
}

// Describe describes a virtual machine's current state
func (p *Provider) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	vmid, node, err := p.parseVMReference(req.Id)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	// A VM that is not the requester's (its VMID reused) is answered
	// VM_NOT_OWNED, and none of its state is returned (vm_owner_check.go).
	// It is not reported Exists=false: the manager would create the
	// VirtualMachine's VM again, and a VM the provider cannot prove gone (an
	// adopted VM carrying an earlier owner's stamp, a VM renamed in PVE) must
	// not get a second one.
	if err := p.checkVMOwner(ctx, node, vmid, requestOwner(req.GetOwner()), ownerCheckOpDescribe); err != nil {
		return nil, err
	}

	vm, err := p.client.GetVM(ctx, node, vmid)
	if err != nil {
		if stderrors.Is(err, pveapi.ErrVMNotFound) {
			// "Not on this node" is reported as gone only when no node of the
			// cluster has the VMID: a VM moved to another node is not gone, and
			// Exists=false makes the manager create it again.
			gone, goneErr := p.vmGone(ctx, vmid)
			if goneErr != nil {
				p.logCause("cannot confirm that the VM is gone", goneErr)
				return nil, errors.NewUnavailable(fmt.Sprintf("Proxmox VM %d is not on node %s and the cluster could not be checked for it", vmid, node), nil)
			}
			if !gone {
				return nil, errors.NewUnavailable(fmt.Sprintf("Proxmox VM %d is not on node %s but exists elsewhere in the cluster", vmid, node), nil)
			}
			return &providerv1.DescribeResponse{
				Exists:     false,
				PowerState: "notfound",
			}, nil
		}
		return nil, errors.NewInternal("failed to describe VM", err)
	}

	// Convert PVE status to the CRD's power-state enum ("On"/"Off"/"OffGraceful").
	// The manager writes this value straight into VirtualMachine.status.powerState,
	// so it MUST be a valid enum member — a lowercase "on"/"off" or "suspended" is
	// rejected by API validation and stalls the VM (and any migration awaiting it).
	// A suspended/paused guest is memory-resident, so it maps to On.
	powerState := "Off"
	switch vm.Status {
	case "running", "suspended", "paused":
		powerState = "On"
	case "stopped":
		powerState = "Off"
	}

	// Generate console URL
	endpoint := ""
	if p.client != nil && p.client.Config() != nil {
		endpoint = p.client.Config().Endpoint
	}
	consoleURL := fmt.Sprintf("%s/#v1:0:=qemu/%d:4:5:=console",
		strings.TrimSuffix(endpoint, "/api2"), vmid)

	// Extract IP addresses from guest agent
	var ips []string
	if vm.Status == "running" {
		// Try to get IP addresses from QEMU guest agent
		interfaces, err := p.client.GetGuestNetworkInterfaces(ctx, node, vmid)
		if err != nil {
			p.logger.Debug("Failed to get guest network interfaces (guest agent may not be available)", "error", err)
		} else {
			// Extract IP addresses from interfaces
			for _, iface := range interfaces {
				// Skip loopback interface
				if iface.Name == "lo" {
					continue
				}

				for _, ipAddr := range iface.IPAddresses {
					// Filter out link-local addresses
					ip := ipAddr.IPAddress
					if ip != "" && !strings.HasPrefix(ip, "127.") &&
						!strings.HasPrefix(ip, "169.254.") &&
						!strings.HasPrefix(ip, "fe80:") {
						ips = append(ips, ip)
					}
				}
			}
		}
	}

	// Provider-specific details
	providerRaw := map[string]string{
		"node":      vm.Node,
		"vmid":      strconv.Itoa(vm.VMID),
		"status":    vm.Status,
		"qmpstatus": vm.QMPStatus,
	}
	if vm.PID != 0 {
		providerRaw["pid"] = strconv.Itoa(vm.PID)
	}
	if vm.ConfigLock != "" {
		providerRaw["lock"] = vm.ConfigLock
	}

	providerRawJSON, _ := json.Marshal(providerRaw)

	return &providerv1.DescribeResponse{
		Exists:          true,
		PowerState:      powerState,
		Ips:             ips,
		ConsoleUrl:      consoleURL,
		ProviderRawJson: string(providerRawJSON),
	}, nil
}

// TaskStatus checks the status of an async task
func (p *Provider) TaskStatus(ctx context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	// Parse task ID to extract node
	taskID := req.Task.Id
	parts := strings.Split(taskID, ":")
	if len(parts) < 3 {
		return &providerv1.TaskStatusResponse{
			Done:  true,
			Error: fmt.Sprintf("invalid task ID format: %s", taskID),
		}, nil
	}

	node := parts[1]
	task, err := p.client.GetTaskStatus(ctx, node, taskID)
	if err != nil {
		// PVE answered the query with an error of the request itself (a
		// malformed or unknown UPID): it will never report this task, so the
		// task is over, with an error, and the manager stops asking.
		if pveapi.IsTerminalTaskQueryError(err) {
			return &providerv1.TaskStatusResponse{
				Done:  true,
				Error: fmt.Sprintf("PVE does not report this task: %v", err),
			}, nil
		}
		// Any other failure (an API blip, a timeout, the token, a 5xx): the
		// task's outcome is unknown, not failed. Answering Done with an error
		// here made the manager forget a task that was still running. A
		// transport-level answer keeps the task recorded, and the manager asks
		// again.
		return nil, errors.NewUnavailable("Proxmox task status query", err)
	}

	done := task.Status == "stopped"
	errorMsg := ""
	if done && task.ExitCode != nil && *task.ExitCode != "OK" {
		errorMsg = fmt.Sprintf("task failed with exit code: %s", *task.ExitCode)
	}

	return &providerv1.TaskStatusResponse{
		Done:  done,
		Error: errorMsg,
	}, nil
}

// SnapshotCreate creates a VM snapshot
func (p *Provider) SnapshotCreate(ctx context.Context, req *providerv1.SnapshotCreateRequest) (*providerv1.SnapshotCreateResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	vmid, node, err := p.parseVMReference(req.VmId)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	// A VM that is not the requester's (its VMID reused) is never
	// snapshotted (vm_owner_check.go).
	if err := p.checkVMOwner(ctx, node, vmid, requestOwner(req.GetOwner()), ownerCheckOpSnapshotCreate); err != nil {
		return nil, err
	}

	// Generate snapshot name if not provided
	snapName := req.NameHint
	if snapName == "" {
		snapName = fmt.Sprintf("snapshot-%d", time.Now().Unix())
	}

	// Create snapshot
	taskID, err := p.client.CreateSnapshot(ctx, node, vmid, snapName, req.Description, req.IncludeMemory)
	if err != nil {
		return nil, errors.NewInternal("failed to create snapshot", err)
	}

	result := &providerv1.SnapshotCreateResponse{
		SnapshotId: snapName,
	}

	if taskID != "" {
		result.Task = &providerv1.TaskRef{Id: taskID}
	}

	return result, nil
}

// SnapshotDelete deletes a VM snapshot
func (p *Provider) SnapshotDelete(ctx context.Context, req *providerv1.SnapshotDeleteRequest) (*providerv1.TaskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	vmid, _, err := p.parseVMReference(req.VmId)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	// PVE deletes a snapshot in a worker task, which fails ("snapshot ...
	// does not exist") when there is nothing to delete: the manager would
	// then retry forever. A snapshot that is provably not there is answered
	// NotFound up front, the manager's idempotent "already gone": its VM is
	// absent cluster-wide, or the VM's ACTUAL node (an HA move changes it)
	// does not list it. Anything else that cannot be established is an error.
	node, exists, err := p.snapshotVMNode(ctx, vmid)
	if err != nil {
		return nil, errors.NewUnavailable("PVE cluster resources", err)
	}
	if !exists {
		return nil, errors.NewNotFound("snapshot", req.SnapshotId)
	}
	// A VM that is not the requester's (its VMID reused) has none of the
	// requester's snapshots: VM_NOT_OWNED (a NotFound), and none of its
	// snapshots is listed or deleted (vm_owner_check.go).
	if err := p.checkVMOwner(ctx, node, vmid, requestOwner(req.GetOwner()), ownerCheckOpSnapshotDelete); err != nil {
		return nil, err
	}
	present, err := p.snapshotPresent(ctx, node, vmid, req.SnapshotId)
	if err != nil {
		return nil, errors.NewUnavailable("PVE snapshot list", err)
	}
	if !present {
		return nil, errors.NewNotFound("snapshot", req.SnapshotId)
	}

	taskID, err := p.client.DeleteSnapshot(ctx, node, vmid, req.SnapshotId)
	if stderrors.Is(err, pveapi.ErrSnapshotNotExist) {
		return nil, errors.NewNotFound("snapshot", req.SnapshotId)
	}
	if err != nil {
		return nil, errors.NewInternal("failed to delete snapshot", err)
	}

	result := &providerv1.TaskResponse{}
	if taskID != "" {
		result.Task = &providerv1.TaskRef{Id: taskID}
	}

	return result, nil
}

// snapshotPresent reports whether VM vmid on node (its actual node,
// snapshotVMNode) has the snapshot name. PVE lists a "current" pseudo-entry
// for the running state, which is never a snapshot.
func (p *Provider) snapshotPresent(ctx context.Context, node string, vmid int, name string) (bool, error) {
	list, err := p.client.ListSnapshots(ctx, node, vmid)
	if err != nil {
		return false, err
	}
	for _, s := range list {
		if s != nil && s.Name == name && name != "current" {
			return true, nil
		}
	}
	return false, nil
}

// SnapshotRevert reverts a VM to a snapshot
func (p *Provider) SnapshotRevert(ctx context.Context, req *providerv1.SnapshotRevertRequest) (*providerv1.TaskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	vmid, node, err := p.parseVMReference(req.VmId)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	// A VM that is not the requester's (its VMID reused) is never reverted
	// (vm_owner_check.go).
	if err := p.checkVMOwner(ctx, node, vmid, requestOwner(req.GetOwner()), ownerCheckOpSnapshotRevert); err != nil {
		return nil, err
	}

	taskID, err := p.client.RevertSnapshot(ctx, node, vmid, req.SnapshotId)
	if err != nil {
		return nil, errors.NewInternal("failed to revert snapshot", err)
	}

	result := &providerv1.TaskResponse{}
	if taskID != "" {
		result.Task = &providerv1.TaskRef{Id: taskID}
	}

	return result, nil
}

// allocateVMID allocates the VMID of a VM Create or Clone is about to make,
// from the cluster (/cluster/nextid). Unlike nextVMID it never falls back to a
// time-derived id: a failure is answered VM_OPERATION_FAILED (nothing was
// created; retry), so a VM is only ever created under a VMID PVE handed out.
func (p *Provider) allocateVMID(ctx context.Context) (int, error) {
	id, err := p.client.GetNextVMID(ctx)
	if err != nil {
		return 0, p.createNotPerformed("cannot allocate a VMID from /cluster/nextid; nothing was created (will retry)", err)
	}
	return id, nil
}

// nextVMID allocates a VMID from the cluster (/cluster/nextid). On error it falls
// back to a time-derived id (logging a warning) so the legacy disk import does
// not hard-fail when the endpoint is unavailable — but the API path is
// preferred because a purely time-based id collides when two VMs are created
// in the same second (#261 P2-2). Create and Clone use allocateVMID.
func (p *Provider) nextVMID(ctx context.Context) int {
	id, err := p.client.GetNextVMID(ctx)
	if err != nil {
		p.logger.Warn("Failed to allocate VMID via /cluster/nextid; falling back to a time-based id", "error", err)
		return int(time.Now().Unix()%999999) + 100000
	}
	return id
}

// parseClassSizing extracts the CPU count and memory (MiB) of a Clone's
// VMClass override. The VMClone controller sends a marshaled v1beta1
// VMClassSpec (VMCloneReconciler.classJSON, clonePlacementAdmission.
// cloneClassJSON): `cpu` and `memory`, a resource.Quantity such as "8Gi" —
// not the contracts.VMClass "MemoryMiB" a Create carries. Reading only
// "MemoryMiB" applied the class's CPU (encoding/json matches "cpu" to "CPU"
// case-insensitively) and silently dropped its memory. A contracts.VMClass
// body ("CPU", "MemoryMiB") is still read, so either encoding sizes the clone.
// Returns zeros when the JSON is empty or unparseable (the clone keeps the
// source's size).
func parseClassSizing(classJSON string) (cpus int, memoryMiB int64) {
	if classJSON == "" {
		return 0, 0
	}
	var class struct {
		CPU       int32              `json:"cpu"`
		Memory    *resource.Quantity `json:"memory"`
		MemoryMiB int32              `json:"MemoryMiB"`
	}
	if err := json.Unmarshal([]byte(classJSON), &class); err != nil {
		return 0, 0
	}
	memoryMiB = int64(class.MemoryMiB)
	if class.Memory != nil && class.Memory.Value() > 0 {
		memoryMiB = class.Memory.Value() / bytesPerMiB
	}
	return int(class.CPU), memoryMiB
}

// bytesPerMiB converts a resource.Quantity's bytes to MiB, the unit of the
// PVE `memory` setting.
const bytesPerMiB = 1024 * 1024

// applyVMClassSizing reconfigures a VM to the VMClass CPU/memory. It is used
// after a clone (which inherits the source/template hardware): the VMClass CPU
// is the total vCPU count, so sockets is pinned to 1 and cores carries the whole
// count (PVE computes vCPUs as sockets × cores), and MemoryMiB maps straight to
// the PVE `memory` field. A zero count or memory is left untouched.
func (p *Provider) applyVMClassSizing(ctx context.Context, node string, vmid, cpus int, memoryMiB int64) error {
	vals := url.Values{}
	if cpus > 0 {
		vals.Set("cores", strconv.Itoa(cpus))
		vals.Set("sockets", "1")
	}
	if memoryMiB > 0 {
		vals.Set("memory", strconv.FormatInt(memoryMiB, 10))
	}
	if len(vals) == 0 {
		return nil
	}
	task, err := p.client.ReconfigureVMRaw(ctx, node, vmid, vals)
	if err != nil {
		return err
	}
	if task != "" {
		return p.client.WaitForTask(ctx, node, task)
	}
	return nil
}

// Clone clones a VM. When the request names the target VirtualMachine with a
// UID (CloneRequest.target_vm.uid), the clone is stamped with it and a VM
// named target_name already stamped with that UID is returned instead of a
// second clone (finished first if it was interrupted). Without a UID the
// clone is unstamped: it never inherits the source's stamp. Failures are
// classified as for Create (create_errors.go).
//
// Cloud-init identity (R26): unlike libvirt and vSphere, the provider does
// not rewrite a clone's cloud-init data, because PVE regenerates it. The
// clone is named target_name, PVE's clone always gives every netN a new MAC
// address, and PVE rebuilds the cloud-init drive from the VM's own config at
// each start: its user-data carries `hostname: <VM name>` and, without a
// `cicustom` meta snippet (the provider never sets one), its meta-data
// instance-id is the SHA-1 of that user-data and the network data — which
// includes the MAC addresses — so a clone boots as a new cloud-init instance
// with its own hostname. Exceptions: a source configured out of band with a
// `cicustom` meta-data snippet keeps that snippet's fixed instance-id on its
// clones; and with `citype: configdrive2` the network data (ENI) lists no MAC
// addresses, so a clone whose PVE name equals its source's (the same
// VirtualMachine name in another namespace) gets the source's instance-id.
func (p *Provider) Clone(ctx context.Context, req *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	if p.client == nil {
		return nil, p.createNotPerformed("PVE client not configured; nothing was cloned", nil)
	}

	owner, err := cloneTargetOwner(req)
	if err != nil {
		return nil, err
	}
	unlock, err := p.lockOwner(owner.UID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	sourceVMID, sourceNode, err := p.parseVMReference(req.SourceVmId)
	if err != nil {
		if _, convErr := strconv.Atoi(req.SourceVmId); convErr == nil {
			// A bare VMID: only the node discovery can have failed.
			return nil, p.createNotPerformed("cannot find the PVE node of the clone source; nothing was cloned (will retry)", err)
		}
		return nil, errors.NewInvalidSpec("invalid source VM reference: %v", err)
	}

	var stamp *ownerStamp
	if !owner.IsZero() {
		if err := p.checkInflight(ctx, owner.UID); err != nil {
			return nil, err
		}
		existing, err := p.findOwnedVM(ctx, req.TargetName, owner)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return p.rebindClone(ctx, req, existing)
		}
		stamp = &ownerStamp{owner: owner, phase: ownerStampPhaseCreating}
	}

	// A source VM that is not the requester's source VirtualMachine's (its
	// VMID reused) is never cloned: its disks would be copied into a VM the
	// requester owns (vm_owner_check.go). Checked after the re-bind above,
	// which never reads the source: a retried Clone whose target already
	// exists is answered with it even if the source's VMID was reused since.
	if err := p.checkCloneSourceOwner(ctx, sourceNode, sourceVMID, requestOwner(req.GetSourceOwner())); err != nil {
		return nil, err
	}

	// Generate new VMID for clone
	targetVMID, err := p.allocateVMID(ctx)
	if err != nil {
		return nil, err
	}

	config := &pveapi.VMConfig{
		VMID:   targetVMID,
		Name:   req.TargetName,
		Custom: map[string]string{},
	}
	if req.Linked {
		config.Custom["full"] = "0"
	} else {
		config.Custom["full"] = "1"
	}

	// Honor placement overrides (#261 P1-2): the target node and storage. The
	// clone request is issued to the source node; PVE's `target` parameter places
	// the result on another node, so the post-clone sizing must use that node.
	targetNode := sourceNode
	if req.PlacementJson != "" {
		var placement struct {
			Host      string `json:"Host"`
			Datastore string `json:"Datastore"`
		}
		if err := json.Unmarshal([]byte(req.PlacementJson), &placement); err == nil {
			if placement.Host != "" {
				config.Custom["target"] = placement.Host
				targetNode = placement.Host
			}
			if placement.Datastore != "" {
				config.Storage = placement.Datastore
			}
		}
	}

	// The clone inherits the source's description: keep it, minus the source's
	// owner stamp, plus the clone's own (none without a target UID).
	base, sourceHas, err := p.inheritedDescription(ctx, sourceNode, sourceVMID)
	if stderrors.Is(err, pveapi.ErrVMNotFound) {
		if gone, goneErr := p.vmGone(ctx, sourceVMID); goneErr == nil && gone {
			return nil, errors.NewNotFound("VM", req.SourceVmId)
		}
		err = fmt.Errorf("clone source %d is not on node %s but may exist elsewhere in the cluster", sourceVMID, sourceNode)
	}
	if err != nil {
		return nil, p.vmCreateNotPerformed(fmt.Sprintf("cannot read clone source %d; nothing was cloned (will retry)", sourceVMID), err)
	}
	p.warnIfTruncated(targetVMID, base, stamp)
	if desc := cloneRequestDescription(base, sourceHas, stamp); desc != "" {
		config.Custom[pveConfigKeyDescription] = desc
	}

	p.trackTask(stamp, sourceNode, targetVMID, "")
	cloneTask, err := p.client.CloneVM(ctx, sourceNode, sourceVMID, config)
	if err != nil {
		return nil, p.submitFailed(stamp, targetVMID, p.classifySubmitError(ctx, "clone VM", targetVMID, stamp, err))
	}
	p.trackTask(stamp, sourceNode, targetVMID, cloneTask)
	if werr := p.client.WaitForTask(ctx, sourceNode, cloneTask); werr != nil {
		if stderrors.Is(werr, pveapi.ErrTaskFailed) {
			p.untrackTask(stamp, targetVMID)
		}
		return nil, p.failAfterTask(ctx, targetNode, targetVMID, req.TargetName, stamp, "the clone task failed", werr)
	}
	p.untrackTask(stamp, targetVMID)
	if err := p.ensureOwnerStamp(ctx, targetNode, targetVMID, base, stamp); err != nil {
		return nil, p.failAfterSubmit(ctx, targetNode, targetVMID, req.TargetName, stamp, true, "stamping the clone failed", err)
	}

	// Apply the VMClass sizing override (#261 P1-2): a clone inherits the source's
	// cores/memory, so the requested class size must be set explicitly or the
	// override is silently ignored.
	cpus, memMiB := parseClassSizing(req.ClassJson)
	if err := p.applyVMClassSizing(ctx, targetNode, targetVMID, cpus, memMiB); err != nil {
		return nil, p.failAfterSubmit(ctx, targetNode, targetVMID, req.TargetName, stamp, true, "applying VMClass sizing to the clone failed", err)
	}
	if stamp != nil {
		ready := stamp.withPhase(ownerStampPhaseReady)
		if err := p.writeOwnerStamp(ctx, targetNode, targetVMID, base, &ready); err != nil {
			return nil, p.failAfterSubmit(ctx, targetNode, targetVMID, req.TargetName, &ready, true, "marking the clone ready failed", err)
		}
	}

	return &providerv1.CloneResponse{
		TargetVmId: fmt.Sprintf("%d", targetVMID),
	}, nil
}

// rebindClone answers a Clone that found vm, a VM named target_name stamped
// with the target VirtualMachine's UID, which an earlier Clone for it made: a
// "ready" clone is returned; an interrupted ("creating") one gets the VMClass
// sizing again and is marked ready; a locked one is left alone (retryable,
// outcome unknown).
func (p *Provider) rebindClone(ctx context.Context, req *providerv1.CloneRequest, vm *ownedVM) (*providerv1.CloneResponse, error) {
	id := p.vmIDOn(ctx, vm.node, vm.vmid)
	if vm.stamp.phase == ownerStampPhaseReady {
		p.logger.Info("A clone stamped with the target VirtualMachine's UID already exists; treating the clone as idempotent",
			"name", req.TargetName, "vmid", vm.vmid, "node", vm.node)
		return &providerv1.CloneResponse{TargetVmId: id}, nil
	}
	if vm.lock != "" {
		return nil, p.outcomeUnknown(fmt.Sprintf("Proxmox VM %d, made by an earlier Clone for this VirtualMachine, "+
			"is still being worked on (lock %q); will retry", vm.vmid, vm.lock), nil)
	}
	cpus, memMiB := parseClassSizing(req.ClassJson)
	if err := p.applyVMClassSizing(ctx, vm.node, vm.vmid, cpus, memMiB); err != nil {
		return nil, p.outcomeUnknown(fmt.Sprintf("finishing clone %d failed", vm.vmid), err)
	}
	ready := vm.stamp.withPhase(ownerStampPhaseReady)
	if err := p.writeOwnerStamp(ctx, vm.node, vm.vmid, vm.description, &ready); err != nil {
		return nil, p.outcomeUnknown(fmt.Sprintf("marking clone %d ready failed", vm.vmid), err)
	}
	return &providerv1.CloneResponse{TargetVmId: id}, nil
}

// ImagePrepare is implemented in image.go (structured source parsing, target_name
// handling, idempotency, and the verify-vs-import precedence).

// GetCapabilities returns the provider's capabilities
func (p *Provider) GetCapabilities(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return p.capabilities.GetCapabilities(ctx, req)
}

// Helper methods

// parseCreateRequest parses the gRPC create request into PVE API format
func (p *Provider) parseCreateRequest(ctx context.Context, req *providerv1.CreateRequest) (*pveapi.VMConfig, string, error) {
	// Allocate the VMID from the cluster. Its failure, like the node
	// discovery's below, is answered VM_OPERATION_FAILED: nothing was created.
	vmid, err := p.allocateVMID(ctx)
	if err != nil {
		return nil, "", err
	}

	config := &pveapi.VMConfig{
		VMID: vmid,
		Name: req.Name,
	}

	// Parse VMClass for CPU/memory. ClassJson is a marshaled contracts.VMClass,
	// whose Go fields have NO json tags, so the keys are "CPU" and "MemoryMiB"
	// (NOT "cpus"/"memory" — reading those left every Proxmox VM at the PVE
	// default of 1 core / 512 MB regardless of the VMClass; #261 P1-1). This
	// mirrors the vSphere/libvirt parse of the same contract. MemoryMiB is already
	// in MiB, which is exactly what the PVE `memory` config field expects.
	if req.ClassJson != "" {
		var class struct {
			CPU       int32 `json:"CPU"`
			MemoryMiB int32 `json:"MemoryMiB"`
		}
		if err := json.Unmarshal([]byte(req.ClassJson), &class); err == nil {
			if class.CPU > 0 {
				config.CPUs = int(class.CPU)
			}
			if class.MemoryMiB > 0 {
				config.Memory = int64(class.MemoryMiB)
			}
		}
	}

	// Parse VMImage for template
	// The controller sends contracts.VMImage which has the template in TemplateName field
	if req.ImageJson != "" {
		// First try parsing as contracts.VMImage (sent by controller)
		var contractsImage map[string]interface{}
		if err := json.Unmarshal([]byte(req.ImageJson), &contractsImage); err == nil {
			// Check for TemplateName field (from contracts.VMImage - note capital T)
			if templateName, ok := contractsImage["TemplateName"].(string); ok && templateName != "" {
				config.Template = templateName
				p.logger.Info("Parsed template from contracts.VMImage", "template", templateName)
			}

			// ADR-0006 Proxmox TARGET: a migration's imported disk arrives as a
			// contracts.VMImage with a node-side Path (set by ImportDisk and
			// threaded through VirtualMachine.Spec.ImportedDisk.Path) and no
			// TemplateName. Capturing it here switches Create onto the
			// importdisk-attach path. The Format is the disk format `qm
			// importdisk --format` writes into the VM's storage (the TARGET
			// format; PVE still probes the stage's own format).
			//
			// The path and format are tenant-writable: the path is taken only
			// when the manager marked it as this VM's own verified migration
			// disk (ImportedDisk) and it is exactly this VM's import stage, and
			// the format only when it is one of importStageFormats
			// (import_stage.go); anything else is refused before anything is
			// created.
			if path, ok := contractsImage["Path"].(string); ok && strings.TrimSpace(path) != "" && config.Template == "" {
				imported, _ := contractsImage["ImportedDisk"].(bool)
				owner := contracts.ObjectIdentity{Namespace: req.GetOwner().GetNamespace(), Name: req.GetOwner().GetName()}
				if err := checkImportedDiskPath(path, imported, owner, req.Name); err != nil {
					return nil, "", err
				}
				format, err := importedDiskFormat(contractsImage["Format"])
				if err != nil {
					return nil, "", err
				}
				config.ImportedDiskPath = path
				config.ImportedDiskFormat = format
				p.logger.Info("Parsed imported-disk path from contracts.VMImage (migration import)",
					"path", config.ImportedDiskPath, "format", config.ImportedDiskFormat)
			}

			// Check for storage hint
			if storage, ok := contractsImage["storage"].(string); ok && storage != "" {
				config.Storage = storage
			}
		}

		// A VMImageSpec's source.proxmox.storage is a storage hint even when the
		// template is already named (R33): it picks the cloud-init drive's
		// storage, and is the full clone's target storage, as documented
		// ("the Proxmox storage for cloning"). A hint from the contracts.VMImage
		// above ("storage") wins. The block below reads it for the no-template
		// case, along with the template.
		if config.Template != "" && config.Storage == "" {
			var imageSpec v1beta1.VMImageSpec
			if err := json.Unmarshal([]byte(req.ImageJson), &imageSpec); err == nil && imageSpec.Source.Proxmox != nil {
				config.Storage = imageSpec.Source.Proxmox.Storage
			}
		}

		// Fallback: try to parse as VMImageSpec for backwards compatibility
		if config.Template == "" {
			var imageSpec v1beta1.VMImageSpec
			if err := json.Unmarshal([]byte(req.ImageJson), &imageSpec); err == nil {
				// Check for Proxmox-specific image source
				if imageSpec.Source.Proxmox != nil {
					proxmoxSource := imageSpec.Source.Proxmox

					// Set template ID or name
					if proxmoxSource.TemplateID != nil {
						config.Template = fmt.Sprintf("%d", *proxmoxSource.TemplateID)
					} else if proxmoxSource.TemplateName != "" {
						config.Template = proxmoxSource.TemplateName
					}

					// Set storage if specified
					if proxmoxSource.Storage != "" {
						config.Storage = proxmoxSource.Storage
					}

					// Set node if specified (for template location)
					if proxmoxSource.Node != "" {
						// Store node hint for later use
						if config.Custom == nil {
							config.Custom = make(map[string]string)
						}
						config.Custom["template_node"] = proxmoxSource.Node
					}

					// Set clone type (full vs linked)
					if proxmoxSource.FullClone != nil && !*proxmoxSource.FullClone {
						if config.Custom == nil {
							config.Custom = make(map[string]string)
						}
						config.Custom["full_clone"] = "0" // Linked clone
					}
				}
			}
		}
	}

	// Parse Networks configuration (networks.go). A bad attachment is refused
	// before anything is created.
	networks, ipConfigs, err := parseNetworks(req.NetworksJson, config.Template != "")
	if err != nil {
		return nil, "", err
	}
	config.Networks = networks
	config.IPConfigs = ipConfigs

	// Configure cloud-init if user data provided
	if len(req.UserData) > 0 {
		// The cloud-init drive (ide2) needs a "<storage>:cloudinit" value, and
		// the storage must hold VM images. It is chosen where the VM exists to
		// ask (diskfulCloudInitStorage for a template clone, whose primary
		// disk's storage it can follow; Create's diskless branch otherwise),
		// never defaulted to PVE's `local` here: on a stock install that
		// storage has no `images` content and PVE refuses to start the VM
		// (R33). The storages named for the VM are checked now, before
		// anything is created, as PVE storage IDs: they end up in a config
		// value.
		if err := checkRequestStorage(config.Storage); err != nil {
			return nil, "", err
		}
		if _, err := p.defaultStorage(); err != nil {
			return nil, "", err
		}

		// Extract SSH keys and user from cloud-init data if possible
		userData := string(req.UserData)
		if strings.Contains(userData, "ssh_authorized_keys:") {
			// Try to extract SSH keys from YAML
			lines := strings.Split(userData, "\n")
			var sshKeys []string
			inKeys := false
			for _, line := range lines {
				if strings.Contains(line, "ssh_authorized_keys:") {
					inKeys = true
					continue
				}
				if inKeys && strings.HasPrefix(strings.TrimSpace(line), "- ") {
					key := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
					key = strings.Trim(key, "\"'")
					// Extra safety: ensure no trailing/leading whitespace including newlines
					key = strings.TrimSpace(key)
					if key != "" {
						sshKeys = append(sshKeys, key)
					}
				} else if inKeys && !strings.HasPrefix(strings.TrimSpace(line), " ") {
					inKeys = false
				}
			}
			if len(sshKeys) > 0 {
				// Join multiple keys with newline separator (no trailing newline)
				// Then trim again to be absolutely sure
				config.SSHKeys = strings.TrimSpace(strings.Join(sshKeys, "\n"))
			}
		}

		// Extract username
		if strings.Contains(userData, "name:") {
			lines := strings.Split(userData, "\n")
			for _, line := range lines {
				if strings.Contains(line, "name:") && !strings.Contains(line, "hostname:") {
					parts := strings.Split(line, ":")
					if len(parts) >= 2 {
						username := strings.TrimSpace(parts[1])
						username = strings.Trim(username, "\"' ")
						if username != "" {
							config.CIUser = username
						}
					}
					break
				}
			}
		}
	}

	// Find appropriate node
	node, err := p.client.FindNode(ctx)
	if err != nil {
		return nil, "", p.createNotPerformed("cannot find the PVE node to create the VM on; nothing was created (will retry)", err)
	}

	return config, node, nil
}

// parseVMReference parses a VM reference (ID) into VMID and node
func (p *Provider) parseVMReference(ref string) (int, string, error) {
	// Try to parse as simple VMID first
	if vmid, err := strconv.Atoi(ref); err == nil {
		// Find node for this VMID
		node, err := p.client.FindNode(context.Background())
		if err != nil {
			return 0, "", fmt.Errorf("failed to find node: %w", err)
		}
		return vmid, node, nil
	}

	// Try to parse as node:vmid format
	parts := strings.Split(ref, ":")
	if len(parts) == 2 {
		vmid, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, "", fmt.Errorf("invalid VMID in reference: %s", parts[1])
		}
		return vmid, parts[0], nil
	}

	return 0, "", fmt.Errorf("invalid VM reference format: %s", ref)
}

// GetDiskInfo retrieves detailed information about a VM's disk
func (p *Provider) GetDiskInfo(ctx context.Context, req *providerv1.GetDiskInfoRequest) (*providerv1.GetDiskInfoResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	p.logger.Info("Getting disk info", "vm_id", req.VmId)

	// Parse VM reference
	vmid, node, err := p.parseVMReference(req.VmId)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	// Get VM configuration
	config, err := p.client.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return nil, errors.NewInternal("failed to get VM config", err)
	}
	// The same config decides whether the VM is the requester's: the disks of
	// one that is not (its VMID reused) are never described (vm_owner_check.go).
	if err := p.checkOwnerInConfig(config, node, vmid, requestOwner(req.GetOwner()), ownerCheckOpGetDiskInfo); err != nil {
		return nil, err
	}

	// Get VM info for additional details
	vmInfo, err := p.client.GetVM(ctx, node, vmid)
	if err != nil {
		p.logger.Warn("Failed to get VM info", "error", err)
	}

	// Find primary disk (scsi0, virtio0, sata0, or ide0)
	diskID := "scsi0" // default
	if req.DiskId != "" {
		diskID = req.DiskId
	}

	// Get disk config
	var diskInfo string
	var found bool
	if val, ok := config[diskID]; ok {
		if strVal, ok := val.(string); ok {
			diskInfo = strVal
			found = true
		}
	}

	if !found {
		return nil, errors.NewNotFound("disk %s not found", diskID)
	}

	// Parse disk info (format: "local-lvm:vm-100-disk-0,size=32G")
	parts := strings.Split(diskInfo, ",")
	var storagePath, size string
	if len(parts) > 0 {
		storagePath = parts[0]
	}
	for _, part := range parts {
		if strings.HasPrefix(part, "size=") {
			size = strings.TrimPrefix(part, "size=")
		}
	}

	// Parse size to bytes
	virtualSizeBytes, err := p.parseDiskSize(size)
	if err != nil {
		p.logger.Warn("Failed to parse disk size", "size", size, "error", err)
		virtualSizeBytes = 0
	}

	_ = vmInfo // reserved for future enrichment

	// Start from config-derived guesses, then prefer the real size/used/format
	// from the storage volume when the storage API is reachable (#261 P2-1). The
	// config string only carries the provisioned size and no thin usage, so the
	// guesses are a fallback, not the source of truth.
	actualSizeBytes := virtualSizeBytes
	format := "qcow2" // default for Proxmox
	if strings.Contains(storagePath, "raw") || strings.Contains(diskInfo, "raw") {
		format = "raw"
	}
	if storage, _, ok := strings.Cut(storagePath, ":"); ok && storage != "" {
		if vols, cerr := p.client.GetStorageContent(ctx, node, storage); cerr == nil {
			for _, v := range vols {
				if v.VolID != storagePath {
					continue
				}
				if v.Format != "" {
					format = v.Format
				}
				if v.Size > 0 {
					virtualSizeBytes = v.Size
				}
				if v.Used > 0 {
					actualSizeBytes = v.Used
				} else {
					actualSizeBytes = virtualSizeBytes
				}
				break
			}
		} else {
			p.logger.Warn("Failed to query storage content for disk info; using config-derived values",
				"storage", storage, "error", cerr)
		}
	}

	// Get snapshots
	snapshotList, err := p.client.ListSnapshots(ctx, node, vmid)
	if err != nil {
		p.logger.Warn("Failed to list snapshots", "error", err)
		snapshotList = []*pveapi.Snapshot{}
	}

	// Convert snapshots to string list
	snapshots := make([]string, 0, len(snapshotList))
	for _, snap := range snapshotList {
		snapshots = append(snapshots, snap.Name)
	}

	response := &providerv1.GetDiskInfoResponse{
		DiskId:           diskID,
		Format:           format,
		VirtualSizeBytes: virtualSizeBytes,
		ActualSizeBytes:  actualSizeBytes,
		Path:             storagePath,
		IsBootable:       true, // Primary disk is always bootable
		Snapshots:        snapshots,
		BackingFile:      "",
		Metadata: map[string]string{
			"node":    node,
			"vmid":    fmt.Sprintf("%d", vmid),
			"storage": storagePath,
		},
	}

	// Every disk of the VM with its provisioned size, for the manager's
	// disk-size limit check before a clone.
	response.Disks = p.configDiskCapacities(config)

	p.logger.Info("Disk info retrieved", "disk_id", diskID, "format", format, "size_bytes", virtualSizeBytes)
	return response, nil
}

// pveBusDiskKey matches the VM config keys of attached bus disks (scsi0,
// virtio1, sata2, ide3); "unusedN" (detached) and EFI/TPM state volumes are
// not bus disks.
var pveBusDiskKey = regexp.MustCompile(`^(scsi|virtio|sata|ide)[0-9]+$`)

// configDiskCapacities lists every attached disk in a VM config with the
// provisioned size its "size=" option names. CD-ROM media (including the
// cloud-init drive) are skipped; a disk whose size is missing or does not
// parse is listed with size 0 (unknown).
func (p *Provider) configDiskCapacities(config map[string]interface{}) []*providerv1.DiskCapacity {
	keys := make([]string, 0, len(config))
	for k := range config {
		if pveBusDiskKey.MatchString(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []*providerv1.DiskCapacity
	for _, k := range keys {
		value, ok := config[k].(string)
		if !ok {
			continue
		}
		parts := strings.Split(value, ",")
		if slices.Contains(parts, pveMediaCDROM) || strings.Contains(parts[0], "cloudinit") {
			continue
		}
		c := &providerv1.DiskCapacity{DiskId: k, Path: parts[0]}
		for _, part := range parts[1:] {
			if size, found := strings.CutPrefix(part, "size="); found {
				if b, err := p.parseDiskSize(size); err == nil && b > 0 {
					c.VirtualSizeBytes = b
				}
			}
		}
		out = append(out, c)
	}
	return out
}

// exportNeedsConversion decides whether the export path must run a qemu-img
// convert pass over the downloaded disk before upload.
//
// A pass is required when:
//   - the requested target format differs from the source format (a genuine
//     format conversion), OR
//   - compression is requested AND the target format is qcow2. qemu-img applies
//     internal compression (`-c`) only during a convert pass, so even a
//     qcow2 -> qcow2 export must be forced through a pass to honor Compress.
//
// Compression is meaningful only for qcow2 in this provider. raw has no
// container to compress into, and vmdk stream-optimized compression is not
// produced on the Proxmox export path today; for those targets a Compress=true
// request does not force a pass and produces uncompressed output. The advertised
// ExportCompression capability is therefore honest specifically for qcow2.
func exportNeedsConversion(sourceFormat, targetFormat string, compress bool) bool {
	if targetFormat != sourceFormat {
		return true
	}
	return compress && targetFormat == "qcow2"
}

// ExportDisk exports a VM disk for migration
func (p *Provider) ExportDisk(ctx context.Context, req *providerv1.ExportDiskRequest) (*providerv1.ExportDiskResponse, error) {
	// ADR-0006: Proxmox disks live on the PVE node, so the pod-mounted pvc path can
	// never work; accept only the S3 relay and the NFS qemu-img paths (both run
	// node-side) and reject pvc/unknown backends honestly (Slice 4).
	if err := migration.EnsureS3OrNFSBackend(req.BackendType); err != nil {
		return nil, err
	}
	// Proxmox's S3 path advertises relay-only transfer (RelayOnlyTransferModes);
	// reject a `direct` request loudly instead of silently running it as relay
	// (#261 P1-3, ADR-0006 D2). NFS is host/node-side by nature (the node's
	// qemu-img writes nfs:// directly), so it is exempt from the relay check.
	if req.BackendType != migration.BackendNFS {
		if err := migration.EnsureRelayMode(req.TransferMode); err != nil {
			return nil, err
		}
	}

	// The disk of a VM that is not the requester's (its VMID reused) is never
	// read or copied (vm_owner_check.go). Checked before every backend path.
	if err := p.checkRequestVMOwner(ctx, req.VmId, requestOwner(req.GetOwner()), ownerCheckOpExportDisk); err != nil {
		return nil, err
	}

	// ADR-0006 Slice 4 Proxmox SOURCE path: the node's qemu-img writes the
	// flattened qcow2 straight to the NFS export (libnfs), no pod hop.
	if req.BackendType == migration.BackendNFS {
		return p.exportDiskToNFS(ctx, req)
	}

	// ADR-0006 Proxmox SOURCE path: stream the node disk to S3 over SSH.
	if req.BackendType == migration.BackendS3 {
		return p.exportDiskToS3(ctx, req)
	}

	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	p.logger.Info("Exporting disk", "vm_id", req.VmId, "destination", req.DestinationUrl)

	// Parse VM reference
	vmid, node, err := p.parseVMReference(req.VmId)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	// Get disk information first
	diskInfo, err := p.GetDiskInfo(ctx, &providerv1.GetDiskInfoRequest{
		VmId:       req.VmId,
		DiskId:     req.DiskId,
		SnapshotId: req.SnapshotId,
		// Owner-checked on the very config that yields the volume copied
		// below, not only by ExportDisk's pre-check (vm_owner_check.go).
		Owner: req.GetOwner(),
	})
	if isOwnerRefusal(err) {
		return nil, err
	}
	if err != nil {
		return nil, errors.NewInternal("failed to get disk info", err)
	}

	// Validate format compatibility
	targetFormat := req.Format
	if targetFormat == "" {
		targetFormat = diskInfo.Format
	}
	if targetFormat != "qcow2" && targetFormat != "raw" && targetFormat != "vmdk" {
		return nil, errors.NewInvalidSpec("unsupported export format: %s", targetFormat)
	}

	exportID := fmt.Sprintf("export-%d-%d", vmid, time.Now().Unix())

	// Proxmox disk export strategy:
	// 1. Use vzdump to create a backup (VMA format)
	// 2. Extract the disk from VMA archive
	// 3. Convert if necessary
	// 4. Upload to destination

	p.logger.Info("Creating backup for disk export", "vmid", vmid, "node", node)

	// For now, we'll use a simplified approach:
	// - Stop the VM if required
	// - Use qemu-img to convert/copy the disk
	// - Calculate checksum
	// Note: Full implementation would use vzdump and handle running VMs

	p.logger.Warn("Using simplified disk export (not using vzdump)")
	p.logger.Info("Note: For production, implement vzdump-based export for running VMs")

	// Parse storage path (format: "storage:vm-100-disk-0")
	storageParts := strings.Split(diskInfo.Path, ":")
	if len(storageParts) != 2 {
		return nil, errors.NewInvalidSpec("invalid disk path format: %s", diskInfo.Path)
	}

	// Configure storage client
	// URL format: pvc://<pvc-name>/<file-path>
	// Provider pods have PVCs mounted at /mnt/migration-storage/<pvc-name>
	// Extract PVC name from URL to construct the correct mount path
	pvcName, err := extractPVCNameFromURL(req.DestinationUrl)
	if err != nil {
		return nil, errors.NewInternal("failed to extract PVC name from URL", err)
	}

	// Mount path matches where the controller mounts PVCs: /mnt/migration-storage/<pvc-name>
	mountPath := fmt.Sprintf("/mnt/migration-storage/%s", pvcName)

	storageConfig := storage.StorageConfig{
		Type:      "pvc",
		MountPath: mountPath,
	}

	storageClient, err := storage.NewStorage(storageConfig)
	if err != nil {
		return nil, errors.NewInternal("failed to create storage client", err)
	}
	defer storageClient.Close()

	// Create storage manager for Proxmox disk access
	storageManager := NewStorageManager(p.client, node)

	// Create temporary file for download
	pveStorage := storageParts[0]
	volid := storageParts[1]
	tempFile := fmt.Sprintf("/tmp/%s", exportID)
	defer func() {
		_ = os.Remove(tempFile)
	}()

	// Download from Proxmox storage
	p.logger.Info("Downloading disk from Proxmox storage", "storage", pveStorage, "volid", volid)

	file, err := os.Create(tempFile)
	if err != nil {
		return nil, errors.NewInternal("failed to create temp file", err)
	}
	defer file.Close()

	// Download with progress tracking
	downloadProgress := func(transferred, total int64) {
		if total > 0 {
			progress := float64(transferred) / float64(total) * 100
			p.logger.Debug("Download progress", "percent", progress, "transferred", transferred, "total", total)
		}
	}

	err = storageManager.DownloadVolume(ctx, pveStorage, volid, file, downloadProgress)
	if err != nil {
		return nil, errors.NewInternal("failed to download disk from Proxmox storage", err)
	}

	// Close file to flush writes
	file.Close()

	// Convert format and/or compress if needed.
	//
	// A qemu-img convert pass is required when the export format differs from
	// the source, OR when compression is requested for a compressible target
	// (qcow2): qemu-img applies `-c` only during a convert pass, so we force one
	// to honor req.Compress even when the format is unchanged (#219). Raw and
	// vmdk targets are not given a forced pass for compression here — see
	// exportNeedsConversion for the exact rule.
	var uploadPath string
	if exportNeedsConversion(diskInfo.Format, targetFormat, req.Compress) {
		p.logger.Info("Converting disk format",
			"from", diskInfo.Format, "to", targetFormat, "compress", req.Compress)
		convertedPath := fmt.Sprintf("/tmp/%s-converted.%s", exportID, targetFormat)

		// Use diskutil for conversion
		qemuImg := diskutil.NewQemuImg()
		err = qemuImg.Convert(ctx, diskutil.ConvertOptions{
			SourcePath:        tempFile,
			DestinationPath:   convertedPath,
			SourceFormat:      diskutil.SupportedFormat(diskInfo.Format),
			DestinationFormat: diskutil.SupportedFormat(targetFormat),
			// Honor the caller's request (#219). diskutil applies `-c` only to
			// qcow2; for raw/vmdk targets this is a no-op at the qemu-img layer.
			Compression: req.Compress,
		})
		if err != nil {
			return nil, errors.NewInternal("failed to convert disk format", err)
		}

		uploadPath = convertedPath
		defer os.Remove(convertedPath)
	} else {
		uploadPath = tempFile
	}

	// Upload to destination storage
	p.logger.Info("Uploading disk to storage", "destination", req.DestinationUrl)

	// Re-open file for reading
	uploadFile, err := os.Open(uploadPath)
	if err != nil {
		return nil, errors.NewInternal("failed to open file for upload", err)
	}
	defer uploadFile.Close()

	// Get file size
	stat, err := uploadFile.Stat()
	if err != nil {
		return nil, errors.NewInternal("failed to stat file", err)
	}

	// Upload with progress tracking
	uploadProgress := func(transferred, total int64) {
		if total > 0 {
			progress := float64(transferred) / float64(total) * 100
			p.logger.Debug("Upload progress", "percent", progress, "transferred", transferred, "total", total)
		}
	}

	uploadReq := storage.UploadRequest{
		Reader:           uploadFile,
		DestinationURL:   req.DestinationUrl,
		ContentLength:    stat.Size(),
		ProgressCallback: uploadProgress,
	}

	uploadResp, err := storageClient.Upload(ctx, uploadReq)
	if err != nil {
		return nil, errors.NewInternal("failed to upload disk", err)
	}

	p.logger.Info("Disk export completed", "export_id", exportID, "checksum", uploadResp.Checksum, "bytes", uploadResp.BytesTransferred)

	response := &providerv1.ExportDiskResponse{
		ExportId:           exportID,
		Task:               nil, // Synchronous operation
		EstimatedSizeBytes: uploadResp.BytesTransferred,
		Checksum:           uploadResp.Checksum,
	}

	return response, nil
}

// ImportDisk imports a disk from an external source
func (p *Provider) ImportDisk(ctx context.Context, req *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	// ADR-0006: Proxmox disks live on the PVE node, so the pod-mounted pvc path can
	// never work; accept only the S3 relay and the NFS qemu-img paths (both run
	// node-side) and reject pvc/unknown backends honestly (Slice 4).
	if err := migration.EnsureS3OrNFSBackend(req.BackendType); err != nil {
		return nil, err
	}
	// Proxmox's S3 path advertises relay-only transfer (RelayOnlyTransferModes);
	// reject a `direct` request loudly instead of silently running it as relay
	// (#261 P1-3, ADR-0006 D2). NFS is host/node-side by nature (the node's
	// qemu-img reads nfs:// directly), so it is exempt from the relay check.
	if req.BackendType != migration.BackendNFS {
		if err := migration.EnsureRelayMode(req.TransferMode); err != nil {
			return nil, err
		}
	}

	// rc2 R7: an import and DeleteLandedDisk of the same stage exclude each
	// other (landing_guard.go). A request with an invalid target is refused
	// below, by the import path itself.
	if tv := req.GetTargetVm(); validStageIdentity(tv.GetNamespace(), tv.GetName()) == nil {
		release, ok := stageGuards.beginImport(importStageName(tv.GetNamespace(), tv.GetName()))
		if !ok {
			return nil, errors.NewUnavailable("ImportDisk: the import stage of this VM is being deleted; retry", nil)
		}
		defer release()
	}

	// ADR-0006 Slice 4 Proxmox TARGET path: the node's qemu-img reads the staged
	// qcow2 from the NFS export and writes a node-local stage file for qm importdisk.
	if req.BackendType == migration.BackendNFS {
		return p.importDiskFromNFS(ctx, req)
	}

	// ADR-0006 Proxmox TARGET path: stage the S3 qcow2 on the node and return its
	// path for the Create path to consume with qm importdisk.
	if req.BackendType == migration.BackendS3 {
		return p.importDiskFromS3(ctx, req)
	}

	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	p.logger.Info("Importing disk", "source", req.SourceUrl, "storage", req.StorageHint)

	// Find appropriate node
	node, err := p.client.FindNode(ctx)
	if err != nil {
		return nil, errors.NewInternal("failed to find node", err)
	}

	// Determine storage
	pveStorage := "local-lvm"
	if req.StorageHint != "" {
		pveStorage = req.StorageHint
	}

	// Determine target format
	targetFormat := req.Format
	if targetFormat == "" {
		targetFormat = "qcow2" // Default
	}
	if targetFormat != "qcow2" && targetFormat != "raw" && targetFormat != "vmdk" {
		return nil, errors.NewInvalidSpec("unsupported import format: %s", targetFormat)
	}

	// Generate disk ID and VMID for import
	diskID := req.TargetName
	if diskID == "" {
		diskID = fmt.Sprintf("imported-%d", time.Now().Unix())
	}

	// Generate a temporary VMID for qm importdisk
	tempVMID := p.nextVMID(ctx)

	p.logger.Info("Preparing disk import", "disk_id", diskID, "node", node, "storage", pveStorage, "format", targetFormat)

	// Proxmox disk import strategy:
	// 1. Download disk from SourceURL to temp location
	// 2. Verify checksum if requested
	// 3. Use `qm importdisk` to import into Proxmox storage
	// 4. Return the imported disk reference

	// Configure storage client
	// URL format: pvc://<pvc-name>/<file-path>
	// Provider pods have PVCs mounted at /mnt/migration-storage/<pvc-name>
	// Extract PVC name from URL to construct the correct mount path
	pvcName, err := extractPVCNameFromURL(req.SourceUrl)
	if err != nil {
		return nil, errors.NewInternal("failed to extract PVC name from URL", err)
	}

	// Mount path matches where the controller mounts PVCs: /mnt/migration-storage/<pvc-name>
	mountPath := fmt.Sprintf("/mnt/migration-storage/%s", pvcName)

	storageConfig := storage.StorageConfig{
		Type:      "pvc",
		MountPath: mountPath,
	}

	storageClient, err := storage.NewStorage(storageConfig)
	if err != nil {
		return nil, errors.NewInternal("failed to create storage client", err)
	}
	defer storageClient.Close()

	// Create storage manager for Proxmox disk upload
	storageManager := NewStorageManager(p.client, node)

	// Create temporary file for download
	tempFile := fmt.Sprintf("/tmp/%s-import", diskID)
	defer func() {
		_ = os.Remove(tempFile)
	}()

	// Download from storage
	p.logger.Info("Downloading disk from storage", "source", req.SourceUrl)

	file, err := os.Create(tempFile)
	if err != nil {
		return nil, errors.NewInternal("failed to create temp file", err)
	}
	defer file.Close()

	// Download with progress tracking
	downloadProgress := func(transferred, total int64) {
		if total > 0 {
			progress := float64(transferred) / float64(total) * 100
			p.logger.Debug("Download progress", "percent", progress, "transferred", transferred, "total", total)
		}
	}

	downloadReq := storage.DownloadRequest{
		SourceURL:        req.SourceUrl,
		Writer:           file,
		VerifyChecksum:   req.VerifyChecksum,
		ExpectedChecksum: req.ExpectedChecksum,
		ProgressCallback: downloadProgress,
	}

	downloadResp, err := storageClient.Download(ctx, downloadReq)
	if err != nil {
		return nil, errors.NewInternal("failed to download disk", err)
	}

	// Close file to flush writes
	file.Close()

	p.logger.Info("Download completed", "bytes", downloadResp.BytesTransferred, "checksum", downloadResp.Checksum)

	// Convert to target format if needed
	var importPath string
	if targetFormat != "qcow2" {
		p.logger.Info("Converting to target format", "target_format", targetFormat)
		convertedPath := fmt.Sprintf("/tmp/%s-converted.%s", diskID, targetFormat)

		// Use diskutil for conversion
		qemuImg := diskutil.NewQemuImg()
		err = qemuImg.Convert(ctx, diskutil.ConvertOptions{
			SourcePath:        tempFile,
			DestinationPath:   convertedPath,
			DestinationFormat: diskutil.SupportedFormat(targetFormat),
			Compression:       false, // No compression for migration (faster)
		})
		if err != nil {
			return nil, errors.NewInternal("failed to convert disk format", err)
		}

		importPath = convertedPath
		defer os.Remove(convertedPath)
	} else {
		importPath = tempFile
	}

	// Upload to Proxmox storage
	p.logger.Info("Uploading to Proxmox storage", "storage", pveStorage, "disk_id", diskID)

	// Re-open file for reading
	uploadFile, err := os.Open(importPath)
	if err != nil {
		return nil, errors.NewInternal("failed to open file for Proxmox upload", err)
	}
	defer uploadFile.Close()

	// Get file size
	stat, err := uploadFile.Stat()
	if err != nil {
		return nil, errors.NewInternal("failed to stat file", err)
	}

	// Generate filename for Proxmox storage
	filename := fmt.Sprintf("%d/vm-%d-disk-0.%s", tempVMID, tempVMID, targetFormat)

	// Upload to Proxmox storage with progress tracking
	uploadProgress := func(transferred, total int64) {
		if total > 0 {
			progress := float64(transferred) / float64(total) * 100
			p.logger.Debug("Proxmox upload progress", "percent", progress, "transferred", transferred, "total", total)
		}
	}

	volid, err := storageManager.UploadVolume(ctx, uploadFile, pveStorage, filename, stat.Size(), uploadProgress)
	if err != nil {
		return nil, errors.NewInternal("failed to upload to Proxmox storage", err)
	}

	p.logger.Info("Disk import completed", "disk_id", diskID, "volid", volid)

	response := &providerv1.ImportDiskResponse{
		DiskId:          diskID,
		Path:            volid,
		Task:            nil, // Synchronous operation
		ActualSizeBytes: downloadResp.ContentLength,
		Checksum:        downloadResp.Checksum,
	}

	return response, nil
}

// ListVMs returns all VMs managed by this provider. The owner filter
// (req.owner_namespace / owner_name, ADR-0007 A6.2) is not implemented:
// Proxmox is a thin client of pve-cluster and never schedules, so it does not
// advertise supports_list_owner_filter, ignores the filter and never sets
// owner_filter_applied (D7, honesty-first).
//
// Each VM reports its owner stamp (vm_ownership.go) as provider_raw
// "owner_uid", plus "owner_stamp_state" (unreadable / multiple) when the stamp
// cannot be relied on, including when the VM's config cannot be read (e.g. its
// node is down). The listing comes from /cluster/resources, which lists VMs on
// offline nodes too; it fails as a whole only when that list cannot be read,
// and never silently omits a VM. Templates are skipped.
func (p *Provider) ListVMs(ctx context.Context, req *providerv1.ListVMsRequest) (*providerv1.ListVMsResponse, error) {
	p.logger.Info("Listing all virtual machines")

	if p.client == nil {
		return nil, fmt.Errorf("Proxmox client not configured")
	}

	// One cluster-wide list (/cluster/resources?type=vm), from the cluster's
	// replicated configuration: a VM on a node that is down is still listed
	// (status "unknown"), so the listing never silently omits a VM, and one
	// node being down does not fail it. Its scope is the configured node
	// selector, or every node.
	clusterVMs, err := p.client.ListClusterVMs(ctx)
	if err != nil {
		p.logger.Error("Failed to list the cluster's VMs", "error", err)
		return nil, errors.NewUnavailable("cannot list the Proxmox cluster's VMs", nil)
	}
	inScope := func(string) bool { return true }
	if sel := p.client.Config().NodeSelector; len(sel) > 0 {
		inScope = func(node string) bool { return slices.Contains(sel, node) }
	}

	var allVMs []*providerv1.VMInfo

	for _, cvm := range clusterVMs {
		if cvm.Type != pveapi.ClusterResourceTypeQEMU || !inScope(cvm.Node) {
			continue
		}
		node := cvm.Node
		vm := pveapi.VM{VMID: cvm.VMID, Name: cvm.Name, Status: cvm.Status, Node: cvm.Node,
			CPUs: int(cvm.MaxCPU), Memory: cvm.MaxMem, Template: cvm.Template}
		// Skip templates: ListVMs is the adoption feed, and a template is
		// shared image state, never a VM. Excluded rather than returned
		// tagged with contracts.VMInfoArtifactKey, which a manager older
		// than that key would not honour.
		if vm.Template == 1 {
			continue
		}

		// Get VM config for detailed information. A VM whose config cannot
		// be read is still listed, with owner_stamp_state "unreadable", so
		// it is never mistaken for an absent or unowned VM.
		config, err := p.client.GetVMConfig(ctx, node, vm.VMID)
		if stderrors.Is(err, pveapi.ErrVMNotFound) {
			if gone, goneErr := p.vmGone(ctx, vm.VMID); goneErr == nil && gone {
				continue // removed since the node listing
			}
			err = fmt.Errorf("VM %d is not on node %s but may exist elsewhere in the cluster", vm.VMID, node)
		}
		if err != nil {
			p.logger.Warn("Failed to get VM config; listing the VM with an unreadable owner stamp", "vmid", vm.VMID, "error", err)
			allVMs = append(allVMs, &providerv1.VMInfo{
				Id:   strconv.Itoa(vm.VMID),
				Name: vm.Name,
				ProviderRaw: map[string]string{
					"vmid":                             strconv.Itoa(vm.VMID),
					"node":                             node,
					"status":                           vm.Status,
					contracts.VMInfoOwnerStampStateKey: contracts.OwnerStampUnreadable,
				},
			})
			continue
		}
		if configIsTemplate(config) {
			continue
		}

		// Extract power state
		powerState := "Off"
		if vm.Status == "running" {
			powerState = "On"
		}

		// Extract CPU and memory
		cpu := countToInt32(int64(vm.CPUs))
		if cpu == 0 {
			if cores, ok := config["cores"].(float64); ok && cores > 0 {
				cpu = countToInt32(int64(cores))
			} else {
				cpu = 1 // Default
			}
		}

		memoryMiB := vm.Memory / (1024 * 1024) // Convert bytes to MiB
		if memoryMiB == 0 {
			if mem, ok := config["memory"].(float64); ok {
				memoryMiB = int64(mem)
			} else {
				memoryMiB = 1024 // Default 1 GiB
			}
		}

		// Extract disk information
		var disks []*providerv1.DiskInfo
		for key, value := range config {
			if strings.HasPrefix(key, "virtio") || strings.HasPrefix(key, "scsi") || strings.HasPrefix(key, "ide") || strings.HasPrefix(key, "sata") {
				if diskStr, ok := value.(string); ok && diskStr != "" {
					// Parse disk string format: storage:size,format=qcow2
					parts := strings.Split(diskStr, ",")
					diskPath := parts[0]
					format := "qcow2"
					for _, part := range parts[1:] {
						if strings.HasPrefix(part, "format=") {
							format = strings.TrimPrefix(part, "format=")
						}
					}

					// Extract size if available
					sizeGiB := int32(0)
					if sizeParts := strings.Split(diskPath, ":"); len(sizeParts) > 1 {
						if size, err := strconv.ParseInt(sizeParts[1], 10, 32); err == nil {
							sizeGiB = int32(size)
						}
					}

					diskInfo := &providerv1.DiskInfo{
						Id:      fmt.Sprintf("%d-%s", vm.VMID, key),
						Path:    diskPath,
						SizeGib: sizeGiB,
						Format:  format,
					}
					disks = append(disks, diskInfo)
				}
			}
		}

		// Extract network information (networks.go): the bridge and the MAC
		// of each NIC, ordered by NIC index.
		networks := networkInfosFromConfig(config)

		// Build provider raw metadata
		providerRaw := make(map[string]string)
		providerRaw["vmid"] = strconv.Itoa(vm.VMID)
		providerRaw["node"] = node
		providerRaw["status"] = vm.Status
		providerRaw["power_state"] = powerState
		// The owner stamp (vm_ownership.go): owner_uid, and
		// owner_stamp_state when it cannot be relied on. Adoption skips a
		// VM owned by a live VirtualMachine or with a stamp state.
		ownerRaw(providerRaw, readOwnerStamp(configString(config, pveConfigKeyDescription)))

		vmInfo := &providerv1.VMInfo{
			Id:          strconv.Itoa(vm.VMID),
			Name:        vm.Name,
			PowerState:  powerState,
			Ips:         []string{}, // IPs would need guest agent or other method
			Cpu:         cpu,
			MemoryMib:   memoryMiB,
			Disks:       disks,
			Networks:    networks,
			ProviderRaw: providerRaw,
		}

		allVMs = append(allVMs, vmInfo)
	}

	return &providerv1.ListVMsResponse{
		Vms: allVMs,
	}, nil
}

// parseDiskSize converts Proxmox disk size string (e.g., "32G", "1024M") to bytes
func (p *Provider) parseDiskSize(sizeStr string) (int64, error) {
	if sizeStr == "" {
		return 0, fmt.Errorf("empty size string")
	}

	// Remove trailing spaces
	sizeStr = strings.TrimSpace(sizeStr)

	// Extract number and unit
	var value float64
	var unit string

	// Try to parse with scanf
	n, err := fmt.Sscanf(sizeStr, "%f%s", &value, &unit)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid size format: %s", sizeStr)
	}

	// Default to bytes if no unit
	if n == 1 {
		return int64(value), nil
	}

	// Convert to bytes based on unit
	switch strings.ToUpper(unit) {
	case "B":
		return int64(value), nil
	case "K", "KB", "KIB":
		return int64(value * 1024), nil
	case "M", "MB", "MIB":
		return int64(value * 1024 * 1024), nil
	case "G", "GB", "GIB":
		return int64(value * 1024 * 1024 * 1024), nil
	case "T", "TB", "TIB":
		return int64(value * 1024 * 1024 * 1024 * 1024), nil
	default:
		return 0, fmt.Errorf("unknown unit: %s", unit)
	}
}

// countToInt32 converts a count PVE reports (vCPUs) to the int32 the gRPC
// contract carries, clamped to [0, math.MaxInt32] instead of wrapping.
func countToInt32(v int64) int32 {
	switch {
	case v < 0:
		return 0
	case v > math.MaxInt32:
		return math.MaxInt32
	default:
		return int32(v)
	}
}
