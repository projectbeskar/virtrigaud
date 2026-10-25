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

// VMNetworkAttachmentSpec defines the desired state of VMNetworkAttachment. A
// VirtualMachine uses an attachment through spec.networks[].networkRef, in
// its own namespace. Only the spec.network block that matches the type of the
// VM's Provider is read; ipAllocation, security, qos and metadata are
// accepted but not acted on in this release.
type VMNetworkAttachmentSpec struct {
	// Network holds the per-hypervisor network settings. Only the block that
	// matches the type of the VM's Provider (vsphere, libvirt or proxmox) is
	// read; the other blocks are ignored.
	Network NetworkConfig `json:"network"`

	// IPAllocation defines how IP addresses are assigned. Accepted but not acted
	// on in this release. This applies to every field below it. Set a static IP,
	// prefix, gateway and DNS servers on the entry of the VirtualMachine's
	// spec.networks instead.
	// +optional
	IPAllocation *IPAllocationConfig `json:"ipAllocation,omitempty"`

	// Security defines network security settings. Accepted but not acted on in
	// this release. This applies to every field below it.
	// +optional
	Security *NetworkSecurityConfig `json:"security,omitempty"`

	// QoS defines Quality of Service settings. Accepted but not acted on in this
	// release. This applies to every field below it.
	// +optional
	QoS *NetworkQoSConfig `json:"qos,omitempty"`

	// Metadata contains a display name, description, environment and tags for
	// the network. Accepted but not acted on in this release. This applies to
	// every field below it; no controller reads them.
	// +optional
	Metadata *NetworkMetadata `json:"metadata,omitempty"`
}

// NetworkConfig holds the per-hypervisor network settings. The manager reads
// only the block that matches the type of the VM's Provider.
type NetworkConfig struct {
	// VSphere contains the settings used when the VM's Provider is a vSphere
	// Provider. Only portgroup is acted on.
	// +optional
	VSphere *VSphereNetworkConfig `json:"vsphere,omitempty"`

	// Libvirt contains the settings used when the VM's Provider is a libvirt
	// Provider. Only networkName, bridge.name and model are acted on.
	// +optional
	Libvirt *LibvirtNetworkConfig `json:"libvirt,omitempty"`

	// Proxmox contains the settings used when the VM's Provider is a Proxmox VE
	// Provider. Only bridge, model and vlanTag are acted on.
	// +optional
	Proxmox *ProxmoxNetworkConfig `json:"proxmox,omitempty"`

	// Type specifies the network type. Accepted but not acted on in this
	// release. The network a VM joins is set by the provider block (portgroup,
	// networkName or bridge).
	// +optional
	// +kubebuilder:default="bridged"
	// +kubebuilder:validation:Enum=bridged;nat;isolated;host-only;external
	Type NetworkType `json:"type,omitempty"`

	// MTU specifies the Maximum Transmission Unit. Accepted but not acted on in
	// this release (nor is proxmox.mtu).
	// +optional
	// +kubebuilder:validation:Minimum=68
	// +kubebuilder:validation:Maximum=9000
	// +kubebuilder:default=1500
	MTU *int32 `json:"mtu,omitempty"`
}

// NetworkType represents the type of network. Accepted but not acted on in
// this release.
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

