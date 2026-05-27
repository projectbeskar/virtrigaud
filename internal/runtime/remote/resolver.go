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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	grpcClient "github.com/projectbeskar/virtrigaud/internal/transport/grpc"
)

// Required key names inside a Provider TLS Secret.
//
// Per ADR-0003 the canonical layout is `tls.crt` / `tls.key` / `ca.crt`,
// matching the kube-apiserver / cert-manager / `kubernetes.io/tls`
// convention. Both `kubernetes.io/tls`-typed Secrets (which carry
// `tls.crt`/`tls.key` and accept `ca.crt` as an extra key) and plain
// `Opaque` Secrets (all three keys explicit) are supported.
const (
	tlsSecretKeyCert = "tls.crt"
	tlsSecretKeyKey  = "tls.key"
	tlsSecretKeyCA   = "ca.crt"
)

// ErrTLSBlockMissing is returned from buildTLSConfig when the Provider
// CR's spec.runtime.service.tls field is nil. Per ADR-0003 (Accepted
// 2026-05-27, decision #3) v0.3.7 ships TLS-on-by-default with a "loud
// failure" semantic: an unset tls block is an explicit operator-action
// gate, not a silent plaintext fallback. The caller (ProviderController)
// is expected to detect this error via errors.Is and surface a Condition
// (`TLSConfigured=False, Reason=TLSBlockMissing`) instructing the
// operator to either (a) provision a Secret and set tls.enabled=true, or
// (b) explicitly set tls.enabled=false to keep plaintext.
var ErrTLSBlockMissing = errors.New("provider.spec.runtime.service.tls is unset; v0.3.7 requires an explicit TLS decision (see umbrella #156 / ADR-0003)")

// ErrTLSSecretRefMissing is returned when tls.enabled=true but
// tls.secretRef is nil or empty. Without a Secret reference there is no
// material to load.
var ErrTLSSecretRefMissing = errors.New("provider.spec.runtime.service.tls.enabled=true requires a non-empty secretRef")

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
	// provider.Name populates the `provider` label on every
	// virtrigaud_vm_operations_total sample (G7.1 / #124).
	// One CircuitBreaker per Provider CR is allocated from the shared
	// registry (G6 / #111); when cbRegistry is nil (test path), the
	// gRPC client is constructed without breaker protection.
	var cb *resilience.CircuitBreaker
	if r.cbRegistry != nil {
		cb = r.cbRegistry.GetOrCreate(circuitBreakerName, string(provider.Spec.Type), provider.Name)
	}
	client, err := grpcClient.NewClient(ctx, provider.Status.Runtime.Endpoint, string(provider.Spec.Type), provider.Name, cb, tlsConfig)
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

