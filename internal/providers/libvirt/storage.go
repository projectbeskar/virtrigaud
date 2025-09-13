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
	"log"
	"path/filepath"
	"strings"
	"time"
)

// StorageProvider manages libvirt storage operations
type StorageProvider struct {
	virshProvider *VirshProvider
}

// StoragePool represents a libvirt storage pool
type StoragePool struct {
	Name      string `json:"name"`
	UUID      string `json:"uuid"`
	State     string `json:"state"`
	Autostart string `json:"autostart"`
	Capacity  string `json:"capacity"`
	Available string `json:"available"`
	Used      string `json:"used"`
	Path      string `json:"path"`
	Type      string `json:"type"`
}

// StorageVolume represents a storage volume in a pool
type StorageVolume struct {
	Name       string `json:"name"`
	Pool       string `json:"pool"`
	Path       string `json:"path"`
	Type       string `json:"type"`
	Capacity   string `json:"capacity"`
	Allocation string `json:"allocation"`
	Format     string `json:"format"`
}

// ImageTemplate represents a downloadable VM template
type ImageTemplate struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Format      string `json:"format"`
	Size        string `json:"size"`
	OS          string `json:"os"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

// NewStorageProvider creates a new storage provider
func NewStorageProvider(virshProvider *VirshProvider) *StorageProvider {
	return &StorageProvider{
		virshProvider: virshProvider,
	}
}

// EnsureDefaultStoragePool ensures the default storage pool exists and is active
func (s *StorageProvider) EnsureDefaultStoragePool(ctx context.Context) error {
	log.Printf("INFO Ensuring default storage pool exists and is active")

	// Check if default pool exists
	result, err := s.virshProvider.runVirshCommand(ctx, "pool-list", "--all")
	if err != nil {
		return fmt.Errorf("failed to list storage pools: %w", err)
	}

	// Parse pool list to check if default pool exists
	hasDefaultPool := strings.Contains(result.Stdout, "default")

	if hasDefaultPool {
		// Check if the existing pool uses the correct path
		poolInfo, err := s.virshProvider.runVirshCommand(ctx, "pool-dumpxml", "default")
		if err == nil && strings.Contains(poolInfo.Stdout, "/var/lib/libvirt/images") {
			// Old pool with wrong path - delete and recreate
			log.Printf("INFO Deleting old default storage pool with incorrect path")
			_, _ = s.virshProvider.runVirshCommand(ctx, "pool-destroy", "default")
			_, _ = s.virshProvider.runVirshCommand(ctx, "pool-undefine", "default")
			hasDefaultPool = false
		}
	}

	if !hasDefaultPool {
		// Create default storage pool
		log.Printf("INFO Creating default storage pool")
		if err := s.createDefaultStoragePool(ctx); err != nil {
			return fmt.Errorf("failed to create default storage pool: %w", err)
		}
	}

	// Ensure the pool is active
	if err := s.ensurePoolActive(ctx, "default"); err != nil {
		return fmt.Errorf("failed to activate default storage pool: %w", err)
	}

	log.Printf("INFO Default storage pool is ready")
	return nil
}

// createDefaultStoragePool creates the default storage pool
func (s *StorageProvider) createDefaultStoragePool(ctx context.Context) error {
	// Use user-writable directory for storage pool to avoid permission issues
	poolPath := "/home/wrkode/libvirt-images"
	if _, err := s.virshProvider.runVirshCommand(ctx, "!", "mkdir", "-p", poolPath); err != nil {
		return fmt.Errorf("failed to create pool directory: %w", err)
	}

	// Define the default storage pool
	poolXML := fmt.Sprintf(`<pool type='dir'>
  <name>default</name>
  <target>
    <path>%s</path>
    <permissions>
      <mode>0755</mode>
      <owner>0</owner>
      <group>0</group>
    </permissions>
  </target>
