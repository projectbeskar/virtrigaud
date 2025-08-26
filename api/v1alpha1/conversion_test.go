package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/api/testutil/roundtrip"
)

// VMClass fixture creation functions
func createSimpleVMClassAlpha() *VMClass {
	return &VMClass{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1alpha1",
			Kind:       "VMClass",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmclass-simple",
			Namespace: "test-ns",
		},
		Spec: VMClassSpec{
			CPU:              2,
			MemoryMiB:        2048,
			Firmware:         "BIOS",
			GuestToolsPolicy: "install",
		},
	}
}

func createSimpleVMClassBeta() *v1beta1.VMClass {
	return &v1beta1.VMClass{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "VMClass",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmclass-simple",
			Namespace: "test-ns",
		},
		Spec: v1beta1.VMClassSpec{
			CPU:              2,
			Memory:           resource.MustParse("2Gi"),
			Firmware:         v1beta1.FirmwareTypeBIOS,
			GuestToolsPolicy: v1beta1.GuestToolsPolicyInstall,
		},
	}
}

// VMImage fixture creation functions
func createSimpleVMImageAlpha() *VMImage {
	return &VMImage{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1alpha1",
			Kind:       "VMImage",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmimage-simple",
			Namespace: "test-ns",
		},
		Spec: VMImageSpec{
			VSphere: &VSphereImageSpec{
				TemplateName: "ubuntu-template",
			},
		},
	}
}

func createSimpleVMImageBeta() *v1beta1.VMImage {
	return &v1beta1.VMImage{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "VMImage",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmimage-simple",
			Namespace: "test-ns",
		},
		Spec: v1beta1.VMImageSpec{
			Source: v1beta1.ImageSource{
				VSphere: &v1beta1.VSphereImageSource{
					TemplateName: "ubuntu-template",
				},
			},
		},
	}
}

// VMNetworkAttachment fixture creation functions
func createSimpleVMNetworkAttachmentAlpha() *VMNetworkAttachment {
	return &VMNetworkAttachment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1alpha1",
			Kind:       "VMNetworkAttachment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmnetwork-simple",
			Namespace: "test-ns",
		},
		Spec: VMNetworkAttachmentSpec{
			IPPolicy: "dhcp",
		},
	}
}

func createSimpleVMNetworkAttachmentBeta() *v1beta1.VMNetworkAttachment {
	return &v1beta1.VMNetworkAttachment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "VMNetworkAttachment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmnetwork-simple",
			Namespace: "test-ns",
		},
		Spec: v1beta1.VMNetworkAttachmentSpec{
			IPAllocation: &v1beta1.IPAllocationConfig{
				Type: v1beta1.IPAllocationTypeDHCP,
			},
		},
	}
}

// VMSnapshot fixture creation functions
func createSimpleVMSnapshotAlpha() *VMSnapshot {
	return &VMSnapshot{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1alpha1",
			Kind:       "VMSnapshot",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmsnapshot-simple",
			Namespace: "test-ns",
		},
		Spec: VMSnapshotSpec{
			VMRef: LocalObjectReference{
				Name: "test-vm",
			},
			NameHint: "snapshot-1",
		},
	}
}

func createSimpleVMSnapshotBeta() *v1beta1.VMSnapshot {
	return &v1beta1.VMSnapshot{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "VMSnapshot",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmsnapshot-simple",
			Namespace: "test-ns",
		},
		Spec: v1beta1.VMSnapshotSpec{
			VMRef: v1beta1.LocalObjectReference{
				Name: "test-vm",
			},
			SnapshotConfig: &v1beta1.SnapshotConfig{
				Name: "snapshot-1",
			},
		},
	}
}

// VMClone fixture creation functions
func createSimpleVMCloneAlpha() *VMClone {
	return &VMClone{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1alpha1",
			Kind:       "VMClone",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmclone-simple",
			Namespace: "test-ns",
		},
		Spec: VMCloneSpec{
			SourceRef: LocalObjectReference{
				Name: "source-vm",
			},
			Target: VMCloneTarget{
				Name: "cloned-vm",
			},
		},
	}
}

