//go:build virtrigaud_dev

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

package daemon_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/daemon"
	"github.com/projectbeskar/virtrigaud/internal/agent/enrol"
	"github.com/projectbeskar/virtrigaud/internal/agent/signer"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// renewalAge is how far the clock is moved so the host certificate (30 days)
// is past two thirds of its lifetime.
const renewalAge = 21 * 24 * time.Hour

// renewalHost is a host whose signer serves renewal from a key file in a
// private directory, with the clock past the renewal time.
func renewalHost(t *testing.T) (*host, string) {
	t.Helper()
	h := newHost(t)
	keyDir := filepath.Join(t.TempDir(), "signer-keys")
	if err := os.Mkdir(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(keyDir, trust.FileHostKey)
	writeKey(t, keyFile, h.pki.HostKey)
	socket := h.signer.socket
	h.signer.stop()
	h.signer = startRenewalSigner(t, socket, h.pki.HostKey, keyFile)
	h.nowOff.Store(int64(renewalAge))
	return h, keyFile
}

// writeKey writes key as a private PEM file.
func writeKey(t *testing.T, path string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// startRenewalSigner serves the key in keyFile with renewal on.
func startRenewalSigner(t *testing.T, socket string, key *ecdsa.PrivateKey, keyFile string) *testSigner {
	t.Helper()
	if k, err := signer.LoadKey(keyFile, testUID); err == nil {
		key = k
	}
	srv, err := signer.NewServer(signer.ServerOptions{Key: key, AllowedUID: testUID, Log: logr.Discard(),
		Renewal: true, KeyFile: keyFile, OwnerUID: testUID, RenewalBurst: 100})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := signer.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &testSigner{socket: socket, srv: srv, cancel: cancel, done: make(chan error, 1)}
	go func() { s.done <- srv.Serve(ctx, lis) }()
	t.Cleanup(s.stop)
	return s
}

// pkiPath is a file of the host's key material directory.
func (h *host) pkiPath(name string) string { return filepath.Join(h.cfg.PKIDir(), name) }

// waitFor polls cond until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// renewalConns caches one connection per run: the agent throttles handshakes
// per source.
var renewalConns sync.Map

// rconn returns the run's cached connection.
func (r *running) rconn() *grpc.ClientConn {
	if v, ok := renewalConns.Load(r); ok {
		if cc, ok := v.(*grpc.ClientConn); ok {
			return cc
		}
	}
	cc := r.conn()
	renewalConns.Store(r, cc)
	return cc
}

// renewalAnswer asks GetHostInfo for the pending renewal (renewal version
// requested) and returns the signed host answer blob and its body.
func (r *running) renewalAnswer(requested uint32) (*providerv1.SignedBlob, *providerv1.HostAnswer) {
	r.h.t.Helper()
	info, err := providerv1.NewProviderClient(r.rconn()).GetHostInfo(callCtx(r.h.t),
		&providerv1.GetHostInfoRequest{AnswerNonce: bytes.Repeat([]byte{5}, 16), RenewalVersion: requested})
	if err != nil {
		r.h.t.Fatalf("GetHostInfo: %v", err)
	}
	blob := info.GetHostAnswer()
	if blob == nil {
		r.h.t.Fatalf("no signed answer\n%s", r.h.logs.text())
	}
	a := &providerv1.HostAnswer{}
	if err := proto.Unmarshal(blob.GetBody(), a); err != nil {
		r.h.t.Fatal(err)
	}
	return blob, a
}

// install calls InstallRenewedCertificate.
func (r *running) install(der []byte, activate bool) (*providerv1.SignedBlob, error) {
	r.h.t.Helper()
	resp, err := agentv1.NewAgentServiceClient(r.rconn()).InstallRenewedCertificate(callCtx(r.h.t),
		&agentv1.InstallRenewedCertificateRequest{AnswerNonce: bytes.Repeat([]byte{6}, 16), Certificate: der, Activate: activate})
	return resp.GetHostAnswer(), err
}

// issueRenewal waits for the host's pending renewal, verifies it as the
// manager does, and issues a certificate for it from the host CA.
func issueRenewal(t *testing.T, r *running, h *host) ([]byte, *ecdsa.PublicKey) {
	t.Helper()
	waitFor(t, "the renewal request", func() bool { return exists(t, h.pkiPath(enrol.FileRenewalCSR)) })
	blob, a := r.renewalAnswer(hostidentity.RenewalVersion)
	if a.GetPendingRenewal() == nil {
		t.Fatalf("the answer carries no pending renewal\n%s", h.logs.text())
	}
	block, _ := pem.Decode(h.pki.HostCertPEM)
	if !bytes.Equal(a.GetPendingRenewal().GetSignerCertificate(), blob.GetSignerCertificate()) ||
		!bytes.Equal(blob.GetSignerCertificate(), block.Bytes) {
		t.Fatal("the renewal request is not signed with the current certificate")
	}
	pin, _ := hostidentity.SPKIPin(&h.pki.HostKey.PublicKey)
	v, err := hostidentity.VerifyRenewal(a.GetPendingRenewal(), hostidentity.RenewalExpectation{
		Anchors: h.pki.HostCAPool(), HostURI: h.pki.HostURI, HostName: trusttest.HostName, HostID: trusttest.HostName,
		Pins: []string{pin}, ProviderUID: trusttest.ProviderUID, InstallID: h.pki.Record.InstallID,
		EnrolmentID: h.pki.Record.EnrolmentID, Version: 1, Now: time.Now().Add(renewalAge),
	})
	if err != nil {
		t.Fatalf("the renewal request does not verify: %v", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: v.CSR})
	certPEM := h.pki.HostCA.Issue(t, csrPEM, hostidentitytest.IssueOptions{})
	cb, _ := pem.Decode(certPEM)
	csr, _ := x509.ParseCertificateRequest(v.CSR)
	return cb.Bytes, ecPub(t, csr.PublicKey)
}

// signerOf parses a host answer's signer certificate key.
func signerOf(t *testing.T, blob *providerv1.SignedBlob) *ecdsa.PublicKey {
	t.Helper()
	c, err := x509.ParseCertificate(blob.GetSignerCertificate())
	if err != nil {
		t.Fatal(err)
	}
	return ecPub(t, c.PublicKey)
}

// TestRenewalEndToEnd: past two thirds of its lifetime the agent asks its
// signer for a new key's request and offers it, co-signed, only to a
// GetHostInfo that asks for it; a certificate that certifies it is staged
// (the answer names it), re-staging is idempotent, activation of another
// certificate is refused, and activation of the staged one puts the new key
// in use for answers and TLS at once.
func TestRenewalEndToEnd(t *testing.T) {
	h, keyFile := renewalHost(t)
	r, _ := h.run()
	der, newPub := issueRenewal(t, r, h)
	// The renewed certificate is not due for renewal at the real clock.
	h.nowOff.Store(0)

	// Not asked for: no pending renewal in the answer.
	if _, a := r.renewalAnswer(0); a.GetPendingRenewal() != nil {
		t.Fatal("a pending renewal in an answer that did not ask for it")
	}
	// Activation before staging is refused.
	if _, err := r.install(der, true); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("activation before staging: %v", err)
	}
	blob, err := r.install(der, false)
	if err != nil {
		t.Fatalf("stage: %v\n%s", err, h.logs.text())
	}
	sum := sha256.Sum256(der)
	a := &providerv1.HostAnswer{}
	_ = proto.Unmarshal(blob.GetBody(), a)
	if !bytes.Equal(a.GetStagedCertificateSha256(), sum[:]) {
		t.Fatal("the stage answer does not name the staged certificate")
	}
	if _, ga := r.renewalAnswer(1); !bytes.Equal(ga.GetStagedCertificateSha256(), sum[:]) {
		t.Fatal("GetHostInfo does not name the staged certificate")
	}
	if _, err := r.install(der, false); err != nil {
		t.Fatalf("re-stage: %v", err)
	}
	// Another certificate for the same key: activation needs the staged one,
	// byte for byte.
	other := h.pki.HostCA.Issue(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST",
		Bytes: mustCSR(t, h)}), hostidentitytest.IssueOptions{Lifetime: 29 * 24 * time.Hour})
	ob, _ := pem.Decode(other)
	if _, err := r.install(ob.Bytes, true); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("activation of a certificate that is not the staged one: %v", err)
	}

	blob, err = r.install(der, true)
	if err != nil {
		t.Fatalf("activate: %v\n%s", err, h.logs.text())
	}
	if !signerOf(t, blob).Equal(newPub) {
		t.Fatal("the activation answer is not signed with the new key")
	}
	_ = proto.Unmarshal(blob.GetBody(), a)
	if len(a.GetStagedCertificateSha256()) != 0 {
		t.Fatal("the activation answer still names a staged certificate")
	}
	// Files: the host certificate is the renewed one, the renewal files are
	// gone, the signer's key file holds the new key.
	b, err := os.ReadFile(h.pkiPath(trust.FileHostCert))
	if err != nil {
		t.Fatal(err)
	}
	if pb, _ := pem.Decode(b); pb == nil || !bytes.Equal(pb.Bytes, der) {
		t.Fatal("host.crt is not the renewed certificate")
	}
	if exists(t, h.pkiPath(enrol.FileRenewalCSR)) || exists(t, h.pkiPath(enrol.FileRenewalCert)) {
		t.Fatal("renewal files are left")
	}
	if k, err := signer.LoadKey(keyFile, testUID); err != nil || !k.PublicKey.Equal(newPub) {
		t.Fatalf("the signer's key file does not hold the new key: %v", err)
	}
	// A new connection sees the new certificate in TLS; answers are signed by
	// the new key; re-activation is idempotent; no renewal is pending.
	blob, ga := r.renewalAnswer(1)
	if !signerOf(t, blob).Equal(newPub) || ga.GetPendingRenewal() != nil || len(ga.GetStagedCertificateSha256()) != 0 {
		t.Fatal("after activation the answer is not by the new key, or names a renewal")
	}
	if _, err := r.install(der, true); err != nil {
		t.Fatalf("re-activation: %v", err)
	}
	if _, err := r.install(der, false); err != nil {
		t.Fatalf("a late stage of the certificate in use: %v", err)
	}
	var p peer.Peer
	if _, err := agentv1.NewAgentServiceClient(r.conn()).Hello(callCtx(t), &agentv1.HelloRequest{AnswerNonce: bytes.Repeat([]byte{7}, 16),
		CallerApiVersion: 1}, grpc.Peer(&p)); err != nil {
		t.Fatalf("Hello on a new connection: %v", err)
	}
	if ti, ok := p.AuthInfo.(credentials.TLSInfo); !ok || len(ti.State.PeerCertificates) == 0 ||
		!bytes.Equal(ti.State.PeerCertificates[0].Raw, der) {
		t.Fatal("a new connection does not see the renewed certificate")
	}
	if !bytes.Contains([]byte(h.logs.text()), []byte("renewed certificate in use")) {
		t.Fatal("no audit line for the activation")
	}
	if bytes.Contains([]byte(h.logs.text()), []byte("PRIVATE KEY")) || bytes.Contains([]byte(h.logs.text()), []byte("BEGIN CERTIFICATE")) {
		t.Fatal("key or certificate material in the logs")
	}
}

