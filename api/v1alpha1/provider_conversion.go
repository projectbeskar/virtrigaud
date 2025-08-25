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
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/conversion"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// ConvertTo converts this Provider to the Hub version (v1beta1)
func (src *Provider) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.Provider)

	// Convert metadata
	dst.ObjectMeta = src.ObjectMeta

	// Convert Spec
	if err := convertProviderSpec(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert Provider spec: %w", err)
	}

	// Convert Status
	if err := convertProviderStatus(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert Provider status: %w", err)
	}

	return nil
}

// ConvertFrom converts from the Hub version (v1beta1) to this version
func (dst *Provider) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.Provider)

	// Convert metadata
	dst.ObjectMeta = src.ObjectMeta

	// Convert Spec
	if err := convertProviderSpecFromBeta(&src.Spec, &dst.Spec); err != nil {
		return fmt.Errorf("failed to convert Provider spec from beta: %w", err)
	}

	// Convert Status
	if err := convertProviderStatusFromBeta(&src.Status, &dst.Status); err != nil {
		return fmt.Errorf("failed to convert Provider status from beta: %w", err)
	}

	return nil
}

// convertProviderSpec converts v1alpha1 ProviderSpec to v1beta1
func convertProviderSpec(src *ProviderSpec, dst *v1beta1.ProviderSpec) error {
	// Convert Type - alpha uses string, beta uses typed enum
	switch src.Type {
	case "vsphere":
		dst.Type = v1beta1.ProviderTypeVSphere
	case "libvirt":
		dst.Type = v1beta1.ProviderTypeLibvirt
	case "firecracker":
		dst.Type = v1beta1.ProviderTypeFirecracker
	case "qemu":
		dst.Type = v1beta1.ProviderTypeQEMU
	default:
		// Map unknown types to a safe default
		dst.Type = v1beta1.ProviderTypeLibvirt
	}

	// Direct field mappings
	dst.Endpoint = src.Endpoint
	dst.CredentialSecretRef = convertObjectRef(src.CredentialSecretRef)
	dst.InsecureSkipVerify = src.InsecureSkipVerify

	// Convert Defaults
	if src.Defaults != nil {
		dst.Defaults = &v1beta1.ProviderDefaults{
			Datastore:    src.Defaults.Datastore,
			Cluster:      src.Defaults.Cluster,
			Folder:       src.Defaults.Folder,
			ResourcePool: "", // New field in beta
			Network:      "", // New field in beta
		}
	}

	// Convert RateLimit
	if src.RateLimit != nil {
		dst.RateLimit = &v1beta1.RateLimit{
			QPS:   src.RateLimit.QPS,
			Burst: src.RateLimit.Burst,
		}
	}

	// Convert Runtime - expand from simple alpha version to full beta version
	if src.Runtime != nil {
		dst.Runtime = &v1beta1.ProviderRuntimeSpec{
			Mode:  convertRuntimeMode(src.Runtime.Mode),
			Image: src.Runtime.Image,
		}

		// Set defaults for new fields in beta
		if src.Runtime.Replicas != nil {
			dst.Runtime.Replicas = src.Runtime.Replicas
		}

		// Convert Service configuration
		if src.Runtime.Service != nil {
			dst.Runtime.Service = &v1beta1.ProviderServiceSpec{
				Port: src.Runtime.Service.Port,
			}
			// Convert TLS configuration
			if src.Runtime.TLS != nil {
				dst.Runtime.Service.TLS = &v1beta1.ProviderTLSSpec{
					Enabled:            src.Runtime.TLS.Enabled,
					InsecureSkipVerify: false, // Default to secure
				}
				if src.Runtime.TLS.SecretRef.Name != "" {
					dst.Runtime.Service.TLS.SecretRef = &src.Runtime.TLS.SecretRef
				}
			}
		}

		// Convert other runtime fields with defaults for new beta fields
		dst.Runtime.Resources = src.Runtime.Resources
		dst.Runtime.NodeSelector = src.Runtime.NodeSelector
		dst.Runtime.Tolerations = src.Runtime.Tolerations
		dst.Runtime.Affinity = src.Runtime.Affinity
		dst.Runtime.SecurityContext = src.Runtime.SecurityContext
		dst.Runtime.Env = src.Runtime.Env

		// New fields in beta - set to nil/defaults
		dst.Runtime.ImagePullPolicy = "IfNotPresent" // Default pull policy
		dst.Runtime.ImagePullSecrets = nil
		dst.Runtime.LivenessProbe = nil
		dst.Runtime.ReadinessProbe = nil
	}

	// New fields in beta - set sensible defaults
	dst.HealthCheck = &v1beta1.ProviderHealthCheck{
		Enabled:          true,
		FailureThreshold: 3,
		SuccessThreshold: 1,
	}
	dst.ConnectionPooling = &v1beta1.ConnectionPooling{
		MaxConnections:     10,
		MaxIdleConnections: 5,
	}

	return nil
}

