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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// newResolverTestScheme registers v1beta1 + corev1 (for Secret reads).
func newResolverTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, infravirtrigaudiov1beta1.AddToScheme(sch))
	require.NoError(t, corev1.AddToScheme(sch))
	return sch
}

// genTestCertPEM generates a self-signed ECDSA cert + key pair and
// returns them PEM-encoded. Suitable for use as `tls.crt`/`tls.key`
// inside a test Secret. Not signed by a CA — fine for parser tests; we
// just need bytes that survive tls.X509KeyPair and
// x509.AppendCertsFromPEM.
func genTestCertPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-cert"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	require.NoError(t, err)

	keyDER, err := x509.MarshalECPrivateKey(priv)
	require.NoError(t, err)

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// newTestProvider builds a minimal Provider CR with the given tls spec
// attached. Pass nil for `tls` to model the loud-failure case.
func newTestProvider(tlsSpec *infravirtrigaudiov1beta1.ProviderTLSSpec) *infravirtrigaudiov1beta1.Provider {
	return &infravirtrigaudiov1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-provider",
			Namespace: "default",
		},
		Spec: infravirtrigaudiov1beta1.ProviderSpec{
			Type:     "mock",
			Endpoint: "localhost:9090",
			CredentialSecretRef: infravirtrigaudiov1beta1.ObjectRef{
				Name: "test-creds",
			},
			Runtime: &infravirtrigaudiov1beta1.ProviderRuntimeSpec{
				Mode:  infravirtrigaudiov1beta1.RuntimeModeRemote,
				Image: "virtrigaud/provider-mock:test",
				Service: &infravirtrigaudiov1beta1.ProviderServiceSpec{
					Port: 9443,
					TLS:  tlsSpec,
				},
			},
		},
	}
}

// TestBuildTLSConfig_NilTLSBlock — the loud-failure case. Per ADR-0003
// decision #3 (Accepted 2026-05-27), a Provider whose tls block is nil
// must produce ErrTLSBlockMissing so the controller can surface the
// `TLSConfigured=False, Reason=TLSBlockMissing` Condition.
func TestBuildTLSConfig_NilTLSBlock(t *testing.T) {
	sch := newResolverTestScheme(t)
	prov := newTestProvider(nil)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := NewResolver(cli, nil)

	cfg, err := r.buildTLSConfig(context.Background(), prov)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTLSBlockMissing),
		"nil tls block must produce ErrTLSBlockMissing, got %v", err)
	assert.Nil(t, cfg, "no TLSConfig should be returned alongside an error")
}

// TestBuildTLSConfig_NilService — when spec.runtime.service itself is
// nil (subset of the loud-failure case) the same error must fire.
func TestBuildTLSConfig_NilService(t *testing.T) {
	sch := newResolverTestScheme(t)
	prov := &infravirtrigaudiov1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "test-provider", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.ProviderSpec{
			Type:     "mock",
			Endpoint: "localhost:9090",
			Runtime: &infravirtrigaudiov1beta1.ProviderRuntimeSpec{
				Mode:  infravirtrigaudiov1beta1.RuntimeModeRemote,
				Image: "x:y",
				// Service intentionally nil.
			},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := NewResolver(cli, nil)

	_, err := r.buildTLSConfig(context.Background(), prov)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTLSBlockMissing), "nil service should also produce ErrTLSBlockMissing")
}

// TestBuildTLSConfig_EnabledFalse_ReturnsNilNil — explicit plaintext
// opt-out is the only path that legitimately returns (nil, nil).
func TestBuildTLSConfig_EnabledFalse_ReturnsNilNil(t *testing.T) {
	sch := newResolverTestScheme(t)
	prov := newTestProvider(&infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false})
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := NewResolver(cli, nil)

	cfg, err := r.buildTLSConfig(context.Background(), prov)
	require.NoError(t, err)
	assert.Nil(t, cfg, "tls.enabled=false must produce (nil, nil) — plaintext opt-out")
}

