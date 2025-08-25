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

// VMClassSpec defines the desired state of VMClass
type VMClassSpec struct {
	// CPU specifies the number of virtual CPUs
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	CPU int32 `json:"cpu"`

	// Memory specifies memory allocation using Kubernetes resource quantities
	Memory resource.Quantity `json:"memory"`

	// Firmware specifies the firmware type
	// +optional
	// +kubebuilder:default="BIOS"
	// +kubebuilder:validation:Enum=BIOS;UEFI;EFI
	Firmware FirmwareType `json:"firmware,omitempty"`

	// DiskDefaults provides default disk settings
	// +optional
	DiskDefaults *DiskDefaults `json:"diskDefaults,omitempty"`

	// GuestToolsPolicy specifies guest tools installation policy
	// +optional
	// +kubebuilder:default="install"
	GuestToolsPolicy GuestToolsPolicy `json:"guestToolsPolicy,omitempty"`

	// ExtraConfig contains provider-specific extra configuration
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	ExtraConfig map[string]string `json:"extraConfig,omitempty"`

	// ResourceLimits defines resource limits and reservations
	// +optional
	ResourceLimits *VMResourceLimits `json:"resourceLimits,omitempty"`

	// PerformanceProfile defines performance-related settings
	// +optional
	PerformanceProfile *PerformanceProfile `json:"performanceProfile,omitempty"`

	// SecurityProfile defines security-related settings
	// +optional
	SecurityProfile *SecurityProfile `json:"securityProfile,omitempty"`
}

// FirmwareType represents the firmware type for VMs
// +kubebuilder:validation:Enum=BIOS;UEFI;EFI
type FirmwareType string

const (
	// FirmwareTypeBIOS indicates BIOS firmware
	FirmwareTypeBIOS FirmwareType = "BIOS"
	// FirmwareTypeUEFI indicates UEFI firmware
	FirmwareTypeUEFI FirmwareType = "UEFI"
	// FirmwareTypeEFI indicates EFI firmware (alias for UEFI)
	FirmwareTypeEFI FirmwareType = "EFI"
)

// GuestToolsPolicy represents the guest tools installation policy
// +kubebuilder:validation:Enum=install;skip;upgrade;uninstall
type GuestToolsPolicy string

const (
	// GuestToolsPolicyInstall installs guest tools if not present
	GuestToolsPolicyInstall GuestToolsPolicy = "install"
	// GuestToolsPolicySkip skips guest tools installation
	GuestToolsPolicySkip GuestToolsPolicy = "skip"
	// GuestToolsPolicyUpgrade upgrades guest tools if present
	GuestToolsPolicyUpgrade GuestToolsPolicy = "upgrade"
	// GuestToolsPolicyUninstall removes guest tools if present
	GuestToolsPolicyUninstall GuestToolsPolicy = "uninstall"
)

// VMResourceLimits defines resource limits and reservations
type VMResourceLimits struct {
	// CPULimit is the maximum CPU usage limit (in MHz or percentage)
	// +optional
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=100000
	CPULimit *int32 `json:"cpuLimit,omitempty"`

	// CPUReservation is the guaranteed CPU allocation (in MHz)
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100000
	CPUReservation *int32 `json:"cpuReservation,omitempty"`

	// MemoryLimit is the maximum memory usage limit
	// +optional
	MemoryLimit *resource.Quantity `json:"memoryLimit,omitempty"`

	// MemoryReservation is the guaranteed memory allocation
	// +optional
	MemoryReservation *resource.Quantity `json:"memoryReservation,omitempty"`

	// CPUShares defines the relative CPU priority (higher = more priority)
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	CPUShares *int32 `json:"cpuShares,omitempty"`
}

// PerformanceProfile defines performance-related settings
type PerformanceProfile struct {
	// LatencySensitivity configures latency sensitivity
	// +optional
	// +kubebuilder:default="normal"
	// +kubebuilder:validation:Enum=low;normal;high
	LatencySensitivity string `json:"latencySensitivity,omitempty"`

	// CPUHotAddEnabled allows adding CPUs while VM is running
	// +optional
	// +kubebuilder:default=false
	CPUHotAddEnabled bool `json:"cpuHotAddEnabled,omitempty"`

	// MemoryHotAddEnabled allows adding memory while VM is running
	// +optional
	// +kubebuilder:default=false
	MemoryHotAddEnabled bool `json:"memoryHotAddEnabled,omitempty"`

	// VirtualizationBasedSecurity enables VBS features
	// +optional
	// +kubebuilder:default=false
	VirtualizationBasedSecurity bool `json:"virtualizationBasedSecurity,omitempty"`

	// NestedVirtualization enables nested virtualization
	// +optional
	// +kubebuilder:default=false
	NestedVirtualization bool `json:"nestedVirtualization,omitempty"`

	// HyperThreadingPolicy controls hyperthreading usage
	// +optional
	// +kubebuilder:default="auto"
	// +kubebuilder:validation:Enum=auto;prefer;avoid;require
	HyperThreadingPolicy string `json:"hyperThreadingPolicy,omitempty"`
}

