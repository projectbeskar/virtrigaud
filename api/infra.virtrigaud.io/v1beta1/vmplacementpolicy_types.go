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

// VMPlacementPolicySpec defines the desired state of VMPlacementPolicy. Only
// the scheduler of a clustered Provider reads it (see VMPlacementPolicy).
// securityConstraints, priority and weight are accepted but not acted on in
// this release.
type VMPlacementPolicySpec struct {
	// Hard constraints that a Host must satisfy to be a candidate for the VM.
	// The scheduler acts on hosts, excludedHosts and nodeSelector; the other
	// fields are accepted but not acted on in this release.
	// +optional
	Hard *PlacementConstraints `json:"hard,omitempty"`

	// Soft constraints that rank the candidate Hosts without removing any. The
	// scheduler acts on hosts, excludedHosts and nodeSelector; the other fields
	// are accepted but not acted on in this release.
	// +optional
	Soft *PlacementConstraints `json:"soft,omitempty"`

	// AntiAffinity defines anti-affinity rules. The scheduler acts on
	// hostAntiAffinity and vmAntiAffinity; the cluster, datastore, zone and
	// application rules are accepted but not acted on in this release.
	// +optional
	AntiAffinity *AntiAffinityRules `json:"antiAffinity,omitempty"`

	// Affinity defines affinity rules. The scheduler acts on hostAffinity and
	// vmAffinity; the cluster, datastore, zone and application rules are
	// accepted but not acted on in this release.
	// +optional
	Affinity *AffinityRules `json:"affinity,omitempty"`

	// ResourceConstraints defines placement constraints on the resources and CPU
	// features of a Host. The scheduler acts on minCPUPerHost, minMemoryPerHost,
	// minDiskSpacePerHost, requiredFeatures and preferredFeatures; the
	// utilization limits are accepted but not acted on in this release.
	// +optional
	ResourceConstraints *ResourceConstraints `json:"resourceConstraints,omitempty"`

	// SecurityConstraints defines security-based placement constraints. Accepted
	// but not acted on in this release. This applies to every field below it: no
	// Host status field carries this information. Label the Hosts and use
	// nodeSelector instead.
	// +optional
	SecurityConstraints *SecurityConstraints `json:"securityConstraints,omitempty"`

	// Priority defines the priority of this placement policy. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	Priority *int32 `json:"priority,omitempty"`

	// Weight defines the weight of this placement policy. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Weight *int32 `json:"weight,omitempty"`
}

// PlacementConstraints defines placement constraints on Hosts. The scheduler
// acts on hosts, excludedHosts and nodeSelector, in hard or soft form; the
// other fields are accepted but not acted on in this release.
type PlacementConstraints struct {
	// Clusters specifies allowed clusters for VM placement. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Clusters []string `json:"clusters,omitempty"`

	// Datastores specifies allowed datastores for VM placement. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=100
	Datastores []string `json:"datastores,omitempty"`

	// Hosts lists Hosts by resource name. In hard, only the listed Hosts are
	// candidates. In soft, a listed Host ranks higher.
	// +optional
	// +kubebuilder:validation:MaxItems=200
	Hosts []string `json:"hosts,omitempty"`

	// Folders specifies allowed folders for VM placement. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Folders []string `json:"folders,omitempty"`

	// ResourcePools specifies allowed resource pools for VM placement. Accepted
	// but not acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=100
	ResourcePools []string `json:"resourcePools,omitempty"`

	// Networks specifies allowed networks for VM placement. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Networks []string `json:"networks,omitempty"`

	// Zones specifies allowed availability zones. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Zones []string `json:"zones,omitempty"`

	// Regions specifies allowed regions. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Regions []string `json:"regions,omitempty"`

	// NodeSelector is a set of label key/value pairs matched against the labels
	// in Host.spec.labels. In hard, a Host must carry every pair. In soft, a
	// Host ranks higher for each pair it carries.
	// +optional
	// +kubebuilder:validation:MaxProperties=20
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations specifies tolerations for node placement. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Tolerations []VMToleration `json:"tolerations,omitempty"`

	// ExcludedClusters specifies clusters to exclude from placement. Accepted
	// but not acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	ExcludedClusters []string `json:"excludedClusters,omitempty"`

	// ExcludedHosts lists Hosts by resource name. In hard, a listed Host is not
	// a candidate. In soft, a listed Host ranks lower.
	// +optional
	// +kubebuilder:validation:MaxItems=200
	ExcludedHosts []string `json:"excludedHosts,omitempty"`

	// ExcludedDatastores specifies datastores to exclude from placement.
	// Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=100
	ExcludedDatastores []string `json:"excludedDatastores,omitempty"`
}

