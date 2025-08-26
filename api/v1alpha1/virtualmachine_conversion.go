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

package v1alpha1

import (
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/conversion"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// ConvertTo converts this VirtualMachine to the Hub version (v1beta1)
func (src *VirtualMachine) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.VirtualMachine)

	// Convert metadata
	dst.ObjectMeta = src.ObjectMeta

	// Convert Spec
	if err := convertVirtualMachineSpec(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert VirtualMachine spec: %w", err)
	}

	// Convert Status
	if err := convertVirtualMachineStatus(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert VirtualMachine status: %w", err)
	}

	return nil
}

// ConvertFrom converts from the Hub version (v1beta1) to this version
func (dst *VirtualMachine) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.VirtualMachine)

	// Convert metadata
	dst.ObjectMeta = src.ObjectMeta

	// Convert Spec
	if err := convertVirtualMachineSpecFromBeta(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert VirtualMachine spec from beta: %w", err)
	}

	// Convert Status
	if err := convertVirtualMachineStatusFromBeta(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert VirtualMachine status from beta: %w", err)
	}

	return nil
}

// convertVirtualMachineSpec converts v1alpha1 VirtualMachineSpec to v1beta1
func convertVirtualMachineSpec(src *VirtualMachineSpec, dst *v1beta1.VirtualMachineSpec) error {
	// Direct field mappings
	dst.ProviderRef = convertObjectRef(src.ProviderRef)
	dst.ClassRef = convertObjectRef(src.ClassRef)
	dst.ImageRef = convertObjectRef(src.ImageRef)

	// Convert Networks - alpha uses VMNetworkRef, beta uses VMNetworkRef (enhanced)
	if len(src.Networks) > 0 {
		dst.Networks = make([]v1beta1.VMNetworkRef, len(src.Networks))
		for i, alphaNet := range src.Networks {
			dst.Networks[i] = v1beta1.VMNetworkRef{
				Name: alphaNet.Name,
				NetworkRef: v1beta1.ObjectRef{
					Name:      alphaNet.Name, // In alpha, Name was the network name
					Namespace: "",            // Default to current namespace
				},
				IPAddress:  alphaNet.StaticIP, // alpha StaticIP -> beta IPAddress
				MACAddress: "",                // Not present in alpha
			}
		}
	}

	// Convert Disks - enhance with new validation fields
	if len(src.Disks) > 0 {
		dst.Disks = make([]v1beta1.DiskSpec, len(src.Disks))
		for i, alphaDisk := range src.Disks {
			dst.Disks[i] = v1beta1.DiskSpec{
				Name:         alphaDisk.Name,
				SizeGiB:      alphaDisk.SizeGiB,
				Type:         alphaDisk.Type,
				ExpandPolicy: "Offline", // Default since not present in alpha
				StorageClass: "",        // Not present in alpha
			}
		}
	}

	// Convert UserData
	if src.UserData != nil {
		dst.UserData = &v1beta1.UserData{}
		if src.UserData.CloudInit != nil {
			dst.UserData.CloudInit = &v1beta1.CloudInit{
				Inline: src.UserData.CloudInit.Inline,
			}
			if src.UserData.CloudInit.SecretRef != nil {
				dst.UserData.CloudInit.SecretRef = &v1beta1.LocalObjectReference{
					Name: src.UserData.CloudInit.SecretRef.Name,
				}
			}
		}
		// Ignition is new in beta, so set to nil
		dst.UserData.Ignition = nil
	}

	// Convert Placement
	if src.Placement != nil {
		dst.Placement = &v1beta1.Placement{
			Cluster:      src.Placement.Cluster,
			Host:         "", // New field in beta
			Datastore:    src.Placement.Datastore,
			Folder:       src.Placement.Folder,
			ResourcePool: "", // New field in beta
		}
	}

	// Convert PowerState - alpha uses string, beta uses typed enum
	// Only set if source has a value, do not apply defaults in conversion
	switch src.PowerState {
	case "On":
		dst.PowerState = v1beta1.PowerStateOn
	case "Off":
		dst.PowerState = v1beta1.PowerStateOff
		// default: leave dst.PowerState empty (no defaulting in conversion)
	}

	// Convert Tags
	dst.Tags = src.Tags

	// Convert Resources
	if src.Resources != nil {
		dst.Resources = &v1beta1.VirtualMachineResources{
			CPU:       src.Resources.CPU,
			MemoryMiB: src.Resources.MemoryMiB,
		}
		if src.Resources.GPU != nil {
			dst.Resources.GPU = &v1beta1.GPUConfig{
				Count:  src.Resources.GPU.Count,
				Type:   src.Resources.GPU.Type,
				Memory: src.Resources.GPU.Memory,
			}
		}
	}

	// Convert PlacementRef
	if src.PlacementRef != nil {
		dst.PlacementRef = &v1beta1.LocalObjectReference{
			Name: src.PlacementRef.Name,
		}
	}

	// Convert Snapshot
	if src.Snapshot != nil {
		dst.Snapshot = &v1beta1.VMSnapshotOperation{}
		if src.Snapshot.RevertToRef != nil {
			dst.Snapshot.RevertToRef = &v1beta1.LocalObjectReference{
				Name: src.Snapshot.RevertToRef.Name,
			}
		}
	}

	// Lifecycle is new in beta, set to nil
	dst.Lifecycle = nil

	return nil
}

