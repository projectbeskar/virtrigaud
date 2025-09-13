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
	"time"
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

// PrepareCloudInit creates cloud-init configuration remotely and returns the ISO path
func (c *CloudInitProvider) PrepareCloudInit(ctx context.Context, config CloudInitConfig) (string, error) {
	log.Printf("INFO Preparing cloud-init configuration for instance: %s", config.InstanceID)
	
	// Work remotely on the libvirt host to avoid read-only filesystem issues
	remoteDir := fmt.Sprintf("/tmp/virtrigaud-cloudinit/%s", config.InstanceID)
	
	// Create remote directory
	if _, err := c.virshProvider.runVirshCommand(ctx, "!", "mkdir", "-p", remoteDir); err != nil {
		return "", fmt.Errorf("failed to create remote cloud-init directory: %w", err)
	}
	
	// Generate metadata if not provided
	if config.MetaData == "" {
		config.MetaData = c.generateMetaData(config.InstanceID, config.Hostname)
	}
	
	// Write user-data file remotely
	userDataPath := filepath.Join(remoteDir, "user-data")
	if err := c.writeRemoteFile(ctx, userDataPath, config.UserData); err != nil {
		return "", fmt.Errorf("failed to write remote user-data: %w", err)
	}
	
	// Write meta-data file remotely
	metaDataPath := filepath.Join(remoteDir, "meta-data")
	if err := c.writeRemoteFile(ctx, metaDataPath, config.MetaData); err != nil {
		return "", fmt.Errorf("failed to write remote meta-data: %w", err)
	}
	
	// Create cloud-init ISO using genisoimage (NoCloud datasource) on remote host
	isoPath := filepath.Join(remoteDir, "cloud-init.iso")
	if err := c.createRemoteCloudInitISO(ctx, remoteDir, isoPath); err != nil {
		return "", fmt.Errorf("failed to create remote cloud-init ISO: %w", err)
	}
	
	log.Printf("INFO Successfully created remote cloud-init ISO: %s", isoPath)
	return isoPath, nil
}

// writeRemoteFile writes content to a file on the remote libvirt host
func (c *CloudInitProvider) writeRemoteFile(ctx context.Context, remotePath, content string) error {
	// Use cat with heredoc to write content to remote file (handles multiline content)
	// This approach avoids shell escaping issues with printf
	heredocMarker := "EOF_CLOUDINIT_" + fmt.Sprintf("%d", time.Now().UnixNano())
	command := fmt.Sprintf("cat > '%s' << '%s'\n%s\n%s", remotePath, heredocMarker, content, heredocMarker)
	
	_, err := c.virshProvider.runVirshCommand(ctx, "!", "bash", "-c", command)
	if err != nil {
		return fmt.Errorf("failed to write remote file %s: %w", remotePath, err)
	}
	
	log.Printf("DEBUG Wrote remote file: %s", remotePath)
	return nil
}

// createRemoteCloudInitISO creates an ISO9660 filesystem with cloud-init files on remote host
func (c *CloudInitProvider) createRemoteCloudInitISO(ctx context.Context, sourceDir, isoPath string) error {
	// Use genisoimage to create NoCloud datasource ISO on remote host
	// The ISO must contain user-data and meta-data files at the root
	result, err := c.virshProvider.runVirshCommand(ctx, "!", "genisoimage", 
		"-output", isoPath,
		"-volid", "cidata",     // Volume ID for NoCloud datasource
		"-joliet",              // Enable Joliet extensions
		"-rock",                // Enable Rock Ridge extensions  
		"-input-charset", "utf-8",
		sourceDir)
	
	if err != nil {
		return fmt.Errorf("genisoimage failed on remote host: %w, output: %s", err, result.Stderr)
	}
	
	log.Printf("DEBUG Created remote cloud-init ISO with genisoimage: %s", isoPath)
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
	_, err := c.virshProvider.runVirshCommand(ctx, "!", "ssh", "wrkode@172.16.56.38", 
		"mkdir", "-p", remoteDir)
	if err != nil {
		log.Printf("WARN Failed to create remote directory (may already exist): %v", err)
	}
	
	// Copy ISO file using scp
	result, err := c.virshProvider.runVirshCommand(ctx, "!", "scp", localPath, 
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
