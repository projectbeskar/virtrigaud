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

// ConvertTo converts this VMClone to the Hub version (v1beta1).
func (src *VMClone) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.VMClone)

	// ObjectMeta conversion
	dst.ObjectMeta = src.ObjectMeta

	// Spec conversion: Alpha → Beta
	if err := convertVMCloneSpecToV1Beta1(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert VMClone spec: %w", err)
	}

	// Status conversion: 1:1 mapping
	if err := convertVMCloneStatusToV1Beta1(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert VMClone status: %w", err)
	}

	return nil
}

// ConvertFrom converts from the Hub version (v1beta1) to this version.
func (dst *VMClone) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.VMClone)

	// ObjectMeta conversion
	dst.ObjectMeta = src.ObjectMeta

	// Spec conversion: Beta → Alpha
	if err := convertVMCloneSpecFromV1Beta1(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert VMClone spec from v1beta1: %w", err)
	}

	// Status conversion: 1:1 mapping
	if err := convertVMCloneStatusFromV1Beta1(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert VMClone status from v1beta1: %w", err)
	}

	return nil
}

func convertVMCloneSpecToV1Beta1(src *VMCloneSpec, dst *v1beta1.VMCloneSpec) error {
	// Convert source reference: Alpha has simple SourceRef, Beta has CloneSource with multiple options
	dst.Source = v1beta1.CloneSource{
		VMRef: &v1beta1.LocalObjectReference{
			Name: src.SourceRef.Name,
		},
	}

	// Convert target configuration 1:1
	dst.Target = v1beta1.VMCloneTarget{
		Name:      src.Target.Name,
		Namespace: src.Target.Namespace,
	}

	// Convert optional target fields
	if src.Target.ClassRef != nil {
		dst.Target.ClassRef = &v1beta1.LocalObjectReference{
			Name: src.Target.ClassRef.Name,
		}
	}

	if src.Target.PlacementRef != nil {
		dst.Target.PlacementRef = &v1beta1.LocalObjectReference{
			Name: src.Target.PlacementRef.Name,
		}
	}

	// Note: ImageRef is not present in v1beta1.VMCloneTarget

	// Convert clone options: Alpha has separate fields, Beta has CloneOptions struct
	if src.Linked || src.PowerOn {
		dst.Options = &v1beta1.CloneOptions{
			PowerOn: src.PowerOn,
		}

		if src.Linked {
			dst.Options.Type = v1beta1.CloneTypeLinkedClone
		} else {
			dst.Options.Type = v1beta1.CloneTypeFullClone
		}
	}

	// Convert customization 1:1
	if src.Customization != nil {
		dst.Customization = convertVMCustomizationToV1Beta1(src.Customization)
	}

	return nil
}

func convertVMCloneSpecFromV1Beta1(src *v1beta1.VMCloneSpec, dst *VMCloneSpec) error {
	// Convert source: Beta has multiple source types, Alpha only supports VM reference
	if src.Source.VMRef != nil {
		dst.SourceRef = LocalObjectReference{
			Name: src.Source.VMRef.Name,
		}
	} else if src.Source.SnapshotRef != nil {
		return fmt.Errorf("snapshot-based cloning is not supported in v1alpha1 API")
	} else if src.Source.TemplateRef != nil {
		return fmt.Errorf("template-based cloning is not supported in v1alpha1 API")
	} else if src.Source.ImageRef != nil {
		return fmt.Errorf("image-based cloning is not supported in v1alpha1 API")
	} else {
		return fmt.Errorf("no valid source specified for clone")
	}

	// Convert target configuration 1:1
	dst.Target = VMCloneTarget{
		Name:      src.Target.Name,
		Namespace: src.Target.Namespace,
	}

	// Convert optional target fields
	if src.Target.ClassRef != nil {
		dst.Target.ClassRef = &LocalObjectReference{
			Name: src.Target.ClassRef.Name,
		}
	}

	if src.Target.PlacementRef != nil {
		dst.Target.PlacementRef = &LocalObjectReference{
			Name: src.Target.PlacementRef.Name,
		}
	}

	// Note: ImageRef doesn't exist in beta VMCloneTarget, only in alpha

	// Convert clone options: Beta has CloneOptions struct, Alpha has separate fields
	if src.Options != nil {
		dst.PowerOn = src.Options.PowerOn

		// Map clone type to linked flag
		switch src.Options.Type {
		case v1beta1.CloneTypeLinkedClone:
			dst.Linked = true
		case v1beta1.CloneTypeFullClone:
			dst.Linked = false
		case v1beta1.CloneTypeInstantClone:
			return fmt.Errorf("instant clone type is not supported in v1alpha1 API")
		}
	}

	// Convert customization 1:1
	if src.Customization != nil {
		dst.Customization = convertVMCustomizationFromV1Beta1(src.Customization)
	}

	// Check for non-representable fields
	if src.Metadata != nil {
		return fmt.Errorf("clone metadata is not supported in v1alpha1 API")
	}

	return nil
}

func convertVMCloneStatusToV1Beta1(src *VMCloneStatus, dst *v1beta1.VMCloneStatus) error {
	// Convert phase enum
	if src.Phase != "" {
		dst.Phase = v1beta1.ClonePhase(src.Phase)
	}

	// Convert message
	if src.Message != "" {
		dst.Message = src.Message
	}

	// Convert conditions 1:1
	dst.Conditions = append([]metav1.Condition(nil), src.Conditions...)

	// Convert other status fields
	dst.StartTime = src.StartTime
	dst.CompletionTime = src.CompletionTime
	if src.TargetRef != nil {
		dst.TargetRef = &v1beta1.LocalObjectReference{
			Name: src.TargetRef.Name,
		}
	}

	// Note: ObservedGeneration and Error may not exist in alpha version

	return nil
}

func convertVMCloneStatusFromV1Beta1(src *v1beta1.VMCloneStatus, dst *VMCloneStatus) error {
	// Convert phase enum
	if src.Phase != "" {
		dst.Phase = ClonePhase(src.Phase)
	}

	// Convert message
	if src.Message != "" {
		dst.Message = src.Message
	}

	// Convert conditions 1:1
	dst.Conditions = append([]metav1.Condition(nil), src.Conditions...)

	// Convert other status fields
	dst.StartTime = src.StartTime
	dst.CompletionTime = src.CompletionTime
	if src.TargetRef != nil {
		dst.TargetRef = &LocalObjectReference{
			Name: src.TargetRef.Name,
		}
	}

	// Note: ObservedGeneration and Error may not exist in alpha version

	return nil
}

// Helper functions for converting nested structs

func convertVMCustomizationToV1Beta1(src *VMCustomization) *v1beta1.VMCustomization {
	if src == nil {
		return nil
	}
	return &v1beta1.VMCustomization{
		Hostname: src.Hostname,
		// Add other fields as available in both APIs
	}
}

func convertVMCustomizationFromV1Beta1(src *v1beta1.VMCustomization) *VMCustomization {
	if src == nil {
		return nil
	}
	return &VMCustomization{
		Hostname: src.Hostname,
		// Add other fields as available in both APIs
	}
}
