package conversione2e

import (
	"context"
	"os"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/api/v1alpha1"
)

func TestConversionWebhook(t *testing.T) {
	// Skip if KUBEBUILDER_ASSETS environment variable is not set
	// This will be set in CI but may not be available locally
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS environment variable not set, skipping envtest conversion tests")
	}

	ctx := context.Background()

	// Set up envtest environment with rendered CRDs (including conversion webhooks)
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../charts/virtrigaud/crds"},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		// If we can't start the test environment, skip the test
		t.Skipf("Failed to start test environment (kubebuilder tools may not be installed): %v", err)
		return
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("Failed to stop test environment: %v", err)
		}
	}()

	// Register schemes
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add client-go scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add v1alpha1 scheme: %v", err)
	}
	if err := v1beta1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add v1beta1 scheme: %v", err)
	}

	// Create client
	k8sClient, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Test VirtualMachine conversion
	t.Run("VirtualMachine_AlphaToBeta", func(t *testing.T) {
		testVMAlphaToBeta(t, ctx, k8sClient)
	})

	t.Run("VirtualMachine_BetaToAlpha", func(t *testing.T) {
		testVMBetaToAlpha(t, ctx, k8sClient)
	})

	// Test Provider conversion
	t.Run("Provider_AlphaToBeta", func(t *testing.T) {
		testProviderAlphaToBeta(t, ctx, k8sClient)
	})

	// Test VMClass conversion
	t.Run("VMClass_AlphaToBeta", func(t *testing.T) {
		testVMClassAlphaToBeta(t, ctx, k8sClient)
	})

	// Test new CRD types
	t.Run("VMImage_AlphaToBeta", func(t *testing.T) {
		testVMImageAlphaToBeta(t, ctx, k8sClient)
	})

	t.Run("VMNetworkAttachment_AlphaToBeta", func(t *testing.T) {
		testVMNetworkAttachmentAlphaToBeta(t, ctx, k8sClient)
	})

	t.Run("VMSnapshot_AlphaToBeta", func(t *testing.T) {
		testVMSnapshotAlphaToBeta(t, ctx, k8sClient)
	})

	t.Run("VMClone_AlphaToBeta", func(t *testing.T) {
		testVMCloneAlphaToBeta(t, ctx, k8sClient)
	})

	t.Run("VMSet_AlphaToBeta", func(t *testing.T) {
		testVMSetAlphaToBeta(t, ctx, k8sClient)
	})

	t.Run("VMPlacementPolicy_AlphaToBeta", func(t *testing.T) {
		testVMPlacementPolicyAlphaToBeta(t, ctx, k8sClient)
	})
}

func testVMAlphaToBeta(t *testing.T, ctx context.Context, k8sClient client.Client) {
	// Create v1alpha1 VirtualMachine
	alphaVM := &v1alpha1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vm-alpha-to-beta",
			Namespace: "default",
		},
		Spec: v1alpha1.VirtualMachineSpec{
			ProviderRef: v1alpha1.ObjectRef{Name: "test-provider"},
			ClassRef:    v1alpha1.ObjectRef{Name: "test-class"},
			ImageRef:    v1alpha1.ObjectRef{Name: "test-image"},
			PowerState:  "On",
			Networks: []v1alpha1.VMNetworkRef{
				{
					Name:     "network1",
					IPPolicy: "dhcp",
					StaticIP: "192.168.1.10",
				},
			},
		},
	}

	// Create the alpha VM
	if err := k8sClient.Create(ctx, alphaVM); err != nil {
		t.Fatalf("Failed to create alpha VM: %v", err)
	}
	defer func() {
		if err := k8sClient.Delete(ctx, alphaVM); err != nil {
			t.Errorf("Failed to delete VM: %v", err)
		}
	}()

	// Wait a bit for the object to be processed
	time.Sleep(200 * time.Millisecond)

	// Read back as v1beta1
	betaVM := &v1beta1.VirtualMachine{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(alphaVM), betaVM); err != nil {
		t.Fatalf("Failed to read VM as v1beta1: %v", err)
	}

	// Verify basic fields were converted correctly
	if betaVM.Spec.ProviderRef.Name != "test-provider" {
		t.Errorf("Expected ProviderRef.Name to be 'test-provider', got %s", betaVM.Spec.ProviderRef.Name)
	}
	if betaVM.Spec.PowerState != v1beta1.PowerStateOn {
		t.Errorf("Expected PowerState to be 'On', got %s", betaVM.Spec.PowerState)
	}
	if len(betaVM.Spec.Networks) != 1 {
		t.Errorf("Expected 1 network, got %d", len(betaVM.Spec.Networks))
	} else {
		// Verify network conversion
		if betaVM.Spec.Networks[0].Name != "network1" {
			t.Errorf("Expected network name 'network1', got %s", betaVM.Spec.Networks[0].Name)
		}
		if betaVM.Spec.Networks[0].IPAddress != "192.168.1.10" {
			t.Errorf("Expected IP address '192.168.1.10', got %s", betaVM.Spec.Networks[0].IPAddress)
		}
	}

	// Update a beta field and verify it persists
	betaVM.Spec.PowerState = v1beta1.PowerStateOff
	if err := k8sClient.Update(ctx, betaVM); err != nil {
		t.Fatalf("Failed to update beta VM: %v", err)
	}

	// Read back as alpha and verify the change persists
	updatedAlphaVM := &v1alpha1.VirtualMachine{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(alphaVM), updatedAlphaVM); err != nil {
		t.Fatalf("Failed to read updated VM as v1alpha1: %v", err)
	}

	if updatedAlphaVM.Spec.PowerState != "Off" {
		t.Errorf("Expected PowerState to be 'Off' after update, got %s", updatedAlphaVM.Spec.PowerState)
	}
}

