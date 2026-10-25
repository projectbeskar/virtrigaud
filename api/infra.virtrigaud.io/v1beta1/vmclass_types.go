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

// VMClassSpec defines the desired state of VMClass. A VMClass is a reusable
// hardware profile that VirtualMachines name in spec.classRef. cpu and
// memory are acted on by every provider; the other fields are acted on only
// by the providers named in their descriptions, and some are accepted but
// not acted on in this release.
type VMClassSpec struct {
	// CPU specifies the number of virtual CPUs. A VirtualMachine's
	// spec.resources.cpu overrides it for that VirtualMachine. On a clustered
	// Provider it is also the CPU the VirtualMachine takes from its host.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	CPU int32 `json:"cpu"`

	// Memory specifies memory allocation using Kubernetes resource
	// quantities. A VirtualMachine's spec.resources.memoryMiB overrides it
	// for that VirtualMachine. It must be non-negative and below 100Ti: every
	// value below 90Ti written in bytes, k/Ki, M/Mi, G/Gi or T/Ti is
	// accepted, and nothing at or above 100Ti is. That is more than three
	// times the largest VM any supported hypervisor runs (24 TB on vSphere
	// 8), and keeps the MiB value the provider contract carries far inside
	// its int32 range. The rules bound the number of integer digits per unit
	// suffix with core CEL only (no quantity library, which Kubernetes < 1.29
	// lacks), so they are enforced on every supported Kubernetes version.
	// +kubebuilder:validation:XValidation:rule="type(self) == int ? (self >= 0 && self < 100000000000000) : self.matches('^[+]?[0-9.]*(k|Ki|Mi?|Gi?|Ti?|[mun]|[eE]([+]?0*(1[0-3]|[0-9])|-[0-9]+))?$')",message="memory must be a non-negative quantity below 100Ti"
	// +kubebuilder:validation:XValidation:rule="type(self) == int || ((self.matches('[a-zA-Z]') || !self.matches('^[+]?0*[1-9][0-9]{14}')) && (!self.matches('(k|Ki)$') || !self.matches('^[+]?0*[1-9][0-9]{11}')) && (!self.matches('Mi?$') || !self.matches('^[+]?0*[1-9][0-9]{8}')))",message="memory must be a non-negative quantity below 100Ti"
	// +kubebuilder:validation:XValidation:rule="type(self) == int || ((!self.matches('Gi?$') || !self.matches('^[+]?0*[1-9][0-9]{5}')) && (!self.matches('Ti?$') || !self.matches('^[+]?0*[1-9][0-9]{2}')) && (!self.matches('[mun]$') || !self.matches('^[+]?0*[1-9][0-9]{17}')))",message="memory must be a non-negative quantity below 100Ti"
	// +kubebuilder:validation:XValidation:rule="type(self) == int || ((!self.matches('[eE][+]?0*[0-9]$') || !self.matches('^[+]?0*[1-9][0-9]{5}')) && (!self.matches('[eE][+]?0*1[0-3]$') || !self.matches('^[+]?0*[1-9][0-9]')) && (!self.matches('[eE]-') || !self.matches('^[+]?0*[1-9][0-9]{14}')))",message="memory must be a non-negative quantity below 100Ti"
	Memory resource.Quantity `json:"memory"`

	// Firmware specifies the firmware type: BIOS (the default) or UEFI. Only
	// the vSphere provider reads it: UEFI selects EFI firmware and any other
	// value selects BIOS, except that securityProfile.secureBoot or
	// tpmEnabled force EFI firmware. The libvirt provider selects UEFI
	// firmware only through securityProfile.secureBoot; Proxmox and mock
	// ignore this field.
	// +optional
	// +kubebuilder:default="BIOS"
	// +kubebuilder:validation:Enum=BIOS;UEFI;EFI
	Firmware FirmwareType `json:"firmware,omitempty"`

	// DiskDefaults provides default disk settings. Only size is acted on:
	// libvirt creates the root disk at that size (20GiB when diskDefaults is
	// unset) and vSphere grows the root disk to it after creation (it never
	// shrinks a disk). type, iops and storageClass are accepted but not acted
	// on in this release. Proxmox and mock ignore the block.
	// +optional
	DiskDefaults *DiskDefaults `json:"diskDefaults,omitempty"`

	// GuestToolsPolicy specifies guest tools installation policy. Accepted
	// but not acted on in this release. The value is sent to the provider,
	// but no provider acts on it.
	// +optional
	// +kubebuilder:default="install"
	GuestToolsPolicy GuestToolsPolicy `json:"guestToolsPolicy,omitempty"`

	// ExtraConfig contains provider-specific extra configuration. Only the
	// vSphere provider reads it, and only the key vsphere.hardwareVersion
	// (the VM hardware version, for example 21). Any other key, and every key
	// on libvirt, Proxmox and mock, is accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	ExtraConfig map[string]string `json:"extraConfig,omitempty"`

	// ResourceLimits defines resource limits and reservations. Accepted but
	// not acted on in this release. The values are sent to the provider, but
	// no provider applies them.
	// +optional
	ResourceLimits *VMResourceLimits `json:"resourceLimits,omitempty"`

	// PerformanceProfile defines performance-related settings.
	// nestedVirtualization, cpuHotAddEnabled and memoryHotAddEnabled are
	// acted on by vSphere and libvirt, and virtualizationBasedSecurity by
	// vSphere only; latencySensitivity and hyperThreadingPolicy are accepted
	// but not acted on in this release. Proxmox and mock ignore the block.
	// +optional
	PerformanceProfile *PerformanceProfile `json:"performanceProfile,omitempty"`

	// SecurityProfile defines security-related settings. secureBoot,
	// tpmEnabled and vtdEnabled are acted on by vSphere and libvirt;
	// tpmVersion and encryptionPolicy are accepted but not acted on in this
	// release. Proxmox and mock ignore the block.
	// +optional
	SecurityProfile *SecurityProfile `json:"securityProfile,omitempty"`

	// ConsumerNamespaceSelector selects the namespaces, other than this
	// VMClass's own, that may use it: VirtualMachines there may name it in
	// spec.classRef. A reference from the VMClass's own namespace is always
	// allowed.
	//
	//   - unset (the default): only its own namespace may use it.
	//   - {} (an empty selector): every namespace may use it.
	//   - matchLabels / matchExpressions: the namespaces whose labels match. The
	//     kubernetes.io/metadata.name label names a namespace explicitly.
	//
	// A reference from a namespace it does not select is refused with
	// Ready=False, reason ConsumerNotAllowed, and no provider call is made for
	// it. The selector is matched against Namespace labels, so whoever can label
	// a namespace can widen what that namespace may use (see
	// docs/cross-namespace-references.md). The selector is operator-side policy:
	// it is never sent to a provider.
	// +optional
	ConsumerNamespaceSelector *metav1.LabelSelector `json:"consumerNamespaceSelector,omitempty"`
}

