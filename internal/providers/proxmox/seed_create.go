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
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/url"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/cloneidentity"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// How Create and Clone give a VM its NoCloud seed (seed.go).

// linkedCloneUnsupportedMessage refuses a linked Clone: the provider makes
// full clones only.
const linkedCloneUnsupportedMessage = "the Proxmox provider does not support linked clones; use a full clone"

// finishingVMFailedMessage and finishingCloneFailedMessage (with the VMID)
// answer a re-sent Create or Clone that could not finish the VM an earlier
// attempt made.
const (
	finishingVMFailedMessage    = "finishing Proxmox VM %d failed"
	finishingCloneFailedMessage = "finishing clone %d failed"
)

// failSeedOrStep answers a Create whose step what failed (cause) after the VM
// vmid was made: a seed build whose outcome is unknown leaves the VM in place
// (outcomeUnknown; a re-sent Create finishes it), anything else removes it
// (failAfterSubmit). A definite refusal (InvalidSpec: no free slot,
// genisoimage missing, the VM on another node than the SSH data plane's) is
// answered as such once the VM is removed, so the manager does not make and
// remove a VM again at every retry.
func (p *Provider) failSeedOrStep(ctx context.Context, node string, vmid int, name string, stamp *ownerStamp, what string, cause error) error {
	if isSeedInFlight(cause) {
		return p.outcomeUnknown(fmt.Sprintf("%s: the cloud-init seed of VM %d may still be being built on the node", what, vmid), cause)
	}
	if status.Code(cause) == codes.InvalidArgument {
		if rbErr := p.rollbackCreatedVM(ctx, node, vmid, name, stamp, true); rbErr != nil {
			p.logger.Error("A failed create left a VM that could not be removed", "vmid", vmid, "node", node,
				"name", name, "step", what, "error", cause, "rollback_error", rbErr)
			return p.outcomeUnknown(fmt.Sprintf("%s; VM %d could not be removed", what, vmid), cause)
		}
		return cause
	}
	return p.failAfterSubmit(ctx, node, vmid, name, stamp, true, what, cause)
}

// applyCreateSeed gives VM vmid on node, whose config is cfg, the seed of
// the Create req: the request's user-data byte for byte, the rendered
// meta-data (createSeedMetaData) and the network-config rendered from
// ipConfigs and the VM's NICs (renderNetworkConfig), on the storage
// chooseStorage picks from cfg. A VM that already has a seed of its own is
// left as it is (attachSeed).
func (p *Provider) applyCreateSeed(ctx context.Context, req *providerv1.CreateRequest, node string, vmid int,
	ipConfigs []pveapi.IPConfig, cfg map[string]interface{}, chooseStorage func(map[string]interface{}) (string, error)) error {
	storage, err := chooseStorage(cfg)
	if err != nil {
		return err
	}
	meta, err := createSeedMetaData(req, vmid)
	if err != nil {
		return err
	}
	network, skipped, err := renderNetworkConfig(ipConfigs, cfg)
	if err != nil {
		return err
	}
	if len(skipped) > 0 {
		p.logger.Warn("The VM has no NIC (or no MAC) for some network attachments; the seed's network-config leaves them out",
			"vmid", vmid, "attachments", skipped)
	}
	return p.attachSeed(ctx, node, vmid, storage, cfg, seedContent{userData: req.UserData, metaData: meta, networkConfig: network})
}

// configureCloneSeed gives a VM cloned from a template for the Create req its
// seed: it reads the clone's config again (the NICs' MACs after
// applyCloneNetworks), sets the boot order to the boot disk only
// (cloneBootOrder), and attaches the seed on the storage a cloud-init drive
// would use (diskfulCloudInitStorage). qemu-server appends the seed's slot to
// that order when the seed is attached; attachSeed takes it out again, so the
// clone's final order is the boot disk alone. The seed replaces a PVE
// cloud-init drive the template gave the clone.
func (p *Provider) configureCloneSeed(ctx context.Context, req *providerv1.CreateRequest, vmConfig *pveapi.VMConfig, node string) error {
	vmid := vmConfig.VMID
	cfg, err := p.client.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("read the config of cloned VM %d: %w", vmid, err)
	}
	if err := p.setCloneBootOrder(ctx, node, vmid, cfg); err != nil {
		return err
	}
	return p.applyCreateSeed(ctx, req, node, vmid, vmConfig.IPConfigs, cfg, func(cfg map[string]interface{}) (string, error) {
		return p.diskfulCloudInitStorage(vmConfig.Storage, cfg, vmid)
	})
}

