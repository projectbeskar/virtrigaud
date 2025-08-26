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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// ConvertTo converts this VMNetworkAttachment to the Hub version (v1beta1).
func (src *VMNetworkAttachment) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.VMNetworkAttachment)

	// ObjectMeta conversion
	dst.ObjectMeta = src.ObjectMeta

	// Spec conversion: Alpha → Beta
	if err := convertVMNetworkAttachmentSpecToV1Beta1(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert VMNetworkAttachment spec: %w", err)
	}

	// Status conversion: 1:1 mapping
	if err := convertVMNetworkAttachmentStatusToV1Beta1(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert VMNetworkAttachment status: %w", err)
	}

	return nil
}

// ConvertFrom converts from the Hub version (v1beta1) to this version.
func (dst *VMNetworkAttachment) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.VMNetworkAttachment)

	// ObjectMeta conversion
	dst.ObjectMeta = src.ObjectMeta

	// Spec conversion: Beta → Alpha
	if err := convertVMNetworkAttachmentSpecFromV1Beta1(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert VMNetworkAttachment spec from v1beta1: %w", err)
	}

	// Status conversion: 1:1 mapping
	if err := convertVMNetworkAttachmentStatusFromV1Beta1(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert VMNetworkAttachment status from v1beta1: %w", err)
	}

	return nil
}

func convertVMNetworkAttachmentSpecToV1Beta1(src *VMNetworkAttachmentSpec, dst *v1beta1.VMNetworkAttachmentSpec) error {
	// Convert network configuration
	dst.Network = v1beta1.NetworkConfig{}

	if src.VSphere != nil {
		dst.Network.VSphere = convertVSphereNetworkSpecToV1Beta1(src.VSphere)
	}

	if src.Libvirt != nil {
		dst.Network.Libvirt = convertLibvirtNetworkSpecToV1Beta1(src.Libvirt)
	}

	// Convert IP policy to IPAllocation
	if src.IPPolicy != "" {
		dst.IPAllocation = &v1beta1.IPAllocationConfig{}

		switch src.IPPolicy {
		case "dhcp":
			dst.IPAllocation.Type = v1beta1.IPAllocationTypeDHCP
		case "static":
			dst.IPAllocation.Type = v1beta1.IPAllocationTypeStatic
		default:
			return fmt.Errorf("unsupported IPPolicy value: %s", src.IPPolicy)
		}
	}

	return nil
}

func convertVMNetworkAttachmentSpecFromV1Beta1(src *v1beta1.VMNetworkAttachmentSpec, dst *VMNetworkAttachmentSpec) error {
	// Convert network configuration
	if src.Network.VSphere != nil {
		dst.VSphere = convertVSphereNetworkSpecFromV1Beta1(src.Network.VSphere)
	}

	if src.Network.Libvirt != nil {
		dst.Libvirt = convertLibvirtNetworkSpecFromV1Beta1(src.Network.Libvirt)
	}

	// Convert IPAllocation to IP policy
	if src.IPAllocation != nil {
		switch src.IPAllocation.Type {
		case v1beta1.IPAllocationTypeDHCP:
			dst.IPPolicy = "dhcp"
		case v1beta1.IPAllocationTypeStatic:
			dst.IPPolicy = "static"
		case v1beta1.IPAllocationTypePool:
			return fmt.Errorf("IPAllocationTypePool is not supported in v1alpha1 API")
		case v1beta1.IPAllocationTypeNone:
			return fmt.Errorf("IPAllocationTypeNone is not supported in v1alpha1 API")
		default:
			return fmt.Errorf("unsupported IPAllocationType: %s", src.IPAllocation.Type)
		}
	}

	return nil
}

func convertVMNetworkAttachmentStatusToV1Beta1(src *VMNetworkAttachmentStatus, dst *v1beta1.VMNetworkAttachmentStatus) error {
	// 1:1 mapping for status fields
	dst.Ready = src.Ready
	dst.AvailableOn = append([]string(nil), src.AvailableOn...)
	dst.Conditions = append([]metav1.Condition(nil), src.Conditions...)
	dst.ObservedGeneration = src.ObservedGeneration

	return nil
}

func convertVMNetworkAttachmentStatusFromV1Beta1(src *v1beta1.VMNetworkAttachmentStatus, dst *VMNetworkAttachmentStatus) error {
	// 1:1 mapping for status fields
	dst.Ready = src.Ready
	dst.AvailableOn = append([]string(nil), src.AvailableOn...)
	dst.Conditions = append([]metav1.Condition(nil), src.Conditions...)
	dst.ObservedGeneration = src.ObservedGeneration

	return nil
}

// Helper functions for converting nested structs

func convertVSphereNetworkSpecToV1Beta1(src *VSphereNetworkSpec) *v1beta1.VSphereNetworkConfig {
	if src == nil {
		return nil
	}
	return &v1beta1.VSphereNetworkConfig{
		Portgroup: src.Portgroup,
		// Map other fields as available in both APIs
	}
}

func convertVSphereNetworkSpecFromV1Beta1(src *v1beta1.VSphereNetworkConfig) *VSphereNetworkSpec {
	if src == nil {
		return nil
	}
	return &VSphereNetworkSpec{
		Portgroup: src.Portgroup,
		// Map other fields as available in both APIs
	}
}

func convertLibvirtNetworkSpecToV1Beta1(src *LibvirtNetworkSpec) *v1beta1.LibvirtNetworkConfig {
	if src == nil {
		return nil
	}
	// Simplified conversion - would need full field mapping in real implementation
	return &v1beta1.LibvirtNetworkConfig{
		// Bridge field structure differs between versions
	}
}

func convertLibvirtNetworkSpecFromV1Beta1(src *v1beta1.LibvirtNetworkConfig) *LibvirtNetworkSpec {
	if src == nil {
		return nil
	}
	// Simplified conversion - would need full field mapping in real implementation
	return &LibvirtNetworkSpec{
		// Bridge field structure differs between versions
	}
}
