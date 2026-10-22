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
)

// CloudInitConfig represents cloud-init configuration for libvirt VMs
type CloudInitConfig struct {
	UserData string // YAML cloud-init configuration
	MetaData string // Instance metadata (JSON)
	// InstanceID names the seed directory (<staging>/virtrigaud-cloudinit-
	// <InstanceID>.<random>: always the domain name, which Delete and the
	// seed reclaim look for) and, when MetaData is empty, is the generated
	// meta-data's instance-id. A clone passes its own MetaData, whose
	// instance-id is not the domain name (clone_seed.go).
	InstanceID string
	Hostname   string // VM hostname
	// NetworkConfig and VendorData are the NoCloud network-config and
	// vendor-data files, written into the seed only when non-empty (a clone
	// carries its source seed's).
	NetworkConfig string
	VendorData    string
}

// CloudInitProvider manages cloud-init ISO creation and attachment for libvirt
type CloudInitProvider struct {
	virshProvider *VirshProvider
	// seedDir is the host directory each create's seed directory is made in
	// (seedDirDefault unless the caller points it elsewhere; seed_dir.go).
	seedDir string
}

// NewCloudInitProvider creates a new cloud-init provider for libvirt
func NewCloudInitProvider(virshProvider *VirshProvider) *CloudInitProvider {
	return &CloudInitProvider{
		virshProvider: virshProvider,
		seedDir:       seedDirDefault,
	}
}

// PrepareCloudInit builds the NoCloud seed ISO for one create on the libvirt
// host and returns the ISO's path.
//
// The seed lives in a directory made for this create alone, inside the seed
// directory (seed_dir.go, checked by ensureSeedRoot: owned by the SSH user, not
// listable by anyone else): <seed dir>/virtrigaud-cloudinit-<instance ID>.<random>,
// created by `mktemp -d` (exclusively, mode 0700), so a concurrent create — of
// any VM — can never overwrite this VM's user-data (which may carry secrets),
// and nothing else on the host can pre-create or predict it. user-data and
// meta-data are written private to the SSH user, the ISO is built under umask
// 0077, and sealSeedDir then sets it to 0600 explicitly, keeps the SSH user's
// private copy of it, removes user-data and meta-data, and opens the directory
// to traverse-only (cloudInitSeedDirMode) so the qemu process can open the ISO
// by its exact path. Any failure removes the directory again.
func (c *CloudInitProvider) PrepareCloudInit(ctx context.Context, config CloudInitConfig) (string, error) {
	log.Printf("INFO Preparing cloud-init configuration for instance: %s", config.InstanceID)

	// Work remotely on the libvirt host to avoid read-only filesystem issues.
	remoteDir, err := newSeedDir(ctx, c.virshProvider, c.seedDir, config.InstanceID)
	if err != nil {
		return "", fmt.Errorf("failed to create remote cloud-init directory: %w", err)
	}
	prepared := false
	defer func() {
		if !prepared {
			removeHostPath(ctx, c.virshProvider, remoteDir, true)
		}
	}()

	// Generate metadata if not provided
	if config.MetaData == "" {
		config.MetaData = c.generateMetaData(config.InstanceID, config.Hostname)
	}
	log.Printf("DEBUG Cloud-init meta-data ready (length=%d)", len(config.MetaData))

	// Write user-data file remotely
	userDataPath := filepath.Join(remoteDir, cloudInitUserDataName)
	if err := c.writeRemoteFile(ctx, userDataPath, config.UserData); err != nil {
		return "", fmt.Errorf("failed to write remote user-data: %w", err)
	}

	// Write meta-data file remotely
	metaDataPath := filepath.Join(remoteDir, cloudInitMetaDataName)
	if err := c.writeRemoteFile(ctx, metaDataPath, config.MetaData); err != nil {
		return "", fmt.Errorf("failed to write remote meta-data: %w", err)
	}

	// The optional NoCloud files, in a fixed order (a clone carries its
	// source seed's).
	for _, f := range []struct{ name, content string }{
		{seedFileNetworkConfig, config.NetworkConfig},
		{seedFileVendorData, config.VendorData},
	} {
		if f.content == "" {
			continue
		}
		if err := c.writeRemoteFile(ctx, filepath.Join(remoteDir, f.name), f.content); err != nil {
			return "", fmt.Errorf("failed to write remote %s: %w", f.name, err)
		}
	}

	// Create cloud-init ISO using genisoimage (NoCloud datasource) on remote host
	isoPath := filepath.Join(remoteDir, cloudInitISOName)
	if err := c.createRemoteCloudInitISO(ctx, remoteDir, isoPath); err != nil {
		return "", fmt.Errorf("failed to create remote cloud-init ISO: %w", err)
	}

	// The ISO private (0600), user-data and meta-data gone, and the directory
	// traverse-only for the domain's qemu process.
	if err := sealSeedDir(ctx, c.virshProvider, remoteDir); err != nil {
		return "", fmt.Errorf("failed to seal the cloud-init seed directory: %w", err)
	}

	prepared = true
	log.Printf("INFO Successfully created remote cloud-init ISO: %s", isoPath)
	return isoPath, nil
}