// setCloneBootOrder sets the boot order of the clone vmid, whose config is
// cfg, to its boot disk only (cloneBootOrder), unless it already is that or
// the config shows no disk (the template's order is kept).
func (p *Provider) setCloneBootOrder(ctx context.Context, node string, vmid int, cfg map[string]interface{}) error {
	boot := cloneBootOrder(cfg)
	if boot == "" {
		p.logger.Info("The cloned VM's config shows no disk; its boot order is left as the template made it", "vmid", vmid)
		return nil
	}
	if configString(cfg, pveConfigKeyBoot) == boot {
		return nil
	}
	p.logger.Info("Setting boot order", "vmid", vmid, "boot", boot)
	task, err := p.client.ReconfigureVMRaw(ctx, node, vmid, url.Values{pveConfigKeyBoot: {boot}})
	if err != nil {
		return fmt.Errorf("set the boot order of cloned VM %d: %w", vmid, err)
	}
	if err := p.client.WaitForTask(ctx, node, task); err != nil {
		return fmt.Errorf("set the boot order of cloned VM %d: %w", vmid, err)
	}
	return nil
}

// createDisklessWithSeed makes a new VM (no template) for a Create with
// user-data. The VM is made without any of PVE's cloud-init keys (no
// cloud-init drive, ipconfigN or nameserver) and stamped "creating"; once its
// create task has ended (its NICs have their MACs) it gets its seed
// (finishDisklessSeed) and is stamped ready. A failure after the VM was made
// removes it, except while the seed may still be being built
// (failSeedOrStep). The answer carries no task: the VM is complete.
func (p *Provider) createDisklessWithSeed(ctx context.Context, req *providerv1.CreateRequest, vmConfig *pveapi.VMConfig,
	node string, stamp *ownerStamp) (*providerv1.CreateResponse, error) {
	vmid := vmConfig.VMID
	if _, err := p.disklessCloudInitStorage(vmConfig.Storage); err != nil {
		return nil, err
	}
	shell := *vmConfig
	shell.IPConfigs = nil
	shell.Custom = make(map[string]string, len(vmConfig.Custom)+1)
	for k, v := range vmConfig.Custom {
		shell.Custom[k] = v
	}
	if stamp != nil {
		shell.Custom[pveConfigKeyDescription] = composeDescription("", stamp)
	}
	p.logger.Info("Creating new VM with a cloud-init seed", "vmid", vmid)
	p.trackTask(stamp, node, vmid, "")
	taskID, err := p.client.CreateVM(ctx, node, &shell)
	if err != nil {
		return nil, p.submitFailed(stamp, vmid, p.classifySubmitError(ctx, "create VM", vmid, stamp, err))
	}
	p.trackTask(stamp, node, vmid, taskID)
	if err := p.client.WaitForTask(ctx, node, taskID); err != nil {
		if stderrors.Is(err, pveapi.ErrTaskFailed) {
			p.untrackTask(stamp, vmid)
		}
		return nil, p.failAfterTask(ctx, node, vmid, req.Name, stamp, "the VM create task failed", err)
	}
	p.untrackTask(stamp, vmid)
	if err := p.finishDisklessSeed(ctx, req, vmConfig, node, vmid); err != nil {
		return nil, p.failSeedOrStep(ctx, node, vmid, req.Name, stamp, "attaching the cloud-init seed failed", err)
	}
	if stamp != nil {
		ready := stamp.withPhase(ownerStampPhaseReady)
		if err := p.writeOwnerStamp(ctx, node, vmid, "", &ready); err != nil {
			return nil, p.failAfterSubmit(ctx, node, vmid, req.Name, &ready, true, "marking the VM ready failed", err)
		}
	}
	return &providerv1.CreateResponse{Id: fmt.Sprintf("%d", vmid)}, nil
}