// buildTLSConfig builds TLS configuration for the gRPC client from the
// Provider CR's spec.runtime.service.tls block.
//
// Semantics (ADR-0003 / umbrella #156):
//
//   - tls block nil  → return ErrTLSBlockMissing. v0.3.7 ships TLS-on-
//     by-default; the operator must make an explicit decision. The
//     caller (ProviderController) detects this error with errors.Is and
//     surfaces a `TLSConfigured=False, Reason=TLSBlockMissing` Condition
//     instructing the operator to either provision a TLS Secret or
//     explicitly opt out via tls.enabled=false.
//   - tls.enabled=false → return (nil, nil). Explicit plaintext opt-out;
//     manager dials over insecure credentials. Compensating control
//     (NetworkPolicy + encrypted CNI) is the operator's responsibility.
//   - tls.enabled=true  → load tls.crt / tls.key / ca.crt from the
//     referenced Secret and build a *tls.Config with TLS1.3 floor.
//
// Both `kubernetes.io/tls`-typed Secrets (canonical tls.crt/tls.key plus
// extra ca.crt) and plain Opaque Secrets (all three keys explicit) are
// supported — both shapes are documented in the PR-4 operator runbook.
//
// This is the v0.3.7 baseline: a fresh *tls.Config is built per call.
// Hot-reload via sigs.k8s.io/controller-runtime/pkg/certwatcher is the
// scope of PR-3, not PR-1. Until then Kubernetes' Secret-to-Pod sync
// (~60s) plus a fresh dial cycle is the rotation mechanism.
func (r *Resolver) buildTLSConfig(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (*grpcClient.TLSConfig, error) {
	// Defensive: the calling path validates spec.Runtime != nil before
	// reaching here, but make this function safe to call directly in
	// tests against Providers with partial specs.
	if provider.Spec.Runtime == nil || provider.Spec.Runtime.Service == nil || provider.Spec.Runtime.Service.TLS == nil {
		return nil, ErrTLSBlockMissing
	}
	tlsSpec := provider.Spec.Runtime.Service.TLS

	// Explicit plaintext opt-out. Visible to compliance auditors via
	// the Condition the controller emits separately.
	if !tlsSpec.Enabled {
		return nil, nil
	}

	if tlsSpec.SecretRef == nil || tlsSpec.SecretRef.Name == "" {
		return nil, ErrTLSSecretRefMissing
	}

	// Load the Secret. Per ADR-0003 the Secret lives in the Provider's
	// namespace (single administrative boundary).
	secret := &corev1.Secret{}
	if err := r.client.Get(ctx, types.NamespacedName{
		Namespace: provider.Namespace,
		Name:      tlsSpec.SecretRef.Name,
	}, secret); err != nil {
		return nil, fmt.Errorf("get TLS Secret %s/%s: %w", provider.Namespace, tlsSpec.SecretRef.Name, err)
	}

	// Validate required keys are present. Both kubernetes.io/tls
	// Secrets and Opaque Secrets are accepted as long as all three keys
	// exist; this is the price of supporting per-operator PKI shapes
	// (cert-manager-produced kubernetes.io/tls vs. hand-rolled Opaque).
	var missing []string
	certPEM, ok := secret.Data[tlsSecretKeyCert]
	if !ok || len(certPEM) == 0 {
		missing = append(missing, tlsSecretKeyCert)
	}
	keyPEM, ok := secret.Data[tlsSecretKeyKey]
	if !ok || len(keyPEM) == 0 {
		missing = append(missing, tlsSecretKeyKey)
	}
	caPEM, ok := secret.Data[tlsSecretKeyCA]
	if !ok || len(caPEM) == 0 {
		missing = append(missing, tlsSecretKeyCA)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("TLS Secret %s/%s is missing required key(s): %v (expected: tls.crt, tls.key, ca.crt)",
			provider.Namespace, tlsSpec.SecretRef.Name, missing)
	}

	// Parse certificate + key into a usable tls.Certificate.
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse client cert/key from TLS Secret %s/%s: %w", provider.Namespace, tlsSpec.SecretRef.Name, err)
	}

	// Build the CA pool that the manager uses to verify the provider's
	// server certificate.
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse ca.crt from TLS Secret %s/%s: no valid PEM certificates found", provider.Namespace, tlsSpec.SecretRef.Name)
	}

	// ServerName mirrors how the dial target is constructed in
	// reconcileRemoteRuntime: <service>.<namespace>.svc.cluster.local.
	// Anchoring SNI to the Service FQDN lets operators mint provider
	// server certs against a deterministic SAN.
	serverName := fmt.Sprintf("virtrigaud-provider-%s-%s.%s.svc.cluster.local",
		provider.Namespace, provider.Name, provider.Namespace)

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13, // ADR-0003 floor.
		ServerName:   serverName,
		// InsecureSkipVerify is a dev-only escape hatch. Defaulting to
		// false is enforced at the CRD schema level
		// (+kubebuilder:default=false on the field) and we never flip
		// it to true anywhere in this code path on the operator's
		// behalf — only honour what they explicitly set on the CR.
		InsecureSkipVerify: tlsSpec.InsecureSkipVerify, //nolint:gosec // operator-controlled dev escape hatch; see ADR-0003
	}

	return &grpcClient.TLSConfig{PrebuiltConfig: tlsCfg}, nil
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