// AntiAffinityRules defines anti-affinity placement rules. The scheduler acts
// on hostAntiAffinity and vmAntiAffinity; the other rules are accepted but
// not acted on in this release.
type AntiAffinityRules struct {
	// VMAntiAffinity defines anti-affinity rules between VMs.
	// +optional
	VMAntiAffinity *VMAntiAffinity `json:"vmAntiAffinity,omitempty"`

	// HostAntiAffinity limits the number of VMs on one Host.
	// +optional
	HostAntiAffinity *HostAntiAffinityRule `json:"hostAntiAffinity,omitempty"`

	// ClusterAntiAffinity prevents VMs from being placed in the same cluster.
	// Accepted but not acted on in this release.
	// +optional
	ClusterAntiAffinity *ClusterAntiAffinityRule `json:"clusterAntiAffinity,omitempty"`

	// DatastoreAntiAffinity prevents VMs from being placed on the same
	// datastore. Accepted but not acted on in this release.
	// +optional
	DatastoreAntiAffinity *DatastoreAntiAffinityRule `json:"datastoreAntiAffinity,omitempty"`

	// ZoneAntiAffinity prevents VMs from being placed in the same zone. Accepted
	// but not acted on in this release.
	// +optional
	ZoneAntiAffinity *ZoneAntiAffinityRule `json:"zoneAntiAffinity,omitempty"`

	// ApplicationAntiAffinity prevents VMs from different applications being
	// co-located. Accepted but not acted on in this release.
	// +optional
	ApplicationAntiAffinity *ApplicationAntiAffinityRule `json:"applicationAntiAffinity,omitempty"`
}

// AffinityRules defines affinity placement rules. The scheduler acts on
// hostAffinity and vmAffinity; the other rules are accepted but not acted on
// in this release.
type AffinityRules struct {
	// VMAffinity defines affinity rules between VMs.
	// +optional
	VMAffinity *VMAffinity `json:"vmAffinity,omitempty"`

	// HostAffinity prefers, or requires, the listed Hosts.
	// +optional
	HostAffinity *HostAffinityRule `json:"hostAffinity,omitempty"`

	// ClusterAffinity encourages VMs to be placed in the same cluster. Accepted
	// but not acted on in this release.
	// +optional
	ClusterAffinity *ClusterAffinityRule `json:"clusterAffinity,omitempty"`

	// DatastoreAffinity encourages VMs to be placed on the same datastore.
	// Accepted but not acted on in this release.
	// +optional
	DatastoreAffinity *DatastoreAffinityRule `json:"datastoreAffinity,omitempty"`

	// ZoneAffinity encourages VMs to be placed in the same zone. Accepted but
	// not acted on in this release.
	// +optional
	ZoneAffinity *ZoneAffinityRule `json:"zoneAffinity,omitempty"`

	// ApplicationAffinity encourages VMs from the same application to be
	// co-located. Accepted but not acted on in this release.
	// +optional
	ApplicationAffinity *ApplicationAffinityRule `json:"applicationAffinity,omitempty"`
}