// VSphereNetworkConfig defines vSphere-specific network settings. Only
// portgroup is acted on: the vSphere provider attaches the VM's first network
// (the first entry of spec.networks) to the network of that name with a
// vmxnet3 adapter. The other fields are accepted but not acted on in this
// release.
type VSphereNetworkConfig struct {
	// Portgroup is the name of the vSphere network (a standard or distributed
	// portgroup) that the VM's network adapter is attached to. The vSphere
	// provider uses it for the first entry of the VM's spec.networks only.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Portgroup string `json:"portgroup,omitempty"`

	// DistributedSwitch specifies the distributed virtual switch. Accepted but
	// not acted on in this release. This applies to every field below it.
	// +optional
	DistributedSwitch *DistributedSwitchConfig `json:"distributedSwitch,omitempty"`

	// VLAN specifies the VLAN configuration. Accepted but not acted on in this
	// release. This applies to every field below it: the manager forwards
	// vlan.vlanID, but the vSphere provider does not use it, so the VLAN is the
	// one the portgroup is configured with.
	// +optional
	VLAN *VLANConfig `json:"vlan,omitempty"`

	// Security defines portgroup security settings. Accepted but not acted on in
	// this release. This applies to every field below it.
	// +optional
	Security *PortgroupSecurityConfig `json:"security,omitempty"`

	// TrafficShaping defines traffic shaping settings. Accepted but not acted on
	// in this release. This applies to every field below it.
	// +optional
	TrafficShaping *TrafficShapingConfig `json:"trafficShaping,omitempty"`

	// PCISlotNumber specifies the PCI slot number for the network adapter.
	// Accepted but not acted on in this release. The manager forwards it to the
	// provider, but the vSphere provider does not use it, so it does not control
	// the guest's interface name.
	// +optional
	// +kubebuilder:validation:Minimum=32
	// +kubebuilder:validation:Maximum=1024
	PCISlotNumber *int32 `json:"pciSlotNumber,omitempty"`

	// AdapterType specifies the network adapter type. Accepted but not acted on
	// in this release. The vSphere provider always creates a vmxnet3 adapter.
	// +optional
	// +kubebuilder:default="vmxnet3"
	// +kubebuilder:validation:Enum=vmxnet3;e1000;e1000e
	AdapterType string `json:"adapterType,omitempty"`
}

