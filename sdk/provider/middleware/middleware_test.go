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

package middleware

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
	"net/url"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// mintLeafCert returns a self-signed *x509.Certificate whose Subject CN,
// DNSNames, and URIs reflect the args. The certificate is *not* chained to
// any CA — the tests only care about the leaf's identity fields.
func mintLeafCert(t *testing.T, cn string, dnsSANs []string, uriSANs []string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     dnsSANs,
	}
	for _, u := range uriSANs {
		parsed, err := url.Parse(u)
		if err != nil {
			t.Fatalf("parse URI %q: %v", u, err)
		}
		tmpl.URIs = append(tmpl.URIs, parsed)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return cert
}

// ctxWithVerifiedPeer builds a context carrying a peer.Peer whose AuthInfo is
// a credentials.TLSInfo with VerifiedChains populated by `leaf`. This matches
// the post-handshake state the gRPC server gives to interceptors when
// ClientAuth=RequireAndVerifyClientCert has succeeded.
func ctxWithVerifiedPeer(leaf *x509.Certificate) context.Context {
	tlsInfo := credentials.TLSInfo{
		State: tls.ConnectionState{
			VerifiedChains: [][]*x509.Certificate{{leaf}},
		},
	}
	p := &peer.Peer{
		Addr:     &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345},
		AuthInfo: tlsInfo,
	}
	return peer.NewContext(context.Background(), p)
}

func ctxWithUnverifiedPeer(leaf *x509.Certificate) context.Context {
	tlsInfo := credentials.TLSInfo{
		State: tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{leaf},
			// VerifiedChains intentionally empty — simulates a TLS server that
			// did NOT enforce ClientAuth=RequireAndVerifyClientCert.
		},
	}
	p := &peer.Peer{
		Addr:     &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345},
		AuthInfo: tlsInfo,
	}
	return peer.NewContext(context.Background(), p)
}

func ctxWithPlaintextPeer() context.Context {
	p := &peer.Peer{
		Addr:     &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345},
		AuthInfo: nil,
	}
	return peer.NewContext(context.Background(), p)
}

// TestValidateTLSPeer_NoPeerInfo: an interceptor called without a peer on the
// context (extremely rare, but defensible) must reject with Unauthenticated.
func TestValidateTLSPeer_NoPeerInfo(t *testing.T) {
	err := validateTLSPeer(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Errorf("expected code Unauthenticated, got %s", got)
	}
}

// TestValidateTLSPeer_PlaintextConnection: peer present but AuthInfo nil
// (plaintext connection) must reject with Unauthenticated.
func TestValidateTLSPeer_PlaintextConnection(t *testing.T) {
	err := validateTLSPeer(ctxWithPlaintextPeer(), nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Errorf("expected code Unauthenticated, got %s", got)
	}
}

// TestValidateTLSPeer_NoVerifiedChain: TLS handshake completed but the server
// did not enforce client-cert verification — must reject with Unauthenticated.
func TestValidateTLSPeer_NoVerifiedChain(t *testing.T) {
	leaf := mintLeafCert(t, "anyone", []string{"anyone.example"}, nil)
	err := validateTLSPeer(ctxWithUnverifiedPeer(leaf), nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Errorf("expected code Unauthenticated, got %s", got)
	}
}

// TestValidateTLSPeer_EmptyAllowList_Accepts: ADR-0003 decision #5 — empty
// AllowedSANs means "trust any cert the TLS stack validated against the CA".
func TestValidateTLSPeer_EmptyAllowList_Accepts(t *testing.T) {
	leaf := mintLeafCert(t, "virtrigaud-manager",
		[]string{"virtrigaud-manager.svc"}, nil)
	if err := validateTLSPeer(ctxWithVerifiedPeer(leaf), nil); err != nil {
		t.Errorf("expected accept with empty allow-list, got %v", err)
	}
	// Also accept the explicit empty slice (different from nil but equivalent).
	if err := validateTLSPeer(ctxWithVerifiedPeer(leaf), []string{}); err != nil {
		t.Errorf("expected accept with []string{}, got %v", err)
	}
}

// TestValidateTLSPeer_DNSSANMatch: the most common operational case —
// SAN allow-list pinned to the manager's DNS name.
func TestValidateTLSPeer_DNSSANMatch(t *testing.T) {
	leaf := mintLeafCert(t, "virtrigaud-manager",
		[]string{"virtrigaud-manager.virtrigaud-system.svc"}, nil)
	allowed := []string{"virtrigaud-manager.virtrigaud-system.svc"}
	if err := validateTLSPeer(ctxWithVerifiedPeer(leaf), allowed); err != nil {
		t.Errorf("expected accept for matching DNS SAN, got %v", err)
	}
}

