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

// VMCloneFinalizer is the finalizer the VMClone controller puts on every
// VMClone. Deleting a VMClone only removes the VMClone: the target
// VirtualMachine it produced is kept.
const VMCloneFinalizer = "clone.infra.virtrigaud.io/finalizer"

// VMCloneSpec defines the desired state of VMClone. Fields that are
// accepted by the schema but not acted on in this release are marked as
// such.
type VMCloneSpec struct {
	// Source defines the VirtualMachine to clone. Only vmRef is supported.
	Source CloneSource `json:"source"`

	// Target defines the VirtualMachine the clone produces. The controller
	// creates it before it asks the Provider to clone, and binds it to the
	// cloned machine once the clone is made.
	Target VMCloneTarget `json:"target"`

	// Options defines cloning options. Only type and powerOn are acted on;
	// every other option is accepted but not acted on in this release.
	// +optional
	Options *CloneOptions `json:"options,omitempty"`

	// Customization defines how the cloned machine is customized. Only
	// hostname, domain and userData.cloudInit.inline are acted on, and only
	// by some providers and only for a source VM that carries cloud-init data
	// (a NoCloud seed on libvirt, cloud-init guestinfo keys on vSphere); see
	// the field descriptions. Every other field of the block is accepted but
	// not acted on in this release. The fields among them that hold, or
	// commonly hold, secret values (sysprep.productKey,
	// sysprep.adminPassword.value, sysprep.joinDomain.password.value,
	// sysprep.customCommands, certificates[].data, guestCommands[].command
	// and .arguments, userData.ignition.inline) are never sent to the
	// provider, but are stored in plaintext in the VMClone: do not put real
	// credentials there.
	// +optional
	Customization *VMCustomization `json:"customization,omitempty"`

	// Metadata contains clone operation metadata. Accepted but not acted on
	// in this release. No controller or provider reads it.
	// +optional
	Metadata *CloneMetadata `json:"metadata,omitempty"`
}

// CloneSource defines the VirtualMachine to clone. Only vmRef is supported:
// a VMClone without vmRef fails with reason UnsupportedSource. snapshotRef,
// templateRef and imageRef are accepted but not acted on in this release.
type CloneSource struct {
	// VMRef names the VirtualMachine to clone, in the VMClone's namespace.
	// The clone waits (Pending) until that VirtualMachine exists and has been
	// created on its Provider, and is made on that Provider.
	// +optional
	VMRef *LocalObjectReference `json:"vmRef,omitempty"`

	// SnapshotRef references a snapshot to clone from. Accepted but not acted
	// on in this release.
	// +optional
	SnapshotRef *LocalObjectReference `json:"snapshotRef,omitempty"`

	// TemplateRef references a VM template to clone from. Accepted but not
	// acted on in this release.
	// +optional
	TemplateRef *ObjectRef `json:"templateRef,omitempty"`

	// ImageRef references a VM image to clone from. Accepted but not acted on
	// in this release.
	// +optional
	ImageRef *ObjectRef `json:"imageRef,omitempty"`
}

