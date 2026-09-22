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

// Package client provides a high-level gRPC client for provider services.
package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Config holds client configuration options.
type Config struct {
	// Address is the provider service address (host:port)
	Address string

	// TLS configuration
	TLS *TLSConfig

	// Timeout configuration
	Timeout *TimeoutConfig

	// Retry configuration
	Retry *RetryConfig

	// KeepAlive configuration
	KeepAlive *KeepAliveConfig
}

// TLSConfig holds TLS client configuration.
type TLSConfig struct {
	// Enabled enables TLS
	Enabled bool

	// InsecureSkipVerify skips certificate verification
	InsecureSkipVerify bool

	// ServerName for TLS verification
	ServerName string

	// CertFile for client certificate (mTLS)
	CertFile string

	// KeyFile for client private key (mTLS)
	KeyFile string

	// CAFile for server certificate verification
	CAFile string
}

// TimeoutConfig holds timeout configuration.
type TimeoutConfig struct {
	// DialTimeout for connection establishment
	DialTimeout time.Duration

	// CallTimeout for individual RPC calls
	CallTimeout time.Duration

	// PerMethodTimeouts maps method names to specific timeouts
	PerMethodTimeouts map[string]time.Duration
}

// RetryConfig holds retry configuration.
type RetryConfig struct {
	// Enabled enables automatic retries
	Enabled bool

	// MaxAttempts is the maximum number of retry attempts
	MaxAttempts int

	// InitialBackoff is the initial backoff duration
	InitialBackoff time.Duration

	// MaxBackoff is the maximum backoff duration
	MaxBackoff time.Duration

	// Multiplier for exponential backoff
	Multiplier float64

	// RetryableErrors lists errors that should be retried
	RetryableErrors []error
}

// KeepAliveConfig holds keep-alive configuration.
type KeepAliveConfig struct {
	// Time is the keep-alive interval
	Time time.Duration

	// Timeout is the keep-alive timeout
	Timeout time.Duration

	// PermitWithoutStream allows keep-alive without active streams
	PermitWithoutStream bool
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig(address string) *Config {
	return &Config{
		Address: address,
		TLS: &TLSConfig{
			Enabled: false,
		},
		Timeout: &TimeoutConfig{
			DialTimeout: 10 * time.Second,
			CallTimeout: 30 * time.Second,
		},
		Retry: &RetryConfig{
			Enabled:        true,
			MaxAttempts:    3,
			InitialBackoff: 100 * time.Millisecond,
			MaxBackoff:     5 * time.Second,
			Multiplier:     2.0,
		},
		KeepAlive: &KeepAliveConfig{
			Time:                30 * time.Second,
			Timeout:             5 * time.Second,
			PermitWithoutStream: true,
		},
	}
}

// Client is a high-level provider service client.
type Client struct {
	config *Config
	conn   *grpc.ClientConn
	client providerv1.ProviderClient
}

// New creates a new provider client.
func New(config *Config) (*Client, error) {
	if config == nil {
		return nil, fmt.Errorf("config is required")
	}
	if config.Address == "" {
		return nil, fmt.Errorf("address is required")
	}

	// Build dial options
	var opts []grpc.DialOption

	// Add credentials
	if config.TLS != nil && config.TLS.Enabled {
		creds, err := buildTLSCredentials(config.TLS)
		if err != nil {
			return nil, fmt.Errorf("failed to build TLS credentials: %w", err)
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Add keep-alive
	if config.KeepAlive != nil {
		opts = append(opts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                config.KeepAlive.Time,
			Timeout:             config.KeepAlive.Timeout,
			PermitWithoutStream: config.KeepAlive.PermitWithoutStream,
		}))
	}

	// Add dial timeout
	if config.Timeout != nil && config.Timeout.DialTimeout > 0 {
		// Note: DialTimeout is handled via context in Dial call
	}

	// Establish connection
	ctx := context.Background()
	if config.Timeout != nil && config.Timeout.DialTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, config.Timeout.DialTimeout)
		defer cancel()
	}

	conn, err := grpc.DialContext(ctx, config.Address, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to dial provider at %s: %w", config.Address, err)
	}

	client := providerv1.NewProviderClient(conn)

	return &Client{
		config: config,
		conn:   conn,
		client: client,
	}, nil
}

