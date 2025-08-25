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

package v1beta1

import (
	"k8s.io/apimachinery/pkg/api/resource"
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

	// ResourceConstraints defines resource-based placement constraints
	// +optional
	ResourceConstraints *ResourceConstraints `json:"resourceConstraints,omitempty"`

	// SecurityConstraints defines security-based placement constraints
	// +optional
	SecurityConstraints *SecurityConstraints `json:"securityConstraints,omitempty"`

	// Priority defines the priority of this placement policy
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	Priority *int32 `json:"priority,omitempty"`

	// Weight defines the weight of this placement policy
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Weight *int32 `json:"weight,omitempty"`
}

// PlacementConstraints defines resource placement constraints
type PlacementConstraints struct {
	// Clusters specifies allowed clusters for VM placement
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Clusters []string `json:"clusters,omitempty"`

	// Datastores specifies allowed datastores for VM placement
	// +optional
	// +kubebuilder:validation:MaxItems=100
	Datastores []string `json:"datastores,omitempty"`

	// Hosts specifies allowed hosts for VM placement
	// +optional
	// +kubebuilder:validation:MaxItems=200
	Hosts []string `json:"hosts,omitempty"`

	// Folders specifies allowed folders for VM placement
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Folders []string `json:"folders,omitempty"`

	// ResourcePools specifies allowed resource pools for VM placement
	// +optional
	// +kubebuilder:validation:MaxItems=100
	ResourcePools []string `json:"resourcePools,omitempty"`

	// Networks specifies allowed networks for VM placement
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Networks []string `json:"networks,omitempty"`

	// Zones specifies allowed availability zones
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Zones []string `json:"zones,omitempty"`

	// Regions specifies allowed regions
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Regions []string `json:"regions,omitempty"`

	// NodeSelector specifies node selection criteria for libvirt provider
	// +optional
	// +kubebuilder:validation:MaxProperties=20
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations specifies tolerations for node placement
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Tolerations []VMToleration `json:"tolerations,omitempty"`

	// ExcludedClusters specifies clusters to exclude from placement
	// +optional
	// +kubebuilder:validation:MaxItems=50
	ExcludedClusters []string `json:"excludedClusters,omitempty"`

	// ExcludedHosts specifies hosts to exclude from placement
	// +optional
	// +kubebuilder:validation:MaxItems=200
	ExcludedHosts []string `json:"excludedHosts,omitempty"`

	// ExcludedDatastores specifies datastores to exclude from placement
	// +optional
	// +kubebuilder:validation:MaxItems=100
	ExcludedDatastores []string `json:"excludedDatastores,omitempty"`
}

// AntiAffinityRules defines anti-affinity placement rules
type AntiAffinityRules struct {
	// VMAntiAffinity defines anti-affinity rules between VMs
	// +optional
	VMAntiAffinity *VMAntiAffinity `json:"vmAntiAffinity,omitempty"`

	// HostAntiAffinity prevents VMs from being placed on the same host
	// +optional
	HostAntiAffinity *HostAntiAffinityRule `json:"hostAntiAffinity,omitempty"`

	// ClusterAntiAffinity prevents VMs from being placed in the same cluster
	// +optional
	ClusterAntiAffinity *ClusterAntiAffinityRule `json:"clusterAntiAffinity,omitempty"`

	// DatastoreAntiAffinity prevents VMs from being placed on the same datastore
	// +optional
	DatastoreAntiAffinity *DatastoreAntiAffinityRule `json:"datastoreAntiAffinity,omitempty"`

	// ZoneAntiAffinity prevents VMs from being placed in the same zone
	// +optional
	ZoneAntiAffinity *ZoneAntiAffinityRule `json:"zoneAntiAffinity,omitempty"`

	// ApplicationAntiAffinity prevents VMs from different applications being co-located
	// +optional
	ApplicationAntiAffinity *ApplicationAntiAffinityRule `json:"applicationAntiAffinity,omitempty"`
}

