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

// ConvertTo converts this VMSnapshot to the Hub version (v1beta1).
func (src *VMSnapshot) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.VMSnapshot)

	// ObjectMeta conversion
	dst.ObjectMeta = src.ObjectMeta

	// Spec conversion: Alpha → Beta
	if err := convertVMSnapshotSpecToV1Beta1(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert VMSnapshot spec: %w", err)
	}

	// Status conversion: 1:1 mapping
	if err := convertVMSnapshotStatusToV1Beta1(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert VMSnapshot status: %w", err)
	}

	return nil
}

// ConvertFrom converts from the Hub version (v1beta1) to this version.
func (dst *VMSnapshot) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.VMSnapshot)

	// ObjectMeta conversion
	dst.ObjectMeta = src.ObjectMeta

	// Spec conversion: Beta → Alpha
	if err := convertVMSnapshotSpecFromV1Beta1(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert VMSnapshot spec from v1beta1: %w", err)
	}

	// Status conversion: 1:1 mapping
	if err := convertVMSnapshotStatusFromV1Beta1(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert VMSnapshot status from v1beta1: %w", err)
	}

	return nil
}

func convertVMSnapshotSpecToV1Beta1(src *VMSnapshotSpec, dst *v1beta1.VMSnapshotSpec) error {
	// Convert VM reference 1:1
	dst.VMRef = v1beta1.LocalObjectReference{
		Name: src.VMRef.Name,
	}

	// Convert snapshot configuration
	if src.NameHint != "" || src.Memory || src.Description != "" {
		dst.SnapshotConfig = &v1beta1.SnapshotConfig{}

		if src.NameHint != "" {
			dst.SnapshotConfig.Name = src.NameHint
		}

		if src.Description != "" {
			dst.SnapshotConfig.Description = src.Description
		}

		// Map memory flag directly
		dst.SnapshotConfig.IncludeMemory = src.Memory
	}

	// Convert retention policy 1:1
	if src.RetentionPolicy != nil {
		dst.RetentionPolicy = &v1beta1.SnapshotRetentionPolicy{
			MaxAge:           src.RetentionPolicy.MaxAge,
			DeleteOnVMDelete: src.RetentionPolicy.DeleteOnVMDelete,
		}
	}

	return nil
}

func convertVMSnapshotSpecFromV1Beta1(src *v1beta1.VMSnapshotSpec, dst *VMSnapshotSpec) error {
	// Convert VM reference 1:1
	dst.VMRef = LocalObjectReference{
		Name: src.VMRef.Name,
	}

	// Convert snapshot configuration
	if src.SnapshotConfig != nil {
		if src.SnapshotConfig.Name != "" {
			dst.NameHint = src.SnapshotConfig.Name
		}

		if src.SnapshotConfig.Description != "" {
			dst.Description = src.SnapshotConfig.Description
		}

		// Map memory flag directly - conversion is lossless even if provider doesn't support memory snapshots
		dst.Memory = src.SnapshotConfig.IncludeMemory
	}

	// Convert retention policy 1:1
	if src.RetentionPolicy != nil {
		dst.RetentionPolicy = &SnapshotRetentionPolicy{
			MaxAge:           src.RetentionPolicy.MaxAge,
			DeleteOnVMDelete: src.RetentionPolicy.DeleteOnVMDelete,
		}
	}

	// Check for non-representable fields in v1alpha1
	if src.Schedule != nil {
		return fmt.Errorf("snapshot scheduling is not supported in v1alpha1 API")
	}

	if src.Metadata != nil {
		return fmt.Errorf("snapshot metadata is not supported in v1alpha1 API")
	}

	return nil
}

func convertVMSnapshotStatusToV1Beta1(src *VMSnapshotStatus, dst *v1beta1.VMSnapshotStatus) error {
	// Convert snapshot ID
	if src.SnapshotID != "" {
		dst.SnapshotID = src.SnapshotID
	}

	// Convert phase enum
	if src.Phase != "" {
		dst.Phase = v1beta1.SnapshotPhase(src.Phase)
	}

	// Convert message
	if src.Message != "" {
		dst.Message = src.Message
	}

	// Convert conditions 1:1
	dst.Conditions = append([]metav1.Condition(nil), src.Conditions...)

	// Convert other status fields (only those that exist in alpha)
	dst.CreationTime = src.CreationTime

	return nil
}

func convertVMSnapshotStatusFromV1Beta1(src *v1beta1.VMSnapshotStatus, dst *VMSnapshotStatus) error {
	// Convert snapshot ID
	if src.SnapshotID != "" {
		dst.SnapshotID = src.SnapshotID
	}

	// Convert phase enum
	if src.Phase != "" {
		dst.Phase = SnapshotPhase(src.Phase)
	}

	// Convert message
	if src.Message != "" {
		dst.Message = src.Message
	}

	// Convert conditions 1:1
	dst.Conditions = append([]metav1.Condition(nil), src.Conditions...)

	// Convert other status fields (only those that exist in alpha)
	dst.CreationTime = src.CreationTime

	return nil
}
