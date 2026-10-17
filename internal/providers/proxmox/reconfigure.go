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
	"fmt"
	"slices"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Reconfigure: which disk a size goes to.
//
// The Proxmox Create keeps the template's disks as they are: it applies
// neither the VMClass spec.diskDefaults.size nor spec.disks (it attaches no
// additional disk). So a Reconfigure:
//
//   - grows the VM's primary disk — the first disk of its boot order, else the
//     disk Create would pick (virtio, scsi, sata, ide; pveapi
//     DetectPrimaryDisk) — to the VMClass size (Class.DiskDefaults.SizeGiB).
//     That size is a floor, as on vSphere and libvirt: a disk already that
//     large is left as it is;
//   - leaves every disk alone for spec.disks: no disk was made for an entry,
//     so no disk is its — the template's other disks (scsi1, ...) are not.
//     The entries are logged. Before this fix every spec.disks size went to
//     scsi0, the root disk.
//
// The grow runs, and is waited for, before the CPU/memory change: a
// Reconfigure that grows a disk no longer returns before sending the CPU and
// memory (which it used to drop while reporting success).

// pveGiB is the number of bytes in a GiB.
const pveGiB int64 = 1 << 30

// pveReconfigureDesired is the part of a marshaled contracts.CreateRequest a
// Proxmox Reconfigure applies. contracts.VMClass, DiskDefaults and DiskSpec
// carry no json tags, so the keys are their Go field names.
type pveReconfigureDesired struct {
	Class struct {
		CPU          int32 `json:"CPU"`
		MemoryMiB    int32 `json:"MemoryMiB"`
		DiskDefaults *struct {
			SizeGiB int32 `json:"SizeGiB"`
		} `json:"DiskDefaults"`
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

// pveDiskPrefixes is the bus order Create picks a primary disk in
// (pveapi.Client.DetectPrimaryDisk).
var pveDiskPrefixes = []string{"virtio", "scsi", "sata", "ide"}

// pveIsDisk reports whether the config value of bus key is an attached disk
// (not a CD-ROM, not the cloud-init drive).
func pveIsDisk(key string, value any) bool {
	s, ok := value.(string)
	if !ok || s == "" || !pveBusDiskKey.MatchString(key) {
		return false
	}
	parts := strings.Split(s, ",")
	return !slices.Contains(parts, "media=cdrom") && !strings.Contains(parts[0], "cloudinit")
}

// pvePrimaryDisk returns the config key of the VM's primary disk: the first
// disk in its boot order ("boot: order=scsi0;ide2;net0"), else the first disk
// in pveDiskPrefixes order — or "" when it has none.
func pvePrimaryDisk(config map[string]any) string {
	if boot, ok := config["boot"].(string); ok {
		for _, opt := range strings.Split(boot, ",") {
			order, found := strings.CutPrefix(strings.TrimSpace(opt), "order=")
			if !found {
				continue
			}
			for _, key := range strings.Split(order, ";") {
				if key = strings.TrimSpace(key); pveIsDisk(key, config[key]) {
					return key
				}
			}
		}
	}
	for _, prefix := range pveDiskPrefixes {
		for i := 0; i < 32; i++ {
			key := fmt.Sprintf("%s%d", prefix, i)
			if pveIsDisk(key, config[key]) {
				return key
			}
		}
	}
	return ""
}

// pveDiskSizeBytes returns the size the "size=" option of disk config value
// names, or 0 when it has none or it does not parse.
func (p *Provider) pveDiskSizeBytes(value any) int64 {
	s, _ := value.(string)
	for _, part := range strings.Split(s, ",") {
		if size, found := strings.CutPrefix(part, "size="); found {
			if b, err := p.parseDiskSize(size); err == nil && b > 0 {
				return b
			}
		}
	}
	return 0
}

// growPrimaryDisk grows VM vmid's primary disk to sizeGiB (a floor) and waits
// for the resize task, so the CPU/memory change that follows is sent after it.
func (p *Provider) growPrimaryDisk(ctx context.Context, node string, vmid int, config map[string]any, sizeGiB int64) error {
	key := pvePrimaryDisk(config)
	if key == "" {
		p.logger.Info("The VM has no disk; the VMClass disk size is not applied", "vmid", vmid)
		return nil
	}
	current := p.pveDiskSizeBytes(config[key])
	if current == 0 {
		// PVE refuses a smaller absolute size; without the current one a grow
		// could be a shrink request, so nothing is sent.
		p.logger.Warn("The primary disk's size is unknown; the VMClass disk size is not applied", "vmid", vmid, "disk", key)
		return nil
	}
	if sizeGiB*pveGiB <= current {
		return nil
	}
	p.logger.Info("Primary disk grow requested", "vmid", vmid, "disk", key,
		"old_bytes", current, "new_gb", sizeGiB)
	taskID, err := p.client.ResizeDisk(ctx, node, vmid, key, sizeGiB)
	if err != nil {
		return errors.NewInternal("failed to resize disk", err)
	}
	if err := p.client.WaitForTask(ctx, node, taskID); err != nil {
		return errors.NewInternal("disk resize task failed", err)
	}
	return nil
}

// reconfigureVM applies desired to VM vmid on node, whose current config is
// currentConfig: the primary disk grow first (waited for), then CPU/memory.
func (p *Provider) reconfigureVM(ctx context.Context, node string, vmid int, currentConfig map[string]any, desired *pveReconfigureDesired) (*providerv1.TaskResponse, error) {
	for _, d := range desired.Disks {
		if d.SizeGiB > 0 {
			p.logger.Info("No disk was made for this spec.disks entry (the Proxmox Create does not attach spec.disks); leaving every disk as it is",
				"vmid", vmid, "disk_name", d.Name)
		}
	}
	if dd := desired.Class.DiskDefaults; dd != nil && dd.SizeGiB > 0 {
		if err := p.growPrimaryDisk(ctx, node, vmid, currentConfig, int64(dd.SizeGiB)); err != nil {
			return nil, err
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
		return nil, errors.NewInternal("failed to reconfigure VM", err)
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
