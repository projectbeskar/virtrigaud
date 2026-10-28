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
	ctrl "sigs.k8s.io/controller-runtime"
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
	clients      map[string]*cachedClient
	clientsMutex sync.RWMutex
	// providerLocks holds one *sync.Mutex per Provider (cache key). It
	// serialises every (re)validation, dial, eviction and store for that
	// Provider, so N reconciles arriving at once send one Validate and dial
	// one connection, and two configurations of one Provider can never
	// interleave their evict/store (no overwritten-but-open client, no
	// eviction of a client another resolution just installed).
	providerLocks sync.Map
	// cbRegistry is the CircuitBreaker registry shared by all gRPC
	// clients this resolver creates. May be nil — in which case clients
	// are constructed without circuit-breaker protection (intended for
	// tests that don't exercise the breaker path).
	cbRegistry *resilience.Registry
}

// cachedClient is a validated gRPC client and the configuration identity it
// was validated for (validationKey).
type cachedClient struct {
	client *grpcClient.Client
	key    string
	// uid and generation are the Provider object the client was validated
	// for; a resolution holding an older generation of the same Provider
	// never replaces it (newerCached).
	uid        types.UID
	generation int64
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
		clients:    make(map[string]*cachedClient),
		cbRegistry: cbRegistry,
	}
}

// GetProvider resolves a Provider object to a remote provider implementation
func (r *Resolver) GetProvider(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
	// All providers are now remote
	return r.getRemoteProvider(ctx, provider)
}

// runtimeEndpoint returns the gRPC endpoint of the Provider's runtime, or an
// error when the runtime is not Running or has no endpoint yet.
func runtimeEndpoint(provider *infravirtrigaudiov1beta1.Provider) (string, error) {
	if provider.Status.Runtime == nil || provider.Status.Runtime.Endpoint == "" {
		return "", fmt.Errorf("remote provider runtime is not ready: no endpoint available")
	}
	if provider.Status.Runtime.Phase != infravirtrigaudiov1beta1.ProviderRuntimePhaseRunning {
		return "", fmt.Errorf("remote provider runtime is not ready: phase=%s", provider.Status.Runtime.Phase)
	}
	return provider.Status.Runtime.Endpoint, nil
}

// ReadCapabilities reads the capabilities of the Provider's runtime over a
// connection opened for this read and closed after it
// (grpcClient.ReadCapabilities). It neither uses nor replaces the cached
// client GetProvider returns: that client's connection may still be held by a
// pod of a superseded rollout, and closing it would cut the RPCs other
// controllers have in flight on it. The Provider controller records what it
// returns on Provider status.reportedCapabilities.
func (r *Resolver) ReadCapabilities(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (contracts.Capabilities, error) {
	endpoint, err := runtimeEndpoint(provider)
	if err != nil {
		return contracts.Capabilities{}, err
	}
	tlsConfig, err := r.buildTLSConfig(ctx, provider)
	if err != nil {
		return contracts.Capabilities{}, fmt.Errorf("failed to build TLS config: %w", err)
	}
	caps, err := grpcClient.ReadCapabilities(ctx, endpoint, string(provider.Spec.Type), tlsConfig)
	if err != nil {
		return contracts.Capabilities{}, fmt.Errorf("read capabilities of Provider %s/%s: %w", provider.Namespace, provider.Name, err)
	}
	return caps, nil
}

// getRemoteProvider creates or reuses a gRPC client for remote providers
func (r *Resolver) getRemoteProvider(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
	// Check if provider runtime is ready
	if _, err := runtimeEndpoint(provider); err != nil {
		return nil, err
	}

	cacheKey := fmt.Sprintf("%s/%s", provider.Namespace, provider.Name)
	key := r.validationKey(ctx, provider)

	// A client validated for this exact configuration, whose connection and
	// breaker are healthy, is reused as it is: Validate checks the provider's
	// configuration and backend once per connection, not once per call.
	if cached := r.cachedFor(cacheKey, key); cached != nil && r.healthy(provider, cached) {
		return cached, nil
	}

	unlock := r.lockProvider(cacheKey)
	defer unlock()
	return r.revalidateOrConnect(ctx, provider, cacheKey, key)
}

// lockProvider takes cacheKey's Provider lock and returns its release.
func (r *Resolver) lockProvider(cacheKey string) (unlock func()) {
	m, _ := r.providerLocks.LoadOrStore(cacheKey, &sync.Mutex{})
	mu, _ := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// revalidateOrConnect runs with the Provider's lock held (lockProvider) when
// there is no reusable client: a cached client for the same configuration is
// validated again (its connection or breaker reported trouble) and kept if it
// passes; a cached client validated for a newer generation of the same
// Provider is used instead of being replaced by this caller's older view;
// otherwise the cached client, if any, is closed and a new one is dialed,
// validated and cached. A failed validation returns an error and caches
// nothing (fail closed).
func (r *Resolver) revalidateOrConnect(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, cacheKey, key string) (*grpcClient.Client, error) {
	// A resolution that completed while this one waited for the lock may
	// have installed a client for a newer spec than the Provider object this
	// caller holds (an informer read from before the update). Never roll it
	// back: use it, or fail if it no longer validates.
	if newer := r.newerCached(cacheKey, provider); newer != nil {
		if r.healthy(provider, newer) {
			return newer, nil
		}
		if err := newer.Validate(ctx); err != nil {
			return nil, fmt.Errorf("remote provider validation failed: %w", err)
		}
		return newer, nil
	}
	// Another caller may have finished while this one waited.
	if cached := r.cachedFor(cacheKey, key); cached != nil {
		if r.healthy(provider, cached) {
			return cached, nil
		}
		if err := cached.Validate(ctx); err == nil {
			return cached, nil
		}
	}
	r.evict(cacheKey)

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

	// Cache the client with the configuration it was validated for. An
	// empty key (the TLS Secret could not be read) is never reused.
	r.clientsMutex.Lock()
	r.clients[cacheKey] = &cachedClient{client: client, key: key, uid: provider.UID, generation: provider.Generation}
	r.clientsMutex.Unlock()

	return client, nil
}

// validationKey identifies the configuration a client is validated for: the
// Provider object (UID, so a re-created Provider is a new one), its spec
// (generation: endpoint, type, credentials reference, TLS block…), the
// runtime endpoint the manager dials (status, not covered by generation), and
// with TLS on, the TLS Secret's resourceVersion (rotated material needs a new
// connection). Any change makes the cached client unusable and forces a new,
// validated one. It returns "" when the TLS Secret cannot be read; "" matches
// no cached client, so the dial path reports the error.
func (r *Resolver) validationKey(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) string {
	key := fmt.Sprintf("%s|%d|%s|%s", provider.UID, provider.Generation, provider.Spec.Type, provider.Status.Runtime.Endpoint)
	rt := provider.Spec.Runtime
	if rt == nil || rt.Service == nil || rt.Service.TLS == nil || !rt.Service.TLS.Enabled {
		return key
	}
	ref := rt.Service.TLS.SecretRef
	if ref == nil || ref.Name == "" {
		return ""
	}
	secret := &corev1.Secret{}
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: ref.Name}, secret); err != nil {
		return ""
	}
	return key + "|" + secret.ResourceVersion
}

