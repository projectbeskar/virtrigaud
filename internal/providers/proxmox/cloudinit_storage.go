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
	"fmt"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

const (
	// cloudInitDriveVolume is the volume name PVE gives a cloud-init drive
	// ("<storage>:cloudinit" in the ide2 value).
	cloudInitDriveVolume = "cloudinit"

	// cloudInitDefaultSlot is the device a cloud-init drive is put on when the
	// VM has none yet.
	cloudInitDefaultSlot = "ide2"

	// cloudInitLastResortStorage is the storage a cloud-init drive goes to only
	// when nothing else names or reveals one. It is PVE's stock `local` storage,
	// which is a directory storage WITHOUT the `images` content type on a default
	// install, so PVE refuses to start a VM whose cloud-init drive is there
	// ("storage 'local' does not support content-type 'images'"). It is a last
	// resort for a VM with no disk to follow, not a default to rely on: see
	// cloudInitStorage.
	cloudInitLastResortStorage = "local"

	// invalidRequestStorageMessage and invalidDefaultStorageMessage are the
	// InvalidSpec answers for a configured storage that is not a PVE storage ID.
	// They never repeat the value: it comes from a request or an environment
	// variable and is not put in a "<storage>:cloudinit" config value, nor in a
	// message.
	invalidRequestStorageMessage = "the Proxmox storage named for the VM is not a valid PVE storage ID"
	invalidDefaultStorageMessage = "the provider's default storage (PROVIDER_DEFAULT_STORAGE / PVE_DEFAULT_STORAGE) is not a valid PVE storage ID"
)

// cloudInitDrive returns the config value that puts a cloud-init drive on
// storage.
func cloudInitDrive(storage string) string {
	return fmt.Sprintf("%s:%s", storage, cloudInitDriveVolume)
}

// checkRequestStorage returns an InvalidSpec error when explicit, the storage
// the request or the image names for the VM, is set and is not a PVE storage
// ID.
func checkRequestStorage(explicit string) error {
	if explicit != "" && !pveapi.ValidStorageID(explicit) {
		return errors.NewInvalidSpec(invalidRequestStorageMessage)
	}
	return nil
}

// defaultStorage returns the provider's default storage
// (PROVIDER_DEFAULT_STORAGE / PVE_DEFAULT_STORAGE), "" when none is set, or an
// InvalidSpec error when it is set and is not a PVE storage ID.
func (p *Provider) defaultStorage() (string, error) {
	if p.client == nil || p.client.Config() == nil {
		return "", nil
	}
	def := p.client.Config().DefaultStorage
	if def != "" && !pveapi.ValidStorageID(def) {
		return "", errors.NewInvalidSpec(invalidDefaultStorageMessage)
	}
	return def, nil
}

// diskfulCloudInitStorage chooses the storage of the cloud-init drive of a VM
// cloned from a template, whose config cfg (read after the clone) shows its
// disks. In order:
//
//  1. explicit, the storage the request or the image names for the VM;
//  2. the storage the VM's primary disk lives on (pveapi.PrimaryDiskFromConfig):
//     the drive goes where the VM's disk is, and that storage holds disk
//     images, so it can hold the drive too, whatever this PVE calls its
//     storages;
//  3. the provider's default storage (PROVIDER_DEFAULT_STORAGE /
//     PVE_DEFAULT_STORAGE);
//  4. cloudInitLastResortStorage, only when the config shows no storage-backed
//     disk (a warning is logged).
//
// explicit is checked by checkRequestStorage before the clone, and again here
// (it ends up in a config value). An invalid storage is an InvalidSpec error.
func (p *Provider) diskfulCloudInitStorage(explicit string, cfg map[string]interface{}, vmid int) (string, error) {
	if err := checkRequestStorage(explicit); err != nil {
		return "", err
	}
	if explicit != "" {
		return explicit, nil
	}
	_, volume := pveapi.PrimaryDiskFromConfig(cfg)
	if storage := pveapi.DiskStorage(volume); storage != "" {
		return storage, nil
	}
	def, err := p.defaultStorage()
	if err != nil {
		return "", err
	}
	if def != "" {
		return def, nil
	}
	p.logger.Warn("The cloned VM's config shows no storage-backed disk and no default storage is set; "+
		"the cloud-init drive goes to the last-resort storage, set PVE_DEFAULT_STORAGE to a storage with the "+
		"images content type if PVE refuses to start the VM",
		"vmid", vmid, "storage", cloudInitLastResortStorage)
	return cloudInitLastResortStorage, nil
}

// disklessCloudInitStorage chooses the storage of the cloud-init drive of a VM
// made without a template, which has no disk to follow: explicit, else the
// provider's default storage, else cloudInitLastResortStorage.
func (p *Provider) disklessCloudInitStorage(explicit string) (string, error) {
	if err := checkRequestStorage(explicit); err != nil {
		return "", err
	}
	if explicit != "" {
		return explicit, nil
	}
	def, err := p.defaultStorage()
	if err != nil {
		return "", err
	}
	if def != "" {
		return def, nil
	}
	return cloudInitLastResortStorage, nil
}

// cloudInitChange decides what a clone's config update does about the
// cloud-init drive. cfg is the config of the clone, which PVE made from the
// template's: a template that has a cloud-init drive gives the clone its own
// volume (vm-<vmid>-cloudinit, on the clone's storage). Asking PVE for a new
// "<storage>:cloudinit" on a storage that already holds that volume fails
// (the logical volume exists; the drive is left as it is), so:
//
//   - a drive already on storage: nothing is set (set is false); PVE builds
//     the drive's contents again at every start;
//   - a drive on another storage: it is set to a new one on storage, in the
//     slot it is in (PVE removes the old volume);
//   - no drive: it is set in cloudInitDefaultSlot.
//
// slot is the device the drive is on afterwards (it is logged; the boot order
// does not name it, see cloneBootOrder).
func cloudInitChange(cfg map[string]interface{}, storage string) (slot, value string, set bool) {
	existingSlot, existingValue, found := pveapi.CloudInitDrive(cfg)
	if !found {
		return cloudInitDefaultSlot, cloudInitDrive(storage), true
	}
	if pveapi.DiskStorage(existingValue) == storage {
		return existingSlot, "", false
	}
	return existingSlot, cloudInitDrive(storage), true
}
