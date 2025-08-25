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
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// ConvertTo converts this VMClass to the Hub version (v1beta1)
func (src *VMClass) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.VMClass)

	// Convert metadata
	dst.ObjectMeta = src.ObjectMeta

	// Convert Spec
	if err := convertVMClassSpec(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert VMClass spec: %w", err)
	}

	// Convert Status
	if err := convertVMClassStatus(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert VMClass status: %w", err)
	}

	return nil
}

// ConvertFrom converts from the Hub version (v1beta1) to this version
func (dst *VMClass) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.VMClass)

	// Convert metadata
	dst.ObjectMeta = src.ObjectMeta

	// Convert Spec
	if err := convertVMClassSpecFromBeta(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert VMClass spec from beta: %w", err)
	}

	// Convert Status
	if err := convertVMClassStatusFromBeta(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert VMClass status from beta: %w", err)
	}

	return nil
}

// convertVMClassSpec converts v1alpha1 VMClassSpec to v1beta1
func convertVMClassSpec(src *VMClassSpec, dst *v1beta1.VMClassSpec) error {
	// Direct field mappings
	dst.CPU = src.CPU

	// Convert Memory - alpha uses int32 (MiB), beta uses resource.Quantity
	memoryMiB := resource.NewQuantity(int64(src.MemoryMiB)*1024*1024, resource.BinarySI)
	dst.Memory = *memoryMiB

	// Convert Firmware - alpha uses string, beta uses typed enum
	switch src.Firmware {
	case "BIOS":
		dst.Firmware = v1beta1.FirmwareTypeBIOS
	case "UEFI":
		dst.Firmware = v1beta1.FirmwareTypeUEFI
	case "EFI":
		dst.Firmware = v1beta1.FirmwareTypeEFI
	default:
		dst.Firmware = v1beta1.FirmwareTypeBIOS // Default
	}

	// Convert DiskDefaults
	if src.DiskDefaults != nil {
		dst.DiskDefaults = &v1beta1.DiskDefaults{
			Size: resource.MustParse(strconv.Itoa(int(src.DiskDefaults.SizeGiB)) + "Gi"),
		}

		// Convert disk type
		switch src.DiskDefaults.Type {
		case "thin":
			dst.DiskDefaults.Type = v1beta1.DiskTypeThin
		case "thick":
			dst.DiskDefaults.Type = v1beta1.DiskTypeThick
		case "eagerzeroedthick":
			dst.DiskDefaults.Type = v1beta1.DiskTypeEagerZeroedThick
		default:
			dst.DiskDefaults.Type = v1beta1.DiskTypeThin
		}

		// New fields in beta - set defaults
		dst.DiskDefaults.IOPS = nil        // Not present in alpha
		dst.DiskDefaults.StorageClass = "" // Not present in alpha
	}

	// Convert GuestToolsPolicy - alpha uses string, beta uses typed enum
	switch src.GuestToolsPolicy {
	case "install":
		dst.GuestToolsPolicy = v1beta1.GuestToolsPolicyInstall
	case "skip":
		dst.GuestToolsPolicy = v1beta1.GuestToolsPolicySkip
	case "upgrade":
		dst.GuestToolsPolicy = v1beta1.GuestToolsPolicyUpgrade
	default:
		dst.GuestToolsPolicy = v1beta1.GuestToolsPolicyInstall
	}

	// Convert ExtraConfig
	dst.ExtraConfig = src.ExtraConfig

	// New fields in beta - set to nil (will use defaults)
	dst.ResourceLimits = nil
	dst.PerformanceProfile = nil
	dst.SecurityProfile = nil

	return nil
}

// convertVMClassStatus converts v1alpha1 VMClassStatus to v1beta1
func convertVMClassStatus(src *VMClassStatus, dst *v1beta1.VMClassStatus) error {
	// Direct field mappings
	dst.Conditions = src.Conditions
	dst.ObservedGeneration = src.ObservedGeneration

	// New fields in beta - set defaults
	dst.UsedByVMs = 0            // Not tracked in alpha
	dst.SupportedProviders = nil // Not tracked in alpha
	dst.ValidationResults = nil  // Not present in alpha

	return nil
}

// convertVMClassSpecFromBeta converts v1beta1 VMClassSpec to v1alpha1
func convertVMClassSpecFromBeta(src *v1beta1.VMClassSpec, dst *VMClassSpec) error {
	// Direct field mappings
	dst.CPU = src.CPU

	// Convert Memory - beta uses resource.Quantity, alpha uses int32 (MiB)
	memoryBytes := src.Memory.Value()
	memoryMiB := memoryBytes / (1024 * 1024)
	if memoryMiB > int64(^uint32(0)>>1) {
		return fmt.Errorf("memory value %d MiB exceeds int32 range", memoryMiB)
	}
	dst.MemoryMiB = int32(memoryMiB)

	// Convert Firmware - beta uses typed enum, alpha uses string
	switch src.Firmware {
	case v1beta1.FirmwareTypeBIOS:
		dst.Firmware = "BIOS"
	case v1beta1.FirmwareTypeUEFI:
		dst.Firmware = "UEFI"
	case v1beta1.FirmwareTypeEFI:
		dst.Firmware = "EFI"
	default:
		dst.Firmware = "BIOS"
	}

	// Convert DiskDefaults
	if src.DiskDefaults != nil {
		sizeGiB := src.DiskDefaults.Size.Value() / (1024 * 1024 * 1024)
		if sizeGiB > int64(^uint32(0)>>1) {
			return fmt.Errorf("disk size %d GiB exceeds int32 range", sizeGiB)
		}

		dst.DiskDefaults = &DiskDefaults{
			SizeGiB: int32(sizeGiB),
		}

		// Convert disk type
		switch src.DiskDefaults.Type {
		case v1beta1.DiskTypeThin:
			dst.DiskDefaults.Type = "thin"
		case v1beta1.DiskTypeThick:
			dst.DiskDefaults.Type = "thick"
		case v1beta1.DiskTypeEagerZeroedThick:
			dst.DiskDefaults.Type = "eagerzeroedthick"
		default:
			dst.DiskDefaults.Type = "thin"
		}

		// New beta fields (IOPS, StorageClass) are ignored
	}

	// Convert GuestToolsPolicy - beta uses typed enum, alpha uses string
	switch src.GuestToolsPolicy {
	case v1beta1.GuestToolsPolicyInstall:
		dst.GuestToolsPolicy = "install"
	case v1beta1.GuestToolsPolicySkip:
		dst.GuestToolsPolicy = "skip"
	case v1beta1.GuestToolsPolicyUpgrade:
		dst.GuestToolsPolicy = "upgrade"
	case v1beta1.GuestToolsPolicyUninstall:
		dst.GuestToolsPolicy = "skip" // Map uninstall to skip for alpha compatibility
	default:
		dst.GuestToolsPolicy = "install"
	}

	// Convert ExtraConfig
	dst.ExtraConfig = src.ExtraConfig

	// New beta fields (ResourceLimits, PerformanceProfile, SecurityProfile) are ignored

	return nil
}

// convertVMClassStatusFromBeta converts v1beta1 VMClassStatus to v1alpha1
func convertVMClassStatusFromBeta(src *v1beta1.VMClassStatus, dst *VMClassStatus) error {
	// Direct field mappings
	dst.Conditions = src.Conditions
	dst.ObservedGeneration = src.ObservedGeneration

	// New beta fields are ignored (UsedByVMs, SupportedProviders, ValidationResults)

	return nil
}
