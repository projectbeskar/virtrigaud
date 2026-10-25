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

// Pool scheduling strategies (HostPoolSpec.Strategy).
const (
	// PoolStrategySpread spreads VMs across hosts to balance load. This is the
	// default strategy.
	PoolStrategySpread = "Spread"
	// PoolStrategyBinPack packs VMs onto the fewest hosts to maximize
	// consolidation.
	PoolStrategyBinPack = "BinPack"
)

// Intra-cluster migration storage modes (PoolMigrationPolicy.DefaultStorageMode).
// The policy is recorded but not acted on in this release (see
// PoolMigrationPolicy).
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
	// MigrationStorageModeStageCopy is a cold copy through a staging location for
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
// Per-host visibility is asserted via Host.spec.labels. VirtRigaud consumes
// pre-set-up storage; it does not deploy it. The list is descriptive in this
// release: no controller reads it.
type StoragePoolRef struct {
	// Name is the storage pool name (e.g. a libvirt pool name or an NFS export
	// identifier). A matching Host.spec.labels entry asserts per-host visibility.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// PoolNetworkRef names a network (bridge / VLAN / overlay) available in a
// HostPool. Per-host presence is asserted via Host.spec.labels. The list is
// descriptive in this release: no controller reads it.
type PoolNetworkRef struct {
	// Name is the network name (e.g. the bridge name shared across the pool). A
	// matching Host.spec.labels entry asserts per-host presence.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// PoolMigrationPolicy is the default migration policy for VMs in a HostPool.
// The API records it, but no controller reads it in this release: there is no
// live migration between the hosts of a pool, so none of these fields has an
// effect.
type PoolMigrationPolicy struct {
	// This is a defaulted bool and deliberately carries NO omitempty: with
	// omitempty an explicit false would be dropped on marshal and the apiserver
	// would re-apply the true default.

	// DefaultLive requests live (RAM-transfer) migration by default. When false,
	// migrations tolerate downtime and may fall back to stage_copy. Defaults
	// true. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default=true
	DefaultLive bool `json:"defaultLive"`

	// DefaultStorageMode selects the intra-cluster data path. Defaults to shared.
	// Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default=shared
	// +kubebuilder:validation:Enum=shared;block_all;block_inc;stage_copy
	DefaultStorageMode string `json:"defaultStorageMode,omitempty"`

	// RequireTLS would force QEMU-native --tls on the migration data path.
	// Defaults false. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default=false
	RequireTLS bool `json:"requireTLS,omitempty"`

	// BandwidthMbps optionally caps migration bandwidth in Mbps. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=1
	BandwidthMbps *int32 `json:"bandwidthMbps,omitempty"`

	// MaxDowntimeMs optionally caps the tolerated switchover downtime in
	// milliseconds. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxDowntimeMs *int32 `json:"maxDowntimeMs,omitempty"`
}

// HostPoolSpec is admin-authored cluster policy for a named group of Hosts under
// one clustered Provider: scheduling strategy, overcommit, storage/network
// references, and the default migration policy. Only strategy and overcommit
// affect placement in this release.
//
// Same-namespace model (security): a HostPool lives in its clustered Provider's
// namespace, alongside that Provider's Hosts, so ProviderRef is a
// LocalObjectReference (see HostSpec).
type HostPoolSpec struct {
	// ProviderRef is the clustered Provider that owns this pool. It must be in
	// the HostPool's own namespace (no cross-namespace pool injection).
	ProviderRef LocalObjectReference `json:"providerRef"`

	// Strategy is the scheduling strategy for VMs placed in this pool: Spread
	// balances VMs across the hosts, BinPack fills the fewest hosts first.
	// Defaults to Spread.
	// +optional
	// +kubebuilder:default=Spread
	// +kubebuilder:validation:Enum=Spread;BinPack
	Strategy string `json:"strategy,omitempty"`

	// Overcommit ratios the scheduler applies to each host's CPU and memory when
	// it checks whether a new VM fits.
	// +optional
	Overcommit *OvercommitRatios `json:"overcommit,omitempty"`

	// StoragePools are the named shared/local storage pools available in this
	// pool. Per-host visibility is asserted via Host.spec.labels. Consumed, not
	// deployed, by VirtRigaud. Descriptive: no controller reads this list.
	// +optional
	StoragePools []StoragePoolRef `json:"storagePools,omitempty"`

	// Networks are the named networks (bridge/VLAN/overlay) available in this
	// pool; per-host presence is asserted via Host.spec.labels. Descriptive: no
	// controller reads this list.
	// +optional
	Networks []PoolNetworkRef `json:"networks,omitempty"`

	// Migration is the default migration policy for VMs in this pool. Accepted but
	// not acted on in this release.
	// +optional
	Migration *PoolMigrationPolicy `json:"migration,omitempty"`
}

// HostPoolStatus aggregates member-host counts and readiness for the pool. The
// HostPool controller writes it whenever a member Host joins, leaves, or
// changes health, schedulability, providerRef or deletion state. Its Ready
// condition is True when SchedulableHosts > 0.
//
// A count of 0 (an empty pool, or no schedulable host) is written as 0 rather
// than omitted, so it shows in the status and in the kubectl get columns.
type HostPoolStatus struct {
	// TotalHosts is the number of Hosts whose spec.poolRef points at this pool.
	// Only Hosts in the pool's own namespace are members (the same-namespace
	// model); a Host being deleted is counted until it is gone.
	// +optional
	TotalHosts int32 `json:"totalHosts"`

	// ReadyHosts is the number of member Hosts reporting Health=Ready.
	// +optional
	ReadyHosts int32 `json:"readyHosts"`

	// SchedulableHosts is the number of member Hosts that are both Health=Ready
	// and spec.schedulable=true, and that the scheduler can therefore place a
	// new VM on: a Host being deleted, or one whose spec.providerRef is not the
	// pool's Provider, is never a placement target and is not counted.
	// +optional
	SchedulableHosts int32 `json:"schedulableHosts"`

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
//+kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.totalHosts`,description="Member Hosts (spec.poolRef names this pool)"
//+kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyHosts`,description="Member Hosts reporting Health=Ready"
//+kubebuilder:printcolumn:name="Schedulable",type=integer,JSONPath=`.status.schedulableHosts`,description="Member Hosts the scheduler can place a new VM on"
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// HostPool is the Schema for the hostpools API: a named group of Hosts under one
// clustered Provider, carrying cluster scheduling and migration policy. It does
// not affect single-host (spec.topology=single) providers.
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