// Close closes the client connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Validate validates the provider configuration.
func (c *Client) Validate(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
	ctx, cancel := c.withTimeout(ctx, "/provider.v1.Provider/Validate")
	defer cancel()
	resp, err := c.client.Validate(ctx, req)
	return resp, errors.FromGRPCError(err)
}

// Create creates a new virtual machine.
func (c *Client) Create(ctx context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	ctx, cancel := c.withTimeout(ctx, "/provider.v1.Provider/Create")
	defer cancel()
	resp, err := c.client.Create(ctx, req)
	return resp, errors.FromGRPCError(err)
}

// Delete deletes a virtual machine.
func (c *Client) Delete(ctx context.Context, req *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	ctx, cancel := c.withTimeout(ctx, "/provider.v1.Provider/Delete")
	defer cancel()
	resp, err := c.client.Delete(ctx, req)
	return resp, errors.FromGRPCError(err)
}

// Power performs power operations on a virtual machine.
func (c *Client) Power(ctx context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	ctx, cancel := c.withTimeout(ctx, "/provider.v1.Provider/Power")
	defer cancel()
	resp, err := c.client.Power(ctx, req)
	return resp, errors.FromGRPCError(err)
}

// Reconfigure reconfigures a virtual machine.
func (c *Client) Reconfigure(ctx context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	ctx, cancel := c.withTimeout(ctx, "/provider.v1.Provider/Reconfigure")
	defer cancel()
	resp, err := c.client.Reconfigure(ctx, req)
	return resp, errors.FromGRPCError(err)
}

// Describe describes a virtual machine's current state.
func (c *Client) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	ctx, cancel := c.withTimeout(ctx, "/provider.v1.Provider/Describe")
	defer cancel()
	resp, err := c.client.Describe(ctx, req)
	return resp, errors.FromGRPCError(err)
}

// ListVMs lists all VMs managed by the provider.
func (c *Client) ListVMs(ctx context.Context) ([]*providerv1.VMInfo, error) {
	ctx, cancel := c.withTimeout(ctx, "/provider.v1.Provider/ListVMs")
	defer cancel()
	resp, err := c.client.ListVMs(ctx, &providerv1.ListVMsRequest{})
	if err != nil {
		return nil, errors.FromGRPCError(err)
	}
	return resp.Vms, nil
}

// ListHosts lists the hypervisor hosts a clustered provider fronts (ADR-0007
// P1). Non-clustered providers return a gRPC Unimplemented error.
func (c *Client) ListHosts(ctx context.Context) ([]*providerv1.HostInfo, error) {
	ctx, cancel := c.withTimeout(ctx, "/provider.v1.Provider/ListHosts")
	defer cancel()
	resp, err := c.client.ListHosts(ctx, &providerv1.ListHostsRequest{})
	if err != nil {
		return nil, errors.FromGRPCError(err)
	}
	return resp.Hosts, nil
}

// GetHostInfo refreshes a single host's inventory (ADR-0007 P1) — cheaper than a
// full ListHosts poll. Non-clustered providers return a gRPC Unimplemented error.
func (c *Client) GetHostInfo(ctx context.Context, hostID string) (*providerv1.HostInfo, error) {
	ctx, cancel := c.withTimeout(ctx, "/provider.v1.Provider/GetHostInfo")
	defer cancel()
	resp, err := c.client.GetHostInfo(ctx, &providerv1.GetHostInfoRequest{HostId: hostID})
	return resp, errors.FromGRPCError(err)
}

// TaskStatus checks the status of an async task.
func (c *Client) TaskStatus(ctx context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	ctx, cancel := c.withTimeout(ctx, "/provider.v1.Provider/TaskStatus")
	defer cancel()
	resp, err := c.client.TaskStatus(ctx, req)
	return resp, errors.FromGRPCError(err)
}

// SnapshotCreate creates a VM snapshot.
func (c *Client) SnapshotCreate(ctx context.Context, req *providerv1.SnapshotCreateRequest) (*providerv1.SnapshotCreateResponse, error) {
	ctx, cancel := c.withTimeout(ctx, "/provider.v1.Provider/SnapshotCreate")
	defer cancel()
	resp, err := c.client.SnapshotCreate(ctx, req)
	return resp, errors.FromGRPCError(err)
}

