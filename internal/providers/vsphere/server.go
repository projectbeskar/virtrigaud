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

package vsphere

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

const (
	// CredentialsPath is where the controller mounts the credentials secret
	CredentialsPath = "/etc/virtrigaud/credentials"
)

// Provider implements the vSphere provider using the SDK pattern
type Provider struct {
	providerv1.UnimplementedProviderServer
	client *govmomi.Client
	logger *slog.Logger
	config *Config
}

// Config holds the vSphere provider configuration
type Config struct {
	Endpoint           string
	Username           string
	Password           string
	InsecureSkipVerify bool
}

// New creates a new vSphere provider that reads configuration from environment and mounted secrets
func New() *Provider {
	// Load configuration from environment (set by provider controller)
	config := &Config{
		Endpoint:           os.Getenv("PROVIDER_ENDPOINT"),
		InsecureSkipVerify: false, // Default to secure
	}

	// Load credentials from mounted secret files
	if err := loadCredentialsFromFiles(config); err != nil {
		slog.Error("Failed to load credentials from mounted secret", "error", err)
	}

	// Create vSphere client
	client, err := createVSphereClient(config)
	if err != nil {
		// Log error but continue - validation will catch connection issues
		slog.Error("Failed to create vSphere client", "error", err)
	}

	return &Provider{
		config: config,
		client: client,
		logger: slog.Default(),
	}
}

// loadCredentialsFromFiles reads credentials from mounted secret files
func loadCredentialsFromFiles(config *Config) error {
	// Read username from mounted secret
	if data, err := os.ReadFile(CredentialsPath + "/username"); err == nil {
		config.Username = strings.TrimSpace(string(data))
	} else {
		return fmt.Errorf("failed to read username from %s/username: %w", CredentialsPath, err)
	}

	// Read password from mounted secret
	if data, err := os.ReadFile(CredentialsPath + "/password"); err == nil {
		config.Password = strings.TrimSpace(string(data))
	} else {
		return fmt.Errorf("failed to read password from %s/password: %w", CredentialsPath, err)
	}

	return nil
}

// createVSphereClient creates a govmomi client from the configuration
func createVSphereClient(config *Config) (*govmomi.Client, error) {
	if config.Endpoint == "" {
		return nil, fmt.Errorf("PROVIDER_ENDPOINT environment variable is required")
	}

	if config.Username == "" || config.Password == "" {
		return nil, fmt.Errorf("username and password are required in mounted credentials secret")
	}

	// Parse the endpoint URL
	u, err := url.Parse(config.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid vSphere endpoint URL: %w", err)
	}

	// Set up authentication
	u.User = url.UserPassword(config.Username, config.Password)

	// Create SOAP client
	soapClient := soap.NewClient(u, config.InsecureSkipVerify)

	// Configure TLS if needed
	if !config.InsecureSkipVerify {
		soapClient.DefaultTransport().TLSClientConfig = &tls.Config{
			ServerName: u.Hostname(),
		}
	}

	// Create vSphere client
	vimClient, err := vim25.NewClient(context.Background(), soapClient)
	if err != nil {
		return nil, fmt.Errorf("failed to create vSphere VIM client: %w", err)
	}

	client := &govmomi.Client{
		Client:         vimClient,
		SessionManager: session.NewManager(vimClient),
	}

	// Login to vSphere
	if err := client.Login(context.Background(), nil); err != nil {
		return nil, fmt.Errorf("failed to login to vSphere: %w", err)
	}

	return client, nil
}

// Validate validates the provider configuration and connectivity
func (p *Provider) Validate(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
	if p.client == nil {
		return &providerv1.ValidateResponse{
			Ok:      false,
			Message: "vSphere client not configured",
		}, nil
	}

	// Test the connection by checking if the session is valid
	if !p.client.Valid() {
		// Try to reconnect
		client, err := createVSphereClient(p.config)
		if err != nil {
			return &providerv1.ValidateResponse{
				Ok:      false,
				Message: fmt.Sprintf("Failed to connect to vSphere: %v", err),
			}, nil
		}
		p.client = client
	}

	return &providerv1.ValidateResponse{
		Ok:      true,
		Message: "vSphere provider is ready",
	}, nil
}

// GetCapabilities returns the provider's capabilities
func (p *Provider) GetCapabilities(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return &providerv1.GetCapabilitiesResponse{
		SupportsReconfigureOnline:   true,
		SupportsDiskExpansionOnline: true,
		SupportsSnapshots:           true,
		SupportsMemorySnapshots:     false, // vSphere snapshots don't include memory by default
		SupportsLinkedClones:        true,
		SupportsImageImport:         true,
		SupportedDiskTypes:          []string{"thin", "thick", "eager-zeroed"},
		SupportedNetworkTypes:       []string{"standard", "distributed"},
	}, nil
}

// Create creates a new virtual machine
func (p *Provider) Create(ctx context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	return nil, errors.NewUnimplemented("Create operation not yet implemented for vSphere")
}

// Delete deletes a virtual machine
func (p *Provider) Delete(ctx context.Context, req *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	return nil, errors.NewUnimplemented("Delete operation not yet implemented for vSphere")
}

// Power performs power operations on a virtual machine
func (p *Provider) Power(ctx context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	return nil, errors.NewUnimplemented("Power operation not yet implemented for vSphere")
}

// Reconfigure reconfigures a virtual machine
func (p *Provider) Reconfigure(ctx context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	return nil, errors.NewUnimplemented("Reconfigure operation not yet implemented for vSphere")
}

// Describe retrieves virtual machine information
func (p *Provider) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	return nil, errors.NewUnimplemented("Describe operation not yet implemented for vSphere")
}

// TaskStatus checks the status of an async task
func (p *Provider) TaskStatus(ctx context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	return nil, errors.NewUnimplemented("TaskStatus operation not yet implemented for vSphere")
}

// SnapshotCreate creates a snapshot of a virtual machine
func (p *Provider) SnapshotCreate(ctx context.Context, req *providerv1.SnapshotCreateRequest) (*providerv1.SnapshotCreateResponse, error) {
	return nil, errors.NewUnimplemented("SnapshotCreate operation not yet implemented for vSphere")
}

// SnapshotDelete deletes a snapshot
func (p *Provider) SnapshotDelete(ctx context.Context, req *providerv1.SnapshotDeleteRequest) (*providerv1.TaskResponse, error) {
	return nil, errors.NewUnimplemented("SnapshotDelete operation not yet implemented for vSphere")
}

// SnapshotRevert reverts to a snapshot
func (p *Provider) SnapshotRevert(ctx context.Context, req *providerv1.SnapshotRevertRequest) (*providerv1.TaskResponse, error) {
	return nil, errors.NewUnimplemented("SnapshotRevert operation not yet implemented for vSphere")
}

// Clone clones a virtual machine
func (p *Provider) Clone(ctx context.Context, req *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	return nil, errors.NewUnimplemented("Clone operation not yet implemented for vSphere")
}

// ImagePrepare prepares an image/template
func (p *Provider) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.TaskResponse, error) {
	return nil, errors.NewUnimplemented("ImagePrepare operation not yet implemented for vSphere")
}
