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

package libvirt

import (
	"context"
	"fmt"

	libvirt "libvirt.org/go/libvirt"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// createStorageVolume creates a storage volume for the VM
func (p *Provider) createStorageVolume(ctx context.Context, req contracts.CreateRequest) (string, error) {
	// Get or create storage pool
	pool, err := p.getDefaultStoragePool()
	if err != nil {
		return "", fmt.Errorf("failed to get storage pool: %w", err)
	}
	defer pool.Free() //nolint:errcheck // Libvirt pool cleanup not critical in defer

	// Check if we have a base image to clone from
	if req.Image.Path != "" {
		return p.createVolumeFromImage(pool, req)
	}

	// Create blank volume
	return p.createBlankVolume(pool, req)
}

// getDefaultStoragePool gets the default storage pool
func (p *Provider) getDefaultStoragePool() (*libvirt.StoragePool, error) {
	// Try to get 'default' pool first
	pool, err := p.conn.LookupStoragePoolByName("default")
	if err == nil {
		return pool, nil
	}

	// Fall back to first available pool
	pools, err := p.conn.ListAllStoragePools(0)
	if err != nil {
		return nil, fmt.Errorf("failed to list storage pools: %w", err)
	}

	if len(pools) == 0 {
		return nil, fmt.Errorf("no storage pools available")
	}

	return &pools[0], nil
}

// createVolumeFromImage creates a volume by cloning from a base image
func (p *Provider) createVolumeFromImage(pool *libvirt.StoragePool, req contracts.CreateRequest) (string, error) {
	// Check if base image exists by trying to look it up in the connection
	baseVol, err := p.conn.LookupStorageVolByPath(req.Image.Path)
	if err != nil {
		return "", fmt.Errorf("base image not found: %s", req.Image.Path)
	}
	defer baseVol.Free() //nolint:errcheck // Libvirt volume cleanup not critical in defer

	// Create clone volume XML
	volumeName := fmt.Sprintf("%s.qcow2", req.Name)
	diskSizeBytes := int64(req.Class.DiskDefaults.SizeGiB) * 1024 * 1024 * 1024

	volumeXML := fmt.Sprintf(`<volume type='file'>
  <name>%s</name>
  <capacity unit='bytes'>%d</capacity>
  <allocation unit='bytes'>0</allocation>
  <target>
    <format type='qcow2'/>
    <permissions>
      <mode>0644</mode>
    </permissions>
  </target>
  <backingStore>
    <path>%s</path>
    <format type='%s'/>
  </backingStore>
</volume>`, volumeName, diskSizeBytes, req.Image.Path, req.Image.Format)

	// Create the volume
	vol, err := pool.StorageVolCreateXML(volumeXML, 0)
	if err != nil {
		return "", fmt.Errorf("failed to create storage volume: %w", err)
	}
	defer vol.Free() //nolint:errcheck // Libvirt volume cleanup not critical in defer

	// Get the path of the created volume
	path, err := vol.GetPath()
	if err != nil {
		return "", fmt.Errorf("failed to get volume path: %w", err)
	}

	return path, nil
}

// createBlankVolume creates a blank storage volume
func (p *Provider) createBlankVolume(pool *libvirt.StoragePool, req contracts.CreateRequest) (string, error) {
	volumeName := fmt.Sprintf("%s.qcow2", req.Name)
	diskSizeBytes := int64(req.Class.DiskDefaults.SizeGiB) * 1024 * 1024 * 1024

	volumeXML := fmt.Sprintf(`<volume type='file'>
  <name>%s</name>
  <capacity unit='bytes'>%d</capacity>
  <allocation unit='bytes'>0</allocation>
  <target>
    <format type='qcow2'/>
    <permissions>
      <mode>0644</mode>
    </permissions>
  </target>
</volume>`, volumeName, diskSizeBytes)

	// Create the volume
	vol, err := pool.StorageVolCreateXML(volumeXML, 0)
	if err != nil {
		return "", fmt.Errorf("failed to create storage volume: %w", err)
	}
	defer vol.Free() //nolint:errcheck // Libvirt volume cleanup not critical in defer

	// Get the path of the created volume
	path, err := vol.GetPath()
	if err != nil {
		return "", fmt.Errorf("failed to get volume path: %w", err)
	}

	return path, nil
}

// createCloudInitISO creates a cloud-init ISO for the VM
func (p *Provider) createCloudInitISO(ctx context.Context, vmName, cloudInitData string) (string, error) {
	// Get storage pool for cloud-init ISOs
	pool, err := p.getDefaultStoragePool()
	if err != nil {
		return "", fmt.Errorf("failed to get storage pool: %w", err)
	}
	defer pool.Free() //nolint:errcheck // Libvirt pool cleanup not critical in defer

	// Create cloud-init ISO name
	isoName := fmt.Sprintf("%s-cloud-init.iso", vmName)

	// For now, we'll create a simple placeholder
	// In a full implementation, you'd want to:
	// 1. Create a temporary directory with cloud-init files
	// 2. Use genisoimage or similar to create the ISO
	// 3. Store it in the storage pool

	// Create a simple volume XML for the ISO
	volumeXML := fmt.Sprintf(`<volume type='file'>
  <name>%s</name>
  <capacity unit='bytes'>1048576</capacity>
  <allocation unit='bytes'>1048576</allocation>
  <target>
    <format type='raw'/>
    <permissions>
      <mode>0644</mode>
    </permissions>
  </target>
</volume>`, isoName)

	// Create the volume
	vol, err := pool.StorageVolCreateXML(volumeXML, 0)
	if err != nil {
		return "", fmt.Errorf("failed to create cloud-init ISO volume: %w", err)
	}
	defer vol.Free() //nolint:errcheck // Libvirt volume cleanup not critical in defer

	// Get the path of the created volume
	path, err := vol.GetPath()
	if err != nil {
		return "", fmt.Errorf("failed to get cloud-init ISO path: %w", err)
	}

	// TODO: Actually create the cloud-init ISO content
	// For MVP, we'll just return the path
	return path, nil
}

// deleteStorageVolume deletes a storage volume

// getStoragePoolPath returns the path of the default storage pool
