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

package daemon

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/enrol"
	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	"github.com/projectbeskar/virtrigaud/internal/agent/signer"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Certificate renewal (development builds only). See package enrol for the
// files and the crash safety, package signer for the pending key.

// Retry bounds of a failed renewal request to the signer.
const (
	renewalRetryMin = 30 * time.Second
	renewalRetryMax = 10 * time.Minute
)

// Renewal steps a test can stop the activation at, as a crash would
// (Hooks.RenewalStep): after the signer put the new key in use, and after
// the staged certificate became the host certificate.
const (
	// RenewalStepKeySwitched follows the signer's switch to the new key.
	RenewalStepKeySwitched = "key-switched"
	// RenewalStepPromoted follows the promotion of the staged certificate.
	RenewalStepPromoted = "promoted"
)

// errRenewalStopped is returned when a test hook stops an activation.
var errRenewalStopped = errors.New("the renewal was stopped by a test hook")

// renewer is the agent's certificate renewal: the renewal timer, the
// pending request, staging and activation. Its lock also serialises the
// daemon's reloads with an activation, so credentials are replaced by one of
// them at a time.
type renewer struct {
	d *daemon
	// run is the daemon's run context: the lifetime of the signer clients it
	// connects.
	run context.Context //nolint:containedctx // the lifetime of the signer clients, like signer.ClientOptions.Lifetime

	mu      sync.Mutex
	pending enrol.Pending
	// signed is the co-signed pending request, for signedFor (the digest of
	// the current certificate and the request).
	signed    *providerv1.SignedBlob
	signedFor [32]byte
	// nextTry and failures pace failed requests to the signer.
	nextTry  time.Time
	failures int
}

// newRenewer returns the renewer of d with its pending renewal read from
// the key material directory; a malformed renewal file is discarded.
func newRenewer(ctx context.Context, d *daemon) *renewer {
	r := &renewer{d: d, run: ctx}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadLocked()
	return r
}

// reader is the strict reader of the agent's key material files.
func (r *renewer) reader() func(string) ([]byte, error) { return trust.KeyFileReader(r.d.uid) }

// loadLocked reads the pending renewal; a file that does not load is
// discarded (the renewal starts again).
func (r *renewer) loadLocked() {
	p, err := enrol.LoadPending(r.d.o.Config.PKIDir(), r.reader())
	if err != nil {
		r.d.log.Error(err, "a renewal file does not load; the pending renewal is discarded")
		if derr := enrol.DiscardPending(r.d.o.Config.PKIDir()); derr != nil {
			r.d.log.Error(derr, "discard the renewal files")
		}
		p = enrol.Pending{}
	}
	r.pending, r.signed = p, nil
}

