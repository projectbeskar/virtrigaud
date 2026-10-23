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
	"fmt"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
)

const (
	// cloudInitDriveVolume is the volume name PVE gives a cloud-init drive
	// ("<storage>:cloudinit" in the ide2 value).
	cloudInitDriveVolume = "cloudinit"

	// cloudInitLastResortStorage is the storage a cloud-init drive goes to only
	// when nothing else names or reveals one. It is PVE's stock `local` storage,
	// which is a directory storage WITHOUT the `images` content type on a default
	// install, so PVE refuses to start a VM whose cloud-init drive is there
	// ("storage 'local' does not support content-type 'images'"). It is a last
	// resort for a VM with no disk to follow, not a default to rely on: see
	// cloudInitStorage.
	cloudInitLastResortStorage = "local"
)

// cloudInitDrive returns the ide2 value that puts a cloud-init drive on
// storage.
func cloudInitDrive(storage string) string {
	return fmt.Sprintf("%s:%s", storage, cloudInitDriveVolume)
}

// configuredCloudInitStorage returns the storage the request or the operator
// chose for the VM, or "" when neither did: explicit is the placement or image
// storage of the request (VMConfig.Storage); the operator's choice is the
// provider's default storage (PROVIDER_DEFAULT_STORAGE / PVE_DEFAULT_STORAGE).
// The first non-empty wins.
func (p *Provider) configuredCloudInitStorage(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if p.client != nil && p.client.Config() != nil {
		return p.client.Config().DefaultStorage
	}
	return ""
}

// cloudInitStorage chooses the storage of the cloud-init drive of VM vmid on
// node, a VM cloned from a template, which already has its boot disk. In
// order:
//
//  1. configured, the storage the request or the operator named
//     (configuredCloudInitStorage);
//  2. the storage the VM's boot disk actually lives on, read from the clone's
//     config: that storage holds disk images, so it can hold the cloud-init
//     drive too, whatever this PVE calls its storages;
//  3. cloudInitLastResortStorage, only when the config shows no such disk.
//
// A failure to read the config is not an error: the drive then goes where
// step 3 says, and the later config update reports a real PVE problem.
func (p *Provider) cloudInitStorage(ctx context.Context, node string, vmid int, configured string) string {
	if configured != "" {
		return configured
	}
	_, volume, err := p.client.DetectPrimaryDiskVolume(ctx, node, vmid)
	if err != nil {
		p.logger.Warn("Cannot read the cloned VM's config to find its boot disk's storage for the cloud-init drive",
			"vmid", vmid, "error", err)
		return cloudInitLastResortStorage
	}
	if storage := pveapi.DiskStorage(volume); storage != "" {
		return storage
	}
	p.logger.Warn("The cloned VM's boot disk names no storage; the cloud-init drive goes to the last-resort storage, "+
		"set PVE_DEFAULT_STORAGE to a storage with the images content type if PVE refuses to start the VM",
		"vmid", vmid, "storage", cloudInitLastResortStorage)
	return cloudInitLastResortStorage
}
