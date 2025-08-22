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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VMImageSpec defines the desired state of VMImage.
type VMImageSpec struct {
	// VSphere contains vSphere-specific image configuration
	// +optional
	VSphere *VSphereImageSpec `json:"vsphere,omitempty"`

	// Libvirt contains Libvirt-specific image configuration
	// +optional
	Libvirt *LibvirtImageSpec `json:"libvirt,omitempty"`

	// Prepare contains optional image preparation steps
	// +optional
	Prepare *ImagePrepare `json:"prepare,omitempty"`
}

// VMImageStatus defines the observed state of VMImage.
type VMImageStatus struct {
	// Ready indicates if the image is ready for use
	// +optional
	Ready bool `json:"ready,omitempty"`

	// AvailableOn lists the providers where the image is available
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

	// Phase represents the current phase of image preparation
	// +optional
	Phase ImagePhase `json:"phase,omitempty"`

	// Message provides additional details about the current state
	// +optional
	Message string `json:"message,omitempty"`

	// PrepareTaskRef tracks any ongoing image preparation operations
	// +optional
	PrepareTaskRef string `json:"prepareTaskRef,omitempty"`

	// ImportProgress shows the progress of image import operations
	// +optional
	ImportProgress *ImageImportProgress `json:"importProgress,omitempty"`
}

// ImagePhase represents the phase of image preparation
// +kubebuilder:validation:Enum=Pending;Importing;Preparing;Ready;Failed
type ImagePhase string

const (
	// ImagePhasePending indicates the image is waiting to be processed
	ImagePhasePending ImagePhase = "Pending"
	// ImagePhaseImporting indicates the image is being imported
	ImagePhaseImporting ImagePhase = "Importing"
	// ImagePhasePreparing indicates the image is being prepared
	ImagePhasePreparing ImagePhase = "Preparing"
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
	Percentage *int32 `json:"percentage,omitempty"`

	// TransferRate is the current transfer rate in bytes per second
	// +optional
	TransferRate *int64 `json:"transferRate,omitempty"`

	// ETA is the estimated time to completion
	// +optional
	ETA *metav1.Duration `json:"eta,omitempty"`
}

// VSphereImageSpec defines vSphere-specific image configuration
type VSphereImageSpec struct {
	// TemplateName references an existing vSphere template
	// +optional
	TemplateName string `json:"templateName,omitempty"`

	// OVAURL provides a URL to an OVA file to import
	// +optional
	OVAURL string `json:"ovaURL,omitempty"`

	// Checksum provides expected checksum for verification
	// +optional
	Checksum string `json:"checksum,omitempty"`

	// ChecksumType specifies the checksum algorithm
	// +optional
	// +kubebuilder:default="sha256"
	// +kubebuilder:validation:Enum=md5;sha1;sha256;sha512
	ChecksumType string `json:"checksumType,omitempty"`
}

// LibvirtImageSpec defines Libvirt-specific image configuration
type LibvirtImageSpec struct {
	// Path specifies the path to the image file
	// +optional
	Path string `json:"path,omitempty"`

	// URL provides a URL to download the image
	// +optional
	URL string `json:"url,omitempty"`

	// Format specifies the image format
	// +optional
	// +kubebuilder:default="qcow2"
	// +kubebuilder:validation:Enum=qcow2;raw;vmdk
	Format string `json:"format,omitempty"`

	// Checksum provides expected checksum for verification
	// +optional
	Checksum string `json:"checksum,omitempty"`

	// ChecksumType specifies the checksum algorithm
	// +optional
	// +kubebuilder:default="sha256"
	// +kubebuilder:validation:Enum=md5;sha1;sha256;sha512
	ChecksumType string `json:"checksumType,omitempty"`
}

// ImagePrepare defines optional image preparation steps
type ImagePrepare struct {
	// OnMissing defines the action to take when image is missing
	// +optional
	// +kubebuilder:default="Import"
	// +kubebuilder:validation:Enum=Import;Fail
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
	Retries *int32 `json:"retries,omitempty"`

	// Force forces re-import even if image exists
	// +optional
	Force bool `json:"force,omitempty"`

	// Storage defines storage-specific preparation options
	// +optional
	Storage *StoragePrepareOptions `json:"storage,omitempty"`
}

// ImageMissingAction defines actions to take when an image is missing
// +kubebuilder:validation:Enum=Import;Fail
type ImageMissingAction string

const (
	// ImageMissingActionImport imports the missing image
	ImageMissingActionImport ImageMissingAction = "Import"
	// ImageMissingActionFail fails when the image is missing
	ImageMissingActionFail ImageMissingAction = "Fail"
)

// StoragePrepareOptions defines storage-specific preparation options
type StoragePrepareOptions struct {
	// VSphere storage options
	// +optional
	VSphere *VSphereStorageOptions `json:"vsphere,omitempty"`

	// Libvirt storage options
	// +optional
	Libvirt *LibvirtStorageOptions `json:"libvirt,omitempty"`
}

// VSphereStorageOptions defines vSphere storage preparation options
type VSphereStorageOptions struct {
	// Datastore specifies the target datastore for import
	// +optional
	Datastore string `json:"datastore,omitempty"`

	// Folder specifies the target folder for import
	// +optional
	Folder string `json:"folder,omitempty"`

	// ThinProvisioned indicates whether to use thin provisioning
	// +optional
	ThinProvisioned *bool `json:"thinProvisioned,omitempty"`
}

// LibvirtStorageOptions defines Libvirt storage preparation options
type LibvirtStorageOptions struct {
	// StoragePool specifies the target storage pool for import
	// +optional
	StoragePool string `json:"storagePool,omitempty"`

	// AllocationPolicy defines how storage is allocated
	// +optional
	// +kubebuilder:validation:Enum=eager;lazy
	AllocationPolicy string `json:"allocationPolicy,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Providers",type="string",JSONPath=".status.availableOn"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// VMImage is the Schema for the vmimages API.
type VMImage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VMImageSpec   `json:"spec,omitempty"`
	Status VMImageStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VMImageList contains a list of VMImage.
type VMImageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VMImage `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VMImage{}, &VMImageList{})
}
