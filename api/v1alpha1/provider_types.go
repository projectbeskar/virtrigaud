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

// ProviderSpec defines the desired state of Provider.
type ProviderSpec struct {
	// Type specifies the provider type
	// +kubebuilder:validation:Enum=vsphere;libvirt;firecracker;qemu
	Type string `json:"type"`

	// Endpoint is the provider endpoint URI
	Endpoint string `json:"endpoint"`

	// CredentialSecretRef references the Secret containing credentials
	CredentialSecretRef ObjectRef `json:"credentialSecretRef"`

	// InsecureSkipVerify disables TLS verification
	// +optional
	// +kubebuilder:default=false
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`

	// Defaults provides default placement settings
	// +optional
	Defaults *ProviderDefaults `json:"defaults,omitempty"`

	// RateLimit configures API rate limiting
	// +optional
	RateLimit *RateLimit `json:"rateLimit,omitempty"`
}

// ProviderStatus defines the observed state of Provider.
type ProviderStatus struct {
	// Healthy indicates if the provider is healthy
	// +optional
	Healthy bool `json:"healthy,omitempty"`

	// LastHealthCheck records the last health check time
	// +optional
	LastHealthCheck *metav1.Time `json:"lastHealthCheck,omitempty"`

	// Conditions represent the latest available observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration reflects the generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ProviderDefaults provides default settings for VMs
type ProviderDefaults struct {
	// Datastore specifies the default datastore
	// +optional
	Datastore string `json:"datastore,omitempty"`

	// Cluster specifies the default cluster
	// +optional
	Cluster string `json:"cluster,omitempty"`

	// Folder specifies the default folder
	// +optional
	Folder string `json:"folder,omitempty"`
}

// RateLimit configures API rate limiting
type RateLimit struct {
	// QPS specifies queries per second
	// +optional
	// +kubebuilder:default=10
	QPS int `json:"qps,omitempty"`

	// Burst specifies the burst capacity
	// +optional
	// +kubebuilder:default=20
	Burst int `json:"burst,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".spec.endpoint"
// +kubebuilder:printcolumn:name="Healthy",type="boolean",JSONPath=".status.healthy"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Provider is the Schema for the providers API.
type Provider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProviderSpec   `json:"spec,omitempty"`
	Status ProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProviderList contains a list of Provider.
type ProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Provider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Provider{}, &ProviderList{})
}
