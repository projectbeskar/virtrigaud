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

// VMImageSpec defines the desired state of VMImage. Source says where the image
// comes from; prepare.onMissing and prepare.timeout steer how it is prepared on a
// Provider. metadata and distribution are descriptive. Several other fields are
// accepted but not acted on in this release; each says so.
type VMImageSpec struct {
	// Source defines where the image comes from. Set the block for one kind of
	// source: vsphere, libvirt and proxmox are acted on; http, registry and
	// dataVolume are accepted but not acted on in this release. Nothing checks
	// that only one block is set.
	Source ImageSource `json:"source"`

	// Prepare contains optional image preparation steps. Only onMissing and
	// timeout are acted on; the other fields are accepted but not acted on in
	// this release. Preparation applies only to sources that must be imported (a
	// vSphere ovaURL, a libvirt url); an image that names something already on
	// the hypervisor (a template, a libvirt path) is never prepared.
	// +optional
	Prepare *ImagePrepare `json:"prepare,omitempty"`

	// Metadata contains image metadata and annotations. Descriptive: no
	// controller or provider reads it.
	// +optional
	Metadata *ImageMetadata `json:"metadata,omitempty"`

	// Distribution contains OS distribution information. Descriptive: no
	// controller or provider reads it.
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

// ImageSource defines the source of the VM image. The vsphere, libvirt and
// proxmox sources are acted on by the provider of that kind. The http, registry
// and dataVolume sources are accepted but not acted on in this release: no
// provider downloads from them, so the vSphere, libvirt and Proxmox providers
// refuse to prepare an image that has only such a source (the VMImage reports
// reason InvalidSource and VMs that use it are not created). To import from a
// URL, use vsphere.ovaURL or libvirt.url.
type ImageSource struct {
	// VSphere contains vSphere-specific image configuration. It is acted on by
	// the vSphere provider.
	// +optional
	VSphere *VSphereImageSource `json:"vsphere,omitempty"`

	// Libvirt contains Libvirt-specific image configuration. It is acted on by
	// the libvirt provider.
	// +optional
	Libvirt *LibvirtImageSource `json:"libvirt,omitempty"`

	// HTTP contains HTTP/HTTPS download configuration. Accepted but not acted
	// on in this release: no provider downloads from an http source. (The
	// Proxmox provider reads http.url only to refuse it.)
	// +optional
	HTTP *HTTPImageSource `json:"http,omitempty"`

	// Registry contains container registry image configuration. Accepted but
	// not acted on in this release.
	// +optional
	Registry *RegistryImageSource `json:"registry,omitempty"`

	// DataVolume contains DataVolume-based image configuration. Accepted but
	// not acted on in this release.
	// +optional
	DataVolume *DataVolumeImageSource `json:"dataVolume,omitempty"`

	// Proxmox contains Proxmox VE-specific image configuration. It is acted on
	// by the Proxmox provider.
	// +optional
	Proxmox *ProxmoxImageSource `json:"proxmox,omitempty"`
}

// VSphereImageSource defines vSphere-specific image configuration. Set one of:
//   - TemplateName: an existing vSphere template, by name or inventory path.
//     Nothing is prepared; VMs are cloned from it.
//   - OVAURL: an OVA to download and import as a template, when the first VM
//     that uses the image is created.
//   - ContentLibrary: accepted but not acted on in this release.
//
// Setting OVAURL together with TemplateName or ContentLibrary is refused when
// the image is prepared.
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

	// ContentLibrary references a vSphere content library item. Accepted but not
	// acted on in this release: the provider does not resolve content library
	// items, so a VM cannot be created from an image that has only this source.
	// Use templateName or ovaURL.
	// +optional
	ContentLibrary *ContentLibraryRef `json:"contentLibrary,omitempty"`

	// OVAURL is the URL of an OVA file to import as a vSphere template. The
	// vSphere provider downloads it and imports it when the first VM that uses the
	// image is created; the template is named after the VMImage's namespace, name
	// and UID and is reused by later VMs. The scheme must be http or https, and a
	// bare .ovf is refused when the image is prepared (publish an .ova). The
	// provider refuses addresses such as loopback and link-local ones. Keep
	// credentials out of the URL: it is part of the VMImage, which other
	// namespaces may be able to read (see docs/image-preparation.md).
	// +optional
	// +kubebuilder:validation:Pattern="^https?://.*\\.(ova|ovf)$"
	OVAURL string `json:"ovaURL,omitempty"`

	// Checksum is the expected checksum of the downloaded OVA: the hex digest
	// only, without an algorithm prefix, compared case-insensitively. It is
	// checked when the OVA is imported (ovaURL) and a mismatch fails the prepare;
	// it is not used with templateName. Empty disables the check.
	// +optional
	Checksum string `json:"checksum,omitempty"`

	// ChecksumType specifies the checksum algorithm: md5, sha1, sha256 or sha512.
	// +optional
	// +kubebuilder:default="sha256"
	ChecksumType ChecksumType `json:"checksumType,omitempty"`

	// ProviderRef references the vSphere provider for importing. Accepted but not
	// acted on in this release: an image is prepared through the Provider of the
	// VirtualMachine that uses it.
	// +optional
	ProviderRef *LocalObjectReference `json:"providerRef,omitempty"`
}

