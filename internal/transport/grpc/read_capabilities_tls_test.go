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

package grpc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// selfSignedServerCert returns a self-signed certificate for serverName and a
// pool that trusts it.
func selfSignedServerCert(t *testing.T, serverName string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: serverName},
		DNSNames:              []string{serverName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// TestReadCapabilities_TLS pins that the one-shot capability read uses the
// TLS configuration it is given (the Provider's, built by the resolver): it
// reads from a TLS provider that the configuration trusts, and a plaintext
// read of the same provider fails.
func TestReadCapabilities_TLS(t *testing.T) {
	const serverName = "provider.example.com"
	cert, pool := selfSignedServerCert(t, serverName)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gsrv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	})))
	providerv1.RegisterProviderServer(gsrv, &fakeProviderServer{
		GetCapabilitiesFn: func(context.Context, *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
			return &providerv1.GetCapabilitiesResponse{SupportsSnapshots: true}, nil
		},
	})
	go func() { _ = gsrv.Serve(lis) }()
	t.Cleanup(gsrv.Stop)
	addr := lis.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	caps, err := ReadCapabilities(ctx, addr, "read-caps-tls-test", &TLSConfig{PrebuiltConfig: &tls.Config{
		RootCAs:    pool,
		ServerName: serverName,
		MinVersion: tls.VersionTLS13,
	}})
	require.NoError(t, err)
	assert.True(t, caps.SupportsSnapshots)

	plainCtx, plainCancel := context.WithTimeout(context.Background(), time.Second)
	defer plainCancel()
	_, err = ReadCapabilities(plainCtx, addr, "read-caps-tls-test", nil)
	require.Error(t, err, "a plaintext read of a TLS provider fails")

	untrustedCtx, untrustedCancel := context.WithTimeout(context.Background(), time.Second)
	defer untrustedCancel()
	_, err = ReadCapabilities(untrustedCtx, addr, "read-caps-tls-test", &TLSConfig{PrebuiltConfig: &tls.Config{
		RootCAs:    x509.NewCertPool(),
		ServerName: serverName,
		MinVersion: tls.VersionTLS13,
	}})
	require.Error(t, err, "a provider certificate the configuration does not trust is refused")
}