// TestValidateTLSPeer_DNSSANMatch_MultipleAllowed: allow-list with multiple
// entries — only one needs to match.
func TestValidateTLSPeer_DNSSANMatch_MultipleAllowed(t *testing.T) {
	leaf := mintLeafCert(t, "manager", []string{"manager.alt.svc"}, nil)
	allowed := []string{"manager.svc", "manager.alt.svc", "manager.dev.svc"}
	if err := validateTLSPeer(ctxWithVerifiedPeer(leaf), allowed); err != nil {
		t.Errorf("expected accept for one-of-many DNS SAN match, got %v", err)
	}
}

// TestValidateTLSPeer_URISANMatch: SPIFFE-style URI SANs are honored.
func TestValidateTLSPeer_URISANMatch(t *testing.T) {
	leaf := mintLeafCert(t, "spiffe-id",
		nil,
		[]string{"spiffe://virtrigaud.local/manager"},
	)
	allowed := []string{"spiffe://virtrigaud.local/manager"}
	if err := validateTLSPeer(ctxWithVerifiedPeer(leaf), allowed); err != nil {
		t.Errorf("expected accept for matching URI SAN, got %v", err)
	}
}

// TestValidateTLSPeer_CNFallback: legacy cert with only a CN (no SANs). CN
// fallback per ADR-0003 spec lets it through.
func TestValidateTLSPeer_CNFallback(t *testing.T) {
	leaf := mintLeafCert(t, "virtrigaud-manager", nil, nil)
	allowed := []string{"virtrigaud-manager"}
	if err := validateTLSPeer(ctxWithVerifiedPeer(leaf), allowed); err != nil {
		t.Errorf("expected accept via CN fallback, got %v", err)
	}
}

// TestValidateTLSPeer_AllowListMiss: cert is validly chained but its identity
// fields don't match any allow-list entry — reject with PermissionDenied.
func TestValidateTLSPeer_AllowListMiss(t *testing.T) {
	leaf := mintLeafCert(t, "rogue", []string{"rogue.svc"}, nil)
	allowed := []string{"virtrigaud-manager.svc", "virtrigaud-manager"}
	err := validateTLSPeer(ctxWithVerifiedPeer(leaf), allowed)
	if err == nil {
		t.Fatal("expected reject, got nil")
	}
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Errorf("expected code PermissionDenied, got %s", got)
	}
}

// TestAuthenticateRequest_RequireTLSFalse_NoOp: when Auth.RequireTLS is false
// authenticateRequest does no TLS check at all — even an unverified context
// passes through.
func TestAuthenticateRequest_RequireTLSFalse_NoOp(t *testing.T) {
	cfg := &AuthConfig{RequireTLS: false}
	if err := authenticateRequest(ctxWithPlaintextPeer(), cfg); err != nil {
		t.Errorf("expected pass-through with RequireTLS=false, got %v", err)
	}
	// Also a context with no peer at all.
	if err := authenticateRequest(context.Background(), cfg); err != nil {
		t.Errorf("expected pass-through with RequireTLS=false and no peer, got %v", err)
	}
}

// TestAuthenticateRequest_PropagatesUnauthenticated ensures the status code
// from validateTLSPeer flows through authenticateRequest unchanged — a key
// regression check, since the previous implementation wrapped *every* TLS
// failure as PermissionDenied.
func TestAuthenticateRequest_PropagatesUnauthenticated(t *testing.T) {
	cfg := &AuthConfig{RequireTLS: true}
	err := authenticateRequest(ctxWithPlaintextPeer(), cfg)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated to propagate through authenticateRequest, got %s", got)
	}
}

// TestAuthenticateRequest_PropagatesPermissionDenied: identity mismatch must
// surface as PermissionDenied to the caller, not as a generic auth failure.
func TestAuthenticateRequest_PropagatesPermissionDenied(t *testing.T) {
	leaf := mintLeafCert(t, "rogue", []string{"rogue.svc"}, nil)
	cfg := &AuthConfig{
		RequireTLS:  true,
		AllowedSANs: []string{"virtrigaud-manager.svc"},
	}
	err := authenticateRequest(ctxWithVerifiedPeer(leaf), cfg)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Errorf("expected PermissionDenied to propagate, got %s", got)
	}
}
