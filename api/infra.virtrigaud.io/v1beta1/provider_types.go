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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProviderType represents the type of virtualization provider
// +kubebuilder:validation:Enum=vsphere;libvirt;firecracker;qemu;proxmox
type ProviderType string

const (
	// ProviderTypeVSphere indicates a VMware vSphere provider
	ProviderTypeVSphere ProviderType = "vsphere"
	// ProviderTypeLibvirt indicates a libvirt provider
	ProviderTypeLibvirt ProviderType = "libvirt"
	// ProviderTypeFirecracker indicates a Firecracker provider
	ProviderTypeFirecracker ProviderType = "firecracker"
	// ProviderTypeQEMU indicates a QEMU provider
	ProviderTypeQEMU ProviderType = "qemu"
	// ProviderTypeProxmox indicates a Proxmox VE provider
	ProviderTypeProxmox ProviderType = "proxmox"
)

// ProviderRuntimeMode specifies how the provider is executed
// +kubebuilder:validation:Enum=InProcess;Remote
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
	// +optional
	// +kubebuilder:default=9443
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// TLS defines TLS configuration for the service
	// +optional
	TLS *ProviderTLSSpec `json:"tls,omitempty"`
}

// ProviderTLSSpec defines TLS configuration for provider communication
type ProviderTLSSpec struct {
	// Enabled determines if TLS is enabled for provider communication
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// SecretRef references a secret containing tls.crt, tls.key, and ca.crt
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

	// InsecureSkipVerify disables TLS certificate verification
	// +optional
	// +kubebuilder:default=false
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// ProviderRuntimeSpec defines the runtime configuration for providers
type ProviderRuntimeSpec struct {
	// Mode specifies the runtime mode
	// +optional
	// +kubebuilder:default="InProcess"
	Mode ProviderRuntimeMode `json:"mode,omitempty"`

	// Image is the container image for remote providers (required if Mode=Remote)
	// +optional
	// +kubebuilder:validation:Pattern="^[a-zA-Z0-9._/-]+:[a-zA-Z0-9._-]+$"
	Image string `json:"image,omitempty"`

	// ImagePullPolicy defines the image pull policy
	// +optional
	// +kubebuilder:default="IfNotPresent"
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// ImagePullSecrets are references to secrets for pulling images
	// +optional
	// +kubebuilder:validation:MaxItems=10
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// Replicas is the number of provider instances (default 1)
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	Replicas *int32 `json:"replicas,omitempty"`

	// Service defines the service configuration
	// +optional
	Service *ProviderServiceSpec `json:"service,omitempty"`

	// Resources defines resource requirements for provider pods
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// NodeSelector is a selector which must be true for the pod to fit on a node
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations allow pods to schedule onto nodes with matching taints
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity defines scheduling constraints
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// SecurityContext defines security context for provider pods
	// +optional
	SecurityContext *corev1.SecurityContext `json:"securityContext,omitempty"`

	// Env defines additional environment variables for provider pods
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Env []corev1.EnvVar `json:"env,omitempty"`

	// LivenessProbe defines the liveness probe for provider pods
	// +optional
	LivenessProbe *corev1.Probe `json:"livenessProbe,omitempty"`

	// ReadinessProbe defines the readiness probe for provider pods
	// +optional
	ReadinessProbe *corev1.Probe `json:"readinessProbe,omitempty"`
}

// ProviderRuntimeStatus defines the runtime status for providers
type ProviderRuntimeStatus struct {
	// Mode indicates the current runtime mode
	// +optional
	Mode ProviderRuntimeMode `json:"mode,omitempty"`

	// Endpoint is the gRPC endpoint (host:port) for remote providers
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// ServiceRef references the Kubernetes service for remote providers
	// +optional
	ServiceRef *corev1.LocalObjectReference `json:"serviceRef,omitempty"`

	// Phase indicates the runtime phase
	// +optional
	Phase ProviderRuntimePhase `json:"phase,omitempty"`

	// Message provides additional details about the runtime status
	// +optional
	Message string `json:"message,omitempty"`

	// ReadyReplicas is the number of ready provider replicas
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// AvailableReplicas is the number of available provider replicas
	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`
}

// ProviderRuntimePhase represents the phase of provider runtime
// +kubebuilder:validation:Enum=Pending;Starting;Running;Stopping;Failed
type ProviderRuntimePhase string

const (
	// ProviderRuntimePhasePending indicates the runtime is being prepared
	ProviderRuntimePhasePending ProviderRuntimePhase = "Pending"
	// ProviderRuntimePhaseStarting indicates the runtime is starting
	ProviderRuntimePhaseStarting ProviderRuntimePhase = "Starting"
	// ProviderRuntimePhaseRunning indicates the runtime is operational
	ProviderRuntimePhaseRunning ProviderRuntimePhase = "Running"
	// ProviderRuntimePhaseStopping indicates the runtime is stopping
	ProviderRuntimePhaseStopping ProviderRuntimePhase = "Stopping"
	// ProviderRuntimePhaseFailed indicates the runtime has failed
	ProviderRuntimePhaseFailed ProviderRuntimePhase = "Failed"
)

// ProviderSpec defines the desired state of Provider
type ProviderSpec struct {
	// Type specifies the provider type
	Type ProviderType `json:"type"`

	// Endpoint is the provider endpoint URI
	// +kubebuilder:validation:Pattern="^(https?|tcp|grpc)://[a-zA-Z0-9.-]+:[0-9]+(/.*)?$"
	Endpoint string `json:"endpoint"`

	// CredentialSecretRef references the Secret containing credentials
	CredentialSecretRef ObjectRef `json:"credentialSecretRef"`

	// InsecureSkipVerify disables TLS verification (deprecated, use runtime.service.tls.insecureSkipVerify)
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

	// HealthCheck defines health checking configuration
	// +optional
	HealthCheck *ProviderHealthCheck `json:"healthCheck,omitempty"`

	// ConnectionPooling defines connection pooling settings
	// +optional
	ConnectionPooling *ConnectionPooling `json:"connectionPooling,omitempty"`
}

// ProviderHealthCheck defines health checking configuration
type ProviderHealthCheck struct {
	// Enabled indicates whether health checking is enabled
	// +optional
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// Interval defines how often to check provider health
	// +optional
	// +kubebuilder:default="30s"
	Interval *metav1.Duration `json:"interval,omitempty"`

	// Timeout defines the timeout for health checks
	// +optional
	// +kubebuilder:default="10s"
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// FailureThreshold is the number of consecutive failures before marking unhealthy
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// SuccessThreshold is the number of consecutive successes before marking healthy
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	SuccessThreshold int32 `json:"successThreshold,omitempty"`
}

// ConnectionPooling defines connection pooling settings
type ConnectionPooling struct {
	// MaxConnections is the maximum number of connections to maintain
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxConnections int32 `json:"maxConnections,omitempty"`

	// MaxIdleConnections is the maximum number of idle connections
	// +optional
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	MaxIdleConnections int32 `json:"maxIdleConnections,omitempty"`

	// ConnectionTimeout is the timeout for establishing connections
	// +optional
	// +kubebuilder:default="30s"
	ConnectionTimeout *metav1.Duration `json:"connectionTimeout,omitempty"`

	// IdleTimeout is the timeout for idle connections
	// +optional
	// +kubebuilder:default="5m"
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`
}

// ProviderStatus defines the observed state of Provider
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

	// Capabilities lists the provider's supported capabilities
	// +optional
	Capabilities []ProviderCapability `json:"capabilities,omitempty"`

	// Version reports the provider version
	// +optional
	Version string `json:"version,omitempty"`

	// ConnectedVMs is the number of VMs currently managed by this provider
	// +optional
	ConnectedVMs int32 `json:"connectedVMs,omitempty"`

	// ResourceUsage provides resource usage statistics
	// +optional
	ResourceUsage *ProviderResourceUsage `json:"resourceUsage,omitempty"`
}

