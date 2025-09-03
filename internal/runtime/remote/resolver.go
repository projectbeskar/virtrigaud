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

package remote

import (
	"context"
	"fmt"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	grpcClient "github.com/projectbeskar/virtrigaud/internal/transport/grpc"
)

// Resolver resolves Provider objects to remote provider implementations
type Resolver struct {
	client       client.Client
	clients      map[string]*grpcClient.Client
	clientsMutex sync.RWMutex
}

// NewResolver creates a new remote provider resolver
func NewResolver(k8sClient client.Client) *Resolver {
	return &Resolver{
		client:  k8sClient,
		clients: make(map[string]*grpcClient.Client),
	}
}

// GetProvider resolves a Provider object to a remote provider implementation
func (r *Resolver) GetProvider(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
	// All providers are now remote
	return r.getRemoteProvider(ctx, provider)
}

// getRemoteProvider creates or reuses a gRPC client for remote providers
func (r *Resolver) getRemoteProvider(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
	// Check if provider runtime is ready
	if provider.Status.Runtime == nil || provider.Status.Runtime.Endpoint == "" {
		return nil, fmt.Errorf("remote provider runtime is not ready: no endpoint available")
	}

	if provider.Status.Runtime.Phase != "Ready" {
		return nil, fmt.Errorf("remote provider runtime is not ready: phase=%s", provider.Status.Runtime.Phase)
	}

	// Create cache key
	cacheKey := fmt.Sprintf("%s/%s", provider.Namespace, provider.Name)

	r.clientsMutex.RLock()
	existingClient, exists := r.clients[cacheKey]
	r.clientsMutex.RUnlock()

	if exists {
		// Validate that the client is still usable
		if err := existingClient.Validate(ctx); err != nil {
			// Client is no longer valid, remove it and create a new one
			r.clientsMutex.Lock()
			delete(r.clients, cacheKey)
			existingClient.Close() //nolint:errcheck // Client cleanup not critical
			r.clientsMutex.Unlock()
		} else {
			// Client is still valid, reuse it
			return existingClient, nil
		}
	}

	// Create new gRPC client
	tlsConfig, err := r.buildTLSConfig(ctx, provider)
	if err != nil {
		return nil, fmt.Errorf("failed to build TLS config: %w", err)
	}

	client, err := grpcClient.NewClient(ctx, provider.Status.Runtime.Endpoint, tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC client: %w", err)
	}

	// Validate the new client
	if err := client.Validate(ctx); err != nil {
		client.Close() //nolint:errcheck // Client cleanup not critical
		return nil, fmt.Errorf("remote provider validation failed: %w", err)
	}

	// Cache the client
	r.clientsMutex.Lock()
	r.clients[cacheKey] = client
	r.clientsMutex.Unlock()

	return client, nil
}

// buildTLSConfig builds TLS configuration for gRPC client based on provider spec
func (r *Resolver) buildTLSConfig(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (*grpcClient.TLSConfig, error) {
	// If TLS is not enabled, return nil for insecure connection
	// TLS configuration removed in v1beta1, always return nil for insecure connection
	if true {
		return nil, nil
	}

	// For TLS-enabled providers, we would need to read the TLS secret
	// This is a simplified implementation - in production you'd want to:
	// 1. Read the TLS secret referenced in provider.Spec.Runtime.TLS.SecretRef
	// 2. Extract tls.crt, tls.key, and ca.crt
	// 3. Write them to temporary files or use in-memory certificates
	// 4. Return the appropriate TLSConfig

	// For now, return a basic TLS config that trusts the server certificate
	return &grpcClient.TLSConfig{
		Insecure: false, // This should be configurable for dev environments
	}, nil
}

// CleanupClient removes and closes a cached gRPC client
func (r *Resolver) CleanupClient(provider *infravirtrigaudiov1beta1.Provider) {
	cacheKey := fmt.Sprintf("%s/%s", provider.Namespace, provider.Name)

	r.clientsMutex.Lock()
	defer r.clientsMutex.Unlock()

	if client, exists := r.clients[cacheKey]; exists {
		client.Close() //nolint:errcheck // Client cleanup not critical
		delete(r.clients, cacheKey)
	}
}

// CleanupAllClients closes all cached gRPC clients
func (r *Resolver) CleanupAllClients() {
	r.clientsMutex.Lock()
	defer r.clientsMutex.Unlock()

	for key, client := range r.clients {
		client.Close() //nolint:errcheck // Client cleanup not critical
		delete(r.clients, key)
	}
}

// IsRemoteProvider checks if a provider is configured for remote runtime
// Always returns true since all providers are now remote
func (r *Resolver) IsRemoteProvider(provider *infravirtrigaudiov1beta1.Provider) bool {
	return true
}