// mustCSR returns the host's pending request (DER).
func mustCSR(t *testing.T, h *host) []byte {
	t.Helper()
	p, err := enrol.LoadPending(h.cfg.PKIDir(), trust.KeyFileReader(testUID))
	if err != nil || p.CSR == nil {
		t.Fatalf("no pending request: %v", err)
	}
	return p.CSR.Raw
}

// TestRenewalStageRefusals: a certificate that does not chain to the pinned
// issuer, that changes an identity (another enrolment, another epoch), that
// certifies another key, or that breaks the profile is never staged.
func TestRenewalStageRefusals(t *testing.T) {
	h, _ := renewalHost(t)
	r, _ := h.run()
	issueRenewal(t, r, h)
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: mustCSR(t, h)})
	otherRec := h.pki.Record
	otherRec.EnrolmentID = bytes.Repeat([]byte{0x55}, 16)
	epochRec := h.pki.Record
	epochRec.StartEpoch = 7
	otherKey := hostidentitytest.NewKey(t)
	foreignCA := hostidentitytest.NewCA(t)
	cases := map[string][]byte{
		"another CA":           foreignCA.Issue(t, csrPEM, hostidentitytest.IssueOptions{}),
		"another enrolment":    h.pki.HostCA.Issue(t, csrPEM, hostidentitytest.IssueOptions{URIs: hostidentity.RequestURIs(h.pki.HostURI, otherRec)}),
		"another epoch":        h.pki.HostCA.Issue(t, csrPEM, hostidentitytest.IssueOptions{URIs: hostidentity.RequestURIs(h.pki.HostURI, epochRec)}),
		"another key":          h.pki.HostCA.Issue(t, csrPEM, hostidentitytest.IssueOptions{PublicKey: &otherKey.PublicKey}),
		"the current key":      h.pki.HostCA.Issue(t, csrPEM, hostidentitytest.IssueOptions{PublicKey: &h.pki.HostKey.PublicKey}),
		"client auth usage":    h.pki.HostCA.Issue(t, csrPEM, hostidentitytest.IssueOptions{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}),
		"lifetime too long":    h.pki.HostCA.Issue(t, csrPEM, hostidentitytest.IssueOptions{Lifetime: 60 * 24 * time.Hour}),
		"expired by the clock": h.pki.HostCA.Issue(t, csrPEM, hostidentitytest.IssueOptions{Lifetime: 24 * time.Hour}),
	}
	for name, certPEM := range cases {
		b, _ := pem.Decode(certPEM)
		if _, err := r.install(b.Bytes, false); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: stage = %v, want InvalidArgument", name, err)
		}
	}
	if exists(t, h.pkiPath(enrol.FileRenewalCert)) {
		t.Fatal("a refused certificate was staged")
	}
	if _, a := r.renewalAnswer(1); len(a.GetStagedCertificateSha256()) != 0 {
		t.Fatal("an answer names a staged certificate after refusals")
	}
	if _, err := r.install([]byte{0x30, 0x01}, false); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("garbage: %v", err)
	}
}

