/*
Copyright 2026.

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
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// validateCounter is a provider gRPC server that only answers Validate. It
// counts the calls and answers ok, or not ok while failing is set.
type validateCounter struct {
	providerv1.UnimplementedProviderServer
	calls   atomic.Int32
	failing atomic.Bool
	delay   time.Duration
}

// Validate counts the call and answers after delay.
func (v *validateCounter) Validate(context.Context, *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
	v.calls.Add(1)
	time.Sleep(v.delay)
	if v.failing.Load() {
		return &providerv1.ValidateResponse{Ok: false, Message: "backend unreachable"}, nil
	}
	return &providerv1.ValidateResponse{Ok: true}, nil
}

// startValidateServer serves v on a loopback port and returns its address.
func startValidateServer(t *testing.T, v *validateCounter) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	providerv1.RegisterProviderServer(srv, v)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// runningProvider is a plaintext Provider whose runtime serves at endpoint.
func runningProvider(endpoint string) *infravirtrigaudiov1beta1.Provider {
	p := newTestProvider(&infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false})
	p.UID = "uid-1"
	p.Generation = 1
	p.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Endpoint: endpoint, Phase: "Running"}
	return p
}

// newValidationResolver returns a resolver over an empty fake client.
func newValidationResolver(t *testing.T, cb *resilience.Registry) *Resolver {
	t.Helper()
	return NewResolver(fake.NewClientBuilder().WithScheme(newResolverTestScheme(t)).Build(), cb)
}

// TestGetProvider_ReusesValidatedClient: a healthy cached client is reused
// without another Validate.
func TestGetProvider_ReusesValidatedClient(t *testing.T) {
	v := &validateCounter{}
	p := runningProvider(startValidateServer(t, v))
	r := newValidationResolver(t, resilience.NewRegistry(resilience.DefaultConfig()))
	ctx := context.Background()

	first, err := r.GetProvider(ctx, p)
	require.NoError(t, err)
	for i := 0; i < 50; i++ {
		again, err := r.GetProvider(ctx, p)
		require.NoError(t, err)
		assert.Same(t, first, again)
	}
	assert.Equal(t, int32(1), v.calls.Load(), "one Validate for the connection, not one per call")
}

// TestGetProvider_ConcurrentFirstUseValidatesOnce: concurrent first
// resolutions dial and validate once.
func TestGetProvider_ConcurrentFirstUseValidatesOnce(t *testing.T) {
	v := &validateCounter{delay: 200 * time.Millisecond}
	p := runningProvider(startValidateServer(t, v))
	r := newValidationResolver(t, nil)

	var wg sync.WaitGroup
	got := make([]any, 20)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := r.GetProvider(context.Background(), p)
			assert.NoError(t, err)
			got[i] = c
		}(i)
	}
	wg.Wait()
	assert.Equal(t, int32(1), v.calls.Load())
	for _, c := range got {
		assert.Same(t, got[0], c)
	}
}

// TestGetProvider_ConfigurationChangeRevalidates: a new spec generation, a
// new runtime endpoint or a re-created Provider (new UID) gets a new,
// validated client.
func TestGetProvider_ConfigurationChangeRevalidates(t *testing.T) {
	v1, v2 := &validateCounter{}, &validateCounter{}
	addr1, addr2 := startValidateServer(t, v1), startValidateServer(t, v2)
	r := newValidationResolver(t, nil)
	ctx := context.Background()

	p := runningProvider(addr1)
	c1, err := r.GetProvider(ctx, p)
	require.NoError(t, err)

	p.Generation = 2 // e.g. spec.credentialSecretRef or the TLS block changed
	c2, err := r.GetProvider(ctx, p)
	require.NoError(t, err)
	assert.NotSame(t, c1, c2)
	assert.Equal(t, int32(2), v1.calls.Load())

	p.Status.Runtime.Endpoint = addr2 // the runtime moved
	c3, err := r.GetProvider(ctx, p)
	require.NoError(t, err)
	assert.NotSame(t, c2, c3)
	assert.Equal(t, int32(1), v2.calls.Load(), "the new endpoint is validated")

	p.UID = "uid-2" // deleted and re-created under the same name
	c4, err := r.GetProvider(ctx, p)
	require.NoError(t, err)
	assert.NotSame(t, c3, c4)
	assert.Equal(t, int32(2), v2.calls.Load())

	_, err = r.GetProvider(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, int32(2), v2.calls.Load(), "unchanged again: reused")
}

// TestValidationKey_TLSSecretRotation: with TLS on, the key follows the TLS
// Secret's resourceVersion (rotated material needs a new connection), and a
// Secret that cannot be read gives the never-reused empty key.
func TestValidationKey_TLSSecretRotation(t *testing.T) {
	sch := newResolverTestScheme(t)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tls-secret", Namespace: "default"}, Data: map[string][]byte{"tls.crt": []byte("a")}}
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(secret).Build()
	r := NewResolver(cli, nil)
	ctx := context.Background()

	p := runningProvider("127.0.0.1:1")
	p.Spec.Runtime.Service.TLS = &infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: true, SecretRef: &corev1.LocalObjectReference{Name: "tls-secret"}}
	before := r.validationKey(ctx, p)
	require.NotEmpty(t, before)
	assert.Equal(t, before, r.validationKey(ctx, p), "stable while nothing changes")

	secret.Data["tls.crt"] = []byte("rotated")
	require.NoError(t, cli.Update(ctx, secret))
	assert.NotEqual(t, before, r.validationKey(ctx, p), "a rotated Secret invalidates")

	require.NoError(t, cli.Delete(ctx, secret))
	assert.Empty(t, r.validationKey(ctx, p))
	assert.Nil(t, r.cachedFor("default/test-provider", ""), "the empty key never matches a cached client")
}

// TestGetProvider_ValidationFailureFailsClosedAndRecovers: a provider that
// fails validation is not cached and not returned; once it passes, it is.
func TestGetProvider_ValidationFailureFailsClosedAndRecovers(t *testing.T) {
	v := &validateCounter{}
	v.failing.Store(true)
	p := runningProvider(startValidateServer(t, v))
	r := newValidationResolver(t, nil)
	ctx := context.Background()

	_, err := r.GetProvider(ctx, p)
	require.Error(t, err)
	_, err = r.GetProvider(ctx, p)
	require.Error(t, err)
	assert.Equal(t, int32(2), v.calls.Load(), "a failed validation is retried, never cached")

	v.failing.Store(false)
	c, err := r.GetProvider(ctx, p)
	require.NoError(t, err)
	require.NotNil(t, c)
	_, err = r.GetProvider(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, int32(3), v.calls.Load(), "validated once after recovery, then reused")
}

// TestGetProvider_OpenBreakerRevalidates: while the Provider's circuit
// breaker is open the cached client is not trusted: resolution validates
// again and fails closed; after the breaker resets it resolves again.
func TestGetProvider_OpenBreakerRevalidates(t *testing.T) {
	v := &validateCounter{}
	p := runningProvider(startValidateServer(t, v))
	reg := resilience.NewRegistry(resilience.DefaultConfig())
	r := newValidationResolver(t, reg)
	ctx := context.Background()

	_, err := r.GetProvider(ctx, p)
	require.NoError(t, err)

	cb := reg.GetOrCreate(circuitBreakerName, string(p.Spec.Type), p.Name)
	boom := errors.New("provider unavailable")
	for i := 0; i < resilience.DefaultConfig().FailureThreshold; i++ {
		_ = cb.Call(ctx, func(context.Context) error { return boom })
	}
	require.Equal(t, resilience.StateOpen, cb.GetState())

	_, err = r.GetProvider(ctx, p)
	require.Error(t, err, "an open breaker must not hand out the cached client")

	cb.Reset()
	_, err = r.GetProvider(ctx, p)
	require.NoError(t, err)
}

// TestGetProvider_ClosedConnectionIsReplaced: a cached client whose
// connection was closed is validated, found unusable and replaced by a new,
// validated client.
func TestGetProvider_ClosedConnectionIsReplaced(t *testing.T) {
	v := &validateCounter{}
	p := runningProvider(startValidateServer(t, v))
	r := newValidationResolver(t, nil)
	ctx := context.Background()

	c1, err := r.GetProvider(ctx, p)
	require.NoError(t, err)
	r.clientsMutex.RLock()
	require.NoError(t, r.clients["default/test-provider"].client.Close())
	r.clientsMutex.RUnlock()

	c2, err := r.GetProvider(ctx, p)
	require.NoError(t, err)
	assert.NotSame(t, c1, c2)
	assert.Equal(t, int32(2), v.calls.Load())
}

// TestGetProvider_StaleConfigurationNeverReplacesNewer: a resolution that
// holds an older generation of the Provider (an informer read from before an
// update) after a newer configuration has been validated gets the newer
// client; it does not dial its old endpoint, evict or close the newer one.
func TestGetProvider_StaleConfigurationNeverReplacesNewer(t *testing.T) {
	vOld, vNew := &validateCounter{}, &validateCounter{}
	oldAddr, newAddr := startValidateServer(t, vOld), startValidateServer(t, vNew)
	r := newValidationResolver(t, nil)
	ctx := context.Background()

	stale := runningProvider(oldAddr) // generation 1
	current := runningProvider(newAddr)
	current.Generation = 2

	newer, err := r.GetProvider(ctx, current)
	require.NoError(t, err)

	got, err := r.GetProvider(ctx, stale)
	require.NoError(t, err)
	assert.Same(t, newer, got, "the stale caller gets the newer validated client")
	assert.Equal(t, int32(0), vOld.calls.Load(), "the old endpoint is never dialed or validated")
	assert.Equal(t, int32(1), vNew.calls.Load())

	r.clientsMutex.RLock()
	entry := r.clients["default/test-provider"]
	r.clientsMutex.RUnlock()
	assert.Same(t, newer, entry.client)
	assert.Equal(t, int64(2), entry.generation)
	assert.True(t, entry.client.ConnectionUsable(), "the newer client was not closed")
}

// TestGetProvider_ConcurrentConfigurationsConverge: old- and new-generation
// resolutions racing from an empty cache end with the newest configuration
// cached, and every other client handed out has been closed (none is
// overwritten while still open).
func TestGetProvider_ConcurrentConfigurationsConverge(t *testing.T) {
	vOld, vNew := &validateCounter{delay: 20 * time.Millisecond}, &validateCounter{delay: 20 * time.Millisecond}
	oldAddr, newAddr := startValidateServer(t, vOld), startValidateServer(t, vNew)
	r := newValidationResolver(t, nil)

	var mu sync.Mutex
	handed := map[any]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := runningProvider(oldAddr)
			if i%2 == 1 {
				p = runningProvider(newAddr)
				p.Generation = 2
			}
			c, err := r.GetProvider(context.Background(), p)
			if !assert.NoError(t, err) {
				return
			}
			mu.Lock()
			handed[c] = true
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	r.clientsMutex.RLock()
	entry := r.clients["default/test-provider"]
	r.clientsMutex.RUnlock()
	require.NotNil(t, entry)
	assert.Equal(t, int64(2), entry.generation, "the newest configuration wins")
	assert.True(t, entry.client.ConnectionUsable())
	assert.Equal(t, int32(1), vNew.calls.Load(), "the newest configuration is validated once")
	for c := range handed {
		if c == any(entry.client) {
			continue
		}
		cl, ok := c.(interface{ ConnectionUsable() bool })
		require.True(t, ok)
		assert.False(t, cl.ConnectionUsable(), "a replaced client must have been closed, not leaked")
	}
}
