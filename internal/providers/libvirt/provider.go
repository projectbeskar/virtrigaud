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
	"log/slog"
	"os"
	"strings"

	libvirt "libvirt.org/go/libvirt"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Provider implements the contracts.Provider interface for Libvirt/KVM
type Provider struct {
	// provider configuration
	config *v1beta1.Provider

	// Kubernetes client for reading secrets
	k8sClient client.Client

	// Libvirt connection
	conn *libvirt.Connect

	// cached credentials
	credentials *Credentials
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

	// Load credentials from mounted secret files
	if err := loadCredentialsFromFiles(config); err != nil {
		slog.Error("Failed to load credentials from mounted secret", "error", err)
	}

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

	if config.Endpoint != "" && config.Username != "" && config.Password != "" {
		slog.Info("Attempting to connect to libvirt with credentials")
		if err := p.connectWithConfig(context.Background(), config); err != nil {
			slog.Error("Failed to connect to libvirt", "error", err)
		}
	} else {
		slog.Warn("Libvirt provider configuration incomplete",
			"endpoint_set", config.Endpoint != "",
			"endpoint_value", config.Endpoint,
			"username_set", config.Username != "",
			"username_value", config.Username,
			"password_set", config.Password != "",
			"password_length", len(config.Password))
	}

	return p
}

// loadCredentialsFromFiles reads credentials from mounted secret files
func loadCredentialsFromFiles(config *Config) error {
	usernamePath := CredentialsPath + "/username"
	passwordPath := CredentialsPath + "/password"
	sshKeyPath := CredentialsPath + "/ssh-privatekey"

	slog.Info("Loading credentials from mounted secret files",
		"username_path", usernamePath,
		"password_path", passwordPath,
		"ssh_key_path", sshKeyPath)

	// Read username from mounted secret
	if data, err := os.ReadFile(usernamePath); err == nil {
		config.Username = strings.TrimSpace(string(data))
		slog.Info("Successfully loaded username",
			"username_length", len(config.Username),
			"username_value", config.Username)
	} else {
		slog.Error("Failed to read username file", "path", usernamePath, "error", err)
		return fmt.Errorf("failed to read username from %s: %w", usernamePath, err)
	}

	// Read password from mounted secret
	if data, err := os.ReadFile(passwordPath); err == nil {
		config.Password = strings.TrimSpace(string(data))
		slog.Info("Successfully loaded password",
			"password_length", len(config.Password))
	} else {
		slog.Error("Failed to read password file", "path", passwordPath, "error", err)
		return fmt.Errorf("failed to read password from %s: %w", passwordPath, err)
	}

	// Read SSH private key from mounted secret (optional)
	if data, err := os.ReadFile(sshKeyPath); err == nil {
		config.SSHPrivateKey = strings.TrimSpace(string(data))
		slog.Info("Successfully loaded SSH private key",
			"ssh_key_length", len(config.SSHPrivateKey))
	} else {
		slog.Info("SSH private key not found, will try username/password authentication",
			"path", sshKeyPath, "error", err)
		// SSH key is optional - don't return error
	}

	return nil
}

// connectWithConfig establishes a libvirt connection using the provided config
func (p *Provider) connectWithConfig(ctx context.Context, config *Config) error {
	// Use our existing SSH authentication logic
	p.config = &v1beta1.Provider{
		Spec: v1beta1.ProviderSpec{
			Endpoint: config.Endpoint,
		},
	}

	// Set credentials from config
	p.credentials = &Credentials{
		Username:      config.Username,
		Password:      config.Password,
		SSHPrivateKey: config.SSHPrivateKey,
	}

	return p.connect(ctx)
}

// NewProvider creates a new Libvirt provider instance (legacy K8s API method)
func NewProvider(ctx context.Context, k8sClient client.Client, provider *v1beta1.Provider) (contracts.Provider, error) {
	if string(provider.Spec.Type) != "libvirt" {
		return nil, contracts.NewInvalidSpecError(fmt.Sprintf("invalid provider type: %s, expected libvirt", string(provider.Spec.Type)), nil)
	}

	p := &Provider{
		config:    provider,
		k8sClient: k8sClient,
	}

	// Load credentials
	if err := p.loadCredentials(ctx); err != nil {
		return nil, contracts.NewUnauthorizedError("failed to load credentials", err)
	}

	// Initialize Libvirt connection
	if err := p.connect(ctx); err != nil {
		return nil, contracts.NewRetryableError("failed to connect to Libvirt", err)
	}

	return p, nil
}

