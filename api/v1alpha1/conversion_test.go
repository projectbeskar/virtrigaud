package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/api/testutil/roundtrip"
)

// createFullVirtualMachineAlpha creates a v1alpha1 VirtualMachine with all fields populated
func createFullVirtualMachineAlpha() *VirtualMachine {
	return &VirtualMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1alpha1",
			Kind:       "VirtualMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vm",
			Namespace: "default",
			Labels: map[string]string{
				"app": "test",
			},
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
					StaticIP: "192.168.1.10",
				},
			},
			PowerState: "On",
		},
	}
}

// createFullVirtualMachineBeta creates a v1beta1 VirtualMachine with all fields populated
func createFullVirtualMachineBeta() *v1beta1.VirtualMachine {
	return &v1beta1.VirtualMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "VirtualMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vm",
			Namespace: "default",
			Labels: map[string]string{
				"app": "test",
			},
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

func TestVirtualMachine_AlphaBetaAlpha_RoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		alpha *VirtualMachine
		beta  *v1beta1.VirtualMachine
	}{
		{
			name:  "full_vm",
			alpha: createFullVirtualMachineAlpha(),
			beta:  createFullVirtualMachineBeta(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roundtrip.RoundTripTest(t, tt.alpha, tt.beta)
		})
	}
}

func TestVirtualMachine_BetaAlphaBeta_RoundTrip(t *testing.T) {
	alpha := createFullVirtualMachineAlpha()
	beta := createFullVirtualMachineBeta()
	roundtrip.RoundTripTest(t, beta, alpha)
}

// TODO: Add tests for other CRDs once their conversion implementations are complete
// func TestVMClass_AlphaBetaAlpha_RoundTrip(t *testing.T) { ... }
// func TestVMImage_AlphaBetaAlpha_RoundTrip(t *testing.T) { ... }
// func TestVMNetworkAttachment_AlphaBetaAlpha_RoundTrip(t *testing.T) { ... }
// func TestVMSnapshot_AlphaBetaAlpha_RoundTrip(t *testing.T) { ... }
// func TestVMClone_AlphaBetaAlpha_RoundTrip(t *testing.T) { ... }
// func TestVMSet_AlphaBetaAlpha_RoundTrip(t *testing.T) { ... }
// func TestVMPlacementPolicy_AlphaBetaAlpha_RoundTrip(t *testing.T) { ... }