// DistributedSwitchConfig defines distributed virtual switch configuration.
// Accepted but not acted on in this release.
type DistributedSwitchConfig struct {
	// Name is the name of the distributed switch. Accepted but not acted on in
	// this release.
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`

	// UUID is the UUID of the distributed switch (optional). Accepted but not
	// acted on in this release.
	// +optional
	UUID string `json:"uuid,omitempty"`

	// PortgroupType specifies the type of portgroup. Accepted but not acted on
	// in this release.
	// +optional
	// +kubebuilder:validation:Enum=ephemeral;distributed
	PortgroupType string `json:"portgroupType,omitempty"`
}

// VLANConfig defines VLAN configuration. Accepted but not acted on in this
// release.
type VLANConfig struct {
	// Type specifies the VLAN type. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default="none"
	// +kubebuilder:validation:Enum=none;vlan;pvlan;trunk
	Type string `json:"type,omitempty"`

	// VlanID specifies the VLAN ID for VLAN type. Accepted but not acted on in
	// this release. The manager forwards it, but the vSphere provider ignores
	// it.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4094
	VlanID *int32 `json:"vlanID,omitempty"`

	// TrunkVlanIDs specifies VLAN IDs for trunk type. Accepted but not acted on
	// in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=100
	TrunkVlanIDs []int32 `json:"trunkVlanIDs,omitempty"`

	// PrimaryVlanID specifies the primary VLAN ID for PVLAN. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4094
	PrimaryVlanID *int32 `json:"primaryVlanID,omitempty"`

	// SecondaryVlanID specifies the secondary VLAN ID for PVLAN. Accepted but
	// not acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4094
	SecondaryVlanID *int32 `json:"secondaryVlanID,omitempty"`
}

// PortgroupSecurityConfig defines portgroup security settings. Accepted but
// not acted on in this release.
type PortgroupSecurityConfig struct {
	// AllowPromiscuous allows promiscuous mode. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:default=false
	AllowPromiscuous *bool `json:"allowPromiscuous,omitempty"`

	// AllowMACChanges allows MAC address changes. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:default=true
	AllowMACChanges *bool `json:"allowMACChanges,omitempty"`

	// AllowForgedTransmits allows forged transmits. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:default=true
	AllowForgedTransmits *bool `json:"allowForgedTransmits,omitempty"`
}

// TrafficShapingConfig defines traffic shaping settings. Accepted but not
// acted on in this release.
type TrafficShapingConfig struct {
	// Enabled indicates if traffic shaping is enabled. Accepted but not acted on
	// in this release.
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// AverageBandwidth is the average bandwidth in bits per second. Accepted but
	// not acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=1000
	AverageBandwidth *int64 `json:"averageBandwidth,omitempty"`

	// PeakBandwidth is the peak bandwidth in bits per second. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=1000
	PeakBandwidth *int64 `json:"peakBandwidth,omitempty"`

	// BurstSize is the burst size in bytes. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:Minimum=1024
	BurstSize *int64 `json:"burstSize,omitempty"`
}

// LibvirtNetworkConfig defines libvirt-specific network settings. Only
// networkName, bridge.name and model are acted on; driver, filterRef,
// bridge.stp and bridge.delay are accepted but not acted on in this release.
// On a clustered Provider (spec.topology: cluster) the scheduler also
// requires a Host that carries the label net.virtrigaud.io/<name> set to
// "true", where <name> is networkName, or bridge.name when networkName is
// empty.
type LibvirtNetworkConfig struct {
	// NetworkName is the name of the libvirt network (for example default) that
	// the NIC attaches to. It is used only when bridge is not set: a bridge
	// takes precedence.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern="^[^<>&'\"/\\x00-\\x1f]+$"
	NetworkName string `json:"networkName,omitempty"`

	// Bridge attaches the NIC to a host bridge instead of a libvirt network.
	// Only bridge.name is acted on. Set either bridge or networkName: when both
	// are set, the libvirt provider uses the bridge.
	// +optional
	Bridge *BridgeConfig `json:"bridge,omitempty"`

	// Model is the NIC model of the libvirt interface. The libvirt provider uses
	// virtio when it is empty.
	// +optional
	// +kubebuilder:default="virtio"
	// +kubebuilder:validation:Enum=virtio;e1000;e1000e;rtl8139;pcnet;ne2k_pci
	Model string `json:"model,omitempty"`

	// Driver specifies the network driver configuration. Accepted but not acted
	// on in this release. This applies to every field below it.
	// +optional
	Driver *NetworkDriverConfig `json:"driver,omitempty"`

	// FilterRef specifies network filter configuration. Accepted but not acted
	// on in this release. This applies to every field below it.
	// +optional
	FilterRef *NetworkFilterRef `json:"filterRef,omitempty"`
}

// BridgeConfig defines bridge configuration. Only name is acted on; stp and
// delay are accepted but not acted on in this release.
type BridgeConfig struct {
	// Name is the name of the host bridge that the NIC attaches to.
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:Pattern="^[^<>&'\"/\\x00-\\x1f]+$"
	Name string `json:"name"`

	// STP enables Spanning Tree Protocol. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:default=false
	STP bool `json:"stp,omitempty"`

	// Delay is the STP forwarding delay. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=30
	Delay *int32 `json:"delay,omitempty"`
}

// NetworkDriverConfig defines network driver configuration. Accepted but not
// acted on in this release.
type NetworkDriverConfig struct {
	// Name is the driver name. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:Enum=kvm;vfio;uio
	Name string `json:"name,omitempty"`

	// Queues specifies the number of queues. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16
	Queues *int32 `json:"queues,omitempty"`

	// TxMode specifies the TX mode. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:Enum=iothread;timer
	TxMode string `json:"txMode,omitempty"`

	// IOEventFD enables IO event file descriptor. Accepted but not acted on in
	// this release.
	// +optional
	IOEventFD *bool `json:"ioEventFD,omitempty"`

	// EventIDX enables event index. Accepted but not acted on in this release.
	// +optional
	EventIDX *bool `json:"eventIDX,omitempty"`
}

// NetworkFilterRef references a network filter. Accepted but not acted on in
// this release.
type NetworkFilterRef struct {
	// Filter is the filter name. Accepted but not acted on in this release.
	// +kubebuilder:validation:MaxLength=255
	Filter string `json:"filter"`

	// Parameters contains filter parameters. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:MaxProperties=20
	Parameters map[string]string `json:"parameters,omitempty"`
}

// IPAllocationConfig defines IP address allocation settings. Accepted but not
// acted on in this release.
type IPAllocationConfig struct {
	// Type specifies the IP allocation type. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:default="DHCP"
	// +kubebuilder:validation:Enum=DHCP;Static;Pool;None
	Type IPAllocationType `json:"type,omitempty"`

	// StaticConfig contains static IP configuration. Accepted but not acted on
	// in this release.
	// +optional
	StaticConfig *StaticIPConfig `json:"staticConfig,omitempty"`

	// PoolConfig contains IP pool configuration. Accepted but not acted on in
	// this release.
	// +optional
	PoolConfig *IPPoolConfig `json:"poolConfig,omitempty"`

	// DHCPConfig contains DHCP configuration. Accepted but not acted on in this
	// release.
	// +optional
	DHCPConfig *DHCPConfig `json:"dhcpConfig,omitempty"`

	// DNSConfig contains DNS configuration. Accepted but not acted on in this
	// release.
	// +optional
	DNSConfig *DNSConfig `json:"dnsConfig,omitempty"`
}

// IPAllocationType represents the type of IP allocation. Accepted but not
// acted on in this release.
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

// StaticIPConfig defines static IP configuration. Accepted but not acted on
// in this release.
type StaticIPConfig struct {
	// Address is the static IP address (CIDR notation). Accepted but not acted
	// on in this release.
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)/([0-9]|[1-2][0-9]|3[0-2])$"
	Address string `json:"address"`

	// Gateway is the default gateway. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$"
	Gateway string `json:"gateway,omitempty"`

	// Routes contains static routes. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Routes []StaticRoute `json:"routes,omitempty"`
}

// StaticRoute defines a static route. Accepted but not acted on in this
// release.
type StaticRoute struct {
	// Destination is the destination network (CIDR notation). Accepted but not
	// acted on in this release.
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)/([0-9]|[1-2][0-9]|3[0-2])$"
	Destination string `json:"destination"`

	// Gateway is the route gateway. Accepted but not acted on in this release.
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$"
	Gateway string `json:"gateway"`

	// Metric is the route metric. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Metric *int32 `json:"metric,omitempty"`
}

// IPPoolConfig defines IP pool configuration. Accepted but not acted on in
// this release.
type IPPoolConfig struct {
	// PoolRef references an IP pool resource. Accepted but not acted on in this
	// release.
	PoolRef LocalObjectReference `json:"poolRef"`

	// PreferredIP requests a preferred IP from the pool. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$"
	PreferredIP string `json:"preferredIP,omitempty"`
}

// DHCPConfig defines DHCP configuration. Accepted but not acted on in this
// release.
type DHCPConfig struct {
	// ClientID specifies the DHCP client ID. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	ClientID string `json:"clientID,omitempty"`

	// Hostname specifies the hostname to request. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Hostname string `json:"hostname,omitempty"`

	// Options contains DHCP options. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxProperties=20
	Options map[string]string `json:"options,omitempty"`
}

// DNSConfig defines DNS configuration. Accepted but not acted on in this
// release.
type DNSConfig struct {
	// Servers contains DNS server addresses. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	Servers []string `json:"servers,omitempty"`

	// SearchDomains contains DNS search domains. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	SearchDomains []string `json:"searchDomains,omitempty"`

	// Options contains DNS resolver options. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:MaxProperties=10
	Options map[string]string `json:"options,omitempty"`
}

// NetworkSecurityConfig defines network security settings. Accepted but not
// acted on in this release.
type NetworkSecurityConfig struct {
	// Firewall contains firewall configuration. Accepted but not acted on in
	// this release.
	// +optional
	Firewall *FirewallConfig `json:"firewall,omitempty"`

	// Isolation contains network isolation settings. Accepted but not acted on
	// in this release.
	// +optional
	Isolation *NetworkIsolationConfig `json:"isolation,omitempty"`

	// Encryption contains network encryption settings. Accepted but not acted on
	// in this release.
	// +optional
	Encryption *NetworkEncryptionConfig `json:"encryption,omitempty"`
}

// FirewallConfig defines firewall configuration. Accepted but not acted on in
// this release.
type FirewallConfig struct {
	// Enabled indicates if firewall is enabled. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// DefaultPolicy specifies the default firewall policy. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:default="ACCEPT"
	// +kubebuilder:validation:Enum=ACCEPT;DROP;REJECT
	DefaultPolicy string `json:"defaultPolicy,omitempty"`

	// Rules contains firewall rules. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=100
	Rules []FirewallRule `json:"rules,omitempty"`
}

// FirewallRule defines a firewall rule. Accepted but not acted on in this
// release.
type FirewallRule struct {
	// Name is the rule name. Accepted but not acted on in this release.
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`

	// Action specifies the rule action. Accepted but not acted on in this
	// release.
	// +kubebuilder:validation:Enum=ACCEPT;DROP;REJECT
	Action string `json:"action"`

	// Direction specifies the traffic direction. Accepted but not acted on in
	// this release.
	// +kubebuilder:validation:Enum=in;out;inout
	Direction string `json:"direction"`

	// Protocol specifies the protocol. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:Enum=tcp;udp;icmp;all
	Protocol string `json:"protocol,omitempty"`

	// SourceCIDR specifies the source CIDR. Accepted but not acted on in this
	// release.
	// +optional
	SourceCIDR string `json:"sourceCIDR,omitempty"`

	// DestinationCIDR specifies the destination CIDR. Accepted but not acted on
	// in this release.
	// +optional
	DestinationCIDR string `json:"destinationCIDR,omitempty"`

	// Ports specifies the port range. Accepted but not acted on in this release.
	// +optional
	Ports *PortRange `json:"ports,omitempty"`

	// Priority specifies the rule priority. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	Priority *int32 `json:"priority,omitempty"`
}