// TestBuildTLSConfig_EnabledTrue_NoSecretRef — tls.enabled=true with a
// nil secretRef must produce ErrTLSSecretRefMissing so the controller
// can attach a discriminating Condition reason.
func TestBuildTLSConfig_EnabledTrue_NoSecretRef(t *testing.T) {
	sch := newResolverTestScheme(t)
	prov := newTestProvider(&infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: true})
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := NewResolver(cli, nil)

	_, err := r.buildTLSConfig(context.Background(), prov)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTLSSecretRefMissing),
		"tls.enabled=true without secretRef should produce ErrTLSSecretRefMissing, got %v", err)
}

// TestBuildTLSConfig_EnabledTrue_MissingSecret — tls.enabled=true with
// a secretRef pointing at a non-existent Secret should fail with an
// IsNotFound-style error message.
func TestBuildTLSConfig_EnabledTrue_MissingSecret(t *testing.T) {
	sch := newResolverTestScheme(t)
	prov := newTestProvider(&infravirtrigaudiov1beta1.ProviderTLSSpec{
		Enabled:   true,
		SecretRef: &corev1.LocalObjectReference{Name: "does-not-exist"},
	})
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := NewResolver(cli, nil)

	_, err := r.buildTLSConfig(context.Background(), prov)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get TLS Secret default/does-not-exist",
		"missing-Secret error message must name the Secret being looked up")
}

// TestBuildTLSConfig_EnabledTrue_MissingCAKey — Secret exists but
// `ca.crt` is missing. The returned error must name the missing key so
// the operator-facing Condition can call it out.
func TestBuildTLSConfig_EnabledTrue_MissingCAKey(t *testing.T) {
	sch := newResolverTestScheme(t)
	certPEM, keyPEM := genTestCertPEM(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tls-secret", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
			// ca.crt intentionally missing
		},
	}
	prov := newTestProvider(&infravirtrigaudiov1beta1.ProviderTLSSpec{
		Enabled:   true,
		SecretRef: &corev1.LocalObjectReference{Name: "tls-secret"},
	})
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(secret).Build()
	r := NewResolver(cli, nil)

	_, err := r.buildTLSConfig(context.Background(), prov)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ca.crt", "error must name the missing ca.crt key")
}

// TestBuildTLSConfig_EnabledTrue_MissingCertKey — Secret missing `tls.crt`.
func TestBuildTLSConfig_EnabledTrue_MissingCertKey(t *testing.T) {
	sch := newResolverTestScheme(t)
	certPEM, keyPEM := genTestCertPEM(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tls-secret", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"tls.key": keyPEM,
			"ca.crt":  certPEM,
		},
	}
	prov := newTestProvider(&infravirtrigaudiov1beta1.ProviderTLSSpec{
		Enabled:   true,
		SecretRef: &corev1.LocalObjectReference{Name: "tls-secret"},
	})
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(secret).Build()
	r := NewResolver(cli, nil)

	_, err := r.buildTLSConfig(context.Background(), prov)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tls.crt", "error must name the missing tls.crt key")
}

// TestBuildTLSConfig_EnabledTrue_MissingKeyKey — Secret missing `tls.key`.
func TestBuildTLSConfig_EnabledTrue_MissingKeyKey(t *testing.T) {
	sch := newResolverTestScheme(t)
	certPEM, _ := genTestCertPEM(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tls-secret", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"ca.crt":  certPEM,
		},
	}
	prov := newTestProvider(&infravirtrigaudiov1beta1.ProviderTLSSpec{
		Enabled:   true,
		SecretRef: &corev1.LocalObjectReference{Name: "tls-secret"},
	})
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(secret).Build()
	r := NewResolver(cli, nil)

	_, err := r.buildTLSConfig(context.Background(), prov)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tls.key", "error must name the missing tls.key key")
}