// ContentLibraryRef references a vSphere content library item. Accepted but not
// acted on in this release (see VSphereImageSource.ContentLibrary).
type ContentLibraryRef struct {
	// Library is the name of the content library. Accepted but not acted on in
	// this release.
	// +kubebuilder:validation:MaxLength=255
	Library string `json:"library"`

	// Item is the name of the library item. Accepted but not acted on in this
	// release.
	// +kubebuilder:validation:MaxLength=255
	Item string `json:"item"`

	// Version specifies the item version (optional). Accepted but not acted on
	// in this release.
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

// LibvirtImageSource defines Libvirt-specific image configuration. Set path (an
// image already on the libvirt host, used as written) or url (an image the
// provider downloads and prepares), not both.
type LibvirtImageSource struct {
	// Path is the absolute path of an existing base image file on the libvirt
	// host. It is used by reference: it is never downloaded or prepared, so
	// checksum is not checked for it. The provider copies it into the VM's own
	// disk; it is never attached in place. For security the provider resolves the path on the host
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

	// URL is the location of an image the libvirt provider downloads, on the
	// hypervisor host, when the first VM that uses the image is created (scheme
	// http, https or ftp). The provider converts it to qcow2 and publishes it in
	// the storage pool; every VM created from it gets its own copy. Keep
	// credentials out of the URL. Set url or path, not both.
	// +optional
	// +kubebuilder:validation:Pattern="^(https?|ftp)://.*"
	URL string `json:"url,omitempty"`

	// Format specifies the image format. Accepted but not acted on in this
	// release: the provider detects the format of the file itself, and converts
	// a downloaded image to qcow2.
	// +optional
	// +kubebuilder:default="qcow2"
	Format ImageFormat `json:"format,omitempty"`

	// Checksum is the expected checksum of the downloaded image: the hex digest
	// only, without an algorithm prefix, compared case-insensitively. It is
	// checked when a url image is prepared and a mismatch fails the prepare. It
	// is not checked for a path. Empty disables the check.
	// +optional
	Checksum string `json:"checksum,omitempty"`

	// ChecksumType specifies the checksum algorithm: md5, sha1, sha256 or sha512.
	// +optional
	// +kubebuilder:default="sha256"
	ChecksumType ChecksumType `json:"checksumType,omitempty"`

	// StoragePool is the libvirt storage pool a prepared (url) image is
	// published in. When empty the pool "default" is used. The pool must exist
	// and its directory must be one of the provider's allowed image directories.
	// It is not used for a path image.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	StoragePool string `json:"storagePool,omitempty"`
}

// HTTPImageSource defines HTTP/HTTPS download configuration. Accepted but not
// acted on in this release: no provider downloads from an http source, and none
// of its fields is read (the Proxmox provider reads url only to refuse it).
type HTTPImageSource struct {
	// URL is the HTTP/HTTPS URL to download the image. Accepted but not acted on
	// in this release.
	// +kubebuilder:validation:Pattern="^https?://.*"
	URL string `json:"url"`

	// Headers contains HTTP headers to include in the request. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxProperties=20
	Headers map[string]string `json:"headers,omitempty"`

	// Checksum provides expected checksum for verification. Accepted but not
	// acted on in this release.
	// +optional
	Checksum string `json:"checksum,omitempty"`

	// ChecksumType specifies the checksum algorithm. Accepted but not acted on
	// in this release.
	// +optional
	// +kubebuilder:default="sha256"
	ChecksumType ChecksumType `json:"checksumType,omitempty"`

	// Authentication contains authentication configuration. Accepted but not
	// acted on in this release: no credentials are read or sent.
	// +optional
	Authentication *HTTPAuthentication `json:"authentication,omitempty"`

	// Timeout specifies the download timeout. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:default="30m"
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// HTTPAuthentication defines HTTP authentication options. Accepted but not
// acted on in this release: the referenced Secrets are never read.
type HTTPAuthentication struct {
	// BasicAuth contains basic authentication configuration. Accepted but not
	// acted on in this release.
	// +optional
	BasicAuth *BasicAuthConfig `json:"basicAuth,omitempty"`

	// Bearer contains bearer token authentication. Accepted but not acted on in
	// this release.
	// +optional
	Bearer *BearerTokenConfig `json:"bearer,omitempty"`

	// ClientCert contains client certificate authentication. Accepted but not
	// acted on in this release.
	// +optional
	ClientCert *ClientCertConfig `json:"clientCert,omitempty"`
}

// BasicAuthConfig defines basic authentication configuration. Accepted but not
// acted on in this release.
type BasicAuthConfig struct {
	// SecretRef references a secret containing username and password. Accepted
	// but not acted on in this release.
	SecretRef LocalObjectReference `json:"secretRef"`

	// UsernameKey is the key in the secret containing the username (default:
	// username). Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default="username"
	UsernameKey string `json:"usernameKey,omitempty"`

	// PasswordKey is the key in the secret containing the password (default:
	// password). Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default="password"
	PasswordKey string `json:"passwordKey,omitempty"`
}

// BearerTokenConfig defines bearer token authentication configuration. Accepted
// but not acted on in this release.
type BearerTokenConfig struct {
	// SecretRef references a secret containing the bearer token. Accepted but
	// not acted on in this release.
	SecretRef LocalObjectReference `json:"secretRef"`

	// TokenKey is the key in the secret containing the token (default: token).
	// Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default="token"
	TokenKey string `json:"tokenKey,omitempty"`
}

// ClientCertConfig defines client certificate authentication configuration.
// Accepted but not acted on in this release.
type ClientCertConfig struct {
	// SecretRef references a secret containing the client certificate and key.
	// Accepted but not acted on in this release.
	SecretRef LocalObjectReference `json:"secretRef"`

	// CertKey is the key in the secret containing the certificate (default:
	// tls.crt). Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default="tls.crt"
	CertKey string `json:"certKey,omitempty"`

	// KeyKey is the key in the secret containing the private key (default:
	// tls.key). Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default="tls.key"
	KeyKey string `json:"keyKey,omitempty"`
}

// RegistryImageSource defines container registry image configuration. Accepted
// but not acted on in this release: no provider pulls from a registry, and none
// of its fields is read.
type RegistryImageSource struct {
	// Image is the container image reference. Accepted but not acted on in this
	// release.
	// +kubebuilder:validation:Pattern="^[a-zA-Z0-9._/-]+:[a-zA-Z0-9._-]+$"
	Image string `json:"image"`

	// PullSecretRef references a secret for pulling the image. Accepted but not
	// acted on in this release: the Secret is never read.
	// +optional
	PullSecretRef *LocalObjectReference `json:"pullSecretRef,omitempty"`

	// Format specifies the expected image format. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:default="qcow2"
	Format ImageFormat `json:"format,omitempty"`
}

// DataVolumeImageSource defines DataVolume-based image configuration. Accepted
// but not acted on in this release: no controller or provider reads a
// DataVolume.
type DataVolumeImageSource struct {
	// Name is the name of the DataVolume. Accepted but not acted on in this
	// release.
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Namespace is the namespace of the DataVolume (defaults to the image's
	// namespace). Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`
}

// ProxmoxImageSource defines Proxmox VE-specific image configuration. It
// references a template that already exists on Proxmox VE; VirtRigaud does not
// import images into Proxmox in this release. Only templateID (or a templateName
// that is a VMID) is acted on; storage, node, format and fullClone are accepted
// but not acted on in this release.
type ProxmoxImageSource struct {
	// TemplateID specifies an existing Proxmox template VMID. A VM is cloned from
	// it with a full clone. Nothing is prepared for this source.
	// +optional
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=999999999
	TemplateID *int `json:"templateID,omitempty"`

	// TemplateName specifies an existing Proxmox template. The clone is made by
	// VMID only, so the value must be the template's VMID as a number; any other
	// name makes the VM create fail with InvalidSpec. Use templateID.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	TemplateName string `json:"templateName,omitempty"`

	// Storage specifies the Proxmox storage for cloning, for example "local-lvm".
	// Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Storage string `json:"storage,omitempty"`

	// Node specifies the Proxmox node where the template exists. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Node string `json:"node,omitempty"`

	// Format specifies the disk format. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:default="qcow2"
	// +kubebuilder:validation:Enum=raw;qcow2;vmdk
	Format string `json:"format,omitempty"`

	// FullClone determines if this should be a full clone (default) or linked
	// clone. Accepted but not acted on in this release: the provider always makes
	// a full clone.
	// +optional
	// +kubebuilder:default=true
	FullClone *bool `json:"fullClone,omitempty"`
}

// ImageFormat represents the format of a VM image. The values are descriptive
// in this release: no provider acts on an image format declared in a VMImage.
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

// ChecksumType represents the checksum algorithm. The vSphere and libvirt
// providers verify an image's checksum with it when they prepare the image.
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

// ImageMetadata contains image metadata and annotations. Descriptive: no
// controller or provider reads any field of it.
type ImageMetadata struct {
	// DisplayName is a human-readable name for the image. Descriptive: no
	// controller reads it.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	DisplayName string `json:"displayName,omitempty"`

	// Description provides a description of the image. Descriptive: no
	// controller reads it.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// Version specifies the image version. Descriptive: no controller reads it.
	// +optional
	// +kubebuilder:validation:MaxLength=100
	Version string `json:"version,omitempty"`

	// Architecture specifies the CPU architecture. Descriptive: no controller or
	// provider checks it against the hypervisor.
	// +optional
	// +kubebuilder:default="amd64"
	// +kubebuilder:validation:Enum=amd64;arm64;x86_64;aarch64
	Architecture string `json:"architecture,omitempty"`

	// Tags are key-value pairs for categorizing the image. Descriptive: no
	// controller reads them.
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Tags map[string]string `json:"tags,omitempty"`

	// Annotations are additional metadata annotations. Descriptive: no
	// controller reads them, and they are not copied to the image on the
	// hypervisor.
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Annotations map[string]string `json:"annotations,omitempty"`
}

// OSDistribution contains OS distribution information. Descriptive: no
// controller or provider reads any field of it.
type OSDistribution struct {
	// Name is the OS distribution name. Descriptive: no controller reads it.
	// +optional
	// +kubebuilder:validation:Enum=ubuntu;centos;rhel;fedora;debian;suse;windows;freebsd;coreos;other
	Name string `json:"name,omitempty"`

	// Version is the distribution version. Descriptive: no controller reads it.
	// +optional
	// +kubebuilder:validation:MaxLength=100
	Version string `json:"version,omitempty"`

	// Variant is the distribution variant (e.g., server, desktop). Descriptive:
	// no controller reads it.
	// +optional
	// +kubebuilder:validation:MaxLength=100
	Variant string `json:"variant,omitempty"`

	// Family is the OS family. Descriptive: no controller reads it.
	// +optional
	// +kubebuilder:validation:Enum=linux;windows;bsd;other
	Family string `json:"family,omitempty"`

	// Kernel specifies kernel information. Descriptive: no controller reads it.
	// +optional
	Kernel *KernelInfo `json:"kernel,omitempty"`
}

// KernelInfo contains kernel information. Descriptive: no controller reads it.
type KernelInfo struct {
	// Version is the kernel version. Descriptive: no controller reads it.
	// +optional
	Version string `json:"version,omitempty"`

	// Type is the kernel type. Descriptive: no controller reads it.
	// +optional
	// +kubebuilder:validation:Enum=linux;windows;freebsd;other
	Type string `json:"type,omitempty"`
}

// ImagePrepare defines optional image preparation steps. Only onMissing and
// timeout are acted on. validateChecksum, retries, force, storage and
// optimization are accepted but not acted on in this release.
type ImagePrepare struct {
	// OnMissing defines the action to take when the image is not yet prepared
	// on the Provider of the VM being created. Import (the default) prepares
	// it, then creates the VM. Fail and Wait never prepare: the VM is held, and
	// the VMImage is marked Failed or Pending. It applies only to a source that
	// must be imported (a vSphere ovaURL, a libvirt url); an image that names
	// something already on the hypervisor is used as written whatever this says.
	// See docs/image-preparation.md.
	// +optional
	// +kubebuilder:default="Import"
	OnMissing ImageMissingAction `json:"onMissing,omitempty"`

	// ValidateChecksum validates the image checksum. Accepted but not acted on
	// in this release: the vSphere and libvirt providers verify the checksum of
	// an image they prepare whenever source.*.checksum is set.
	// +optional
	// +kubebuilder:default=true
	ValidateChecksum bool `json:"validateChecksum,omitempty"`

	// Timeout defines the maximum time to wait for preparation. The libvirt
	// provider uses it as the time limit of the download. The manager and the
	// vSphere and libvirt providers also use twice this value, with a floor of
	// two hours, as the time after which a prepare started by another request
	// that has not finished is treated as abandoned (stalled).
	// +optional
	// +kubebuilder:default="30m"
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Retries defines the number of retry attempts for failed operations.
	// Accepted but not acted on in this release: the manager retries a failed
	// prepare on its own schedule.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	Retries *int32 `json:"retries,omitempty"`

	// Force forces re-import even if image exists. Accepted but not acted on in
	// this release: an existing prepared image for the same source is reused.
	// +optional
	Force bool `json:"force,omitempty"`

	// Storage defines storage-specific preparation options. Accepted but not
	// acted on in this release: the libvirt storage pool comes from
	// source.libvirt.storagePool, and the vSphere import folder from the
	// Provider's spec.defaults.folder.
	// +optional
	Storage *StoragePrepareOptions `json:"storage,omitempty"`

	// Optimization defines image optimization options. Accepted but not acted on
	// in this release.
	// +optional
	Optimization *ImageOptimization `json:"optimization,omitempty"`
}

// ImageMissingAction defines actions to take when an image is not yet prepared
// on the Provider of the VM being created.
// +kubebuilder:validation:Enum=Import;Fail;Wait
type ImageMissingAction string

const (
	// ImageMissingActionImport prepares the image on the Provider, then creates
	// the VM
	ImageMissingActionImport ImageMissingAction = "Import"
	// ImageMissingActionFail does not prepare the image: the VMImage reports
	// Ready=False with phase Failed and the VM is held until the image is prepared
	// out of band
	ImageMissingActionFail ImageMissingAction = "Fail"
	// ImageMissingActionWait does not prepare the image: the VMImage reports
	// Ready=False with phase Pending and the VM is held until the image becomes
	// available out of band
	ImageMissingActionWait ImageMissingAction = "Wait"
)

// StoragePrepareOptions defines storage-specific preparation options. Accepted
// but not acted on in this release: no controller or provider reads any field
// of it.
type StoragePrepareOptions struct {
	// VSphere storage options. Accepted but not acted on in this release.
	// +optional
	VSphere *VSphereStorageOptions `json:"vsphere,omitempty"`

	// Libvirt storage options. Accepted but not acted on in this release.
	// +optional
	Libvirt *LibvirtStorageOptions `json:"libvirt,omitempty"`

	// PreferredFormat specifies the preferred target format. Accepted but not
	// acted on in this release.
	// +optional
	PreferredFormat ImageFormat `json:"preferredFormat,omitempty"`

	// Compression enables compression during import. Accepted but not acted on
	// in this release.
	// +optional
	// +kubebuilder:default=false
	Compression bool `json:"compression,omitempty"`
}

// VSphereStorageOptions defines vSphere storage preparation options. Accepted
// but not acted on in this release.
type VSphereStorageOptions struct {
	// Datastore specifies the target datastore for import. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Datastore string `json:"datastore,omitempty"`

	// Folder specifies the target folder for import. Accepted but not acted on
	// in this release: an OVA is imported into the Provider's spec.defaults.folder.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Folder string `json:"folder,omitempty"`

	// ThinProvisioned indicates whether to use thin provisioning. Accepted but
	// not acted on in this release.
	// +optional
	ThinProvisioned *bool `json:"thinProvisioned,omitempty"`

	// DiskType specifies the disk provisioning type. Accepted but not acted on
	// in this release.
	// +optional
	// +kubebuilder:validation:Enum=thin;thick;eagerzeroedthick
	DiskType string `json:"diskType,omitempty"`
}

// LibvirtStorageOptions defines Libvirt storage preparation options. Accepted
// but not acted on in this release.
type LibvirtStorageOptions struct {
	// StoragePool specifies the target storage pool for import. Accepted but not
	// acted on in this release: use source.libvirt.storagePool.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	StoragePool string `json:"storagePool,omitempty"`

	// AllocationPolicy defines how storage is allocated. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:validation:Enum=eager;lazy
	AllocationPolicy string `json:"allocationPolicy,omitempty"`

	// Preallocation specifies preallocation mode. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:Enum=off;metadata;falloc;full
	Preallocation string `json:"preallocation,omitempty"`
}

// ImageOptimization defines image optimization options. Accepted but not acted
// on in this release: no controller or provider reads any field of it.
type ImageOptimization struct {
	// EnableCompression enables image compression. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:default=false
	EnableCompression bool `json:"enableCompression,omitempty"`

	// RemoveUnusedSpace removes unused space from the image. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:default=false
	RemoveUnusedSpace bool `json:"removeUnusedSpace,omitempty"`

	// ConvertFormat converts the image to a more optimal format. Accepted but not
	// acted on in this release.
	// +optional
	ConvertFormat ImageFormat `json:"convertFormat,omitempty"`

	// EnableDeltaSync enables delta synchronization for updates. Accepted but
	// not acted on in this release.
	// +optional
	// +kubebuilder:default=false
	EnableDeltaSync bool `json:"enableDeltaSync,omitempty"`
}

// VMImageStatus defines the observed state of VMImage. It is written only by the
// operator's VirtualMachine controller, when it prepares the image for a VM, so
// an image that needs no preparation (it names a template or a libvirt path
// already on the hypervisor) has no status. size, checksum, format and
// importProgress are not populated in this release.
type VMImageStatus struct {
	// Ready is true once the image is prepared on at least one Provider (see
	// providerStatus for the state on each).
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Phase represents the current phase of image preparation: Pending,
	// Importing, Ready or Failed. Downloading, Converting and Optimizing are
	// never set in this release.
	// +optional
	Phase ImagePhase `json:"phase,omitempty"`

	// Message provides additional details about the current state, for example
	// why a prepare is held or failed.
	// +optional
	Message string `json:"message,omitempty"`

	// AvailableOn lists the Providers the image is available on, each as
	// "<namespace>/<name>" (the ProviderStatus keys whose entry is available).
	// It is informational: creating a VM consults ProviderStatus only.
	// +optional
	AvailableOn []string `json:"availableOn,omitempty"`

	// Conditions represent the latest available observations. The Ready and
	// Importing conditions are set; Downloading and Validated are never set in
	// this release.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is not populated in this release; each condition
	// carries its own observedGeneration.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastPrepareTime records when a prepare was last started (an asynchronous
	// prepare in flight) or completed.
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

	// ImportProgress shows the progress of image import operations. Not
	// populated in this release.
	// +optional
	ImportProgress *ImageImportProgress `json:"importProgress,omitempty"`

	// Size is the size of the prepared image. Not populated in this release, so
	// the Size column of kubectl get is empty.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// Checksum is the actual checksum of the prepared image. Not populated in
	// this release.
	// +optional
	Checksum string `json:"checksum,omitempty"`

	// Format is the actual format of the prepared image. Not populated in this
	// release.
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

// ImagePhase represents the phase of image preparation. The controller sets
// Pending, Importing, Ready and Failed; Downloading, Converting and Optimizing
// are never set in this release.
// +kubebuilder:validation:Enum=Pending;Downloading;Importing;Converting;Optimizing;Ready;Failed
type ImagePhase string

const (
	// ImagePhasePending indicates the image is waiting to be processed
	ImagePhasePending ImagePhase = "Pending"
	// ImagePhaseDownloading is never set in this release
	ImagePhaseDownloading ImagePhase = "Downloading"
	// ImagePhaseImporting indicates the image is being imported
	ImagePhaseImporting ImagePhase = "Importing"
	// ImagePhaseConverting is never set in this release
	ImagePhaseConverting ImagePhase = "Converting"
	// ImagePhaseOptimizing is never set in this release
	ImagePhaseOptimizing ImagePhase = "Optimizing"
	// ImagePhaseReady indicates the image is ready for use
	ImagePhaseReady ImagePhase = "Ready"
	// ImagePhaseFailed indicates the image preparation failed
	ImagePhaseFailed ImagePhase = "Failed"
)

// ImageImportProgress tracks the progress of image import operations. Not
// populated in this release.
type ImageImportProgress struct {
	// TotalBytes is the total size of the image being imported. Not populated in
	// this release.
	// +optional
	TotalBytes *int64 `json:"totalBytes,omitempty"`

	// TransferredBytes is the number of bytes transferred so far. Not populated
	// in this release.
	// +optional
	TransferredBytes *int64 `json:"transferredBytes,omitempty"`

	// Percentage is the completion percentage (0-100). Not populated in this
	// release.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Percentage *int32 `json:"percentage,omitempty"`

	// TransferRate is the current transfer rate in bytes per second. Not
	// populated in this release.
	// +optional
	TransferRate *int64 `json:"transferRate,omitempty"`

	// ETA is the estimated time to completion. Not populated in this release.
	// +optional
	ETA *metav1.Duration `json:"eta,omitempty"`

	// StartTime is when the import started. Not populated in this release.
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

	// SourceDigest is the digest of the spec.source this entry was prepared
	// for ("sha256:" and 64 lowercase hex digits), as confirmed by the
	// provider's stamp on the prepared artifact. A VirtualMachine is
	// created from the entry only while it equals the digest of the current
	// spec.source; an entry without it (recorded by an earlier release) or with
	// another digest is not used, and the image is prepared again for the
	// current source when spec.prepare.onMissing allows it.
	// +optional
	// +kubebuilder:validation:MaxLength=71
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	SourceDigest string `json:"sourceDigest,omitempty"`

	// ID is the provider-specific identifier of the prepared image (on vSphere,
	// the template's absolute inventory path).
	// +optional
	ID string `json:"id,omitempty"`

	// Path is the provider-specific location of the prepared image (on libvirt,
	// the file in the storage pool).
	// +optional
	Path string `json:"path,omitempty"`

	// Size is the image size on this provider. Not populated in this release.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// LastUpdated is when this entry was last updated
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`

	// Message provides provider-specific status information, for example why
	// the image was held or refused on this Provider
	// +optional
	Message string `json:"message,omitempty"`
}

// VMImage condition types
const (
	// VMImageConditionReady indicates whether the image is ready
	VMImageConditionReady = "Ready"
	// VMImageConditionDownloading is never set in this release
	VMImageConditionDownloading = "Downloading"
	// VMImageConditionImporting indicates whether a prepare of the image is in
	// flight
	VMImageConditionImporting = "Importing"
	// VMImageConditionValidated is never set in this release
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

// VMImage describes where a VM image comes from and records where it has been
// prepared. VirtualMachines use it through spec.imageRef. An image that must be
// imported (a vSphere ovaURL, a libvirt url) is prepared on a Provider when the
// first VM that uses it on that Provider is created; an image that names
// something already on the hypervisor (a template, a libvirt path) is used as
// written.
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
