/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package libvirt

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Provider implements the contracts.Provider interface for Libvirt/KVM via virsh
type Provider struct {
	// provider configuration
	config *v1beta1.Provider

	// Kubernetes client for reading secrets
	k8sClient client.Client

	// Virsh-based provider (replaces libvirt-go)
	virshProvider *VirshProvider

	// cached credentials
	credentials *Credentials
}

// ProviderConfig represents the configuration for the provider
type ProviderConfig struct {
	Spec ProviderSpec
	Namespace string
}

// ProviderSpec represents the spec of the provider configuration
type ProviderSpec struct {
	Endpoint            string
	CredentialSecretRef CredentialSecretRef
}

// CredentialSecretRef represents a reference to a credential secret
type CredentialSecretRef struct {
	Name      string
	Namespace string
}

// Credentials holds Libvirt authentication information
type Credentials struct {
	Username string
	Password string
	// SSH key authentication
	SSHPrivateKey string
	SSHPublicKey  string
	// For TLS connections
	CertData string
	KeyData  string
	CAData   string
}

const (
	// CredentialsPath is where the controller mounts the credentials secret
	CredentialsPath = "/etc/virtrigaud/credentials"
)

// Config holds the libvirt provider configuration
type Config struct {
	Endpoint       string
	Username       string
	Password       string
	SSHPrivateKey  string
}

// New creates a new Libvirt provider that reads configuration from environment and mounted secrets
func New() *Provider {
	// Load configuration from environment (set by provider controller)
	config := &Config{
		Endpoint: os.Getenv("PROVIDER_ENDPOINT"),
	}

	// Credentials are now loaded by virsh provider from environment variables

	p := &Provider{
		config:    nil, // We'll create a minimal config
		k8sClient: nil, // No K8s client needed in container mode
		credentials: &Credentials{
			Username: config.Username,
			Password: config.Password,
		},
	}

	// Try to establish libvirt connection
	slog.Info("Libvirt provider configuration loaded",
		"endpoint", config.Endpoint,
		"username", config.Username,
		"password_length", len(config.Password))

	// Create provider configuration
	providerConfig := &ProviderConfig{
		Spec: ProviderSpec{
			Endpoint: config.Endpoint,
			CredentialSecretRef: CredentialSecretRef{
				Name:      "libvirt-credentials", // Default name
				Namespace: "default",
			},
		},
		Namespace: "default",
	}
	
	// Create virsh provider to replace libvirt-go
	virshProvider := NewVirshProvider(providerConfig)
	p.virshProvider = virshProvider
	
	// Create minimal v1beta1.Provider config for compatibility
	p.config = &v1beta1.Provider{
		Spec: v1beta1.ProviderSpec{
			Endpoint: config.Endpoint,
		},
	}
	
	// Initialize the virsh provider
	ctx := context.Background()
	if err := virshProvider.Initialize(ctx); err != nil {
		log.Printf("ERROR Failed to initialize virsh provider: %v", err)
	} else {
		log.Printf("INFO Successfully initialized virsh provider")
	}

	return p
}

// Removed old file-based credential loading - now using environment variables via virsh provider

// Removed old libvirt-go connection logic - now using virsh provider

// NewProvider creates a new Libvirt provider instance (legacy K8s API method)
func NewProvider(ctx context.Context, k8sClient client.Client, provider *v1beta1.Provider) (contracts.Provider, error) {
	if string(provider.Spec.Type) != "libvirt" {
		return nil, contracts.NewInvalidSpecError(fmt.Sprintf("invalid provider type: %s, expected libvirt", string(provider.Spec.Type)), nil)
	}

	log.Printf("INFO Creating virsh-based provider from K8s API")

	// Create provider configuration for virsh
	providerConfig := &ProviderConfig{
		Spec: ProviderSpec{
			Endpoint: provider.Spec.Endpoint,
			CredentialSecretRef: CredentialSecretRef{
				Name:      provider.Spec.CredentialSecretRef.Name,
				Namespace: provider.Spec.CredentialSecretRef.Namespace,
			},
		},
		Namespace: provider.Namespace,
	}

	// Create virsh provider
	virshProvider := NewVirshProvider(providerConfig)

	p := &Provider{
		config:        provider,
		k8sClient:     k8sClient,
		virshProvider: virshProvider,
		credentials:   &Credentials{},
	}

	// Initialize the virsh provider
	if err := virshProvider.Initialize(ctx); err != nil {
		return nil, contracts.NewRetryableError("failed to initialize virsh provider", err)
	}

	log.Printf("INFO Successfully created virsh-based provider via K8s API")
	return p, nil
}

// Validate ensures the provider connection is healthy using virsh
func (p *Provider) Validate(ctx context.Context) error {
	if p.virshProvider == nil {
		return contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Test the connection by listing domains
	domains, err := p.virshProvider.listDomains(ctx)
	if err != nil {
		return contracts.NewRetryableError("virsh connection validation failed", err)
	}

	log.Printf("INFO Connection validation successful - found %d domains", len(domains))
	return nil
}

// Contract methods are now implemented in provider_virsh.go using virsh commands
