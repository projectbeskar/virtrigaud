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
	"io/ioutil"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/capabilities"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

const (
	// CredentialsPath is where the controller mounts the credentials secret
	CredentialsPath = "/etc/virtrigaud/credentials"
)

// Provider implements the vSphere provider using the SDK pattern
type Provider struct {
	providerv1.UnimplementedProviderServer
	client       *govmomi.Client
	finder       *find.Finder
	capabilities *capabilities.Manager
	logger       *slog.Logger
	config       *Config
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
	// Get capabilities for vSphere
	caps := GetProviderCapabilities()

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
		config:       config,
		client:       client,
		capabilities: caps,
		logger:       slog.Default(),
	}
}

// loadCredentialsFromFiles reads credentials from mounted secret files
func loadCredentialsFromFiles(config *Config) error {
	// Read username from mounted secret
	if data, err := ioutil.ReadFile(CredentialsPath + "/username"); err == nil {
		config.Username = strings.TrimSpace(string(data))
	} else {
		return fmt.Errorf("failed to read username from %s/username: %w", CredentialsPath, err)
	}

	// Read password from mounted secret
	if data, err := ioutil.ReadFile(CredentialsPath + "/password"); err == nil {
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
		Capabilities: p.capabilities.Proto(),
	}, nil
}

// Create creates a new virtual machine
func (p *Provider) Create(ctx context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	return nil, errors.NewUnimplemented("Create operation not yet implemented for vSphere")
}

// Delete deletes a virtual machine
func (p *Provider) Delete(ctx context.Context, req *providerv1.DeleteRequest) (*providerv1.DeleteResponse, error) {
	return nil, errors.NewUnimplemented("Delete operation not yet implemented for vSphere")
}

// Get retrieves virtual machine information
func (p *Provider) Get(ctx context.Context, req *providerv1.GetRequest) (*providerv1.GetResponse, error) {
	return nil, errors.NewUnimplemented("Get operation not yet implemented for vSphere")
}

// List lists virtual machines
func (p *Provider) List(ctx context.Context, req *providerv1.ListRequest) (*providerv1.ListResponse, error) {
	return nil, errors.NewUnimplemented("List operation not yet implemented for vSphere")
}

// Update updates a virtual machine
func (p *Provider) Update(ctx context.Context, req *providerv1.UpdateRequest) (*providerv1.UpdateResponse, error) {
	return nil, errors.NewUnimplemented("Update operation not yet implemented for vSphere")
}

// Start starts a virtual machine
func (p *Provider) Start(ctx context.Context, req *providerv1.StartRequest) (*providerv1.StartResponse, error) {
	return nil, errors.NewUnimplemented("Start operation not yet implemented for vSphere")
}

// Stop stops a virtual machine
func (p *Provider) Stop(ctx context.Context, req *providerv1.StopRequest) (*providerv1.StopResponse, error) {
	return nil, errors.NewUnimplemented("Stop operation not yet implemented for vSphere")
}

// Restart restarts a virtual machine
func (p *Provider) Restart(ctx context.Context, req *providerv1.RestartRequest) (*providerv1.RestartResponse, error) {
	return nil, errors.NewUnimplemented("Restart operation not yet implemented for vSphere")
}

// GetCapabilities returns the vSphere provider capabilities
func GetProviderCapabilities() *capabilities.Manager {
	return capabilities.NewBuilder().
		Core().
		VSphere().
		DiskTypes("thin", "thick", "eager-zeroed").
		NetworkTypes("standard", "distributed").
		Build()
}