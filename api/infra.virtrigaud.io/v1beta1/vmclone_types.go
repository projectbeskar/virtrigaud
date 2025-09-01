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

// VMCloneSpec defines the desired state of VMClone
type VMCloneSpec struct {
	// Source defines the source for cloning
	Source CloneSource `json:"source"`

	// Target defines the target VM configuration
	Target VMCloneTarget `json:"target"`

	// Options defines cloning options
	// +optional
	Options *CloneOptions `json:"options,omitempty"`

	// Customization defines VM customization options
	// +optional
	Customization *VMCustomization `json:"customization,omitempty"`

	// Metadata contains clone operation metadata
	// +optional
	Metadata *CloneMetadata `json:"metadata,omitempty"`
}

// CloneSource defines the source for cloning
type CloneSource struct {
	// VMRef references the source virtual machine to clone
	// +optional
	VMRef *LocalObjectReference `json:"vmRef,omitempty"`

	// SnapshotRef references a specific snapshot to clone from
	// +optional
	SnapshotRef *LocalObjectReference `json:"snapshotRef,omitempty"`

	// TemplateRef references a VM template to clone from
	// +optional
	TemplateRef *ObjectRef `json:"templateRef,omitempty"`

	// ImageRef references a VM image to clone from
	// +optional
	ImageRef *ObjectRef `json:"imageRef,omitempty"`
}

