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
