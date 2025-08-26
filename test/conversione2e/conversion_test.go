package conversione2e

import (
	"context"
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

func TestConversionVirtualMachine(t *testing.T) {
	ctx := context.Background()

	// Set up envtest environment
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("Failed to start test environment: %v", err)
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

	t.Run("AlphaToBetaConversion", func(t *testing.T) {
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
			},
		}

		// Create the alpha VM
		if err := k8sClient.Create(ctx, alphaVM); err != nil {
			t.Fatalf("Failed to create alpha VM: %v", err)
		}

		// Wait a bit for the object to be processed
		time.Sleep(100 * time.Millisecond)

		// Read back as v1beta1
		betaVM := &v1beta1.VirtualMachine{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(alphaVM), betaVM); err != nil {
			t.Fatalf("Failed to read VM as v1beta1: %v", err)
		}

		// Verify basic fields were converted
		if betaVM.Spec.ProviderRef.Name != "test-provider" {
			t.Errorf("Expected ProviderRef.Name to be 'test-provider', got %s", betaVM.Spec.ProviderRef.Name)
		}
		if betaVM.Spec.PowerState != v1beta1.PowerStateOn {
			t.Errorf("Expected PowerState to be 'On', got %s", betaVM.Spec.PowerState)
		}

		// Clean up
		if err := k8sClient.Delete(ctx, alphaVM); err != nil {
			t.Errorf("Failed to delete VM: %v", err)
		}
	})

	t.Run("BetaToAlphaConversion", func(t *testing.T) {
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
			},
		}

		// Create the beta VM
		if err := k8sClient.Create(ctx, betaVM); err != nil {
			t.Fatalf("Failed to create beta VM: %v", err)
		}

		// Wait a bit for the object to be processed
		time.Sleep(100 * time.Millisecond)

		// Read back as v1alpha1
		alphaVM := &v1alpha1.VirtualMachine{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(betaVM), alphaVM); err != nil {
			t.Fatalf("Failed to read VM as v1alpha1: %v", err)
		}

		// Verify basic fields were converted
		if alphaVM.Spec.ProviderRef.Name != "test-provider" {
			t.Errorf("Expected ProviderRef.Name to be 'test-provider', got %s", alphaVM.Spec.ProviderRef.Name)
		}
		if alphaVM.Spec.PowerState != "On" {
			t.Errorf("Expected PowerState to be 'On', got %s", alphaVM.Spec.PowerState)
		}

		// Clean up
		if err := k8sClient.Delete(ctx, betaVM); err != nil {
			t.Errorf("Failed to delete VM: %v", err)
		}
	})
}

func TestConversionProvider(t *testing.T) {
	ctx := context.Background()

	// Set up envtest environment
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("Failed to start test environment: %v", err)
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

	t.Run("ProviderAlphaToBetaConversion", func(t *testing.T) {
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

		// Wait a bit for the object to be processed
		time.Sleep(100 * time.Millisecond)

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

		// Clean up
		if err := k8sClient.Delete(ctx, alphaProvider); err != nil {
			t.Errorf("Failed to delete Provider: %v", err)
		}
	})
}
