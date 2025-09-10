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
	"strings"

	libvirt "libvirt.org/go/libvirt"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// findDomain finds a domain by name
func (p *Provider) findDomain(name string) (*libvirt.Domain, error) {
	if err := p.ensureConnection(context.Background()); err != nil {
		return nil, err
	}

	domain, err := p.conn.LookupDomainByName(name)
	if err != nil {
		return nil, err
	}

	return domain, nil
}

// findDomainByUUID finds a domain by its UUID
func (p *Provider) findDomainByUUID(uuid string) (*libvirt.Domain, error) {
	if err := p.ensureConnection(context.Background()); err != nil {
		return nil, err
	}

	domain, err := p.conn.LookupDomainByUUIDString(uuid)
	if err != nil {
		return nil, err
	}

	return domain, nil
}

// createDomain creates a new domain
func (p *Provider) createDomain(ctx context.Context, req contracts.CreateRequest) (string, error) {
	// Create storage volume first
	volumePath, err := p.createStorageVolume(ctx, req)
	if err != nil {
		return "", contracts.NewRetryableError("failed to create storage volume", err)
	}

	// Create cloud-init ISO if needed
	var cloudInitPath string
	if req.UserData != nil && req.UserData.CloudInitData != "" {
		cloudInitPath, err = p.createCloudInitISO(ctx, req.Name, req.UserData.CloudInitData)
		if err != nil {
			return "", contracts.NewRetryableError("failed to create cloud-init ISO", err)
		}
	}

	// Generate domain XML configuration
	domainXML := p.generateDomainXML(req, volumePath, cloudInitPath)

	// Define the domain
	domain, err := p.conn.DomainDefineXML(domainXML)
	if err != nil {
		return "", contracts.NewRetryableError("failed to define domain", err)
	}
	defer domain.Free() //nolint:errcheck // Libvirt domain cleanup not critical in defer

	// Start the domain if power state is On
	if req.Class.ExtraConfig["autostart"] == "true" || strings.ToLower(req.Tags[0]) == "autostart" {
		err = domain.Create()
		if err != nil {
			return "", contracts.NewRetryableError("failed to start domain", err)
		}
	}

	// Get the domain UUID
	uuid, err := domain.GetUUIDString()
	if err != nil {
		return "", contracts.NewRetryableError("failed to get domain UUID", err)
	}

	return uuid, nil
}

// generateDomainXML generates the XML configuration for a Libvirt domain
func (p *Provider) generateDomainXML(req contracts.CreateRequest, volumePath, cloudInitPath string) string {
	// Generate a basic domain XML configuration
	// This is a simplified version - in production, you'd want more sophisticated XML generation

	memoryKB := req.Class.MemoryMiB * 1024 // Convert to KB for Libvirt

	xml := fmt.Sprintf(`<domain type='kvm'>
  <name>%s</name>
  <memory unit='KiB'>%d</memory>
  <currentMemory unit='KiB'>%d</currentMemory>
  <vcpu placement='static'>%d</vcpu>
  <os>
    <type arch='x86_64' machine='pc-i440fx-2.12'>hvm</type>
    <boot dev='hd'/>
  </os>
  <features>
    <acpi/>
    <apic/>
  </features>
  <cpu mode='host-model' check='partial'/>
  <clock offset='utc'>
    <timer name='rtc' tickpolicy='catchup'/>
    <timer name='pit' tickpolicy='delay'/>
    <timer name='hpet' present='no'/>
  </clock>
  <on_poweroff>destroy</on_poweroff>
  <on_reboot>restart</on_reboot>
  <on_crash>destroy</on_crash>
  <pm>
    <suspend-to-mem enabled='no'/>
    <suspend-to-disk enabled='no'/>
  </pm>
  <devices>
    <emulator>/usr/bin/qemu-system-x86_64</emulator>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='%s'/>
      <target dev='vda' bus='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x07' function='0x0'/>
    </disk>`,
		req.Name, memoryKB, memoryKB, req.Class.CPU, volumePath)

	// Add cloud-init ISO if present
	if cloudInitPath != "" {
		xml += fmt.Sprintf(`
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='%s'/>
      <target dev='hda' bus='ide'/>
      <readonly/>
      <address type='drive' controller='0' bus='0' target='0' unit='0'/>
    </disk>`, cloudInitPath)
	}

	// Add network interfaces
	for i, network := range req.Networks {
		networkName := network.NetworkName
		if networkName == "" {
			networkName = "default"
		}

		xml += fmt.Sprintf(`
    <interface type='network'>
      <mac address='52:54:00:6b:3c:%02x'/>
      <source network='%s'/>
      <model type='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x%02x' function='0x0'/>
    </interface>`, i+10, networkName, i+3)
	}

	// Add console and graphics
	xml += `
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
    <redirdev bus='usb' type='spicevmc'>
      <address type='usb' bus='0' port='2'/>
    </redirdev>
    <redirdev bus='usb' type='spicevmc'>
      <address type='usb' bus='0' port='3'/>
    </redirdev>
    <memballoon model='virtio'>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x08' function='0x0'/>
    </memballoon>
  </devices>
</domain>`

	return xml
}

