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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VMClassSpec defines the desired state of VMClass.
type VMClassSpec struct {
	// CPU specifies the number of virtual CPUs
	CPU int32 `json:"cpu"`

	// MemoryMiB specifies memory in MiB
	MemoryMiB int32 `json:"memoryMiB"`

	// Firmware specifies the firmware type
	// +optional
	// +kubebuilder:default="BIOS"
	// +kubebuilder:validation:Enum=BIOS;UEFI
	Firmware string `json:"firmware,omitempty"`

	// DiskDefaults provides default disk settings
	// +optional
	DiskDefaults *DiskDefaults `json:"diskDefaults,omitempty"`

	// GuestToolsPolicy specifies guest tools installation policy
	// +optional
	// +kubebuilder:default="install"
	// +kubebuilder:validation:Enum=install;skip;upgrade
	GuestToolsPolicy string `json:"guestToolsPolicy,omitempty"`

	// ExtraConfig contains provider-specific extra configuration
	// +optional
	ExtraConfig map[string]string `json:"extraConfig,omitempty"`
}

// VMClassStatus defines the observed state of VMClass.
type VMClassStatus struct {
	// Conditions represent the latest available observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration reflects the generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// DiskDefaults provides default disk settings
type DiskDefaults struct {
	// Type specifies the default disk type
	// +optional
	// +kubebuilder:default="thin"
	// +kubebuilder:validation:Enum=thin;thick;eagerzeroedthick
	Type string `json:"type,omitempty"`

	// SizeGiB specifies the default root disk size in GiB
	// +optional
	// +kubebuilder:default=40
	SizeGiB int32 `json:"sizeGiB,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="CPU",type="integer",JSONPath=".spec.cpu"
// +kubebuilder:printcolumn:name="Memory",type="string",JSONPath=".spec.memoryMiB"
// +kubebuilder:printcolumn:name="Firmware",type="string",JSONPath=".spec.firmware"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// VMClass is the Schema for the vmclasses API.
type VMClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VMClassSpec   `json:"spec,omitempty"`
	Status VMClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VMClassList contains a list of VMClass.
type VMClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VMClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VMClass{}, &VMClassList{})
}
