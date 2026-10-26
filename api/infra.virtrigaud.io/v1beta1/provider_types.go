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
	// ProviderTypeFirecracker names a Firecracker provider. The schema accepts
	// it, but VirtRigaud ships no Firecracker provider implementation.
	ProviderTypeFirecracker ProviderType = "firecracker"
	// ProviderTypeQEMU names a QEMU provider. The schema accepts it, but
	// VirtRigaud ships no QEMU provider implementation.
	ProviderTypeQEMU ProviderType = "qemu"
	// ProviderTypeProxmox indicates a Proxmox VE provider
	ProviderTypeProxmox ProviderType = "proxmox"
)

// Provider deployment topologies (ProviderSpec.Topology). A string enum rather
// than a bool, so that further modes can be added as enum values.
const (
	// ProviderTopologySingle is one gRPC process bound to one host — today's
	// behavior, byte-for-byte unchanged. It is the default.
	ProviderTopologySingle = "single"
	// ProviderTopologyCluster fronts N bare hosts registered as Host CRs under a
	// HostPool; the operator owns inventory and placement.
	ProviderTopologyCluster = "cluster"
)

// ProviderAllowConsumerOrphanOnDeleteAnnotation is the Provider annotation
// that, set to "true" on a clustered Provider by its administrator, lets
// VirtualMachines in OTHER namespaces
// (consumers granted through spec.consumerNamespaceSelector) be detached with
// virtrigaud.io/orphan-on-delete, or released with virtrigaud.io/force-delete
// without a confirmed provider delete. Without it, only VMs in the Provider's
// own namespace may be orphaned or force-deleted on a clustered Provider: such
// a VM keeps running on its host but leaves the scheduler's committed-capacity
// accounting, so a consumer could otherwise occupy capacity nobody sees. Single-host Providers ignore it.
const ProviderAllowConsumerOrphanOnDeleteAnnotation = "infra.virtrigaud.io/allow-consumer-orphan-on-delete"

// ProviderInUseFinalizer is the finalizer the Provider controller keeps on
// every Provider. A Provider being deleted is held while any
// VirtualMachine (in any namespace), VMSnapshot, VMClone or VMMigration still
// needs it, with its runtime kept up so they can still be deleted through it.
// It is removed once nothing needs the Provider, or at once when the Provider
// is annotated virtrigaud.io/force-delete=true.
const ProviderInUseFinalizer = "provider.infra.virtrigaud.io/in-use"

// ProviderTmpSizeLimitAnnotation is the Provider annotation that sets the
// sizeLimit of the writable /tmp emptyDir of the provider pods the manager
// runs for it, overriding the manager-wide default (--provider-tmp-size-limit).
// The value is a positive resource quantity (e.g. "20Gi") or
// ProviderTmpSizeLimitNone for no limit. A pod that writes more than the limit
// to /tmp is evicted by the kubelet. Changing it rolls the provider pods.
const ProviderTmpSizeLimitAnnotation = "infra.virtrigaud.io/tmp-size-limit"

// ProviderTmpSizeLimitNone is the ProviderTmpSizeLimitAnnotation (and
// --provider-tmp-size-limit) value that leaves /tmp without a sizeLimit.
const ProviderTmpSizeLimitNone = "none"

// ProviderRuntimeMode specifies how the provider is executed
// +kubebuilder:validation:Enum=Remote
type ProviderRuntimeMode string

const (
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

	// TLS defines TLS configuration for the service. Leaving it unset is not an
	// implicit plaintext choice: a Provider without a tls block is not deployed
	// (condition TLSConfigured=False, reason TLSBlockMissing). Set tls.enabled to
	// false to opt out of TLS explicitly.
	// +optional
	TLS *ProviderTLSSpec `json:"tls,omitempty"`
}