func createSimpleVMCloneBeta() *v1beta1.VMClone {
	return &v1beta1.VMClone{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "VMClone",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmclone-simple",
			Namespace: "test-ns",
		},
		Spec: v1beta1.VMCloneSpec{
			Source: v1beta1.CloneSource{
				VMRef: &v1beta1.LocalObjectReference{
					Name: "source-vm",
				},
			},
			Target: v1beta1.VMCloneTarget{
				Name: "cloned-vm",
			},
		},
	}
}

// VMSet fixture creation functions
func createSimpleVMSetAlpha() *VMSet {
	return &VMSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1alpha1",
			Kind:       "VMSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmset-simple",
			Namespace: "test-ns",
		},
		Spec: VMSetSpec{
			Replicas: func() *int32 { r := int32(3); return &r }(),
			Template: VMSetTemplate{
				Spec: VirtualMachineSpec{
					ProviderRef: ObjectRef{Name: "test-provider"},
					ClassRef:    ObjectRef{Name: "test-class"},
					ImageRef:    ObjectRef{Name: "test-image"},
				},
			},
		},
	}
}

func createSimpleVMSetBeta() *v1beta1.VMSet {
	return &v1beta1.VMSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "VMSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmset-simple",
			Namespace: "test-ns",
		},
		Spec: v1beta1.VMSetSpec{
			Replicas: func() *int32 { r := int32(3); return &r }(),
			Template: v1beta1.VMSetTemplate{
				Spec: v1beta1.VirtualMachineSpec{
					ProviderRef: v1beta1.ObjectRef{Name: "test-provider"},
					ClassRef:    v1beta1.ObjectRef{Name: "test-class"},
					ImageRef:    v1beta1.ObjectRef{Name: "test-image"},
				},
			},
		},
	}
}

// VMPlacementPolicy fixture creation functions
func createSimpleVMPlacementPolicyAlpha() *VMPlacementPolicy {
	return &VMPlacementPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1alpha1",
			Kind:       "VMPlacementPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmplacementpolicy-simple",
			Namespace: "test-ns",
		},
		Spec: VMPlacementPolicySpec{
			Hard: &PlacementConstraints{
				Clusters: []string{"cluster1"},
			},
		},
	}
}

func createSimpleVMPlacementPolicyBeta() *v1beta1.VMPlacementPolicy {
	return &v1beta1.VMPlacementPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "VMPlacementPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmplacementpolicy-simple",
			Namespace: "test-ns",
		},
		Spec: v1beta1.VMPlacementPolicySpec{
			Hard: &v1beta1.PlacementConstraints{
				Clusters: []string{"cluster1"},
			},
		},
	}
}

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

// createVirtualMachineWithEmptyPowerStateAlpha creates a VM with explicitly empty PowerState
func createVirtualMachineWithEmptyPowerStateAlpha() *VirtualMachine {
	vm := createSimpleVirtualMachineAlpha()
	vm.ObjectMeta.Name = "test-vm-empty-powerstate"
	// Explicitly ensure PowerState is empty (default Go zero value)
	vm.Spec.PowerState = ""
	vm.Status.PowerState = ""
	return vm
}

func createVirtualMachineWithEmptyPowerStateBeta() *v1beta1.VirtualMachine {
	vm := createSimpleVirtualMachineBeta()
	vm.ObjectMeta.Name = "test-vm-empty-powerstate"
	// Explicitly ensure PowerState is empty (default Go zero value)
	vm.Spec.PowerState = ""
	vm.Status.PowerState = ""
	vm.Status.Phase = ""
	return vm
}