// convertProviderStatus converts v1alpha1 ProviderStatus to v1beta1
func convertProviderStatus(src *ProviderStatus, dst *v1beta1.ProviderStatus) error {
	// Direct field mappings
	dst.Healthy = src.Healthy
	dst.LastHealthCheck = src.LastHealthCheck
	dst.Conditions = src.Conditions
	dst.ObservedGeneration = src.ObservedGeneration

	// Convert Runtime status
	if src.Runtime != nil {
		dst.Runtime = &v1beta1.ProviderRuntimeStatus{
			Mode:              convertRuntimeMode(src.Runtime.Mode),
			Endpoint:          src.Runtime.Endpoint,
			ServiceRef:        src.Runtime.ServiceRef,
			Message:           src.Runtime.Message,
			ReadyReplicas:     0, // Default for new field
			AvailableReplicas: 0, // Default for new field
		}

		// Convert phase - alpha uses string, beta uses typed enum
		switch src.Runtime.Phase {
		case "Pending":
			dst.Runtime.Phase = v1beta1.ProviderRuntimePhasePending
		case "Starting":
			dst.Runtime.Phase = v1beta1.ProviderRuntimePhaseStarting
		case "Running":
			dst.Runtime.Phase = v1beta1.ProviderRuntimePhaseRunning
		case "Stopping":
			dst.Runtime.Phase = v1beta1.ProviderRuntimePhaseStopping
		case "Failed":
			dst.Runtime.Phase = v1beta1.ProviderRuntimePhaseFailed
		default:
			dst.Runtime.Phase = v1beta1.ProviderRuntimePhasePending
		}
	}

	// New fields in beta - set sensible defaults
	dst.Capabilities = []v1beta1.ProviderCapability{
		v1beta1.ProviderCapabilityVirtualMachines,
	} // Default capabilities
	dst.Version = ""        // Not present in alpha
	dst.ConnectedVMs = 0    // Not present in alpha
	dst.ResourceUsage = nil // Not present in alpha

	return nil
}

