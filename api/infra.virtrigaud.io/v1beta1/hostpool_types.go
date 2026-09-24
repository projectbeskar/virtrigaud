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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Pool scheduling strategies (HostPoolSpec.Strategy, ADR-0007 D4).
const (
	// PoolStrategySpread spreads VMs across hosts to balance load. This is the
	// default strategy.
	PoolStrategySpread = "Spread"
	// PoolStrategyBinPack packs VMs onto the fewest hosts to maximize
	// consolidation.
	PoolStrategyBinPack = "BinPack"
)

// Intra-cluster migration storage modes (PoolMigrationPolicy.DefaultStorageMode,
// ADR-0007 D5). Capabilities constrain which modes a given hypervisor honors; an
// unsupported mode is rejected, never silently downgraded.
const (
	// MigrationStorageModeShared transfers RAM + device state only; both hosts
	// see the same disk at the same path. This is the default and the clean case.
	MigrationStorageModeShared = "shared"
	// MigrationStorageModeBlockAll is libvirt live block migration of the full
	// disk (--copy-storage-all) when the disk is host-local and the VM must stay
	// up.
	MigrationStorageModeBlockAll = "block_all"
	// MigrationStorageModeBlockInc is libvirt incremental live block migration
	// (--copy-storage-inc).
	MigrationStorageModeBlockInc = "block_inc"
	// MigrationStorageModeStageCopy reuses the ADR-0006 cold staging pipeline for
	// tolerate-downtime local-disk moves (intra-cluster qcow2->qcow2, no format
	// conversion).
	MigrationStorageModeStageCopy = "stage_copy"
)

// OvercommitRatios are the CPU and memory overcommit ratios applied when
// computing a pool's allocatable capacity. Ratios are expressed as decimal
// strings (e.g. "4.0") to keep floating-point values out of the API surface.
type OvercommitRatios struct {
	// CPU is the vCPU overcommit ratio (e.g. "4.0" = up to 4 vCPU per physical
	// core). Empty means no CPU overcommit ("1.0").
	// +optional
	CPU string `json:"cpu,omitempty"`

	// Memory is the memory overcommit ratio (e.g. "1.0" = no memory overcommit).
	// Empty means no memory overcommit ("1.0").
	// +optional
	Memory string `json:"memory,omitempty"`
}

