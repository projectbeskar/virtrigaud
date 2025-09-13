# Provider Development Guide

This document explains how to implement a new provider for VirtRigaud.

## Overview

Providers are responsible for implementing VM lifecycle operations on specific hypervisor platforms. VirtRigaud uses a Remote Provider architecture where each provider runs as an independent gRPC service, communicating with the manager controller.

## Provider Interface

All providers must implement the `contracts.Provider` interface:

```go
type Provider interface {
    // Validate ensures the provider session/credentials are healthy
    Validate(ctx context.Context) error

    // Create creates a new VM if it doesn't exist (idempotent)
    Create(ctx context.Context, req CreateRequest) (CreateResponse, error)

    // Delete removes a VM (idempotent)
    Delete(ctx context.Context, id string) (taskRef string, err error)

    // Power performs a power operation on the VM
    Power(ctx context.Context, id string, op PowerOp) (taskRef string, err error)

    // Reconfigure modifies VM resources
    Reconfigure(ctx context.Context, id string, desired CreateRequest) (taskRef string, err error)

    // Describe returns the current state of the VM
    Describe(ctx context.Context, id string) (DescribeResponse, error)

    // IsTaskComplete checks if an async task is complete
    IsTaskComplete(ctx context.Context, taskRef string) (done bool, err error)
}
```

## Implementation Steps

### 1. Create Provider Package

Create a new package under `internal/providers/` for your provider:

```
internal/providers/yourprovider/
├── provider.go      # Main provider implementation
├── session.go       # Connection/session management
├── tasks.go         # Async task handling
├── converter.go     # Type conversions
├── network.go       # Network operations
└── storage.go       # Storage operations
```

### 2. Implement the Provider

```go
package yourprovider

import (
    "context"
    "github.com/projectbeskar/virtrigaud/api/v1beta1"
    "github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

type Provider struct {
    config   *v1beta1.Provider
    client   YourProviderClient
}

func NewProvider(ctx context.Context, provider *v1beta1.Provider) (contracts.Provider, error) {
    // Initialize your provider client
    // Parse credentials from secret
    // Establish connection
    return &Provider{
        config: provider,
        client: client,
    }, nil
}

func (p *Provider) Validate(ctx context.Context) error {
    // Check connection health
    // Validate credentials
    return nil
}

// Implement other interface methods...
```

### 3. Create Provider gRPC Server

Create a gRPC server for your provider:

```go
// cmd/provider-yourprovider/main.go
package main

import (
    "context"
    "log"
    "net"
    
    "google.golang.org/grpc"
    "github.com/projectbeskar/virtrigaud/pkg/grpc/provider"
    "github.com/projectbeskar/virtrigaud/internal/providers/yourprovider"
)

func main() {
    lis, err := net.Listen("tcp", ":9090")
    if err != nil {
        log.Fatal(err)
    }
    
    s := grpc.NewServer()
    provider.RegisterProviderServer(s, &yourprovider.GRPCServer{})
    
    log.Println("Provider server listening on :9090")
    if err := s.Serve(lis); err != nil {
        log.Fatal(err)
    }
}
```

### 4. Handle Credentials

Providers should read credentials from Kubernetes secrets. Common credential fields:

- `username` / `password`: Basic authentication
- `token`: API token authentication
- `tls.crt` / `tls.key`: TLS client certificates

Example:

```go
func (p *Provider) getCredentials(ctx context.Context) (*Credentials, error) {
    secret := &corev1.Secret{}
    err := p.client.Get(ctx, types.NamespacedName{
        Name:      p.config.Spec.CredentialSecretRef.Name,
        Namespace: p.config.Namespace,
    }, secret)
    if err != nil {
        return nil, err
    }

    return &Credentials{
        Username: string(secret.Data["username"]),
        Password: string(secret.Data["password"]),
    }, nil
}
```

## Error Handling

Use the provided error types for consistent error handling:

```go
import "github.com/projectbeskar/virtrigaud/internal/providers/contracts"

// For not found errors
return contracts.NewNotFoundError("VM not found", err)

// For retryable errors
return contracts.NewRetryableError("Connection timeout", err)

// For validation errors
return contracts.NewInvalidSpecError("Invalid CPU count", nil)
```

## Asynchronous Operations

For long-running operations, return a task reference:

```go
func (p *Provider) Create(ctx context.Context, req CreateRequest) (CreateResponse, error) {
    taskID, err := p.client.CreateVMAsync(...)
    if err != nil {
        return CreateResponse{}, err
    }

    return CreateResponse{
        ID:      vmID,
        TaskRef: taskID,
    }, nil
}

func (p *Provider) IsTaskComplete(ctx context.Context, taskRef string) (bool, error) {
    task, err := p.client.GetTask(taskRef)
    if err != nil {
        return false, err
    }
    return task.IsComplete(), nil
}
```

## Type Conversions

Convert between CRD types and provider-specific types:

```go
func (p *Provider) convertVMClass(class contracts.VMClass) YourProviderVMSpec {
    return YourProviderVMSpec{
        CPUs:   class.CPU,
        Memory: class.MemoryMiB * 1024 * 1024, // Convert to bytes
        // ... other conversions
    }
}
```

## Testing

Create unit tests for your provider:

```go
func TestProvider_Create(t *testing.T) {
    provider := &Provider{
        client: &mockClient{},
    }

    req := contracts.CreateRequest{
        Name: "test-vm",
        // ... populate request
    }

    resp, err := provider.Create(context.Background(), req)
    assert.NoError(t, err)
    assert.NotEmpty(t, resp.ID)
}
```

## Provider-Specific CRD Fields

Update the CRD types to include provider-specific fields:

```go
// In VMImage types
type YourProviderImageSpec struct {
    ImageID   string `json:"imageId,omitempty"`
    Checksum  string `json:"checksum,omitempty"`
}

// In VMNetworkAttachment types
type YourProviderNetworkSpec struct {
    NetworkID string `json:"networkId,omitempty"`
    VLAN      int32  `json:"vlan,omitempty"`
}
```

## Best Practices

1. **Idempotency**: All operations should be idempotent
2. **Error Classification**: Use appropriate error types
3. **Resource Cleanup**: Ensure proper cleanup in Delete operations
4. **Logging**: Use structured logging with context
5. **Timeouts**: Respect context timeouts
6. **Rate Limiting**: Implement client-side rate limiting
7. **Retry Logic**: Handle transient failures gracefully

## Examples

See the existing providers for reference:

- `internal/providers/vsphere/` - vSphere implementation
- `internal/providers/libvirt/` - Libvirt implementation (production ready)

## Provider Configuration

Each provider type should support these configuration options:

- Connection endpoints
- Authentication credentials
- Default placement settings
- Rate limiting configuration
- Provider-specific options

Example Provider spec:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: my-provider
spec:
  type: yourprovider
  endpoint: https://api.yourprovider.com
  credentialSecretRef:
    name: provider-creds
  defaults:
    region: us-west-2
    zone: us-west-2a
  rateLimit:
    qps: 10
    burst: 20
```