func testVMBetaToAlpha(t *testing.T, ctx context.Context, k8sClient client.Client) {
	// Create v1beta1 VirtualMachine
	betaVM := &v1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vm-beta-to-alpha",
			Namespace: "default",
		},
		Spec: v1beta1.VirtualMachineSpec{
			ProviderRef: v1beta1.ObjectRef{Name: "test-provider"},
			ClassRef:    v1beta1.ObjectRef{Name: "test-class"},
			ImageRef:    v1beta1.ObjectRef{Name: "test-image"},
			PowerState:  v1beta1.PowerStateOn,
			Networks: []v1beta1.VMNetworkRef{
				{
					Name: "network1",
					NetworkRef: v1beta1.ObjectRef{
						Name: "network1",
					},
					IPAddress: "192.168.1.10",
				},
			},
		},
	}

	// Create the beta VM
	if err := k8sClient.Create(ctx, betaVM); err != nil {
		t.Fatalf("Failed to create beta VM: %v", err)
	}
	defer func() {
		if err := k8sClient.Delete(ctx, betaVM); err != nil {
			t.Errorf("Failed to delete VM: %v", err)
		}
	}()

	// Wait a bit for the object to be processed
	time.Sleep(200 * time.Millisecond)

	// Read back as v1alpha1
	alphaVM := &v1alpha1.VirtualMachine{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(betaVM), alphaVM); err != nil {
		t.Fatalf("Failed to read VM as v1alpha1: %v", err)
	}

	// Verify basic fields were converted correctly
	if alphaVM.Spec.ProviderRef.Name != "test-provider" {
		t.Errorf("Expected ProviderRef.Name to be 'test-provider', got %s", alphaVM.Spec.ProviderRef.Name)
	}
	if alphaVM.Spec.PowerState != "On" {
		t.Errorf("Expected PowerState to be 'On', got %s", alphaVM.Spec.PowerState)
	}
	if len(alphaVM.Spec.Networks) != 1 {
		t.Errorf("Expected 1 network, got %d", len(alphaVM.Spec.Networks))
	} else {
		// Verify network conversion back to alpha
		if alphaVM.Spec.Networks[0].Name != "network1" {
			t.Errorf("Expected network name 'network1', got %s", alphaVM.Spec.Networks[0].Name)
		}
		if alphaVM.Spec.Networks[0].StaticIP != "192.168.1.10" {
			t.Errorf("Expected static IP '192.168.1.10', got %s", alphaVM.Spec.Networks[0].StaticIP)
		}
	}
}

