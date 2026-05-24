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
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	grpcClient "github.com/projectbeskar/virtrigaud/internal/transport/grpc"
)

// circuitBreakerName is the logical name passed to the resilience
// Registry when getting/creating a CircuitBreaker per Provider CR. We
// use a single name ("rpc") because today there is one breaker per
// Provider that guards every RPC on that Provider's client. If we ever
// want per-RPC-method breakers, this would become the RPC short name.
const circuitBreakerName = "rpc"

// Resolver resolves Provider objects to remote provider implementations
type Resolver struct {
	client       client.Client
	clients      map[string]*grpcClient.Client
	clientsMutex sync.RWMutex
	// cbRegistry is the CircuitBreaker registry shared by all gRPC
	// clients this resolver creates. May be nil — in which case clients
	// are constructed without circuit-breaker protection (intended for
	// tests that don't exercise the breaker path).
	cbRegistry *resilience.Registry
}

// NewResolver creates a new remote provider resolver.
//
// cbRegistry is the shared CircuitBreaker registry used to allocate one
// breaker per Provider CR. Passing nil disables circuit-breaker wiring
// for clients created by this resolver — useful in tests with a fake
// k8s client where the gRPC path is never actually dialed. Production
// callers (cmd/manager/main.go) must pass a real registry so the G6
// (#111) per-Provider breakers actually emit
// virtrigaud_circuit_breaker_* samples.
func NewResolver(k8sClient client.Client, cbRegistry *resilience.Registry) *Resolver {
	return &Resolver{
		client:     k8sClient,
		clients:    make(map[string]*grpcClient.Client),
		cbRegistry: cbRegistry,
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

	if provider.Status.Runtime.Phase != "Running" {
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

	// provider.Spec.Type populates the `provider_type` label on every
	// virtrigaud_provider_rpc_* sample emitted by this client (G4 / #90).
	// One CircuitBreaker per Provider CR is allocated from the shared
	// registry (G6 / #111); when cbRegistry is nil (test path), the
	// gRPC client is constructed without breaker protection.
	var cb *resilience.CircuitBreaker
	if r.cbRegistry != nil {
		cb = r.cbRegistry.GetOrCreate(circuitBreakerName, string(provider.Spec.Type), provider.Name)
	}
	client, err := grpcClient.NewClient(ctx, provider.Status.Runtime.Endpoint, string(provider.Spec.Type), cb, tlsConfig)
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

// CleanupClient removes and closes a cached gRPC client.
//
// Also removes the per-Provider CircuitBreaker from the registry (G6 /
// #111) so its metric series (virtrigaud_circuit_breaker_state and
// _failures_total with this Provider's labels) stop being emitted once
// the Provider CR is gone. Without this cleanup, deleted Providers
// would leak both a CB struct and stale metric samples indefinitely.
func (r *Resolver) CleanupClient(provider *infravirtrigaudiov1beta1.Provider) {
	cacheKey := fmt.Sprintf("%s/%s", provider.Namespace, provider.Name)

	r.clientsMutex.Lock()
	defer r.clientsMutex.Unlock()

	if client, exists := r.clients[cacheKey]; exists {
		client.Close() //nolint:errcheck // Client cleanup not critical
		delete(r.clients, cacheKey)
	}
	if r.cbRegistry != nil {
		r.cbRegistry.Remove(circuitBreakerName, string(provider.Spec.Type), provider.Name)
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
