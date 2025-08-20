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

// VMNetworkAttachmentSpec defines the desired state of VMNetworkAttachment.
type VMNetworkAttachmentSpec struct {
	// VSphere contains vSphere-specific network configuration
	// +optional
	VSphere *VSphereNetworkSpec `json:"vsphere,omitempty"`

	// Libvirt contains Libvirt-specific network configuration
	// +optional
	Libvirt *LibvirtNetworkSpec `json:"libvirt,omitempty"`

	// IPPolicy specifies the default IP assignment policy
	// +optional
	// +kubebuilder:default="dhcp"
	// +kubebuilder:validation:Enum=dhcp;static
	IPPolicy string `json:"ipPolicy,omitempty"`

	// MacAddress specifies a static MAC address
	// +optional
	MacAddress string `json:"macAddress,omitempty"`
}

// VMNetworkAttachmentStatus defines the observed state of VMNetworkAttachment.
type VMNetworkAttachmentStatus struct {
	// Ready indicates if the network is ready for use
	// +optional
	Ready bool `json:"ready,omitempty"`

	// AvailableOn lists the providers where the network is available
	// +optional
	AvailableOn []string `json:"availableOn,omitempty"`

	// Conditions represent the latest available observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration reflects the generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// VSphereNetworkSpec defines vSphere-specific network configuration
type VSphereNetworkSpec struct {
	// Portgroup specifies the vSphere portgroup name
	// +optional
	Portgroup string `json:"portgroup,omitempty"`

	// NetworkName specifies the vSphere network name
	// +optional
	NetworkName string `json:"networkName,omitempty"`

	// VLAN specifies the VLAN ID
	// +optional
	VLAN int32 `json:"vlan,omitempty"`
}

// LibvirtNetworkSpec defines Libvirt-specific network configuration
type LibvirtNetworkSpec struct {
	// NetworkName specifies the Libvirt network name
	// +optional
	NetworkName string `json:"networkName,omitempty"`

	// Bridge specifies the bridge name
	// +optional
	Bridge string `json:"bridge,omitempty"`

	// Model specifies the network device model
	// +optional
	// +kubebuilder:default="virtio"
	// +kubebuilder:validation:Enum=virtio;e1000;rtl8139
	Model string `json:"model,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="IP Policy",type="string",JSONPath=".spec.ipPolicy"
// +kubebuilder:printcolumn:name="Providers",type="string",JSONPath=".status.availableOn"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// VMNetworkAttachment is the Schema for the vmnetworkattachments API.
type VMNetworkAttachment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VMNetworkAttachmentSpec   `json:"spec,omitempty"`
	Status VMNetworkAttachmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VMNetworkAttachmentList contains a list of VMNetworkAttachment.
type VMNetworkAttachmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VMNetworkAttachment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VMNetworkAttachment{}, &VMNetworkAttachmentList{})
}