// writeRemoteFile writes content to a file on the remote libvirt host. The
// content (cloud-init user-data may carry secrets) travels over the SSH
// session's stdin — never inside the command line, never through a heredoc a
// crafted line could terminate — and the file is created private to the SSH
// user (VirshProvider.writeRemoteFile).
func (c *CloudInitProvider) writeRemoteFile(ctx context.Context, remotePath, content string) error {
	if err := c.virshProvider.writeRemoteFile(ctx, remotePath, []byte(content)); err != nil {
		return fmt.Errorf("failed to write remote file %s: %w", remotePath, err)
	}

	log.Printf("DEBUG Wrote remote file: %s", remotePath)
	return nil
}

// createRemoteCloudInitISO creates an ISO9660 filesystem with cloud-init files
// on the remote host, under umask seedFileUmask so the ISO is private to the
// SSH user from the moment it exists (sealSeedDir then sets its mode
// explicitly).
func (c *CloudInitProvider) createRemoteCloudInitISO(ctx context.Context, sourceDir, isoPath string) error {
	// Use genisoimage to create NoCloud datasource ISO on remote host
	// The ISO must contain user-data and meta-data files at the root
	result, err := runHost(ctx, c.virshProvider, withUmask(seedFileUmask, "genisoimage",
		"-output", isoPath,
		"-volid", "cidata", // Volume ID for NoCloud datasource
		"-joliet", // Enable Joliet extensions
		"-rock",   // Enable Rock Ridge extensions
		"-input-charset", "utf-8",
		sourceDir)...)

	if err != nil {
		stderr := ""
		if result != nil {
			stderr = result.Stderr
		}
		return fmt.Errorf("genisoimage failed on remote host: %w, output: %s", err, stderr)
	}

	log.Printf("DEBUG Created remote cloud-init ISO with genisoimage: %s", isoPath)
	return nil
}

// generateMetaData creates basic metadata for the VM instance
func (c *CloudInitProvider) generateMetaData(instanceID, hostname string) string {
	// Basic metadata following cloud-init NoCloud format with default DHCP networking
	//
	// Multi-Distribution Support:
	// - Ubuntu/Debian: Uses netplan or NetworkManager depending on version
	// - RHEL/CentOS/Rocky/Alma: Uses NetworkManager or network-scripts
	// - Fedora: Uses NetworkManager
	// - openSUSE/SLES: Uses Wicked or NetworkManager
	// - Arch: Uses systemd-networkd or NetworkManager
	//
	// Cloud-init version 1 format:
	// - Immediate application without requiring reboot
	// - Cloud-init translates to distro-specific configs automatically
	// - Multiple interface names ensure compatibility with different naming schemes:
	//   * eth0 - Traditional naming (older distros, custom configs)
	//   * ens3 - Predictable naming for virtio (most common in VMs)
	//   * enp0s3 - Predictable naming with PCI info
	//   * enp1s0 - Alternative PCI-based naming
	//
	// User Override: Users can provide custom networking in user-data which takes precedence.
	// Windows Support: Windows VMs should use cloudbase-init specific configuration (see docs).
	metadata := fmt.Sprintf(`instance-id: %s
local-hostname: %s
network:
  version: 1
  config:
    # Traditional interface naming (older distros, custom configs)
    - type: physical
      name: eth0
      subnets:
        - type: dhcp4
        - type: dhcp6
    # Predictable naming - most common for virtio NICs in VMs
    - type: physical
      name: ens3
      subnets:
        - type: dhcp4
        - type: dhcp6
    # Predictable naming with PCI slot info
    - type: physical
      name: enp0s3
      subnets:
        - type: dhcp4
        - type: dhcp6
    # Alternative PCI-based naming
    - type: physical
      name: enp1s0
      subnets:
        - type: dhcp4
        - type: dhcp6
`, instanceID, hostname)

	return metadata
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

// CleanupCloudInit removes, on the libvirt host, the seed directory
// PrepareCloudInit made for isoPath — used when the create that prepared it
// fails. It is best-effort (failures are logged) and refuses any path that is
// not a seed directory directly inside the seed directory, so it can never
// remove anything else.
//
// (It replaces a cleanup that ran os.RemoveAll on the PROVIDER's filesystem:
// a no-op for an SSH host, and on a local connection it deleted the ISO a
// just-defined domain still referenced.)
func (c *CloudInitProvider) CleanupCloudInit(ctx context.Context, isoPath string) {
	dir := filepath.Dir(isoPath)
	if !isSeedDirIn(dir, c.seedDir) {
		log.Printf("WARN Not removing %s: it is not a cloud-init seed directory", dir)
		return
	}
	removeHostPath(ctx, c.virshProvider, dir, true)
	log.Printf("INFO Cleaned up cloud-init seed directory: %s", dir)
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
