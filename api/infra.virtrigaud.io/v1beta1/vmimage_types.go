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

// VMImageSpec defines the desired state of VMImage
type VMImageSpec struct {
	// Source defines the image source configuration
	Source ImageSource `json:"source"`

	// Prepare contains optional image preparation steps
	// +optional
	Prepare *ImagePrepare `json:"prepare,omitempty"`

	// Metadata contains image metadata and annotations
	// +optional
	Metadata *ImageMetadata `json:"metadata,omitempty"`

	// Distribution contains OS distribution information
	// +optional
	Distribution *OSDistribution `json:"distribution,omitempty"`

	// ConsumerNamespaceSelector selects the namespaces, other than this
	// VMImage's own, that may use it: VirtualMachines there may name it in
	// spec.imageRef (which prepares it on, and creates VMs from it through,
	// their Provider). A reference from the VMImage's own namespace is always
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

// ImageSource defines the source of the VM image
type ImageSource struct {
	// VSphere contains vSphere-specific image configuration
	// +optional
	VSphere *VSphereImageSource `json:"vsphere,omitempty"`

	// Libvirt contains Libvirt-specific image configuration
	// +optional
	Libvirt *LibvirtImageSource `json:"libvirt,omitempty"`

	// HTTP contains HTTP/HTTPS download configuration
	// +optional
	HTTP *HTTPImageSource `json:"http,omitempty"`

	// Registry contains container registry image configuration
	// +optional
	Registry *RegistryImageSource `json:"registry,omitempty"`

	// DataVolume contains DataVolume-based image configuration
	// +optional
	DataVolume *DataVolumeImageSource `json:"dataVolume,omitempty"`

	// Proxmox contains Proxmox VE-specific image configuration
	// +optional
	Proxmox *ProxmoxImageSource `json:"proxmox,omitempty"`
}

// VSphereImageSource defines vSphere-specific image configuration.
//
// This struct supports multiple ways to reference a VM template:
//   - TemplateName: A simple template name or full inventory path
//   - ContentLibrary: Reference to a vSphere Content Library item
//   - OVAURL: URL to download and import an OVA file
type VSphereImageSource struct {
	// TemplateName references an existing vSphere template by name.
	// This can be a simple name, matched exactly against the VMs in the
	// Provider's default datacenter, or an inventory path within that
	// datacenter's VM folder (absolute, e.g. "/DC1/vm/templates/ubuntu", or
	// relative to the VM folder, e.g. "templates/ubuntu"; an absolute path in any
	// other datacenter is rejected). It must name an object marked as a vSphere
	// template: a regular VM (running or powered off) is never used as a clone
	// source, a name matching more than one template is rejected (use an
	// inventory path), and a name of the form "vm-" followed only by digits is
	// rejected.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	TemplateName string `json:"templateName,omitempty"`

	// ContentLibrary references a vSphere content library item
	// +optional
	ContentLibrary *ContentLibraryRef `json:"contentLibrary,omitempty"`

	// OVAURL provides a URL to an OVA file to import
	// +optional
	// +kubebuilder:validation:Pattern="^https?://.*\\.(ova|ovf)$"
	OVAURL string `json:"ovaURL,omitempty"`

	// Checksum provides expected checksum for verification
	// +optional
	Checksum string `json:"checksum,omitempty"`

	// ChecksumType specifies the checksum algorithm
	// +optional
	// +kubebuilder:default="sha256"
	ChecksumType ChecksumType `json:"checksumType,omitempty"`

	// ProviderRef references the vSphere provider for importing
	// +optional
	ProviderRef *LocalObjectReference `json:"providerRef,omitempty"`
}

// ContentLibraryRef references a vSphere content library item
type ContentLibraryRef struct {
	// Library is the name of the content library
	// +kubebuilder:validation:MaxLength=255
	Library string `json:"library"`

	// Item is the name of the library item
	// +kubebuilder:validation:MaxLength=255
	Item string `json:"item"`

	// Version specifies the item version (optional)
	// +optional
	Version string `json:"version,omitempty"`
}

// LibvirtImagePathPattern is the admission-time shape of
// LibvirtImageSource.Path. It is the SINGLE regular expression shared by the
// CRD schema (the +kubebuilder:validation:Pattern marker on
// LibvirtImageSource.Path, which must be kept byte-identical —
// TestLibvirtImagePathCRDPatternMatchesGo enforces that) and the libvirt
// provider's pre-resolve validation (internal/providers/libvirt/imagepath.go).
//
// It admits an absolute path made of one or more non-empty segments, where no
// segment is "..", no segment starts with "-", and no character is a slash
// inside a segment or a C0/DEL/C1 control character. Spaces, dots in names
// (".local"), and non-ASCII letters remain legal.
//
// The pattern is defense in depth only. The authoritative control is the
// provider's host-side confinement: the path is canonicalized ON THE HYPERVISOR
// HOST (so symlinks cannot escape) and must then be a regular file directly
// inside an allowed image directory that is not a disk of any VM (security: a
// tenant-controlled path must never let a VM read an arbitrary host file or
// another VM's disk).
const LibvirtImagePathPattern = `^(/+([^/.\x00-\x1f\x7f-\x9f-][^/\x00-\x1f\x7f-\x9f]*|\.([^/.\x00-\x1f\x7f-\x9f][^/\x00-\x1f\x7f-\x9f]*)?|\.\.[^/\x00-\x1f\x7f-\x9f]+))+$`

// LibvirtImagePathMaxLength bounds LibvirtImageSource.Path (Linux PATH_MAX).
const LibvirtImagePathMaxLength = 4096

// LibvirtImageSource defines Libvirt-specific image configuration
type LibvirtImageSource struct {
	// Path is the absolute path of an existing base image file on the libvirt
	// host. The provider copies it into the VM's own disk; it is never attached
	// in place. For security the provider resolves the path on the host
	// (following symlinks) and accepts it only if the result is a regular file
	// directly inside one of the provider's allowed image directories
	// (VIRTRIGAUD_LIBVIRT_IMAGE_DIRS, default /var/lib/libvirt/images), is not
	// a disk of any existing VM, does not use a VirtRigaud-reserved name
	// (*-disk.qcow2, *-migrated.qcow2, dotfiles), and has no backing file or
	// external data file.
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	// +kubebuilder:validation:Pattern=`^(/+([^/.\x00-\x1f\x7f-\x9f-][^/\x00-\x1f\x7f-\x9f]*|\.([^/.\x00-\x1f\x7f-\x9f][^/\x00-\x1f\x7f-\x9f]*)?|\.\.[^/\x00-\x1f\x7f-\x9f]+))+$`
	Path string `json:"path,omitempty"`

	// URL provides a URL to download the image
	// +optional
	// +kubebuilder:validation:Pattern="^(https?|ftp)://.*"
	URL string `json:"url,omitempty"`

	// Format specifies the image format
	// +optional
	// +kubebuilder:default="qcow2"
	Format ImageFormat `json:"format,omitempty"`

	// Checksum provides expected checksum for verification
	// +optional
	Checksum string `json:"checksum,omitempty"`

	// ChecksumType specifies the checksum algorithm
	// +optional
	// +kubebuilder:default="sha256"
	ChecksumType ChecksumType `json:"checksumType,omitempty"`

	// StoragePool specifies the libvirt storage pool
	// +optional
	// +kubebuilder:validation:MaxLength=255
	StoragePool string `json:"storagePool,omitempty"`
}

// HTTPImageSource defines HTTP/HTTPS download configuration
type HTTPImageSource struct {
	// URL is the HTTP/HTTPS URL to download the image
	// +kubebuilder:validation:Pattern="^https?://.*"
	URL string `json:"url"`

	// Headers contains HTTP headers to include in the request
	// +optional
	// +kubebuilder:validation:MaxProperties=20
	Headers map[string]string `json:"headers,omitempty"`

	// Checksum provides expected checksum for verification
	// +optional
	Checksum string `json:"checksum,omitempty"`

	// ChecksumType specifies the checksum algorithm
	// +optional
	// +kubebuilder:default="sha256"
	ChecksumType ChecksumType `json:"checksumType,omitempty"`

	// Authentication contains authentication configuration
	// +optional
	Authentication *HTTPAuthentication `json:"authentication,omitempty"`

	// Timeout specifies the download timeout
	// +optional
	// +kubebuilder:default="30m"
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// HTTPAuthentication defines HTTP authentication options
type HTTPAuthentication struct {
	// BasicAuth contains basic authentication configuration
	// +optional
	BasicAuth *BasicAuthConfig `json:"basicAuth,omitempty"`

	// Bearer contains bearer token authentication
	// +optional
	Bearer *BearerTokenConfig `json:"bearer,omitempty"`

	// ClientCert contains client certificate authentication
	// +optional
	ClientCert *ClientCertConfig `json:"clientCert,omitempty"`
}

// BasicAuthConfig defines basic authentication configuration
type BasicAuthConfig struct {
	// SecretRef references a secret containing username and password
	SecretRef LocalObjectReference `json:"secretRef"`

	// UsernameKey is the key in the secret containing the username (default: username)
	// +optional
	// +kubebuilder:default="username"
	UsernameKey string `json:"usernameKey,omitempty"`

	// PasswordKey is the key in the secret containing the password (default: password)
	// +optional
	// +kubebuilder:default="password"
	PasswordKey string `json:"passwordKey,omitempty"`
}

// BearerTokenConfig defines bearer token authentication configuration
type BearerTokenConfig struct {
	// SecretRef references a secret containing the bearer token
	SecretRef LocalObjectReference `json:"secretRef"`

	// TokenKey is the key in the secret containing the token (default: token)
	// +optional
	// +kubebuilder:default="token"
	TokenKey string `json:"tokenKey,omitempty"`
}

// ClientCertConfig defines client certificate authentication configuration
type ClientCertConfig struct {
	// SecretRef references a secret containing the client certificate and key
	SecretRef LocalObjectReference `json:"secretRef"`

	// CertKey is the key in the secret containing the certificate (default: tls.crt)
	// +optional
	// +kubebuilder:default="tls.crt"
	CertKey string `json:"certKey,omitempty"`

	// KeyKey is the key in the secret containing the private key (default: tls.key)
	// +optional
	// +kubebuilder:default="tls.key"
	KeyKey string `json:"keyKey,omitempty"`
}

// RegistryImageSource defines container registry image configuration
type RegistryImageSource struct {
	// Image is the container image reference
	// +kubebuilder:validation:Pattern="^[a-zA-Z0-9._/-]+:[a-zA-Z0-9._-]+$"
	Image string `json:"image"`

	// PullSecretRef references a secret for pulling the image
	// +optional
	PullSecretRef *LocalObjectReference `json:"pullSecretRef,omitempty"`

	// Format specifies the expected image format
	// +optional
	// +kubebuilder:default="qcow2"
	Format ImageFormat `json:"format,omitempty"`
}

// DataVolumeImageSource defines DataVolume-based image configuration
type DataVolumeImageSource struct {
	// Name is the name of the DataVolume
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Namespace is the namespace of the DataVolume (defaults to image namespace)
	// +optional
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`
}

// ProxmoxImageSource defines Proxmox VE-specific image configuration
type ProxmoxImageSource struct {
	// TemplateID specifies an existing Proxmox template VMID
	// +optional
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=999999999
	TemplateID *int `json:"templateID,omitempty"`

	// TemplateName specifies an existing Proxmox template name
	// +optional
	// +kubebuilder:validation:MaxLength=255
	TemplateName string `json:"templateName,omitempty"`

	// Storage specifies the Proxmox storage for cloning
	// +optional
	// +kubebuilder:validation:MaxLength=255
	// Examples: "local-lvm", "vms", "nfs-storage"
	Storage string `json:"storage,omitempty"`

	// Node specifies the Proxmox node where the template exists
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Node string `json:"node,omitempty"`

	// Format specifies the disk format
	// +optional
	// +kubebuilder:default="qcow2"
	// +kubebuilder:validation:Enum=raw;qcow2;vmdk
	Format string `json:"format,omitempty"`

	// FullClone determines if this should be a full clone (default) or linked clone
	// +optional
	// +kubebuilder:default=true
	FullClone *bool `json:"fullClone,omitempty"`
}

// ImageFormat represents the format of a VM image
// +kubebuilder:validation:Enum=qcow2;raw;vmdk;vhd;vhdx;iso;ova;ovf
type ImageFormat string

const (
	// ImageFormatQCOW2 indicates QEMU QCOW2 format
	ImageFormatQCOW2 ImageFormat = "qcow2"
	// ImageFormatRaw indicates raw disk format
	ImageFormatRaw ImageFormat = "raw"
	// ImageFormatVMDK indicates VMware VMDK format
	ImageFormatVMDK ImageFormat = "vmdk"
	// ImageFormatVHD indicates Microsoft VHD format
	ImageFormatVHD ImageFormat = "vhd"
	// ImageFormatVHDX indicates Microsoft VHDX format
	ImageFormatVHDX ImageFormat = "vhdx"
	// ImageFormatISO indicates ISO format
	ImageFormatISO ImageFormat = "iso"
	// ImageFormatOVA indicates OVA format
	ImageFormatOVA ImageFormat = "ova"
	// ImageFormatOVF indicates OVF format
	ImageFormatOVF ImageFormat = "ovf"
)

// ChecksumType represents the checksum algorithm
// +kubebuilder:validation:Enum=md5;sha1;sha256;sha512
type ChecksumType string

const (
	// ChecksumTypeMD5 indicates MD5 checksum
	ChecksumTypeMD5 ChecksumType = "md5"
	// ChecksumTypeSHA1 indicates SHA1 checksum
	ChecksumTypeSHA1 ChecksumType = "sha1"
	// ChecksumTypeSHA256 indicates SHA256 checksum
	ChecksumTypeSHA256 ChecksumType = "sha256"
	// ChecksumTypeSHA512 indicates SHA512 checksum
	ChecksumTypeSHA512 ChecksumType = "sha512"
)

// ImageMetadata contains image metadata and annotations
type ImageMetadata struct {
	// DisplayName is a human-readable name for the image
	// +optional
	// +kubebuilder:validation:MaxLength=255
	DisplayName string `json:"displayName,omitempty"`

	// Description provides a description of the image
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// Version specifies the image version
	// +optional
	// +kubebuilder:validation:MaxLength=100
	Version string `json:"version,omitempty"`

	// Architecture specifies the CPU architecture
	// +optional
	// +kubebuilder:default="amd64"
	// +kubebuilder:validation:Enum=amd64;arm64;x86_64;aarch64
	Architecture string `json:"architecture,omitempty"`

	// Tags are key-value pairs for categorizing the image
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Tags map[string]string `json:"tags,omitempty"`

	// Annotations are additional metadata annotations
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Annotations map[string]string `json:"annotations,omitempty"`
}

// OSDistribution contains OS distribution information
type OSDistribution struct {
	// Name is the OS distribution name
	// +optional
	// +kubebuilder:validation:Enum=ubuntu;centos;rhel;fedora;debian;suse;windows;freebsd;coreos;other
	Name string `json:"name,omitempty"`

	// Version is the distribution version
	// +optional
	// +kubebuilder:validation:MaxLength=100
	Version string `json:"version,omitempty"`

	// Variant is the distribution variant (e.g., server, desktop)
	// +optional
	// +kubebuilder:validation:MaxLength=100
	Variant string `json:"variant,omitempty"`

	// Family is the OS family
	// +optional
	// +kubebuilder:validation:Enum=linux;windows;bsd;other
	Family string `json:"family,omitempty"`

	// Kernel specifies kernel information
	// +optional
	Kernel *KernelInfo `json:"kernel,omitempty"`
}

// KernelInfo contains kernel information
type KernelInfo struct {
	// Version is the kernel version
	// +optional
	Version string `json:"version,omitempty"`

	// Type is the kernel type
	// +optional
	// +kubebuilder:validation:Enum=linux;windows;freebsd;other
	Type string `json:"type,omitempty"`
}

// ImagePrepare defines optional image preparation steps
type ImagePrepare struct {
	// OnMissing defines the action to take when image is missing
	// +optional
	// +kubebuilder:default="Import"
	OnMissing ImageMissingAction `json:"onMissing,omitempty"`

	// ValidateChecksum validates the image checksum
	// +optional
	// +kubebuilder:default=true
	ValidateChecksum bool `json:"validateChecksum,omitempty"`

	// Timeout defines the maximum time to wait for preparation
	// +optional
	// +kubebuilder:default="30m"
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Retries defines the number of retry attempts for failed operations
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	Retries *int32 `json:"retries,omitempty"`

	// Force forces re-import even if image exists
	// +optional
	Force bool `json:"force,omitempty"`

	// Storage defines storage-specific preparation options
	// +optional
	Storage *StoragePrepareOptions `json:"storage,omitempty"`

	// Optimization defines image optimization options
	// +optional
	Optimization *ImageOptimization `json:"optimization,omitempty"`
}

// ImageMissingAction defines actions to take when an image is missing
// +kubebuilder:validation:Enum=Import;Fail;Wait
type ImageMissingAction string

const (
	// ImageMissingActionImport imports the missing image
	ImageMissingActionImport ImageMissingAction = "Import"
	// ImageMissingActionFail fails when the image is missing
	ImageMissingActionFail ImageMissingAction = "Fail"
	// ImageMissingActionWait waits for the image to become available
	ImageMissingActionWait ImageMissingAction = "Wait"
)

// StoragePrepareOptions defines storage-specific preparation options
type StoragePrepareOptions struct {
	// VSphere storage options
	// +optional
	VSphere *VSphereStorageOptions `json:"vsphere,omitempty"`

	// Libvirt storage options
	// +optional
	Libvirt *LibvirtStorageOptions `json:"libvirt,omitempty"`

	// PreferredFormat specifies the preferred target format
	// +optional
	PreferredFormat ImageFormat `json:"preferredFormat,omitempty"`

	// Compression enables compression during import
	// +optional
	// +kubebuilder:default=false
	Compression bool `json:"compression,omitempty"`
}

// VSphereStorageOptions defines vSphere storage preparation options
type VSphereStorageOptions struct {
	// Datastore specifies the target datastore for import
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Datastore string `json:"datastore,omitempty"`

	// Folder specifies the target folder for import
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Folder string `json:"folder,omitempty"`

	// ThinProvisioned indicates whether to use thin provisioning
	// +optional
	ThinProvisioned *bool `json:"thinProvisioned,omitempty"`

	// DiskType specifies the disk provisioning type
	// +optional
	// +kubebuilder:validation:Enum=thin;thick;eagerzeroedthick
	DiskType string `json:"diskType,omitempty"`
}

// LibvirtStorageOptions defines Libvirt storage preparation options
type LibvirtStorageOptions struct {
	// StoragePool specifies the target storage pool for import
	// +optional
	// +kubebuilder:validation:MaxLength=255
	StoragePool string `json:"storagePool,omitempty"`

	// AllocationPolicy defines how storage is allocated
	// +optional
	// +kubebuilder:validation:Enum=eager;lazy
	AllocationPolicy string `json:"allocationPolicy,omitempty"`

	// Preallocation specifies preallocation mode
	// +optional
	// +kubebuilder:validation:Enum=off;metadata;falloc;full
	Preallocation string `json:"preallocation,omitempty"`
}

// ImageOptimization defines image optimization options
type ImageOptimization struct {
	// EnableCompression enables image compression
	// +optional
	// +kubebuilder:default=false
	EnableCompression bool `json:"enableCompression,omitempty"`

	// RemoveUnusedSpace removes unused space from the image
	// +optional
	// +kubebuilder:default=false
	RemoveUnusedSpace bool `json:"removeUnusedSpace,omitempty"`

	// ConvertFormat converts the image to a more optimal format
	// +optional
	ConvertFormat ImageFormat `json:"convertFormat,omitempty"`

	// EnableDeltaSync enables delta synchronization for updates
	// +optional
	// +kubebuilder:default=false
	EnableDeltaSync bool `json:"enableDeltaSync,omitempty"`
}

// VMImageStatus defines the observed state of VMImage
type VMImageStatus struct {
	// Ready indicates if the image is ready for use
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Phase represents the current phase of image preparation
	// +optional
	Phase ImagePhase `json:"phase,omitempty"`

	// Message provides additional details about the current state
	// +optional
	Message string `json:"message,omitempty"`

	// AvailableOn lists the Providers the image is available on, each as
	// "<namespace>/<name>" (the ProviderStatus keys whose entry is available).
	// It is informational: creating a VM consults ProviderStatus only.
	// +optional
	AvailableOn []string `json:"availableOn,omitempty"`

	// Conditions represent the latest available observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration reflects the generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastPrepareTime records when the image was last prepared
	// +optional
	LastPrepareTime *metav1.Time `json:"lastPrepareTime,omitempty"`

	// PrepareTaskRef is deprecated and no longer written. An in-flight
	// prepare task is recorded per Provider, in
	// ProviderStatus["<namespace>/<name>"].taskRef, and polled only through
	// that Provider. A value left by an earlier release cannot be attributed
	// to a Provider: the controller clears it without polling it and issues
	// the prepare again (it is idempotent on every provider).
	// +optional
	PrepareTaskRef string `json:"prepareTaskRef,omitempty"`

	// ImportProgress shows the progress of image import operations
	// +optional
	ImportProgress *ImageImportProgress `json:"importProgress,omitempty"`

	// Size is the size of the prepared image
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// Checksum is the actual checksum of the prepared image
	// +optional
	Checksum string `json:"checksum,omitempty"`

	// Format is the actual format of the prepared image
	// +optional
	Format ImageFormat `json:"format,omitempty"`

	// ProviderStatus is the per-Provider prepare state, keyed by the
	// Provider's identity "<namespace>/<name>" (for example "team-a/vsphere"),
	// so two Providers with the same name in different namespaces never share
	// an entry. A VirtualMachine consults only the entry of the Provider it
	// uses, and trusts it only when the entry's providerUID is that Provider's
	// current UID.
	//
	// Entries written by an earlier release are keyed by the bare Provider
	// name. On the first prepare reconcile such an entry is re-keyed to
	// "<this VMImage's namespace>/<name>" when that Provider exists, and is
	// dropped otherwise. A re-keyed entry has no providerUID, so it is
	// re-validated through its Provider before a VM is created from it.
	// +optional
	ProviderStatus map[string]ProviderImageStatus `json:"providerStatus,omitempty"`
}

// ImagePhase represents the phase of image preparation
// +kubebuilder:validation:Enum=Pending;Downloading;Importing;Converting;Optimizing;Ready;Failed
type ImagePhase string

const (
	// ImagePhasePending indicates the image is waiting to be processed
	ImagePhasePending ImagePhase = "Pending"
	// ImagePhaseDownloading indicates the image is being downloaded
	ImagePhaseDownloading ImagePhase = "Downloading"
	// ImagePhaseImporting indicates the image is being imported
	ImagePhaseImporting ImagePhase = "Importing"
	// ImagePhaseConverting indicates the image is being converted
	ImagePhaseConverting ImagePhase = "Converting"
	// ImagePhaseOptimizing indicates the image is being optimized
	ImagePhaseOptimizing ImagePhase = "Optimizing"
	// ImagePhaseReady indicates the image is ready for use
	ImagePhaseReady ImagePhase = "Ready"
	// ImagePhaseFailed indicates the image preparation failed
	ImagePhaseFailed ImagePhase = "Failed"
)

// ImageImportProgress tracks the progress of image import operations
type ImageImportProgress struct {
	// TotalBytes is the total size of the image being imported
	// +optional
	TotalBytes *int64 `json:"totalBytes,omitempty"`

	// TransferredBytes is the number of bytes transferred so far
	// +optional
	TransferredBytes *int64 `json:"transferredBytes,omitempty"`

	// Percentage is the completion percentage (0-100)
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Percentage *int32 `json:"percentage,omitempty"`

	// TransferRate is the current transfer rate in bytes per second
	// +optional
	TransferRate *int64 `json:"transferRate,omitempty"`

	// ETA is the estimated time to completion
	// +optional
	ETA *metav1.Duration `json:"eta,omitempty"`

	// StartTime is when the import started
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
}

// ProviderImageStatus is the prepare state of an image on one Provider (an
// entry of VMImageStatus.ProviderStatus).
type ProviderImageStatus struct {
	// Available indicates if the image is available on this provider
	Available bool `json:"available"`

	// ProviderUID is the UID of the Provider object this entry was recorded
	// through. A VirtualMachine uses the entry only when it equals its
	// Provider's current UID. An entry with another UID (the Provider was
	// deleted and re-created under the same namespace and name) or with none
	// (an entry from an earlier release) is re-validated: the idempotent
	// prepare is issued again through the current Provider, which confirms (or
	// re-creates) the prepared image, and its UID is recorded. When
	// spec.prepare.onMissing forbids preparing (Fail, Wait), an available
	// entry recorded through a previous object of the same Provider is
	// accepted as is, with a warning, and the current UID recorded; an entry
	// with no UID is never used and VM creates wait (reason
	// ProviderUIDMissing) until this field is set. The prepare runs only
	// before a VM is created, never for a VM that exists.
	// +optional
	ProviderUID string `json:"providerUID,omitempty"`

	// TaskRef is the provider task of an in-flight asynchronous prepare on
	// this Provider. It is polled only through this Provider, and only while
	// ProviderUID is the Provider's current UID; it is cleared when the
	// prepare completes.
	// +optional
	TaskRef string `json:"taskRef,omitempty"`

	// ID is the provider-specific image identifier
	// +optional
	ID string `json:"id,omitempty"`

	// Path is the provider-specific image path
	// +optional
	Path string `json:"path,omitempty"`

	// Size is the image size on this provider
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// LastUpdated is when the status was last updated
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`

	// Message provides provider-specific status information
	// +optional
	Message string `json:"message,omitempty"`
}

// VMImage condition types
const (
	// VMImageConditionReady indicates whether the image is ready
	VMImageConditionReady = "Ready"
	// VMImageConditionDownloading indicates whether the image is downloading
	VMImageConditionDownloading = "Downloading"
	// VMImageConditionImporting indicates whether the image is importing
	VMImageConditionImporting = "Importing"
	// VMImageConditionValidated indicates whether the image is validated
	VMImageConditionValidated = "Validated"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.status.size`
//+kubebuilder:printcolumn:name="Providers",type=string,JSONPath=`.status.availableOn[*]`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
//+kubebuilder:resource:shortName=vmimg

// VMImage is the Schema for the vmimages API
// +kubebuilder:storageversion
type VMImage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VMImageSpec   `json:"spec,omitempty"`
	Status VMImageStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// VMImageList contains a list of VMImage
type VMImageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VMImage `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VMImage{}, &VMImageList{})
}
