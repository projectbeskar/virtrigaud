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
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// ConvertTo converts this VMSet to the Hub version (v1beta1).
func (src *VMSet) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.VMSet)

	// ObjectMeta conversion
	dst.ObjectMeta = src.ObjectMeta

	// Spec conversion: Alpha → Beta
	if err := convertVMSetSpecToV1Beta1(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert VMSet spec: %w", err)
	}

	// Status conversion: 1:1 mapping
	if err := convertVMSetStatusToV1Beta1(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert VMSet status: %w", err)
	}

	return nil
}

// ConvertFrom converts from the Hub version (v1beta1) to this version.
func (dst *VMSet) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.VMSet)

	// ObjectMeta conversion
	dst.ObjectMeta = src.ObjectMeta

	// Spec conversion: Beta → Alpha
	if err := convertVMSetSpecFromV1Beta1(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert VMSet spec from v1beta1: %w", err)
	}

	// Status conversion: 1:1 mapping
	if err := convertVMSetStatusFromV1Beta1(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert VMSet status from v1beta1: %w", err)
	}

	return nil
}

func convertVMSetSpecToV1Beta1(src *VMSetSpec, dst *v1beta1.VMSetSpec) error {
	// Convert basic fields 1:1
	dst.Replicas = src.Replicas
	dst.MinReadySeconds = src.MinReadySeconds
	dst.RevisionHistoryLimit = src.RevisionHistoryLimit

	// Convert selector 1:1
	if src.Selector != nil {
		dst.Selector = &metav1.LabelSelector{
			MatchLabels:      src.Selector.MatchLabels,
			MatchExpressions: src.Selector.MatchExpressions,
		}
	}

	// Convert template: Alpha and Beta both have VMSetTemplate but with different VM specs
	dst.Template = v1beta1.VMSetTemplate{
		ObjectMeta: src.Template.ObjectMeta,
	}

	// Convert embedded VirtualMachine spec using the VM conversion helper
	srcVM := &VirtualMachine{Spec: src.Template.Spec}
	dstVM := &v1beta1.VirtualMachine{}
	if err := srcVM.ConvertTo(dstVM); err != nil {
		return fmt.Errorf("failed to convert embedded VM spec: %w", err)
	}
	dst.Template.Spec = dstVM.Spec

	// Convert update strategy
	if err := convertVMSetUpdateStrategyToV1Beta1(&src.UpdateStrategy, &dst.UpdateStrategy); err != nil {
		return fmt.Errorf("failed to convert update strategy: %w", err)
	}

	return nil
}

func convertVMSetSpecFromV1Beta1(src *v1beta1.VMSetSpec, dst *VMSetSpec) error {
	// Convert basic fields 1:1
	dst.Replicas = src.Replicas
	dst.MinReadySeconds = src.MinReadySeconds
	dst.RevisionHistoryLimit = src.RevisionHistoryLimit

	// Convert selector 1:1
	if src.Selector != nil {
		dst.Selector = &metav1.LabelSelector{
			MatchLabels:      src.Selector.MatchLabels,
			MatchExpressions: src.Selector.MatchExpressions,
		}
	}

	// Convert template: Beta and Alpha both have VMSetTemplate but with different VM specs
	dst.Template = VMSetTemplate{
		ObjectMeta: src.Template.ObjectMeta,
	}

	// Convert embedded VirtualMachine spec using the VM conversion helper
	srcVM := &v1beta1.VirtualMachine{Spec: src.Template.Spec}
	dstVM := &VirtualMachine{}
	if err := dstVM.ConvertFrom(srcVM); err != nil {
		return fmt.Errorf("failed to convert embedded VM spec: %w", err)
	}
	dst.Template.Spec = dstVM.Spec

	// Convert update strategy
	if err := convertVMSetUpdateStrategyFromV1Beta1(&src.UpdateStrategy, &dst.UpdateStrategy); err != nil {
		return fmt.Errorf("failed to convert update strategy: %w", err)
	}

	// Check for non-representable fields in v1alpha1
	if src.PersistentVolumeClaimRetentionPolicy != nil {
		return fmt.Errorf("PVC retention policy is not supported in v1alpha1 API")
	}

	if src.Ordinals != nil {
		return fmt.Errorf("ordinals configuration is not supported in v1alpha1 API")
	}

	if src.ServiceName != "" {
		return fmt.Errorf("service name is not supported in v1alpha1 API")
	}

	if len(src.VolumeClaimTemplates) > 0 {
		return fmt.Errorf("volume claim templates are not supported in v1alpha1 API")
	}

	return nil
}

