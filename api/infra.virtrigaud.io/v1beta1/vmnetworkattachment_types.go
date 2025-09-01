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

// VMNetworkAttachmentSpec defines the desired state of VMNetworkAttachment
type VMNetworkAttachmentSpec struct {
	// Network defines the underlying network configuration
	Network NetworkConfig `json:"network"`

	// IPAllocation defines IP address allocation settings
	// +optional
	IPAllocation *IPAllocationConfig `json:"ipAllocation,omitempty"`

	// Security defines network security settings
	// +optional
	Security *NetworkSecurityConfig `json:"security,omitempty"`

	// QoS defines Quality of Service settings
	// +optional
	QoS *NetworkQoSConfig `json:"qos,omitempty"`

	// Metadata contains network metadata and labels
	// +optional
	Metadata *NetworkMetadata `json:"metadata,omitempty"`
}

// NetworkConfig defines the underlying network configuration
type NetworkConfig struct {
	// VSphere contains vSphere-specific network configuration
	// +optional
	VSphere *VSphereNetworkConfig `json:"vsphere,omitempty"`

	// Libvirt contains Libvirt-specific network configuration
	// +optional
	Libvirt *LibvirtNetworkConfig `json:"libvirt,omitempty"`

	// Type specifies the network type
	// +optional
	// +kubebuilder:default="bridged"
	// +kubebuilder:validation:Enum=bridged;nat;isolated;host-only;external
	Type NetworkType `json:"type,omitempty"`

	// MTU specifies the Maximum Transmission Unit
	// +optional
	// +kubebuilder:validation:Minimum=68
	// +kubebuilder:validation:Maximum=9000
	// +kubebuilder:default=1500
	MTU *int32 `json:"mtu,omitempty"`
}

// NetworkType represents the type of network
// +kubebuilder:validation:Enum=bridged;nat;isolated;host-only;external
type NetworkType string

const (
	// NetworkTypeBridged indicates a bridged network
	NetworkTypeBridged NetworkType = "bridged"
	// NetworkTypeNAT indicates a NAT network
	NetworkTypeNAT NetworkType = "nat"
	// NetworkTypeIsolated indicates an isolated network
	NetworkTypeIsolated NetworkType = "isolated"
	// NetworkTypeHostOnly indicates a host-only network
	NetworkTypeHostOnly NetworkType = "host-only"
	// NetworkTypeExternal indicates an external network
	NetworkTypeExternal NetworkType = "external"
)

// VSphereNetworkConfig defines vSphere-specific network configuration
type VSphereNetworkConfig struct {
	// Portgroup specifies the vSphere portgroup name
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Portgroup string `json:"portgroup,omitempty"`

	// DistributedSwitch specifies the distributed virtual switch
	// +optional
	DistributedSwitch *DistributedSwitchConfig `json:"distributedSwitch,omitempty"`

	// VLAN specifies the VLAN configuration
	// +optional
	VLAN *VLANConfig `json:"vlan,omitempty"`

	// Security defines portgroup security settings
	// +optional
	Security *PortgroupSecurityConfig `json:"security,omitempty"`

	// TrafficShaping defines traffic shaping settings
	// +optional
	TrafficShaping *TrafficShapingConfig `json:"trafficShaping,omitempty"`
}