// SnapshotDelete deletes a VM snapshot.
func (c *Client) SnapshotDelete(ctx context.Context, req *providerv1.SnapshotDeleteRequest) (*providerv1.TaskResponse, error) {
	ctx, cancel := c.withTimeout(ctx, "/provider.v1.Provider/SnapshotDelete")
	defer cancel()
	resp, err := c.client.SnapshotDelete(ctx, req)
	return resp, errors.FromGRPCError(err)
}

// SnapshotRevert reverts a VM to a snapshot.
func (c *Client) SnapshotRevert(ctx context.Context, req *providerv1.SnapshotRevertRequest) (*providerv1.TaskResponse, error) {
	ctx, cancel := c.withTimeout(ctx, "/provider.v1.Provider/SnapshotRevert")
	defer cancel()
	resp, err := c.client.SnapshotRevert(ctx, req)
	return resp, errors.FromGRPCError(err)
}

// Clone clones a virtual machine.
func (c *Client) Clone(ctx context.Context, req *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	ctx, cancel := c.withTimeout(ctx, "/provider.v1.Provider/Clone")
	defer cancel()
	resp, err := c.client.Clone(ctx, req)
	return resp, errors.FromGRPCError(err)
}

// ImagePrepare prepares an image for use. The response reports the prepared
// image's provider-specific location (id/path) so callers can create VMs from
// the prepared template instead of re-resolving the source (issue #154, PR-6 /
// #214).
func (c *Client) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error) {
	ctx, cancel := c.withTimeout(ctx, "/provider.v1.Provider/ImagePrepare")
	defer cancel()
	resp, err := c.client.ImagePrepare(ctx, req)
	return resp, errors.FromGRPCError(err)
}

// GetCapabilities gets the provider's capabilities.
func (c *Client) GetCapabilities(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	ctx, cancel := c.withTimeout(ctx, "/provider.v1.Provider/GetCapabilities")
	defer cancel()
	resp, err := c.client.GetCapabilities(ctx, req)
	return resp, errors.FromGRPCError(err)
}

// withTimeout returns a context bound to the timeout configured for method,
// along with the context.CancelFunc that releases the timer backing it.
// Callers must always invoke the returned cancel func — typically via
// `defer cancel()` immediately after this call returns — even when no
// timeout ends up being applied: in that case the returned func is a no-op,
// so the call pattern stays uniform across every RPC method regardless of
// whether Config.Timeout is set.
func (c *Client) withTimeout(ctx context.Context, method string) (context.Context, context.CancelFunc) {
	if c.config.Timeout == nil {
		return ctx, func() {}
	}

	// Check for method-specific timeout
	if timeout, ok := c.config.Timeout.PerMethodTimeouts[method]; ok {
		return context.WithTimeout(ctx, timeout)
	}

	// Use default timeout
	if c.config.Timeout.CallTimeout > 0 {
		return context.WithTimeout(ctx, c.config.Timeout.CallTimeout)
	}

	return ctx, func() {}
}

// buildTLSCredentials creates TLS credentials from the given config.
func buildTLSCredentials(config *TLSConfig) (credentials.TransportCredentials, error) {
	tlsConfig := &tls.Config{
		ServerName:         config.ServerName,
		InsecureSkipVerify: config.InsecureSkipVerify,
	}

	// Load client certificate for mTLS
	if config.CertFile != "" && config.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	// TODO: Load CA certificate for server verification
	if config.CAFile != "" {
		// Load CA cert
	}

	return credentials.NewTLS(tlsConfig), nil
}

// WaitForTask waits for a task to complete, polling for status.
func (c *Client) WaitForTask(ctx context.Context, taskRef *providerv1.TaskRef, pollInterval time.Duration) error {
	if taskRef == nil || taskRef.Id == "" {
		return nil // No task to wait for
	}

	if pollInterval == 0 {
		pollInterval = 2 * time.Second
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			resp, err := c.TaskStatus(ctx, &providerv1.TaskStatusRequest{
				Task: taskRef,
			})
			if err != nil {
				return fmt.Errorf("failed to check task status: %w", err)
			}

			if resp.Done {
				if resp.Error != "" {
					return fmt.Errorf("task failed: %s", resp.Error)
				}
				return nil // Task completed successfully
			}
			// Continue polling
		}
	}
}