func testProviderAlphaToBeta(t *testing.T, ctx context.Context, k8sClient client.Client) {
	// Create v1alpha1 Provider
	alphaProvider := &v1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-provider-alpha-to-beta",
			Namespace: "default",
		},
		Spec: v1alpha1.ProviderSpec{
			Type:                "vsphere",
			Endpoint:            "vcenter.example.com",
			CredentialSecretRef: v1alpha1.ObjectRef{Name: "test-creds"},
		},
	}

	// Create the alpha Provider
	if err := k8sClient.Create(ctx, alphaProvider); err != nil {
		t.Fatalf("Failed to create alpha Provider: %v", err)
	}
	defer func() {
		if err := k8sClient.Delete(ctx, alphaProvider); err != nil {
			t.Errorf("Failed to delete Provider: %v", err)
		}
	}()

	// Wait a bit for the object to be processed
	time.Sleep(200 * time.Millisecond)

	// Read back as v1beta1
	betaProvider := &v1beta1.Provider{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(alphaProvider), betaProvider); err != nil {
		t.Fatalf("Failed to read Provider as v1beta1: %v", err)
	}

	// Verify basic fields were converted
	if betaProvider.Spec.Type != v1beta1.ProviderTypeVSphere {
		t.Errorf("Expected Type to be 'vsphere', got %s", betaProvider.Spec.Type)
	}
	if betaProvider.Spec.Endpoint != "vcenter.example.com" {
		t.Errorf("Expected Endpoint to be 'vcenter.example.com', got %s", betaProvider.Spec.Endpoint)
	}
}

func testVMClassAlphaToBeta(t *testing.T, ctx context.Context, k8sClient client.Client) {
	// Create v1alpha1 VMClass
	alphaVMClass := &v1alpha1.VMClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmclass-alpha-to-beta",
			Namespace: "default",
		},
		Spec: v1alpha1.VMClassSpec{
			CPU:              2,
			MemoryMiB:        2048,
			Firmware:         "BIOS",
			GuestToolsPolicy: "install",
		},
	}

	// Create the alpha VMClass
	if err := k8sClient.Create(ctx, alphaVMClass); err != nil {
		t.Fatalf("Failed to create alpha VMClass: %v", err)
	}
	defer func() {
		if err := k8sClient.Delete(ctx, alphaVMClass); err != nil {
			t.Errorf("Failed to delete VMClass: %v", err)
		}
	}()

	// Wait a bit for the object to be processed
	time.Sleep(200 * time.Millisecond)

	// Read back as v1beta1
	betaVMClass := &v1beta1.VMClass{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(alphaVMClass), betaVMClass); err != nil {
		t.Fatalf("Failed to read VMClass as v1beta1: %v", err)
	}

	// Verify basic fields were converted
	if betaVMClass.Spec.CPU != 2 {
		t.Errorf("Expected CPU to be 2, got %d", betaVMClass.Spec.CPU)
	}
	if betaVMClass.Spec.Firmware != v1beta1.FirmwareTypeBIOS {
		t.Errorf("Expected Firmware to be BIOS, got %s", betaVMClass.Spec.Firmware)
	}
	if betaVMClass.Spec.GuestToolsPolicy != v1beta1.GuestToolsPolicyInstall {
		t.Errorf("Expected GuestToolsPolicy to be install, got %s", betaVMClass.Spec.GuestToolsPolicy)
	}

	// Verify memory conversion (2048 MiB -> 2Gi)
	expectedMemory := betaVMClass.Spec.Memory.String()
	if expectedMemory != "2Gi" {
		t.Errorf("Expected Memory to be 2Gi, got %s", expectedMemory)
	}
}

func testVMImageAlphaToBeta(t *testing.T, ctx context.Context, k8sClient client.Client) {
	// Create v1alpha1 VMImage
	alphaVMImage := &v1alpha1.VMImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmimage-alpha-to-beta",
			Namespace: "default",
		},
		Spec: v1alpha1.VMImageSpec{
			VSphere: &v1alpha1.VSphereImageSpec{
				TemplateName: "ubuntu-template",
			},
		},
	}

	// Create the alpha VMImage
	if err := k8sClient.Create(ctx, alphaVMImage); err != nil {
		t.Fatalf("Failed to create alpha VMImage: %v", err)
	}
	defer func() {
		if err := k8sClient.Delete(ctx, alphaVMImage); err != nil {
			t.Errorf("Failed to delete VMImage: %v", err)
		}
	}()

	// Wait a bit for the object to be processed
	time.Sleep(200 * time.Millisecond)

	// Read back as v1beta1
	betaVMImage := &v1beta1.VMImage{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(alphaVMImage), betaVMImage); err != nil {
		t.Fatalf("Failed to read VMImage as v1beta1: %v", err)
	}

	// Verify basic fields were converted
	if betaVMImage.Spec.Source.VSphere == nil {
		t.Error("Expected VSphere source to be set")
	} else if betaVMImage.Spec.Source.VSphere.TemplateName != "ubuntu-template" {
		t.Errorf("Expected template name 'ubuntu-template', got %s", betaVMImage.Spec.Source.VSphere.TemplateName)
	}
}