// DistributedSwitchConfig defines distributed virtual switch configuration
type DistributedSwitchConfig struct {
	// Name is the name of the distributed switch
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`

	// UUID is the UUID of the distributed switch (optional)
	// +optional
	UUID string `json:"uuid,omitempty"`

	// PortgroupType specifies the type of portgroup
	// +optional
	// +kubebuilder:validation:Enum=ephemeral;distributed
	PortgroupType string `json:"portgroupType,omitempty"`
}

// VLANConfig defines VLAN configuration
type VLANConfig struct {
	// Type specifies the VLAN type
	// +optional
	// +kubebuilder:default="none"
	// +kubebuilder:validation:Enum=none;vlan;pvlan;trunk
	Type string `json:"type,omitempty"`

	// VlanID specifies the VLAN ID for VLAN type
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4094
	VlanID *int32 `json:"vlanID,omitempty"`

	// TrunkVlanIDs specifies VLAN IDs for trunk type
	// +optional
	// +kubebuilder:validation:MaxItems=100
	TrunkVlanIDs []int32 `json:"trunkVlanIDs,omitempty"`

	// PrimaryVlanID specifies the primary VLAN ID for PVLAN
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4094
	PrimaryVlanID *int32 `json:"primaryVlanID,omitempty"`

	// SecondaryVlanID specifies the secondary VLAN ID for PVLAN
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4094
	SecondaryVlanID *int32 `json:"secondaryVlanID,omitempty"`
}

// PortgroupSecurityConfig defines portgroup security settings
type PortgroupSecurityConfig struct {
	// AllowPromiscuous allows promiscuous mode
	// +optional
	// +kubebuilder:default=false
	AllowPromiscuous *bool `json:"allowPromiscuous,omitempty"`

	// AllowMACChanges allows MAC address changes
	// +optional
	// +kubebuilder:default=true
	AllowMACChanges *bool `json:"allowMACChanges,omitempty"`

	// AllowForgedTransmits allows forged transmits
	// +optional
	// +kubebuilder:default=true
	AllowForgedTransmits *bool `json:"allowForgedTransmits,omitempty"`
}

// TrafficShapingConfig defines traffic shaping settings
type TrafficShapingConfig struct {
	// Enabled indicates if traffic shaping is enabled
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// AverageBandwidth is the average bandwidth in bits per second
	// +optional
	// +kubebuilder:validation:Minimum=1000
	AverageBandwidth *int64 `json:"averageBandwidth,omitempty"`

	// PeakBandwidth is the peak bandwidth in bits per second
	// +optional
	// +kubebuilder:validation:Minimum=1000
	PeakBandwidth *int64 `json:"peakBandwidth,omitempty"`

	// BurstSize is the burst size in bytes
	// +optional
	// +kubebuilder:validation:Minimum=1024
	BurstSize *int64 `json:"burstSize,omitempty"`
}

// LibvirtNetworkConfig defines Libvirt-specific network configuration
type LibvirtNetworkConfig struct {
	// NetworkName specifies the Libvirt network name
	// +optional
	// +kubebuilder:validation:MaxLength=255
	NetworkName string `json:"networkName,omitempty"`

	// Bridge specifies the bridge configuration
	// +optional
	Bridge *BridgeConfig `json:"bridge,omitempty"`

	// Model specifies the network device model
	// +optional
	// +kubebuilder:default="virtio"
	// +kubebuilder:validation:Enum=virtio;e1000;e1000e;rtl8139;pcnet;ne2k_pci
	Model string `json:"model,omitempty"`

	// Driver specifies the network driver configuration
	// +optional
	Driver *NetworkDriverConfig `json:"driver,omitempty"`

	// FilterRef specifies network filter configuration
	// +optional
	FilterRef *NetworkFilterRef `json:"filterRef,omitempty"`
}

// BridgeConfig defines bridge configuration
type BridgeConfig struct {
	// Name is the bridge name
	// +kubebuilder:validation:MaxLength=15
	Name string `json:"name"`

	// STP enables Spanning Tree Protocol
	// +optional
	// +kubebuilder:default=false
	STP bool `json:"stp,omitempty"`

	// Delay is the STP forwarding delay
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=30
	Delay *int32 `json:"delay,omitempty"`
}

// NetworkDriverConfig defines network driver configuration
type NetworkDriverConfig struct {
	// Name is the driver name
	// +optional
	// +kubebuilder:validation:Enum=kvm;vfio;uio
	Name string `json:"name,omitempty"`

	// Queues specifies the number of queues
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16
	Queues *int32 `json:"queues,omitempty"`

	// TxMode specifies the TX mode
	// +optional
	// +kubebuilder:validation:Enum=iothread;timer
	TxMode string `json:"txMode,omitempty"`

	// IOEventFD enables IO event file descriptor
	// +optional
	IOEventFD *bool `json:"ioEventFD,omitempty"`

	// EventIDX enables event index
	// +optional
	EventIDX *bool `json:"eventIDX,omitempty"`
}

// NetworkFilterRef references a network filter
type NetworkFilterRef struct {
	// Filter is the filter name
	// +kubebuilder:validation:MaxLength=255
	Filter string `json:"filter"`

	// Parameters contains filter parameters
	// +optional
	// +kubebuilder:validation:MaxProperties=20
	Parameters map[string]string `json:"parameters,omitempty"`
}

// IPAllocationConfig defines IP address allocation settings
type IPAllocationConfig struct {
	// Type specifies the IP allocation type
	// +optional
	// +kubebuilder:default="DHCP"
	// +kubebuilder:validation:Enum=DHCP;Static;Pool;None
	Type IPAllocationType `json:"type,omitempty"`

	// StaticConfig contains static IP configuration
	// +optional
	StaticConfig *StaticIPConfig `json:"staticConfig,omitempty"`

	// PoolConfig contains IP pool configuration
	// +optional
	PoolConfig *IPPoolConfig `json:"poolConfig,omitempty"`

	// DHCPConfig contains DHCP configuration
	// +optional
	DHCPConfig *DHCPConfig `json:"dhcpConfig,omitempty"`

	// DNSConfig contains DNS configuration
	// +optional
	DNSConfig *DNSConfig `json:"dnsConfig,omitempty"`
}

// IPAllocationType represents the type of IP allocation
// +kubebuilder:validation:Enum=DHCP;Static;Pool;None
type IPAllocationType string

const (
	// IPAllocationTypeDHCP uses DHCP for IP allocation
	IPAllocationTypeDHCP IPAllocationType = "DHCP"
	// IPAllocationTypeStatic uses static IP allocation
	IPAllocationTypeStatic IPAllocationType = "Static"
	// IPAllocationTypePool uses an IP pool for allocation
	IPAllocationTypePool IPAllocationType = "Pool"
	// IPAllocationTypeNone disables IP allocation
	IPAllocationTypeNone IPAllocationType = "None"
)

// StaticIPConfig defines static IP configuration
type StaticIPConfig struct {
	// Address is the static IP address (CIDR notation)
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)/([0-9]|[1-2][0-9]|3[0-2])$"
	Address string `json:"address"`

	// Gateway is the default gateway
	// +optional
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$"
	Gateway string `json:"gateway,omitempty"`

	// Routes contains static routes
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Routes []StaticRoute `json:"routes,omitempty"`
}

// StaticRoute defines a static route
type StaticRoute struct {
	// Destination is the destination network (CIDR notation)
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)/([0-9]|[1-2][0-9]|3[0-2])$"
	Destination string `json:"destination"`

	// Gateway is the route gateway
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$"
	Gateway string `json:"gateway"`

	// Metric is the route metric
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Metric *int32 `json:"metric,omitempty"`
}

// IPPoolConfig defines IP pool configuration
type IPPoolConfig struct {
	// PoolRef references an IP pool resource
	PoolRef LocalObjectReference `json:"poolRef"`

	// PreferredIP requests a preferred IP from the pool
	// +optional
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$"
	PreferredIP string `json:"preferredIP,omitempty"`
}

// DHCPConfig defines DHCP configuration
type DHCPConfig struct {
	// ClientID specifies the DHCP client ID
	// +optional
	// +kubebuilder:validation:MaxLength=255
	ClientID string `json:"clientID,omitempty"`

	// Hostname specifies the hostname to request
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Hostname string `json:"hostname,omitempty"`

	// Options contains DHCP options
	// +optional
	// +kubebuilder:validation:MaxProperties=20
	Options map[string]string `json:"options,omitempty"`
}

// DNSConfig defines DNS configuration
type DNSConfig struct {
	// Servers contains DNS server addresses
	// +optional
	// +kubebuilder:validation:MaxItems=10
	Servers []string `json:"servers,omitempty"`

	// SearchDomains contains DNS search domains
	// +optional
	// +kubebuilder:validation:MaxItems=10
	SearchDomains []string `json:"searchDomains,omitempty"`

	// Options contains DNS resolver options
	// +optional
	// +kubebuilder:validation:MaxProperties=10
	Options map[string]string `json:"options,omitempty"`
}

// NetworkSecurityConfig defines network security settings
type NetworkSecurityConfig struct {
	// Firewall contains firewall configuration
	// +optional
	Firewall *FirewallConfig `json:"firewall,omitempty"`

	// Isolation contains network isolation settings
	// +optional
	Isolation *NetworkIsolationConfig `json:"isolation,omitempty"`

	// Encryption contains network encryption settings
	// +optional
	Encryption *NetworkEncryptionConfig `json:"encryption,omitempty"`
}

// FirewallConfig defines firewall configuration
type FirewallConfig struct {
	// Enabled indicates if firewall is enabled
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// DefaultPolicy specifies the default firewall policy
	// +optional
	// +kubebuilder:default="ACCEPT"
	// +kubebuilder:validation:Enum=ACCEPT;DROP;REJECT
	DefaultPolicy string `json:"defaultPolicy,omitempty"`

	// Rules contains firewall rules
	// +optional
	// +kubebuilder:validation:MaxItems=100
	Rules []FirewallRule `json:"rules,omitempty"`
}

// FirewallRule defines a firewall rule
type FirewallRule struct {
	// Name is the rule name
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`

	// Action specifies the rule action
	// +kubebuilder:validation:Enum=ACCEPT;DROP;REJECT
	Action string `json:"action"`

	// Direction specifies the traffic direction
	// +kubebuilder:validation:Enum=in;out;inout
	Direction string `json:"direction"`

	// Protocol specifies the protocol
	// +optional
	// +kubebuilder:validation:Enum=tcp;udp;icmp;all
	Protocol string `json:"protocol,omitempty"`

	// SourceCIDR specifies the source CIDR
	// +optional
	SourceCIDR string `json:"sourceCIDR,omitempty"`

	// DestinationCIDR specifies the destination CIDR
	// +optional
	DestinationCIDR string `json:"destinationCIDR,omitempty"`

	// Ports specifies the port range
	// +optional
	Ports *PortRange `json:"ports,omitempty"`

	// Priority specifies the rule priority
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	Priority *int32 `json:"priority,omitempty"`
}