// FirmwareType represents the firmware type for VMs. See
// VMClassSpec.Firmware for which providers read it.
// +kubebuilder:validation:Enum=BIOS;UEFI;EFI
type FirmwareType string

const (
	// FirmwareTypeBIOS indicates BIOS firmware
	FirmwareTypeBIOS FirmwareType = "BIOS"
	// FirmwareTypeUEFI indicates UEFI firmware
	FirmwareTypeUEFI FirmwareType = "UEFI"
	// FirmwareTypeEFI is accepted by the schema as an alias for UEFI, but the
	// vSphere provider recognises only UEFI: there, EFI selects BIOS firmware
	// (unless secure boot or a TPM forces EFI).
	FirmwareTypeEFI FirmwareType = "EFI"
)

// GuestToolsPolicy represents the guest tools installation policy. Accepted
// but not acted on in this release.
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

// VMResourceLimits defines resource limits and reservations. Accepted but
// not acted on in this release.
type VMResourceLimits struct {
	// CPULimit is the maximum CPU usage limit (in MHz or percentage).
	// Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=100000
	CPULimit *int32 `json:"cpuLimit,omitempty"`

	// CPUReservation is the guaranteed CPU allocation (in MHz). Accepted but
	// not acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100000
	CPUReservation *int32 `json:"cpuReservation,omitempty"`

	// MemoryLimit is the maximum memory usage limit. Accepted but not acted
	// on in this release.
	// +optional
	MemoryLimit *resource.Quantity `json:"memoryLimit,omitempty"`

	// MemoryReservation is the guaranteed memory allocation. Accepted but not
	// acted on in this release.
	// +optional
	MemoryReservation *resource.Quantity `json:"memoryReservation,omitempty"`

	// CPUShares defines the relative CPU priority (higher = more priority).
	// Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	CPUShares *int32 `json:"cpuShares,omitempty"`
}