// describeDomain returns comprehensive state information of a domain
func (p *Provider) describeDomain(domain *libvirt.Domain) (contracts.DescribeResponse, error) {
	// Get basic domain info
	name, err := domain.GetName()
	if err != nil {
		return contracts.DescribeResponse{}, contracts.NewRetryableError("failed to get domain name", err)
	}

	uuid, err := domain.GetUUIDString()
	if err != nil {
		return contracts.DescribeResponse{}, contracts.NewRetryableError("failed to get domain UUID", err)
	}

	// Get comprehensive domain information
	domainInfo, err := domain.GetInfo()
	if err != nil {
		return contracts.DescribeResponse{}, contracts.NewRetryableError("failed to get domain info", err)
	}

	// Map libvirt state to VirtRigaud power state
	powerState := p.mapLibvirtPowerState(domainInfo.State)

	// Get IP addresses using multiple methods
	ips := p.getDomainIPs(domain)

	// Get memory statistics
	memoryStats := p.getDomainMemoryStats(domain)

	// Get CPU statistics
	cpuStats := p.getDomainCPUStats(domain)

	// Get network statistics
	networkStats := p.getDomainNetworkStats(domain)

	// Get storage statistics
	storageStats := p.getDomainStorageStats(domain)

	// Get guest OS information
	guestInfo := p.getDomainGuestInfo(domain)

	// Get console information
	consoleURL := p.getDomainConsoleURL(domain)

	// Create comprehensive provider raw JSON matching vSphere format
	providerRawJson := p.createProviderRawJSON(name, uuid, powerState, domainInfo, memoryStats, cpuStats, networkStats, storageStats, guestInfo, ips)

	response := contracts.DescribeResponse{
		Exists:     true,
		PowerState: powerState,
		IPs:        ips,
		ConsoleURL: consoleURL,
		ProviderRaw: map[string]string{
			"json": providerRawJson,
		},
	}

	return response, nil
}

// getDomainIPs attempts to get IP addresses from the domain using multiple methods
func (p *Provider) getDomainIPs(domain *libvirt.Domain) []string {
	var ips []string

	// Method 1: Try to get interface addresses from guest agent (most reliable)
	ifaces, err := domain.ListAllInterfaceAddresses(libvirt.DOMAIN_INTERFACE_ADDRESSES_SRC_AGENT)
	if err == nil {
		for _, iface := range ifaces {
			for _, addr := range iface.Addrs {
				if p.isValidIPAddress(addr.Addr) {
					ips = append(ips, addr.Addr)
				}
			}
		}
		if len(ips) > 0 {
			return ips
		}
	}

	// Method 2: Fall back to DHCP lease lookup
	ifaces, err = domain.ListAllInterfaceAddresses(libvirt.DOMAIN_INTERFACE_ADDRESSES_SRC_LEASE)
	if err == nil {
		for _, iface := range ifaces {
			for _, addr := range iface.Addrs {
				if p.isValidIPAddress(addr.Addr) {
					ips = append(ips, addr.Addr)
				}
			}
		}
		if len(ips) > 0 {
			return ips
		}
	}

	// Method 3: Try ARP table lookup (for bridged networks)
	ifaces, err = domain.ListAllInterfaceAddresses(libvirt.DOMAIN_INTERFACE_ADDRESSES_SRC_ARP)
	if err == nil {
		for _, iface := range ifaces {
			for _, addr := range iface.Addrs {
				if p.isValidIPAddress(addr.Addr) {
					ips = append(ips, addr.Addr)
				}
			}
		}
	}

	return ips
}