func convertVMSetUpdateStrategyToV1Beta1(src *VMSetUpdateStrategy, dst *v1beta1.VMSetUpdateStrategy) error {
	// Convert strategy type enum
	if src.Type != "" {
		dst.Type = v1beta1.VMSetUpdateStrategyType(src.Type)
	}

	// Convert rolling update configuration
	if src.RollingUpdate != nil {
		dst.RollingUpdate = &v1beta1.RollingUpdateVMSetStrategy{}

		if src.RollingUpdate.MaxUnavailable != nil {
			dst.RollingUpdate.MaxUnavailable = &intstr.IntOrString{
				Type:   src.RollingUpdate.MaxUnavailable.Type,
				IntVal: src.RollingUpdate.MaxUnavailable.IntVal,
				StrVal: src.RollingUpdate.MaxUnavailable.StrVal,
			}
		}

		if src.RollingUpdate.MaxSurge != nil {
			dst.RollingUpdate.MaxSurge = &intstr.IntOrString{
				Type:   src.RollingUpdate.MaxSurge.Type,
				IntVal: src.RollingUpdate.MaxSurge.IntVal,
				StrVal: src.RollingUpdate.MaxSurge.StrVal,
			}
		}
	}

	return nil
}

func convertVMSetUpdateStrategyFromV1Beta1(src *v1beta1.VMSetUpdateStrategy, dst *VMSetUpdateStrategy) error {
	// Convert strategy type enum
	if src.Type != "" {
		dst.Type = VMSetUpdateStrategyType(src.Type)
	}

	// Convert rolling update configuration
	if src.RollingUpdate != nil {
		dst.RollingUpdate = &RollingUpdateVMSetStrategy{}

		if src.RollingUpdate.MaxUnavailable != nil {
			dst.RollingUpdate.MaxUnavailable = &intstr.IntOrString{
				Type:   src.RollingUpdate.MaxUnavailable.Type,
				IntVal: src.RollingUpdate.MaxUnavailable.IntVal,
				StrVal: src.RollingUpdate.MaxUnavailable.StrVal,
			}
		}

		if src.RollingUpdate.MaxSurge != nil {
			dst.RollingUpdate.MaxSurge = &intstr.IntOrString{
				Type:   src.RollingUpdate.MaxSurge.Type,
				IntVal: src.RollingUpdate.MaxSurge.IntVal,
				StrVal: src.RollingUpdate.MaxSurge.StrVal,
			}
		}
	}

	return nil
}

func convertVMSetStatusToV1Beta1(src *VMSetStatus, dst *v1beta1.VMSetStatus) error {
	// Convert basic status fields (only those that exist in alpha)
	dst.Replicas = src.Replicas
	dst.ReadyReplicas = src.ReadyReplicas
	dst.UpdatedReplicas = src.UpdatedReplicas
	dst.AvailableReplicas = src.AvailableReplicas
	dst.CollisionCount = src.CollisionCount

	// Convert conditions 1:1
	dst.Conditions = append([]metav1.Condition(nil), src.Conditions...)

	return nil
}

func convertVMSetStatusFromV1Beta1(src *v1beta1.VMSetStatus, dst *VMSetStatus) error {
	// Convert basic status fields (only those that exist in alpha)
	dst.Replicas = src.Replicas
	dst.ReadyReplicas = src.ReadyReplicas
	dst.UpdatedReplicas = src.UpdatedReplicas
	dst.AvailableReplicas = src.AvailableReplicas
	dst.CollisionCount = src.CollisionCount

	// Convert conditions 1:1
	dst.Conditions = append([]metav1.Condition(nil), src.Conditions...)

	return nil
}