// ProviderTLSSpec defines TLS configuration for provider communication
type ProviderTLSSpec struct {
	// Deliberately NOT `omitempty`: this field has a default of true, so
	// omitempty drops an explicit `false` when the object is re-serialized (e.g.
	// by a controller Update), letting the apiserver re-apply the default and
	// silently flip tls.enabled false to true. Always serializing it keeps an
	// explicit plaintext opt-out durable.

	// Enabled determines if TLS is enabled for the connection between the
	// manager and the provider pod. Defaults to true. With true, secretRef is
	// required: a Provider with tls.enabled=true and no secretRef is not
	// deployed (condition TLSConfigured=False, reason SecretRefMissing). Setting
	// it to false is an explicit opt-out that runs the provider gRPC server
	// without TLS, recorded as TLSConfigured=False, reason ExplicitlyDisabled.
	// +optional
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`

	// SecretRef references a Secret in the Provider's namespace containing
	// tls.crt, tls.key and ca.crt. The manager presents tls.crt/tls.key as its
	// client certificate and verifies the provider's server certificate against
	// ca.crt; the provider pod mounts the same Secret.
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

	// InsecureSkipVerify makes the manager skip verification of the provider's
	// gRPC server certificate when it connects to the provider pod (the manager
	// still presents its client certificate). It is read only by the manager,
	// when it builds its TLS client config for this Provider from SecretRef, and
	// only while tls.enabled is true.
	// It does not change anything on the provider pod and has no effect on the
	// connection from the provider to the hypervisor; that is the separate
	// spec.insecureSkipVerify. Defaults to false.
	//
	// This is an insecure escape hatch for first bootstrap and labs: with it the
	// manager cannot tell the real provider from an impostor, which defeats the
	// server-authentication half of mutual TLS. The manager logs a warning each
	// time it opens a new connection to the provider while this is true.
	// +optional
	// +kubebuilder:default=false
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// ProviderRuntimeSpec defines the runtime configuration for providers
type ProviderRuntimeSpec struct {
	// Mode specifies the runtime mode (always Remote)
	// +optional
	// +kubebuilder:default="Remote"
	Mode ProviderRuntimeMode `json:"mode,omitempty"`

	// Image is the container image for remote providers (required)
	// Supports formats: image:tag, image@digest, or image:tag@digest
	// +kubebuilder:validation:Pattern="^[a-zA-Z0-9._/-]+(:[a-zA-Z0-9._-]+)?(@[a-zA-Z0-9]+:[a-fA-F0-9]{64})?$"
	Image string `json:"image"`

	// ImagePullPolicy defines the image pull policy
	// +optional
	// +kubebuilder:default="IfNotPresent"
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// ImagePullSecrets are references to secrets for pulling images. Accepted but
	// not acted on in this release: the manager does not add them to the provider
	// pod.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// Replicas is the number of provider instances (default 1). A clustered
	// Provider (topology: cluster) and a vSphere Provider always run one
	// replica, replaced with the Recreate strategy; for them a larger value is
	// ignored.
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

	// LogLevel sets the log level for provider pods
	// Defaults to the controller's log level if not specified
	// +optional
	// +kubebuilder:validation:Enum=debug;info;warn;error
	// +kubebuilder:default="info"
	LogLevel string `json:"logLevel,omitempty"`

	// LogFormat sets the log format for provider pods
	// +optional
	// +kubebuilder:validation:Enum=text;json
	// +kubebuilder:default="text"
	LogFormat string `json:"logFormat,omitempty"`

	// Env defines additional environment variables for provider pods
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Env []corev1.EnvVar `json:"env,omitempty"`

	// LivenessProbe defines the liveness probe for provider pods. Accepted but not
	// acted on in this release: the manager always uses its built-in probe (HTTP
	// GET /healthz on port 8080).
	// +optional
	LivenessProbe *corev1.Probe `json:"livenessProbe,omitempty"`

	// ReadinessProbe defines the readiness probe for provider pods. Accepted but
	// not acted on in this release: the manager always uses its built-in probe
	// (HTTP GET /healthz on port 8080).
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

	// ReadyReplicas is the number of ready provider replicas. Not populated in
	// this release; see the provider Deployment's status instead.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// AvailableReplicas is the number of available provider replicas. Not
	// populated in this release; see the provider Deployment's status instead.
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
	// Type specifies the provider type.
	//
	// Type is IMMUTABLE once set: the controllers decide type-specific safety
	// rules from it (for example, a libvirt linked clone into another namespace
	// is refused), and the VMs a Provider manages were created by, and are
	// addressed with, the provider implementation of this type. To change it,
	// create a new Provider.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.type is immutable; create a new Provider to change it"
	Type ProviderType `json:"type"`

	// Topology selects the provider's deployment topology:
	//   - "single" (default): one gRPC process bound to one host. There is no
	//     scheduler, no host inventory and no projected host Secret.
	//   - "cluster": the provider fronts N bare hosts registered as Host CRs under
	//     a HostPool. The operator owns inventory and placement, and renders the
	//     pool's Host CRs into a Secret mounted into the provider pod; the
	//     provider reads that mounted file, never the Kubernetes API.
	// Only meaningful for hypervisors with no native cluster manager: the
	// validating webhook accepts "cluster" for type libvirt only and rejects it
	// for the other types. An empty value is defaulted to "single".
	//
	// Topology is IMMUTABLE once set: VMs record where they
	// run (status.placement) under one topology, and every per-VM call is routed
	// and owner-checked by it, so flipping it would send a placed VM's calls down
	// the wrong path. To change it, create a new Provider. The apiserver compares
	// the defaulted values, so an object created before this field existed (read
	// back as "single") keeps accepting ordinary updates.
	// +optional
	// +kubebuilder:default=single
	// +kubebuilder:validation:Enum=single;cluster
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.topology is immutable; create a new Provider to change it"
	Topology string `json:"topology,omitempty"`

	// Endpoint is the URI of the hypervisor the provider manages: for vSphere the
	// vCenter SDK URL (https://vcenter.example.com/sdk), for Proxmox the API URL
	// (https://pve.example.com:8006), for libvirt a connection URI (qemu://,
	// qemu+ssh://, qemu+tcp:// or qemu+tls://). The schema also accepts
	// http(s)://, tcp:// and grpc:// URIs. The manager passes it to the provider
	// pod as PROVIDER_ENDPOINT. For a clustered libvirt Provider (topology:
	// cluster) the hosts are given by Host.spec.endpoint instead.
	// +kubebuilder:validation:Pattern="^((https?://[a-zA-Z0-9.-]+(:[0-9]+)?(/.*)?|(tcp|grpc)://[a-zA-Z0-9.-]+:[0-9]+(/.*)?)|qemu(\\+ssh|\\+tcp|\\+tls)?://([a-zA-Z0-9@.-]+(:[0-9]+)?)?(/.*))$"
	Endpoint string `json:"endpoint"`

	// CredentialSecretRef references the Secret containing the credentials the
	// provider uses to reach the hypervisor. The manager mounts it into the
	// provider pod; the provider reads it from the mounted files.
	CredentialSecretRef ObjectRef `json:"credentialSecretRef"`

	// InsecureSkipVerify makes the provider pod skip verification of the TLS
	// certificate of the hypervisor management endpoint (vCenter for vSphere, the
	// Proxmox VE API for Proxmox). The manager passes it to the provider pod as
	// the TLS_INSECURE_SKIP_VERIFY environment variable, and only the vSphere and
	// Proxmox providers read it; the libvirt provider ignores it (for libvirt SSH
	// hosts see the LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION environment
	// variable). Defaults to false.
	//
	// It is not the same setting as spec.runtime.service.tls.insecureSkipVerify,
	// which controls the manager's verification of the provider's own gRPC server
	// certificate; setting one does not affect the other. It is an insecure
	// escape hatch: with it set, a network attacker can impersonate the
	// hypervisor endpoint and capture the credentials the provider sends to it.
	// +optional
	// +kubebuilder:default=false
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`

	// Defaults provides default placement settings. Only the vSphere provider
	// uses them, and only datastore, storagePod, cluster and folder are passed to
	// it (as PROVIDER_DEFAULT_* environment variables); resourcePool and network
	// are accepted but not acted on in this release. libvirt and Proxmox ignore
	// this block.
	// +optional
	Defaults *ProviderDefaults `json:"defaults,omitempty"`

	// RateLimit configures API rate limiting. Accepted but not acted on in this
	// release.
	// +optional
	RateLimit *RateLimit `json:"rateLimit,omitempty"`

	// Runtime defines how the provider is executed (required)
	Runtime *ProviderRuntimeSpec `json:"runtime"`

	// HealthCheck defines health checking configuration. Accepted but not acted
	// on in this release: status.healthy follows the readiness of the provider
	// Deployment, whatever these settings say.
	// +optional
	HealthCheck *ProviderHealthCheck `json:"healthCheck,omitempty"`

	// ConnectionPooling defines connection pooling settings. Accepted but not
	// acted on in this release.
	// +optional
	ConnectionPooling *ConnectionPooling `json:"connectionPooling,omitempty"`

	// ConsumerNamespaceSelector selects the namespaces, other than this
	// Provider's own, that may use it: VirtualMachines there may name it in
	// spec.providerRef, and VMClones, VMMigrations and VMSnapshots there may act
	// on VMs through it. A reference from the Provider's own namespace is
	// always allowed.
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

// ProviderHealthCheck defines health checking configuration. Accepted but not
// acted on in this release.
type ProviderHealthCheck struct {
	// Deliberately NOT `omitempty`: a defaulted bool with omitempty has its
	// explicit `false` dropped and re-defaulted to true on the next write (see
	// ProviderTLSSpec.Enabled).

	// Enabled indicates whether health checking is enabled. Defaults to true.
	// Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`

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

// ConnectionPooling defines connection pooling settings. Accepted but not acted
// on in this release.
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
	// Healthy is true while the provider Deployment reports a ready replica
	// (the ProviderAvailable condition is True); false when it has none.
	// +optional
	Healthy bool `json:"healthy,omitempty"`

	// LastHealthCheck is the time of the last reconcile that found the provider
	// healthy.
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

	// Capabilities lists the provider's supported capabilities. Not populated in
	// this release; see reportedCapabilities.
	// +optional
	Capabilities []ProviderCapability `json:"capabilities,omitempty"`

	// ReportedCapabilities is the provider's self-reported capability set,
	// fetched from the provider GetCapabilities RPC. It reflects what the running
	// provider advertises and is consumed by the manager to surface features and
	// (when capability enforcement is enabled) to gate capability-dependent
	// operations. It is read once a rollout of the provider runtime is complete,
	// again after the old pods' termination grace period, and every 10 minutes
	// while the provider is healthy.
	// +optional
	ReportedCapabilities *ReportedCapabilities `json:"reportedCapabilities,omitempty"`

	// Version reports the provider version. Not populated in this release.
	// +optional
	Version string `json:"version,omitempty"`

	// ConnectedVMs is the number of VirtualMachines, in any namespace, whose
	// spec.providerRef names this Provider.
	// +optional
	ConnectedVMs int32 `json:"connectedVMs,omitempty"`

	// ResourceUsage provides resource usage statistics. Not populated in this
	// release.
	// +optional
	ResourceUsage *ProviderResourceUsage `json:"resourceUsage,omitempty"`

	// Adoption tracks VM adoption status
	// +optional
	Adoption *ProviderAdoptionStatus `json:"adoption,omitempty"`
}

// ReportedCapabilities mirrors the provider.v1 GetCapabilitiesResponse — the
// capability set a provider advertises at runtime via the GetCapabilities RPC.
// All fields are optional and default to the zero value when a
// provider does not advertise them.
type ReportedCapabilities struct {
	// SupportsReconfigureOnline reports online CPU/memory reconfigure support.
	// +optional
	SupportsReconfigureOnline bool `json:"supportsReconfigureOnline,omitempty"`
	// SupportsDiskExpansionOnline reports online disk expansion support.
	// +optional
	SupportsDiskExpansionOnline bool `json:"supportsDiskExpansionOnline,omitempty"`
	// SupportsSnapshots reports VM snapshot support.
	// +optional
	SupportsSnapshots bool `json:"supportsSnapshots,omitempty"`
	// SupportsMemorySnapshots reports memory-inclusive snapshot support.
	// +optional
	SupportsMemorySnapshots bool `json:"supportsMemorySnapshots,omitempty"`
	// SupportsLinkedClones reports linked (copy-on-write) clone support.
	// +optional
	SupportsLinkedClones bool `json:"supportsLinkedClones,omitempty"`
	// SupportsImageImport reports image import/preparation support.
	// +optional
	SupportsImageImport bool `json:"supportsImageImport,omitempty"`
	// SupportedDiskTypes lists supported disk formats.
	// +optional
	SupportedDiskTypes []string `json:"supportedDiskTypes,omitempty"`
	// SupportedNetworkTypes lists supported NIC models.
	// +optional
	SupportedNetworkTypes []string `json:"supportedNetworkTypes,omitempty"`
	// SupportsDiskExport reports disk export (migration source) support.
	// +optional
	SupportsDiskExport bool `json:"supportsDiskExport,omitempty"`
	// SupportsDiskImport reports disk import (migration target) support.
	// +optional
	SupportsDiskImport bool `json:"supportsDiskImport,omitempty"`
	// SupportedExportFormats lists supported export formats.
	// +optional
	SupportedExportFormats []string `json:"supportedExportFormats,omitempty"`
	// SupportedImportFormats lists supported import formats.
	// +optional
	SupportedImportFormats []string `json:"supportedImportFormats,omitempty"`
	// SupportsExportCompression reports export compression support.
	// +optional
	SupportsExportCompression bool `json:"supportsExportCompression,omitempty"`
	// SupportedExportBackends lists the storage backends the provider can export
	// disks to (e.g. "pvc", "nfs", "s3"). Empty/absent means pvc-only, the
	// behavior of a provider that predates the other backends.
	// +optional
	SupportedExportBackends []string `json:"supportedExportBackends,omitempty"`
	// SupportedImportBackends lists the storage backends the provider can import
	// disks from (e.g. "pvc", "nfs", "s3"). Empty/absent means pvc-only.
	// +optional
	SupportedImportBackends []string `json:"supportedImportBackends,omitempty"`
	// SupportedTransferModes lists the disk-transfer modes the provider supports
	// (e.g. "relay", "direct"). Empty/absent means relay-only, the pod-side path
	// through the provider pod.
	// +optional
	SupportedTransferModes []string `json:"supportedTransferModes,omitempty"`
	// SupportsImageArtifactIdentity reports that the provider's image prepare
	// names, stamps and verifies each prepared-image artifact by the VMImage's
	// UID and source digest, and never reuses an artifact by bare name.
	// Import-style images are not prepared through a provider that
	// supports image import but does not report this: VM creates wait until
	// the provider is upgraded.
	// +optional
	SupportsImageArtifactIdentity bool `json:"supportsImageArtifactIdentity,omitempty"`
	// SupportsHonestReconfigure reports that the provider's Reconfigure never
	// reports success for a change it did not apply: each change is applied
	// to the running VM and its persistent definition, or to the definition
	// only and reported as restart-required, or the call fails. A VM on a
	// clustered Provider that does not report this is not resized
	// (Reconfiguring=False/ProviderLacksHonestReconfigure): its
	// committed-capacity accounting trusts the result.
	// +optional
	SupportsHonestReconfigure bool `json:"supportsHonestReconfigure,omitempty"`
}

// ProviderAdoptionStatus tracks VM adoption progress
type ProviderAdoptionStatus struct {
	// LastDiscoveryTime is when VMs were last discovered
	// +optional
	LastDiscoveryTime *metav1.Time `json:"lastDiscoveryTime,omitempty"`

	// DiscoveredVMs is the count of VMs found in provider
	// +optional
	DiscoveredVMs int32 `json:"discoveredVMs,omitempty"`

	// AdoptedVMs is the count of VMs successfully adopted
	// +optional
	AdoptedVMs int32 `json:"adoptedVMs,omitempty"`

	// FailedAdoptions is the count of failed adoption attempts
	// +optional
	FailedAdoptions int32 `json:"failedAdoptions,omitempty"`

	// Message provides details about adoption status
	// +optional
	Message string `json:"message,omitempty"`
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
	// Datastore specifies the default datastore.
	// Mutually exclusive with StoragePod; Datastore takes precedence if both are set.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Datastore string `json:"datastore,omitempty"`

	// StoragePod specifies a vSphere Datastore Cluster (StoragePod) used as the default
	// for automatic datastore selection when no explicit Datastore is specified.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	StoragePod string `json:"storagePod,omitempty"`

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

// RateLimit configures API rate limiting. Accepted but not acted on in this
// release.
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

func init() {
	SchemeBuilder.Register(&Provider{}, &ProviderList{})
}