// isValidIPAddress filters out unwanted IP addresses
func (p *Provider) isValidIPAddress(ip string) bool {
	// Skip loopback addresses
	if strings.HasPrefix(ip, "127.") {
		return false
	}
	// Skip link-local addresses
	if strings.HasPrefix(ip, "169.254.") {
		return false
	}
	// Skip IPv6 link-local addresses
	if strings.HasPrefix(ip, "fe80:") {
		return false
	}
	// Skip localhost IPv6
	if ip == "::1" {
		return false
	}
	return true
}

// mapLibvirtPowerState converts libvirt domain state to VirtRigaud power state
func (p *Provider) mapLibvirtPowerState(state libvirt.DomainState) string {
	switch state {
	case libvirt.DOMAIN_RUNNING:
		return "On"
	case libvirt.DOMAIN_BLOCKED:
		return "On" // Domain is running but blocked on resource
	case libvirt.DOMAIN_PAUSED:
		return "On" // Domain is paused
	case libvirt.DOMAIN_SHUTDOWN:
		return "Off" // Domain is being shut down
	case libvirt.DOMAIN_SHUTOFF:
		return "Off" // Domain is shut off
	case libvirt.DOMAIN_CRASHED:
		return "Off" // Domain has crashed
	case libvirt.DOMAIN_PMSUSPENDED:
		return "Off" // Domain is suspended by guest power management
	default:
		return "Off" // Unknown state, assume off
	}
}

// getDomainMemoryStats collects memory statistics
func (p *Provider) getDomainMemoryStats(domain *libvirt.Domain) map[string]interface{} {
	stats := make(map[string]interface{})

	// Get memory statistics from domain
	memStats, err := domain.MemoryStats(8, 0) // Request up to 8 stats
	if err == nil {
		for _, stat := range memStats {
			// Use int comparison for memory stat tags
			switch int(stat.Tag) {
			case 6: // DOMAIN_MEMORY_STAT_ACTUAL_BALLOON
				stats["actual_balloon_kb"] = stat.Val
			case 7: // DOMAIN_MEMORY_STAT_RSS
				stats["rss_kb"] = stat.Val
			case 8: // DOMAIN_MEMORY_STAT_USABLE
				stats["usable_kb"] = stat.Val
			case 9: // DOMAIN_MEMORY_STAT_AVAILABLE
				stats["available_kb"] = stat.Val
			case 10: // DOMAIN_MEMORY_STAT_UNUSED
				stats["unused_kb"] = stat.Val
			}
		}
	}

	return stats
}

// getDomainCPUStats collects CPU statistics
func (p *Provider) getDomainCPUStats(domain *libvirt.Domain) map[string]interface{} {
	stats := make(map[string]interface{})

	// Get basic domain info for CPU count instead of detailed stats
	// The GetCPUStats API has complex structure that varies by hypervisor
	domainInfo, err := domain.GetInfo()
	if err == nil {
		stats["vcpu_count"] = domainInfo.NrVirtCpu
		stats["max_memory_kb"] = domainInfo.MaxMem
		stats["current_memory_kb"] = domainInfo.Memory
		stats["cpu_time_ns"] = domainInfo.CpuTime
		stats["state"] = int(domainInfo.State)
	}

	return stats
}

// getDomainNetworkStats collects network interface statistics
func (p *Provider) getDomainNetworkStats(domain *libvirt.Domain) map[string]interface{} {
	stats := make(map[string]interface{})

	// Try common interface names for libvirt/KVM
	interfaceNames := []string{"vnet0", "tap0", "virbr0-nic", "eth0"}

	for _, ifName := range interfaceNames {
		interfaceStats, err := domain.InterfaceStats(ifName)
		if err == nil {
			stats["interface_name"] = ifName
			stats["rx_bytes"] = interfaceStats.RxBytes
			stats["rx_packets"] = interfaceStats.RxPackets
			stats["rx_errors"] = interfaceStats.RxErrs
			stats["rx_dropped"] = interfaceStats.RxDrop
			stats["tx_bytes"] = interfaceStats.TxBytes
			stats["tx_packets"] = interfaceStats.TxPackets
			stats["tx_errors"] = interfaceStats.TxErrs
			stats["tx_dropped"] = interfaceStats.TxDrop
			break // Found working interface
		}
	}

	return stats
}

// getDomainStorageStats collects storage statistics
func (p *Provider) getDomainStorageStats(domain *libvirt.Domain) map[string]interface{} {
	stats := make(map[string]interface{})

	// Get block device statistics for primary disk (simplified)
	blockStats, err := domain.BlockStats("vda") // Common default device name
	if err == nil {
		stats["read_requests"] = blockStats.RdReq
		stats["read_bytes"] = blockStats.RdBytes
		stats["write_requests"] = blockStats.WrReq
		stats["write_bytes"] = blockStats.WrBytes
		stats["errors"] = blockStats.Errs
	}

	return stats
}