func testVMNetworkAttachmentAlphaToBeta(t *testing.T, ctx context.Context, k8sClient client.Client) {
	// Create v1alpha1 VMNetworkAttachment
	alphaVMNetwork := &v1alpha1.VMNetworkAttachment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmnetwork-alpha-to-beta",
			Namespace: "default",
		},
		Spec: v1alpha1.VMNetworkAttachmentSpec{
			IPPolicy: "dhcp",
		},
	}

	// Create the alpha VMNetworkAttachment
	if err := k8sClient.Create(ctx, alphaVMNetwork); err != nil {
		t.Fatalf("Failed to create alpha VMNetworkAttachment: %v", err)
	}
	defer func() {
		if err := k8sClient.Delete(ctx, alphaVMNetwork); err != nil {
			t.Errorf("Failed to delete VMNetworkAttachment: %v", err)
		}
	}()

	// Wait a bit for the object to be processed
	time.Sleep(200 * time.Millisecond)

	// Read back as v1beta1
	betaVMNetwork := &v1beta1.VMNetworkAttachment{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(alphaVMNetwork), betaVMNetwork); err != nil {
		t.Fatalf("Failed to read VMNetworkAttachment as v1beta1: %v", err)
	}

	// Verify basic fields were converted
	if betaVMNetwork.Spec.IPAllocation == nil {
		t.Error("Expected IPAllocation to be set")
	} else if betaVMNetwork.Spec.IPAllocation.Type != v1beta1.IPAllocationTypeDHCP {
		t.Errorf("Expected IPAllocation type DHCP, got %v", betaVMNetwork.Spec.IPAllocation.Type)
	}
}

func testVMSnapshotAlphaToBeta(t *testing.T, ctx context.Context, k8sClient client.Client) {
	// Create v1alpha1 VMSnapshot
	alphaVMSnapshot := &v1alpha1.VMSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmsnapshot-alpha-to-beta",
			Namespace: "default",
		},
		Spec: v1alpha1.VMSnapshotSpec{
			VMRef: v1alpha1.LocalObjectReference{
				Name: "test-vm",
			},
			NameHint: "snapshot-1",
		},
	}

	// Create the alpha VMSnapshot
	if err := k8sClient.Create(ctx, alphaVMSnapshot); err != nil {
		t.Fatalf("Failed to create alpha VMSnapshot: %v", err)
	}
	defer func() {
		if err := k8sClient.Delete(ctx, alphaVMSnapshot); err != nil {
			t.Errorf("Failed to delete VMSnapshot: %v", err)
		}
	}()

	// Wait a bit for the object to be processed
	time.Sleep(200 * time.Millisecond)

	// Read back as v1beta1
	betaVMSnapshot := &v1beta1.VMSnapshot{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(alphaVMSnapshot), betaVMSnapshot); err != nil {
		t.Fatalf("Failed to read VMSnapshot as v1beta1: %v", err)
	}

	// Verify basic fields were converted
	if betaVMSnapshot.Spec.VMRef.Name != "test-vm" {
		t.Errorf("Expected VM ref 'test-vm', got %s", betaVMSnapshot.Spec.VMRef.Name)
	}
	if betaVMSnapshot.Spec.SnapshotConfig == nil {
		t.Error("Expected SnapshotConfig to be set")
	} else if betaVMSnapshot.Spec.SnapshotConfig.Name != "snapshot-1" {
		t.Errorf("Expected snapshot name 'snapshot-1', got %s", betaVMSnapshot.Spec.SnapshotConfig.Name)
	}
}

func testVMCloneAlphaToBeta(t *testing.T, ctx context.Context, k8sClient client.Client) {
	// Create v1alpha1 VMClone
	alphaVMClone := &v1alpha1.VMClone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmclone-alpha-to-beta",
			Namespace: "default",
		},
		Spec: v1alpha1.VMCloneSpec{
			SourceRef: v1alpha1.LocalObjectReference{
				Name: "source-vm",
			},
			Target: v1alpha1.VMCloneTarget{
				Name: "cloned-vm",
			},
		},
	}

	// Create the alpha VMClone
	if err := k8sClient.Create(ctx, alphaVMClone); err != nil {
		t.Fatalf("Failed to create alpha VMClone: %v", err)
	}
	defer func() {
		if err := k8sClient.Delete(ctx, alphaVMClone); err != nil {
			t.Errorf("Failed to delete VMClone: %v", err)
		}
	}()

	// Wait a bit for the object to be processed
	time.Sleep(200 * time.Millisecond)

	// Read back as v1beta1
	betaVMClone := &v1beta1.VMClone{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(alphaVMClone), betaVMClone); err != nil {
		t.Fatalf("Failed to read VMClone as v1beta1: %v", err)
	}

	// Verify basic fields were converted
	if betaVMClone.Spec.Source.VMRef == nil {
		t.Error("Expected source VM ref to be set")
	} else if betaVMClone.Spec.Source.VMRef.Name != "source-vm" {
		t.Errorf("Expected source VM 'source-vm', got %s", betaVMClone.Spec.Source.VMRef.Name)
	}
	if betaVMClone.Spec.Target.Name != "cloned-vm" {
		t.Errorf("Expected target name 'cloned-vm', got %s", betaVMClone.Spec.Target.Name)
	}
}