// cachedFor returns the cached client of cacheKey if it was validated for key.
func (r *Resolver) cachedFor(cacheKey, key string) *grpcClient.Client {
	if key == "" {
		return nil
	}
	r.clientsMutex.RLock()
	defer r.clientsMutex.RUnlock()
	if c, ok := r.clients[cacheKey]; ok && c.key == key {
		return c.client
	}
	return nil
}

// newerCached returns the cached client of cacheKey when it was validated
// for a newer generation of the same Provider object than provider.
func (r *Resolver) newerCached(cacheKey string, provider *infravirtrigaudiov1beta1.Provider) *grpcClient.Client {
	r.clientsMutex.RLock()
	defer r.clientsMutex.RUnlock()
	if c, ok := r.clients[cacheKey]; ok && c.uid == provider.UID && c.generation > provider.Generation {
		return c.client
	}
	return nil
}

// healthy reports whether a cached client may be reused without validating
// it again: its connection is usable and the Provider's circuit breaker is
// closed. A breaker that has opened (repeated failures) or is probing
// (half-open) sends the caller through validation, as every call did before.
func (r *Resolver) healthy(provider *infravirtrigaudiov1beta1.Provider, c *grpcClient.Client) bool {
	if !c.ConnectionUsable() {
		return false
	}
	if r.cbRegistry != nil {
		if cb, ok := r.cbRegistry.Get(circuitBreakerName, string(provider.Spec.Type), provider.Name); ok && cb.GetState() != resilience.StateClosed {
			return false
		}
	}
	return true
}

// evict closes and forgets the cached client of cacheKey, if any.
func (r *Resolver) evict(cacheKey string) {
	r.clientsMutex.Lock()
	defer r.clientsMutex.Unlock()
	if c, ok := r.clients[cacheKey]; ok {
		c.client.Close() //nolint:errcheck // Client cleanup not critical
		delete(r.clients, cacheKey)
	}
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

	// Loud, per-reconcile signal when the operator has opted into the
	// dev-only escape hatch. ADR-0003 mandates a steady drumbeat in the
	// manager log so this never silently survives into a regulated
	// environment. Info level (logr has no Warn); the "WARNING:" prefix
	// and structured K/V fields keep the line greppable.
	if tlsSpec.InsecureSkipVerify {
		ctrl.LoggerFrom(ctx).Info(
			"WARNING: Provider has spec.runtime.service.tls.insecureSkipVerify=true; the manager will NOT verify the provider gRPC server certificate. Use only for lab / first-bootstrap scenarios; this defeats mTLS.",
			"provider", provider.Name,
			"namespace", provider.Namespace,
		)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13, // ADR-0003 floor.
		ServerName:   serverName,
		// InsecureSkipVerify is a dev-only escape hatch. Defaulting to
		// false is enforced at the CRD schema level
		// (+kubebuilder:default=false on the field) and we never flip
		// it to true anywhere in this code path on the operator's
		// behalf — only honour what they explicitly set on the CR. The
		// gosec G402 suppression below uses the canonical
		// `// #nosec G402 -- <reason>` form so it satisfies both
		// golangci-lint's bundled gosec AND the standalone gosec
		// binary that GitHub Advanced Security invokes (the two read
		// different annotations).
		InsecureSkipVerify: tlsSpec.InsecureSkipVerify, // #nosec G402 -- operator-controlled escape hatch per ADR-0003; default is false (CRD schema), value is logged at WARN when true
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

	if c, exists := r.clients[cacheKey]; exists {
		c.client.Close() //nolint:errcheck // Client cleanup not critical
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

	for key, c := range r.clients {
		c.client.Close() //nolint:errcheck // Client cleanup not critical
		delete(r.clients, key)
	}
}

// IsRemoteProvider checks if a provider is configured for remote runtime
// Always returns true since all providers are now remote
func (r *Resolver) IsRemoteProvider(provider *infravirtrigaudiov1beta1.Provider) bool {
	return true
}