// ProviderCapability represents a provider capability
// +kubebuilder:validation:Enum=VirtualMachines;Snapshots;Cloning;LiveMigration;ConsoleAccess;DiskManagement;NetworkManagement;GPUPassthrough;HighAvailability;Backup;Templates
type ProviderCapability string

const (
	// ProviderCapabilityVirtualMachines indicates basic VM management
	ProviderCapabilityVirtualMachines ProviderCapability = "VirtualMachines"
	// ProviderCapabilitySnapshots indicates snapshot support
	ProviderCapabilitySnapshots ProviderCapability = "Snapshots"
	// ProviderCapabilityCloning indicates VM cloning support
	ProviderCapabilityCloning ProviderCapability = "Cloning"
	// ProviderCapabilityLiveMigration indicates live migration support
	ProviderCapabilityLiveMigration ProviderCapability = "LiveMigration"
	// ProviderCapabilityConsoleAccess indicates console access support
	ProviderCapabilityConsoleAccess ProviderCapability = "ConsoleAccess"
	// ProviderCapabilityDiskManagement indicates disk management support
	ProviderCapabilityDiskManagement ProviderCapability = "DiskManagement"
	// ProviderCapabilityNetworkManagement indicates network management support
	ProviderCapabilityNetworkManagement ProviderCapability = "NetworkManagement"
	// ProviderCapabilityGPUPassthrough indicates GPU passthrough support
	ProviderCapabilityGPUPassthrough ProviderCapability = "GPUPassthrough"
	// ProviderCapabilityHighAvailability indicates HA support
	ProviderCapabilityHighAvailability ProviderCapability = "HighAvailability"
	// ProviderCapabilityBackup indicates backup support
	ProviderCapabilityBackup ProviderCapability = "Backup"
	// ProviderCapabilityTemplates indicates template management support
	ProviderCapabilityTemplates ProviderCapability = "Templates"
)

