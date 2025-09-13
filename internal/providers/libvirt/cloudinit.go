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
	"os"
	"path/filepath"
	"strings"
)

// CloudInitConfig represents cloud-init configuration for libvirt VMs
type CloudInitConfig struct {
	UserData string // YAML cloud-init configuration
	MetaData string // Instance metadata (JSON)
	InstanceID string // Unique instance identifier
	Hostname string // VM hostname
}

// CloudInitProvider manages cloud-init ISO creation and attachment for libvirt
type CloudInitProvider struct {
	virshProvider *VirshProvider
	tempDir       string // Directory for storing cloud-init files
}

// NewCloudInitProvider creates a new cloud-init provider for libvirt
func NewCloudInitProvider(virshProvider *VirshProvider) *CloudInitProvider {
	return &CloudInitProvider{
		virshProvider: virshProvider,
		tempDir:       "/tmp/virtrigaud-cloudinit", // Writable temp directory
	}
}

// PrepareCloudInit creates cloud-init configuration and returns the ISO path
func (c *CloudInitProvider) PrepareCloudInit(ctx context.Context, config CloudInitConfig) (string, error) {
	log.Printf("INFO Preparing cloud-init configuration for instance: %s", config.InstanceID)
	
	// Ensure temp directory exists
	instanceDir := filepath.Join(c.tempDir, config.InstanceID)
	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create cloud-init directory: %w", err)
	}
	
	// Generate metadata if not provided
	if config.MetaData == "" {
		config.MetaData = c.generateMetaData(config.InstanceID, config.Hostname)
	}
	
	// Write user-data file
	userDataPath := filepath.Join(instanceDir, "user-data")
	if err := os.WriteFile(userDataPath, []byte(config.UserData), 0644); err != nil {
		return "", fmt.Errorf("failed to write user-data: %w", err)
	}
	
	// Write meta-data file
	metaDataPath := filepath.Join(instanceDir, "meta-data")
	if err := os.WriteFile(metaDataPath, []byte(config.MetaData), 0644); err != nil {
		return "", fmt.Errorf("failed to write meta-data: %w", err)
	}
	
	// Create cloud-init ISO using genisoimage (NoCloud datasource)
	isoPath := filepath.Join(instanceDir, "cloud-init.iso")
	if err := c.createCloudInitISO(ctx, instanceDir, isoPath); err != nil {
		return "", fmt.Errorf("failed to create cloud-init ISO: %w", err)
	}
	
	log.Printf("INFO Successfully created cloud-init ISO: %s", isoPath)
	return isoPath, nil
}

// createCloudInitISO creates an ISO9660 filesystem with cloud-init files
func (c *CloudInitProvider) createCloudInitISO(ctx context.Context, sourceDir, isoPath string) error {
	// Use genisoimage to create NoCloud datasource ISO
	// The ISO must contain user-data and meta-data files at the root
	result, err := c.virshProvider.runVirshCommand(ctx, "!", "genisoimage", 
		"-output", isoPath,
		"-volid", "cidata",     // Volume ID for NoCloud datasource
		"-joliet",              // Enable Joliet extensions
		"-rock",                // Enable Rock Ridge extensions  
		"-input-charset", "utf-8",
		sourceDir)
	
	if err != nil {
		return fmt.Errorf("genisoimage failed: %w, output: %s", err, result.Stderr)
	}
	
	log.Printf("DEBUG Created cloud-init ISO with genisoimage: %s", isoPath)
	return nil
}

// generateMetaData creates basic metadata for the VM instance
func (c *CloudInitProvider) generateMetaData(instanceID, hostname string) string {
	// Basic metadata following cloud-init NoCloud format
	metadata := fmt.Sprintf(`{
  "instance-id": "%s",
  "local-hostname": "%s",
  "network": {
    "version": 1,
    "config": [
      {
        "type": "dhcp",
        "interface": "eth0"
      }
    ]
  }
}`, instanceID, hostname)
	
	return metadata
}