// SecurityProfile defines security-related settings
type SecurityProfile struct {
	// SecureBoot enables secure boot functionality
	// +optional
	// +kubebuilder:default=false
	SecureBoot bool `json:"secureBoot,omitempty"`

	// TPMEnabled enables TPM (Trusted Platform Module)
	// +optional
	// +kubebuilder:default=false
	TPMEnabled bool `json:"tpmEnabled,omitempty"`

	// TPMVersion specifies the TPM version
	// +optional
	// +kubebuilder:validation:Enum=1.2;2.0
	TPMVersion string `json:"tpmVersion,omitempty"`

	// VTDEnabled enables Intel VT-d or AMD-Vi
	// +optional
	// +kubebuilder:default=false
	VTDEnabled bool `json:"vtdEnabled,omitempty"`

	// EncryptionPolicy defines VM encryption settings
	// +optional
	EncryptionPolicy *EncryptionPolicy `json:"encryptionPolicy,omitempty"`
}

// EncryptionPolicy defines VM encryption settings
type EncryptionPolicy struct {
	// Enabled indicates if encryption should be used
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// KeyProvider specifies the encryption key provider
	// +optional
	// +kubebuilder:validation:Enum=standard;hardware;external
	KeyProvider string `json:"keyProvider,omitempty"`

	// RequireEncryption mandates encryption (fails if not available)
	// +optional
	// +kubebuilder:default=false
	RequireEncryption bool `json:"requireEncryption,omitempty"`
}

// DiskDefaults provides default disk settings
type DiskDefaults struct {
	// Type specifies the default disk type
	// +optional
	// +kubebuilder:default="thin"
	Type DiskType `json:"type,omitempty"`

	// Size specifies the default root disk size
	// +optional
	// +kubebuilder:default="40Gi"
	Size resource.Quantity `json:"size,omitempty"`

	// IOPS specifies the default IOPS limit
	// +optional
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=100000
	IOPS *int32 `json:"iops,omitempty"`

	// StorageClass specifies the default storage class
	// +optional
	// +kubebuilder:validation:MaxLength=253
	StorageClass string `json:"storageClass,omitempty"`
}

// DiskType represents the type of disk provisioning
// +kubebuilder:validation:Enum=thin;thick;eagerzeroedthick;ssd;hdd;nvme
type DiskType string

const (
	// DiskTypeThin indicates thin provisioned disks
	DiskTypeThin DiskType = "thin"
	// DiskTypeThick indicates thick provisioned disks
	DiskTypeThick DiskType = "thick"
	// DiskTypeEagerZeroedThick indicates eager zeroed thick provisioned disks
	DiskTypeEagerZeroedThick DiskType = "eagerzeroedthick"
	// DiskTypeSSD indicates SSD storage
	DiskTypeSSD DiskType = "ssd"
	// DiskTypeHDD indicates HDD storage
	DiskTypeHDD DiskType = "hdd"
	// DiskTypeNVMe indicates NVMe storage
	DiskTypeNVMe DiskType = "nvme"
)

// VMClassStatus defines the observed state of VMClass
type VMClassStatus struct {
	// Conditions represent the latest available observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration reflects the generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// UsedByVMs is the number of VMs currently using this class
	// +optional
	UsedByVMs int32 `json:"usedByVMs,omitempty"`

	// SupportedProviders lists the providers that support this class
	// +optional
	SupportedProviders []string `json:"supportedProviders,omitempty"`

	// ValidationResults contains validation results for different providers
	// +optional
	ValidationResults map[string]ValidationResult `json:"validationResults,omitempty"`
}

// ValidationResult represents a validation result for a provider
type ValidationResult struct {
	// Valid indicates if the class is valid for the provider
	Valid bool `json:"valid"`

	// Message provides details about the validation result
	// +optional
	Message string `json:"message,omitempty"`

	// Warnings lists any validation warnings
	// +optional
	Warnings []string `json:"warnings,omitempty"`

	// LastValidated is when this validation was last performed
	// +optional
	LastValidated *metav1.Time `json:"lastValidated,omitempty"`
}

// VMClass condition types
const (
	// VMClassConditionReady indicates whether the class is ready
	VMClassConditionReady = "Ready"
	// VMClassConditionValidated indicates whether the class is validated
	VMClassConditionValidated = "Validated"
	// VMClassConditionSupported indicates whether the class is supported by providers
	VMClassConditionSupported = "Supported"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="CPU",type=integer,JSONPath=`.spec.cpu`
//+kubebuilder:printcolumn:name="Memory",type=string,JSONPath=`.spec.memory`
//+kubebuilder:printcolumn:name="Firmware",type=string,JSONPath=`.spec.firmware`
//+kubebuilder:printcolumn:name="Used By",type=integer,JSONPath=`.status.usedByVMs`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
//+kubebuilder:resource:shortName=vmclass

// VMClass is the Schema for the vmclasses API
type VMClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VMClassSpec   `json:"spec,omitempty"`
	Status VMClassStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// VMClassList contains a list of VMClass
type VMClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VMClass `json:"items"`
}

// Hub marks this version as the conversion hub
func (*VMClass) Hub() {}

func init() {
	SchemeBuilder.Register(&VMClass{}, &VMClassList{})
}
