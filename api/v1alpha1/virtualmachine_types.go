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

// VirtualMachineSpec defines the desired state of VirtualMachine.
type VirtualMachineSpec struct {
	// ProviderRef references the Provider that manages this VM
	ProviderRef ObjectRef `json:"providerRef"`

	// ClassRef references the VMClass that defines resource allocation
	ClassRef ObjectRef `json:"classRef"`

	// ImageRef references the VMImage to use as base template
	ImageRef ObjectRef `json:"imageRef"`

	// Networks specifies network attachments for the VM
	// +optional
	Networks []VMNetworkRef `json:"networks,omitempty"`

	// Disks specifies additional disks beyond the root disk
	// +optional
	Disks []DiskSpec `json:"disks,omitempty"`

	// UserData contains cloud-init configuration
	// +optional
	UserData *UserData `json:"userData,omitempty"`

	// Placement provides hints for VM placement
	// +optional
	Placement *Placement `json:"placement,omitempty"`

	// PowerState specifies the desired power state
	// +optional
	// +kubebuilder:default="On"
	// +kubebuilder:validation:Enum=On;Off
	PowerState string `json:"powerState,omitempty"`

	// Tags are applied to the VM for organization
	// +optional
	Tags []string `json:"tags,omitempty"`
}

// VirtualMachineStatus defines the observed state of VirtualMachine.
type VirtualMachineStatus struct {
	// ID is the provider-specific identifier for this VM
	// +optional
	ID string `json:"id,omitempty"`

	// PowerState reflects the current power state
	// +optional
	PowerState string `json:"powerState,omitempty"`

	// IPs contains the IP addresses assigned to the VM
	// +optional
	IPs []string `json:"ips,omitempty"`

	// ConsoleURL provides access to the VM console
	// +optional
	ConsoleURL string `json:"consoleURL,omitempty"`

	// Conditions represent the latest available observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration reflects the generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastTaskRef references the last async operation
	// +optional
	LastTaskRef string `json:"lastTaskRef,omitempty"`

	// Provider contains provider-specific details
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Provider map[string]string `json:"provider,omitempty"`
}

// ObjectRef represents a reference to another object
type ObjectRef struct {
	// Name of the referenced object
	Name string `json:"name"`

	// Namespace of the referenced object (defaults to current namespace)
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// VMNetworkRef references a VMNetworkAttachment with optional IP configuration
type VMNetworkRef struct {
	// Name references a VMNetworkAttachment
	Name string `json:"name"`

	// IPPolicy specifies how IP addresses are assigned
	// +optional
	// +kubebuilder:default="dhcp"
	// +kubebuilder:validation:Enum=dhcp;static
	IPPolicy string `json:"ipPolicy,omitempty"`

	// StaticIP specifies a static IP when IPPolicy is "static"
	// +optional
	StaticIP string `json:"staticIP,omitempty"`
}

// DiskSpec defines additional disk requirements
type DiskSpec struct {
	// SizeGiB specifies disk size in GiB
	SizeGiB int32 `json:"sizeGiB"`

	// Type specifies the disk type (thin, thick, etc.)
	// +optional
	Type string `json:"type,omitempty"`

	// Name provides a name for the disk
	// +optional
	Name string `json:"name,omitempty"`
}

// UserData contains cloud-init configuration
type UserData struct {
	// CloudInit contains cloud-init specific configuration
	// +optional
	CloudInit *CloudInitConfig `json:"cloudInit,omitempty"`
}

// CloudInitConfig specifies cloud-init configuration
type CloudInitConfig struct {
	// SecretRef references a Secret containing cloud-init data
	// +optional
	SecretRef *ObjectRef `json:"secretRef,omitempty"`

	// Inline contains inline cloud-init configuration
	// +optional
	Inline string `json:"inline,omitempty"`
}

// Placement provides placement hints for VM creation
type Placement struct {
	// Datastore specifies the preferred datastore
	// +optional
	Datastore string `json:"datastore,omitempty"`

	// Cluster specifies the preferred cluster
	// +optional
	Cluster string `json:"cluster,omitempty"`

	// Folder specifies the preferred folder
	// +optional
	Folder string `json:"folder,omitempty"`
}

const (
	// VirtualMachineFinalizer is used to ensure proper cleanup
	VirtualMachineFinalizer = "virtualmachine.finalizers.infra.virtrigaud.io"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Provider",type="string",JSONPath=".spec.providerRef.name"
// +kubebuilder:printcolumn:name="Class",type="string",JSONPath=".spec.classRef.name"
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.imageRef.name"
// +kubebuilder:printcolumn:name="Power",type="string",JSONPath=".status.powerState"
// +kubebuilder:printcolumn:name="IPs",type="string",JSONPath=".status.ips"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// VirtualMachine is the Schema for the virtualmachines API.
type VirtualMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VirtualMachineSpec   `json:"spec,omitempty"`
	Status VirtualMachineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VirtualMachineList contains a list of VirtualMachine.
type VirtualMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VirtualMachine{}, &VirtualMachineList{})
}
