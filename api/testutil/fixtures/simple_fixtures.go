package fixtures

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/api/v1alpha1"
)

// CreateSimpleVirtualMachineAlpha creates a simple v1alpha1 VirtualMachine for testing
func CreateSimpleVirtualMachineAlpha() *v1alpha1.VirtualMachine {
	return &v1alpha1.VirtualMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1alpha1",
			Kind:       "VirtualMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vm-simple",
			Namespace: "test-ns",
		},
		Spec: v1alpha1.VirtualMachineSpec{
			ProviderRef: v1alpha1.ObjectRef{
				Name: "test-provider",
			},
			ClassRef: v1alpha1.ObjectRef{
				Name: "test-class",
			},
			ImageRef: v1alpha1.ObjectRef{
				Name: "test-image",
			},
			Networks: []v1alpha1.VMNetworkRef{
				{
					Name:     "network1",
					StaticIP: "192.168.1.10",
				},
			},
			PowerState: "On",
		},
	}
}

// CreateSimpleVirtualMachineBeta creates a simple v1beta1 VirtualMachine for testing
func CreateSimpleVirtualMachineBeta() *v1beta1.VirtualMachine {
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
	}
}

// CreateSimpleProviderAlpha creates a simple v1alpha1 Provider for testing
func CreateSimpleProviderAlpha() *v1alpha1.Provider {
	return &v1alpha1.Provider{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1alpha1",
			Kind:       "Provider",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-provider-simple",
			Namespace: "test-ns",
		},
		Spec: v1alpha1.ProviderSpec{
			Type:     "vsphere",
			Endpoint: "vcenter.example.com",
			CredentialSecretRef: v1alpha1.ObjectRef{
				Name: "vcenter-creds",
			},
		},
	}
}

// CreateSimpleProviderBeta creates a simple v1beta1 Provider for testing
func CreateSimpleProviderBeta() *v1beta1.Provider {
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
		},
	}
}

// CreateInvalidAlphaVM creates an alpha VM that should fail conversion to beta
func CreateInvalidAlphaVM() *v1alpha1.VirtualMachine {
	return &v1alpha1.VirtualMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1alpha1",
			Kind:       "VirtualMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid-vm",
			Namespace: "test-ns",
		},
		Spec: v1alpha1.VirtualMachineSpec{
			ProviderRef: v1alpha1.ObjectRef{
				Name: "invalid-provider",
			},
			ClassRef: v1alpha1.ObjectRef{
				Name: "invalid-class",
			},
			ImageRef: v1alpha1.ObjectRef{
				Name: "invalid-image",
			},
			PowerState: "InvalidState", // This should be rejected in beta
		},
	}
}
