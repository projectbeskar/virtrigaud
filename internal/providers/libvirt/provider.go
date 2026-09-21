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
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// Provider implements the contracts.Provider interface for Libvirt/KVM via virsh
type Provider struct {
	// provider configuration
	config *v1beta1.Provider

	// Kubernetes client for reading secrets
	k8sClient client.Client

	// Virsh-based provider (replaces libvirt-go). Retained as the handle the
	// package's own ~138 runVirshCommand call sites operate on; the gRPC Server
	// reaches it through the registry seam instead (ADR-0008 PR 2).
	virshProvider *VirshProvider

	// registry owns the per-host connection(s). Today it holds exactly one Conn
	// (built from PROVIDER_ENDPOINT, wrapping virshProvider); ADR-0007 P1 changes
	// only the constructor to project N hosts from a mounted Secret.
	registry hostconn.Registry

	// hostID identifies the single host in the registry. Under ADR-0007 it equals
	// the Host CR name.
	hostID hostconn.HostID

	// cached credentials
	credentials *Credentials
}

// ProviderConfig represents the configuration for the provider
type ProviderConfig struct {
	Spec      ProviderSpec
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
	Endpoint      string
	Username      string
	Password      string
	SSHPrivateKey string
}

// New creates a new Libvirt provider that reads configuration from environment
// and mounted secrets.
//
// It fails closed: if the virsh provider cannot initialize (bad/absent
// credentials, unreachable host), New returns an error instead of a Provider so
// the process never begins serving gRPC on a dead connection. This fixes the
// prior behaviour where an Initialize failure was logged and swallowed
// (provider.go:141), matching NewProvider and PR #291 finding B2. The caller
// (cmd/provider-libvirt/main.go) exits non-zero on error, so the pod restarts
// until the host is reachable rather than reporting healthy while every RPC
// fails.
func New() (*Provider, error) {
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

	// Build the per-host connection seam (ADR-0008 PR 2). One host today, keyed
	// off PROVIDER_ENDPOINT; ADR-0007 P1 changes only this to project N hosts.
	hostID := hostIDFromEndpoint(config.Endpoint)
	registry, err := hostconn.NewRegistry(newVirshConn(hostID, virshProvider))
	if err != nil {
		return nil, fmt.Errorf("build libvirt host registry: %w", err)
	}
	p.registry = registry
	p.hostID = hostID

	// Initialize the virsh provider — fail closed (B2). Previously this logged
	// and continued, so the process could serve gRPC on a dead connection.
	ctx := context.Background()
	if err := virshProvider.Initialize(ctx); err != nil {
		return nil, contracts.NewRetryableError("failed to initialize virsh provider", err)
	}

	log.Printf("INFO Successfully initialized virsh provider")
	return p, nil
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

	// Build the per-host connection seam (ADR-0008 PR 2), keyed off the
	// provider's endpoint, so this path is consistent with New().
	hostID := hostIDFromEndpoint(provider.Spec.Endpoint)
	registry, err := hostconn.NewRegistry(newVirshConn(hostID, virshProvider))
	if err != nil {
		return nil, contracts.NewRetryableError("build libvirt host registry", err)
	}
	p.registry = registry
	p.hostID = hostID

	// Initialize the virsh provider
	if err := virshProvider.Initialize(ctx); err != nil {
		return nil, contracts.NewRetryableError("failed to initialize virsh provider", err)
	}

	log.Printf("INFO Successfully created virsh-based provider via K8s API")
	return p, nil
}

// conn returns the libvirt connection for the provider's single host, obtained
// from the registry seam (ADR-0008 PR 2) rather than the directly-held
// virshProvider field. The gRPC Server uses this for the snapshot / import / disk
// RPCs that previously type-asserted the concrete *Provider.
//
// It resolves the Conn through the Registry on every call (never caches it, per
// the ADR's connection-lifecycle rule) and narrows the transport-neutral
// hostconn.Conn to the richer libvirtConn view those RPCs need. A future
// non-virsh transport that does not provide the virsh helpers fails here cleanly
// rather than silently.
func (p *Provider) conn(ctx context.Context) (libvirtConn, error) {
	if p.registry == nil {
		return nil, contracts.NewRetryableError("libvirt provider not initialized", nil)
	}
	c, err := p.registry.ConnFor(ctx, p.hostID)
	if err != nil {
		return nil, fmt.Errorf("get libvirt host connection %q: %w", p.hostID, err)
	}
	lc, ok := c.(libvirtConn)
	if !ok {
		return nil, fmt.Errorf("libvirt host connection %q does not support virsh operations", p.hostID)
	}
	return lc, nil
}

// Compile-time proof that *Provider satisfies the backend interface the gRPC
// Server holds (server.go), so no concrete type assertion is needed there.
var _ providerBackend = (*Provider)(nil)

// Validate ensures the provider connection is healthy using virsh
func (p *Provider) Validate(ctx context.Context) error {
	if p.virshProvider == nil {
		return contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Validate must stay lightweight: controllers call it before most provider
	// operations. Do not use listDomains here; it runs domstate for every domain
	// and can exceed the manager-side gRPC deadline on busy libvirt hosts.
	result, err := p.virshProvider.runVirshCommand(ctx, "list", "--all", "--name")
	if err != nil {
		return contracts.NewRetryableError("virsh connection validation failed", err)
	}

	domainCount := 0
	for _, line := range strings.Split(result.Stdout, "\n") {
		if strings.TrimSpace(line) != "" {
			domainCount++
		}
	}

	log.Printf("INFO Connection validation successful - found %d domains", domainCount)
	return nil
}

// Contract methods are now implemented in provider_virsh.go using virsh commands