// PortRange defines a port range. Accepted but not acted on in this release.
type PortRange struct {
	// Start is the starting port. Accepted but not acted on in this release.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Start int32 `json:"start"`

	// End is the ending port (optional, defaults to start). Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	End *int32 `json:"end,omitempty"`
}

// NetworkIsolationConfig defines network isolation settings. Accepted but not
// acted on in this release.
type NetworkIsolationConfig struct {
	// Mode specifies the isolation mode. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:Enum=none;strict;custom
	Mode string `json:"mode,omitempty"`

	// AllowedNetworks contains allowed network CIDRs. Accepted but not acted on
	// in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	AllowedNetworks []string `json:"allowedNetworks,omitempty"`

	// DeniedNetworks contains denied network CIDRs. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	DeniedNetworks []string `json:"deniedNetworks,omitempty"`
}

// NetworkEncryptionConfig defines network encryption settings. Accepted but
// not acted on in this release.
type NetworkEncryptionConfig struct {
	// Enabled indicates if encryption is enabled. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// Protocol specifies the encryption protocol. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:Enum=ipsec;wireguard;openvpn
	Protocol string `json:"protocol,omitempty"`

	// KeyRef references encryption keys. Accepted but not acted on in this
	// release.
	// +optional
	KeyRef *LocalObjectReference `json:"keyRef,omitempty"`
}