// exclusive runs f under the renewer's lock (nil renewer: f alone).
func (r *renewer) exclusive(f func() error) error {
	if r == nil {
		return f()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	err := f()
	r.loadLocked()
	return err
}

// tick runs the renewal timer: it discards a pending renewal that belongs
// to an earlier certificate (a re-enrolment replaced it), and asks the
// signer for a renewal request once the current certificate is two thirds
// through its lifetime and none is pending.
func (r *renewer) tick(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.d.creds.Load().Identity
	now := r.d.o.Now()
	if r.pending.CSR != nil && !r.pending.MatchesCurrent(cur.Leaf) {
		r.discardLocked(ctx, "the pending renewal belongs to an earlier certificate")
	}
	if r.pending.CSR != nil || !enrol.RenewalDue(cur.Leaf, now) || now.Before(r.nextTry) || !now.Before(cur.Leaf.NotAfter) {
		return
	}
	if err := r.requestLocked(ctx, cur); err != nil {
		wait := min(renewalRetryMin<<min(r.failures, 8), renewalRetryMax)
		r.failures++
		r.nextTry = now.Add(wait)
		r.d.log.Error(err, "certificate renewal request failed; it is asked again later", "retryIn", wait.String())
		return
	}
	r.failures, r.nextTry = 0, time.Time{}
	r.d.log.Info("certificate renewal requested: a new key is pending, its request is offered to the manager",
		"enrolment", hex.EncodeToString(cur.Record.EnrolmentID), "newKeyPin", r.pending.KeyPin,
		"renewAt", enrol.RenewalAt(cur.Leaf).UTC().Format(time.RFC3339), "notAfter", cur.Leaf.NotAfter.UTC().Format(time.RFC3339))
}

// requestLocked asks the signer for a request for its pending key carrying
// the current certificate's URIs and keeps it.
func (r *renewer) requestLocked(ctx context.Context, cur *trust.Identity) error {
	c, err := signer.Connect(ctx, r.d.signerOptions(r.run))
	if err != nil {
		return fmt.Errorf("reach the signer: %w", err)
	}
	uris := make([]string, 0, len(cur.Leaf.URIs))
	for _, u := range cur.Leaf.URIs {
		uris = append(uris, u.String())
	}
	der, err := c.RenewalRequest(ctx, uris)
	if err != nil {
		return err
	}
	if err := enrol.SavePendingCSR(r.d.o.Config.PKIDir(), der, cur.Leaf); err != nil {
		return err
	}
	r.loadLocked()
	if r.pending.CSR == nil {
		return errors.New("the renewal request was not kept")
	}
	return nil
}

// discardLocked removes the pending renewal: the signer's pending key and the
// agent's files.
func (r *renewer) discardLocked(ctx context.Context, why string) {
	if pin, err := hex.DecodeString(r.pending.KeyPin); err == nil {
		if c, err := signer.Connect(ctx, r.d.signerOptions(r.run)); err == nil {
			if err := c.DiscardRenewal(ctx, pin); err != nil {
				r.d.log.Error(err, "discard the signer's pending renewal key")
			}
		}
	}
	if err := enrol.DiscardPending(r.d.o.Config.PKIDir()); err != nil {
		r.d.log.Error(err, "discard the renewal files")
	}
	r.d.log.Info("pending certificate renewal discarded", "why", why, "keyPin", r.pending.KeyPin)
	r.loadLocked()
}

// PendingRequest implements server.Renewals: the pending renewal request of
// the current certificate, co-signed by the current key under the
// host-signed domain separator; nil when there is none or it cannot be
// signed now. The signed request is kept and reused while the certificate
// and the request do not change.
func (r *renewer) PendingRequest(_ context.Context, requested uint32) *providerv1.SignedBlob {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.d.creds.Load().Identity
	if requested == 0 || r.pending.CSR == nil || !r.pending.MatchesCurrent(cur.Leaf) {
		return nil
	}
	key := sha256.Sum256(append(bytes.Clone(cur.Leaf.Raw), r.pending.CSR.Raw...))
	if r.signed != nil && key == r.signedFor {
		return proto.CloneOf(r.signed)
	}
	body, err := answer.RenewalRequest(answer.Renewal{Requested: requested, HostID: cur.HostID(), Record: cur.Record,
		CertificateDER: cur.Leaf.Raw, CSRDER: r.pending.CSR.Raw})
	if err != nil {
		r.d.log.Error(err, "build the renewal request")
		return nil
	}
	blob, err := answer.SignRenewalRequest(cur.Signer, cur.Leaf.Raw, body)
	if err != nil {
		r.d.log.Error(err, "sign the renewal request; the answer carries none")
		return nil
	}
	r.signed, r.signedFor = blob, key
	return proto.CloneOf(blob)
}

// StagedSHA256 implements server.Renewals.
func (r *renewer) StagedSHA256() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending.Staged == nil {
		return nil
	}
	sum := sha256.Sum256(r.pending.Staged.Raw)
	return sum[:]
}

