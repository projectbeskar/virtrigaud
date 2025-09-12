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

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Clean provider implementation using only virsh

// Create creates a new VM using virsh
func (p *Provider) Create(ctx context.Context, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	log.Printf("INFO Creating VM: %s", req.Name)

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

	// TODO: Generate domain XML and create via virsh
	// For now, return success to test the basic flow
	log.Printf("INFO Would create new domain: %s", req.Name)

	return contracts.CreateResponse{
		ID: req.Name,
	}, nil
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