// PortRange defines a port range
type PortRange struct {
	// Start is the starting port
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Start int32 `json:"start"`

	// End is the ending port (optional, defaults to start)
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	End *int32 `json:"end,omitempty"`
}

// NetworkIsolationConfig defines network isolation settings
type NetworkIsolationConfig struct {
	// Mode specifies the isolation mode
	// +optional
	// +kubebuilder:validation:Enum=none;strict;custom
	Mode string `json:"mode,omitempty"`

	// AllowedNetworks contains allowed network CIDRs
	// +optional
	// +kubebuilder:validation:MaxItems=50
	AllowedNetworks []string `json:"allowedNetworks,omitempty"`

	// DeniedNetworks contains denied network CIDRs
	// +optional
	// +kubebuilder:validation:MaxItems=50
	DeniedNetworks []string `json:"deniedNetworks,omitempty"`
}

// NetworkEncryptionConfig defines network encryption settings
type NetworkEncryptionConfig struct {
	// Enabled indicates if encryption is enabled
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// Protocol specifies the encryption protocol
	// +optional
	// +kubebuilder:validation:Enum=ipsec;wireguard;openvpn
	Protocol string `json:"protocol,omitempty"`

	// KeyRef references encryption keys
	// +optional
	KeyRef *LocalObjectReference `json:"keyRef,omitempty"`
}