// Install implements server.Renewals.
//
// Staging (activate false): a certificate already in use succeeds and
// changes nothing; otherwise it needs a pending renewal and must pass
// enrol.CheckRenewed; it then replaces any staged certificate.
//
// Activation (activate true): a certificate already in use succeeds and
// changes nothing; otherwise it must be the staged certificate byte for byte
// and pass enrol.CheckRenewed again. The signer puts the new key in use, the
// staged certificate becomes the host certificate, and the credentials (TLS
// certificate and answer signer) are replaced at once by the new pair.
func (r *renewer) Install(ctx context.Context, der []byte, activate bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.d.creds.Load()
	if bytes.Equal(der, cur.Identity.Leaf.Raw) {
		return nil
	}
	sum := sha256.Sum256(der)
	digest := hex.EncodeToString(sum[:])
	if r.pending.CSR == nil {
		return status.Error(codes.FailedPrecondition, "no certificate renewal is pending on this host")
	}
	if activate {
		if (r.pending.Staged == nil || !bytes.Equal(r.pending.Staged.Raw, der)) && !guard.Off(guard.RenewActivateStaged) {
			return status.Error(codes.FailedPrecondition, "the certificate is not the staged certificate")
		}
	}
	leaf, err := enrol.CheckRenewed(der, cur.Identity, r.pending, cur.Trust, r.d.o.Now())
	if err != nil {
		r.d.log.Info("renewed certificate refused", "sha256", digest, "reason", err.Error())
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if !activate {
		if r.pending.Staged != nil && bytes.Equal(r.pending.Staged.Raw, der) {
			return nil
		}
		if err := enrol.Stage(r.d.o.Config.PKIDir(), der); err != nil {
			r.d.log.Error(err, "stage the renewed certificate")
			return status.Error(codes.Internal, "the renewed certificate could not be staged")
		}
		r.loadLocked()
		r.d.log.Info("renewed certificate staged", "sha256", digest, "newKeyPin", r.pending.KeyPin,
			"notAfter", leaf.NotAfter.UTC().Format(time.RFC3339), "enrolment", hex.EncodeToString(cur.Identity.Record.EnrolmentID))
		return nil
	}
	if err := r.activateLocked(ctx, cur, leaf); err != nil {
		if errors.Is(err, errRenewalStopped) {
			return status.Error(codes.Unavailable, err.Error())
		}
		r.d.log.Error(err, "the renewed certificate could not be put in use; the running credentials stay", "sha256", digest)
		return status.Error(codes.Unavailable, "the renewed certificate could not be put in use")
	}
	return nil
}

// activateLocked puts the staged certificate leaf and the signer's pending
// key in use and replaces the credentials.
func (r *renewer) activateLocked(ctx context.Context, cur *server.Credentials, leaf *x509.Certificate) error {
	pin, err := keyPinBytes(leaf.PublicKey)
	if err != nil {
		return err
	}
	c, err := signer.Connect(ctx, r.d.signerOptions(r.run))
	if err != nil {
		return fmt.Errorf("reach the signer: %w", err)
	}
	if _, err := c.ActivateRenewal(ctx, pin); err != nil {
		return fmt.Errorf("the signer did not put the new key in use: %w", err)
	}
	if err := r.step(RenewalStepKeySwitched); err != nil {
		return err
	}
	if err := enrol.Promote(r.d.o.Config.PKIDir(), r.reader()); err != nil {
		return fmt.Errorf("make the renewed certificate the host certificate: %w", err)
	}
	if err := r.step(RenewalStepPromoted); err != nil {
		return err
	}
	nc, err := signer.Connect(ctx, r.d.signerOptions(r.run))
	if err != nil {
		return fmt.Errorf("reach the signer for the new key: %w", err)
	}
	id, err := trust.LoadIdentity(r.d.o.Config.PKIDir(), cur.Trust, r.d.o.Now(), r.reader(), nc)
	if err != nil {
		return fmt.Errorf("load the renewed identity: %w", err)
	}
	if !id.SameEnrolment(cur.Identity) || !bytes.Equal(id.Leaf.Raw, leaf.Raw) {
		return errors.New("the loaded identity is not the renewed certificate of the same enrolment")
	}
	oldPin, _ := keyPinHex(cur.Identity.Leaf.PublicKey)
	newPin, _ := keyPinHex(leaf.PublicKey)
	r.d.creds.Store(&server.Credentials{Identity: id, Trust: cur.Trust})
	r.loadLocked()
	r.d.log.Info("renewed certificate in use: TLS and signed answers use the new key",
		"enrolment", hex.EncodeToString(id.Record.EnrolmentID), "oldKeyPin", oldPin, "newKeyPin", newPin,
		"sha256", hex.EncodeToString(sha256Of(leaf.Raw)), "notAfter", leaf.NotAfter.UTC().Format(time.RFC3339))
	return nil
}

// step runs the test hook of a renewal step.
func (r *renewer) step(name string) error {
	if h := r.d.o.Hooks.RenewalStep; h != nil {
		if err := h(name); err != nil {
			return fmt.Errorf("%w: %s: %v", errRenewalStopped, name, err)
		}
	}
	return nil
}

// recoverRenewal finishes a renewal cut short between the signer's switch
// to the new key and the promotion of the staged certificate, before the
// identity is loaded (development builds only).
func (d *daemon) recoverRenewal(tc *trust.Config, sg *signer.Client) {
	pub, ok := sg.Public().(*ecdsa.PublicKey)
	if !ok {
		return
	}
	done, err := enrol.RecoverRenewal(d.o.Config.PKIDir(), trust.KeyFileReader(d.uid), tc, pub, d.o.Now())
	switch {
	case err != nil:
		d.log.Error(err, "an interrupted certificate renewal could not be finished")
	case done:
		d.log.Info("finished an interrupted certificate renewal: the staged certificate is the host certificate")
	}
}

// keyPinBytes returns the SHA-256 of a public key's DER SubjectPublicKeyInfo.
func keyPinBytes(pub any) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("encode the public key: %w", err)
	}
	return sha256Of(der), nil
}

// keyPinHex is keyPinBytes in lowercase hexadecimal.
func keyPinHex(pub any) (string, error) {
	b, err := keyPinBytes(pub)
	return hex.EncodeToString(b), err
}

// sha256Of returns the SHA-256 digest of b.
func sha256Of(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