</pool>`, poolPath)

	// Write pool XML to temporary file
	poolFile := "/tmp/default-pool.xml"
	heredocMarker := "EOF_POOL_" + fmt.Sprintf("%d", time.Now().UnixNano())
	command := fmt.Sprintf("cat > '%s' << '%s'\n%s\n%s", poolFile, heredocMarker, poolXML, heredocMarker)

	if _, err := s.virshProvider.runVirshCommand(ctx, "!", "bash", "-c", command); err != nil {
		return fmt.Errorf("failed to write pool XML: %w", err)
	}

	// Define the pool
	if _, err := s.virshProvider.runVirshCommand(ctx, "pool-define", poolFile); err != nil {
		return fmt.Errorf("failed to define storage pool: %w", err)
	}

	// Clean up temporary file
	_, _ = s.virshProvider.runVirshCommand(ctx, "!", "rm", "-f", poolFile)

	// Build the pool (create directory structure)
	if _, err := s.virshProvider.runVirshCommand(ctx, "pool-build", "default"); err != nil {
		log.Printf("WARN Failed to build storage pool (may already exist): %v", err)
	}

	// Set autostart
	if _, err := s.virshProvider.runVirshCommand(ctx, "pool-autostart", "default"); err != nil {
		log.Printf("WARN Failed to set pool autostart: %v", err)
	}

	log.Printf("INFO Successfully created default storage pool")
	return nil
}

// ensurePoolActive ensures a storage pool is active
func (s *StorageProvider) ensurePoolActive(ctx context.Context, poolName string) error {
	// Check pool state
	result, err := s.virshProvider.runVirshCommand(ctx, "pool-info", poolName)
	if err != nil {
		return fmt.Errorf("failed to get pool info: %w", err)
	}

	// If pool is not active, start it
	if !strings.Contains(result.Stdout, "State:") || !strings.Contains(result.Stdout, "running") {
		log.Printf("INFO Starting storage pool: %s", poolName)
		if _, err := s.virshProvider.runVirshCommand(ctx, "pool-start", poolName); err != nil {
			return fmt.Errorf("failed to start storage pool: %w", err)
		}
	}

	return nil
}

// CreateVolume creates a new storage volume
func (s *StorageProvider) CreateVolume(ctx context.Context, poolName, volumeName, format string, sizeGB int) (*StorageVolume, error) {
	log.Printf("INFO Creating storage volume: %s in pool %s (%dGB, %s)", volumeName, poolName, sizeGB, format)

	// Ensure pool is active
	if err := s.ensurePoolActive(ctx, poolName); err != nil {
		return nil, fmt.Errorf("failed to ensure pool is active: %w", err)
	}

	// Create volume using vol-create-as
	sizeBytes := fmt.Sprintf("%dG", sizeGB)
	result, err := s.virshProvider.runVirshCommand(ctx, "vol-create-as", poolName, volumeName, sizeBytes, "--format", format)
	if err != nil {
		return nil, fmt.Errorf("failed to create volume: %w, output: %s", err, result.Stderr)
	}

	// Get volume information
	volume, err := s.GetVolumeInfo(ctx, poolName, volumeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get created volume info: %w", err)
	}

	log.Printf("INFO Successfully created storage volume: %s", volumeName)
	return volume, nil
}

// GetVolumeInfo retrieves information about a storage volume
func (s *StorageProvider) GetVolumeInfo(ctx context.Context, poolName, volumeName string) (*StorageVolume, error) {
	result, err := s.virshProvider.runVirshCommand(ctx, "vol-info", volumeName, "--pool", poolName)
	if err != nil {
		return nil, fmt.Errorf("failed to get volume info: %w", err)
	}

	// Parse volume info
	volume := &StorageVolume{
		Name: volumeName,
		Pool: poolName,
	}

	lines := strings.Split(result.Stdout, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Type:") {
			volume.Type = strings.TrimSpace(strings.TrimPrefix(line, "Type:"))
		} else if strings.HasPrefix(line, "Capacity:") {
			volume.Capacity = strings.TrimSpace(strings.TrimPrefix(line, "Capacity:"))
		} else if strings.HasPrefix(line, "Allocation:") {
			volume.Allocation = strings.TrimSpace(strings.TrimPrefix(line, "Allocation:"))
		}
	}

	// Get volume path
	pathResult, err := s.virshProvider.runVirshCommand(ctx, "vol-path", volumeName, "--pool", poolName)
	if err == nil {
		volume.Path = strings.TrimSpace(pathResult.Stdout)
	}

	return volume, nil
}

// DownloadCloudImage downloads a cloud image and creates a bootable volume
func (s *StorageProvider) DownloadCloudImage(ctx context.Context, imageURL, volumeName, poolName string, sizeGB int) (*StorageVolume, error) {
	log.Printf("INFO Downloading cloud image from %s to volume %s", imageURL, volumeName)

	// Ensure pool is active
	if err := s.ensurePoolActive(ctx, poolName); err != nil {
		return nil, fmt.Errorf("failed to ensure pool is active: %w", err)
	}

	// Get pool path
	poolInfo, err := s.GetPoolInfo(ctx, poolName)
	if err != nil {
		return nil, fmt.Errorf("failed to get pool info: %w", err)
	}

	// Download image to temporary location
	tempImage := filepath.Join("/tmp", fmt.Sprintf("%s-temp.img", volumeName))
	log.Printf("INFO Downloading image to temporary location: %s", tempImage)

	result, err := s.virshProvider.runVirshCommand(ctx, "!", "wget", "-O", tempImage, imageURL)
	if err != nil {
		return nil, fmt.Errorf("failed to download image: %w, output: %s", err, result.Stderr)
	}

	// Get image info
	imageInfoCmd := fmt.Sprintf("qemu-img info '%s'", tempImage)
	infoResult, err := s.virshProvider.runVirshCommand(ctx, "!", "bash", "-c", imageInfoCmd)
	if err != nil {
		log.Printf("WARN Failed to get image info: %v", err)
	} else {
		log.Printf("DEBUG Image info: %s", infoResult.Stdout)
	}

	// Create target volume path
	targetPath := filepath.Join(poolInfo.Path, fmt.Sprintf("%s.qcow2", volumeName))

	// Convert and resize image if needed
	if sizeGB > 0 {
		log.Printf("INFO Converting and resizing image to %dGB", sizeGB)
		
		// First convert the image
		result, err = s.virshProvider.runVirshCommand(ctx, "!", "qemu-img", "convert", "-f", "qcow2", "-O", "qcow2", tempImage, targetPath)
		if err != nil {
			return nil, fmt.Errorf("failed to convert image: %w, output: %s", err, result.Stderr)
		}
		
		// Then resize it
		sizeSpec := fmt.Sprintf("%dG", sizeGB)
		result, err = s.virshProvider.runVirshCommand(ctx, "!", "qemu-img", "resize", targetPath, sizeSpec)
		if err != nil {
			return nil, fmt.Errorf("failed to resize image: %w, output: %s", err, result.Stderr)
		}
	} else {
		// Just convert to target location
		log.Printf("INFO Converting image to qcow2 format")
		result, err = s.virshProvider.runVirshCommand(ctx, "!", "qemu-img", "convert", "-f", "qcow2", "-O", "qcow2", tempImage, targetPath)
		if err != nil {
			return nil, fmt.Errorf("failed to convert image: %w, output: %s", err, result.Stderr)
		}
	}

	// Clean up temporary file
	_, _ = s.virshProvider.runVirshCommand(ctx, "!", "rm", "-f", tempImage)

	// Refresh storage pool to recognize new volume
	if _, err := s.virshProvider.runVirshCommand(ctx, "pool-refresh", poolName); err != nil {
		log.Printf("WARN Failed to refresh storage pool: %v", err)
	}

	// Get volume information
	volume, err := s.GetVolumeInfo(ctx, poolName, volumeName)
	if err != nil {
		// If vol-info fails, create a basic volume info
		volume = &StorageVolume{
			Name:   volumeName,
			Pool:   poolName,
			Path:   targetPath,
			Format: "qcow2",
			Type:   "file",
		}
	}

	log.Printf("INFO Successfully downloaded and prepared cloud image: %s", volumeName)
	return volume, nil
}

// GetPoolInfo retrieves information about a storage pool
func (s *StorageProvider) GetPoolInfo(ctx context.Context, poolName string) (*StoragePool, error) {
	result, err := s.virshProvider.runVirshCommand(ctx, "pool-info", poolName)
	if err != nil {
		return nil, fmt.Errorf("failed to get pool info: %w", err)
	}

	pool := &StoragePool{
		Name: poolName,
	}

	lines := strings.Split(result.Stdout, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "UUID:") {
			pool.UUID = strings.TrimSpace(strings.TrimPrefix(line, "UUID:"))
		} else if strings.HasPrefix(line, "State:") {
			pool.State = strings.TrimSpace(strings.TrimPrefix(line, "State:"))
		} else if strings.HasPrefix(line, "Autostart:") {
			pool.Autostart = strings.TrimSpace(strings.TrimPrefix(line, "Autostart:"))
		} else if strings.HasPrefix(line, "Capacity:") {
			pool.Capacity = strings.TrimSpace(strings.TrimPrefix(line, "Capacity:"))
		} else if strings.HasPrefix(line, "Available:") {
			pool.Available = strings.TrimSpace(strings.TrimPrefix(line, "Available:"))
		}
	}

	// Calculate used space
	if pool.Capacity != "" && pool.Available != "" {
		// This is a simplified calculation - in reality you'd parse the byte values
		pool.Used = "calculated"
	}

	// Get pool path
	pathResult, err := s.virshProvider.runVirshCommand(ctx, "pool-dumpxml", poolName)
	if err == nil && strings.Contains(pathResult.Stdout, "<path>") {
		// Extract path from XML (simple string parsing)
		start := strings.Index(pathResult.Stdout, "<path>") + 6
		end := strings.Index(pathResult.Stdout[start:], "</path>")
		if end > 0 {
			pool.Path = pathResult.Stdout[start : start+end]
		}
	}

	return pool, nil
}

// ListVolumes lists all volumes in a storage pool
func (s *StorageProvider) ListVolumes(ctx context.Context, poolName string) ([]*StorageVolume, error) {
	result, err := s.virshProvider.runVirshCommand(ctx, "vol-list", poolName)
	if err != nil {
		return nil, fmt.Errorf("failed to list volumes: %w", err)
	}

	var volumes []*StorageVolume
	lines := strings.Split(result.Stdout, "\n")

	// Skip header lines and parse volume entries
	for i, line := range lines {
		if i < 2 || strings.TrimSpace(line) == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) >= 2 {
			volumeName := fields[0]
			if volumeName != "Name" && volumeName != "----" {
				volume, err := s.GetVolumeInfo(ctx, poolName, volumeName)
				if err != nil {
					log.Printf("WARN Failed to get info for volume %s: %v", volumeName, err)
					continue
				}
				volumes = append(volumes, volume)
			}
		}
	}

	return volumes, nil
}

// DeleteVolume deletes a storage volume
func (s *StorageProvider) DeleteVolume(ctx context.Context, poolName, volumeName string) error {
	log.Printf("INFO Deleting storage volume: %s from pool %s", volumeName, poolName)

	result, err := s.virshProvider.runVirshCommand(ctx, "vol-delete", volumeName, "--pool", poolName)
	if err != nil {
		return fmt.Errorf("failed to delete volume: %w, output: %s", err, result.Stderr)
	}

	log.Printf("INFO Successfully deleted storage volume: %s", volumeName)
	return nil
}

// CloneVolume creates a clone of an existing volume
func (s *StorageProvider) CloneVolume(ctx context.Context, poolName, sourceVolume, targetVolume string) (*StorageVolume, error) {
	log.Printf("INFO Cloning volume %s to %s in pool %s", sourceVolume, targetVolume, poolName)

	result, err := s.virshProvider.runVirshCommand(ctx, "vol-clone", sourceVolume, targetVolume, "--pool", poolName)
	if err != nil {
		return nil, fmt.Errorf("failed to clone volume: %w, output: %s", err, result.Stderr)
	}

	// Get cloned volume information
	volume, err := s.GetVolumeInfo(ctx, poolName, targetVolume)
	if err != nil {
		return nil, fmt.Errorf("failed to get cloned volume info: %w", err)
	}

	log.Printf("INFO Successfully cloned volume: %s", targetVolume)
	return volume, nil
}

// ResizeVolume resizes an existing volume
func (s *StorageProvider) ResizeVolume(ctx context.Context, poolName, volumeName string, newSizeGB int) error {
	log.Printf("INFO Resizing volume %s to %dGB", volumeName, newSizeGB)

	newSize := fmt.Sprintf("%dG", newSizeGB)
	result, err := s.virshProvider.runVirshCommand(ctx, "vol-resize", volumeName, newSize, "--pool", poolName)
	if err != nil {
		return fmt.Errorf("failed to resize volume: %w, output: %s", err, result.Stderr)
	}

	log.Printf("INFO Successfully resized volume: %s", volumeName)
	return nil
}

// GetPredefinedTemplates returns a list of commonly used cloud images
func (s *StorageProvider) GetPredefinedTemplates() []*ImageTemplate {
	return []*ImageTemplate{
		{
			Name:        "ubuntu-22.04-server",
			URL:         "https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img",
			Format:      "qcow2",
			Size:        "2.5GB",
			OS:          "Ubuntu",
			Version:     "22.04 LTS",
			Description: "Ubuntu 22.04 LTS Server Cloud Image",
		},
		{
			Name:        "ubuntu-20.04-server",
			URL:         "https://cloud-images.ubuntu.com/focal/current/focal-server-cloudimg-amd64.img",
			Format:      "qcow2",
			Size:        "2.2GB",
			OS:          "Ubuntu",
			Version:     "20.04 LTS",
			Description: "Ubuntu 20.04 LTS Server Cloud Image",
		},
		{
			Name:        "centos-stream-9",
			URL:         "https://cloud.centos.org/centos/9-stream/x86_64/images/CentOS-Stream-GenericCloud-9-latest.x86_64.qcow2",
			Format:      "qcow2",
			Size:        "1.1GB",
			OS:          "CentOS",
			Version:     "Stream 9",
			Description: "CentOS Stream 9 Cloud Image",
		},
		{
			Name:        "debian-12",
			URL:         "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-generic-amd64.qcow2",
			Format:      "qcow2",
			Size:        "512MB",
			OS:          "Debian",
			Version:     "12 (Bookworm)",
			Description: "Debian 12 Cloud Image",
		},
	}
}

// CreateVolumeFromTemplate downloads and prepares a volume from a predefined template
func (s *StorageProvider) CreateVolumeFromTemplate(ctx context.Context, templateName, volumeName, poolName string, sizeGB int) (*StorageVolume, error) {
	log.Printf("INFO Creating volume %s from template %s", volumeName, templateName)

	// Find template
	templates := s.GetPredefinedTemplates()
	var template *ImageTemplate
	for _, t := range templates {
		if t.Name == templateName {
			template = t
			break
		}
	}

	if template == nil {
		return nil, fmt.Errorf("template not found: %s", templateName)
	}

	// Download and create volume
	return s.DownloadCloudImage(ctx, template.URL, volumeName, poolName, sizeGB)
}