// NetworkQoSConfig defines Quality of Service settings. Accepted but not
// acted on in this release.
type NetworkQoSConfig struct {
	// IngressLimit limits inbound traffic in bits per second. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=1000
	IngressLimit *int64 `json:"ingressLimit,omitempty"`

	// EgressLimit limits outbound traffic in bits per second. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=1000
	EgressLimit *int64 `json:"egressLimit,omitempty"`

	// Priority specifies traffic priority. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=7
	Priority *int32 `json:"priority,omitempty"`

	// DSCP specifies DSCP marking. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=63
	DSCP *int32 `json:"dscp,omitempty"`
}

// NetworkMetadata contains network metadata and labels. Accepted but not
// acted on in this release.
type NetworkMetadata struct {
	// DisplayName is a human-readable name. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	DisplayName string `json:"displayName,omitempty"`

	// Description provides a description. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// Environment specifies the environment (dev, staging, prod, qa, uat).
	// Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:Enum=dev;staging;prod;test;qa;uat
	Environment string `json:"environment,omitempty"`

	// Tags are key-value pairs for categorizing. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Tags map[string]string `json:"tags,omitempty"`
}

// VMNetworkAttachmentStatus defines the observed state of
// VMNetworkAttachment. Not populated in this release: the controller for this
// resource only watches it and writes no status.
type VMNetworkAttachmentStatus struct {
	// Ready indicates if the network is ready for use. Not populated in this
	// release.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Phase represents the current phase. Not populated in this release.
	// +optional
	Phase NetworkAttachmentPhase `json:"phase,omitempty"`

	// Message provides additional details about the current state. Not populated
	// in this release.
	// +optional
	Message string `json:"message,omitempty"`

	// AvailableOn lists the providers where the network is available. Not
	// populated in this release.
	// +optional
	AvailableOn []string `json:"availableOn,omitempty"`

	// Conditions represent the latest available observations. Not populated in
	// this release.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration reflects the generation observed by the controller. Not
	// populated in this release.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ConnectedVMs is the number of VMs using this network. Not populated in
	// this release.
	// +optional
	ConnectedVMs int32 `json:"connectedVMs,omitempty"`

	// IPAllocations contains current IP allocations. Not populated in this
	// release.
	// +optional
	IPAllocations []IPAllocation `json:"ipAllocations,omitempty"`

	// ProviderStatus contains provider-specific status information. Not
	// populated in this release.
	// +optional
	ProviderStatus map[string]ProviderNetworkStatus `json:"providerStatus,omitempty"`
}