// getDomainGuestInfo collects guest OS information
func (p *Provider) getDomainGuestInfo(domain *libvirt.Domain) map[string]interface{} {
	info := make(map[string]interface{})

	// Get OS type
	osType, err := domain.GetOSType()
	if err == nil {
		info["os_type"] = osType
	}

	// Try to get guest information via guest agent
	// This requires qemu-guest-agent to be installed and running in the guest
	if guestInfo, err := domain.QemuAgentCommand(
		`{"execute":"guest-get-osinfo"}`,
		libvirt.DOMAIN_QEMU_AGENT_COMMAND_DEFAULT, 0); err == nil {
		info["guest_agent_info"] = guestInfo
	}

	// Get hostname via guest agent
	if hostname, err := domain.GetHostname(libvirt.DOMAIN_GET_HOSTNAME_AGENT); err == nil {
		info["hostname"] = hostname
	}

	return info
}

// getDomainConsoleURL generates console access URL
func (p *Provider) getDomainConsoleURL(domain *libvirt.Domain) string {
	// For now, return a simplified VNC URL
	// In production, you'd parse the domain XML to get the actual VNC port
	return "vnc://localhost:5900"
}

// createProviderRawJSON creates comprehensive JSON output matching vSphere format
func (p *Provider) createProviderRawJSON(
	name, uuid, powerState string,
	domainInfo *libvirt.DomainInfo,
	memoryStats, cpuStats, networkStats, storageStats, guestInfo map[string]interface{},
	ips []string,
) string {
	// Get primary IP
	primaryIP := ""
	if len(ips) > 0 {
		primaryIP = ips[0]
	}

	// Get hostname from guest info
	hostname := ""
	if h, ok := guestInfo["hostname"].(string); ok {
		hostname = h
	}

	// Get OS type
	osType := ""
	if os, ok := guestInfo["os_type"].(string); ok {
		osType = os
	}

	// Calculate memory in MB
	maxMemoryMB := domainInfo.MaxMem / 1024
	currentMemoryMB := domainInfo.Memory / 1024

	// Memory usage calculation
	memoryUsageMB := int64(0)
	if rss, ok := memoryStats["rss_kb"].(uint64); ok {
		memoryUsageMB = int64(rss / 1024)
	}

	// Create comprehensive JSON matching vSphere provider format
	providerRawJson := fmt.Sprintf(`{
		"domain_id": "%s",
		"name": "%s",
		"power_state": "%s",
		"primary_ip": "%s",
		"hostname": "%s",
		"os_type": "%s",
		"vcpu_count": %d,
		"max_memory_mb": %d,
		"current_memory_mb": %d,
		"memory_usage_mb": %d,
		"state": %d,
		"memory_stats": %v,
		"cpu_stats": %v,
		"network_stats": %v,
		"storage_stats": %v,
		"guest_info": %v,
		"all_ips": %v
	}`,
		uuid,
		name,
		powerState,
		primaryIP,
		hostname,
		osType,
		domainInfo.NrVirtCpu,
		maxMemoryMB,
		currentMemoryMB,
		memoryUsageMB,
		int(domainInfo.State),
		p.mapToJSON(memoryStats),
		p.mapToJSON(cpuStats),
		p.mapToJSON(networkStats),
		p.mapToJSON(storageStats),
		p.mapToJSON(guestInfo),
		p.stringSliceToJSON(ips),
	)

	return providerRawJson
}

// mapToJSON converts a map to JSON string representation
func (p *Provider) mapToJSON(data map[string]interface{}) string {
	if len(data) == 0 {
		return "{}"
	}
	// Simple JSON serialization - in production you'd use json.Marshal
	result := "{"
	count := 0
	for k, v := range data {
		if count > 0 {
			result += ","
		}
		result += fmt.Sprintf(`"%s":%v`, k, v)
		count++
	}
	result += "}"
	return result
}

// stringSliceToJSON converts a string slice to JSON array
func (p *Provider) stringSliceToJSON(slice []string) string {
	if len(slice) == 0 {
		return "[]"
	}
	result := "["
	for i, s := range slice {
		if i > 0 {
			result += ","
		}
		result += fmt.Sprintf(`"%s"`, s)
	}
	result += "]"
	return result
}
