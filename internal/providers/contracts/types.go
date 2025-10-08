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

package contracts

// VMClass defines VM resource allocation (provider-agnostic)
type VMClass struct {
	// CPU specifies the number of virtual CPUs
	CPU int32
	// MemoryMiB specifies memory in MiB
	MemoryMiB int32
	// Firmware specifies the firmware type (BIOS/UEFI)
	Firmware string
	// DiskDefaults provides default disk settings
	DiskDefaults *DiskDefaults
	// GuestToolsPolicy specifies guest tools policy
	GuestToolsPolicy string
	// ExtraConfig contains provider-specific configuration
	ExtraConfig map[string]string
	// PerformanceProfile defines performance-related settings
	PerformanceProfile *PerformanceProfile
	// SecurityProfile defines security-related settings
	SecurityProfile *SecurityProfile
	// ResourceLimits defines resource limits and reservations
	ResourceLimits *ResourceLimits
}

// VMImage defines the base template/image (provider-agnostic)
type VMImage struct {
	// TemplateName for vSphere templates
	TemplateName string
	// Path for local image files
	Path string
	// URL for remote images
	URL string
	// Format specifies image format
	Format string
	// Checksum for verification
	Checksum string
	// ChecksumType specifies algorithm
	ChecksumType string
}

// NetworkAttachment defines network configuration (provider-agnostic)
type NetworkAttachment struct {
	// Name identifies the network
	Name string
	// Portgroup for vSphere
	Portgroup string
	// NetworkName for generic networks
	NetworkName string
	// Bridge for bridge networks
	Bridge string
	// VLAN ID if applicable
	VLAN int32
	// Model specifies network device model
	Model string
	// MacAddress specifies static MAC
	MacAddress string
	// IPPolicy specifies IP assignment
	IPPolicy string
	// StaticIP for static assignments
	StaticIP string
}

// DiskSpec defines disk requirements (provider-agnostic)
type DiskSpec struct {
	// SizeGiB specifies disk size in GiB
	SizeGiB int32
	// Type specifies disk type (thin, thick, etc.)
	Type string
	// Name provides a name for the disk
	Name string
}

// DiskDefaults provides default disk settings
type DiskDefaults struct {
	// Type specifies the default disk type
	Type string
	// SizeGiB specifies the default root disk size
	SizeGiB int32
}

// UserData contains cloud-init/ignition configuration
type UserData struct {
	// CloudInitData contains the cloud-init configuration
	CloudInitData string
	// Type specifies the user data type (cloud-init, ignition, etc.)
	Type string
}

// Placement provides VM placement hints
type Placement struct {
	// Datastore specifies preferred datastore
	Datastore string
	// Cluster specifies preferred cluster
	Cluster string
	// Folder specifies preferred folder
	Folder string
	// Host specifies preferred host
	Host string
}

// TaskRef represents an asynchronous operation
type TaskRef struct {
	// ID is the task identifier
	ID string
	// Provider specifies which provider owns the task
	Provider string
	// Type specifies the operation type
	Type string
}

// PowerState represents VM power states
type PowerState string

const (
	// PowerStateOn indicates VM is powered on
	PowerStateOn PowerState = "On"
	// PowerStateOff indicates VM is powered off
	PowerStateOff PowerState = "Off"
	// PowerStateSuspended indicates VM is suspended
	PowerStateSuspended PowerState = "Suspended"
	// PowerStateUnknown indicates unknown state
	PowerStateUnknown PowerState = "Unknown"
)

// IPAddress represents an assigned IP address
type IPAddress struct {
	// IP is the IP address
	IP string
	// Type specifies the IP type (IPv4, IPv6)
	Type string
	// Source specifies how the IP was assigned (DHCP, static, etc.)
	Source string
}

// PerformanceProfile defines performance-related settings
type PerformanceProfile struct {
	// LatencySensitivity configures latency sensitivity
	LatencySensitivity string
	// CPUHotAddEnabled allows adding CPUs while VM is running
	CPUHotAddEnabled bool
	// MemoryHotAddEnabled allows adding memory while VM is running
	MemoryHotAddEnabled bool
	// VirtualizationBasedSecurity enables VBS features
	VirtualizationBasedSecurity bool
	// NestedVirtualization enables nested virtualization
	NestedVirtualization bool
	// HyperThreadingPolicy controls hyperthreading usage
	HyperThreadingPolicy string
}

// SecurityProfile defines security-related settings
type SecurityProfile struct {
	// SecureBoot enables secure boot functionality
	SecureBoot bool
	// TPMEnabled enables TPM (Trusted Platform Module)
	TPMEnabled bool
	// TPMVersion specifies the TPM version
	TPMVersion string
	// VTDEnabled enables Intel VT-d or AMD-Vi
	VTDEnabled bool
	// EncryptionEnabled indicates if encryption should be used
	EncryptionEnabled bool
	// KeyProvider specifies the encryption key provider
	KeyProvider string
	// RequireEncryption mandates encryption (fails if not available)
	RequireEncryption bool
}

// ResourceLimits defines resource limits and reservations
type ResourceLimits struct {
	// CPULimit is the maximum CPU usage limit (in MHz or percentage)
	CPULimit *int32
	// CPUReservation is the guaranteed CPU allocation (in MHz)
	CPUReservation *int32
	// MemoryLimitMiB is the maximum memory usage limit in MiB
	MemoryLimitMiB *int32
	// MemoryReservationMiB is the guaranteed memory allocation in MiB
	MemoryReservationMiB *int32
	// CPUShares defines the relative CPU priority (higher = more priority)
	CPUShares *int32
}