func TestVirtualMachine_AlphaBetaAlpha_RoundTrip_EmptyPowerState(t *testing.T) {
	tests := []struct {
		name  string
		alpha *VirtualMachine
		beta  *v1beta1.VirtualMachine
	}{
		{
			name:  "vm_empty_powerstate",
			alpha: createVirtualMachineWithEmptyPowerStateAlpha(),
			beta:  createVirtualMachineWithEmptyPowerStateBeta(),
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

// VMClass Tests
func TestVMClass_AlphaBetaAlpha_RoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		alpha *VMClass
		beta  *v1beta1.VMClass
	}{
		{
			name:  "simple_vmclass",
			alpha: createSimpleVMClassAlpha(),
			beta:  createSimpleVMClassBeta(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roundtrip.RoundTripTest(t, tt.alpha, tt.beta)
		})
	}
}

// VMImage Tests
func TestVMImage_AlphaBetaAlpha_RoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		alpha *VMImage
		beta  *v1beta1.VMImage
	}{
		{
			name:  "simple_vmimage",
			alpha: createSimpleVMImageAlpha(),
			beta:  createSimpleVMImageBeta(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roundtrip.RoundTripTest(t, tt.alpha, tt.beta)
		})
	}
}

// VMNetworkAttachment Tests
func TestVMNetworkAttachment_AlphaBetaAlpha_RoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		alpha *VMNetworkAttachment
		beta  *v1beta1.VMNetworkAttachment
	}{
		{
			name:  "simple_vmnetworkattachment",
			alpha: createSimpleVMNetworkAttachmentAlpha(),
			beta:  createSimpleVMNetworkAttachmentBeta(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roundtrip.RoundTripTest(t, tt.alpha, tt.beta)
		})
	}
}

// VMSnapshot Tests
func TestVMSnapshot_AlphaBetaAlpha_RoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		alpha *VMSnapshot
		beta  *v1beta1.VMSnapshot
	}{
		{
			name:  "simple_vmsnapshot",
			alpha: createSimpleVMSnapshotAlpha(),
			beta:  createSimpleVMSnapshotBeta(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roundtrip.RoundTripTest(t, tt.alpha, tt.beta)
		})
	}
}

// VMClone Tests
func TestVMClone_AlphaBetaAlpha_RoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		alpha *VMClone
		beta  *v1beta1.VMClone
	}{
		{
			name:  "simple_vmclone",
			alpha: createSimpleVMCloneAlpha(),
			beta:  createSimpleVMCloneBeta(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roundtrip.RoundTripTest(t, tt.alpha, tt.beta)
		})
	}
}

// createVMSetWithEmptyPowerStateAlpha creates a VMSet with explicitly empty PowerState
func createVMSetWithEmptyPowerStateAlpha() *VMSet {
	vmset := createSimpleVMSetAlpha()
	vmset.ObjectMeta.Name = "test-vmset-empty-powerstate"
	// Explicitly ensure PowerState is empty (default Go zero value)
	vmset.Spec.Template.Spec.PowerState = ""
	return vmset
}

func createVMSetWithEmptyPowerStateBeta() *v1beta1.VMSet {
	vmset := createSimpleVMSetBeta()
	vmset.ObjectMeta.Name = "test-vmset-empty-powerstate"
	// Explicitly ensure PowerState is empty (default Go zero value)
	vmset.Spec.Template.Spec.PowerState = ""
	return vmset
}

// VMSet Tests
func TestVMSet_AlphaBetaAlpha_RoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		alpha *VMSet
		beta  *v1beta1.VMSet
	}{
		{
			name:  "simple_vmset",
			alpha: createSimpleVMSetAlpha(),
			beta:  createSimpleVMSetBeta(),
		},
		{
			name:  "vmset_empty_powerstate",
			alpha: createVMSetWithEmptyPowerStateAlpha(),
			beta:  createVMSetWithEmptyPowerStateBeta(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roundtrip.RoundTripTest(t, tt.alpha, tt.beta)
		})
	}
}

// VMPlacementPolicy Tests
func TestVMPlacementPolicy_AlphaBetaAlpha_RoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		alpha *VMPlacementPolicy
		beta  *v1beta1.VMPlacementPolicy
	}{
		{
			name:  "simple_vmplacementpolicy",
			alpha: createSimpleVMPlacementPolicyAlpha(),
			beta:  createSimpleVMPlacementPolicyBeta(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roundtrip.RoundTripTest(t, tt.alpha, tt.beta)
		})
	}
}
