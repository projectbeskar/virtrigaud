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
	// ImportIfMissing imports the image if it doesn't exist
	// +optional
	// +kubebuilder:default=true
	ImportIfMissing bool `json:"importIfMissing,omitempty"`

	// ValidateChecksum validates the image checksum
	// +optional
	// +kubebuilder:default=true
	ValidateChecksum bool `json:"validateChecksum,omitempty"`
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