func testVMSetAlphaToBeta(t *testing.T, ctx context.Context, k8sClient client.Client) {
	// Create v1alpha1 VMSet
	alphaVMSet := &v1alpha1.VMSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmset-alpha-to-beta",
			Namespace: "default",
		},
		Spec: v1alpha1.VMSetSpec{
			Replicas: func() *int32 { r := int32(2); return &r }(),
			Template: v1alpha1.VMSetTemplate{
				Spec: v1alpha1.VirtualMachineSpec{
					ProviderRef: v1alpha1.ObjectRef{Name: "test-provider"},
					ClassRef:    v1alpha1.ObjectRef{Name: "test-class"},
					ImageRef:    v1alpha1.ObjectRef{Name: "test-image"},
				},
			},
		},
	}

	// Create the alpha VMSet
	if err := k8sClient.Create(ctx, alphaVMSet); err != nil {
		t.Fatalf("Failed to create alpha VMSet: %v", err)
	}
	defer func() {
		if err := k8sClient.Delete(ctx, alphaVMSet); err != nil {
			t.Errorf("Failed to delete VMSet: %v", err)
		}
	}()

	// Wait a bit for the object to be processed
	time.Sleep(200 * time.Millisecond)

	// Read back as v1beta1
	betaVMSet := &v1beta1.VMSet{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(alphaVMSet), betaVMSet); err != nil {
		t.Fatalf("Failed to read VMSet as v1beta1: %v", err)
	}

	// Verify basic fields were converted
	if betaVMSet.Spec.Replicas == nil || *betaVMSet.Spec.Replicas != 2 {
		t.Error("Expected 2 replicas")
	}
	if betaVMSet.Spec.Template.Spec.ProviderRef.Name != "test-provider" {
		t.Errorf("Expected provider 'test-provider', got %s", betaVMSet.Spec.Template.Spec.ProviderRef.Name)
	}
}

func testVMPlacementPolicyAlphaToBeta(t *testing.T, ctx context.Context, k8sClient client.Client) {
	// Create v1alpha1 VMPlacementPolicy
	alphaVMPlacementPolicy := &v1alpha1.VMPlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vmplacementpolicy-alpha-to-beta",
			Namespace: "default",
		},
		Spec: v1alpha1.VMPlacementPolicySpec{
			Hard: &v1alpha1.PlacementConstraints{
				Clusters: []string{"cluster1"},
			},
		},
	}

	// Create the alpha VMPlacementPolicy
	if err := k8sClient.Create(ctx, alphaVMPlacementPolicy); err != nil {
		t.Fatalf("Failed to create alpha VMPlacementPolicy: %v", err)
	}
	defer func() {
		if err := k8sClient.Delete(ctx, alphaVMPlacementPolicy); err != nil {
			t.Errorf("Failed to delete VMPlacementPolicy: %v", err)
		}
	}()

	// Wait a bit for the object to be processed
	time.Sleep(200 * time.Millisecond)

	// Read back as v1beta1
	betaVMPlacementPolicy := &v1beta1.VMPlacementPolicy{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(alphaVMPlacementPolicy), betaVMPlacementPolicy); err != nil {
		t.Fatalf("Failed to read VMPlacementPolicy as v1beta1: %v", err)
	}

	// Verify basic fields were converted
	if betaVMPlacementPolicy.Spec.Hard == nil {
		t.Error("Expected hard constraints to be set")
	} else if len(betaVMPlacementPolicy.Spec.Hard.Clusters) != 1 || betaVMPlacementPolicy.Spec.Hard.Clusters[0] != "cluster1" {
		t.Error("Expected cluster1 in hard constraints")
	}
}