// VMCloneTarget defines the VirtualMachine a clone produces.
type VMCloneTarget struct {
	// Name is the name of the target VirtualMachine.
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Namespace is the namespace for the target VM (defaults to the VMClone's
	// namespace, which is also the source VM's). A different namespace is
	// allowed only when that Namespace object carries the annotation
	// infra.virtrigaud.io/allowed-source-namespaces and its comma-separated
	// value lists the VMClone's namespace (exact names, no wildcards).
	// Whoever can update the target Namespace can grant this (normally a
	// cluster administrator, since Namespaces are cluster-scoped). Without it
	// the clone is refused (Ready=False, reason TargetNamespaceNotAllowed) and
	// nothing is created in, read from or bound in the target namespace;
	// granting the annotation or changing this field resumes it.
	// +optional
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`

	// ProviderRef references the target provider. Accepted but not acted on
	// in this release. The target VirtualMachine always uses the source
	// VirtualMachine's Provider: a clone is made on the Provider its source
	// runs on.
	// +optional
	ProviderRef *ObjectRef `json:"providerRef,omitempty"`

	// ClassRef names the VMClass of the target VirtualMachine, in the
	// VMClone's namespace. When unset, the target uses the source
	// VirtualMachine's class. The class's CPU and memory are also sent with
	// the Clone call: libvirt applies them, Proxmox applies only the CPU
	// count, and vSphere ignores them at clone time.
	// +optional
	ClassRef *LocalObjectReference `json:"classRef,omitempty"`

	// PlacementRef is copied to the target VirtualMachine's
	// spec.placementRef. It does not place the clone: the clone is made on
	// the source VirtualMachine's Provider (on a clustered Provider, on the
	// source's host), and no provider acts on it at clone time.
	// +optional
	PlacementRef *LocalObjectReference `json:"placementRef,omitempty"`

	// Networks is copied to the target VirtualMachine's spec.networks. It is
	// not applied to the cloned machine at clone time: no provider changes
	// the network devices the clone inherits from its source.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	Networks []VMNetworkRef `json:"networks,omitempty"`

	// Disks defines disk configuration overrides. Accepted but not acted on
	// in this release. The cloned machine keeps the source's disks.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Disks []DiskSpec `json:"disks,omitempty"`

	// Labels defines labels to apply to the target VirtualMachine. The
	// controller always sets virtrigaud.io/adopted=true on it, replacing any
	// value given here.
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations defines annotations to apply to the target VM. Keys in the
	// operator's reserved virtrigaud.io domain (virtrigaud.io/... and
	// *.virtrigaud.io/...), such as virtrigaud.io/orphan-on-delete or the
	// provenance annotations, are not copied; the controller sets its own
	// provenance annotations.
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Annotations map[string]string `json:"annotations,omitempty"`
}

// CloneOptions defines cloning options. Only type and powerOn are acted on;
// every other option is accepted but not acted on in this release.
type CloneOptions struct {
	// Type specifies the clone type. FullClone (the default) copies the
	// disks. LinkedClone makes a copy-on-write clone that keeps depending on
	// the source's disk; it is sent only to a Provider that reports
	// linked-clone support (vSphere and Proxmox; the mock provider accepts it
	// too). On Proxmox a linked clone needs a source that is a template. On
	// libvirt the VMClone fails with reason LinkedCloneUnsupported (or
	// LinkedCloneCrossNamespace when the target is in another namespace).
	// InstantClone is accepted but not acted on in this release: a full clone
	// is made. status.actualCloneType reports what was made.
	// +optional
	// +kubebuilder:default="FullClone"
	// +kubebuilder:validation:Enum=FullClone;LinkedClone;InstantClone
	Type CloneType `json:"type,omitempty"`

	// PowerOn indicates whether to power on the target VirtualMachine after
	// the clone. The clone itself is made powered off; the target
	// VirtualMachine is created with spec.powerState On (true) or Off (false,
	// the default), and the VirtualMachine controller powers it on.
	// +optional
	// +kubebuilder:default=false
	PowerOn bool `json:"powerOn,omitempty"`

	// IncludeSnapshots indicates whether to include snapshots in the clone.
	// Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default=false
	IncludeSnapshots bool `json:"includeSnapshots,omitempty"`

	// Parallel enables parallel disk cloning. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:default=false
	Parallel bool `json:"parallel,omitempty"`

	// Timeout defines the maximum time to wait for clone completion. Accepted
	// but not acted on in this release. The default is not enforced.
	// +optional
	// +kubebuilder:default="30m"
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// RetryPolicy defines retry behavior for failed clones. Accepted but not
	// acted on in this release. The controller re-sends a Clone with a built-in
	// backoff (15 seconds doubling to 5 minutes) when the provider reports that
	// it did nothing, or when the outcome is unknown and the provider recognises
	// a clone it already made. It waits, without failing, for a running source
	// or a blocked host. Any other failure fails the clone, and Failed is
	// terminal.
	// +optional
	RetryPolicy *CloneRetryPolicy `json:"retryPolicy,omitempty"`

	// Storage defines storage-specific clone options. Accepted but not acted
	// on in this release. The provider decides where the clone is stored.
	// +optional
	Storage *CloneStorageOptions `json:"storage,omitempty"`

	// Performance defines performance-related clone options. Accepted but not
	// acted on in this release.
	// +optional
	Performance *ClonePerformanceOptions `json:"performance,omitempty"`
}

// CloneType represents the type of clone operation. FullClone and
// LinkedClone are acted on; InstantClone is accepted but made as a full
// clone.
// +kubebuilder:validation:Enum=FullClone;LinkedClone;InstantClone
type CloneType string

const (
	// CloneTypeFullClone creates a full independent copy of the source's
	// disks.
	CloneTypeFullClone CloneType = "FullClone"
	// CloneTypeLinkedClone creates a copy-on-write clone that depends on the
	// source's disk. The vSphere and Proxmox providers support it (Proxmox from
	// a template source only); libvirt does not.
	CloneTypeLinkedClone CloneType = "LinkedClone"
	// CloneTypeInstantClone is accepted but not acted on in this release: a
	// full clone is made.
	CloneTypeInstantClone CloneType = "InstantClone"
)

// CloneRetryPolicy defines retry behavior for failed clones. Accepted but
// not acted on in this release.
type CloneRetryPolicy struct {
	// MaxRetries is the maximum number of retry attempts. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	MaxRetries *int32 `json:"maxRetries,omitempty"`

	// RetryDelay is the delay between retry attempts. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:default="5m"
	RetryDelay *metav1.Duration `json:"retryDelay,omitempty"`

	// BackoffMultiplier is the multiplier for exponential backoff. Accepted
	// but not acted on in this release.
	// +optional
	// +kubebuilder:default=2
	BackoffMultiplier *int32 `json:"backoffMultiplier,omitempty"`
}

// CloneStorageOptions defines storage-specific clone options. Accepted but
// not acted on in this release.
type CloneStorageOptions struct {
	// PreferThinProvisioning prefers thin provisioning for cloned disks.
	// Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default=true
	PreferThinProvisioning bool `json:"preferThinProvisioning,omitempty"`

	// DiskFormat specifies the preferred disk format for cloned disks.
	// Accepted but not acted on in this release.
	// +optional
	DiskFormat DiskType `json:"diskFormat,omitempty"`

	// StorageClass specifies the storage class for cloned disks. Accepted but
	// not acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	StorageClass string `json:"storageClass,omitempty"`

	// Datastore specifies the target datastore for cloned disks. Accepted but
	// not acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Datastore string `json:"datastore,omitempty"`

	// Folder specifies the target folder for the cloned VM. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Folder string `json:"folder,omitempty"`

	// EnableCompression enables compression during clone operations. Accepted
	// but not acted on in this release.
	// +optional
	// +kubebuilder:default=false
	EnableCompression bool `json:"enableCompression,omitempty"`
}

// ClonePerformanceOptions defines performance-related clone options.
// Accepted but not acted on in this release.
type ClonePerformanceOptions struct {
	// Priority specifies the clone operation priority. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:default="Normal"
	// +kubebuilder:validation:Enum=Low;Normal;High
	Priority string `json:"priority,omitempty"`

	// IOThrottling enables I/O throttling during clone operations. Accepted
	// but not acted on in this release.
	// +optional
	// +kubebuilder:default=false
	IOThrottling bool `json:"ioThrottling,omitempty"`

	// MaxIOPS limits the maximum IOPS during clone operations. Accepted but
	// not acted on in this release.
	// +optional
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=100000
	MaxIOPS *int32 `json:"maxIOPS,omitempty"`

	// ConcurrentDisks limits the number of disks cloned concurrently.
	// Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	ConcurrentDisks *int32 `json:"concurrentDisks,omitempty"`
}

// VMCustomization defines how the cloned machine is customized. Only
// hostname, domain and userData.cloudInit.inline are acted on. Every other
// field is accepted but not acted on in this release; the libvirt and
// vSphere providers log the customization fields they do not apply. The
// manager sends the provider only the fields a provider reads or reports:
// the secret values (sysprep.productKey, sysprep.adminPassword.value,
// sysprep.joinDomain.password.value, certificates[].data), the commands
// (sysprep.customCommands, guestCommands[].command and .arguments) and
// userData.ignition.inline are never sent.
type VMCustomization struct {
	// Hostname sets the hostname of the cloned machine, through the clone's
	// cloud-init identity. It is applied by libvirt (when the source boots
	// from a NoCloud seed) and vSphere (when the source carries cloud-init
	// guestinfo metadata, or a hostname in its guestinfo user-data); Proxmox and
	// mock do not apply it. When unset, the
	// hostname is the target VirtualMachine's name.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern="^[a-zA-Z0-9]([a-zA-Z0-9\\-]*[a-zA-Z0-9])?$"
	Hostname string `json:"hostname,omitempty"`

	// Domain sets the domain name; with the hostname it forms the clone's
	// FQDN. It is applied by libvirt and vSphere on the same terms as
	// hostname; Proxmox and mock do not apply it.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Domain string `json:"domain,omitempty"`

	// TimeZone sets the timezone. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxLength=100
	TimeZone string `json:"timeZone,omitempty"`

	// Networks defines network customization. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	Networks []NetworkCustomization `json:"networks,omitempty"`

	// UserData provides cloud-init user-data for the clone. Only
	// cloudInit.inline is applied, and only by libvirt: it replaces the
	// source's user-data in the clone's cloud-init seed (the source must have
	// one). cloudInit.secretRef is not resolved, so the clone keeps the
	// source's user-data, and ignition is not applied. vSphere, Proxmox and
	// mock do not apply user-data.
	// +optional
	UserData *UserData `json:"userData,omitempty"`

	// Sysprep provides Windows sysprep customization. Accepted but not acted
	// on in this release.
	// +optional
	Sysprep *SysprepCustomization `json:"sysprep,omitempty"`

	// Tags defines additional tags for the cloned VM. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Tags []string `json:"tags,omitempty"`

	// GuestCommands defines commands to run in the guest OS. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	GuestCommands []GuestCommand `json:"guestCommands,omitempty"`

	// Certificates defines certificates to install. Accepted but not acted on
	// in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	Certificates []CertificateSpec `json:"certificates,omitempty"`
}

// NetworkCustomization defines network-specific customization. Accepted but
// not acted on in this release.
type NetworkCustomization struct {
	// Name identifies the network to customize. Accepted but not acted on in
	// this release.
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`

	// IPAddress sets a static IP address. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$"
	IPAddress string `json:"ipAddress,omitempty"`

	// SubnetMask sets the subnet mask. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$"
	SubnetMask string `json:"subnetMask,omitempty"`

	// Gateway sets the network gateway. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$"
	Gateway string `json:"gateway,omitempty"`

	// DNS sets DNS servers. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxItems=5
	DNS []string `json:"dns,omitempty"`

	// MACAddress sets a custom MAC address. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:Pattern="^([0-9A-Fa-f]{2}[:-]){5}([0-9A-Fa-f]{2})$"
	MACAddress string `json:"macAddress,omitempty"`

	// DHCP enables DHCP for this network. Accepted but not acted on in this
	// release.
	// +optional
	DHCP bool `json:"dhcp,omitempty"`
}

// SysprepCustomization defines Windows sysprep customization. Accepted but
// not acted on in this release.
type SysprepCustomization struct {
	// Enabled indicates if sysprep should be run. Accepted but not acted on
	// in this release.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// ProductKey specifies the Windows product key. Accepted but not acted on
	// in this release.
	// WARNING: a value set here is stored in plaintext in the VMClone (etcd,
	// backups, anyone who can read the object). It is accepted but not acted
	// on in this release and is not sent to the provider; do not put a real
	// product key here.
	// +optional
	ProductKey string `json:"productKey,omitempty"`

	// Organization specifies the organization name. Accepted but not acted on
	// in this release.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Organization string `json:"organization,omitempty"`

	// Owner specifies the owner name. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Owner string `json:"owner,omitempty"`

	// AdminPassword specifies the administrator password. Accepted but not
	// acted on in this release.
	// +optional
	AdminPassword *PasswordSpec `json:"adminPassword,omitempty"`

	// JoinDomain specifies domain join configuration. Accepted but not acted
	// on in this release.
	// +optional
	JoinDomain *DomainJoinSpec `json:"joinDomain,omitempty"`

	// CustomCommands specifies custom commands to run during sysprep.
	// Accepted but not acted on in this release, and not sent to the
	// provider. They are stored in plaintext in the VMClone: do not put a
	// credential in them.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	CustomCommands []string `json:"customCommands,omitempty"`
}

// PasswordSpec defines password configuration. Accepted but not acted on in
// this release.
type PasswordSpec struct {
	// Value is the plaintext password (not recommended for production).
	// Accepted but not acted on in this release.
	// WARNING: a value set here is stored in plaintext in the VMClone (etcd,
	// backups, anyone who can read the object). It is accepted but not acted
	// on in this release and is not sent to the provider; do not put a real
	// password here.
	// +optional
	Value string `json:"value,omitempty"`

	// SecretRef references a secret containing the password. Accepted but not
	// acted on in this release.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`

	// SecretKey is the key in the secret containing the password. Accepted
	// but not acted on in this release.
	// +optional
	// +kubebuilder:default="password"
	SecretKey string `json:"secretKey,omitempty"`
}

// DomainJoinSpec defines domain join configuration. Accepted but not acted
// on in this release.
type DomainJoinSpec struct {
	// Domain is the domain name to join. Accepted but not acted on in this
	// release.
	// +kubebuilder:validation:MaxLength=255
	Domain string `json:"domain"`

	// Username is the domain join username. Accepted but not acted on in this
	// release.
	// +kubebuilder:validation:MaxLength=255
	Username string `json:"username"`

	// Password is the domain join password. Accepted but not acted on in this
	// release.
	Password PasswordSpec `json:"password"`

	// OrganizationalUnit specifies the OU for the computer account. Accepted
	// but not acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	OrganizationalUnit string `json:"organizationalUnit,omitempty"`
}

// GuestCommand defines a command to run in the guest OS. Accepted but not
// acted on in this release.
type GuestCommand struct {
	// Command is the command to execute. Accepted but not acted on in this
	// release, and not sent to the provider. It is stored in plaintext in the
	// VMClone: do not put a credential in it.
	// +kubebuilder:validation:MaxLength=1000
	Command string `json:"command"`

	// Arguments contains command arguments. Accepted but not acted on in this
	// release, and not sent to the provider. They are stored in plaintext in
	// the VMClone: do not put a credential in them.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Arguments []string `json:"arguments,omitempty"`

	// WorkingDirectory specifies the working directory. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	WorkingDirectory string `json:"workingDirectory,omitempty"`

	// RunAsUser specifies the user to run the command as. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	RunAsUser string `json:"runAsUser,omitempty"`

	// Timeout specifies the command timeout. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:default="5m"
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Stage specifies when to run the command. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:default="post-customization"
	// +kubebuilder:validation:Enum=pre-customization;post-customization;first-boot
	Stage string `json:"stage,omitempty"`
}

// CertificateSpec defines a certificate to install. Accepted but not acted
// on in this release.
type CertificateSpec struct {
	// Name is the certificate name. Accepted but not acted on in this
	// release.
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`

	// Data contains the certificate data (PEM format). Accepted but not acted
	// on in this release.
	// WARNING: a value set here is stored in plaintext in the VMClone (etcd,
	// backups, anyone who can read the object). It is accepted but not acted
	// on in this release and is not sent to the provider; do not put a real
	// certificate private key here.
	// +optional
	Data string `json:"data,omitempty"`

	// SecretRef references a secret containing the certificate. Accepted but
	// not acted on in this release.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`

	// SecretKey is the key in the secret containing the certificate. Accepted
	// but not acted on in this release.
	// +optional
	// +kubebuilder:default="tls.crt"
	SecretKey string `json:"secretKey,omitempty"`

	// Store specifies the certificate store. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:default="root"
	// +kubebuilder:validation:Enum=root;ca;my;trust
	Store string `json:"store,omitempty"`
}

// CloneMetadata contains clone operation metadata. Accepted but not acted
// on in this release. No controller or provider reads it.
type CloneMetadata struct {
	// Purpose describes the purpose of the clone. Accepted but not acted on
	// in this release.
	// +optional
	// +kubebuilder:validation:Enum=backup;testing;migration;development;production;staging
	Purpose string `json:"purpose,omitempty"`

	// CreatedBy identifies who or what created the clone. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	CreatedBy string `json:"createdBy,omitempty"`

	// Project identifies the project this clone belongs to. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Project string `json:"project,omitempty"`

	// Environment specifies the environment. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:Enum=dev;staging;prod;test;qa;uat
	Environment string `json:"environment,omitempty"`

	// Tags are key-value pairs for categorizing the clone. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Tags map[string]string `json:"tags,omitempty"`
}

// VMCloneStatus defines the observed state of VMClone. Fields that the
// controller does not write are marked as not populated.
type VMCloneStatus struct {
	// TargetRef references the target VirtualMachine. It is set when the
	// clone is Ready.
	// +optional
	TargetRef *LocalObjectReference `json:"targetRef,omitempty"`

	// TargetVMID is the provider's identifier of the cloned machine, as
	// returned by the Clone call. The controller records it before it binds
	// the target VirtualMachine, so an interrupted clone resumes the binding
	// instead of cloning again, and writes it to the target VirtualMachine's
	// status.id.
	// +optional
	TargetVMID string `json:"targetVMID,omitempty"`

	// TargetUID is the uid of the target VirtualMachine this clone created
	// before it asked the Provider to clone. Only that object is ever used as
	// the clone's target or removed when the clone fails: a VirtualMachine
	// re-created later under the target name, even one carrying a copied
	// virtrigaud.io/clone-uid annotation, is never touched.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	TargetUID string `json:"targetUID,omitempty"`

	// Phase represents the current phase of the clone operation: Pending
	// (waiting for a prerequisite; the Ready condition says which), Cloning,
	// Ready or Failed. Failed is terminal: the clone is not retried, so
	// create a new VMClone to try again. Preparing, Customizing and
	// Powering-On are accepted by the schema but never set in this release.
	// +optional
	Phase ClonePhase `json:"phase,omitempty"`

	// Message provides additional details about the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// TaskRef is the provider's task reference for an asynchronous Clone call
	// that has not finished. It is empty otherwise, and providers that clone
	// synchronously never set it.
	// +optional
	TaskRef string `json:"taskRef,omitempty"`

	// StartTime is when the Clone call, which the provider then accepted, was
	// sent.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the clone became Ready. It is not set when the
	// clone fails.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// ActualCloneType indicates the clone type that was made, FullClone or
	// LinkedClone. It is set when the provider accepts the Clone call; a
	// requested InstantClone is reported as FullClone.
	// +optional
	ActualCloneType CloneType `json:"actualCloneType,omitempty"`

	// Progress shows the clone operation progress. Not populated in this
	// release. (The Progress printer column is therefore empty.)
	// +optional
	Progress *CloneProgress `json:"progress,omitempty"`

	// Conditions represent the latest observations of the clone. The
	// controller sets Ready, Cloning and Failed, TargetCleanup (what was done
	// with the target VirtualMachine of a failed clone) and CloneRequested (a
	// Clone call was sent and its outcome is not recorded yet). Customizing
	// is never set.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the generation of the VMClone last processed by
	// the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// RetryCount is the number of times the clone has been retried. Not
	// populated in this release.
	// +optional
	RetryCount int32 `json:"retryCount,omitempty"`

	// LastRetryTime is when the clone was last retried. Not populated in this
	// release.
	// +optional
	LastRetryTime *metav1.Time `json:"lastRetryTime,omitempty"`

	// CustomizationStatus contains customization operation status. Not
	// populated in this release.
	// +optional
	CustomizationStatus *CustomizationStatus `json:"customizationStatus,omitempty"`
}

// ClonePhase represents the phase of a clone operation.
// +kubebuilder:validation:Enum=Pending;Preparing;Cloning;Customizing;Powering-On;Ready;Failed
type ClonePhase string

const (
	// ClonePhasePending indicates the clone is waiting for a prerequisite,
	// such as its source VirtualMachine, its Provider or a source that must
	// be powered off. The Ready condition's reason and message say which.
	ClonePhasePending ClonePhase = "Pending"
	// ClonePhasePreparing is never set in this release.
	ClonePhasePreparing ClonePhase = "Preparing"
	// ClonePhaseCloning indicates the provider accepted the Clone call and
	// the target VirtualMachine is not bound to the cloned machine yet.
	ClonePhaseCloning ClonePhase = "Cloning"
	// ClonePhaseCustomizing is never set in this release.
	ClonePhaseCustomizing ClonePhase = "Customizing"
	// ClonePhasePoweringOn is never set in this release.
	ClonePhasePoweringOn ClonePhase = "Powering-On"
	// ClonePhaseReady indicates the clone is complete and the target
	// VirtualMachine is bound to the cloned machine.
	ClonePhaseReady ClonePhase = "Ready"
	// ClonePhaseFailed indicates the clone failed. It is terminal.
	ClonePhaseFailed ClonePhase = "Failed"
)

// CloneProgress shows the clone operation progress. Not populated in this
// release.
type CloneProgress struct {
	// TotalDisks is the total number of disks to clone. Not populated in this
	// release.
	// +optional
	TotalDisks int32 `json:"totalDisks,omitempty"`

	// CompletedDisks is the number of disks completed. Not populated in this
	// release.
	// +optional
	CompletedDisks int32 `json:"completedDisks,omitempty"`

	// CurrentDisk shows progress of the current disk being cloned. Not
	// populated in this release.
	// +optional
	CurrentDisk *DiskCloneProgress `json:"currentDisk,omitempty"`

	// OverallPercentage is the overall completion percentage (0-100). Not
	// populated in this release.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	OverallPercentage *int32 `json:"overallPercentage,omitempty"`

	// ETA is the estimated time to completion. Not populated in this release.
	// +optional
	ETA *metav1.Duration `json:"eta,omitempty"`
}

// DiskCloneProgress shows the progress of cloning a single disk. Not
// populated in this release.
type DiskCloneProgress struct {
	// Name is the disk name. Not populated in this release.
	Name string `json:"name"`

	// TotalBytes is the total size of the disk. Not populated in this
	// release.
	// +optional
	TotalBytes *int64 `json:"totalBytes,omitempty"`

	// CompletedBytes is the number of bytes completed. Not populated in this
	// release.
	// +optional
	CompletedBytes *int64 `json:"completedBytes,omitempty"`

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
}

// CustomizationStatus contains customization operation status. Not
// populated in this release.
type CustomizationStatus struct {
	// Started indicates if customization has started. Not populated in this
	// release.
	// +optional
	Started bool `json:"started,omitempty"`

	// Completed indicates if customization has completed. Not populated in
	// this release.
	// +optional
	Completed bool `json:"completed,omitempty"`

	// CompletedSteps lists completed customization steps. Not populated in
	// this release.
	// +optional
	CompletedSteps []string `json:"completedSteps,omitempty"`

	// FailedSteps lists failed customization steps. Not populated in this
	// release.
	// +optional
	FailedSteps []string `json:"failedSteps,omitempty"`

	// CurrentStep is the current customization step. Not populated in this
	// release.
	// +optional
	CurrentStep string `json:"currentStep,omitempty"`

	// Message provides customization status details. Not populated in this
	// release.
	// +optional
	Message string `json:"message,omitempty"`
}

// VMClone condition types
const (
	// VMCloneConditionReady indicates whether the clone is ready. It is True
	// when the clone is complete; when it is False, its reason and message
	// say what the clone is waiting for or why it failed.
	VMCloneConditionReady = "Ready"
	// VMCloneConditionCloning is True while the provider has accepted the
	// Clone call and the clone is not bound yet.
	VMCloneConditionCloning = "Cloning"
	// VMCloneConditionCustomizing is never set in this release.
	VMCloneConditionCustomizing = "Customizing"
	// VMCloneConditionFailed is True when the clone has failed.
	VMCloneConditionFailed = "Failed"
)

// VMClone condition reasons
const (
	// VMCloneReasonCompleted indicates the clone completed.
	VMCloneReasonCompleted = "Completed"
	// VMCloneReasonCloning indicates the clone is in progress, or is waiting
	// for its source VirtualMachine to be created on its Provider.
	VMCloneReasonCloning = "Cloning"
	// VMCloneReasonCustomizing is never set in this release.
	VMCloneReasonCustomizing = "Customizing"
	// VMCloneReasonProviderError indicates an error from the provider, or
	// that the Provider or the target VirtualMachine could not be used.
	VMCloneReasonProviderError = "ProviderError"
	// VMCloneReasonSourceNotFound indicates the source VirtualMachine does
	// not exist (yet); the clone waits for it.
	VMCloneReasonSourceNotFound = "SourceNotFound"
	// VMCloneReasonUnsupported indicates the Provider cannot perform the
	// clone: it does not support cloning, or a clustered Provider does not
	// route clones to a host.
	VMCloneReasonUnsupported = "Unsupported"
	// VMCloneReasonInsufficientResources is never set in this release.
	VMCloneReasonInsufficientResources = "InsufficientResources"
	// VMCloneReasonCustomizationFailed is never set in this release.
	VMCloneReasonCustomizationFailed = "CustomizationFailed"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source.vmRef.name`
//+kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.name`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Clone Type",type=string,JSONPath=`.status.actualCloneType`
//+kubebuilder:printcolumn:name="Progress",type=string,JSONPath=`.status.progress.overallPercentage`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
//+kubebuilder:resource:shortName=vmclone

// VMClone is the Schema for the vmclones API. It clones an existing
// VirtualMachine (spec.source.vmRef) into a new VirtualMachine on the same
// Provider, and binds the new VirtualMachine to the cloned machine.
// Deleting a VMClone does not delete the VirtualMachine it produced.
// +kubebuilder:storageversion
type VMClone struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VMCloneSpec   `json:"spec,omitempty"`
	Status VMCloneStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// VMCloneList contains a list of VMClone
type VMCloneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VMClone `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VMClone{}, &VMCloneList{})
}