// AttachCloudInitISO attaches the cloud-init ISO to a domain as a CD-ROM device
func (c *CloudInitProvider) AttachCloudInitISO(ctx context.Context, domainName, isoPath string) error {
	log.Printf("INFO Attaching cloud-init ISO to domain: %s", domainName)
	
	// Copy ISO to remote libvirt server if needed
	remoteISOPath, err := c.copyISOToRemote(ctx, isoPath, domainName)
	if err != nil {
		return fmt.Errorf("failed to copy ISO to remote server: %w", err)
	}
	
	// Attach ISO as CD-ROM device using virsh attach-disk
	result, err := c.virshProvider.runVirshCommand(ctx, "attach-disk", domainName, 
		remoteISOPath, "hdc", "--type", "cdrom", "--config")
	
	if err != nil {
		return fmt.Errorf("failed to attach cloud-init ISO: %w, output: %s", err, result.Stderr)
	}
	
	log.Printf("INFO Successfully attached cloud-init ISO to domain %s", domainName)
	return nil
}

// copyISOToRemote copies the cloud-init ISO to the remote libvirt server
func (c *CloudInitProvider) copyISOToRemote(ctx context.Context, localPath, domainName string) (string, error) {
	// Remote path for cloud-init ISOs
	remoteDir := "/var/lib/libvirt/images/cloud-init"
	remotePath := fmt.Sprintf("%s/%s-cloud-init.iso", remoteDir, domainName)
	
	// Create remote directory
	result, err := c.virshProvider.runVirshCommand(ctx, "!", "ssh", "wrkode@172.16.56.38", 
		"mkdir", "-p", remoteDir)
	if err != nil {
		log.Printf("WARN Failed to create remote directory (may already exist): %v", err)
	}
	
	// Copy ISO file using scp
	result, err = c.virshProvider.runVirshCommand(ctx, "!", "scp", localPath, 
		fmt.Sprintf("wrkode@172.16.56.38:%s", remotePath))
	
	if err != nil {
		return "", fmt.Errorf("scp failed: %w, output: %s", err, result.Stderr)
	}
	
	log.Printf("INFO Copied cloud-init ISO to remote server: %s", remotePath)
	return remotePath, nil
}

// ExtractHostnameFromCloudInit extracts hostname from cloud-init YAML (like vSphere)
func (c *CloudInitProvider) ExtractHostnameFromCloudInit(cloudInitData string) string {
	lines := strings.Split(cloudInitData, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "hostname:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				hostname := strings.TrimSpace(parts[1])
				hostname = strings.Trim(hostname, "\"' ")
				return hostname
			}
		}
	}
	return ""
}

// CleanupCloudInit removes temporary cloud-init files for an instance
func (c *CloudInitProvider) CleanupCloudInit(instanceID string) error {
	instanceDir := filepath.Join(c.tempDir, instanceID)
	if err := os.RemoveAll(instanceDir); err != nil {
		log.Printf("WARN Failed to cleanup cloud-init files for %s: %v", instanceID, err)
		return err
	}
	
	log.Printf("INFO Cleaned up cloud-init files for instance: %s", instanceID)
	return nil
}

// ValidateCloudInitData validates the provided cloud-init YAML
func (c *CloudInitProvider) ValidateCloudInitData(cloudInitData string) error {
	// Basic validation - check if it looks like valid YAML
	if strings.TrimSpace(cloudInitData) == "" {
		return fmt.Errorf("cloud-init data is empty")
	}
	
	// Check for common cloud-init directives
	commonDirectives := []string{"#cloud-config", "hostname:", "users:", "packages:", "runcmd:", "write_files:"}
	hasValidDirective := false
	
	for _, directive := range commonDirectives {
		if strings.Contains(cloudInitData, directive) {
			hasValidDirective = true
			break
		}
	}
	
	if !hasValidDirective {
		log.Printf("WARN Cloud-init data may not be valid - no common directives found")
	}
	
	return nil
}