// convertProviderSpecFromBeta converts v1beta1 ProviderSpec to v1alpha1
func convertProviderSpecFromBeta(src *v1beta1.ProviderSpec, dst *ProviderSpec) error {
	// Convert Type - beta uses typed enum, alpha uses string
	switch src.Type {
	case v1beta1.ProviderTypeVSphere:
		dst.Type = "vsphere"
	case v1beta1.ProviderTypeLibvirt:
		dst.Type = "libvirt"
	case v1beta1.ProviderTypeFirecracker:
		dst.Type = "firecracker"
	case v1beta1.ProviderTypeQEMU:
		dst.Type = "qemu"
	case v1beta1.ProviderTypeProxmox:
		dst.Type = "proxmox" // This type didn't exist in alpha, but we support it
	default:
		dst.Type = "libvirt" // Default fallback
	}

	// Direct field mappings
	dst.Endpoint = src.Endpoint
	dst.CredentialSecretRef = convertObjectRefFromBeta(src.CredentialSecretRef)
	dst.InsecureSkipVerify = src.InsecureSkipVerify

	// Convert Defaults - remove new beta fields
	if src.Defaults != nil {
		dst.Defaults = &ProviderDefaults{
			Datastore: src.Defaults.Datastore,
			Cluster:   src.Defaults.Cluster,
			Folder:    src.Defaults.Folder,
		}
	}

	// Convert RateLimit
	if src.RateLimit != nil {
		dst.RateLimit = &RateLimit{
			QPS:   src.RateLimit.QPS,
			Burst: src.RateLimit.Burst,
		}
	}

	// Convert Runtime - simplify from full beta version to alpha version
	if src.Runtime != nil {
		dst.Runtime = &ProviderRuntimeSpec{
			Mode:     convertRuntimeModeFromBeta(src.Runtime.Mode),
			Image:    src.Runtime.Image,
			Replicas: src.Runtime.Replicas,
		}

		// Convert Service configuration
		if src.Runtime.Service != nil {
			dst.Runtime.Service = &ProviderServiceSpec{
				Port: src.Runtime.Service.Port,
			}
			// Convert TLS configuration
			if src.Runtime.Service.TLS != nil {
				dst.Runtime.TLS = &ProviderTLSSpec{
					Enabled: src.Runtime.Service.TLS.Enabled,
				}
				if src.Runtime.Service.TLS.SecretRef != nil {
					dst.Runtime.TLS.SecretRef = *src.Runtime.Service.TLS.SecretRef
				}
			}
		}

		// Convert other runtime fields - remove new beta fields
		dst.Runtime.Resources = src.Runtime.Resources
		dst.Runtime.NodeSelector = src.Runtime.NodeSelector
		dst.Runtime.Tolerations = src.Runtime.Tolerations
		dst.Runtime.Affinity = src.Runtime.Affinity
		dst.Runtime.SecurityContext = src.Runtime.SecurityContext
		dst.Runtime.Env = src.Runtime.Env

		// New beta fields (ImagePullPolicy, ImagePullSecrets, Probes) are ignored
	}

	// New beta fields (HealthCheck, ConnectionPooling) are ignored

	return nil
}

// convertProviderStatusFromBeta converts v1beta1 ProviderStatus to v1alpha1
func convertProviderStatusFromBeta(src *v1beta1.ProviderStatus, dst *ProviderStatus) error {
	// Direct field mappings
	dst.Healthy = src.Healthy
	dst.LastHealthCheck = src.LastHealthCheck
	dst.Conditions = src.Conditions
	dst.ObservedGeneration = src.ObservedGeneration

	// Convert Runtime status
	if src.Runtime != nil {
		dst.Runtime = &ProviderRuntimeStatus{
			Mode:       convertRuntimeModeFromBeta(src.Runtime.Mode),
			Endpoint:   src.Runtime.Endpoint,
			ServiceRef: src.Runtime.ServiceRef,
			Message:    src.Runtime.Message,
		}

		// Convert phase - beta uses typed enum, alpha uses string
		switch src.Runtime.Phase {
		case v1beta1.ProviderRuntimePhasePending:
			dst.Runtime.Phase = "Pending"
		case v1beta1.ProviderRuntimePhaseStarting:
			dst.Runtime.Phase = "Starting"
		case v1beta1.ProviderRuntimePhaseRunning:
			dst.Runtime.Phase = "Running"
		case v1beta1.ProviderRuntimePhaseStopping:
			dst.Runtime.Phase = "Stopping"
		case v1beta1.ProviderRuntimePhaseFailed:
			dst.Runtime.Phase = "Failed"
		default:
			dst.Runtime.Phase = "Pending"
		}
	}

	// New beta fields (Capabilities, Version, ConnectedVMs, ResourceUsage) are ignored

	return nil
}

// Helper functions for runtime mode conversion
func convertRuntimeMode(src ProviderRuntimeMode) v1beta1.ProviderRuntimeMode {
	switch src {
	case RuntimeModeInProcess:
		return v1beta1.RuntimeModeInProcess
	case RuntimeModeRemote:
		return v1beta1.RuntimeModeRemote
	default:
		return v1beta1.RuntimeModeInProcess
	}
}

func convertRuntimeModeFromBeta(src v1beta1.ProviderRuntimeMode) ProviderRuntimeMode {
	switch src {
	case v1beta1.RuntimeModeInProcess:
		return RuntimeModeInProcess
	case v1beta1.RuntimeModeRemote:
		return RuntimeModeRemote
	default:
		return RuntimeModeInProcess
	}
}