// finishDisklessSeed gives the new VM vmid (made without a template for the
// Create req, parsed into vmConfig) its seed, on the storage a cloud-init
// drive of a VM without a disk would use (disklessCloudInitStorage).
func (p *Provider) finishDisklessSeed(ctx context.Context, req *providerv1.CreateRequest, vmConfig *pveapi.VMConfig, node string, vmid int) error {
	cfg, err := p.client.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("read the config of VM %d: %w", vmid, err)
	}
	return p.applyCreateSeed(ctx, req, node, vmid, vmConfig.IPConfigs, cfg, func(map[string]interface{}) (string, error) {
		return p.disklessCloudInitStorage(vmConfig.Storage)
	})
}

// detachForeignSeeds removes from VM vmid's config every seed CD-ROM that
// references another VM's seed volume: a clone, made without user-data, of a
// template that was converted from a VM with a seed. Left in place, the clone
// would boot with the template's user-data and instance-id. Removing a CD-ROM
// frees nothing (the volume is the template's).
func (p *Provider) detachForeignSeeds(ctx context.Context, node string, vmid int) error {
	cfg, err := p.client.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("read the config of cloned VM %d: %w", vmid, err)
	}
	_, foreign := seedSlots(cfg, vmid)
	if len(foreign) == 0 {
		return nil
	}
	p.logger.Warn("The template has a cloud-init seed; detaching it from the clone, which has no user-data", "vmid", vmid, "slots", foreign)
	task, err := p.client.ReconfigureVMRaw(ctx, node, vmid, url.Values{pveConfigKeyDelete: {strings.Join(foreign, ",")}})
	if err != nil {
		return fmt.Errorf("detach the template's cloud-init seed from cloned VM %d: %w", vmid, err)
	}
	if err := p.client.WaitForTask(ctx, node, task); err != nil {
		return fmt.Errorf("detach the template's cloud-init seed from cloned VM %d: %w", vmid, err)
	}
	return nil
}

// sourceHasSeed reports whether a clone source's config cfg has a seed
// CD-ROM (its own, or one it references).
func sourceHasSeed(cfg map[string]interface{}, vmid int) bool {
	own, foreign := seedSlots(cfg, vmid)
	return len(own)+len(foreign) > 0
}

// cloneNeedsSeed reports whether a clone made by req of a VM (vmid) whose
// config is cfg gets a seed of its own: when the config shows a seed CD-ROM
// (PVE copies its line by reference), or when the VMClone gives inline
// user-data, which reaches the guest only through a seed. A customization
// that is not valid JSON, or inline user-data over 1 MiB, is an InvalidSpec
// error.
func cloneNeedsSeed(req *providerv1.CloneRequest, cfg map[string]interface{}, vmid int) (bool, cloneidentity.Customization, error) {
	cust, err := cloneidentity.ParseCustomization(req.CustomizeJson)
	if err != nil {
		return false, cust, errors.NewInvalidSpec("%s", seedCustomizeInvalidMessage)
	}
	if len(cust.InlineUserData) > maxSeedFileBytes {
		return false, cust, errors.NewInvalidSpec("%s", seedUserDataTooLargeMessage)
	}
	return sourceHasSeed(cfg, vmid) || cust.InlineUserData != "", cust, nil
}