// Validate ensures the provider connection is healthy
func (p *Provider) Validate(ctx context.Context) error {
	if p.conn == nil {
		return contracts.NewRetryableError("Libvirt connection not initialized", nil)
	}

	// Test the connection by checking if it's alive
	alive, err := p.conn.IsAlive()
	if err != nil || !alive {
		// Try to reconnect
		if err := p.connect(ctx); err != nil {
			return contracts.NewRetryableError("failed to validate Libvirt connection", err)
		}
	}

	return nil
}

// Create creates a new VM if it doesn't exist (idempotent)
func (p *Provider) Create(ctx context.Context, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	// Check if domain already exists
	domain, err := p.findDomain(req.Name)
	if err == nil && domain != nil {
		// Domain exists, return its ID
		defer domain.Free() //nolint:errcheck // Libvirt domain cleanup not critical in defer
		uuid, _ := domain.GetUUIDString()
		return contracts.CreateResponse{
			ID: uuid,
		}, nil
	}

	// Create the domain
	domainUUID, err := p.createDomain(ctx, req)
	if err != nil {
		return contracts.CreateResponse{}, err
	}

	return contracts.CreateResponse{
		ID: domainUUID,
	}, nil
}

// Delete removes a VM (idempotent, succeeds even if VM doesn't exist)
func (p *Provider) Delete(ctx context.Context, id string) (taskRef string, err error) {
	domain, err := p.findDomainByUUID(id)
	if err != nil {
		// Domain not found, consider it already deleted
		return "", nil
	}
	defer domain.Free() //nolint:errcheck // Libvirt domain cleanup not critical in defer

	// Check if domain is running
	active, err := domain.IsActive()
	if err != nil {
		return "", contracts.NewRetryableError("failed to check domain state", err)
	}

	if active {
		// Force shutdown the domain
		err = domain.Destroy()
		if err != nil {
			return "", contracts.NewRetryableError("failed to destroy domain", err)
		}
	}

	// Undefine (delete) the domain
	err = domain.Undefine()
	if err != nil {
		return "", contracts.NewRetryableError("failed to undefine domain", err)
	}

	// TODO: Clean up associated storage volumes
	return "", nil
}

// Power performs a power operation on the VM
func (p *Provider) Power(ctx context.Context, id string, op contracts.PowerOp) (taskRef string, err error) {
	domain, err := p.findDomainByUUID(id)
	if err != nil {
		return "", contracts.NewNotFoundError("domain not found", err)
	}
	defer domain.Free() //nolint:errcheck // Libvirt domain cleanup not critical in defer

	switch op {
	case contracts.PowerOpOn:
		err = domain.Create()
	case contracts.PowerOpOff:
		err = domain.Shutdown()
	case contracts.PowerOpReboot:
		err = domain.Reboot(libvirt.DOMAIN_REBOOT_DEFAULT)
	default:
		return "", contracts.NewInvalidSpecError(fmt.Sprintf("unsupported power operation: %s", op), nil)
	}

	if err != nil {
		return "", contracts.NewRetryableError(fmt.Sprintf("failed to perform power operation %s", op), err)
	}

	return "", nil
}

// Reconfigure modifies VM resources (CPU/RAM/Disks) - limited support in Libvirt
func (p *Provider) Reconfigure(ctx context.Context, id string, desired contracts.CreateRequest) (taskRef string, err error) {
	domain, err := p.findDomainByUUID(id)
	if err != nil {
		return "", contracts.NewNotFoundError("domain not found", err)
	}
	defer domain.Free() //nolint:errcheck // Libvirt domain cleanup not critical in defer

	// For basic reconfiguration, we'd need to modify the domain XML
	// This is more complex in Libvirt compared to vSphere
	// For MVP, we'll return not supported
	return "", contracts.NewNotSupportedError("reconfiguration not yet supported for Libvirt provider")
}

// Describe returns the current state of the VM
func (p *Provider) Describe(ctx context.Context, id string) (contracts.DescribeResponse, error) {
	domain, err := p.findDomainByUUID(id)
	if err != nil {
		return contracts.DescribeResponse{
			Exists: false,
		}, nil
	}
	defer domain.Free() //nolint:errcheck // Libvirt domain cleanup not critical in defer

	return p.describeDomain(domain)
}

// IsTaskComplete checks if an async task is complete
// Libvirt operations are generally synchronous, so tasks complete immediately
func (p *Provider) IsTaskComplete(ctx context.Context, taskRef string) (done bool, err error) {
	// Libvirt operations are typically synchronous
	// If we have a taskRef, it means the operation is already complete
	return true, nil
}