// VMCloneTarget defines the target VM configuration
type VMCloneTarget struct {
	// Name is the name of the target VM
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Namespace is the namespace for the target VM (defaults to source namespace)
	// +optional
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`

	// ProviderRef references the target provider (defaults to source provider)
	// +optional
	ProviderRef *ObjectRef `json:"providerRef,omitempty"`

	// ClassRef references the VM class for resource allocation
	// +optional
	ClassRef *LocalObjectReference `json:"classRef,omitempty"`

	// PlacementRef references placement policy for the target VM
	// +optional
	PlacementRef *LocalObjectReference `json:"placementRef,omitempty"`

	// Networks defines network configuration overrides
	// +optional
	// +kubebuilder:validation:MaxItems=10
	Networks []VMNetworkRef `json:"networks,omitempty"`

	// Disks defines disk configuration overrides
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Disks []DiskSpec `json:"disks,omitempty"`

	// Labels defines labels to apply to the target VM
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations defines annotations to apply to the target VM
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Annotations map[string]string `json:"annotations,omitempty"`
}

// CloneOptions defines cloning options
type CloneOptions struct {
	// Type specifies the clone type
	// +optional
	// +kubebuilder:default="FullClone"
	// +kubebuilder:validation:Enum=FullClone;LinkedClone;InstantClone
	Type CloneType `json:"type,omitempty"`

	// PowerOn indicates whether to power on the cloned VM
	// +optional
	// +kubebuilder:default=false
	PowerOn bool `json:"powerOn,omitempty"`

	// IncludeSnapshots indicates whether to include snapshots in the clone
	// +optional
	// +kubebuilder:default=false
	IncludeSnapshots bool `json:"includeSnapshots,omitempty"`

	// Parallel enables parallel disk cloning (if supported)
	// +optional
	// +kubebuilder:default=false
	Parallel bool `json:"parallel,omitempty"`

	// Timeout defines the maximum time to wait for clone completion
	// +optional
	// +kubebuilder:default="30m"
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// RetryPolicy defines retry behavior for failed clones
	// +optional
	RetryPolicy *CloneRetryPolicy `json:"retryPolicy,omitempty"`

	// Storage defines storage-specific clone options
	// +optional
	Storage *CloneStorageOptions `json:"storage,omitempty"`

	// Performance defines performance-related clone options
	// +optional
	Performance *ClonePerformanceOptions `json:"performance,omitempty"`
}

// CloneType represents the type of clone operation
// +kubebuilder:validation:Enum=FullClone;LinkedClone;InstantClone
type CloneType string

const (
	// CloneTypeFullClone creates a full independent clone
	CloneTypeFullClone CloneType = "FullClone"
	// CloneTypeLinkedClone creates a linked clone sharing storage with parent
	CloneTypeLinkedClone CloneType = "LinkedClone"
	// CloneTypeInstantClone creates an instant clone (if supported)
	CloneTypeInstantClone CloneType = "InstantClone"
)

// CloneRetryPolicy defines retry behavior for failed clones
type CloneRetryPolicy struct {
	// MaxRetries is the maximum number of retry attempts
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	MaxRetries *int32 `json:"maxRetries,omitempty"`

	// RetryDelay is the delay between retry attempts
	// +optional
	// +kubebuilder:default="5m"
	RetryDelay *metav1.Duration `json:"retryDelay,omitempty"`

	// BackoffMultiplier is the multiplier for exponential backoff
	// +optional
	// +kubebuilder:default=2
	BackoffMultiplier *int32 `json:"backoffMultiplier,omitempty"`
}

// CloneStorageOptions defines storage-specific clone options
type CloneStorageOptions struct {
	// PreferThinProvisioning prefers thin provisioning for cloned disks
	// +optional
	// +kubebuilder:default=true
	PreferThinProvisioning bool `json:"preferThinProvisioning,omitempty"`

	// DiskFormat specifies the preferred disk format for cloned disks
	// +optional
	DiskFormat DiskType `json:"diskFormat,omitempty"`

	// StorageClass specifies the storage class for cloned disks
	// +optional
	// +kubebuilder:validation:MaxLength=253
	StorageClass string `json:"storageClass,omitempty"`

	// Datastore specifies the target datastore for cloned disks
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Datastore string `json:"datastore,omitempty"`

	// Folder specifies the target folder for the cloned VM
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Folder string `json:"folder,omitempty"`

	// EnableCompression enables compression during clone operations
	// +optional
	// +kubebuilder:default=false
	EnableCompression bool `json:"enableCompression,omitempty"`
}

// ClonePerformanceOptions defines performance-related clone options
type ClonePerformanceOptions struct {
	// Priority specifies the clone operation priority
	// +optional
	// +kubebuilder:default="Normal"
	// +kubebuilder:validation:Enum=Low;Normal;High
	Priority string `json:"priority,omitempty"`

	// IOThrottling enables I/O throttling during clone operations
	// +optional
	// +kubebuilder:default=false
	IOThrottling bool `json:"ioThrottling,omitempty"`

	// MaxIOPS limits the maximum IOPS during clone operations
	// +optional
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=100000
	MaxIOPS *int32 `json:"maxIOPS,omitempty"`

	// ConcurrentDisks limits the number of disks cloned concurrently
	// +optional
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	ConcurrentDisks *int32 `json:"concurrentDisks,omitempty"`
}

// VMCustomization defines VM customization options
type VMCustomization struct {
	// Hostname sets the target VM hostname
	// +optional
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern="^[a-zA-Z0-9]([a-zA-Z0-9\\-]*[a-zA-Z0-9])?$"
	Hostname string `json:"hostname,omitempty"`

	// Domain sets the domain name
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Domain string `json:"domain,omitempty"`

	// TimeZone sets the timezone
	// +optional
	// +kubebuilder:validation:MaxLength=100
	TimeZone string `json:"timeZone,omitempty"`

	// Networks defines network customization
	// +optional
	// +kubebuilder:validation:MaxItems=10
	Networks []NetworkCustomization `json:"networks,omitempty"`

	// UserData provides cloud-init or similar customization data
	// +optional
	UserData *UserData `json:"userData,omitempty"`

	// Sysprep provides Windows sysprep customization
	// +optional
	Sysprep *SysprepCustomization `json:"sysprep,omitempty"`

	// Tags defines additional tags for the cloned VM
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Tags []string `json:"tags,omitempty"`

	// GuestCommands defines commands to run in the guest OS
	// +optional
	// +kubebuilder:validation:MaxItems=20
	GuestCommands []GuestCommand `json:"guestCommands,omitempty"`

	// Certificates defines certificates to install
	// +optional
	// +kubebuilder:validation:MaxItems=10
	Certificates []CertificateSpec `json:"certificates,omitempty"`
}

// NetworkCustomization defines network-specific customization
type NetworkCustomization struct {
	// Name identifies the network to customize
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`

	// IPAddress sets a static IP address
	// +optional
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$"
	IPAddress string `json:"ipAddress,omitempty"`

	// SubnetMask sets the subnet mask
	// +optional
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$"
	SubnetMask string `json:"subnetMask,omitempty"`

	// Gateway sets the network gateway
	// +optional
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$"
	Gateway string `json:"gateway,omitempty"`

	// DNS sets DNS servers
	// +optional
	// +kubebuilder:validation:MaxItems=5
	DNS []string `json:"dns,omitempty"`

	// MACAddress sets a custom MAC address
	// +optional
	// +kubebuilder:validation:Pattern="^([0-9A-Fa-f]{2}[:-]){5}([0-9A-Fa-f]{2})$"
	MACAddress string `json:"macAddress,omitempty"`

	// DHCP enables DHCP for this network
	// +optional
	DHCP bool `json:"dhcp,omitempty"`
}