// NetworkAttachmentPhase represents the phase of network attachment. No
// controller sets it in this release.
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

// IPAllocation represents an IP allocation. Not populated in this release.
type IPAllocation struct {
	// VM is the VM name that has the IP allocated. Not populated in this
	// release.
	VM string `json:"vm"`

	// IP is the allocated IP address. Not populated in this release.
	IP string `json:"ip"`

	// MAC is the allocated MAC address. Not populated in this release.
	// +optional
	MAC string `json:"mac,omitempty"`

	// AllocatedAt is when the IP was allocated. Not populated in this release.
	// +optional
	AllocatedAt *metav1.Time `json:"allocatedAt,omitempty"`

	// LeaseExpiry is when the IP lease expires (for DHCP). Not populated in this
	// release.
	// +optional
	LeaseExpiry *metav1.Time `json:"leaseExpiry,omitempty"`
}

// ProviderNetworkStatus contains provider-specific network status. Not
// populated in this release.
type ProviderNetworkStatus struct {
	// Available indicates if the network is available on this provider. Not
	// populated in this release.
	Available bool `json:"available"`

	// ID is the provider-specific network identifier. Not populated in this
	// release.
	// +optional
	ID string `json:"id,omitempty"`

	// State is the provider-specific network state. Not populated in this
	// release.
	// +optional
	State string `json:"state,omitempty"`

	// LastUpdated is when the status was last updated. Not populated in this
	// release.
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`

	// Message provides provider-specific status information. Not populated in
	// this release.
	// +optional
	Message string `json:"message,omitempty"`
}

// VMNetworkAttachment condition types. No controller sets these in this
// release.
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

// VMNetworkAttachment describes a network that VirtualMachines attach to
// through spec.networks[].networkRef. The manager reads spec.network when it
// creates a VM; no controller acts on the rest of the resource, and its
// status is not populated, so the Ready, Connected VMs and Providers columns
// are empty.
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

// ProxmoxNetworkConfig defines Proxmox VE-specific network settings. bridge,
// model and vlanTag are acted on (see docs/proxmox-networks.md); firewall,
// rateLimit and mtu are accepted but not acted on in this release.
type ProxmoxNetworkConfig struct {
	// Bridge is the Linux bridge the NIC attaches to, for example vmbr0 or
	// vmbr1. The CRD accepts names of the form vmbr<N> only, although the
	// Proxmox provider accepts any Linux bridge name. When it is empty a new VM
	// gets vmbr0 and a VM cloned from a template keeps the template's bridge.
	// +optional
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:Pattern="^vmbr[0-9]+$"
	Bridge string `json:"bridge,omitempty"`

	// Model is the NIC model. It defaults to virtio.
	// +optional
	// +kubebuilder:default="virtio"
	// +kubebuilder:validation:Enum=virtio;e1000;rtl8139;vmxnet3
	Model string `json:"model,omitempty"`

	// VLANTag is the VLAN tag set on the NIC. When bridge is also set, the NIC
	// gets exactly this tag, or none when it is empty.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4094
	VLANTag *int32 `json:"vlanTag,omitempty"`

	// Firewall enables the Proxmox firewall for this interface. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:default=false
	Firewall *bool `json:"firewall,omitempty"`

	// RateLimit specifies the bandwidth limit in MB/s. Accepted but not acted on
	// in this release.
	// +optional
	// +kubebuilder:validation:Minimum=1
	RateLimit *int32 `json:"rateLimit,omitempty"`

	// MTU specifies the Maximum Transmission Unit. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:Minimum=68
	// +kubebuilder:validation:Maximum=65520
	MTU *int32 `json:"mtu,omitempty"`
}

func init() {
	SchemeBuilder.Register(&VMNetworkAttachment{}, &VMNetworkAttachmentList{})
}
