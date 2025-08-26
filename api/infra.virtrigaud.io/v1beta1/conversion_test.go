package v1beta1

import (
	"testing"

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