// checkCloneSeedRequest reports, before anything is cloned, whether the clone
// gets a seed (cloneNeedsSeed), and checks what that seed needs: the SSH data
// plane, its credentials and known_hosts, genisoimage on the node, and the
// clone's node (targetNode) being the node the SSH data plane reaches
// (seedPreflight). A clone that needs no seed needs nothing: it keeps PVE's
// cloud-init handling (R26).
func (p *Provider) checkCloneSeedRequest(ctx context.Context, req *providerv1.CloneRequest, sourceCfg map[string]interface{},
	sourceVMID int, targetNode string) (bool, error) {
	need, cust, err := cloneNeedsSeed(req, sourceCfg, sourceVMID)
	if err != nil {
		return false, err
	}
	if !need {
		if cust.SecretUserData {
			p.logger.Warn("The clone's user-data is given by a Secret reference, which the provider cannot resolve; "+
				"it is not applied", "source_vmid", sourceVMID)
		}
		return false, nil
	}
	if err := p.checkSeedPrerequisites(); err != nil {
		return false, err
	}
	return true, p.seedPreflight(ctx, targetNode)
}

// clonePlacement reads a Clone's placement override (PlacementJson): the node
// the clone lands on and the storage it is cloned to ("" when unset or
// unreadable).
func clonePlacement(req *providerv1.CloneRequest) (host, datastore string) {
	if req.PlacementJson == "" {
		return "", ""
	}
	var placement struct {
		Host      string `json:"Host"`
		Datastore string `json:"Datastore"`
	}
	if err := json.Unmarshal([]byte(req.PlacementJson), &placement); err != nil {
		return "", ""
	}
	return placement.Host, placement.Datastore
}

// cloneSeed gives the clone vmid on node (made by req) a seed of its own when
// it needs one (cloneNeedsSeed): its config shows a seed CD-ROM — PVE's
// clone copies its source's CD-ROM line, so it references the SOURCE's seed
// volume: identity and lifecycle would be shared — or the VMClone gives
// inline user-data. The new seed replaces that reference, or the clone's PVE
// cloud-init drive, in its slot. Its
// identity is the clone's (cloneidentity: instance-id from the target
// VirtualMachine's UID, local-hostname from the customization hostname and
// domain, else the target's name); its user-data is the VMClone's
// customization user-data, else an empty #cloud-config (the source's is not
// carried: the clone's disk already holds what it applied); its
// network-config is DHCP on every NIC, matched by the clone's own MACs. A
// clone that already has a seed of its own (a re-sent Clone) is left as it
// is; one without any seed needs nothing.
func (p *Provider) cloneSeed(ctx context.Context, req *providerv1.CloneRequest, node string, vmid int, storageHint string) error {
	cfg, err := p.client.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("read the config of clone %d: %w", vmid, err)
	}
	need, cust, err := cloneNeedsSeed(req, cfg, vmid)
	if err != nil || !need {
		return err
	}
	if err := p.checkSeedPrerequisites(); err != nil {
		return err
	}
	if len(cust.Unapplied) > 0 {
		p.logger.Warn("The Proxmox provider does not apply these clone customization fields", "vmid", vmid,
			"fields", strings.Join(cust.Unapplied, ", "))
	}
	userData := cust.InlineUserData
	if userData == "" {
		if cust.SecretUserData {
			p.logger.Warn("The clone's user-data is given by a Secret reference, which the provider cannot resolve; "+
				"the clone's seed has an empty user-data", "vmid", vmid)
		}
		userData = seedEmptyUserData
	}
	tv := req.GetTargetVm()
	target := contracts.ObjectIdentity{UID: tv.GetUid(), Namespace: tv.GetNamespace(), Name: tv.GetName()}
	hostname := cloneidentity.Hostname(cust, target, req.TargetName)
	meta, err := cloneidentity.RewriteMetaData("", cloneidentity.InstanceID(target, fmt.Sprintf(seedFallbackIDFormat, vmid)),
		cloneidentity.LocalHostname(hostname, cust.Domain))
	if err != nil {
		return errors.NewInvalidSpec("%s", seedMetaDataInvalidMessage)
	}
	network, _, err := renderNetworkConfig(dhcpIPConfigs(cfg), cfg)
	if err != nil {
		return err
	}
	storage, err := p.diskfulCloudInitStorage(storageHint, cfg, vmid)
	if err != nil {
		return err
	}
	return p.attachSeed(ctx, node, vmid, storage, cfg, seedContent{userData: []byte(userData), metaData: []byte(meta), networkConfig: network})
}
