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
	defer domain.Free()

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

// describeDomain returns the current state of a domain
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

	// Get power state
	active, err := domain.IsActive()
	if err != nil {
		return contracts.DescribeResponse{}, contracts.NewRetryableError("failed to get domain state", err)
	}

	var powerState string
	if active {
		powerState = "On"
	} else {
		powerState = "Off"
	}

	// Get IP addresses (requires guest agent)
	ips := p.getDomainIPs(domain)

	response := contracts.DescribeResponse{
		Exists:     true,
		PowerState: powerState,
		IPs:        ips,
		ProviderRaw: map[string]string{
			"uuid":   uuid,
			"name":   name,
			"active": fmt.Sprintf("%t", active),
		},
	}

	// Generate console URL (VNC)
	response.ConsoleURL = fmt.Sprintf("vnc://localhost:5900") // Simplified

	return response, nil
}

// getDomainIPs attempts to get IP addresses from the domain
func (p *Provider) getDomainIPs(domain *libvirt.Domain) []string {
	var ips []string

	// Try to get interface addresses from guest agent
	ifaces, err := domain.ListAllInterfaceAddresses(libvirt.DOMAIN_INTERFACE_ADDRESSES_SRC_AGENT)
	if err != nil {
		// Fall back to DHCP lease lookup
		ifaces, err = domain.ListAllInterfaceAddresses(libvirt.DOMAIN_INTERFACE_ADDRESSES_SRC_LEASE)
		if err != nil {
			return ips // Return empty if we can't get IPs
		}
	}

	for _, iface := range ifaces {
		for _, addr := range iface.Addrs {
			// Skip loopback and link-local addresses
			if !strings.HasPrefix(addr.Addr, "127.") && !strings.HasPrefix(addr.Addr, "169.254.") {
				ips = append(ips, addr.Addr)
			}
		}
	}

	return ips
}
