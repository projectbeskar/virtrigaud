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

// VMCloneSpec defines the desired state of VMClone
type VMCloneSpec struct {
	// SourceRef references the source virtual machine to clone
	SourceRef LocalObjectReference `json:"sourceRef"`

	// Target defines the target VM configuration
	Target VMCloneTarget `json:"target"`

	// Linked indicates whether to create a linked clone (best effort)
	// +optional
	Linked bool `json:"linked,omitempty"`

	// PowerOn indicates whether to power on the cloned VM
	// +optional
	PowerOn bool `json:"powerOn,omitempty"`

	// Customization defines VM customization options
	// +optional
	Customization *VMCustomization `json:"customization,omitempty"`
}

// VMCloneTarget defines the target VM configuration
type VMCloneTarget struct {
	// Name is the name of the target VM
	Name string `json:"name"`

	// Namespace is the namespace for the target VM (defaults to source namespace)
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// ClassRef references the VM class for resource allocation
	// +optional
	ClassRef *LocalObjectReference `json:"classRef,omitempty"`

	// ImageRef references the VM image (if different from source)
	// +optional
	ImageRef *LocalObjectReference `json:"imageRef,omitempty"`

	// PlacementRef references placement policy for the target VM
	// +optional
	PlacementRef *LocalObjectReference `json:"placementRef,omitempty"`

	// Networks defines network configuration overrides
	// +optional
	Networks []VMNetworkAttachment `json:"networks,omitempty"`

	// Disks defines disk configuration overrides
	// +optional
	Disks []DiskSpec `json:"disks,omitempty"`
}

// VMCustomization defines VM customization options
type VMCustomization struct {
	// Hostname sets the target VM hostname
	// +optional
	Hostname string `json:"hostname,omitempty"`

	// Networks defines network customization
	// +optional
	Networks []NetworkCustomization `json:"networks,omitempty"`

	// UserData provides cloud-init or similar customization data
	// +optional
	UserData *UserData `json:"userData,omitempty"`

	// Tags defines additional tags for the cloned VM
	// +optional
	Tags []string `json:"tags,omitempty"`
}

// NetworkCustomization defines network-specific customization
type NetworkCustomization struct {
	// Name identifies the network to customize
	Name string `json:"name"`

	// IPAddress sets a static IP address
	// +optional
	IPAddress string `json:"ipAddress,omitempty"`

	// Gateway sets the network gateway
	// +optional
	Gateway string `json:"gateway,omitempty"`

	// DNS sets DNS servers
	// +optional
	DNS []string `json:"dns,omitempty"`
}

// VMCloneStatus defines the observed state of VMClone
type VMCloneStatus struct {
	// TargetRef references the created target VM
	// +optional
	TargetRef *LocalObjectReference `json:"targetRef,omitempty"`

	// Phase represents the current phase of the clone operation
	// +optional
	Phase ClonePhase `json:"phase,omitempty"`

	// Message provides additional details about the current state
	// +optional
	Message string `json:"message,omitempty"`

	// TaskRef tracks any ongoing async operations
	// +optional
	TaskRef string `json:"taskRef,omitempty"`

	// StartTime is when the clone operation started
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the clone operation completed
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// LinkedClone indicates whether a linked clone was actually created
	// +optional
	LinkedClone *bool `json:"linkedClone,omitempty"`

	// Conditions represent the current service state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClonePhase represents the phase of a clone operation
// +kubebuilder:validation:Enum=Pending;Cloning;Customizing;Ready;Failed
type ClonePhase string

const (
	// ClonePhasePending indicates the clone is waiting to be processed
	ClonePhasePending ClonePhase = "Pending"
	// ClonePhaseCloning indicates the clone operation is in progress
	ClonePhaseCloning ClonePhase = "Cloning"
	// ClonePhaseCustomizing indicates the clone is being customized
	ClonePhaseCustomizing ClonePhase = "Customizing"
	// ClonePhaseReady indicates the clone is ready for use
	ClonePhaseReady ClonePhase = "Ready"
	// ClonePhaseFailed indicates the clone operation failed
	ClonePhaseFailed ClonePhase = "Failed"
)

// VMClone condition types
const (
	// VMCloneConditionReady indicates whether the clone is ready
	VMCloneConditionReady = "Ready"
	// VMCloneConditionCloning indicates whether the clone is in progress
	VMCloneConditionCloning = "Cloning"
	// VMCloneConditionCustomizing indicates whether customization is in progress
	VMCloneConditionCustomizing = "Customizing"
)

// VMClone condition reasons
const (
	// VMCloneReasonCompleted indicates the clone was successfully completed
	VMCloneReasonCompleted = "Completed"
	// VMCloneReasonCloning indicates the clone is in progress
	VMCloneReasonCloning = "Cloning"
	// VMCloneReasonCustomizing indicates customization is in progress
	VMCloneReasonCustomizing = "Customizing"
	// VMCloneReasonProviderError indicates a provider error occurred
	VMCloneReasonProviderError = "ProviderError"
	// VMCloneReasonSourceNotFound indicates the source VM was not found
	VMCloneReasonSourceNotFound = "SourceNotFound"
	// VMCloneReasonUnsupported indicates cloning is not supported
	VMCloneReasonUnsupported = "Unsupported"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceRef.name`
//+kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.name`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Linked",type=boolean,JSONPath=`.status.linkedClone`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VMClone is the Schema for the vmclones API
type VMClone struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VMCloneSpec   `json:"spec,omitempty"`
	Status VMCloneStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// VMCloneList contains a list of VMClone
type VMCloneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VMClone `json:"items"`
}