// convertVirtualMachineStatus converts v1alpha1 VirtualMachineStatus to v1beta1
func convertVirtualMachineStatus(src *VirtualMachineStatus, dst *v1beta1.VirtualMachineStatus) error {
	// Direct field mappings
	dst.ID = src.ID
	dst.IPs = src.IPs
	dst.ConsoleURL = src.ConsoleURL
	dst.Conditions = src.Conditions
	dst.ObservedGeneration = src.ObservedGeneration
	dst.LastTaskRef = src.LastTaskRef
	dst.Provider = src.Provider
	dst.ReconfigureTaskRef = src.ReconfigureTaskRef
	dst.LastReconfigureTime = src.LastReconfigureTime
	dst.Snapshots = make([]v1beta1.VMSnapshotInfo, len(src.Snapshots))

	// Convert PowerState - alpha uses string, beta uses typed enum
	// Only set if source has a value, do not apply defaults in conversion
	switch src.PowerState {
	case "On":
		dst.PowerState = v1beta1.PowerStateOn
	case "Off":
		dst.PowerState = v1beta1.PowerStateOff
		// default: leave dst.PowerState empty (no defaulting in conversion)
	}

	// Convert CurrentResources
	if src.CurrentResources != nil {
		dst.CurrentResources = &v1beta1.VirtualMachineResources{
			CPU:       src.CurrentResources.CPU,
			MemoryMiB: src.CurrentResources.MemoryMiB,
		}
		if src.CurrentResources.GPU != nil {
			dst.CurrentResources.GPU = &v1beta1.GPUConfig{
				Count:  src.CurrentResources.GPU.Count,
				Type:   src.CurrentResources.GPU.Type,
				Memory: src.CurrentResources.GPU.Memory,
			}
		}
	}

	// Convert Snapshots
	for i, alphaSnapshot := range src.Snapshots {
		dst.Snapshots[i] = v1beta1.VMSnapshotInfo{
			ID:           alphaSnapshot.ID,
			Name:         alphaSnapshot.Name,
			CreationTime: alphaSnapshot.CreationTime,
			Description:  alphaSnapshot.Description,
			SizeBytes:    alphaSnapshot.SizeBytes,
			HasMemory:    alphaSnapshot.HasMemory,
		}
	}

	// New fields in beta - do not apply defaults in conversion
	// dst.Phase left empty (no defaulting in conversion)
	dst.Message = "" // No equivalent in alpha

	return nil
}

