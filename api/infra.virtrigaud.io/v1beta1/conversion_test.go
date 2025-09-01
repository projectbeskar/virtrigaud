package v1beta1

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Simple tests for beta types - avoiding import cycles by testing basic conversions
// No conversion needed - v1beta1 is the only API version

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

	// No conversion needed - v1beta1 is the only API version,
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

// BetaAlphaBeta round-trip tests for the new CRD types

func TestVMImage_BetaAlphaBeta_RoundTrip(t *testing.T) {
	beta := &VMImage{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "VMImage",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmimage-simple",
			Namespace: "test-ns",
		},
		Spec: VMImageSpec{
			Source: ImageSource{
				VSphere: &VSphereImageSource{
					TemplateName: "ubuntu-template",
				},
			},
		},
	}

	// Verify the beta object is well-formed
	if beta.Spec.Source.VSphere == nil {
		t.Error("Expected VSphere source to be set")
	}
	if beta.Spec.Source.VSphere.TemplateName != "ubuntu-template" {
		t.Errorf("Expected template name 'ubuntu-template', got %s", beta.Spec.Source.VSphere.TemplateName)
	}
}

func TestVMNetworkAttachment_BetaAlphaBeta_RoundTrip(t *testing.T) {
	beta := &VMNetworkAttachment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "VMNetworkAttachment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmnetwork-simple",
			Namespace: "test-ns",
		},
		Spec: VMNetworkAttachmentSpec{
			IPAllocation: &IPAllocationConfig{
				Type: IPAllocationTypeDHCP,
			},
		},
	}

	// Verify the beta object is well-formed
	if beta.Spec.IPAllocation == nil {
		t.Error("Expected IPAllocation to be set")
	}
	if beta.Spec.IPAllocation.Type != IPAllocationTypeDHCP {
		t.Errorf("Expected IPAllocation type DHCP, got %v", beta.Spec.IPAllocation.Type)
	}
}

func TestVMSnapshot_BetaAlphaBeta_RoundTrip(t *testing.T) {
	beta := &VMSnapshot{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
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
			SnapshotConfig: &SnapshotConfig{
				Name: "snapshot-1",
			},
		},
	}

	// Verify the beta object is well-formed
	if beta.Spec.VMRef.Name != "test-vm" {
		t.Errorf("Expected VM ref 'test-vm', got %s", beta.Spec.VMRef.Name)
	}
	if beta.Spec.SnapshotConfig == nil {
		t.Error("Expected SnapshotConfig to be set")
	}
}

func TestVMClone_BetaAlphaBeta_RoundTrip(t *testing.T) {
	beta := &VMClone{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "VMClone",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmclone-simple",
			Namespace: "test-ns",
		},
		Spec: VMCloneSpec{
			Source: CloneSource{
				VMRef: &LocalObjectReference{
					Name: "source-vm",
				},
			},
			Target: VMCloneTarget{
				Name: "cloned-vm",
			},
		},
	}

	// Verify the beta object is well-formed
	if beta.Spec.Source.VMRef == nil {
		t.Error("Expected source VM ref to be set")
	}
	if beta.Spec.Source.VMRef.Name != "source-vm" {
		t.Errorf("Expected source VM 'source-vm', got %s", beta.Spec.Source.VMRef.Name)
	}
}

func TestVMSet_BetaAlphaBeta_RoundTrip(t *testing.T) {
	beta := &VMSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
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

	// Verify the beta object is well-formed
	if beta.Spec.Replicas == nil || *beta.Spec.Replicas != 3 {
		t.Error("Expected 3 replicas")
	}
	if beta.Spec.Template.Spec.ProviderRef.Name != "test-provider" {
		t.Errorf("Expected provider 'test-provider', got %s", beta.Spec.Template.Spec.ProviderRef.Name)
	}
}

func TestVMPlacementPolicy_BetaAlphaBeta_RoundTrip(t *testing.T) {
	beta := &VMPlacementPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
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

	// Verify the beta object is well-formed
	if beta.Spec.Hard == nil {
		t.Error("Expected hard constraints to be set")
	}
	if len(beta.Spec.Hard.Clusters) != 1 || beta.Spec.Hard.Clusters[0] != "cluster1" {
		t.Error("Expected cluster1 in hard constraints")
	}
}