// TestRenewalCrashBetweenStageAndActivate (crash point: the certificate is
// staged, not yet in use): after a restart the current certificate and key
// are still in use, the staged certificate is still named, and the
// activation then completes.
func TestRenewalCrashBetweenStageAndActivate(t *testing.T) {
	h, _ := renewalHost(t)
	r, _ := h.run()
	der, newPub := issueRenewal(t, r, h)
	if _, err := r.install(der, false); err != nil {
		t.Fatal(err)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	h.nowOff.Store(0)
	r, _ = h.run()
	blob, a := r.renewalAnswer(1)
	sum := sha256.Sum256(der)
	if signerOf(t, blob).Equal(newPub) || !bytes.Equal(a.GetStagedCertificateSha256(), sum[:]) {
		t.Fatal("after a restart the staged renewal is not as it was")
	}
	if _, err := r.install(der, true); err != nil {
		t.Fatalf("activate after the restart: %v\n%s", err, h.logs.text())
	}
	if blob, _ := r.renewalAnswer(1); !signerOf(t, blob).Equal(newPub) {
		t.Fatal("not activated")
	}
}

// TestRenewalCrashMidActivation (crash point: the signer uses the new key, the
// host certificate is still the old one): the agent cannot sign with the old
// certificate any more, and its restart finishes the renewal from the staged
// certificate, so the host is left with a usable pair.
func TestRenewalCrashMidActivation(t *testing.T) {
	for _, step := range []string{daemon.RenewalStepKeySwitched, daemon.RenewalStepPromoted} {
		t.Run(step, func(t *testing.T) {
			h, _ := renewalHost(t)
			h.hooks.RenewalStep = func(s string) error {
				if s == step {
					return errors.New("crash")
				}
				return nil
			}
			r, _ := h.run()
			der, newPub := issueRenewal(t, r, h)
			if _, err := r.install(der, false); err != nil {
				t.Fatal(err)
			}
			if _, err := r.install(der, true); status.Code(err) != codes.Unavailable {
				t.Fatalf("activation stopped at %s: %v", step, err)
			}
			_ = r.stop()
			h.hooks.RenewalStep = nil
			h.nowOff.Store(0)
			r, _ = h.run()
			blob, a := r.renewalAnswer(1)
			if !signerOf(t, blob).Equal(newPub) || len(a.GetStagedCertificateSha256()) != 0 || a.GetPendingRenewal() != nil {
				t.Fatalf("after the restart the renewed pair is not in use\n%s", h.logs.text())
			}
			if exists(t, h.pkiPath(enrol.FileRenewalCSR)) || exists(t, h.pkiPath(enrol.FileRenewalCert)) {
				t.Fatal("renewal files are left")
			}
			// Re-activation (the Provider's retry) succeeds.
			if _, err := r.install(der, true); err != nil {
				t.Fatalf("re-activation after the restart: %v", err)
			}
		})
	}
}

// TestRenewalDiscardedAfterReenrolment: a pending renewal of an earlier
// enrolment is discarded once the host certificate names a new one.
func TestRenewalDiscardedAfterReenrolment(t *testing.T) {
	h, keyFile := renewalHost(t)
	r, _ := h.run()
	waitFor(t, "the renewal request", func() bool { return exists(t, h.pkiPath(enrol.FileRenewalCSR)) })
	if !exists(t, signer.PendingKeyFile(keyFile)) {
		t.Fatal("no pending key")
	}
	_ = r.stop()
	// Re-enrol: a new certificate for a new enrolment, same key.
	rec := h.pki.Record
	rec.EnrolmentID = bytes.Repeat([]byte{0x66}, 16)
	rec.StartEpoch = 3
	certPEM := h.pki.IssueHost(t, h.pki.HostKey, hostidentity.RequestURIs(h.pki.HostURI, rec), hostidentitytest.IssueOptions{})
	if err := os.WriteFile(h.pkiPath(trust.FileHostCert), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	h.nowOff.Store(0)
	_, _ = h.run()
	waitFor(t, "the discard", func() bool { return !exists(t, h.pkiPath(enrol.FileRenewalCSR)) })
	if exists(t, signer.PendingKeyFile(keyFile)) {
		t.Fatal("the signer's pending key was not discarded")
	}
}

var _ = config.DevBuild

// ecPub returns k as an ECDSA public key, failing the test otherwise.
func ecPub(t testing.TB, k any) *ecdsa.PublicKey {
	t.Helper()
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("not an ECDSA public key: %T", k)
	}
	return pub
}

// TestRenewalActivationHealsInProcess: an activation that fails after the
// signer switched keys (at each point after the switch) leaves the agent
// able to sign again without a restart: the next tick, or the Provider's
// next activation, brings the credentials to the new key.
func TestRenewalActivationHealsInProcess(t *testing.T) {
	for _, c := range []struct {
		step  string
		retry bool
	}{
		{daemon.RenewalStepKeySwitched, false}, {daemon.RenewalStepPromoted, false},
		{daemon.RenewalStepKeySwitched, true}, {daemon.RenewalStepPromoted, true},
	} {
		name := c.step + "/tick"
		if c.retry {
			name = c.step + "/retry"
		}
		t.Run(name, func(t *testing.T) {
			h, _ := renewalHost(t)
			var failed atomic.Bool
			h.hooks.RenewalStep = func(s string) error {
				if s == c.step && failed.CompareAndSwap(false, true) {
					return errors.New("fail once")
				}
				return nil
			}
			r, _ := h.run()
			der, newPub := issueRenewal(t, r, h)
			h.nowOff.Store(0)
			if _, err := r.install(der, false); err != nil {
				t.Fatal(err)
			}
			if _, err := r.install(der, true); status.Code(err) != codes.Unavailable {
				t.Fatalf("activation failing at %s: %v", c.step, err)
			}
			if c.retry {
				if _, err := r.install(der, true); err != nil {
					t.Fatalf("the Provider's retry does not finish the activation: %v\n%s", err, h.logs.text())
				}
			}
			waitFor(t, "answers by the new key", func() bool {
				blob := r.tryAnswer()
				return blob != nil && signerOf(t, blob).Equal(newPub)
			})
			if exists(t, h.pkiPath(enrol.FileRenewalCSR)) || exists(t, h.pkiPath(enrol.FileRenewalCert)) {
				t.Fatal("renewal files are left")
			}
		})
	}
}

// TestRenewalActivationSignerAway: the signer becomes unreachable right after
// it switched keys; the activation gives up on its own deadline, and once
// the signer is back the agent heals in process.
func TestRenewalActivationSignerAway(t *testing.T) {
	daemon.SetActivationTimeout(t, 500*time.Millisecond)
	h, keyFile := renewalHost(t)
	socket := h.signer.socket
	h.hooks.RenewalStep = func(s string) error {
		if s == daemon.RenewalStepKeySwitched {
			h.signer.stop()
		}
		return nil
	}
	r, _ := h.run()
	der, newPub := issueRenewal(t, r, h)
	h.nowOff.Store(0)
	if _, err := r.install(der, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.install(der, true); status.Code(err) != codes.Unavailable {
		t.Fatalf("activation with the signer away: %v", err)
	}
	h.signer = startRenewalSigner(t, socket, newKeyFrom(t, keyFile), keyFile)
	waitFor(t, "answers by the new key", func() bool {
		blob := r.tryAnswer()
		return blob != nil && signerOf(t, blob).Equal(newPub)
	})
}

// newKeyFrom loads the key now in keyFile.
func newKeyFrom(t *testing.T, keyFile string) *ecdsa.PrivateKey {
	t.Helper()
	k, err := signer.LoadKey(keyFile, testUID)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestRenewalRedrawnWhenUnanswered: a request never answered (nothing
// staged) within half the lifetime left when it was made is discarded with
// its pending key, and a request for a new key is made.
func TestRenewalRedrawnWhenUnanswered(t *testing.T) {
	h, keyFile := renewalHost(t)
	r, _ := h.run()
	waitFor(t, "the renewal request", func() bool { return exists(t, h.pkiPath(enrol.FileRenewalCSR)) })
	first := mustCSR(t, h)
	h.nowOff.Store(int64(26 * 24 * time.Hour))
	waitFor(t, "a new request", func() bool {
		p, err := enrol.LoadPending(h.cfg.PKIDir(), trust.KeyFileReader(testUID))
		return err == nil && p.CSR != nil && !bytes.Equal(p.CSR.Raw, first)
	})
	p, _ := enrol.LoadPending(h.cfg.PKIDir(), trust.KeyFileReader(testUID))
	old, _ := x509.ParseCertificateRequest(first)
	if ecPub(t, p.CSR.PublicKey).Equal(ecPub(t, old.PublicKey)) {
		t.Fatal("the new request is for the same key")
	}
	if !exists(t, signer.PendingKeyFile(keyFile)) {
		t.Fatal("no pending key for the new request")
	}
	_ = r
}

// tryAnswer asks GetHostInfo for a signed answer and returns it, or nil when
// there is none (yet).
func (r *running) tryAnswer() *providerv1.SignedBlob {
	info, err := providerv1.NewProviderClient(r.rconn()).GetHostInfo(callCtx(r.h.t),
		&providerv1.GetHostInfoRequest{AnswerNonce: bytes.Repeat([]byte{8}, 16)})
	if err != nil {
		return nil
	}
	return info.GetHostAnswer()
}