// StoragePoolRef names a shared or local storage pool available in a HostPool.
// Per-host visibility is asserted via Host.spec.labels (ADR-0007 D6). VirtRigaud
// consumes pre-set-up storage; it does not deploy it.
type StoragePoolRef struct {
	// Name is the storage pool name (e.g. a libvirt pool name or an NFS export
	// identifier). A matching Host.spec.labels entry asserts per-host visibility.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// PoolNetworkRef names a network (bridge / VLAN / overlay) available in a
// HostPool. Per-host presence is asserted via Host.spec.labels (ADR-0007 D6).
type PoolNetworkRef struct {
	// Name is the network name (e.g. the bridge name shared across the pool). A
	// matching Host.spec.labels entry asserts per-host presence.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// PoolMigrationPolicy is the default migration policy for VMs in a HostPool
// (ADR-0007 D5). Individual migrations may override these defaults.
type PoolMigrationPolicy struct {
	// DefaultLive requests live (RAM-transfer) migration by default. When false,
	// migrations tolerate downtime and may fall back to stage_copy. Defaults
	// true.
	//
	// This is a defaulted bool and deliberately carries NO omitempty (the
	// defaulted-bool footgun, ADR-0006/PR#235): with omitempty an explicit false
	// would be dropped on marshal and the apiserver would re-apply the true
	// default.
	// +optional
	// +kubebuilder:default=true
	DefaultLive bool `json:"defaultLive"`

	// DefaultStorageMode selects the intra-cluster data path (ADR-0007 D5).
	// Defaults to shared.
	// +optional
	// +kubebuilder:default=shared
	// +kubebuilder:validation:Enum=shared;block_all;block_inc;stage_copy
	DefaultStorageMode string `json:"defaultStorageMode,omitempty"`

	// RequireTLS forces QEMU-native --tls on the migration data path (required in
	// regulated/banking deployments). Defaults false.
	// +optional
	// +kubebuilder:default=false
	RequireTLS bool `json:"requireTLS,omitempty"`

	// BandwidthMbps optionally caps migration bandwidth in Mbps.
	// +optional
	// +kubebuilder:validation:Minimum=1
	BandwidthMbps *int32 `json:"bandwidthMbps,omitempty"`

	// MaxDowntimeMs optionally caps the tolerated switchover downtime in
	// milliseconds.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxDowntimeMs *int32 `json:"maxDowntimeMs,omitempty"`
}

// HostPoolSpec is admin-authored cluster policy for a named group of Hosts under
// one clustered Provider (ADR-0007 D3): scheduling strategy, overcommit,
// storage/network references, and the default migration policy.
//
// Same-namespace model (security): a HostPool lives in its clustered Provider's
// namespace, alongside that Provider's Hosts, so ProviderRef is a
// LocalObjectReference (see HostSpec).
type HostPoolSpec struct {
	// ProviderRef is the clustered Provider that owns this pool. It must be in
	// the HostPool's own namespace (no cross-namespace pool injection).
	ProviderRef LocalObjectReference `json:"providerRef"`

	// Strategy is the default scheduling strategy for VMs placed in this pool.
	// Defaults to Spread.
	// +optional
	// +kubebuilder:default=Spread
	// +kubebuilder:validation:Enum=Spread;BinPack
	Strategy string `json:"strategy,omitempty"`

	// Overcommit ratios applied when computing allocatable capacity.
	// +optional
	Overcommit *OvercommitRatios `json:"overcommit,omitempty"`

	// StoragePools are the named shared/local storage pools available in this
	// pool. Per-host visibility is asserted via Host.spec.labels (ADR-0007 D6).
	// Consumed, not deployed, by VirtRigaud.
	// +optional
	StoragePools []StoragePoolRef `json:"storagePools,omitempty"`

	// Networks are the named networks (bridge/VLAN/overlay) available in this
	// pool; per-host presence is asserted via Host.spec.labels (ADR-0007 D6).
	// +optional
	Networks []PoolNetworkRef `json:"networks,omitempty"`

	// Migration is the default migration policy for VMs in this pool.
	// +optional
	Migration *PoolMigrationPolicy `json:"migration,omitempty"`
}

// HostPoolStatus aggregates member-host counts and readiness for the pool.
//
// ADR-0007 specifies HostPool.status only in prose ("aggregates host counts +
// readiness"). These fields are the smallest additive shape consistent with that
// description and with the other kinds' status conventions (ObservedGeneration +
// Conditions).
type HostPoolStatus struct {
	// TotalHosts is the number of Hosts whose spec.poolRef points at this pool.
	// +optional
	TotalHosts int32 `json:"totalHosts,omitempty"`

	// ReadyHosts is the number of member Hosts reporting Health=Ready.
	// +optional
	ReadyHosts int32 `json:"readyHosts,omitempty"`

	// SchedulableHosts is the number of member Hosts that are both Health=Ready
	// and spec.schedulable=true.
	// +optional
	SchedulableHosts int32 `json:"schedulableHosts,omitempty"`

	// ObservedGeneration reflects the generation of the HostPool spec observed by
	// the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations of the pool's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=hp
//+kubebuilder:printcolumn:name="Strategy",type=string,JSONPath=`.spec.strategy`
//+kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.providerRef.name`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// HostPool is the Schema for the hostpools API: a named group of Hosts under one
// clustered Provider, carrying cluster scheduling and migration policy (ADR-0007
// P1 inventory foundation). It is additive and does not affect single-host
// (spec.topology=single) providers (ADR-0007 D9).
type HostPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HostPoolSpec   `json:"spec,omitempty"`
	Status HostPoolStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// HostPoolList contains a list of HostPool.
type HostPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HostPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HostPool{}, &HostPoolList{})
}
