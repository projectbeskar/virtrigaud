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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProviderRuntimeMode specifies how the provider is executed
type ProviderRuntimeMode string

const (
	// RuntimeModeInProcess runs the provider in the manager process
	RuntimeModeInProcess ProviderRuntimeMode = "InProcess"
	// RuntimeModeRemote runs the provider as a separate deployment
	RuntimeModeRemote ProviderRuntimeMode = "Remote"
)

// ProviderServiceSpec defines the service configuration for remote providers
type ProviderServiceSpec struct {
	// Port is the gRPC service port
	// +kubebuilder:default=9443
	Port int32 `json:"port,omitempty"`
}

// ProviderTLSSpec defines TLS configuration for provider communication
type ProviderTLSSpec struct {
	// Enabled determines if TLS is enabled for provider communication
	Enabled bool `json:"enabled,omitempty"`

	// SecretRef references a secret containing tls.crt, tls.key, and ca.crt
	SecretRef corev1.LocalObjectReference `json:"secretRef,omitempty"`
}

// ProviderRuntimeSpec defines the runtime configuration for providers
type ProviderRuntimeSpec struct {
	// Mode specifies the runtime mode
	// +kubebuilder:default="InProcess"
	// +kubebuilder:validation:Enum=InProcess;Remote
	Mode ProviderRuntimeMode `json:"mode,omitempty"`

	// Image is the container image for remote providers (required if Mode=Remote)
	Image string `json:"image,omitempty"`

	// Version is the image version/tag
	Version string `json:"version,omitempty"`

	// Replicas is the number of provider instances (default 1)
	// +kubebuilder:default=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Service defines the service configuration
	Service *ProviderServiceSpec `json:"service,omitempty"`

	// Resources defines resource requirements for provider pods
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// NodeSelector is a selector which must be true for the pod to fit on a node
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations allow pods to schedule onto nodes with matching taints
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity defines scheduling constraints
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// SecurityContext defines security context for provider pods
	SecurityContext *corev1.SecurityContext `json:"securityContext,omitempty"`

	// Env defines additional environment variables for provider pods
	Env []corev1.EnvVar `json:"env,omitempty"`

	// TLS defines TLS configuration for provider communication
	TLS *ProviderTLSSpec `json:"tls,omitempty"`
}

// ProviderRuntimeStatus defines the runtime status for providers
type ProviderRuntimeStatus struct {
	// Mode indicates the current runtime mode
	Mode ProviderRuntimeMode `json:"mode,omitempty"`

	// Endpoint is the gRPC endpoint (host:port) for remote providers
	Endpoint string `json:"endpoint,omitempty"`

	// ServiceRef references the Kubernetes service for remote providers
	ServiceRef *corev1.LocalObjectReference `json:"serviceRef,omitempty"`

	// Phase indicates the runtime phase
	Phase string `json:"phase,omitempty"`

	// Message provides additional details about the runtime status
	Message string `json:"message,omitempty"`
}

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

	// Runtime defines how the provider is executed
	// +optional
	Runtime *ProviderRuntimeSpec `json:"runtime,omitempty"`
}

// ProviderStatus defines the observed state of Provider.
type ProviderStatus struct {
	// Healthy indicates if the provider is healthy
	// +optional
	Healthy bool `json:"healthy,omitempty"`

	// LastHealthCheck records the last health check time
	// +optional
	LastHealthCheck *metav1.Time `json:"lastHealthCheck,omitempty"`

	// Runtime provides runtime status information
	// +optional
	Runtime *ProviderRuntimeStatus `json:"runtime,omitempty"`

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
