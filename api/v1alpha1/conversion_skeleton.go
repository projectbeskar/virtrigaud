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
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// Conversion functions for VMImage
func (src *VMImage) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.VMImage)
	dst.ObjectMeta = src.ObjectMeta
	// TODO: Implement detailed conversion for VMImage
	// Key changes: Enhanced source configuration, metadata, distribution info
	return nil
}

func (dst *VMImage) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.VMImage)
	dst.ObjectMeta = src.ObjectMeta
	// TODO: Implement detailed conversion from VMImage beta
	return nil
}

// Conversion functions for VMNetworkAttachment
func (src *VMNetworkAttachment) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.VMNetworkAttachment)
	dst.ObjectMeta = src.ObjectMeta
	// TODO: Implement detailed conversion for VMNetworkAttachment
	// Key changes: Enhanced network config, IP allocation, security, QoS
	return nil
}

func (dst *VMNetworkAttachment) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.VMNetworkAttachment)
	dst.ObjectMeta = src.ObjectMeta
	// TODO: Implement detailed conversion from VMNetworkAttachment beta
	return nil
}

// Conversion functions for VMSnapshot
func (src *VMSnapshot) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.VMSnapshot)
	dst.ObjectMeta = src.ObjectMeta
	// TODO: Implement detailed conversion for VMSnapshot
	// Key changes: Enhanced config, retention policy, scheduling, metadata
	return nil
}

func (dst *VMSnapshot) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.VMSnapshot)
	dst.ObjectMeta = src.ObjectMeta
	// TODO: Implement detailed conversion from VMSnapshot beta
	return nil
}

// Conversion functions for VMClone
func (src *VMClone) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.VMClone)
	dst.ObjectMeta = src.ObjectMeta
	// TODO: Implement detailed conversion for VMClone
	// Key changes: Enhanced source/target config, options, customization
	return nil
}

func (dst *VMClone) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.VMClone)
	dst.ObjectMeta = src.ObjectMeta
	// TODO: Implement detailed conversion from VMClone beta
	return nil
}

// Conversion functions for VMSet
func (src *VMSet) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.VMSet)
	dst.ObjectMeta = src.ObjectMeta
	// TODO: Implement detailed conversion for VMSet
	// Key changes: Enhanced update strategy, PVC retention, ordinals
	return nil
}

func (dst *VMSet) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.VMSet)
	dst.ObjectMeta = src.ObjectMeta
	// TODO: Implement detailed conversion from VMSet beta
	return nil
}

// Conversion functions for VMPlacementPolicy
func (src *VMPlacementPolicy) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.VMPlacementPolicy)
	dst.ObjectMeta = src.ObjectMeta
	// TODO: Implement detailed conversion for VMPlacementPolicy
	// Key changes: Enhanced constraints, resource/security constraints, stats
	return nil
}

func (dst *VMPlacementPolicy) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.VMPlacementPolicy)
	dst.ObjectMeta = src.ObjectMeta
	// TODO: Implement detailed conversion from VMPlacementPolicy beta
	return nil
}
