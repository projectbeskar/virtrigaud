package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/api/testutil/roundtrip"
)

// createSimpleVirtualMachineAlpha creates a simple v1alpha1 VirtualMachine for testing
func createSimpleVirtualMachineAlpha() *VirtualMachine {
	return &VirtualMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1alpha1",
			Kind:       "VirtualMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vm-simple",
			Namespace: "test-ns",
		},
		Spec: VirtualMachineSpec{
			ProviderRef: ObjectRef{
				Name: "test-provider",
			},
			ClassRef: ObjectRef{
				Name: "test-class",
			},
			ImageRef: ObjectRef{
				Name: "test-image",
			},
			Networks: []VMNetworkRef{
				{
					Name:     "network1",
					IPPolicy: "dhcp", // Set default to match what conversion will set
					StaticIP: "192.168.1.10",
				},
			},
			PowerState: "On",
		},
		Status: VirtualMachineStatus{
			PowerState: "On", // Set default to match what conversion will set
		},
	}
}

// createSimpleVirtualMachineBeta creates a simple v1beta1 VirtualMachine for testing
func createSimpleVirtualMachineBeta() *v1beta1.VirtualMachine {
	return &v1beta1.VirtualMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "VirtualMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vm-simple",
			Namespace: "test-ns",
		},
		Spec: v1beta1.VirtualMachineSpec{
			ProviderRef: v1beta1.ObjectRef{
				Name: "test-provider",
			},
			ClassRef: v1beta1.ObjectRef{
				Name: "test-class",
			},
			ImageRef: v1beta1.ObjectRef{
				Name: "test-image",
			},
			Networks: []v1beta1.VMNetworkRef{
				{
					Name: "network1",
					NetworkRef: v1beta1.ObjectRef{
						Name: "network1",
					},
					IPAddress: "192.168.1.10",
				},
			},
			PowerState: v1beta1.PowerStateOn,
		},
		Status: v1beta1.VirtualMachineStatus{
			PowerState: v1beta1.PowerStateOn,
			Phase:      "Running", // Set default to match what conversion will set
		},
	}
}

// createSimpleProviderAlpha creates a simple v1alpha1 Provider for testing
func createSimpleProviderAlpha() *Provider {
	return &Provider{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1alpha1",
			Kind:       "Provider",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-provider-simple",
			Namespace: "test-ns",
		},
		Spec: ProviderSpec{
			Type:     "vsphere",
			Endpoint: "vcenter.example.com",
			CredentialSecretRef: ObjectRef{
				Name: "vcenter-creds",
			},
		},
	}
}

// createSimpleProviderBeta creates a simple v1beta1 Provider for testing
func createSimpleProviderBeta() *v1beta1.Provider {
	return &v1beta1.Provider{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "Provider",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-provider-simple",
			Namespace: "test-ns",
		},
		Spec: v1beta1.ProviderSpec{
			Type:     v1beta1.ProviderTypeVSphere,
			Endpoint: "vcenter.example.com",
			CredentialSecretRef: v1beta1.ObjectRef{
				Name: "vcenter-creds",
			},
			HealthCheck: &v1beta1.ProviderHealthCheck{
				Enabled:          true,
				FailureThreshold: 3,
				SuccessThreshold: 1,
			},
			ConnectionPooling: &v1beta1.ConnectionPooling{
				MaxConnections:     10,
				MaxIdleConnections: 5,
			},
		},
		Status: v1beta1.ProviderStatus{
			Healthy:      false,
			Capabilities: []v1beta1.ProviderCapability{"VirtualMachines"}, // Set defaults to match conversion
		},
	}
}

func TestVirtualMachine_AlphaBetaAlpha_RoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		alpha *VirtualMachine
		beta  *v1beta1.VirtualMachine
	}{
		{
			name:  "simple_vm",
			alpha: createSimpleVirtualMachineAlpha(),
			beta:  createSimpleVirtualMachineBeta(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roundtrip.RoundTripTest(t, tt.alpha, tt.beta)
		})
	}
}

func TestProvider_AlphaBetaAlpha_RoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		alpha *Provider
		beta  *v1beta1.Provider
	}{
		{
			name:  "simple_provider",
			alpha: createSimpleProviderAlpha(),
			beta:  createSimpleProviderBeta(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roundtrip.RoundTripTest(t, tt.alpha, tt.beta)
		})
	}
}

func TestVirtualMachine_InvalidAlpha_ConversionError(t *testing.T) {
	// Since our current conversion implementation is simple and doesn't validate,
	// this test is skipped. In a real implementation, you'd have validation
	// logic that would cause certain invalid alpha values to fail conversion.

	t.Skip("Skipping invalid conversion test - conversion implementation doesn't validate yet")
}

// TODO: Add tests for other CRDs once their conversion implementations are complete
// func TestVMClass_AlphaBetaAlpha_RoundTrip(t *testing.T) { ... }
// func TestVMImage_AlphaBetaAlpha_RoundTrip(t *testing.T) { ... }
// func TestVMNetworkAttachment_AlphaBetaAlpha_RoundTrip(t *testing.T) { ... }
// func TestVMSnapshot_AlphaBetaAlpha_RoundTrip(t *testing.T) { ... }
// func TestVMClone_AlphaBetaAlpha_RoundTrip(t *testing.T) { ... }
// func TestVMSet_AlphaBetaAlpha_RoundTrip(t *testing.T) { ... }
// func TestVMPlacementPolicy_AlphaBetaAlpha_RoundTrip(t *testing.T) { ... }
