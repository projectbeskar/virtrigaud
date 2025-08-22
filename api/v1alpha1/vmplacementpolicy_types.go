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

// VMPlacementPolicySpec defines the desired state of VMPlacementPolicy
type VMPlacementPolicySpec struct {
	// Hard constraints that must be satisfied for VM placement
	// +optional
	Hard *PlacementConstraints `json:"hard,omitempty"`

	// Soft constraints that should be satisfied if possible
	// +optional
	Soft *PlacementConstraints `json:"soft,omitempty"`

	// AntiAffinity defines anti-affinity rules for VMs
	// +optional
	AntiAffinity *AntiAffinityRules `json:"antiAffinity,omitempty"`

	// Affinity defines affinity rules for VMs
	// +optional
	Affinity *AffinityRules `json:"affinity,omitempty"`
}

// PlacementConstraints defines resource placement constraints
type PlacementConstraints struct {
	// Clusters specifies allowed clusters for VM placement
	// +optional
	Clusters []string `json:"clusters,omitempty"`

	// Datastores specifies allowed datastores for VM placement
	// +optional
	Datastores []string `json:"datastores,omitempty"`

	// Hosts specifies allowed hosts for VM placement
	// +optional
	Hosts []string `json:"hosts,omitempty"`

	// Folders specifies allowed folders for VM placement
	// +optional
	Folders []string `json:"folders,omitempty"`

	// ResourcePools specifies allowed resource pools for VM placement
	// +optional
	ResourcePools []string `json:"resourcePools,omitempty"`

	// Networks specifies allowed networks for VM placement
	// +optional
	Networks []string `json:"networks,omitempty"`

	// Zones specifies allowed availability zones
	// +optional
	Zones []string `json:"zones,omitempty"`

	// NodeSelector specifies node selection criteria for libvirt provider
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations specifies tolerations for node placement
	// +optional
	Tolerations []Toleration `json:"tolerations,omitempty"`
}

// AntiAffinityRules defines anti-affinity placement rules
type AntiAffinityRules struct {
	// VMAntiAffinity defines anti-affinity rules between VMs
	// +optional
	VMAntiAffinity *VMAntiAffinity `json:"vmAntiAffinity,omitempty"`

	// HostAntiAffinity prevents VMs from being placed on the same host
	// +optional
	HostAntiAffinity bool `json:"hostAntiAffinity,omitempty"`

	// ClusterAntiAffinity prevents VMs from being placed in the same cluster
	// +optional
	ClusterAntiAffinity bool `json:"clusterAntiAffinity,omitempty"`

	// DatastoreAntiAffinity prevents VMs from being placed on the same datastore
	// +optional
	DatastoreAntiAffinity bool `json:"datastoreAntiAffinity,omitempty"`
}

// AffinityRules defines affinity placement rules
type AffinityRules struct {
	// VMAffinity defines affinity rules between VMs
	// +optional
	VMAffinity *VMAffinity `json:"vmAffinity,omitempty"`

	// HostAffinity encourages VMs to be placed on the same host
	// +optional
	HostAffinity bool `json:"hostAffinity,omitempty"`

	// ClusterAffinity encourages VMs to be placed in the same cluster
	// +optional
	ClusterAffinity bool `json:"clusterAffinity,omitempty"`

	// DatastoreAffinity encourages VMs to be placed on the same datastore
	// +optional
	DatastoreAffinity bool `json:"datastoreAffinity,omitempty"`
}

// VMAntiAffinity defines anti-affinity rules between VMs
type VMAntiAffinity struct {
	// RequiredDuringScheduling specifies hard anti-affinity rules
	// +optional
	RequiredDuringScheduling []VMAffinityTerm `json:"requiredDuringScheduling,omitempty"`

	// PreferredDuringScheduling specifies soft anti-affinity rules
	// +optional
	PreferredDuringScheduling []WeightedVMAffinityTerm `json:"preferredDuringScheduling,omitempty"`
}

// VMAffinity defines affinity rules between VMs
type VMAffinity struct {
	// RequiredDuringScheduling specifies hard affinity rules
	// +optional
	RequiredDuringScheduling []VMAffinityTerm `json:"requiredDuringScheduling,omitempty"`

	// PreferredDuringScheduling specifies soft affinity rules
	// +optional
	PreferredDuringScheduling []WeightedVMAffinityTerm `json:"preferredDuringScheduling,omitempty"`
}

