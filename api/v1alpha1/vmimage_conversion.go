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

// ConvertTo converts this VMImage to the Hub version (v1beta1).
func (src *VMImage) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.VMImage)

	// ObjectMeta conversion
	dst.ObjectMeta = src.ObjectMeta

	// Spec conversion: Alpha → Beta
	if err := convertVMImageSpecToV1Beta1(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert VMImage spec: %w", err)
	}

	// Status conversion: 1:1 mapping
	if err := convertVMImageStatusToV1Beta1(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert VMImage status: %w", err)
	}

	return nil
}

// ConvertFrom converts from the Hub version (v1beta1) to this version.
func (dst *VMImage) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.VMImage)

	// ObjectMeta conversion
	dst.ObjectMeta = src.ObjectMeta

	// Spec conversion: Beta → Alpha
	if err := convertVMImageSpecFromV1Beta1(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert VMImage spec from v1beta1: %w", err)
	}

	// Status conversion: 1:1 mapping
	if err := convertVMImageStatusFromV1Beta1(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert VMImage status from v1beta1: %w", err)
	}

	return nil
}

func convertVMImageSpecToV1Beta1(src *VMImageSpec, dst *v1beta1.VMImageSpec) error {
	// Convert source configuration: alpha has separate vsphere/libvirt fields,
	// beta has them under spec.source
	dst.Source = v1beta1.ImageSource{}

	if src.VSphere != nil {
		dst.Source.VSphere = convertVSphereImageSpecToV1Beta1(src.VSphere)
	}

	if src.Libvirt != nil {
		dst.Source.Libvirt = convertLibvirtImageSpecToV1Beta1(src.Libvirt)
	}

	// Convert preparation settings 1:1
	if src.Prepare != nil {
		dst.Prepare = convertImagePrepareToV1Beta1(src.Prepare)
	}

	return nil
}

func convertVMImageSpecFromV1Beta1(src *v1beta1.VMImageSpec, dst *VMImageSpec) error {
	// Convert source configuration: beta has source.vsphere/libvirt,
	// alpha has separate vsphere/libvirt fields

	if src.Source.VSphere != nil {
		dst.VSphere = convertVSphereImageSpecFromV1Beta1(src.Source.VSphere)
	}

	if src.Source.Libvirt != nil {
		dst.Libvirt = convertLibvirtImageSpecFromV1Beta1(src.Source.Libvirt)
	}

	// Check for non-representable sources in alpha
	if src.Source.HTTP != nil {
		return fmt.Errorf("HTTP image sources are not supported in v1alpha1 API")
	}
	if src.Source.Registry != nil {
		return fmt.Errorf("registry image sources are not supported in v1alpha1 API")
	}
	if src.Source.DataVolume != nil {
		return fmt.Errorf("DataVolume image sources are not supported in v1alpha1 API")
	}

	// Convert preparation settings 1:1
	if src.Prepare != nil {
		dst.Prepare = convertImagePrepareFromV1Beta1(src.Prepare)
	}

	return nil
}

func convertVMImageStatusToV1Beta1(src *VMImageStatus, dst *v1beta1.VMImageStatus) error {
	// 1:1 mapping for most status fields
	dst.Ready = src.Ready
	dst.AvailableOn = append([]string(nil), src.AvailableOn...)
	dst.Conditions = append([]metav1.Condition(nil), src.Conditions...)
	dst.ObservedGeneration = src.ObservedGeneration
	dst.LastPrepareTime = src.LastPrepareTime
	dst.Message = src.Message

	// Convert phase enum
	if src.Phase != "" {
		dst.Phase = v1beta1.ImagePhase(src.Phase)
	}

	// Note: Size and ProviderStatus are not present in v1alpha1

	return nil
}

func convertVMImageStatusFromV1Beta1(src *v1beta1.VMImageStatus, dst *VMImageStatus) error {
	// 1:1 mapping for most status fields
	dst.Ready = src.Ready
	dst.AvailableOn = append([]string(nil), src.AvailableOn...)
	dst.Conditions = append([]metav1.Condition(nil), src.Conditions...)
	dst.ObservedGeneration = src.ObservedGeneration
	dst.LastPrepareTime = src.LastPrepareTime
	dst.Message = src.Message

	// Convert phase enum
	if src.Phase != "" {
		dst.Phase = ImagePhase(src.Phase)
	}

	// Note: Size and ProviderStatus are not present in v1alpha1

	return nil
}

// Helper functions for converting nested structs
// These would need to be implemented based on the actual struct definitions

func convertVSphereImageSpecToV1Beta1(src *VSphereImageSpec) *v1beta1.VSphereImageSource {
	if src == nil {
		return nil
	}
	return &v1beta1.VSphereImageSource{
		TemplateName: src.TemplateName,
		// Add other fields as needed based on struct definitions
	}
}

func convertVSphereImageSpecFromV1Beta1(src *v1beta1.VSphereImageSource) *VSphereImageSpec {
	if src == nil {
		return nil
	}
	return &VSphereImageSpec{
		TemplateName: src.TemplateName,
		// Add other fields as needed based on struct definitions
	}
}

func convertLibvirtImageSpecToV1Beta1(src *LibvirtImageSpec) *v1beta1.LibvirtImageSource {
	if src == nil {
		return nil
	}
	return &v1beta1.LibvirtImageSource{
		Path:   src.Path,
		URL:    src.URL,
		Format: v1beta1.ImageFormat(src.Format),
	}
}

func convertLibvirtImageSpecFromV1Beta1(src *v1beta1.LibvirtImageSource) *LibvirtImageSpec {
	if src == nil {
		return nil
	}
	return &LibvirtImageSpec{
		Path:   src.Path,
		URL:    src.URL,
		Format: string(src.Format),
	}
}

func convertImagePrepareToV1Beta1(src *ImagePrepare) *v1beta1.ImagePrepare {
	if src == nil {
		return nil
	}
	return &v1beta1.ImagePrepare{
		// Add fields based on struct definitions
	}
}

func convertImagePrepareFromV1Beta1(src *v1beta1.ImagePrepare) *ImagePrepare {
	if src == nil {
		return nil
	}
	return &ImagePrepare{
		// Add fields based on struct definitions
	}
}

// Note: ImageProviderStatus helpers removed as they don't exist in v1alpha1