// TestBuildTLSConfig_EnabledTrue_OpaqueSecret_Happy — Opaque Secret with
// all three keys is the canonical hand-rolled-PKI shape. Returns a
// usable *tls.Config with MinVersion=1.3, RootCAs populated, and one
// Certificate loaded.
func TestBuildTLSConfig_EnabledTrue_OpaqueSecret_Happy(t *testing.T) {
	sch := newResolverTestScheme(t)
	certPEM, keyPEM := genTestCertPEM(t)
	caPEM, _ := genTestCertPEM(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tls-secret", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
			"ca.crt":  caPEM,
		},
	}
	prov := newTestProvider(&infravirtrigaudiov1beta1.ProviderTLSSpec{
		Enabled:   true,
		SecretRef: &corev1.LocalObjectReference{Name: "tls-secret"},
	})
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(secret).Build()
	r := NewResolver(cli, nil)

	cfg, err := r.buildTLSConfig(context.Background(), prov)
	require.NoError(t, err)
	require.NotNil(t, cfg, "should return a non-nil *grpcClient.TLSConfig")
	require.NotNil(t, cfg.PrebuiltConfig, "should carry a PrebuiltConfig (*tls.Config)")

	pre := cfg.PrebuiltConfig
	assert.Equal(t, uint16(tls.VersionTLS13), pre.MinVersion, "MinVersion must be TLS 1.3 per ADR-0003")
	assert.Len(t, pre.Certificates, 1, "exactly one client cert must be loaded")
	assert.NotNil(t, pre.RootCAs, "RootCAs pool must be built from ca.crt")
	assert.False(t, pre.InsecureSkipVerify, "InsecureSkipVerify must default to false")
	assert.Contains(t, pre.ServerName, "test-provider",
		"ServerName must be derived from the Provider name; got %q", pre.ServerName)
}

// TestBuildTLSConfig_EnabledTrue_KubernetesTLSSecret_Happy — the same
// happy path but the Secret is typed `kubernetes.io/tls` (cert-manager
// shape). Per ADR-0003 both shapes must work as long as the three keys
// are present.
func TestBuildTLSConfig_EnabledTrue_KubernetesTLSSecret_Happy(t *testing.T) {
	sch := newResolverTestScheme(t)
	certPEM, keyPEM := genTestCertPEM(t)
	caPEM, _ := genTestCertPEM(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tls-secret", Namespace: "default"},
		Type:       corev1.SecretTypeTLS, // kubernetes.io/tls
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
			"ca.crt":  caPEM, // accepted as an extra key on a kubernetes.io/tls Secret
		},
	}
	prov := newTestProvider(&infravirtrigaudiov1beta1.ProviderTLSSpec{
		Enabled:   true,
		SecretRef: &corev1.LocalObjectReference{Name: "tls-secret"},
	})
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(secret).Build()
	r := NewResolver(cli, nil)

	cfg, err := r.buildTLSConfig(context.Background(), prov)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.PrebuiltConfig)

	pre := cfg.PrebuiltConfig
	assert.Equal(t, uint16(tls.VersionTLS13), pre.MinVersion)
	assert.Len(t, pre.Certificates, 1)
	assert.NotNil(t, pre.RootCAs)
}

// TestBuildTLSConfig_EnabledTrue_InsecureSkipVerifyHonored — the dev-
// only escape hatch on the CRD field must propagate verbatim. We never
// flip this on the operator's behalf; we only honour what's set.
func TestBuildTLSConfig_EnabledTrue_InsecureSkipVerifyHonored(t *testing.T) {
	sch := newResolverTestScheme(t)
	certPEM, keyPEM := genTestCertPEM(t)
	caPEM, _ := genTestCertPEM(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tls-secret", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
			"ca.crt":  caPEM,
		},
	}
	prov := newTestProvider(&infravirtrigaudiov1beta1.ProviderTLSSpec{
		Enabled:            true,
		SecretRef:          &corev1.LocalObjectReference{Name: "tls-secret"},
		InsecureSkipVerify: true,
	})
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(secret).Build()
	r := NewResolver(cli, nil)

	cfg, err := r.buildTLSConfig(context.Background(), prov)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.PrebuiltConfig)

	assert.True(t, cfg.PrebuiltConfig.InsecureSkipVerify,
		"tls.insecureSkipVerify=true on the CR must propagate to the *tls.Config")
}
