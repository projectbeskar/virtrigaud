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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/operation"
)

// stubSigner is a Signer that never signs: the resolver only checks that one
// is loaded.
type stubSigner struct{}

func (stubSigner) InstallID() []byte           { return make([]byte, operation.IDLen) }
func (stubSigner) AuthorityGeneration() uint64 { return 1 }
func (stubSigner) Sign(context.Context, []byte, time.Time) (operation.Signature, error) {
	return operation.Signature{}, errors.New("stub signer never signs")
}

// identityTestProvider is newTestProvider of type t, with plaintext TLS opted out
// and a Running runtime at an address nothing listens on.
func identityTestProvider(t infravirtrigaudiov1beta1.ProviderType) *infravirtrigaudiov1beta1.Provider {
	p := newTestProvider(&infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false})
	p.Spec.Type = t
	p.UID = "uid-1"
	p.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{
		Phase: infravirtrigaudiov1beta1.ProviderRuntimePhaseRunning, Endpoint: "127.0.0.1:1",
	}
	return p
}

// The client mode comes from spec.type alone.
func TestRequiresIdentity_FromTypeOnly(t *testing.T) {
	for _, typ := range []infravirtrigaudiov1beta1.ProviderType{
		infravirtrigaudiov1beta1.ProviderTypeVSphere, infravirtrigaudiov1beta1.ProviderTypeLibvirt,
		infravirtrigaudiov1beta1.ProviderTypeProxmox, "mock",
	} {
		assert.False(t, requiresIdentity(identityTestProvider(typ)), "%s requires no operation identity", typ)
	}
	assert.True(t, requiresIdentity(identityTestProvider(infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor)))
}

// A Provider that requires operation identity is refused with ErrNoSigner
// when no signer is loaded, before anything is dialed, and in a release build
// even when one is passed.
func TestGetProvider_IdentityProviderFailsClosedWithoutSigner(t *testing.T) {
	ch := identityTestProvider(infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor)
	r := NewResolver(fake.NewClientBuilder().WithScheme(newResolverTestScheme(t)).Build(), nil)
	_, err := r.GetProvider(context.Background(), ch)
	require.ErrorIs(t, err, operation.ErrNoSigner)
	assert.Empty(t, r.clients, "nothing is cached")

	withSigner := NewResolver(fake.NewClientBuilder().WithScheme(newResolverTestScheme(t)).Build(), nil,
		WithOperationSigner(stubSigner{}))
	_, err = withSigner.GetProvider(context.Background(), ch)
	if operation.SignerCompiled {
		require.Error(t, err)
		assert.NotErrorIs(t, err, operation.ErrNoSigner, "a development build with a signer goes on to build the client")
		// The identity client needs verified TLS: a plaintext Provider is
		// refused before any dial.
		assert.Contains(t, err.Error(), "TLS")
	} else {
		require.ErrorIs(t, err, operation.ErrNoSigner, "a release build has no signer, whatever is passed")
	}
}

// A Provider that requires no operation identity is resolved as before,
// signer or not.
func TestGetProvider_NonIdentityProviderUnaffectedBySigner(t *testing.T) {
	lv := identityTestProvider(infravirtrigaudiov1beta1.ProviderTypeLibvirt)
	r := NewResolver(fake.NewClientBuilder().WithScheme(newResolverTestScheme(t)).Build(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := r.GetProvider(ctx, lv)
	require.Error(t, err, "nothing listens at the endpoint")
	assert.NotErrorIs(t, err, operation.ErrNoSigner)
}

// The client cache key carries the client mode.
func TestValidationKey_IncludesIdentityMode(t *testing.T) {
	r := NewResolver(fake.NewClientBuilder().WithScheme(newResolverTestScheme(t)).Build(), nil)
	lv := r.validationKey(context.Background(), identityTestProvider(infravirtrigaudiov1beta1.ProviderTypeLibvirt))
	ch := r.validationKey(context.Background(), identityTestProvider(infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor))
	assert.True(t, strings.HasSuffix(lv, "|"+operation.ProtectionNone.String()), lv)
	assert.True(t, strings.HasSuffix(ch, "|"+operation.ProtectionIdentity.String()), ch)
	assert.NotEqual(t, lv, ch)
}
