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
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Clean provider implementation using only virsh

// Create creates a new VM using virsh with full cloud-init support
func (p *Provider) Create(ctx context.Context, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	log.Printf("INFO Creating VM with cloud-init support: %s", req.Name)

	if p.virshProvider == nil {
		return contracts.CreateResponse{}, contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Check if domain already exists
	domains, err := p.virshProvider.listDomains(ctx)
	if err != nil {
		return contracts.CreateResponse{}, contracts.NewRetryableError("failed to list existing domains", err)
	}

	for _, domain := range domains {
		if domain.Name == req.Name {
			log.Printf("INFO Domain %s already exists with state: %s", req.Name, domain.State)
			return contracts.CreateResponse{
				ID: req.Name,
			}, nil
		}
	}

	// Create VM with cloud-init support
	vmID, err := p.createVMWithCloudInit(ctx, req)
	if err != nil {
		return contracts.CreateResponse{}, contracts.NewRetryableError("failed to create VM", err)
	}

	log.Printf("INFO Successfully created VM: %s with ID: %s", req.Name, vmID)
	return contracts.CreateResponse{
		ID: vmID,
	}, nil
}

// createVMWithCloudInit creates a VM with comprehensive cloud-init support and storage management
func (p *Provider) createVMWithCloudInit(ctx context.Context, req contracts.CreateRequest) (string, error) {
	log.Printf("INFO Creating VM with enhanced cloud-init configuration and storage: %s", req.Name)

	// Initialize providers
	cloudInitProvider := NewCloudInitProvider(p.virshProvider)
	storageProvider := NewStorageProvider(p.virshProvider)

	// Ensure default storage pool exists and is active
	if err := storageProvider.EnsureDefaultStoragePool(ctx); err != nil {
		return "", fmt.Errorf("failed to ensure storage pool: %w", err)
	}

	// Create disk image from template or create empty disk
	diskVolumeName := fmt.Sprintf("%s-disk", req.Name)
	var diskPath string

	// Get disk size from VMClass (default to 20GB if not specified)
	diskSizeGB := p.extractDiskSize(req)
	log.Printf("INFO Using disk size: %dGB", diskSizeGB)

	// Check if VMImage is specified in the request
	if imageSpec := p.extractImageSpec(req); imageSpec != "" {
		log.Printf("INFO Creating disk from image template: %s", imageSpec)

		// Try to create volume from predefined template
		volume, err := storageProvider.CreateVolumeFromTemplate(ctx, imageSpec, diskVolumeName, "default", diskSizeGB)
		if err != nil {
			// Fallback: try to download directly if it's a URL
			if strings.HasPrefix(imageSpec, "http") {
				volume, err = storageProvider.DownloadCloudImage(ctx, imageSpec, diskVolumeName, "default", diskSizeGB)
			}
			if err != nil {
				return "", fmt.Errorf("failed to create disk from image: %w", err)
			}
		}
		diskPath = volume.Path
	} else {
		// Create empty disk volume
		log.Printf("INFO Creating empty disk volume: %s", diskVolumeName)
		volume, err := storageProvider.CreateVolume(ctx, "default", diskVolumeName, "qcow2", diskSizeGB)
		if err != nil {
			return "", fmt.Errorf("failed to create disk volume: %w", err)
		}
		diskPath = volume.Path
	}

	// Prepare cloud-init if provided
	var cloudInitISOPath string
	if req.UserData != nil && req.UserData.CloudInitData != "" {
		log.Printf("INFO Preparing cloud-init configuration for VM: %s", req.Name)

		// Extract hostname from cloud-init data
		hostname := cloudInitProvider.ExtractHostnameFromCloudInit(req.UserData.CloudInitData)
		if hostname == "" {
			hostname = req.Name // fallback to VM name
		}

		// Validate cloud-init data
		if err := cloudInitProvider.ValidateCloudInitData(req.UserData.CloudInitData); err != nil {
			return "", fmt.Errorf("invalid cloud-init data: %w", err)
		}

		// Prepare cloud-init configuration
		cloudInitConfig := CloudInitConfig{
			UserData:   req.UserData.CloudInitData,
			InstanceID: req.Name,
			Hostname:   hostname,
		}

		var err error
		cloudInitISOPath, err = cloudInitProvider.PrepareCloudInit(ctx, cloudInitConfig)
		if err != nil {
			return "", fmt.Errorf("failed to prepare cloud-init: %w", err)
		}

		// Cleanup cloud-init files when done (defer)
		defer func() {
			if cleanupErr := cloudInitProvider.CleanupCloudInit(req.Name); cleanupErr != nil {
				log.Printf("WARN Failed to cleanup cloud-init files: %v", cleanupErr)
			}
		}()
	} else {
		// Generate default cloud-init for Ubuntu images
		log.Printf("INFO Generating default cloud-init configuration for VM: %s", req.Name)

		defaultCloudInit := p.generateDefaultCloudInit(req.Name)
		cloudInitConfig := CloudInitConfig{
			UserData:   defaultCloudInit,
			InstanceID: req.Name,
			Hostname:   req.Name,
		}

		var err error
		cloudInitISOPath, err = cloudInitProvider.PrepareCloudInit(ctx, cloudInitConfig)
		if err != nil {
			log.Printf("WARN Failed to prepare default cloud-init: %v", err)
			// Continue without cloud-init
		} else {
			// Cleanup cloud-init files when done (defer)
			defer func() {
				if cleanupErr := cloudInitProvider.CleanupCloudInit(req.Name); cleanupErr != nil {
					log.Printf("WARN Failed to cleanup cloud-init files: %v", cleanupErr)
				}
			}()
		}
	}

	// Generate domain XML with proper disk and cloud-init ISO
	domainXML, err := p.generateDomainXMLWithStorage(req, diskPath, cloudInitISOPath)
	if err != nil {
		return "", fmt.Errorf("failed to generate domain XML: %w", err)
	}

	// Create domain definition file
	if err := p.createDomainDefinition(ctx, req.Name, domainXML); err != nil {
		return "", fmt.Errorf("failed to create domain definition: %w", err)
	}

	// Define the domain in libvirt
	if err := p.defineDomain(ctx, req.Name); err != nil {
		return "", fmt.Errorf("failed to define domain: %w", err)
	}

	log.Printf("INFO Successfully created VM with storage and cloud-init: %s", req.Name)
	return req.Name, nil
}

// Delete removes a VM using virsh
func (p *Provider) Delete(ctx context.Context, id string) (taskRef string, err error) {
	log.Printf("INFO Deleting VM: %s", id)

	if p.virshProvider == nil {
		return "", contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Check if domain exists
	domains, err := p.virshProvider.listDomains(ctx)
	if err != nil {
		return "", contracts.NewRetryableError("failed to list domains", err)
	}

	domainExists := false
	for _, domain := range domains {
		if domain.Name == id {
			domainExists = true
			break
		}
	}

	if !domainExists {
		log.Printf("INFO Domain %s does not exist, already deleted", id)
		return "", nil
	}

	// Stop the domain if running
	if err := p.virshProvider.destroyDomain(ctx, id); err != nil {
		log.Printf("WARN Failed to destroy domain %s: %v", id, err)
		// Continue with undefine even if destroy fails
	}

	// Remove the domain definition
	if err := p.virshProvider.undefineDomain(ctx, id); err != nil {
		return "", contracts.NewRetryableError("failed to undefine domain", err)
	}

	log.Printf("INFO Successfully deleted domain: %s", id)
	return "", nil
}

// Power controls VM power state using virsh
func (p *Provider) Power(ctx context.Context, id string, op contracts.PowerOp) (taskRef string, err error) {
	log.Printf("INFO Power operation %s on VM: %s", op, id)

	if p.virshProvider == nil {
		return "", contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	switch op {
	case contracts.PowerOpOn:
		err = p.virshProvider.startDomain(ctx, id)
	case contracts.PowerOpOff:
		err = p.virshProvider.stopDomain(ctx, id)
	case contracts.PowerOpReboot:
		// Restart by stopping then starting
		if stopErr := p.virshProvider.stopDomain(ctx, id); stopErr != nil {
			log.Printf("WARN Failed to stop domain for reboot: %v", stopErr)
		}
		err = p.virshProvider.startDomain(ctx, id)
	case contracts.PowerOpShutdownGraceful:
		// Graceful shutdown for libvirt - attempt guest shutdown, fallback to force stop
		err = p.virshProvider.shutdownDomain(ctx, id)
		if err != nil {
			log.Printf("WARN Graceful shutdown failed for %s, falling back to force stop: %v", id, err)
			err = p.virshProvider.stopDomain(ctx, id)
		}
	default:
		return "", contracts.NewInvalidSpecError(fmt.Sprintf("unsupported power operation: %s", op), nil)
	}

	if err != nil {
		return "", contracts.NewRetryableError(fmt.Sprintf("failed to perform power operation %s", op), err)
	}

	log.Printf("INFO Successfully performed power operation %s on %s", op, id)
	return "", nil
}

// Reconfigure updates VM configuration using virsh
func (p *Provider) Reconfigure(ctx context.Context, id string, desired contracts.CreateRequest) (taskRef string, err error) {
	log.Printf("INFO Reconfiguring VM: %s", id)

	if p.virshProvider == nil {
		return "", contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	hasChanges := false
	requiresRestart := false

	// Get current domain state
	domainState, err := p.virshProvider.getDomainState(ctx, id)
	if err != nil {
		return "", contracts.NewRetryableError("failed to get domain state", err)
	}

	isRunning := domainState == "running"
	log.Printf("INFO Domain %s current state: %s", id, domainState)

	// Get current domain info for comparison
	currentInfo, err := p.virshProvider.getDomainInfo(ctx, id)
	if err != nil {
		return "", contracts.NewRetryableError("failed to get current domain info", err)
	}

	// Handle CPU changes
	if desired.Class.CPU > 0 {
		currentCPUs, err := p.extractCPUCount(currentInfo)
		if err == nil && currentCPUs != desired.Class.CPU {
			log.Printf("INFO CPU change requested for %s: %d -> %d", id, currentCPUs, desired.Class.CPU)

			if isRunning {
				// Try online CPU change with --live flag
				_, err = p.virshProvider.runVirshCommand(ctx, "setvcpus", id,
					fmt.Sprintf("%d", desired.Class.CPU), "--live")
				if err != nil {
					log.Printf("WARN Online CPU change failed, will require restart: %v", err)
					requiresRestart = true
				} else {
					log.Printf("INFO Successfully changed CPUs online for domain: %s", id)
					hasChanges = true
				}
			} else {
				// Domain is off, change config
				_, err = p.virshProvider.runVirshCommand(ctx, "setvcpus", id,
					fmt.Sprintf("%d", desired.Class.CPU), "--config")
				if err != nil {
					log.Printf("WARN Failed to set CPUs in config: %v", err)
					requiresRestart = true
				} else {
					hasChanges = true
				}
			}
		}
	}

	// Handle Memory changes
	if desired.Class.MemoryMiB > 0 {
		currentMemoryKB, err := p.extractMemoryKB(currentInfo)
		desiredMemoryKB := int64(desired.Class.MemoryMiB) * 1024 // Convert MiB to KiB

		if err == nil && currentMemoryKB != desiredMemoryKB {
			log.Printf("INFO Memory change requested for %s: %d KiB -> %d KiB", id, currentMemoryKB, desiredMemoryKB)

			if isRunning {
				// Try online memory change with --live flag
				_, err = p.virshProvider.runVirshCommand(ctx, "setmem", id,
					fmt.Sprintf("%dK", desiredMemoryKB), "--live")
				if err != nil {
					log.Printf("WARN Online memory change failed, will require restart: %v", err)
					requiresRestart = true
				} else {
					log.Printf("INFO Successfully changed memory online for domain: %s", id)
					hasChanges = true
				}
			} else {
				// Domain is off, change config
				_, err = p.virshProvider.runVirshCommand(ctx, "setmem", id,
					fmt.Sprintf("%dK", desiredMemoryKB), "--config")
				if err != nil {
					log.Printf("WARN Failed to set memory in config: %v", err)
					requiresRestart = true
				} else {
					// Also update max memory
					_, _ = p.virshProvider.runVirshCommand(ctx, "setmaxmem", id,
						fmt.Sprintf("%dK", desiredMemoryKB), "--config")
					hasChanges = true
				}
			}
		}
	}

	// Handle Disk changes
	if len(desired.Disks) > 0 || (desired.Class.DiskDefaults != nil && desired.Class.DiskDefaults.SizeGiB > 0) {
		storageProvider := NewStorageProvider(p.virshProvider)

		// Get desired disk size
		var desiredDiskGB int
		if desired.Class.DiskDefaults != nil && desired.Class.DiskDefaults.SizeGiB > 0 {
			desiredDiskGB = int(desired.Class.DiskDefaults.SizeGiB)
		}

		if desiredDiskGB > 0 {
			// Find the VM's disk volume
			volumeName := fmt.Sprintf("%s-disk", id)

			// Try to resize the volume
			log.Printf("INFO Attempting to resize disk for VM %s to %dGB", id, desiredDiskGB)
			err = storageProvider.ResizeVolume(ctx, "default", volumeName, desiredDiskGB)
			if err != nil {
				log.Printf("WARN Disk resize failed: %v", err)
				// Disk resize failure is not fatal, just log it
			} else {
				log.Printf("INFO Successfully resized disk for VM: %s", id)
				hasChanges = true
			}
		}
	}

	// Log reconfiguration results
	if !hasChanges && !requiresRestart {
		log.Printf("INFO No configuration changes needed for domain: %s", id)
		return "", nil
	}

	if requiresRestart {
		log.Printf("WARN Some changes for domain %s require a restart to take effect", id)
		// Note: The caller (controller) should handle restarting the VM if needed
	}

	log.Printf("INFO Successfully reconfigured domain: %s", id)
	return "", nil
}

// getVNCPort extracts the VNC port from domain XML
func (p *Provider) getVNCPort(ctx context.Context, domainName string) (int, error) {
	// Get domain XML
	result, err := p.virshProvider.runVirshCommand(ctx, "dumpxml", domainName)
	if err != nil {
		return 0, fmt.Errorf("failed to get domain XML: %w", err)
	}

	// Parse XML to find VNC port
	// Look for <graphics type='vnc' port='XXXX'/>
	lines := strings.Split(result.Stdout, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "<graphics") && strings.Contains(line, "type='vnc'") {
			// Extract port attribute
			if portIdx := strings.Index(line, "port='"); portIdx != -1 {
				portStart := portIdx + 6 // len("port='")
				portEnd := strings.Index(line[portStart:], "'")
				if portEnd > 0 {
					portStr := line[portStart : portStart+portEnd]
					port, err := strconv.Atoi(portStr)
					if err == nil && port > 0 {
						return port, nil
					}
				}
			}
		}
	}

	return 0, fmt.Errorf("VNC port not found in domain XML")
}

// extractCPUCount extracts the CPU count from domain info map
func (p *Provider) extractCPUCount(domainInfo map[string]string) (int32, error) {
	cpuStr, exists := domainInfo["CPU(s)"]
	if !exists {
		return 0, fmt.Errorf("CPU count not found in domain info")
	}

	cpuCount, err := strconv.ParseInt(cpuStr, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("failed to parse CPU count: %w", err)
	}

	return int32(cpuCount), nil
}

// extractMemoryKB extracts the memory in KiB from domain info map
func (p *Provider) extractMemoryKB(domainInfo map[string]string) (int64, error) {
	memStr, exists := domainInfo["Max memory"]
	if !exists {
		// Try alternative key
		memStr, exists = domainInfo["Used memory"]
		if !exists {
			return 0, fmt.Errorf("memory not found in domain info")
		}
	}

	// Parse memory string (format: "XXXXXX KiB")
	memStr = strings.TrimSpace(memStr)
	memStr = strings.TrimSuffix(memStr, " KiB")
	memStr = strings.TrimSuffix(memStr, " kB")

	memKB, err := strconv.ParseInt(memStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse memory: %w", err)
	}

	return memKB, nil
}

// Describe returns comprehensive VM information using virsh (enhanced monitoring like vSphere)
func (p *Provider) Describe(ctx context.Context, id string) (contracts.DescribeResponse, error) {
	log.Printf("INFO Describing VM with comprehensive monitoring: %s", id)

	if p.virshProvider == nil {
		return contracts.DescribeResponse{}, contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Get comprehensive domain information (now includes enhanced monitoring)
	domainInfo, err := p.virshProvider.getDomainInfo(ctx, id)
	if err != nil {
		return contracts.DescribeResponse{}, contracts.NewRetryableError("failed to get domain info", err)
	}

	// Initialize guest agent provider for enhanced guest information
	guestAgent := NewGuestAgentProvider(p.virshProvider)

	// Extract power state (libvirt uses different names than vSphere)
	powerState := p.mapLibvirtPowerState(domainInfo["State"])

	// Extract IP addresses from enhanced domain info
	var ips []string
	if guestIPs := domainInfo["guest_ip_addresses"]; guestIPs != "" {
		ips = strings.Split(guestIPs, ",")
		// Filter out empty strings
		var validIPs []string
		for _, ip := range ips {
			if strings.TrimSpace(ip) != "" {
				validIPs = append(validIPs, strings.TrimSpace(ip))
			}
		}
		ips = validIPs
	}

	// Get primary IP (first valid IP)
	primaryIP := ""
	if len(ips) > 0 {
		primaryIP = ips[0]
	}

	// Extract comprehensive information for ProviderRawJson (like vSphere)
	hostname := domainInfo["guest_hostname"]
	if hostname == "" {
		hostname = domainInfo["Name"] // fallback to domain name
	}

	// If VM is running, try to get enhanced guest information via QEMU Guest Agent
	if powerState == "On" {
		if guestInfo, err := guestAgent.GetGuestInfo(ctx, id); err == nil {
			// Enhanced Guest OS Information
			if guestInfo.OSName != "" {
				domainInfo["guest_os"] = guestInfo.OSName
				domainInfo["guest_os_version"] = guestInfo.OSVersion
				domainInfo["guest_os_pretty_name"] = guestInfo.OSPrettyName
				domainInfo["guest_kernel_release"] = guestInfo.OSKernelRelease
				domainInfo["guest_machine"] = guestInfo.OSMachine
			}

			// Guest Agent Status and Version
			domainInfo["guest_agent_status"] = guestInfo.AgentStatus
			if guestInfo.AgentVersion != "" {
				domainInfo["guest_agent_version"] = guestInfo.AgentVersion
			}

			// Enhanced Network Information from Guest Agent
			if len(guestInfo.NetworkInterfaces) > 0 {
				var guestIPs []string
				var interfaceNames []string

				for _, iface := range guestInfo.NetworkInterfaces {
					interfaceNames = append(interfaceNames, iface.Name)
					guestIPs = append(guestIPs, iface.IPAddresses...)

					// Add detailed network statistics
					domainInfo[fmt.Sprintf("net_%s_rx_bytes", iface.Name)] = fmt.Sprintf("%d", iface.Statistics.RxBytes)
					domainInfo[fmt.Sprintf("net_%s_tx_bytes", iface.Name)] = fmt.Sprintf("%d", iface.Statistics.TxBytes)
					domainInfo[fmt.Sprintf("net_%s_rx_packets", iface.Name)] = fmt.Sprintf("%d", iface.Statistics.RxPackets)
					domainInfo[fmt.Sprintf("net_%s_tx_packets", iface.Name)] = fmt.Sprintf("%d", iface.Statistics.TxPackets)
					domainInfo[fmt.Sprintf("net_%s_mac", iface.Name)] = iface.HardwareAddr
				}

				// Use guest agent IPs if available (more accurate than virsh)
				if len(guestIPs) > 0 {
					ips = guestIPs
					primaryIP = guestIPs[0]
				}
				domainInfo["guest_network_interfaces"] = strings.Join(interfaceNames, ",")
			}

			// Guest Filesystem Information
			if len(guestInfo.Filesystems) > 0 {
				var mountpoints []string
				var totalDiskSpace uint64
				var usedDiskSpace uint64

				for _, fs := range guestInfo.Filesystems {
					mountpoints = append(mountpoints, fs.Mountpoint)
					totalDiskSpace += fs.TotalBytes
					usedDiskSpace += fs.UsedBytes

					// Add per-filesystem statistics
					safeMountpoint := strings.ReplaceAll(strings.ReplaceAll(fs.Mountpoint, "/", "_"), "-", "_")
					domainInfo[fmt.Sprintf("fs_%s_total", safeMountpoint)] = fmt.Sprintf("%d", fs.TotalBytes)
					domainInfo[fmt.Sprintf("fs_%s_used", safeMountpoint)] = fmt.Sprintf("%d", fs.UsedBytes)
					domainInfo[fmt.Sprintf("fs_%s_free", safeMountpoint)] = fmt.Sprintf("%d", fs.FreeBytes)
					domainInfo[fmt.Sprintf("fs_%s_type", safeMountpoint)] = fs.Type
				}

				domainInfo["guest_filesystems"] = strings.Join(mountpoints, ",")
				domainInfo["guest_disk_total"] = fmt.Sprintf("%d", totalDiskSpace)
				domainInfo["guest_disk_used"] = fmt.Sprintf("%d", usedDiskSpace)
				domainInfo["guest_disk_free"] = fmt.Sprintf("%d", totalDiskSpace-usedDiskSpace)
			}

			// Guest Time Information
			if !guestInfo.GuestTime.IsZero() {
				domainInfo["guest_time"] = guestInfo.GuestTime.Format(time.RFC3339)
				domainInfo["guest_time_sync"] = "available"
			}

			// Guest Users Information
			if len(guestInfo.Users) > 0 {
				var users []string
				for _, user := range guestInfo.Users {
					if user.Domain != "" {
						users = append(users, fmt.Sprintf("%s@%s", user.User, user.Domain))
					} else {
						users = append(users, user.User)
					}
				}
				domainInfo["guest_users"] = strings.Join(users, ",")
				domainInfo["guest_user_count"] = fmt.Sprintf("%d", len(guestInfo.Users))
			}

			log.Printf("INFO Enhanced guest information collected via QEMU Guest Agent for domain: %s", id)
		} else {
			log.Printf("DEBUG QEMU Guest Agent not available for domain %s: %v", id, err)
		}
	}

	// Add comprehensive monitoring fields to domain info for ProviderRaw
	domainInfo["primary_ip"] = primaryIP
	domainInfo["hostname"] = hostname
	domainInfo["tools_status"] = p.getToolsStatus(domainInfo)
	domainInfo["power_state_mapped"] = string(powerState)

	// Ensure guest OS is properly set
	if domainInfo["guest_os"] == "" && domainInfo["OS Type"] != "" {
		domainInfo["guest_os"] = domainInfo["OS Type"]
	}

	// Generate console URL (VNC/SPICE access info)
	consoleURL := ""
	if powerState == "On" {
		// Try to get VNC display information
		vncPort, err := p.getVNCPort(ctx, id)
		if err == nil && vncPort > 0 {
			// Build VNC URL
			// Extract host from libvirt URI
			host := "localhost" // Default to localhost
			if p.virshProvider != nil && p.virshProvider.uri != "" {
				if parsedURI, err := url.Parse(p.virshProvider.uri); err == nil && parsedURI.Host != "" {
					host = parsedURI.Host
					// Remove port from host if present
					if colonIdx := strings.Index(host, ":"); colonIdx != -1 {
						host = host[:colonIdx]
					}
				}
			}
			consoleURL = fmt.Sprintf("vnc://%s:%d", host, vncPort)
			domainInfo["vnc_port"] = fmt.Sprintf("%d", vncPort)
		}
	}

	// Convert virsh domain info to contracts format
	response := contracts.DescribeResponse{
		Exists:      true,
		PowerState:  string(powerState),
		IPs:         ips,
		ConsoleURL:  consoleURL,
		ProviderRaw: domainInfo, // Pass the enhanced domain info as provider-specific data
	}

	log.Printf("INFO Domain %s comprehensive state: power=%s, ips=%v, monitoring_data=collected", id, response.PowerState, ips)
	return response, nil
}

// IsTaskComplete checks if a task is complete (virsh operations are usually synchronous)
func (p *Provider) IsTaskComplete(ctx context.Context, taskRef string) (done bool, err error) {
	// Most virsh operations are synchronous, so tasks are immediately complete
	return true, nil
}

// mapLibvirtPowerState maps libvirt power states to VirtRigaud standard power states
func (p *Provider) mapLibvirtPowerState(libvirtState string) contracts.PowerState {
	switch strings.ToLower(libvirtState) {
	case "running":
		return "On"
	case "shut off", "shutoff":
		return "Off"
	case "paused", "suspended":
		return "Off" // Treat paused/suspended as Off for consistency with vSphere
	case "in shutdown", "shutting down":
		return "Off" // Transitioning to off
	default:
		return "Off" // Default to Off for unknown states
	}
}

// getToolsStatus determines guest tools equivalent status
func (p *Provider) getToolsStatus(domainInfo map[string]string) string {
	// Check QEMU Guest Agent status first (most accurate)
	if agentStatus := domainInfo["guest_agent_status"]; agentStatus != "" {
		switch agentStatus {
		case "available":
			return "toolsOk"
		case "not_available":
			return "toolsNotInstalled"
		default:
			return "toolsNotRunning"
		}
	}

	// Fallback: Check if we have guest agent connectivity indicators
	if guestHost := domainInfo["guest_hostname"]; guestHost != "" {
		return "toolsOk" // Guest agent is working
	}
	if guestIPs := domainInfo["guest_ip_addresses"]; guestIPs != "" {
		return "toolsOk" // We can get IP addresses from guest
	}
	if guestOS := domainInfo["guest_os"]; guestOS != "" && guestOS != domainInfo["OS Type"] {
		return "toolsOk" // We have enhanced guest OS info
	}

	return "toolsNotInstalled" // No guest agent connectivity
}

// ExecuteGuestCommand executes a command inside the guest via QEMU Guest Agent
func (p *Provider) ExecuteGuestCommand(ctx context.Context, id, command string) (string, error) {
	log.Printf("INFO Executing guest command in VM %s: %s", id, command)

	if p.virshProvider == nil {
		return "", contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	guestAgent := NewGuestAgentProvider(p.virshProvider)
	result, err := guestAgent.ExecuteGuestCommand(ctx, id, command)
	if err != nil {
		return "", contracts.NewRetryableError("failed to execute guest command", err)
	}

	log.Printf("INFO Successfully executed guest command in VM: %s", id)
	return result, nil
}

// SyncGuestTime synchronizes the guest time with the host
func (p *Provider) SyncGuestTime(ctx context.Context, id string) error {
	log.Printf("INFO Synchronizing guest time for VM: %s", id)

	if p.virshProvider == nil {
		return contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	guestAgent := NewGuestAgentProvider(p.virshProvider)
	if err := guestAgent.SetGuestTime(ctx, id); err != nil {
		return contracts.NewRetryableError("failed to sync guest time", err)
	}

	log.Printf("INFO Successfully synchronized guest time for VM: %s", id)
	return nil
}

// GetGuestInfo retrieves detailed guest information via QEMU Guest Agent
func (p *Provider) GetGuestInfo(ctx context.Context, id string) (*GuestAgentInfo, error) {
	log.Printf("INFO Retrieving detailed guest information for VM: %s", id)

	if p.virshProvider == nil {
		return nil, contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	guestAgent := NewGuestAgentProvider(p.virshProvider)
	guestInfo, err := guestAgent.GetGuestInfo(ctx, id)
	if err != nil {
		return nil, contracts.NewRetryableError("failed to get guest info", err)
	}

	log.Printf("INFO Successfully retrieved guest information for VM: %s", id)
	return guestInfo, nil
}

// extractImageSpec extracts the image specification from the request
func (p *Provider) extractImageSpec(req contracts.CreateRequest) string {
	// Check for image specification in the request name or other fields
	// For now, default to Ubuntu 22.04 as a bootable cloud image
	// This can be extended to support different image specifications

	// Default to Ubuntu 22.04 if no image specified
	return "ubuntu-22.04-server"
}

// extractDiskSize extracts the disk size from VMClass DiskDefaults
func (p *Provider) extractDiskSize(req contracts.CreateRequest) int {
	// Check if VMClass has DiskDefaults with size specified
	if req.Class.DiskDefaults != nil && req.Class.DiskDefaults.SizeGiB > 0 {
		log.Printf("INFO Using disk size from VMClass: %dGB", req.Class.DiskDefaults.SizeGiB)
		return int(req.Class.DiskDefaults.SizeGiB)
	}

	// Default to 20GB if not specified
	log.Printf("INFO No disk size specified in VMClass, using default: 20GB")
	return 20
}

// generateDefaultCloudInit generates a default cloud-init configuration
func (p *Provider) generateDefaultCloudInit(vmName string) string {
	return fmt.Sprintf(`#cloud-config
hostname: %s
users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN7lHIuo2QJBkdVDL79bl+tEmJh3pBz7rHImwvNMjenK
      
packages:
  - qemu-guest-agent
  - htop
  - stress
  
runcmd:
  - systemctl enable qemu-guest-agent
  - systemctl start qemu-guest-agent
  - echo "VM %s ready" > /tmp/vm-ready
  
final_message: "VM %s is ready!"
`, vmName, vmName, vmName)
}

// generateDomainXMLWithStorage creates libvirt domain XML with proper storage configuration
func (p *Provider) generateDomainXMLWithStorage(req contracts.CreateRequest, diskPath, cloudInitISOPath string) (string, error) {
	// Extract specifications from request
	cpuCount := int32(1)    // default
	memoryMB := int64(1024) // default 1GB

	// Extract from VMClass
	if req.Class.CPU > 0 {
		cpuCount = req.Class.CPU
	}

	if req.Class.MemoryMiB > 0 {
		memoryMB = int64(req.Class.MemoryMiB)
	}

	// Extract performance and security features
	var nestedVirtualization bool
	var vtdEnabled bool
	var secureBoot bool
	var tpmEnabled bool

	if req.Class.PerformanceProfile != nil {
		nestedVirtualization = req.Class.PerformanceProfile.NestedVirtualization
	}

	if req.Class.SecurityProfile != nil {
		vtdEnabled = req.Class.SecurityProfile.VTDEnabled
		secureBoot = req.Class.SecurityProfile.SecureBoot
		tpmEnabled = req.Class.SecurityProfile.TPMEnabled
	}

	// Generate UUID for the domain
	uuid := p.generateUUID()

	// Build disk devices XML
	diskDevicesXML := fmt.Sprintf(`    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='%s'/>
      <target dev='vda' bus='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x07' function='0x0'/>
    </disk>`, diskPath)

	// Add cloud-init ISO if available
	if cloudInitISOPath != "" {
		diskDevicesXML += fmt.Sprintf(`
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='%s'/>
      <target dev='hda' bus='ide'/>
      <readonly/>
      <address type='drive' controller='0' bus='0' target='0' unit='0'/>
    </disk>`, cloudInitISOPath)
	}

	// Build features XML based on configuration
	featuresXML := `    <acpi/>
    <apic/>`

	if secureBoot {
		featuresXML += `
    <smm state='on'>
      <tseg unit='MiB'>16</tseg>
    </smm>`
	}

	if vtdEnabled {
		featuresXML += `
    <iommu model='intel'/>` // or 'amd' for AMD systems
	}

	// Build CPU configuration with nested virtualization support
	cpuXML := `<cpu mode='host-model' check='partial'>`
	if nestedVirtualization {
		cpuXML += `
    <feature policy='require' name='vmx'/> <!-- Intel VT-x -->
    <feature policy='require' name='svm'/> <!-- AMD-V -->`
	}
	cpuXML += `</cpu>`

	// Build OS configuration with secure boot if needed
	osXML := `    <type arch='x86_64' machine='pc'>hvm</type>
    <boot dev='hd'/>
    <boot dev='cdrom'/>`

	if secureBoot {
		osXML = `    <type arch='x86_64' machine='q35'>hvm</type>
    <loader readonly='yes' type='pflash' secure='yes'>/usr/share/OVMF/OVMF_CODE_4M.secboot.fd</loader>
    <nvram template='/usr/share/OVMF/OVMF_VARS_4M.fd'/>
    <boot dev='hd'/>
    <boot dev='cdrom'/>`
	}

	// Build devices XML with TPM if needed
	devicesXML := fmt.Sprintf(`    <emulator>/usr/bin/qemu-system-x86_64</emulator>
%s`, diskDevicesXML)

	if tpmEnabled {
		devicesXML += `
    <tpm model='tpm-tis'>
      <backend type='emulator' version='2.0'/>
    </tpm>`
	}

	domainXML := fmt.Sprintf(`<domain type='qemu'>
  <name>%s</name>
  <uuid>%s</uuid>
  <memory unit='MiB'>%d</memory>
  <currentMemory unit='MiB'>%d</currentMemory>
  <vcpu placement='static'>%d</vcpu>
  <os>
%s
  </os>
  <features>
%s
  </features>
  %s
  <clock offset='utc'>
    <timer name='rtc' tickpolicy='catchup'/>
    <timer name='pit' tickpolicy='delay'/>
    <timer name='hpet' present='no'/>
  </clock>
  <on_poweroff>destroy</on_poweroff>
  <on_reboot>restart</on_reboot>
  <on_crash>destroy</on_crash>
  <devices>
%s
    <controller type='usb' index='0' model='ich9-ehci1'>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x05' function='0x7'/>
    </controller>
    <controller type='usb' index='0' model='ich9-uhci1'>
      <master startport='0'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x05' function='0x0' multifunction='on'/>
    </controller>
    <controller type='usb' index='0' model='ich9-uhci2'>
      <master startport='2'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x05' function='0x1'/>
    </controller>
    <controller type='usb' index='0' model='ich9-uhci3'>
      <master startport='4'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x05' function='0x2'/>
    </controller>
    <controller type='pci' index='0' model='pci-root'/>
    <controller type='ide' index='0'>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x01' function='0x1'/>
    </controller>
    <controller type='virtio-serial' index='0'>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x06' function='0x0'/>
    </controller>
    <interface type='user'>
      <model type='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x03' function='0x0'/>
    </interface>
    <serial type='pty'>
      <target type='isa-serial' port='0'>
        <model name='isa-serial'/>
      </target>
    </serial>
    <console type='pty'>
      <target type='serial' port='0'/>
    </console>
    <channel type='unix'>
      <target type='virtio' name='org.qemu.guest_agent.0'/>
      <address type='virtio-serial' controller='0' bus='0' port='1'/>
    </channel>
    <input type='tablet' bus='usb'>
      <address type='usb' bus='0' port='1'/>
    </input>
    <input type='mouse' bus='ps2'/>
    <input type='keyboard' bus='ps2'/>
    <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'>
      <listen type='address' address='127.0.0.1'/>
    </graphics>
    <sound model='ich6'>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x04' function='0x0'/>
    </sound>
    <video>
      <model type='cirrus' vram='16384' heads='1' primary='yes'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x02' function='0x0'/>
    </video>
    <memballoon model='virtio'>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x08' function='0x0'/>
    </memballoon>
    <controller type='usb' index='0' model='ich9-ehci1'>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x05' function='0x7'/>
    </controller>
    <controller type='usb' index='0' model='ich9-uhci1'>
      <master startport='0'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x05' function='0x0' multifunction='on'/>
    </controller>
    <controller type='usb' index='0' model='ich9-uhci2'>
      <master startport='2'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x05' function='0x1'/>
    </controller>
    <controller type='usb' index='0' model='ich9-uhci3'>
      <master startport='4'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x05' function='0x2'/>
    </controller>
    <controller type='pci' index='0' model='pci-root'/>
    <controller type='ide' index='0'>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x01' function='0x1'/>
    </controller>
    <controller type='virtio-serial' index='0'>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x06' function='0x0'/>
    </controller>
    <interface type='user'>
      <model type='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x03' function='0x0'/>
    </interface>
    <serial type='pty'>
      <target type='isa-serial' port='0'>
        <model name='isa-serial'/>
      </target>
    </serial>
    <console type='pty'>
      <target type='serial' port='0'/>
    </console>
    <channel type='unix'>
      <target type='virtio' name='org.qemu.guest_agent.0'/>
      <address type='virtio-serial' controller='0' bus='0' port='1'/>
    </channel>
    <input type='tablet' bus='usb'>
      <address type='usb' bus='0' port='1'/>
    </input>
    <input type='mouse' bus='ps2'/>
    <input type='keyboard' bus='ps2'/>
    <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'>
      <listen type='address' address='127.0.0.1'/>
    </graphics>
    <video>
      <model type='cirrus' vram='16384' heads='1' primary='yes'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x02' function='0x0'/>
    </video>
    <memballoon model='virtio'>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x08' function='0x0'/>
    </memballoon>
  </devices>
</domain>`,
		req.Name,
		uuid,
		memoryMB,
		memoryMB,
		cpuCount,
		osXML,
		featuresXML,
		cpuXML,
		devicesXML)

	return domainXML, nil
}

// generateUUID creates a simple UUID for the domain
func (p *Provider) generateUUID() string {
	// Simple UUID generation for demo - in production, use proper UUID library
	return fmt.Sprintf("550e8400-e29b-41d4-a716-%012d", time.Now().UnixNano()%1000000000000)
}

// createDomainDefinition writes the domain XML to a temporary file on the remote server
func (p *Provider) createDomainDefinition(ctx context.Context, domainName, domainXML string) error {
	// Create temporary file path on remote server
	remotePath := fmt.Sprintf("/tmp/%s-domain.xml", domainName)

	// Write domain XML to remote file using heredoc (similar to cloud-init approach)
	heredocMarker := "EOF_DOMAIN_" + fmt.Sprintf("%d", time.Now().UnixNano())
	command := fmt.Sprintf("cat > '%s' << '%s'\n%s\n%s", remotePath, heredocMarker, domainXML, heredocMarker)

	result, err := p.virshProvider.runVirshCommand(ctx, "!", "bash", "-c", command)
	if err != nil {
		return fmt.Errorf("failed to create domain definition file: %w, output: %s", err, result.Stderr)
	}

	log.Printf("INFO Created domain definition file: %s", remotePath)
	return nil
}

// defineDomain defines the domain in libvirt using the XML file
func (p *Provider) defineDomain(ctx context.Context, domainName string) error {
	// Define domain from XML file
	remotePath := fmt.Sprintf("/tmp/%s-domain.xml", domainName)

	result, err := p.virshProvider.runVirshCommand(ctx, "define", remotePath)
	if err != nil {
		return fmt.Errorf("failed to define domain: %w, output: %s", err, result.Stderr)
	}

	// Clean up temporary XML file
	_, cleanupErr := p.virshProvider.runVirshCommand(ctx, "!", "rm", "-f", remotePath)
	if cleanupErr != nil {
		log.Printf("WARN Failed to cleanup domain XML file: %v", cleanupErr)
	}

	log.Printf("INFO Successfully defined domain: %s", domainName)
	return nil
}

// SnapshotCreate creates a VM snapshot using virsh
func (p *Provider) SnapshotCreate(ctx context.Context, req contracts.SnapshotCreateRequest) (contracts.SnapshotCreateResponse, error) {
	log.Printf("INFO Creating snapshot for VM: %s", req.VmId)

	if p.virshProvider == nil {
		return contracts.SnapshotCreateResponse{}, contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Generate snapshot name if not provided
	snapshotName := req.NameHint
	if snapshotName == "" {
		snapshotName = fmt.Sprintf("snapshot-%d", time.Now().Unix())
	}

	// Sanitize snapshot name for virsh
	snapshotName = sanitizeSnapshotName(snapshotName)

	// Prepare snapshot description
	description := req.Description
	if description == "" {
		description = fmt.Sprintf("Snapshot created by VirtRigaud at %s", time.Now().Format(time.RFC3339))
	}

	// Check if domain exists and get its state
	domainState, err := p.virshProvider.getDomainState(ctx, req.VmId)
	if err != nil {
		return contracts.SnapshotCreateResponse{}, contracts.NewRetryableError("failed to get domain state", err)
	}

	log.Printf("INFO Domain %s is in state: %s", req.VmId, domainState)

	// Build virsh snapshot-create-as command
	args := []string{
		"snapshot-create-as",
		req.VmId,
		snapshotName,
		"--description", description,
		"--atomic", // Ensure atomic operation
	}

	// Determine snapshot type based on domain state and request
	if req.IncludeMemory && domainState == "running" {
		// Memory snapshot (full system checkpoint including RAM)
		log.Printf("INFO Creating memory snapshot (includes RAM state)")
		// No --disk-only flag = full snapshot with memory
	} else {
		// Disk-only snapshot (faster, no memory state)
		log.Printf("INFO Creating disk-only snapshot")
		args = append(args, "--disk-only")
	}

	// Execute snapshot creation
	result, err := p.virshProvider.runVirshCommand(ctx, args...)
	if err != nil {
		return contracts.SnapshotCreateResponse{}, fmt.Errorf("failed to create snapshot: %w", err)
	}

	log.Printf("INFO Snapshot created successfully: %s\nOutput: %s", snapshotName, result.Stdout)

	// Return snapshot ID (synchronous operation for libvirt)
	return contracts.SnapshotCreateResponse{
		SnapshotId: snapshotName,
		// No task reference - libvirt snapshots are synchronous
	}, nil
}

// SnapshotDelete deletes a VM snapshot using virsh
func (p *Provider) SnapshotDelete(ctx context.Context, vmId string, snapshotId string) (taskRef string, err error) {
	log.Printf("INFO Deleting snapshot %s from VM: %s", snapshotId, vmId)

	if p.virshProvider == nil {
		return "", contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Check if snapshot exists
	exists, err := p.virshProvider.snapshotExists(ctx, vmId, snapshotId)
	if err != nil {
		return "", fmt.Errorf("failed to check snapshot existence: %w", err)
	}

	if !exists {
		log.Printf("WARN Snapshot %s does not exist, considering deletion successful", snapshotId)
		return "", nil
	}

	// Delete the snapshot
	args := []string{
		"snapshot-delete",
		vmId,
		snapshotId,
	}

	result, err := p.virshProvider.runVirshCommand(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("failed to delete snapshot: %w", err)
	}

	log.Printf("INFO Snapshot deleted successfully: %s\nOutput: %s", snapshotId, result.Stdout)

	// Return empty task reference (synchronous operation)
	return "", nil
}

// SnapshotRevert reverts a VM to a snapshot using virsh
func (p *Provider) SnapshotRevert(ctx context.Context, vmId string, snapshotId string) (taskRef string, err error) {
	log.Printf("INFO Reverting VM %s to snapshot: %s", vmId, snapshotId)

	if p.virshProvider == nil {
		return "", contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Check if snapshot exists
	exists, err := p.virshProvider.snapshotExists(ctx, vmId, snapshotId)
	if err != nil {
		return "", fmt.Errorf("failed to check snapshot existence: %w", err)
	}

	if !exists {
		return "", fmt.Errorf("snapshot %s does not exist", snapshotId)
	}

	// Get current domain state
	domainState, err := p.virshProvider.getDomainState(ctx, vmId)
	if err != nil {
		return "", fmt.Errorf("failed to get domain state: %w", err)
	}

	log.Printf("INFO Domain %s current state: %s", vmId, domainState)

	// Revert to snapshot
	args := []string{
		"snapshot-revert",
		vmId,
		snapshotId,
		"--force", // Force revert even if domain is running
	}

	// If domain was running, keep it running after revert
	if domainState == "running" {
		args = append(args, "--running")
	}

	result, err := p.virshProvider.runVirshCommand(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("failed to revert to snapshot: %w", err)
	}

	log.Printf("INFO Successfully reverted to snapshot: %s\nOutput: %s", snapshotId, result.Stdout)

	// Return empty task reference (synchronous operation)
	return "", nil
}

// TaskStatus returns the status of a task (libvirt operations are mostly synchronous)
func (p *Provider) TaskStatus(ctx context.Context, taskRef string) (contracts.TaskStatus, error) {
	// LibVirt operations are synchronous, so if we have a taskRef, it's completed
	return contracts.TaskStatus{
		IsCompleted: true,
		Error:       "",
		Message:     "Task completed",
	}, nil
}
