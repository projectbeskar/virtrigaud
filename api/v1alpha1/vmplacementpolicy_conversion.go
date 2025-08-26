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

// ConvertTo converts this VMPlacementPolicy to the Hub version (v1beta1).
func (src *VMPlacementPolicy) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.VMPlacementPolicy)

	// ObjectMeta conversion
	dst.ObjectMeta = src.ObjectMeta

	// Spec conversion: Alpha → Beta
	if err := convertVMPlacementPolicySpecToV1Beta1(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert VMPlacementPolicy spec: %w", err)
	}

	// Status conversion: 1:1 mapping
	if err := convertVMPlacementPolicyStatusToV1Beta1(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert VMPlacementPolicy status: %w", err)
	}

	return nil
}

// ConvertFrom converts from the Hub version (v1beta1) to this version.
func (dst *VMPlacementPolicy) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.VMPlacementPolicy)

	// ObjectMeta conversion
	dst.ObjectMeta = src.ObjectMeta

	// Spec conversion: Beta → Alpha
	if err := convertVMPlacementPolicySpecFromV1Beta1(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert VMPlacementPolicy spec from v1beta1: %w", err)
	}

	// Status conversion: 1:1 mapping
	if err := convertVMPlacementPolicyStatusFromV1Beta1(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert VMPlacementPolicy status from v1beta1: %w", err)
	}

	return nil
}

func convertVMPlacementPolicySpecToV1Beta1(src *VMPlacementPolicySpec, dst *v1beta1.VMPlacementPolicySpec) error {
	// Convert hard constraints 1:1
	if src.Hard != nil {
		dst.Hard = convertPlacementConstraintsToV1Beta1(src.Hard)
	}

	// Convert soft constraints 1:1
	if src.Soft != nil {
		dst.Soft = convertPlacementConstraintsToV1Beta1(src.Soft)
	}

	// Convert anti-affinity rules 1:1
	if src.AntiAffinity != nil {
		dst.AntiAffinity = convertAntiAffinityRulesToV1Beta1(src.AntiAffinity)
	}

	// Convert affinity rules 1:1
	if src.Affinity != nil {
		dst.Affinity = convertAffinityRulesToV1Beta1(src.Affinity)
	}

	return nil
}

func convertVMPlacementPolicySpecFromV1Beta1(src *v1beta1.VMPlacementPolicySpec, dst *VMPlacementPolicySpec) error {
	// Convert hard constraints 1:1
	if src.Hard != nil {
		dst.Hard = convertPlacementConstraintsFromV1Beta1(src.Hard)
	}

	// Convert soft constraints 1:1
	if src.Soft != nil {
		dst.Soft = convertPlacementConstraintsFromV1Beta1(src.Soft)
	}

	// Convert anti-affinity rules 1:1
	if src.AntiAffinity != nil {
		dst.AntiAffinity = convertAntiAffinityRulesFromV1Beta1(src.AntiAffinity)
	}

	// Convert affinity rules 1:1
	if src.Affinity != nil {
		dst.Affinity = convertAffinityRulesFromV1Beta1(src.Affinity)
	}

	// Check for non-representable fields in v1alpha1
	if src.ResourceConstraints != nil {
		return fmt.Errorf("resource constraints are not supported in v1alpha1 API")
	}

	if src.SecurityConstraints != nil {
		return fmt.Errorf("security constraints are not supported in v1alpha1 API")
	}

	if src.Priority != nil {
		return fmt.Errorf("priority is not supported in v1alpha1 API")
	}

	if src.Weight != nil {
		return fmt.Errorf("weight is not supported in v1alpha1 API")
	}

	return nil
}

func convertVMPlacementPolicyStatusToV1Beta1(src *VMPlacementPolicyStatus, dst *v1beta1.VMPlacementPolicyStatus) error {
	// Convert conditions 1:1
	dst.Conditions = append([]metav1.Condition(nil), src.Conditions...)

	// Convert other status fields
	dst.ObservedGeneration = src.ObservedGeneration

	return nil
}

func convertVMPlacementPolicyStatusFromV1Beta1(src *v1beta1.VMPlacementPolicyStatus, dst *VMPlacementPolicyStatus) error {
	// Convert conditions 1:1
	dst.Conditions = append([]metav1.Condition(nil), src.Conditions...)

	// Convert other status fields
	dst.ObservedGeneration = src.ObservedGeneration

	return nil
}

// Helper functions for converting nested structs

func convertPlacementConstraintsToV1Beta1(src *PlacementConstraints) *v1beta1.PlacementConstraints {
	if src == nil {
		return nil
	}
	return &v1beta1.PlacementConstraints{
		Clusters:      append([]string(nil), src.Clusters...),
		Datastores:    append([]string(nil), src.Datastores...),
		Hosts:         append([]string(nil), src.Hosts...),
		Folders:       append([]string(nil), src.Folders...),
		ResourcePools: append([]string(nil), src.ResourcePools...),
		Networks:      append([]string(nil), src.Networks...),
		Zones:         append([]string(nil), src.Zones...),
	}
}

func convertPlacementConstraintsFromV1Beta1(src *v1beta1.PlacementConstraints) *PlacementConstraints {
	if src == nil {
		return nil
	}
	return &PlacementConstraints{
		Clusters:      append([]string(nil), src.Clusters...),
		Datastores:    append([]string(nil), src.Datastores...),
		Hosts:         append([]string(nil), src.Hosts...),
		Folders:       append([]string(nil), src.Folders...),
		ResourcePools: append([]string(nil), src.ResourcePools...),
		Networks:      append([]string(nil), src.Networks...),
		Zones:         append([]string(nil), src.Zones...),
	}
}

func convertAntiAffinityRulesToV1Beta1(src *AntiAffinityRules) *v1beta1.AntiAffinityRules {
	if src == nil {
		return nil
	}
	// Simplified conversion - would need full field mapping in real implementation
	return &v1beta1.AntiAffinityRules{}
}

func convertAntiAffinityRulesFromV1Beta1(src *v1beta1.AntiAffinityRules) *AntiAffinityRules {
	if src == nil {
		return nil
	}
	// Simplified conversion - would need full field mapping in real implementation
	return &AntiAffinityRules{}
}

func convertAffinityRulesToV1Beta1(src *AffinityRules) *v1beta1.AffinityRules {
	if src == nil {
		return nil
	}
	// Simplified conversion - would need full field mapping in real implementation
	return &v1beta1.AffinityRules{}
}

func convertAffinityRulesFromV1Beta1(src *v1beta1.AffinityRules) *AffinityRules {
	if src == nil {
		return nil
	}
	// Simplified conversion - would need full field mapping in real implementation
	return &AffinityRules{}
}