// convertVirtualMachineSpecFromBeta converts v1beta1 VirtualMachineSpec to v1alpha1
func convertVirtualMachineSpecFromBeta(src *v1beta1.VirtualMachineSpec, dst *VirtualMachineSpec) error {
	// Direct field mappings
	dst.ProviderRef = convertObjectRefFromBeta(src.ProviderRef)
	dst.ClassRef = convertObjectRefFromBeta(src.ClassRef)
	dst.ImageRef = convertObjectRefFromBeta(src.ImageRef)

	// Convert Networks - beta has enhanced VMNetworkRef, alpha has simpler version
	if len(src.Networks) > 0 {
		dst.Networks = make([]VMNetworkRef, len(src.Networks))
		for i, betaNet := range src.Networks {
			dst.Networks[i] = VMNetworkRef{
				Name:     betaNet.Name,
				IPPolicy: "dhcp", // Default policy
				StaticIP: betaNet.IPAddress,
			}
		}
	}

	// Convert Disks - remove new beta fields
	if len(src.Disks) > 0 {
		dst.Disks = make([]DiskSpec, len(src.Disks))
		for i, betaDisk := range src.Disks {
			dst.Disks[i] = DiskSpec{
				Name:    betaDisk.Name,
				SizeGiB: betaDisk.SizeGiB,
				Type:    betaDisk.Type,
			}
		}
	}

	// Convert UserData
	if src.UserData != nil {
		dst.UserData = &UserData{}
		if src.UserData.CloudInit != nil {
			dst.UserData.CloudInit = &CloudInitConfig{
				Inline: src.UserData.CloudInit.Inline,
			}
			if src.UserData.CloudInit.SecretRef != nil {
				dst.UserData.CloudInit.SecretRef = &ObjectRef{
					Name: src.UserData.CloudInit.SecretRef.Name,
				}
			}
		}
		// Ignition is not supported in alpha - ignore
	}

	// Convert Placement
	if src.Placement != nil {
		dst.Placement = &Placement{
			Cluster:   src.Placement.Cluster,
			Datastore: src.Placement.Datastore,
			Folder:    src.Placement.Folder,
		}
	}

	// Convert PowerState - beta uses typed enum, alpha uses string
	// Only set if source has a value, do not apply defaults in conversion
	switch src.PowerState {
	case v1beta1.PowerStateOn:
		dst.PowerState = "On"
	case v1beta1.PowerStateOff:
		dst.PowerState = "Off"
		// default: leave dst.PowerState empty (no defaulting in conversion)
	}

	// Convert Tags
	dst.Tags = src.Tags

	// Convert Resources
	if src.Resources != nil {
		dst.Resources = &VirtualMachineResources{
			CPU:       src.Resources.CPU,
			MemoryMiB: src.Resources.MemoryMiB,
		}
		if src.Resources.GPU != nil {
			dst.Resources.GPU = &GPUConfig{
				Count:  src.Resources.GPU.Count,
				Type:   src.Resources.GPU.Type,
				Memory: src.Resources.GPU.Memory,
			}
		}
	}

	// Convert PlacementRef
	if src.PlacementRef != nil {
		dst.PlacementRef = &LocalObjectReference{
			Name: src.PlacementRef.Name,
		}
	}

	// Convert Snapshot
	if src.Snapshot != nil {
		dst.Snapshot = &VMSnapshotOperation{}
		if src.Snapshot.RevertToRef != nil {
			dst.Snapshot.RevertToRef = &LocalObjectReference{
				Name: src.Snapshot.RevertToRef.Name,
			}
		}
	}

	// Lifecycle is not supported in alpha - ignore

	return nil
}

// convertVirtualMachineStatusFromBeta converts v1beta1 VirtualMachineStatus to v1alpha1
func convertVirtualMachineStatusFromBeta(src *v1beta1.VirtualMachineStatus, dst *VirtualMachineStatus) error {
	// Direct field mappings
	dst.ID = src.ID
	dst.IPs = src.IPs
	dst.ConsoleURL = src.ConsoleURL
	dst.Conditions = src.Conditions
	dst.ObservedGeneration = src.ObservedGeneration
	dst.LastTaskRef = src.LastTaskRef
	dst.Provider = src.Provider
	dst.ReconfigureTaskRef = src.ReconfigureTaskRef
	dst.LastReconfigureTime = src.LastReconfigureTime
	dst.Snapshots = make([]VMSnapshotInfo, len(src.Snapshots))

	// Convert PowerState - beta uses typed enum, alpha uses string
	// Only set if source has a value, do not apply defaults in conversion
	switch src.PowerState {
	case v1beta1.PowerStateOn:
		dst.PowerState = "On"
	case v1beta1.PowerStateOff:
		dst.PowerState = "Off"
		// default: leave dst.PowerState empty (no defaulting in conversion)
	}

	// Convert CurrentResources
	if src.CurrentResources != nil {
		dst.CurrentResources = &VirtualMachineResources{
			CPU:       src.CurrentResources.CPU,
			MemoryMiB: src.CurrentResources.MemoryMiB,
		}
		if src.CurrentResources.GPU != nil {
			dst.CurrentResources.GPU = &GPUConfig{
				Count:  src.CurrentResources.GPU.Count,
				Type:   src.CurrentResources.GPU.Type,
				Memory: src.CurrentResources.GPU.Memory,
			}
		}
	}

	// Convert Snapshots
	for i, betaSnapshot := range src.Snapshots {
		dst.Snapshots[i] = VMSnapshotInfo{
			ID:           betaSnapshot.ID,
			Name:         betaSnapshot.Name,
			CreationTime: betaSnapshot.CreationTime,
			Description:  betaSnapshot.Description,
			SizeBytes:    betaSnapshot.SizeBytes,
			HasMemory:    betaSnapshot.HasMemory,
		}
	}

	// Beta-specific fields (Phase, Message) are not present in alpha - ignore

	return nil
}

// Helper functions for ObjectRef conversion
func convertObjectRef(src ObjectRef) v1beta1.ObjectRef {
	return v1beta1.ObjectRef{
		Name:      src.Name,
		Namespace: src.Namespace,
	}
}

func convertObjectRefFromBeta(src v1beta1.ObjectRef) ObjectRef {
	return ObjectRef{
		Name:      src.Name,
		Namespace: src.Namespace,
	}
}