// SysprepCustomization defines Windows sysprep customization
type SysprepCustomization struct {
	// Enabled indicates if sysprep should be run
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// ProductKey specifies the Windows product key
	// +optional
	ProductKey string `json:"productKey,omitempty"`

	// Organization specifies the organization name
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Organization string `json:"organization,omitempty"`

	// Owner specifies the owner name
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Owner string `json:"owner,omitempty"`

	// AdminPassword specifies the administrator password
	// +optional
	AdminPassword *PasswordSpec `json:"adminPassword,omitempty"`

	// JoinDomain specifies domain join configuration
	// +optional
	JoinDomain *DomainJoinSpec `json:"joinDomain,omitempty"`

	// CustomCommands specifies custom commands to run during sysprep
	// +optional
	// +kubebuilder:validation:MaxItems=20
	CustomCommands []string `json:"customCommands,omitempty"`
}

// PasswordSpec defines password configuration
type PasswordSpec struct {
	// Value is the plaintext password (not recommended for production)
	// +optional
	Value string `json:"value,omitempty"`

	// SecretRef references a secret containing the password
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`

	// SecretKey is the key in the secret containing the password
	// +optional
	// +kubebuilder:default="password"
	SecretKey string `json:"secretKey,omitempty"`
}

// DomainJoinSpec defines domain join configuration
type DomainJoinSpec struct {
	// Domain is the domain name to join
	// +kubebuilder:validation:MaxLength=255
	Domain string `json:"domain"`

	// Username is the domain join username
	// +kubebuilder:validation:MaxLength=255
	Username string `json:"username"`

	// Password is the domain join password
	Password PasswordSpec `json:"password"`

	// OrganizationalUnit specifies the OU for the computer account
	// +optional
	// +kubebuilder:validation:MaxLength=500
	OrganizationalUnit string `json:"organizationalUnit,omitempty"`
}

// GuestCommand defines a command to run in the guest OS
type GuestCommand struct {
	// Command is the command to execute
	// +kubebuilder:validation:MaxLength=1000
	Command string `json:"command"`

	// Arguments contains command arguments
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Arguments []string `json:"arguments,omitempty"`

	// WorkingDirectory specifies the working directory
	// +optional
	// +kubebuilder:validation:MaxLength=500
	WorkingDirectory string `json:"workingDirectory,omitempty"`

	// RunAsUser specifies the user to run the command as
	// +optional
	// +kubebuilder:validation:MaxLength=255
	RunAsUser string `json:"runAsUser,omitempty"`

	// Timeout specifies the command timeout
	// +optional
	// +kubebuilder:default="5m"
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Stage specifies when to run the command
	// +optional
	// +kubebuilder:default="post-customization"
	// +kubebuilder:validation:Enum=pre-customization;post-customization;first-boot
	Stage string `json:"stage,omitempty"`
}

// CertificateSpec defines a certificate to install
type CertificateSpec struct {
	// Name is the certificate name
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`

	// Data contains the certificate data (PEM format)
	// +optional
	Data string `json:"data,omitempty"`

	// SecretRef references a secret containing the certificate
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`

	// SecretKey is the key in the secret containing the certificate
	// +optional
	// +kubebuilder:default="tls.crt"
	SecretKey string `json:"secretKey,omitempty"`

	// Store specifies the certificate store
	// +optional
	// +kubebuilder:default="root"
	// +kubebuilder:validation:Enum=root;ca;my;trust
	Store string `json:"store,omitempty"`
}

// CloneMetadata contains clone operation metadata
type CloneMetadata struct {
	// Purpose describes the purpose of the clone
	// +optional
	// +kubebuilder:validation:Enum=backup;testing;migration;development;production;staging
	Purpose string `json:"purpose,omitempty"`

	// CreatedBy identifies who or what created the clone
	// +optional
	// +kubebuilder:validation:MaxLength=255
	CreatedBy string `json:"createdBy,omitempty"`

	// Project identifies the project this clone belongs to
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Project string `json:"project,omitempty"`

	// Environment specifies the environment
	// +optional
	// +kubebuilder:validation:Enum=dev;staging;prod;test
	Environment string `json:"environment,omitempty"`

	// Tags are key-value pairs for categorizing the clone
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Tags map[string]string `json:"tags,omitempty"`
}

// VMCloneStatus defines the observed state of VMClone
type VMCloneStatus struct {
	// TargetRef references the created target VM
	// +optional
	TargetRef *LocalObjectReference `json:"targetRef,omitempty"`

	// Phase represents the current phase of the clone operation
	// +optional
	Phase ClonePhase `json:"phase,omitempty"`

	// Message provides additional details about the current state
	// +optional
	Message string `json:"message,omitempty"`

	// TaskRef tracks any ongoing async operations
	// +optional
	TaskRef string `json:"taskRef,omitempty"`

	// StartTime is when the clone operation started
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the clone operation completed
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// ActualCloneType indicates the actual clone type that was used
	// +optional
	ActualCloneType CloneType `json:"actualCloneType,omitempty"`

	// Progress shows the clone operation progress
	// +optional
	Progress *CloneProgress `json:"progress,omitempty"`

	// Conditions represent the current service state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration reflects the generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// RetryCount is the number of times the clone has been retried
	// +optional
	RetryCount int32 `json:"retryCount,omitempty"`

	// LastRetryTime is when the clone was last retried
	// +optional
	LastRetryTime *metav1.Time `json:"lastRetryTime,omitempty"`

	// CustomizationStatus contains customization operation status
	// +optional
	CustomizationStatus *CustomizationStatus `json:"customizationStatus,omitempty"`
}

// ClonePhase represents the phase of a clone operation
// +kubebuilder:validation:Enum=Pending;Preparing;Cloning;Customizing;Powering-On;Ready;Failed
type ClonePhase string

const (
	// ClonePhasePending indicates the clone is waiting to be processed
	ClonePhasePending ClonePhase = "Pending"
	// ClonePhasePreparing indicates the clone is being prepared
	ClonePhasePreparing ClonePhase = "Preparing"
	// ClonePhaseCloning indicates the clone operation is in progress
	ClonePhaseCloning ClonePhase = "Cloning"
	// ClonePhaseCustomizing indicates the clone is being customized
	ClonePhaseCustomizing ClonePhase = "Customizing"
	// ClonePhasePoweringOn indicates the clone is being powered on
	ClonePhasePoweringOn ClonePhase = "Powering-On"
	// ClonePhaseReady indicates the clone is ready for use
	ClonePhaseReady ClonePhase = "Ready"
	// ClonePhaseFailed indicates the clone operation failed
	ClonePhaseFailed ClonePhase = "Failed"
)

// CloneProgress shows the clone operation progress
type CloneProgress struct {
	// TotalDisks is the total number of disks to clone
	// +optional
	TotalDisks int32 `json:"totalDisks,omitempty"`

	// CompletedDisks is the number of disks completed
	// +optional
	CompletedDisks int32 `json:"completedDisks,omitempty"`

	// CurrentDisk shows progress of the current disk being cloned
	// +optional
	CurrentDisk *DiskCloneProgress `json:"currentDisk,omitempty"`

	// OverallPercentage is the overall completion percentage (0-100)
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	OverallPercentage *int32 `json:"overallPercentage,omitempty"`

	// ETA is the estimated time to completion
	// +optional
	ETA *metav1.Duration `json:"eta,omitempty"`
}

// DiskCloneProgress shows the progress of cloning a single disk
type DiskCloneProgress struct {
	// Name is the disk name
	Name string `json:"name"`

	// TotalBytes is the total size of the disk
	// +optional
	TotalBytes *int64 `json:"totalBytes,omitempty"`

	// CompletedBytes is the number of bytes completed
	// +optional
	CompletedBytes *int64 `json:"completedBytes,omitempty"`

	// Percentage is the completion percentage (0-100)
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Percentage *int32 `json:"percentage,omitempty"`

	// TransferRate is the current transfer rate in bytes per second
	// +optional
	TransferRate *int64 `json:"transferRate,omitempty"`
}

// CustomizationStatus contains customization operation status
type CustomizationStatus struct {
	// Started indicates if customization has started
	// +optional
	Started bool `json:"started,omitempty"`

	// Completed indicates if customization has completed
	// +optional
	Completed bool `json:"completed,omitempty"`

	// CompletedSteps lists completed customization steps
	// +optional
	CompletedSteps []string `json:"completedSteps,omitempty"`

	// FailedSteps lists failed customization steps
	// +optional
	FailedSteps []string `json:"failedSteps,omitempty"`

	// CurrentStep is the current customization step
	// +optional
	CurrentStep string `json:"currentStep,omitempty"`

	// Message provides customization status details
	// +optional
	Message string `json:"message,omitempty"`
}

// VMClone condition types
const (
	// VMCloneConditionReady indicates whether the clone is ready
	VMCloneConditionReady = "Ready"
	// VMCloneConditionCloning indicates whether the clone is in progress
	VMCloneConditionCloning = "Cloning"
	// VMCloneConditionCustomizing indicates whether customization is in progress
	VMCloneConditionCustomizing = "Customizing"
	// VMCloneConditionFailed indicates whether the clone has failed
	VMCloneConditionFailed = "Failed"
)

// VMClone condition reasons
const (
	// VMCloneReasonCompleted indicates the clone was successfully completed
	VMCloneReasonCompleted = "Completed"
	// VMCloneReasonCloning indicates the clone is in progress
	VMCloneReasonCloning = "Cloning"
	// VMCloneReasonCustomizing indicates customization is in progress
	VMCloneReasonCustomizing = "Customizing"
	// VMCloneReasonProviderError indicates a provider error occurred
	VMCloneReasonProviderError = "ProviderError"
	// VMCloneReasonSourceNotFound indicates the source VM was not found
	VMCloneReasonSourceNotFound = "SourceNotFound"
	// VMCloneReasonUnsupported indicates cloning is not supported
	VMCloneReasonUnsupported = "Unsupported"
	// VMCloneReasonInsufficientResources indicates insufficient resources
	VMCloneReasonInsufficientResources = "InsufficientResources"
	// VMCloneReasonCustomizationFailed indicates customization failed
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

// VMClone is the Schema for the vmclones API
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