// ProviderResourceUsage provides resource usage statistics
type ProviderResourceUsage struct {
	// CPU usage statistics
	// +optional
	CPU *ResourceUsageStats `json:"cpu,omitempty"`

	// Memory usage statistics
	// +optional
	Memory *ResourceUsageStats `json:"memory,omitempty"`

	// Storage usage statistics
	// +optional
	Storage *ResourceUsageStats `json:"storage,omitempty"`

	// Network usage statistics
	// +optional
	Network *NetworkUsageStats `json:"network,omitempty"`
}

// ResourceUsageStats represents usage statistics for a resource
type ResourceUsageStats struct {
	// Total available capacity
	// +optional
	Total *int64 `json:"total,omitempty"`

	// Used capacity
	// +optional
	Used *int64 `json:"used,omitempty"`

	// Available capacity
	// +optional
	Available *int64 `json:"available,omitempty"`

	// Usage percentage (0-100)
	// +optional
	UsagePercent *int32 `json:"usagePercent,omitempty"`
}

// NetworkUsageStats represents network usage statistics
type NetworkUsageStats struct {
	// BytesReceived is the total bytes received
	// +optional
	BytesReceived *int64 `json:"bytesReceived,omitempty"`

	// BytesSent is the total bytes sent
	// +optional
	BytesSent *int64 `json:"bytesSent,omitempty"`

	// PacketsReceived is the total packets received
	// +optional
	PacketsReceived *int64 `json:"packetsReceived,omitempty"`

	// PacketsSent is the total packets sent
	// +optional
	PacketsSent *int64 `json:"packetsSent,omitempty"`
}

// ProviderDefaults provides default settings for VMs
type ProviderDefaults struct {
	// Datastore specifies the default datastore
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Datastore string `json:"datastore,omitempty"`

	// Cluster specifies the default cluster
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Cluster string `json:"cluster,omitempty"`

	// Folder specifies the default folder
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Folder string `json:"folder,omitempty"`

	// ResourcePool specifies the default resource pool
	// +optional
	// +kubebuilder:validation:MaxLength=255
	ResourcePool string `json:"resourcePool,omitempty"`

	// Network specifies the default network
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Network string `json:"network,omitempty"`
}

// RateLimit configures API rate limiting
type RateLimit struct {
	// QPS specifies queries per second
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	QPS int `json:"qps,omitempty"`

	// Burst specifies the burst capacity
	// +optional
	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2000
	Burst int `json:"burst,omitempty"`
}

// Provider condition types
const (
	// ProviderConditionReady indicates whether the provider is ready
	ProviderConditionReady = "Ready"
	// ProviderConditionHealthy indicates whether the provider is healthy
	ProviderConditionHealthy = "Healthy"
	// ProviderConditionConnected indicates whether the provider is connected
	ProviderConditionConnected = "Connected"
	// ProviderConditionRuntimeReady indicates whether the runtime is ready
	ProviderConditionRuntimeReady = "RuntimeReady"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
//+kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.spec.endpoint`
//+kubebuilder:printcolumn:name="Healthy",type=boolean,JSONPath=`.status.healthy`
//+kubebuilder:printcolumn:name="Connected VMs",type=integer,JSONPath=`.status.connectedVMs`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
//+kubebuilder:resource:shortName=prov

// Provider is the Schema for the providers API
// +kubebuilder:storageversion
type Provider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProviderSpec   `json:"spec,omitempty"`
	Status ProviderStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ProviderList contains a list of Provider
type ProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Provider `json:"items"`
}

// Hub marks this version as the conversion hub
func (*Provider) Hub() {}

func init() {
	SchemeBuilder.Register(&Provider{}, &ProviderList{})
}