// NetworkQoSConfig defines Quality of Service settings
type NetworkQoSConfig struct {
	// IngressLimit limits inbound traffic in bits per second
	// +optional
	// +kubebuilder:validation:Minimum=1000
	IngressLimit *int64 `json:"ingressLimit,omitempty"`

	// EgressLimit limits outbound traffic in bits per second
	// +optional
	// +kubebuilder:validation:Minimum=1000
	EgressLimit *int64 `json:"egressLimit,omitempty"`

	// Priority specifies traffic priority
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=7
	Priority *int32 `json:"priority,omitempty"`

	// DSCP specifies DSCP marking
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=63
	DSCP *int32 `json:"dscp,omitempty"`
}

// NetworkMetadata contains network metadata and labels
type NetworkMetadata struct {
	// DisplayName is a human-readable name
	// +optional
	// +kubebuilder:validation:MaxLength=255
	DisplayName string `json:"displayName,omitempty"`

	// Description provides a description
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// Environment specifies the environment (dev, staging, prod)
	// +optional
	// +kubebuilder:validation:Enum=dev;staging;prod;test
	Environment string `json:"environment,omitempty"`

	// Tags are key-value pairs for categorizing
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Tags map[string]string `json:"tags,omitempty"`
}

// VMNetworkAttachmentStatus defines the observed state of VMNetworkAttachment
type VMNetworkAttachmentStatus struct {
	// Ready indicates if the network is ready for use
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Phase represents the current phase
	// +optional
	Phase NetworkAttachmentPhase `json:"phase,omitempty"`

	// Message provides additional details about the current state
	// +optional
	Message string `json:"message,omitempty"`

	// AvailableOn lists the providers where the network is available
	// +optional
	AvailableOn []string `json:"availableOn,omitempty"`

	// Conditions represent the latest available observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration reflects the generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ConnectedVMs is the number of VMs using this network
	// +optional
	ConnectedVMs int32 `json:"connectedVMs,omitempty"`

	// IPAllocations contains current IP allocations
	// +optional
	IPAllocations []IPAllocation `json:"ipAllocations,omitempty"`

	// ProviderStatus contains provider-specific status information
	// +optional
	ProviderStatus map[string]ProviderNetworkStatus `json:"providerStatus,omitempty"`
}

