# Provider SDK Reference

_Auto-generated from SDK packages in `sdk/`_

The VirtRigaud Provider SDK helps you build custom providers that integrate
with the VirtRigaud operator.

## Installation

```bash
go get github.com/projectbeskar/virtrigaud/sdk/provider
```

---

## SDK Packages

### Server Package

```go
package server // import "."

Package server provides gRPC server bootstrapping utilities for provider
implementations.

TYPES

type Config struct {
	// Port is the gRPC server port (default: 9443)
	Port int

	// HealthPort is the health check server port (default: 8080)
	HealthPort int

	// TLS configuration
	TLS *TLSConfig

	// Logger instance (uses slog.Default() if nil)
	Logger *slog.Logger

	// Middleware configuration
	Middleware *middleware.Config

	// KeepAlive settings
	KeepAlive *KeepAliveConfig

	// GracefulTimeout for shutdown (default: 30s)
	GracefulTimeout time.Duration

	// ServiceName for health checks (default: "provider")
	ServiceName string
}
    Config holds server configuration options.

func DefaultConfig() *Config
    DefaultConfig returns a Config with sensible defaults.

type KeepAliveConfig struct {
	// ServerParameters for server-side keep-alive
	ServerParameters *keepalive.ServerParameters

	// EnforcementPolicy for keep-alive enforcement
	EnforcementPolicy *keepalive.EnforcementPolicy
}
    KeepAliveConfig holds keep-alive settings.

type Server struct {
	// Has unexported fields.
}
    Server wraps a gRPC server with provider-specific functionality.

func New(config *Config) (*Server, error)
    New creates a new provider server with the given configuration.

func (s *Server) RegisterProvider(service interface{})
    RegisterProvider is a convenience method to register a provider service.

func (s *Server) RegisterService(desc *grpc.ServiceDesc, impl interface{})
    RegisterService registers a provider service implementation.

func (s *Server) Serve(ctx context.Context) error
    Serve starts the gRPC server and blocks until shutdown.

func (s *Server) Shutdown() error
    Shutdown gracefully stops the server.

type TLSConfig struct {
	// CertFile path to TLS certificate
	CertFile string

	// KeyFile path to TLS private key
	KeyFile string

	// CAFile path to CA certificate for mTLS (optional)
	CAFile string

	// RequireClientCert enables mTLS client authentication
	RequireClientCert bool

	// AutoReload enables automatic certificate reloading
	AutoReload bool
}
    TLSConfig holds TLS configuration.
```

### Client Package

```go
package client // import "."

Package client provides a high-level gRPC client for provider services.

TYPES

type Client struct {
	// Has unexported fields.
}
    Client is a high-level provider service client.

func New(config *Config) (*Client, error)
    New creates a new provider client.

func (c *Client) Clone(ctx context.Context, req *providerv1.CloneRequest) (*providerv1.CloneResponse, error)
    Clone clones a virtual machine.

func (c *Client) Close() error
    Close closes the client connection.

func (c *Client) Create(ctx context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error)
    Create creates a new virtual machine.

func (c *Client) Delete(ctx context.Context, req *providerv1.DeleteRequest) (*providerv1.TaskResponse, error)
    Delete deletes a virtual machine.

func (c *Client) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error)
    Describe describes a virtual machine's current state.

func (c *Client) GetCapabilities(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error)
    GetCapabilities gets the provider's capabilities.

func (c *Client) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.TaskResponse, error)
    ImagePrepare prepares an image for use.

func (c *Client) Power(ctx context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error)
    Power performs power operations on a virtual machine.

func (c *Client) Reconfigure(ctx context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error)
    Reconfigure reconfigures a virtual machine.

func (c *Client) SnapshotCreate(ctx context.Context, req *providerv1.SnapshotCreateRequest) (*providerv1.SnapshotCreateResponse, error)
    SnapshotCreate creates a VM snapshot.

func (c *Client) SnapshotDelete(ctx context.Context, req *providerv1.SnapshotDeleteRequest) (*providerv1.TaskResponse, error)
    SnapshotDelete deletes a VM snapshot.

func (c *Client) SnapshotRevert(ctx context.Context, req *providerv1.SnapshotRevertRequest) (*providerv1.TaskResponse, error)
    SnapshotRevert reverts a VM to a snapshot.

func (c *Client) TaskStatus(ctx context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error)
    TaskStatus checks the status of an async task.

func (c *Client) Validate(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error)
    Validate validates the provider configuration.

func (c *Client) WaitForTask(ctx context.Context, taskRef *providerv1.TaskRef, pollInterval time.Duration) error
    WaitForTask waits for a task to complete, polling for status.

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
    Config holds client configuration options.

func DefaultConfig(address string) *Config
    DefaultConfig returns a Config with sensible defaults.

type KeepAliveConfig struct {
	// Time is the keep-alive interval
	Time time.Duration

	// Timeout is the keep-alive timeout
	Timeout time.Duration

	// PermitWithoutStream allows keep-alive without active streams
	PermitWithoutStream bool
}
    KeepAliveConfig holds keep-alive configuration.

type RetryConfig struct {
	// Enabled enables automatic retries
	Enabled bool

	// MaxAttempts is the maximum number of retry attempts
	MaxAttempts int

	// InitialBackoff is the initial backoff duration
```


---

_Generated on: 2025-12-02 01:05:57 UTC_