// PerformanceProfile defines performance-related settings. See
// VMClassSpec.PerformanceProfile for which fields are acted on.
type PerformanceProfile struct {
	// LatencySensitivity configures latency sensitivity. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:default="normal"
	// +kubebuilder:validation:Enum=low;normal;high
	LatencySensitivity string `json:"latencySensitivity,omitempty"`

	// CPUHotAddEnabled allows adding CPUs while the VM is running. libvirt
	// provisions headroom for live vCPU growth; vSphere enables CPU hot-add,
	// except when nestedVirtualization is also set (the combination is not
	// supported there, and hot-add is skipped). Proxmox and mock ignore it.
	// +optional
	// +kubebuilder:default=false
	CPUHotAddEnabled bool `json:"cpuHotAddEnabled,omitempty"`

	// MemoryHotAddEnabled allows adding memory while the VM is running.
	// libvirt provisions a memory ceiling above the initial memory for live
	// growth; vSphere enables memory hot-add. On a clustered Provider the
	// scheduler also reserves that headroom on the host. Proxmox and mock
	// ignore it.
	// +optional
	// +kubebuilder:default=false
	MemoryHotAddEnabled bool `json:"memoryHotAddEnabled,omitempty"`

	// VirtualizationBasedSecurity enables VBS features. Only the vSphere
	// provider acts on it; libvirt, Proxmox and mock ignore it.
	// +optional
	// +kubebuilder:default=false
	VirtualizationBasedSecurity bool `json:"virtualizationBasedSecurity,omitempty"`

	// NestedVirtualization enables nested virtualization. It is acted on by
	// vSphere and libvirt; Proxmox and mock ignore it.
	// +optional
	// +kubebuilder:default=false
	NestedVirtualization bool `json:"nestedVirtualization,omitempty"`

	// HyperThreadingPolicy controls hyperthreading usage. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:default="auto"
	// +kubebuilder:validation:Enum=auto;prefer;avoid;require
	HyperThreadingPolicy string `json:"hyperThreadingPolicy,omitempty"`
}

// SecurityProfile defines security-related settings. See
// VMClassSpec.SecurityProfile for which fields are acted on.
type SecurityProfile struct {
	// SecureBoot enables secure boot. vSphere forces EFI firmware and enables
	// secure boot; libvirt boots the VM from secure-boot OVMF firmware on a
	// q35 machine type. Proxmox and mock ignore it.
	// +optional
	// +kubebuilder:default=false
	SecureBoot bool `json:"secureBoot,omitempty"`

	// TPMEnabled enables TPM (Trusted Platform Module). vSphere adds a
	// virtual TPM and forces EFI firmware; libvirt adds an emulated TPM.
	// Proxmox and mock ignore it.
	// +optional
	// +kubebuilder:default=false
	TPMEnabled bool `json:"tpmEnabled,omitempty"`

	// TPMVersion specifies the TPM version. Accepted but not acted on in this
	// release. libvirt always emulates TPM 2.0, and vSphere adds its virtual
	// TPM, whatever is set here.
	// +optional
	// +kubebuilder:validation:Enum=1.2;2.0
	TPMVersion string `json:"tpmVersion,omitempty"`

	// VTDEnabled enables Intel VT-d or AMD-Vi. vSphere sets vvtd.enable;
	// libvirt adds an emulated IOMMU. Proxmox and mock ignore it.
	// +optional
	// +kubebuilder:default=false
	VTDEnabled bool `json:"vtdEnabled,omitempty"`

	// EncryptionPolicy defines VM encryption settings. Accepted but not acted
	// on in this release.
	// +optional
	EncryptionPolicy *EncryptionPolicy `json:"encryptionPolicy,omitempty"`
}

// EncryptionPolicy defines VM encryption settings. Accepted but not acted
// on in this release.
type EncryptionPolicy struct {
	// Enabled indicates if encryption should be used. Accepted but not acted
	// on in this release.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// KeyProvider specifies the encryption key provider. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:Enum=standard;hardware;external
	KeyProvider string `json:"keyProvider,omitempty"`

	// RequireEncryption mandates encryption (fails if not available).
	// Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default=false
	RequireEncryption bool `json:"requireEncryption,omitempty"`
}

