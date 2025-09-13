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

// createVMWithCloudInit creates a VM with comprehensive cloud-init support
func (p *Provider) createVMWithCloudInit(ctx context.Context, req contracts.CreateRequest) (string, error) {
	log.Printf("INFO Creating VM with enhanced cloud-init configuration: %s", req.Name)
	
	// Initialize cloud-init provider
	cloudInitProvider := NewCloudInitProvider(p.virshProvider)
	
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
	}
	
	// Generate domain XML with specifications
	domainXML, err := p.generateDomainXML(req)
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
	
	// Attach cloud-init ISO if provided
	if cloudInitISOPath != "" {
		if err := cloudInitProvider.AttachCloudInitISO(ctx, req.Name, cloudInitISOPath); err != nil {
			log.Printf("WARN Failed to attach cloud-init ISO: %v", err)
			// Continue without cloud-init rather than failing
		} else {
			log.Printf("INFO Successfully attached cloud-init ISO to VM: %s", req.Name)
		}
	}
	
	log.Printf("INFO Successfully created VM definition: %s", req.Name)
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

	// TODO: Implement reconfiguration via virsh
	log.Printf("INFO Would reconfigure domain: %s", id)

	return "", nil
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
	
	// Add comprehensive monitoring fields to domain info for ProviderRaw
	domainInfo["primary_ip"] = primaryIP
	domainInfo["hostname"] = hostname
	domainInfo["tools_status"] = p.getToolsStatus(domainInfo)
	domainInfo["power_state_mapped"] = string(powerState)
	
	// Ensure guest OS is properly set
	if domainInfo["guest_os"] == "" && domainInfo["OS Type"] != "" {
		domainInfo["guest_os"] = domainInfo["OS Type"]
	}

	// Convert virsh domain info to contracts format
	response := contracts.DescribeResponse{
		Exists:      true,
		PowerState:  string(powerState),
		IPs:         ips,
		ConsoleURL:  "", // TODO: Generate VNC/console URL if needed
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
	// Check if we have guest agent connectivity
	if guestHost := domainInfo["guest_hostname"]; guestHost != "" {
		return "toolsOk" // Guest agent is working
	}
	if guestIPs := domainInfo["guest_ip_addresses"]; guestIPs != "" {
		return "toolsOk" // We can get IP addresses from guest
	}
	return "toolsNotInstalled" // No guest agent connectivity
}

// generateDomainXML creates libvirt domain XML from CreateRequest
func (p *Provider) generateDomainXML(req contracts.CreateRequest) (string, error) {
	// Extract specifications from request
	cpuCount := int32(1)   // default
	memoryMB := int64(1024) // default 1GB
	
	// Extract from VMClass
	if req.Class.CPU > 0 {
		cpuCount = req.Class.CPU
	}
	
	if req.Class.MemoryMiB > 0 {
		memoryMB = int64(req.Class.MemoryMiB)
	}
	
	// Basic domain XML template for cloud-init enabled VMs
	domainXML := fmt.Sprintf(`<domain type='qemu'>
  <name>%s</name>
  <uuid>%s</uuid>
  <memory unit='MiB'>%d</memory>
  <currentMemory unit='MiB'>%d</currentMemory>
  <vcpu placement='static'>%d</vcpu>
  <os>
    <type arch='x86_64' machine='pc'>hvm</type>
    <boot dev='hd'/>
    <boot dev='cdrom'/>
  </os>
  <features>
    <acpi/>
    <apic/>
  </features>
  <cpu mode='host-passthrough' check='none'/>
  <clock offset='utc'>
    <timer name='rtc' tickpolicy='catchup'/>
    <timer name='pit' tickpolicy='delay'/>
    <timer name='hpet' present='no'/>
  </clock>
  <on_poweroff>destroy</on_poweroff>
  <on_reboot>restart</on_reboot>
  <on_crash>destroy</on_crash>
  <devices>
    <emulator>/usr/bin/qemu-system-x86_64</emulator>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/%s.qcow2'/>
      <target dev='vda' bus='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x07' function='0x0'/>
    </disk>
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
  </devices>
</domain>`, 
		req.Name,
		p.generateUUID(),
		memoryMB,
		memoryMB,
		cpuCount,
		req.Name)
	
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