// HostAntiAffinityRule limits the number of VMs on one Host. Only VMs in the
// namespace of the VM being placed are counted.
type HostAntiAffinityRule struct {
	// Enabled turns the rule on. A rule that is not enabled is ignored.
	Enabled bool `json:"enabled"`

	// MaxVMsPerHost is the number of VMs a Host may run under the rule. A Host
	// that already runs this many is at the limit. The scheduler uses 1 when it
	// is not set.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	MaxVMsPerHost *int32 `json:"maxVMsPerHost,omitempty"`

	// Scope defines how the rule is applied. With strict, a Host at the limit is
	// not a candidate. With preferred or no value, a Host at the limit ranks
	// lower.
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// ClusterAntiAffinityRule defines cluster anti-affinity rules. Accepted but
// not acted on in this release.
type ClusterAntiAffinityRule struct {
	// Enabled indicates if cluster anti-affinity is enabled. Accepted but not
	// acted on in this release.
	Enabled bool `json:"enabled"`

	// MaxVMsPerCluster limits the number of VMs per cluster. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	MaxVMsPerCluster *int32 `json:"maxVMsPerCluster,omitempty"`

	// Scope defines the scope of the anti-affinity rule. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// DatastoreAntiAffinityRule defines datastore anti-affinity rules. Accepted
// but not acted on in this release.
type DatastoreAntiAffinityRule struct {
	// Enabled indicates if datastore anti-affinity is enabled. Accepted but not
	// acted on in this release.
	Enabled bool `json:"enabled"`

	// MaxVMsPerDatastore limits the number of VMs per datastore. Accepted but
	// not acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	MaxVMsPerDatastore *int32 `json:"maxVMsPerDatastore,omitempty"`

	// Scope defines the scope of the anti-affinity rule. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// ZoneAntiAffinityRule defines zone anti-affinity rules. Accepted but not
// acted on in this release.
type ZoneAntiAffinityRule struct {
	// Enabled indicates if zone anti-affinity is enabled. Accepted but not acted
	// on in this release.
	Enabled bool `json:"enabled"`

	// MaxVMsPerZone limits the number of VMs per zone. Accepted but not acted on
	// in this release.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	MaxVMsPerZone *int32 `json:"maxVMsPerZone,omitempty"`

	// Scope defines the scope of the anti-affinity rule. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// ApplicationAntiAffinityRule defines application anti-affinity rules.
// Accepted but not acted on in this release.
type ApplicationAntiAffinityRule struct {
	// Enabled indicates if application anti-affinity is enabled. Accepted but
	// not acted on in this release.
	Enabled bool `json:"enabled"`

	// ApplicationLabel specifies the label key used to identify applications.
	// Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default="app"
	ApplicationLabel string `json:"applicationLabel,omitempty"`

	// Scope defines the scope of the anti-affinity rule. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// HostAffinityRule prefers, or requires, the listed Hosts.
type HostAffinityRule struct {
	// Enabled turns the rule on. A rule that is not enabled is ignored.
	Enabled bool `json:"enabled"`

	// PreferredHosts lists Hosts by resource name.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	PreferredHosts []string `json:"preferredHosts,omitempty"`

	// Scope defines how the rule is applied. With strict, only the listed Hosts
	// are candidates. With preferred or no value, a listed Host ranks higher.
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// ClusterAffinityRule defines cluster affinity rules. Accepted but not acted
// on in this release.
type ClusterAffinityRule struct {
	// Enabled indicates if cluster affinity is enabled. Accepted but not acted
	// on in this release.
	Enabled bool `json:"enabled"`

	// PreferredClusters lists preferred clusters. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	PreferredClusters []string `json:"preferredClusters,omitempty"`

	// Scope defines the scope of the affinity rule. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// DatastoreAffinityRule defines datastore affinity rules. Accepted but not
// acted on in this release.
type DatastoreAffinityRule struct {
	// Enabled indicates if datastore affinity is enabled. Accepted but not acted
	// on in this release.
	Enabled bool `json:"enabled"`

	// PreferredDatastores lists preferred datastores. Accepted but not acted on
	// in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	PreferredDatastores []string `json:"preferredDatastores,omitempty"`

	// Scope defines the scope of the affinity rule. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// ZoneAffinityRule defines zone affinity rules. Accepted but not acted on in
// this release.
type ZoneAffinityRule struct {
	// Enabled indicates if zone affinity is enabled. Accepted but not acted on
	// in this release.
	Enabled bool `json:"enabled"`

	// PreferredZones lists preferred zones. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	PreferredZones []string `json:"preferredZones,omitempty"`

	// Scope defines the scope of the affinity rule. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// ApplicationAffinityRule defines application affinity rules. Accepted but
// not acted on in this release.
type ApplicationAffinityRule struct {
	// Enabled indicates if application affinity is enabled. Accepted but not
	// acted on in this release.
	Enabled bool `json:"enabled"`

	// ApplicationLabel specifies the label key used to identify applications.
	// Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default="app"
	ApplicationLabel string `json:"applicationLabel,omitempty"`

	// Scope defines the scope of the affinity rule. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:Enum=strict;preferred
	Scope string `json:"scope,omitempty"`
}

// VMAntiAffinity defines anti-affinity rules between VMs, evaluated against
// the other VMs in the namespace of the VM being placed. A Host counts as
// matching a term when a VM on it has labels that satisfy the term.
type VMAntiAffinity struct {
	// RequiredDuringScheduling specifies hard anti-affinity rules: a Host that
	// runs a VM matching any term is not a candidate.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	RequiredDuringScheduling []VMAffinityTerm `json:"requiredDuringScheduling,omitempty"`

	// PreferredDuringScheduling specifies soft anti-affinity rules: a Host that
	// runs a VM matching a term ranks lower by that term's weight.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	PreferredDuringScheduling []WeightedVMAffinityTerm `json:"preferredDuringScheduling,omitempty"`
}

// VMAffinity defines affinity rules between VMs, evaluated against the other
// VMs in the namespace of the VM being placed. A Host counts as matching a
// term when a VM on it has labels that satisfy the term.
type VMAffinity struct {
	// RequiredDuringScheduling specifies hard affinity rules: a Host is a
	// candidate only if, for every term, it runs a VM that matches the term.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	RequiredDuringScheduling []VMAffinityTerm `json:"requiredDuringScheduling,omitempty"`

	// PreferredDuringScheduling specifies soft affinity rules: a Host that runs
	// a VM matching a term ranks higher by that term's weight.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	PreferredDuringScheduling []WeightedVMAffinityTerm `json:"preferredDuringScheduling,omitempty"`
}

// VMAffinityTerm defines a VM affinity term. A VM matches the term when its
// labels satisfy both labelSelector and matchExpressions; a term with neither
// matches every VM. The scheduler treats every term as host-level:
// topologyKey, namespaces and namespaceSelector are not acted on in this
// release.
type VMAffinityTerm struct {
	// LabelSelector selects VMs by their labels.
	// +optional
	LabelSelector *metav1.LabelSelector `json:"labelSelector,omitempty"`

	// Namespaces specifies which namespaces to consider. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Namespaces []string `json:"namespaces,omitempty"`

	// NamespaceSelector selects namespaces using label selectors. Accepted but
	// not acted on in this release.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// TopologyKey specifies the topology domain for the rule. It is required but
	// not acted on in this release: every term is evaluated per Host.
	// +kubebuilder:validation:MaxLength=253
	TopologyKey string `json:"topologyKey"`

	// MatchExpressions is a list of VM selector requirements on the VM's labels.
	// All of them must hold, together with labelSelector.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	MatchExpressions []VMSelectorRequirement `json:"matchExpressions,omitempty"`
}

// WeightedVMAffinityTerm defines a weighted VM affinity term.
type WeightedVMAffinityTerm struct {
	// Weight is added to (affinity) or subtracted from (anti-affinity) the rank
	// of a Host that runs a VM matching the term.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Weight int32 `json:"weight"`

	// VMAffinityTerm defines the VM affinity term
	VMAffinityTerm VMAffinityTerm `json:"vmAffinityTerm"`
}

// VMSelectorRequirement defines a requirement on the labels of a VM.
type VMSelectorRequirement struct {
	// Key is the label key that the requirement applies to.
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`

	// Operator represents the relationship between the label and Values: In,
	// NotIn, Exists or DoesNotExist.
	// +kubebuilder:validation:Enum=In;NotIn;Exists;DoesNotExist
	Operator VMSelectorOperator `json:"operator"`

	// Values is the list of label values used by In and NotIn. It is ignored by
	// Exists and DoesNotExist.
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

// VMToleration represents a toleration for VM placement. Accepted but not
// acted on in this release.
type VMToleration struct {
	// Key is the taint key that the toleration applies to. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key,omitempty"`

	// Operator represents the relationship between the key and value. Accepted
	// but not acted on in this release.
	// +optional
	// +kubebuilder:default="Equal"
	// +kubebuilder:validation:Enum=Exists;Equal
	Operator VMTolerationOperator `json:"operator,omitempty"`

	// Value is the taint value the toleration matches to. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Value string `json:"value,omitempty"`

	// Effect indicates the taint effect to match. Accepted but not acted on in
	// this release.
	// +optional
	Effect VMTaintEffect `json:"effect,omitempty"`

	// TolerationSeconds represents the period of time the toleration tolerates
	// the taint. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TolerationSeconds *int64 `json:"tolerationSeconds,omitempty"`
}

// VMTolerationOperator represents the operator for toleration. Accepted but
// not acted on in this release.
// +kubebuilder:validation:Enum=Exists;Equal
type VMTolerationOperator string

const (
	// VMTolerationOpExists means the toleration exists
	VMTolerationOpExists VMTolerationOperator = "Exists"
	// VMTolerationOpEqual means the toleration equals the value
	VMTolerationOpEqual VMTolerationOperator = "Equal"
)

// VMTaintEffect represents the effect of a taint. Accepted but not acted on
// in this release.
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

// ResourceConstraints defines placement constraints on the resources and CPU
// features of a Host. The scheduler acts on minCPUPerHost, minMemoryPerHost,
// minDiskSpacePerHost, requiredFeatures and preferredFeatures. The
// utilization limits are accepted but not acted on in this release.
type ResourceConstraints struct {
	// MinCPUPerHost is the minimum number of CPUs a Host must have
	// (Host.status.allocatableCPU, before overcommit). A Host below it is not a
	// candidate.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinCPUPerHost *int32 `json:"minCPUPerHost,omitempty"`

	// MinMemoryPerHost is the minimum memory a Host must have
	// (Host.status.allocatableMemoryMiB, before overcommit). A Host below it is
	// not a candidate.
	// +optional
	MinMemoryPerHost *resource.Quantity `json:"minMemoryPerHost,omitempty"`

	// MinDiskSpacePerHost is the minimum allocatable storage a Host must report
	// (Host.status.allocatableStorageBytes). A Host below it, or one that
	// reports none, is not a candidate.
	// +optional
	MinDiskSpacePerHost *resource.Quantity `json:"minDiskSpacePerHost,omitempty"`

	// MaxCPUUtilization specifies maximum allowed CPU utilization. Accepted but
	// not acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MaxCPUUtilization *int32 `json:"maxCPUUtilization,omitempty"`

	// MaxMemoryUtilization specifies maximum allowed memory utilization.
	// Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MaxMemoryUtilization *int32 `json:"maxMemoryUtilization,omitempty"`

	// MaxDiskUtilization specifies maximum allowed disk utilization. Accepted
	// but not acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MaxDiskUtilization *int32 `json:"maxDiskUtilization,omitempty"`

	// RequiredFeatures lists CPU features that a Host must report in
	// Host.status.cpuFeatures. A Host that lacks one is not a candidate.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	RequiredFeatures []string `json:"requiredFeatures,omitempty"`

	// PreferredFeatures lists CPU features that rank a Host higher when it
	// reports them in Host.status.cpuFeatures.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	PreferredFeatures []string `json:"preferredFeatures,omitempty"`
}

// SecurityConstraints defines security-based placement constraints. Accepted
// but not acted on in this release.
type SecurityConstraints struct {
	// RequireSecureBoot requires hosts that support secure boot. Accepted but
	// not acted on in this release.
	// +optional
	RequireSecureBoot bool `json:"requireSecureBoot,omitempty"`

	// RequireTPM requires hosts that support TPM. Accepted but not acted on in
	// this release.
	// +optional
	RequireTPM bool `json:"requireTPM,omitempty"`

	// RequireEncryptedStorage requires hosts that support encrypted storage.
	// Accepted but not acted on in this release.
	// +optional
	RequireEncryptedStorage bool `json:"requireEncryptedStorage,omitempty"`

	// RequireNUMATopology requires hosts that support NUMA topology. Accepted
	// but not acted on in this release.
	// +optional
	RequireNUMATopology bool `json:"requireNUMATopology,omitempty"`

	// AllowedSecurityGroups lists allowed security groups. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	AllowedSecurityGroups []string `json:"allowedSecurityGroups,omitempty"`

	// DeniedSecurityGroups lists denied security groups. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	DeniedSecurityGroups []string `json:"deniedSecurityGroups,omitempty"`

	// IsolationLevel specifies the required isolation level. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:Enum=none;basic;strict;maximum
	IsolationLevel string `json:"isolationLevel,omitempty"`

	// TrustLevel specifies the required trust level. Accepted but not acted on
	// in this release.
	// +optional
	// +kubebuilder:validation:Enum=untrusted;basic;trusted;highly-trusted
	TrustLevel string `json:"trustLevel,omitempty"`
}

// VMPlacementPolicyStatus defines the observed state of VMPlacementPolicy.
// Not populated in this release: no controller writes it.
type VMPlacementPolicyStatus struct {
	// ObservedGeneration is the most recent generation observed by the
	// controller. Not populated in this release.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// UsedByVMs lists VMs currently using this policy. Not populated in this
	// release.
	// +optional
	// +kubebuilder:validation:MaxItems=1000
	UsedByVMs []LocalObjectReference `json:"usedByVMs,omitempty"`

	// Conditions represent the current service state. Not populated in this
	// release.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ValidationResults contains validation results for different providers. Not
	// populated in this release.
	// +optional
	ValidationResults map[string]PolicyValidationResult `json:"validationResults,omitempty"`

	// PlacementStats provides statistics about VM placements using this policy.
	// Not populated in this release.
	// +optional
	PlacementStats *PlacementStatistics `json:"placementStats,omitempty"`

	// ConflictingPolicies lists policies that conflict with this policy. Not
	// populated in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	ConflictingPolicies []PolicyConflict `json:"conflictingPolicies,omitempty"`
}

// PolicyValidationResult represents a validation result for a provider. Not
// populated in this release.
type PolicyValidationResult struct {
	// Valid indicates if the policy is valid for the provider. Not populated in
	// this release.
	Valid bool `json:"valid"`

	// Message provides details about the validation result. Not populated in
	// this release.
	// +optional
	Message string `json:"message,omitempty"`

	// Warnings lists any validation warnings. Not populated in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Warnings []string `json:"warnings,omitempty"`

	// Errors lists any validation errors. Not populated in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Errors []string `json:"errors,omitempty"`

	// SupportedFeatures lists features supported by the provider. Not populated
	// in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	SupportedFeatures []string `json:"supportedFeatures,omitempty"`

	// UnsupportedFeatures lists features not supported by the provider. Not
	// populated in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	UnsupportedFeatures []string `json:"unsupportedFeatures,omitempty"`

	// LastValidated is when this validation was last performed. Not populated in
	// this release.
	// +optional
	LastValidated *metav1.Time `json:"lastValidated,omitempty"`
}

// PlacementStatistics provides statistics about VM placements. Not populated
// in this release.
type PlacementStatistics struct {
	// TotalPlacements is the total number of VM placements using this policy.
	// Not populated in this release.
	// +optional
	TotalPlacements int32 `json:"totalPlacements,omitempty"`

	// SuccessfulPlacements is the number of successful placements. Not populated
	// in this release.
	// +optional
	SuccessfulPlacements int32 `json:"successfulPlacements,omitempty"`

	// FailedPlacements is the number of failed placements. Not populated in this
	// release.
	// +optional
	FailedPlacements int32 `json:"failedPlacements,omitempty"`

	// AveragePlacementTime is the average time for VM placement. Not populated
	// in this release.
	// +optional
	AveragePlacementTime *metav1.Duration `json:"averagePlacementTime,omitempty"`

	// ConstraintViolations is the number of constraint violations. Not populated
	// in this release.
	// +optional
	ConstraintViolations int32 `json:"constraintViolations,omitempty"`

	// LastPlacementTime is when the last VM was placed using this policy. Not
	// populated in this release.
	// +optional
	LastPlacementTime *metav1.Time `json:"lastPlacementTime,omitempty"`

	// PlacementDistribution shows how VMs are distributed across hosts/clusters.
	// Not populated in this release.
	// +optional
	PlacementDistribution map[string]int32 `json:"placementDistribution,omitempty"`
}

// PolicyConflict represents a conflict between policies. Not populated in
// this release.
type PolicyConflict struct {
	// PolicyName is the name of the conflicting policy. Not populated in this
	// release.
	PolicyName string `json:"policyName"`

	// ConflictType describes the type of conflict. Not populated in this
	// release.
	// +kubebuilder:validation:Enum=hard;soft;resource;security;affinity
	ConflictType string `json:"conflictType"`

	// Description provides details about the conflict. Not populated in this
	// release.
	// +optional
	Description string `json:"description,omitempty"`

	// Severity indicates the severity of the conflict. Not populated in this
	// release.
	// +optional
	// +kubebuilder:validation:Enum=low;medium;high;critical
	Severity string `json:"severity,omitempty"`

	// ResolutionSuggestion provides suggestions for resolving the conflict. Not
	// populated in this release.
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

// VMPlacementPolicy holds rules for choosing the Host of a VirtualMachine. A
// VirtualMachine references it with spec.placementRef, in its own namespace.
// Only the scheduler of a clustered Provider (spec.topology: cluster) reads
// it, when it places a VM on a Host; there, a VM whose placementRef names a
// policy that does not exist is not scheduled. On any other Provider the
// reference is not read. Within the policy, hard, soft, affinity,
// antiAffinity and resourceConstraints are acted on as described on each
// field; securityConstraints, priority and weight are accepted but not acted
// on in this release. Status is not populated in this release, so the Used By
// VMs, Successful and Failed columns are empty.
// +kubebuilder:storageversion
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

func init() {
	SchemeBuilder.Register(&VMPlacementPolicy{}, &VMPlacementPolicyList{})
}