// NetworkAttachmentPhase represents the phase of network attachment
// +kubebuilder:validation:Enum=Pending;Configuring;Ready;Failed
type NetworkAttachmentPhase string

const (
	// NetworkAttachmentPhasePending indicates the network is being prepared
	NetworkAttachmentPhasePending NetworkAttachmentPhase = "Pending"
	// NetworkAttachmentPhaseConfiguring indicates the network is being configured
	NetworkAttachmentPhaseConfiguring NetworkAttachmentPhase = "Configuring"
	// NetworkAttachmentPhaseReady indicates the network is ready
	NetworkAttachmentPhaseReady NetworkAttachmentPhase = "Ready"
	// NetworkAttachmentPhaseFailed indicates the network configuration failed
	NetworkAttachmentPhaseFailed NetworkAttachmentPhase = "Failed"
)

// IPAllocation represents an IP allocation
type IPAllocation struct {
	// VM is the VM name that has the IP allocated
	VM string `json:"vm"`

	// IP is the allocated IP address
	IP string `json:"ip"`

	// MAC is the allocated MAC address
	// +optional
	MAC string `json:"mac,omitempty"`

	// AllocatedAt is when the IP was allocated
	// +optional
	AllocatedAt *metav1.Time `json:"allocatedAt,omitempty"`

	// LeaseExpiry is when the IP lease expires (for DHCP)
	// +optional
	LeaseExpiry *metav1.Time `json:"leaseExpiry,omitempty"`
}

// ProviderNetworkStatus contains provider-specific network status
type ProviderNetworkStatus struct {
	// Available indicates if the network is available on this provider
	Available bool `json:"available"`

	// ID is the provider-specific network identifier
	// +optional
	ID string `json:"id,omitempty"`

	// State is the provider-specific network state
	// +optional
	State string `json:"state,omitempty"`

	// LastUpdated is when the status was last updated
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`

	// Message provides provider-specific status information
	// +optional
	Message string `json:"message,omitempty"`
}

// VMNetworkAttachment condition types
const (
	// VMNetworkAttachmentConditionReady indicates whether the network is ready
	VMNetworkAttachmentConditionReady = "Ready"
	// VMNetworkAttachmentConditionConfiguring indicates whether the network is being configured
	VMNetworkAttachmentConditionConfiguring = "Configuring"
	// VMNetworkAttachmentConditionValidated indicates whether the network is validated
	VMNetworkAttachmentConditionValidated = "Validated"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
//+kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.network.type`
//+kubebuilder:printcolumn:name="Connected VMs",type=integer,JSONPath=`.status.connectedVMs`
//+kubebuilder:printcolumn:name="Providers",type=string,JSONPath=`.status.availableOn[*]`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
//+kubebuilder:resource:shortName=vmnet

// VMNetworkAttachment is the Schema for the vmnetworkattachments API
// +kubebuilder:storageversion
type VMNetworkAttachment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VMNetworkAttachmentSpec   `json:"spec,omitempty"`
	Status VMNetworkAttachmentStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// VMNetworkAttachmentList contains a list of VMNetworkAttachment
type VMNetworkAttachmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VMNetworkAttachment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VMNetworkAttachment{}, &VMNetworkAttachmentList{})
}
