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

// HostInUseFinalizer is the finalizer the Host controller keeps on every Host.
// It is removed only once no VirtualMachine of the
// Host's Provider names the Host in status.placement.host or
// status.placement.pendingHost, so a Host that per-VM calls (and VM finalizer
// cleanup) still route to cannot vanish from the provider's inventory.
const HostInUseFinalizer = "host.infra.virtrigaud.io/in-use"

// HostHealth is the live health of a Host, observed by the operator's inventory
// sync from the clustered provider (the ListHosts and GetHostInfo calls).
// +kubebuilder:validation:Enum=Ready;NotReady;Unknown
type HostHealth string

const (
	// HostHealthReady indicates the host is reachable and the provider could
	// read its inventory (for libvirt, a virsh nodeinfo over SSH succeeded).
	HostHealthReady HostHealth = "Ready"
	// HostHealthNotReady indicates the host was probed but is not currently
	// usable for placement or migration.
	HostHealthNotReady HostHealth = "NotReady"
	// HostHealthUnknown indicates the host's health has not yet been determined
	// (e.g. before the first successful heartbeat).
	HostHealthUnknown HostHealth = "Unknown"
)

// HostEndpointPattern is the exact set of connection URIs a Host.spec.endpoint
// may carry. It is the SINGLE regular expression shared by the CRD admission
// schema (the +kubebuilder:validation:Pattern marker on HostSpec.Endpoint, which
// must be kept byte-identical — TestHostCRDSchemaEndpointPatternMatchesGo
// enforces that) and the provider-side re-validation in
// internal/clustered/hostsecret, which cannot import this package.
//
// Accepted shapes (anything else is rejected):
//
//	qemu+ssh://[user@]host[:port]/system
//	qemu+ssh://[user@]host[:port]/session
//	grpc://host:port                      (passes validation; not connectable, see HostSpec.Endpoint)
//
// where user is [A-Za-z0-9._-]+ and host is a DNS name, an IPv4 address, or a
// bracketed IPv6 address. Query strings, fragments, percent-encoding, and any
// other path are rejected: the endpoint's path is forwarded to the hypervisor
// host as a libvirt connection URI, so it must never carry shell metacharacters
// (security: tenant-controlled command injection on the hypervisor host).
const HostEndpointPattern = `^(qemu\+ssh://([A-Za-z0-9._-]+@)?([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*|\[[0-9A-Fa-f:.]+\])(:[0-9]{1,5})?/(system|session)|grpc://([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*|\[[0-9A-Fa-f:.]+\]):[0-9]{1,5})$`

// HostSpec is admin-authored desired inventory: one bare hypervisor host that a
// clustered Provider (spec.topology=cluster) executes on. The admin
// declares which hosts exist; the provider reports what they can do right now
// (see HostStatus).
//
// Same-namespace model (security): a Host, its HostPool, its optional credential
// Secret, and the clustered Provider it names all live in ONE namespace — the
// Provider's. Every reference on this spec is therefore a LocalObjectReference
// (no namespace field). A Host in another namespace can neither enrol itself
// into a Provider's inventory nor point the operator at a credential Secret in
// a namespace its author does not control: the inventory render lists Hosts and
// resolves credential Secrets in the Provider's namespace only.
type HostSpec struct {
	// ProviderRef is the clustered Provider that executes on this host. It must
	// be in the Host's own namespace: a Host can only be fronted by a Provider in
	// the same namespace (no cross-namespace inventory injection).
	ProviderRef LocalObjectReference `json:"providerRef"`

	// PoolRef is the HostPool this host belongs to. Membership is declared by the
	// host itself (Host.spec.poolRef names the HostPool), not by the pool.
	PoolRef LocalObjectReference `json:"poolRef"`

	// Endpoint is the host connection URI. Only these shapes are accepted:
	// qemu+ssh://[user@]host[:port]/system, qemu+ssh://[user@]host[:port]/session,
	// and grpc://host:port. The host is a DNS name, an IPv4 address, or a
	// bracketed IPv6 address. No query string, fragment, percent-encoding, or
	// other path is allowed.
	//
	// The only clustered provider type today is libvirt, and it connects to a
	// host over SSH with the qemu+ssh:// form only (the user, port and
	// system|session instance come from the URI; the SSH key and known_hosts
	// come from the credentials Secret). A grpc://host:port value passes
	// validation but the libvirt provider refuses to connect to it, so the host
	// cannot become Ready and is never used for placement; use qemu+ssh://.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^(qemu\+ssh://([A-Za-z0-9._-]+@)?([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*|\[[0-9A-Fa-f:.]+\])(:[0-9]{1,5})?/(system|session)|grpc://([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*|\[[0-9A-Fa-f:.]+\]):[0-9]{1,5})$`
	Endpoint string `json:"endpoint"`

	// CredentialSecretRef optionally overrides the Provider's credentials for
	// this host (SSH key / known_hosts material). Defaults to the Provider's.
	// The Secret must be in the Host's (= the Provider's) namespace: the operator
	// never reads a credential Secret from another namespace on a Host's behalf,
	// so a Host author cannot exfiltrate a Secret they cannot already read.
	// +optional
	CredentialSecretRef *LocalObjectReference `json:"credentialSecretRef,omitempty"`

	// Labels are placement facts: storage-pool visibility, network/bridge
	// visibility, zone/rack, e.g. {"storage.virtrigaud.io/pool-nfs01":"true",
	// "net.virtrigaud.io/br-vlan100":"true","topology.virtrigaud.io/rack":"r7"}.
	// The scheduler treats them as hard constraints: a host is a candidate for a
	// VM only if it carries net.virtrigaud.io/<network>="true" for every network
	// the VM attaches to, and the labels a VMPlacementPolicy nodeSelector
	// requires. storage.virtrigaud.io/pool-<name> labels are accepted but a VM
	// create is not yet checked against them.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// This is a defaulted bool and deliberately carries NO omitempty: with
	// omitempty an explicit false would be dropped on marshal, the apiserver
	// would re-apply the true default, and the field would silently flip
	// false->true.

	// Schedulable, when false, cordons the host: no new VM is placed on it, and
	// VMs already on it stay where they are (VirtRigaud does not move or
	// evacuate them). Defaults true.
	// +optional
	// +kubebuilder:default=true
	Schedulable bool `json:"schedulable"`
}

