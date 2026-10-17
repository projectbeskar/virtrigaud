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

	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Reconfigure sets CPU and memory, and changes no disk.
//
// The Proxmox Create keeps the template's disks as they are: it applies
// neither the VMClass spec.diskDefaults.size nor spec.disks (it attaches no
// additional disk). So no spec.disks size has a disk to go to — not scsi0,
// the root disk, where every one used to go, and not a template's other disks
// (scsi1, ...) — and the VMClass size is not applied either (that belongs to
// Create). The entries are logged. The CPU/memory change is always sent; a
// disk grow used to return before it, dropping it while reporting success.
//
// A config update PVE fails is answered with VM_OPERATION_FAILED and a
// constant message (the cause is logged), so one VM's failing reconfigure
// never counts toward the manager's per-Provider circuit breaker. The config
// read before it (an unreachable PVE fails there) is not.

// reconfigureFailedMessage is the answer to a CPU/memory config update PVE
// failed.
const reconfigureFailedMessage = "PVE failed the CPU/memory configuration update of this VM " +
	"(details are in the provider log); it is retried"

// pveReconfigureDesired is the part of a marshaled contracts.CreateRequest a
// Proxmox Reconfigure reads. contracts.VMClass and DiskSpec carry no json
// tags, so the keys are their Go field names.
type pveReconfigureDesired struct {
	Class struct {
		CPU       int32 `json:"CPU"`
		MemoryMiB int32 `json:"MemoryMiB"`
	} `json:"Class"`
	Disks []struct {
		Name    string `json:"Name"`
		SizeGiB int32  `json:"SizeGiB"`
	} `json:"Disks"`
}

// parseReconfigureDesired parses a Reconfigure's DesiredJson.
func parseReconfigureDesired(desiredJSON string) (*pveReconfigureDesired, error) {
	var d pveReconfigureDesired
	if err := json.Unmarshal([]byte(desiredJSON), &d); err != nil {
		return nil, errors.NewInvalidSpec("failed to parse desired configuration: %v", err)
	}
	return &d, nil
}

// reconfigureVM applies desired's CPU and memory to VM vmid on node.
func (p *Provider) reconfigureVM(ctx context.Context, node string, vmid int, desired *pveReconfigureDesired) (*providerv1.TaskResponse, error) {
	for _, d := range desired.Disks {
		if d.SizeGiB > 0 {
			p.logger.Info("No disk was made for this spec.disks entry (the Proxmox Create attaches none); leaving every disk as it is",
				"vmid", vmid, "disk_name", d.Name)
		}
	}

	config := &pveapi.ReconfigureConfig{}
	if desired.Class.CPU > 0 {
		cpus := int(desired.Class.CPU)
		config.CPUs = &cpus
	}
	// MemoryMiB is MiB, the unit of PVE's `memory`.
	if desired.Class.MemoryMiB > 0 {
		mem := int64(desired.Class.MemoryMiB)
		config.Memory = &mem
	}
	if config.CPUs == nil && config.Memory == nil {
		return &providerv1.TaskResponse{}, nil
	}
	taskID, err := p.client.ReconfigureVM(ctx, node, vmid, config)
	if err != nil {
		p.logger.Error("PVE failed the CPU/memory configuration update", "vmid", vmid, "error", err)
		return nil, statusWithReasons(codes.Unknown, reconfigureFailedMessage, contracts.VMOperationFailedReason)
	}
	// RestartRequired stays false: this provider does not yet detect a change
	// PVE stored as pending (hot-plug not enabled for it) and so cannot report
	// it; the Reconfigure contract's restart_required is set only by providers
	// that do.
	result := &providerv1.TaskResponse{RestartRequired: false}
	if taskID != "" {
		result.Task = &providerv1.TaskRef{Id: taskID}
	}
	return result, nil
}