// AffinityRules defines affinity placement rules
type AffinityRules struct {
	// VMAffinity defines affinity rules between VMs
	// +optional
	VMAffinity *VMAffinity `json:"vmAffinity,omitempty"`

	// HostAffinity encourages VMs to be placed on the same host
	// +optional
	HostAffinity *HostAffinityRule `json:"hostAffinity,omitempty"`

	// ClusterAffinity encourages VMs to be placed in the same cluster
	// +optional
	ClusterAffinity *ClusterAffinityRule `json:"clusterAffinity,omitempty"`

	// DatastoreAffinity encourages VMs to be placed on the same datastore
	// +optional
	DatastoreAffinity *DatastoreAffinityRule `json:"datastoreAffinity,omitempty"`

	// ZoneAffinity encourages VMs to be placed in the same zone
	// +optional
	ZoneAffinity *ZoneAffinityRule `json:"zoneAffinity,omitempty"`

	// ApplicationAffinity encourages VMs from the same application to be co-located
	// +optional
	ApplicationAffinity *ApplicationAffinityRule `json:"applicationAffinity,omitempty"`
}

// HostAntiAffinityRule defines host anti-affinity rules
type HostAntiAffinityRule struct {
	// Enabled indicates if host anti-affinity is enabled
	Enabled bool `json:"enabled"`

	// MaxVMsPerHost limits the number of VMs per host
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	MaxVMsPerHost *int32 `json:"maxVMsPerHost,omitempty"`

	// Scope defines the scope of the anti-affinity rule
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// ClusterAntiAffinityRule defines cluster anti-affinity rules
type ClusterAntiAffinityRule struct {
	// Enabled indicates if cluster anti-affinity is enabled
	Enabled bool `json:"enabled"`

	// MaxVMsPerCluster limits the number of VMs per cluster
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	MaxVMsPerCluster *int32 `json:"maxVMsPerCluster,omitempty"`

	// Scope defines the scope of the anti-affinity rule
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// DatastoreAntiAffinityRule defines datastore anti-affinity rules
type DatastoreAntiAffinityRule struct {
	// Enabled indicates if datastore anti-affinity is enabled
	Enabled bool `json:"enabled"`

	// MaxVMsPerDatastore limits the number of VMs per datastore
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	MaxVMsPerDatastore *int32 `json:"maxVMsPerDatastore,omitempty"`

	// Scope defines the scope of the anti-affinity rule
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// ZoneAntiAffinityRule defines zone anti-affinity rules
type ZoneAntiAffinityRule struct {
	// Enabled indicates if zone anti-affinity is enabled
	Enabled bool `json:"enabled"`

	// MaxVMsPerZone limits the number of VMs per zone
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	MaxVMsPerZone *int32 `json:"maxVMsPerZone,omitempty"`

	// Scope defines the scope of the anti-affinity rule
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// ApplicationAntiAffinityRule defines application anti-affinity rules
type ApplicationAntiAffinityRule struct {
	// Enabled indicates if application anti-affinity is enabled
	Enabled bool `json:"enabled"`

	// ApplicationLabel specifies the label key used to identify applications
	// +optional
	// +kubebuilder:default="app"
	ApplicationLabel string `json:"applicationLabel,omitempty"`

	// Scope defines the scope of the anti-affinity rule
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// HostAffinityRule defines host affinity rules
type HostAffinityRule struct {
	// Enabled indicates if host affinity is enabled
	Enabled bool `json:"enabled"`

	// PreferredHosts lists preferred hosts
	// +optional
	// +kubebuilder:validation:MaxItems=50
	PreferredHosts []string `json:"preferredHosts,omitempty"`

	// Scope defines the scope of the affinity rule
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// ClusterAffinityRule defines cluster affinity rules
type ClusterAffinityRule struct {
	// Enabled indicates if cluster affinity is enabled
	Enabled bool `json:"enabled"`

	// PreferredClusters lists preferred clusters
	// +optional
	// +kubebuilder:validation:MaxItems=20
	PreferredClusters []string `json:"preferredClusters,omitempty"`

	// Scope defines the scope of the affinity rule
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// DatastoreAffinityRule defines datastore affinity rules
type DatastoreAffinityRule struct {
	// Enabled indicates if datastore affinity is enabled
	Enabled bool `json:"enabled"`

	// PreferredDatastores lists preferred datastores
	// +optional
	// +kubebuilder:validation:MaxItems=50
	PreferredDatastores []string `json:"preferredDatastores,omitempty"`

	// Scope defines the scope of the affinity rule
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// ZoneAffinityRule defines zone affinity rules
type ZoneAffinityRule struct {
	// Enabled indicates if zone affinity is enabled
	Enabled bool `json:"enabled"`

	// PreferredZones lists preferred zones
	// +optional
	// +kubebuilder:validation:MaxItems=10
	PreferredZones []string `json:"preferredZones,omitempty"`

	// Scope defines the scope of the affinity rule
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// ApplicationAffinityRule defines application affinity rules
type ApplicationAffinityRule struct {
	// Enabled indicates if application affinity is enabled
	Enabled bool `json:"enabled"`

	// ApplicationLabel specifies the label key used to identify applications
	// +optional
	// +kubebuilder:default="app"
	ApplicationLabel string `json:"applicationLabel,omitempty"`

	// Scope defines the scope of the affinity rule
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// VMAntiAffinity defines anti-affinity rules between VMs
type VMAntiAffinity struct {
	// RequiredDuringScheduling specifies hard anti-affinity rules
	// +optional
	// +kubebuilder:validation:MaxItems=20
	RequiredDuringScheduling []VMAffinityTerm `json:"requiredDuringScheduling,omitempty"`

	// PreferredDuringScheduling specifies soft anti-affinity rules
	// +optional
	// +kubebuilder:validation:MaxItems=20
	PreferredDuringScheduling []WeightedVMAffinityTerm `json:"preferredDuringScheduling,omitempty"`
}

// VMAffinity defines affinity rules between VMs
type VMAffinity struct {
	// RequiredDuringScheduling specifies hard affinity rules
	// +optional
	// +kubebuilder:validation:MaxItems=20
	RequiredDuringScheduling []VMAffinityTerm `json:"requiredDuringScheduling,omitempty"`

	// PreferredDuringScheduling specifies soft affinity rules
	// +optional
	// +kubebuilder:validation:MaxItems=20
	PreferredDuringScheduling []WeightedVMAffinityTerm `json:"preferredDuringScheduling,omitempty"`
}

// VMAffinityTerm defines a VM affinity term
type VMAffinityTerm struct {
	// LabelSelector selects VMs for affinity rules
	// +optional
	LabelSelector *metav1.LabelSelector `json:"labelSelector,omitempty"`

	// Namespaces specifies which namespaces to consider
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Namespaces []string `json:"namespaces,omitempty"`

	// NamespaceSelector selects namespaces using label selectors
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// TopologyKey specifies the topology domain for the rule
	// +kubebuilder:validation:MaxLength=253
	TopologyKey string `json:"topologyKey"`

	// MatchExpressions is a list of VM selector requirements
	// +optional
	// +kubebuilder:validation:MaxItems=20
	MatchExpressions []VMSelectorRequirement `json:"matchExpressions,omitempty"`
}

// WeightedVMAffinityTerm defines a weighted VM affinity term
type WeightedVMAffinityTerm struct {
	// Weight associated with matching the corresponding VMAffinityTerm
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Weight int32 `json:"weight"`

	// VMAffinityTerm defines the VM affinity term
	VMAffinityTerm VMAffinityTerm `json:"vmAffinityTerm"`
}

// VMSelectorRequirement defines a VM selector requirement
type VMSelectorRequirement struct {
	// Key is the label key that the selector applies to
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`

	// Operator represents a key's relationship to a set of values
	// +kubebuilder:validation:Enum=In;NotIn;Exists;DoesNotExist
	Operator VMSelectorOperator `json:"operator"`

	// Values is an array of string values
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Values []string `json:"values,omitempty"`
}

// VMSelectorOperator represents a selector operator
// +kubebuilder:validation:Enum=In;NotIn;Exists;DoesNotExist
type VMSelectorOperator string

const (
	// VMSelectorOpIn means the key must be in the set of values
	VMSelectorOpIn VMSelectorOperator = "In"
	// VMSelectorOpNotIn means the key must not be in the set of values
	VMSelectorOpNotIn VMSelectorOperator = "NotIn"
	// VMSelectorOpExists means the key must exist
	VMSelectorOpExists VMSelectorOperator = "Exists"
	// VMSelectorOpDoesNotExist means the key must not exist
	VMSelectorOpDoesNotExist VMSelectorOperator = "DoesNotExist"
)

// VMToleration represents a toleration for VM placement
type VMToleration struct {
	// Key is the taint key that the toleration applies to
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key,omitempty"`

	// Operator represents the relationship between the key and value
	// +optional
	// +kubebuilder:default="Equal"
	// +kubebuilder:validation:Enum=Exists;Equal
	Operator VMTolerationOperator `json:"operator,omitempty"`

	// Value is the taint value the toleration matches to
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Value string `json:"value,omitempty"`

	// Effect indicates the taint effect to match
	// +optional
	Effect VMTaintEffect `json:"effect,omitempty"`

	// TolerationSeconds represents the period of time the toleration tolerates the taint
	// +optional
	// +kubebuilder:validation:Minimum=0
	TolerationSeconds *int64 `json:"tolerationSeconds,omitempty"`
}

// VMTolerationOperator represents the operator for toleration
// +kubebuilder:validation:Enum=Exists;Equal
type VMTolerationOperator string

const (
	// VMTolerationOpExists means the toleration exists
	VMTolerationOpExists VMTolerationOperator = "Exists"
	// VMTolerationOpEqual means the toleration equals the value
	VMTolerationOpEqual VMTolerationOperator = "Equal"
)

// VMTaintEffect represents the effect of a taint
// +kubebuilder:validation:Enum=NoSchedule;PreferNoSchedule;NoExecute
type VMTaintEffect string

const (
	// VMTaintEffectNoSchedule means no new VMs will be scheduled
	VMTaintEffectNoSchedule VMTaintEffect = "NoSchedule"
	// VMTaintEffectPreferNoSchedule means avoid scheduling if possible
	VMTaintEffectPreferNoSchedule VMTaintEffect = "PreferNoSchedule"
	// VMTaintEffectNoExecute means existing VMs will be evicted
	VMTaintEffectNoExecute VMTaintEffect = "NoExecute"
)

// ResourceConstraints defines resource-based placement constraints
type ResourceConstraints struct {
	// MinCPUPerHost specifies minimum CPU available per host
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinCPUPerHost *int32 `json:"minCPUPerHost,omitempty"`

	// MinMemoryPerHost specifies minimum memory available per host
	// +optional
	MinMemoryPerHost *resource.Quantity `json:"minMemoryPerHost,omitempty"`

	// MinDiskSpacePerHost specifies minimum disk space available per host
	// +optional
	MinDiskSpacePerHost *resource.Quantity `json:"minDiskSpacePerHost,omitempty"`

	// MaxCPUUtilization specifies maximum allowed CPU utilization
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MaxCPUUtilization *int32 `json:"maxCPUUtilization,omitempty"`

	// MaxMemoryUtilization specifies maximum allowed memory utilization
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MaxMemoryUtilization *int32 `json:"maxMemoryUtilization,omitempty"`

	// MaxDiskUtilization specifies maximum allowed disk utilization
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MaxDiskUtilization *int32 `json:"maxDiskUtilization,omitempty"`

	// RequiredFeatures lists required hardware features
	// +optional
	// +kubebuilder:validation:MaxItems=20
	RequiredFeatures []string `json:"requiredFeatures,omitempty"`

	// PreferredFeatures lists preferred hardware features
	// +optional
	// +kubebuilder:validation:MaxItems=20
	PreferredFeatures []string `json:"preferredFeatures,omitempty"`
}

// SecurityConstraints defines security-based placement constraints
type SecurityConstraints struct {
	// RequireSecureBoot requires hosts that support secure boot
	// +optional
	RequireSecureBoot bool `json:"requireSecureBoot,omitempty"`

	// RequireTPM requires hosts that support TPM
	// +optional
	RequireTPM bool `json:"requireTPM,omitempty"`

	// RequireEncryptedStorage requires hosts that support encrypted storage
	// +optional
	RequireEncryptedStorage bool `json:"requireEncryptedStorage,omitempty"`

	// RequireNUMATopology requires hosts that support NUMA topology
	// +optional
	RequireNUMATopology bool `json:"requireNUMATopology,omitempty"`

	// AllowedSecurityGroups lists allowed security groups
	// +optional
	// +kubebuilder:validation:MaxItems=20
	AllowedSecurityGroups []string `json:"allowedSecurityGroups,omitempty"`

	// DeniedSecurityGroups lists denied security groups
	// +optional
	// +kubebuilder:validation:MaxItems=20
	DeniedSecurityGroups []string `json:"deniedSecurityGroups,omitempty"`

	// IsolationLevel specifies the required isolation level
	// +optional
	// +kubebuilder:validation:Enum=none;basic;strict;maximum
	IsolationLevel string `json:"isolationLevel,omitempty"`

	// TrustLevel specifies the required trust level
	// +optional
	// +kubebuilder:validation:Enum=untrusted;basic;trusted;highly-trusted
	TrustLevel string `json:"trustLevel,omitempty"`
}

// VMPlacementPolicyStatus defines the observed state of VMPlacementPolicy
type VMPlacementPolicyStatus struct {
	// ObservedGeneration is the most recent generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// UsedByVMs lists VMs currently using this policy
	// +optional
	// +kubebuilder:validation:MaxItems=1000
	UsedByVMs []LocalObjectReference `json:"usedByVMs,omitempty"`

	// Conditions represent the current service state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ValidationResults contains validation results for different providers
	// +optional
	ValidationResults map[string]PolicyValidationResult `json:"validationResults,omitempty"`

	// PlacementStats provides statistics about VM placements using this policy
	// +optional
	PlacementStats *PlacementStatistics `json:"placementStats,omitempty"`

	// ConflictingPolicies lists policies that conflict with this policy
	// +optional
	// +kubebuilder:validation:MaxItems=50
	ConflictingPolicies []PolicyConflict `json:"conflictingPolicies,omitempty"`
}

// PolicyValidationResult represents a validation result for a provider
type PolicyValidationResult struct {
	// Valid indicates if the policy is valid for the provider
	Valid bool `json:"valid"`

	// Message provides details about the validation result
	// +optional
	Message string `json:"message,omitempty"`

	// Warnings lists any validation warnings
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Warnings []string `json:"warnings,omitempty"`

	// Errors lists any validation errors
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Errors []string `json:"errors,omitempty"`

	// SupportedFeatures lists features supported by the provider
	// +optional
	// +kubebuilder:validation:MaxItems=50
	SupportedFeatures []string `json:"supportedFeatures,omitempty"`

	// UnsupportedFeatures lists features not supported by the provider
	// +optional
	// +kubebuilder:validation:MaxItems=50
	UnsupportedFeatures []string `json:"unsupportedFeatures,omitempty"`

	// LastValidated is when this validation was last performed
	// +optional
	LastValidated *metav1.Time `json:"lastValidated,omitempty"`
}

// PlacementStatistics provides statistics about VM placements
type PlacementStatistics struct {
	// TotalPlacements is the total number of VM placements using this policy
	// +optional
	TotalPlacements int32 `json:"totalPlacements,omitempty"`

	// SuccessfulPlacements is the number of successful placements
	// +optional
	SuccessfulPlacements int32 `json:"successfulPlacements,omitempty"`

	// FailedPlacements is the number of failed placements
	// +optional
	FailedPlacements int32 `json:"failedPlacements,omitempty"`

	// AveragePlacementTime is the average time for VM placement
	// +optional
	AveragePlacementTime *metav1.Duration `json:"averagePlacementTime,omitempty"`

	// ConstraintViolations is the number of constraint violations
	// +optional
	ConstraintViolations int32 `json:"constraintViolations,omitempty"`

	// LastPlacementTime is when the last VM was placed using this policy
	// +optional
	LastPlacementTime *metav1.Time `json:"lastPlacementTime,omitempty"`

	// PlacementDistribution shows how VMs are distributed across hosts/clusters
	// +optional
	PlacementDistribution map[string]int32 `json:"placementDistribution,omitempty"`
}

// PolicyConflict represents a conflict between policies
type PolicyConflict struct {
	// PolicyName is the name of the conflicting policy
	PolicyName string `json:"policyName"`

	// ConflictType describes the type of conflict
	// +kubebuilder:validation:Enum=hard;soft;resource;security;affinity
	ConflictType string `json:"conflictType"`

	// Description provides details about the conflict
	// +optional
	Description string `json:"description,omitempty"`

	// Severity indicates the severity of the conflict
	// +optional
	// +kubebuilder:validation:Enum=low;medium;high;critical
	Severity string `json:"severity,omitempty"`

	// ResolutionSuggestion provides suggestions for resolving the conflict
	// +optional
	ResolutionSuggestion string `json:"resolutionSuggestion,omitempty"`
}

// VMPlacementPolicy condition types
const (
	// VMPlacementPolicyConditionReady indicates whether the policy is ready
	VMPlacementPolicyConditionReady = "Ready"
	// VMPlacementPolicyConditionValidated indicates whether the policy is validated
	VMPlacementPolicyConditionValidated = "Validated"
	// VMPlacementPolicyConditionConflicts indicates whether the policy has conflicts
	VMPlacementPolicyConditionConflicts = "Conflicts"
	// VMPlacementPolicyConditionSupported indicates whether the policy is supported
	VMPlacementPolicyConditionSupported = "Supported"
)

// VMPlacementPolicy condition reasons
const (
	// VMPlacementPolicyReasonValid indicates the policy is valid
	VMPlacementPolicyReasonValid = "Valid"
	// VMPlacementPolicyReasonInvalid indicates the policy is invalid
	VMPlacementPolicyReasonInvalid = "Invalid"
	// VMPlacementPolicyReasonUnsupported indicates some rules are unsupported
	VMPlacementPolicyReasonUnsupported = "Unsupported"
	// VMPlacementPolicyReasonConflicts indicates the policy has conflicts
	VMPlacementPolicyReasonConflicts = "Conflicts"
	// VMPlacementPolicyReasonNoConflicts indicates the policy has no conflicts
	VMPlacementPolicyReasonNoConflicts = "NoConflicts"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Used By VMs",type=integer,JSONPath=`.status.usedByVMs[*]`,description="Number of VMs using this policy"
//+kubebuilder:printcolumn:name="Priority",type=integer,JSONPath=`.spec.priority`
//+kubebuilder:printcolumn:name="Successful",type=integer,JSONPath=`.status.placementStats.successfulPlacements`
//+kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.placementStats.failedPlacements`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
//+kubebuilder:resource:shortName=vmpp

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

// Hub marks this version as the conversion hub
func (*VMPlacementPolicy) Hub() {}

func init() {
	SchemeBuilder.Register(&VMPlacementPolicy{}, &VMPlacementPolicyList{})
}
