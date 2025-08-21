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

package vsphere

import (
	"context"
	"fmt"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// findVM finds a VM by name
func (p *Provider) findVM(ctx context.Context, name string) (*object.VirtualMachine, error) {
	if err := p.ensureConnection(ctx); err != nil {
		return nil, err
	}

	vms, err := p.finder.VirtualMachineList(ctx, name)
	if err != nil {
		return nil, err
	}

	if len(vms) == 0 {
		return nil, fmt.Errorf("VM not found: %s", name)
	}

	return vms[0], nil
}

// findVMByID finds a VM by its managed object reference ID
func (p *Provider) findVMByID(ctx context.Context, id string) (*object.VirtualMachine, error) {
	if err := p.ensureConnection(ctx); err != nil {
		return nil, err
	}

	ref := types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: id,
	}

	return object.NewVirtualMachine(p.client.Client, ref), nil
}

// createVM creates a new virtual machine
func (p *Provider) createVM(ctx context.Context, req contracts.CreateRequest) (taskRef, vmID string, err error) {
	// Find the template
	template, err := p.findTemplate(ctx, req.Image)
	if err != nil {
		return "", "", contracts.NewNotFoundError("template not found", err)
	}

	// Find the resource pool
	resourcePool, err := p.findResourcePool(ctx, req.Placement)
	if err != nil {
		return "", "", contracts.NewNotFoundError("resource pool not found", err)
	}

	// Find the datastore
	datastore, err := p.findDatastore(ctx, req.Placement)
	if err != nil {
		return "", "", contracts.NewNotFoundError("datastore not found", err)
	}

	// Find the folder
	folder, err := p.findFolder(ctx, req.Placement)
	if err != nil {
		return "", "", contracts.NewNotFoundError("folder not found", err)
	}

	// Build clone specification
	spec := p.buildCloneSpec(req, resourcePool, datastore)

	// Clone the VM
	task, err := template.Clone(ctx, folder, req.Name, spec)
	if err != nil {
		return "", "", contracts.NewRetryableError("failed to clone VM", err)
	}

	// Get the task reference
	taskRef = task.Reference().Value

	// For now, we'll use the task reference as a placeholder VM ID
	// The actual VM ID will be retrieved when the task completes
	vmID = fmt.Sprintf("task-%s", taskRef)

	return taskRef, vmID, nil
}

// buildCloneSpec builds the VM clone specification
func (p *Provider) buildCloneSpec(req contracts.CreateRequest, resourcePool *object.ResourcePool, datastore *object.Datastore) types.VirtualMachineCloneSpec {
	poolRef := resourcePool.Reference()
	datastoreRef := datastore.Reference()

	spec := types.VirtualMachineCloneSpec{
		Location: types.VirtualMachineRelocateSpec{
			Pool:      &poolRef,
			Datastore: &datastoreRef,
		},
		PowerOn: false, // We'll power on separately
	}

	// Build customization spec for guest OS configuration
	if req.UserData != nil && req.UserData.CloudInitData != "" {
		spec.Customization = p.buildCustomizationSpec(req)
	}

	// Configure hardware changes
	configSpec := &types.VirtualMachineConfigSpec{}

	// Set CPU and memory
	cpuCount := int32(req.Class.CPU)
	memoryMB := int64(req.Class.MemoryMiB)

	configSpec.NumCPUs = cpuCount
	configSpec.MemoryMB = memoryMB

	// Configure hardware version and firmware
	if req.Class.Firmware == "UEFI" {
		configSpec.Firmware = string(types.GuestOsDescriptorFirmwareTypeEfi)
	}

	// Add extra configuration
	if len(req.Class.ExtraConfig) > 0 {
		var extraConfig []types.BaseOptionValue
		for key, value := range req.Class.ExtraConfig {
			extraConfig = append(extraConfig, &types.OptionValue{
				Key:   key,
				Value: value,
			})
		}
		configSpec.ExtraConfig = extraConfig
	}

	// Add cloud-init data if present
	if req.UserData != nil && req.UserData.CloudInitData != "" {
		p.addCloudInitConfig(configSpec, req.UserData.CloudInitData)
	}

	spec.Config = configSpec

	return spec
}

// addCloudInitConfig adds cloud-init configuration to the VM config spec
func (p *Provider) addCloudInitConfig(spec *types.VirtualMachineConfigSpec, cloudInitData string) {
	// Add cloud-init data via guestinfo variables
	if spec.ExtraConfig == nil {
		spec.ExtraConfig = []types.BaseOptionValue{}
	}

	// Add cloud-init data
	spec.ExtraConfig = append(spec.ExtraConfig,
		&types.OptionValue{
			Key:   "guestinfo.userdata",
			Value: cloudInitData,
		},
		&types.OptionValue{
			Key:   "guestinfo.userdata.encoding",
			Value: "base64",
		},
	)
}