// DiskDefaults provides default disk settings. See VMClassSpec.DiskDefaults
// for which fields are acted on.
type DiskDefaults struct {
	// Type specifies the default disk type. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:default="thin"
	Type DiskType `json:"type,omitempty"`

	// Size specifies the default root disk size. libvirt creates the root
	// disk at this size and vSphere grows the root disk to it after creation
	// (it never shrinks a disk); Proxmox and mock ignore it. It must be
	// non-negative and below 1Pi: every value below 900Ti written in bytes,
	// k/Ki, M/Mi, G/Gi or T/Ti is accepted, and nothing at or above 1Pi is.
	// That is more than ten times the largest single virtual disk vSphere
	// supports (62 TB), and keeps the GiB value the provider contract carries
	// far inside its int32 range. The rules bound the number of integer
	// digits per unit suffix with core CEL only (no quantity library, which
	// Kubernetes < 1.29 lacks), so they are enforced on every supported
	// Kubernetes version.
	// +optional
	// +kubebuilder:default="40Gi"
	// +kubebuilder:validation:XValidation:rule="type(self) == int ? (self >= 0 && self < 1000000000000000) : self.matches('^[+]?[0-9.]*(k|Ki|Mi?|Gi?|Ti?|[mun]|[eE]([+]?0*(1[0-4]|[0-9])|-[0-9]+))?$')",message="diskDefaults.size must be a non-negative quantity below 1Pi"
	// +kubebuilder:validation:XValidation:rule="type(self) == int || ((self.matches('[a-zA-Z]') || !self.matches('^[+]?0*[1-9][0-9]{15}')) && (!self.matches('(k|Ki)$') || !self.matches('^[+]?0*[1-9][0-9]{12}')) && (!self.matches('Mi?$') || !self.matches('^[+]?0*[1-9][0-9]{9}')))",message="diskDefaults.size must be a non-negative quantity below 1Pi"
	// +kubebuilder:validation:XValidation:rule="type(self) == int || ((!self.matches('Gi?$') || !self.matches('^[+]?0*[1-9][0-9]{6}')) && (!self.matches('Ti?$') || !self.matches('^[+]?0*[1-9][0-9]{3}')) && (!self.matches('[mun]$') || !self.matches('^[+]?0*[1-9][0-9]{18}')))",message="diskDefaults.size must be a non-negative quantity below 1Pi"
	// +kubebuilder:validation:XValidation:rule="type(self) == int || ((!self.matches('[eE][+]?0*[0-9]$') || !self.matches('^[+]?0*[1-9][0-9]{6}')) && (!self.matches('[eE][+]?0*1[0-4]$') || !self.matches('^[+]?0*[1-9][0-9]')) && (!self.matches('[eE]-') || !self.matches('^[+]?0*[1-9][0-9]{15}')))",message="diskDefaults.size must be a non-negative quantity below 1Pi"
	Size resource.Quantity `json:"size,omitempty"`

	// IOPS specifies the default IOPS limit. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=100000
	IOPS *int32 `json:"iops,omitempty"`

	// StorageClass specifies the default storage class. Accepted but not
	// acted on in this release.
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

// VMClassStatus defines the observed state of VMClass. Not populated in
// this release. No controller writes the status of a VMClass.
type VMClassStatus struct {
	// Conditions represent the latest available observations. Not populated
	// in this release.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration reflects the generation observed by the controller.
	// Not populated in this release.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// UsedByVMs is the number of VMs currently using this class. Not
	// populated in this release.
	// +optional
	UsedByVMs int32 `json:"usedByVMs,omitempty"`

	// SupportedProviders lists the providers that support this class. Not
	// populated in this release.
	// +optional
	SupportedProviders []string `json:"supportedProviders,omitempty"`

	// ValidationResults contains validation results for different providers.
	// Not populated in this release.
	// +optional
	ValidationResults map[string]ValidationResult `json:"validationResults,omitempty"`
}

// ValidationResult represents a validation result for a provider. Not
// populated in this release.
type ValidationResult struct {
	// Valid indicates if the class is valid for the provider. Not populated
	// in this release.
	Valid bool `json:"valid"`

	// Message provides details about the validation result. Not populated in
	// this release.
	// +optional
	Message string `json:"message,omitempty"`

	// Warnings lists any validation warnings. Not populated in this release.
	// +optional
	Warnings []string `json:"warnings,omitempty"`

	// LastValidated is when this validation was last performed. Not populated
	// in this release.
	// +optional
	LastValidated *metav1.Time `json:"lastValidated,omitempty"`
}

// VMClass condition types
const (
	// VMClassConditionReady indicates whether the class is ready. It is never
	// set in this release.
	VMClassConditionReady = "Ready"
	// VMClassConditionValidated indicates whether the class is validated. It
	// is never set in this release.
	VMClassConditionValidated = "Validated"
	// VMClassConditionSupported indicates whether the class is supported by
	// providers. It is never set in this release.
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

// VMClass is a reusable hardware profile for VirtualMachines: vCPU count,
// memory, firmware and optional disk, performance and security settings. A
// VirtualMachine names it in spec.classRef.
// +kubebuilder:storageversion
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

func init() {
	SchemeBuilder.Register(&VMClass{}, &VMClassList{})
}
