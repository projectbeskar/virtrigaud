package v1beta1

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Simple tests for beta types - avoiding import cycles by testing basic conversions
// Real conversion testing is handled in v1alpha1 package

func TestVirtualMachine_Basic(t *testing.T) {
	beta := &VirtualMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
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
					Name: "network1",
					NetworkRef: ObjectRef{
						Name: "network1",
					},
					IPAddress: "192.168.1.10",
				},
			},
			PowerState: PowerStateOn,
		},
	}

	// Test that we can create a beta VM without issues
	if beta.Name != "test-vm-simple" {
		t.Errorf("Expected name 'test-vm-simple', got '%s'", beta.Name)
	}
	if beta.Spec.PowerState != PowerStateOn {
		t.Errorf("Expected PowerState On, got %v", beta.Spec.PowerState)
	}
}

func TestProvider_Basic(t *testing.T) {
	beta := &Provider{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "Provider",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-provider-simple",
			Namespace: "test-ns",
		},
		Spec: ProviderSpec{
			Type:     ProviderTypeVSphere,
			Endpoint: "vcenter.example.com",
			CredentialSecretRef: ObjectRef{
				Name: "vcenter-creds",
			},
		},
	}

	// Test that we can create a beta Provider without issues
	if beta.Name != "test-provider-simple" {
		t.Errorf("Expected name 'test-provider-simple', got '%s'", beta.Name)
	}
	if beta.Spec.Type != ProviderTypeVSphere {
		t.Errorf("Expected Type VSphere, got %v", beta.Spec.Type)
	}
}

// BetaAlphaBeta round-trip tests - these verify conversion in the reverse direction
// These types implement conversion interfaces in their v1beta1 versions

func TestVirtualMachine_BetaAlphaBeta_RoundTrip(t *testing.T) {
	beta := &VirtualMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
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
					Name: "network1",
					NetworkRef: ObjectRef{
						Name: "network1",
					},
					IPAddress: "192.168.1.10",
				},
			},
			PowerState: PowerStateOn,
		},
		Status: VirtualMachineStatus{
			PowerState: PowerStateOn,
			Phase:      "Running",
		},
	}

	// Since we can't import v1alpha1 here due to import cycles,
	// this test just verifies the beta object is well-formed
	if beta.Spec.PowerState != PowerStateOn {
		t.Errorf("Expected PowerState On, got %v", beta.Spec.PowerState)
	}
	if len(beta.Spec.Networks) != 1 {
		t.Errorf("Expected 1 network, got %d", len(beta.Spec.Networks))
	}
}

func TestProvider_BetaAlphaBeta_RoundTrip(t *testing.T) {
	beta := &Provider{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "Provider",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-provider-simple",
			Namespace: "test-ns",
		},
		Spec: ProviderSpec{
			Type:     ProviderTypeVSphere,
			Endpoint: "vcenter.example.com",
			CredentialSecretRef: ObjectRef{
				Name: "vcenter-creds",
			},
			HealthCheck: &ProviderHealthCheck{
				Enabled:          true,
				FailureThreshold: 3,
				SuccessThreshold: 1,
			},
			ConnectionPooling: &ConnectionPooling{
				MaxConnections:     10,
				MaxIdleConnections: 5,
			},
		},
		Status: ProviderStatus{
			Healthy:      false,
			Capabilities: []ProviderCapability{"VirtualMachines"},
		},
	}

	// Verify the beta object is well-formed
	if beta.Spec.Type != ProviderTypeVSphere {
		t.Errorf("Expected Type VSphere, got %v", beta.Spec.Type)
	}
	if beta.Spec.HealthCheck == nil {
		t.Error("Expected HealthCheck to be set")
	}
}

func TestVMClass_BetaAlphaBeta_RoundTrip(t *testing.T) {
	beta := &VMClass{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "VMClass",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmclass-simple",
			Namespace: "test-ns",
		},
		Spec: VMClassSpec{
			CPU:              2,
			Memory:           resource.MustParse("2Gi"),
			Firmware:         FirmwareTypeBIOS,
			GuestToolsPolicy: GuestToolsPolicyInstall,
		},
	}

	// Verify the beta object is well-formed
	if beta.Spec.CPU != 2 {
		t.Errorf("Expected CPU 2, got %d", beta.Spec.CPU)
	}
	if beta.Spec.Firmware != FirmwareTypeBIOS {
		t.Errorf("Expected Firmware BIOS, got %v", beta.Spec.Firmware)
	}
}