// buildCustomizationSpec builds guest OS customization specification
func (p *Provider) buildCustomizationSpec(req contracts.CreateRequest) *types.CustomizationSpec {
	// For now, we'll use a basic Linux customization
	// This can be extended based on the image type
	return &types.CustomizationSpec{
		Identity: &types.CustomizationLinuxPrep{
			HostName: &types.CustomizationFixedName{
				Name: req.Name,
			},
		},
		GlobalIPSettings: types.CustomizationGlobalIPSettings{},
		NicSettingMap:    p.buildNetworkCustomization(req.Networks),
	}
}

// buildNetworkCustomization builds network customization settings
func (p *Provider) buildNetworkCustomization(networks []contracts.NetworkAttachment) []types.CustomizationAdapterMapping {
	var mappings []types.CustomizationAdapterMapping

	for _, network := range networks {
		mapping := types.CustomizationAdapterMapping{}

		if network.IPPolicy == "static" && network.StaticIP != "" {
			// Static IP configuration
			mapping.Adapter = types.CustomizationIPSettings{
				Ip: &types.CustomizationFixedIp{
					IpAddress: network.StaticIP,
				},
			}
		} else {
			// DHCP configuration
			mapping.Adapter = types.CustomizationIPSettings{
				Ip: &types.CustomizationDhcpIpGenerator{},
			}
		}

		mappings = append(mappings, mapping)
	}

	return mappings
}

// describeVM returns the current state of a VM
func (p *Provider) describeVM(ctx context.Context, vm *object.VirtualMachine) (contracts.DescribeResponse, error) {
	var mvm mo.VirtualMachine
	err := vm.Properties(ctx, vm.Reference(), []string{
		"runtime.powerState",
		"guest.ipAddress",
		"guest.net",
		"config.name",
		"summary",
	}, &mvm)
	if err != nil {
		return contracts.DescribeResponse{}, contracts.NewRetryableError("failed to get VM properties", err)
	}

	response := contracts.DescribeResponse{
		Exists:     true,
		PowerState: string(mvm.Runtime.PowerState),
		ProviderRaw: map[string]string{
			"moref":    vm.Reference().Value,
			"name":     mvm.Config.Name,
			"uuid":     mvm.Summary.Config.Uuid,
			"instance": mvm.Summary.Config.InstanceUuid,
		},
	}

	// Get IP addresses
	if mvm.Guest != nil {
		response.IPs = p.extractIPAddresses(*mvm.Guest)
	}

	// Get console URL (if available)
	response.ConsoleURL = p.getConsoleURL(vm)

	return response, nil
}

// extractIPAddresses extracts IP addresses from VM guest info
func (p *Provider) extractIPAddresses(guest types.GuestInfo) []string {
	var ips []string

	// Primary IP address
	if guest.IpAddress != "" {
		ips = append(ips, guest.IpAddress)
	}

	// Additional IP addresses from network interfaces
	for _, net := range guest.Net {
		for _, ip := range net.IpAddress {
			// Skip loopback and link-local addresses
			if !strings.HasPrefix(ip, "127.") && !strings.HasPrefix(ip, "169.254.") {
				// Avoid duplicates
				found := false
				for _, existing := range ips {
					if existing == ip {
						found = true
						break
					}
				}
				if !found {
					ips = append(ips, ip)
				}
			}
		}
	}

	return ips
}

// getConsoleURL generates a console URL for the VM
func (p *Provider) getConsoleURL(vm *object.VirtualMachine) string {
	// For MVP, return a placeholder
	// In a real implementation, this would generate a proper console URL
	return fmt.Sprintf("console://vm/%s", vm.Reference().Value)
}

// buildReconfigSpec builds a reconfiguration spec for the VM
func (p *Provider) buildReconfigSpec(desired contracts.CreateRequest) *types.VirtualMachineConfigSpec {
	spec := &types.VirtualMachineConfigSpec{}
	hasChanges := false

	// CPU changes
	if desired.Class.CPU > 0 {
		spec.NumCPUs = int32(desired.Class.CPU)
		hasChanges = true
	}

	// Memory changes
	if desired.Class.MemoryMiB > 0 {
		spec.MemoryMB = int64(desired.Class.MemoryMiB)
		hasChanges = true
	}

	if !hasChanges {
		return nil
	}

	return spec
}
