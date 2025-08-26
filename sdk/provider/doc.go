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

/*
Package provider provides a comprehensive SDK for building VirtRigaud providers.

The SDK includes the following packages:

  - server: gRPC server bootstrapping utilities with TLS, health checks, metrics, and middleware
  - errors: Typed error handling that maps to gRPC status codes  
  - middleware: gRPC interceptors for logging, authentication, rate limiting, recovery, and timeouts
  - capabilities: Provider capability management and advertisement
  - client: High-level gRPC client with retries, circuit breakers, and typed error mapping

# Basic Usage

To create a provider server:

    import (
        "github.com/projectbeskar/virtrigaud/sdk/provider/server"
        "github.com/projectbeskar/virtrigaud/sdk/provider/capabilities"
        "github.com/projectbeskar/virtrigaud/sdk/provider/middleware"
    )

    // Create capability manager
    caps := capabilities.NewBuilder().
        Core().
        Snapshots().
        VSphere().
        DiskTypes("thin", "thick").
        NetworkTypes("bridge", "vlan").
        Build()

    // Configure server
    config := server.DefaultConfig()
    config.TLS = &server.TLSConfig{
        CertFile: "/etc/certs/tls.crt",
        KeyFile:  "/etc/certs/tls.key",
    }
    config.Middleware = &middleware.Config{
        Logging: &middleware.LoggingConfig{Enabled: true},
        Recovery: &middleware.RecoveryConfig{Enabled: true},
    }

    // Create server
    srv, err := server.New(config)
    if err != nil {
        log.Fatal(err)
    }

    // Register provider implementation
    srv.RegisterProvider(&MyProviderImpl{caps: caps})

    // Start server
    if err := srv.Serve(context.Background()); err != nil {
        log.Fatal(err)
    }

# Error Handling

Use typed errors for consistent gRPC status code mapping:

    import "github.com/projectbeskar/virtrigaud/sdk/provider/errors"

    func (p *MyProvider) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
        vm, err := p.findVM(req.Id)
        if err != nil {
            return nil, errors.NewNotFound("VirtualMachine", req.Id)
        }
        
        if !p.canAccess(vm) {
            return nil, errors.NewPermissionDenied("describe VM")
        }
        
        // ... implementation
    }

# Client Usage

To create a provider client:

    import "github.com/projectbeskar/virtrigaud/sdk/provider/client"

    config := client.DefaultConfig("provider.example.com:9443")
    config.TLS.Enabled = true
    config.TLS.ServerName = "provider.example.com"

    client, err := client.New(config)
    if err != nil {
        log.Fatal(err)
    }
    defer client.Close()

    // Use client
    resp, err := client.GetCapabilities(ctx, &providerv1.GetCapabilitiesRequest{})

# Compatibility Policy

The SDK follows semantic versioning. Breaking changes to the public API will only be made in major version releases.

The SDK abstracts the underlying gRPC protocol and provides stable interfaces that will not change in minor releases, even if the internal RPC protocol evolves.

See the Provider Developer Guide for complete documentation: https://projectbeskar.github.io/virtrigaud/providers/
*/
package provider