// VMAffinityTerm defines a VM affinity term
type VMAffinityTerm struct {
	// LabelSelector selects VMs for affinity rules
	// +optional
	LabelSelector *metav1.LabelSelector `json:"labelSelector,omitempty"`

	// Namespaces specifies which namespaces to consider
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`

	// TopologyKey specifies the topology domain for the rule
	TopologyKey string `json:"topologyKey"`
}

// WeightedVMAffinityTerm defines a weighted VM affinity term
type WeightedVMAffinityTerm struct {
	// Weight associated with matching the corresponding VMAffinityTerm
	Weight int32 `json:"weight"`

	// VMAffinityTerm defines the VM affinity term
	VMAffinityTerm VMAffinityTerm `json:"vmAffinityTerm"`
}

// Toleration represents a toleration for node taints
type Toleration struct {
	// Key is the taint key that the toleration applies to
	// +optional
	Key string `json:"key,omitempty"`

	// Operator represents the relationship between the key and value
	// +optional
	Operator TolerationOperator `json:"operator,omitempty"`

	// Value is the taint value the toleration matches to
	// +optional
	Value string `json:"value,omitempty"`

	// Effect indicates the taint effect to match
	// +optional
	Effect TaintEffect `json:"effect,omitempty"`

	// TolerationSeconds represents the period of time the toleration tolerates the taint
	// +optional
	TolerationSeconds *int64 `json:"tolerationSeconds,omitempty"`
}

// TolerationOperator represents the operator for toleration
// +kubebuilder:validation:Enum=Exists;Equal
type TolerationOperator string

const (
	// TolerationOpExists means the toleration exists
	TolerationOpExists TolerationOperator = "Exists"
	// TolerationOpEqual means the toleration equals the value
	TolerationOpEqual TolerationOperator = "Equal"
)

// TaintEffect represents the effect of a taint
// +kubebuilder:validation:Enum=NoSchedule;PreferNoSchedule;NoExecute
type TaintEffect string

const (
	// TaintEffectNoSchedule means no new VMs will be scheduled
	TaintEffectNoSchedule TaintEffect = "NoSchedule"
	// TaintEffectPreferNoSchedule means avoid scheduling if possible
	TaintEffectPreferNoSchedule TaintEffect = "PreferNoSchedule"
	// TaintEffectNoExecute means existing VMs will be evicted
	TaintEffectNoExecute TaintEffect = "NoExecute"
)

// VMPlacementPolicyStatus defines the observed state of VMPlacementPolicy
type VMPlacementPolicyStatus struct {
	// ObservedGeneration is the most recent generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// UsedByVMs lists VMs currently using this policy
	// +optional
	UsedByVMs []LocalObjectReference `json:"usedByVMs,omitempty"`

	// Conditions represent the current service state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VMPlacementPolicy condition types
const (
	// VMPlacementPolicyConditionReady indicates whether the policy is ready
	VMPlacementPolicyConditionReady = "Ready"
	// VMPlacementPolicyConditionValidated indicates whether the policy is validated
	VMPlacementPolicyConditionValidated = "Validated"
)

// VMPlacementPolicy condition reasons
const (
	// VMPlacementPolicyReasonValid indicates the policy is valid
	VMPlacementPolicyReasonValid = "Valid"
	// VMPlacementPolicyReasonInvalid indicates the policy is invalid
	VMPlacementPolicyReasonInvalid = "Invalid"
	// VMPlacementPolicyReasonUnsupported indicates some rules are unsupported
	VMPlacementPolicyReasonUnsupported = "Unsupported"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Used By VMs",type=integer,JSONPath=`.status.usedByVMs[*]`,description="Number of VMs using this policy"
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VMPlacementPolicy is the Schema for the vmplacementpolicies API
type VMPlacementPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VMPlacementPolicySpec   `json:"spec,omitempty"`
	Status VMPlacementPolicyStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// VMPlacementPolicyList contains a list of VMPlacementPolicy
type VMPlacementPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VMPlacementPolicy `json:"items"`
}