// HostStatus is operator-synced observed state from the provider's ListHosts and
// GetHostInfo calls. It carries capacity and health only, never connection
// secrets.
type HostStatus struct {
	// Health is the live host health as last observed by the inventory sync
	// (for libvirt, whether a virsh nodeinfo over SSH succeeded). Only Ready
	// hosts are placement candidates.
	// +optional
	Health HostHealth `json:"health,omitempty"`

	// AllocatableCPU is the schedulable vCPU capacity the provider reports live.
	// +optional
	AllocatableCPU *int32 `json:"allocatableCPU,omitempty"`

	// AllocatableMemoryMiB is the schedulable memory in MiB the provider reports
	// live.
	// +optional
	AllocatableMemoryMiB *int64 `json:"allocatableMemoryMiB,omitempty"`

	// AllocatableStorageBytes is the free storage in bytes the provider reports
	// live for new VM disks: for libvirt, the space available to non-root users
	// in the default storage pool's directory (statvfs, measured on every
	// sync), and 0 when it cannot be measured.
	// +optional
	AllocatableStorageBytes *int64 `json:"allocatableStorageBytes,omitempty"`

	// CPUModel is the host CPU baseline model as the provider reports it.
	// Informational: no controller reads it.
	// +optional
	CPUModel string `json:"cpuModel,omitempty"`

	// CPUFeatures are the host CPU features as the provider reports them. The
	// scheduler matches a VMPlacementPolicy's required CPU features against them.
	// +optional
	CPUFeatures []string `json:"cpuFeatures,omitempty"`

	// MachineTypes are the machine types the host supports, as the provider
	// reports them.
	// +optional
	MachineTypes []string `json:"machineTypes,omitempty"`

	// EmulatorVersion is the host emulator version as the provider reports it.
	// Informational: no controller reads it.
	// +optional
	EmulatorVersion string `json:"emulatorVersion,omitempty"`

	// BoundVMs is reserved for the count of VMs bound to this host. It is not
	// populated in this release and stays 0 (omitted); count the VirtualMachines
	// whose status.placement.host names this Host instead.
	// +optional
	BoundVMs int32 `json:"boundVMs,omitempty"`

	// LastHeartbeatTime is when the operator last successfully synced this host.
	// +optional
	LastHeartbeatTime *metav1.Time `json:"lastHeartbeatTime,omitempty"`

	// ObservedGeneration reflects the generation of the Host spec observed by the
	// controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations of the host's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=hvh
//+kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.health`
//+kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef.name`
//+kubebuilder:printcolumn:name="Schedulable",type=boolean,JSONPath=`.spec.schedulable`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Host is the Schema for the hosts API: one bare hypervisor host registered into
// a clustered Provider's HostPool. It does not affect single-host
// (spec.topology=single) providers.
type Host struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HostSpec   `json:"spec,omitempty"`
	Status HostStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// HostList contains a list of Host.
type HostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Host `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Host{}, &HostList{})
}
