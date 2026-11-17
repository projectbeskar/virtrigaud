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

package controller

// Per-host agent certificate renewal, manager side (development builds only).
//
//  1. At two thirds of its certificate's lifetime the host offers a renewal
//     request for a new key, co-signed by its pinned key, in the signed
//     answer of a GetHostInfo the manager asked for it with
//     (renewal_version). The heartbeat verifies it (hostidentity.VerifyAnswer
//     and VerifyRenewal) and records it in the Host's pins Secret
//     (renewalStep).
//  2. The HostEnrolmentReconciler creates a cert-manager CertificateRequest
//     for it under the manager's own identity, labelled as a renewal and
//     naming the Host (reconcileRenewal).
//  3. The HostEnrolmentRequestReconciler approves it only when all of these
//     hold (decideRenewal): its recorded requester is the manager's own
//     identity; the co-signature by the currently pinned key verifies again;
//     the request's subject alternative names are the current enrolment's,
//     unchanged; the current certificate is valid; the Host is not revoked.
//     Anything else is denied, and a request labelled as a renewal that the
//     manager did not create is never approved.
//  4. The issued certificate is validated against the renewal (same
//     enrolment, the new key) and published in the host inventory for the
//     Provider (completeRenewal, resolveHostRenewal). The Provider stages it
//     on the host.
//  5. Only after a GetHostInfo the manager asked, with its own nonce, returns
//     a verified answer that names the staged certificate's digest is the new
//     key pinned beside the old one (the overlap). The Provider then puts it
//     in use on the host.
//  6. The old key is unpinned after a verified answer of the manager's own
//     request is signed by the new key and names no staged certificate.
//
// An overlap that outlasts renewalOverlap is reported (event) but never ends
// by itself while both keys might be in use: removing either pin without
// evidence could leave the host with no key the Provider accepts. The old
// key is unpinned at the latest when the old certificate expires, when it
// can no longer authenticate the host anyway. A re-key, a revocation and a
// replaced Host clear a renewal.
//
// Every pins write is an update with the read resourceVersion. Nothing here
// logs or records key material; certificates and requests are public and are
// kept in the pins Secret, never in logs, events or the Host status.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"google.golang.org/protobuf/proto"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/devgate"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// renewalOverlap is how long both keys are expected to stay pinned once the
// new key is pinned: the Provider normally activates the new certificate
// within a heartbeat or two. Past it the overlap is reported, not ended (see
// the package comment above).
const renewalOverlap = 24 * time.Hour

// renewalAgentAPIVersion is the agent API version that introduced the
// renewal request (GetHostInfoRequest.renewal_version): the manager asks a
// host for it only once a verified answer showed at least this version.
const renewalAgentAPIVersion = 3

// renewalRequestPrefix prefixes the name of a renewal CertificateRequest.
const renewalRequestPrefix = "virtrigaud-renewal-"

// Bounds of retried renewals. A renewal whose issued certificate is refused,
// or whose request is denied, fails or disappears, starts again for the same
// key after a backoff that doubles from a base up to renewalRetryMax; after
// maxRenewalFailures failures that key is given up (the agent draws a new
// key when its request goes unanswered) and the operator is told to fix the
// issuer. The base is an eighth of the current certificate's remaining
// lifetime, between renewalRetryFloor and renewalRetryBase, so a short-lived
// certificate is retried well within the half of its remaining lifetime after
// which the agent draws a new key. maxRenewalAttempts bounds the keys
// remembered.
const (
	renewalRetryBase   = 10 * time.Minute
	renewalRetryFloor  = time.Minute
	renewalRetryMax    = 6 * time.Hour
	maxRenewalFailures = 5
	maxRenewalAttempts = 8
)

// renewalRequestsRetained is how many finished renewal requests are kept per
// Host for audit (the newest); older finished ones are deleted.
const renewalRequestsRetained = 3

// renewalRequestRecheck is how often a renewal whose request is created but
// not yet issued is looked at again (besides the request's own events).
const renewalRequestRecheck = 5 * time.Minute

// Event reasons of certificate renewal, emitted on the Host.
const (
	eventReasonAgentRenewalRequested     = "AgentRenewalRequested"
	eventReasonAgentRenewalApproved      = "AgentRenewalApproved"
	eventReasonAgentRenewalDenied        = "AgentRenewalDenied"
	eventReasonAgentRenewalIssued        = "AgentRenewalIssued"
	eventReasonAgentRenewalRefused       = "AgentRenewalRefused"
	eventReasonAgentRenewalPinned        = "AgentRenewalPinned"
	eventReasonAgentRenewed              = "AgentRenewed"
	eventReasonAgentRenewalOverlap       = "AgentRenewalOverlapExpired"
	eventReasonAgentRenewalAtExpiry      = "AgentRenewalCompletedAtExpiry"
	eventReasonAgentRenewalNotSupported  = "AgentRenewalNotSupported"
	eventReasonAgentRenewalKeyWasPinned  = "AgentRenewalKeyPreviouslyPinned"
	eventReasonAgentRenewalCreateRefused = "AgentRenewalRequestNotCreated"
	eventReasonAgentRenewalRestarted     = "AgentRenewalRestarted"
	eventReasonAgentRenewalGivenUp       = "AgentRenewalGivenUp"
	eventReasonAgentRenewalEarly         = "AgentRenewalActivatedEarly"
	eventReasonAgentRenewalNoIdentity    = "AgentRenewalNoManagerIdentity"
	eventReasonAgentRenewalPinnedExpiry  = "AgentRenewalPinnedAtExpiry"
	eventReasonAgentRenewalOverdue       = "AgentRenewalOverdue"
)

// Denial reasons of renewal requests.
const (
	// reasonRenewalRequester: the request was not created by the manager.
	reasonRenewalRequester = "RenewalNotRequestedByManager"
	// reasonRenewalIssuer: the request does not target the cert-manager
	// issuer of the host-agent sub-CA.
	reasonRenewalIssuer = "RenewalIssuerRefused"
	// reasonRenewalNotPending: the Host has no renewal recorded for this
	// request.
	reasonRenewalNotPending = "NoPendingRenewal"
	// reasonRenewalNotVerified: the host's co-signed renewal request no
	// longer verifies against the current pins, enrolment and clock (the
	// current certificate expired, the pin changed, the identities differ).
	reasonRenewalNotVerified = "RenewalNotVerified"
	// reasonRenewalNotEnrolled: the Host has no current enrolment.
	reasonRenewalNotEnrolled = "HostNotEnrolled"
)

// errNoManagerIdentity is returned when the manager does not know its own
// identity: no renewal request is created or approved.
var errNoManagerIdentity = errors.New("the manager's own identity is not known; certificate renewal is off")

// renewalState is a Host's certificate renewal in progress, kept in its pins
// Secret. It holds public material only.
type renewalState struct {
	// Request is the host's verified, co-signed renewal request: the
	// deterministic encoding of its SignedBlob. It is verified again before
	// the manager approves its certificate request.
	Request []byte `json:"request"`
	// KeyPin is the pin of the renewal's new key.
	KeyPin string `json:"keyPin"`
	// SignerPin is the pin of the key that co-signed it (the current key).
	SignerPin string `json:"signerPin"`
	// Since is when the manager recorded it.
	Since metav1.Time `json:"since"`
	// CertificateRequest is "namespace/name" of the manager's
	// CertificateRequest, and CertificateRequestUID its UID, once created.
	CertificateRequest    string `json:"certificateRequest,omitempty"`
	CertificateRequestUID string `json:"certificateRequestUID,omitempty"`
	// Approved records that the manager approved it.
	Approved bool `json:"approved,omitempty"`
	// Certificate is the issued, validated renewed certificate (DER), and
	// CertificateSHA256 its digest (hex): the digest the host must name as
	// staged before the new key is pinned.
	Certificate       []byte `json:"certificate,omitempty"`
	CertificateSHA256 string `json:"certificateSHA256,omitempty"`
	// NotAfter is when the renewed certificate expires, and NotBefore when
	// its validity begins.
	NotAfter  *metav1.Time `json:"notAfter,omitempty"`
	NotBefore *metav1.Time `json:"notBefore,omitempty"`
	// PinnedAt is when the new key was pinned beside the old one, and
	// OverlapUntil when the overlap is expected to end.
	PinnedAt     *metav1.Time `json:"pinnedAt,omitempty"`
	OverlapUntil *metav1.Time `json:"overlapUntil,omitempty"`
	// OverlapExpiredReported records that the overlap's expiry was reported.
	OverlapExpiredReported bool `json:"overlapExpiredReported,omitempty"`
	// UnsupportedReported records that the manager reported it cannot
	// renew (no cert-manager issuer).
	UnsupportedReported bool `json:"unsupportedReported,omitempty"`
	// IdentityReported records that the manager reported it cannot create
	// the request because it does not know its own identity.
	IdentityReported bool `json:"identityReported,omitempty"`
}

// renewalAttempt is the failure history of renewals for one new key.
type renewalAttempt struct {
	// KeyPin is the new key.
	KeyPin string `json:"keyPin"`
	// Failures counts the renewals of the key that failed.
	Failures int `json:"failures"`
	// NextTry is the earliest time a renewal of the key is recorded again.
	NextTry metav1.Time `json:"nextTry"`
	// Reason is why the last one failed.
	Reason string `json:"reason,omitempty"`
}

// renewalRetryBaseFor returns the backoff base for a certificate with
// remaining lifetime left: left/8, between renewalRetryFloor and
// renewalRetryBase.
func renewalRetryBaseFor(left time.Duration) time.Duration {
	return min(max(left/8, renewalRetryFloor), renewalRetryBase)
}

// renewalBackoff returns the wait after the n-th failure (n from 1) from
// base.
func renewalBackoff(n int, base time.Duration) time.Duration {
	return min(base<<min(n-1, 10), renewalRetryMax)
}

// failRenewal ends the renewal in progress (before its new key is pinned)
// after a failure, and records it for its key: the key is renewed again only
// after a backoff, and never after maxRenewalFailures. It returns the
// key's history and whether the key is now given up.
func (st *hostEnrolmentState) failRenewal(now time.Time, reason string) (renewalAttempt, bool) {
	r := st.Renewal
	if r == nil || r.PinnedAt != nil {
		return renewalAttempt{}, false
	}
	var left time.Duration
	if st.Current != nil {
		left = st.Current.NotAfter.Sub(now)
	}
	st.Renewal = nil
	i := slices.IndexFunc(st.RenewalAttempts, func(a renewalAttempt) bool { return a.KeyPin == r.KeyPin })
	if i < 0 {
		st.RenewalAttempts = append(st.RenewalAttempts, renewalAttempt{KeyPin: r.KeyPin})
		i = len(st.RenewalAttempts) - 1
	}
	a := &st.RenewalAttempts[i]
	a.Failures++
	a.Reason = reason
	a.NextTry = metav1.NewTime(now.Add(renewalBackoff(a.Failures, renewalRetryBaseFor(left))).UTC().Truncate(time.Second))
	out := *a
	if n := len(st.RenewalAttempts); n > maxRenewalAttempts {
		st.RenewalAttempts = slices.Clone(st.RenewalAttempts[n-maxRenewalAttempts:])
	}
	return out, out.Failures >= maxRenewalFailures
}

// renewalHeldBack reports whether a renewal of key pin may not be recorded
// at now: it is in its backoff, or given up.
func (st *hostEnrolmentState) renewalHeldBack(pin string, now time.Time) bool {
	for _, a := range st.RenewalAttempts {
		if a.KeyPin == pin {
			return a.Failures >= maxRenewalFailures || now.Before(a.NextTry.Time)
		}
	}
	return false
}

// restartedEvents returns the events of a renewal that failed with reason
// and the history a.
func restartedEvents(a renewalAttempt, gaveUp bool, reason string) []hostEvent {
	if gaveUp {
		return []hostEvent{{corev1.EventTypeWarning, eventReasonAgentRenewalGivenUp, fmt.Sprintf("the renewal for key %s failed %d times "+
			"(last: %s); it is not tried again for this key: fix the host-agent issuer (its CA must be the configured sub-CA, and it "+
			"must sign the request's names unchanged); the host asks again with a new key", a.KeyPin, a.Failures, reason)}}
	}
	return []hostEvent{{corev1.EventTypeWarning, eventReasonAgentRenewalRestarted, fmt.Sprintf("the renewal for key %s failed (%s); "+
		"it starts again after %s", a.KeyPin, reason, a.NextTry.UTC().Format(time.RFC3339))}}
}

// phase returns the renewal's mirrored phase.
func (r *renewalState) phase() infravirtrigaudiov1beta1.HostAgentRenewalPhase {
	switch {
	case r.PinnedAt != nil:
		return infravirtrigaudiov1beta1.HostAgentRenewalOverlap
	case len(r.Certificate) > 0:
		return infravirtrigaudiov1beta1.HostAgentRenewalIssued
	}
	return infravirtrigaudiov1beta1.HostAgentRenewalRequested
}

// blob decodes the stored co-signed request.
func (r *renewalState) blob() (*providerv1.SignedBlob, error) {
	b := &providerv1.SignedBlob{}
	if err := proto.Unmarshal(r.Request, b); err != nil {
		return nil, fmt.Errorf("decode the stored renewal request: %w", err)
	}
	return b, nil
}

// body decodes the stored request's body and its signer certificate
// (unverified: callers verify before acting on it).
func (r *renewalState) body() (*providerv1.RenewalRequest, *x509.Certificate, error) {
	b, err := r.blob()
	if err != nil {
		return nil, nil, err
	}
	body := &providerv1.RenewalRequest{}
	if err := proto.Unmarshal(b.GetBody(), body); err != nil {
		return nil, nil, fmt.Errorf("decode the stored renewal request body: %w", err)
	}
	cert, err := x509.ParseCertificate(b.GetSignerCertificate())
	if err != nil {
		return nil, nil, fmt.Errorf("parse the stored renewal request's signer: %w", err)
	}
	return body, cert, nil
}

// newRenewalRequestName draws the name of a renewal CertificateRequest: a
// fixed prefix and 12 random bytes, saved in the pins Secret before the
// request is created, so nobody can predict it.
func newRenewalRequestName(src io.Reader) (string, error) {
	b := make([]byte, 12)
	if _, err := io.ReadFull(src, b); err != nil {
		return "", fmt.Errorf("draw a renewal request name: %w", err)
	}
	return renewalRequestPrefix + hex.EncodeToString(b), nil
}

// reasonAgentRenewalOverdue is the AgentEnrolled reason (status True) of a
// Host whose pinned certificate is past currentEnrolment.renewalOverdueAt
// with no renewal in progress.
const reasonAgentRenewalOverdue = "RenewalOverdue"

// ptrTime returns t, truncated to the second in UTC, as a *metav1.Time.
func ptrTime(t time.Time) *metav1.Time {
	m := metav1.NewTime(t.UTC().Truncate(time.Second))
	return &m
}

// hostHashLabel returns the value of certrequest.LabelHostHash for a Host.
func hostHashLabel(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return hex.EncodeToString(sum[:16])
}

// completeRenewalPins ends an overlap: the renewal's key becomes the only
// pin, the others are recorded as previous keys, and the current enrolment's
// expiry becomes the renewed certificate's. It reports the pins removed.
func (st *hostEnrolmentState) completeRenewalPins() []string {
	r, cur := st.Renewal, st.Current
	if r == nil || cur == nil {
		return nil
	}
	var removed []string
	for _, pin := range cur.KeyPins {
		if pin == r.KeyPin {
			continue
		}
		removed = append(removed, pin)
		if !slices.Contains(st.PreviousKeyPins, pin) {
			st.PreviousKeyPins = append(st.PreviousKeyPins, pin)
		}
	}
	if n := len(st.PreviousKeyPins); n > maxPreviousKeyPins {
		st.PreviousKeyPins = slices.Clone(st.PreviousKeyPins[n-maxPreviousKeyPins:])
	}
	cur.KeyPins = []string{r.KeyPin}
	if r.NotAfter != nil {
		cur.NotAfter, cur.NotBefore = *r.NotAfter, r.NotBefore
	}
	st.Renewal, st.RenewalAttempts = nil, nil
	return removed
}

// renewalExpectation is what a stored renewal request of a Host must verify
// against now: the Host's current enrolment and pins, the sub-CA, the
// Provider's UID.
func renewalExpectation(cfg *HostEnrolmentConfig, host *infravirtrigaudiov1beta1.Host, cur *currentEnrolment,
	providerUID string, now time.Time) (hostidentity.RenewalExpectation, error) {
	anchors, err := hostidentity.RootPool(cfg.CABundle)
	if err != nil {
		return hostidentity.RenewalExpectation{}, err
	}
	uri, err := hostidentity.HostURI(cfg.TrustDomain, host.Namespace, host.Name)
	if err != nil {
		return hostidentity.RenewalExpectation{}, err
	}
	rec, err := cur.Record.toRecord()
	if err != nil {
		return hostidentity.RenewalExpectation{}, err
	}
	return hostidentity.RenewalExpectation{
		Anchors: anchors, HostURI: uri, HostName: host.Name, HostID: host.Name, Pins: slices.Clone(cur.KeyPins),
		ProviderUID: providerUID, InstallID: bytes.Clone(cfg.InstallID), EnrolmentID: rec.EnrolmentID,
		Version: hostidentity.RenewalVersion, Now: now,
	}, nil
}

// renewalOutcome is what one verified host answer did to a Host's renewal.
type renewalOutcome struct {
	// requested names the new key of a renewal the answer started.
	requested string
	// keyWasPinned names a new key that was refused as previously pinned.
	keyWasPinned string
	// pinned names the new key pinned beside the old one.
	pinned string
	// completed names the new key that became the only pin, and removed the
	// old pins.
	completed string
	removed   []string
	// early reports that the completion came from an answer signed by the
	// issued certificate before its key was pinned.
	early bool
	// heldBack names a new key whose renewal is in its backoff or given up.
	heldBack string
}

// renewalTrust is the pins-Secret side of renewal (PinsHostTrust).
type renewalTrust interface {
	// RenewalStep applies a verified host answer of a manager-requested
	// GetHostInfo to the host's renewal.
	RenewalStep(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, v hostidentity.VerifiedAnswer,
		now time.Time) (renewalOutcome, error)
}

// RenewalStep implements renewalTrust. v must be the verified answer of a
// GetHostInfo this manager asked, with its own nonce, for its own verified
// pins: a renewed key is pinned only on such an answer naming the issued
// certificate's digest as staged, and the old key is unpinned only on such
// an answer signed by the new key and naming none. A pending renewal request
// in v (already verified) starts a renewal when none is in progress. The
// pins Secret is read from the API server and written with its
// resourceVersion.
func (p *PinsHostTrust) RenewalStep(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string,
	v hostidentity.VerifiedAnswer, now time.Time) (renewalOutcome, error) {
	var out renewalOutcome
	if v.Host == nil {
		return out, nil
	}
	h, err := p.host(ctx, provider, hostID)
	if err != nil {
		return out, err
	}
	if v.Renewal == nil {
		// Most heartbeats carry no renewal: the cached pins Secret says
		// whether one is in progress before the API server is read.
		if cached, err := loadHostEnrolment(ctx, p.Client, p.Config, h); err == nil && !cached.reset && cached.state.Renewal == nil {
			return out, nil
		}
	}
	l, err := loadHostEnrolment(ctx, p.secretReader(), p.Config, h)
	if err != nil || l.reset {
		return out, err
	}
	st := &l.state
	cur := st.Current
	if cur == nil || cur.Record.EnrolmentID != hex.EncodeToString(v.Context.GetEnrolmentId()) {
		return out, nil
	}
	staged := hex.EncodeToString(v.Host.GetStagedCertificateSha256())
	changed := false
	r := st.Renewal
	switch {
	case r != nil && r.PinnedAt != nil && v.SignerPin == r.KeyPin && staged == "":
		// The new key is in use: it signed this answer.
		out.completed, out.removed = r.KeyPin, st.completeRenewalPins()
		changed = true
	case r != nil && r.PinnedAt == nil && len(r.Certificate) > 0 && v.SignerPin == r.KeyPin && bytes.Equal(v.SignerCertificate, r.Certificate):
		// The host put the issued certificate in use before its key was
		// pinned (a Provider activated early): this answer, signed by exactly
		// the certificate the manager issued and validated for the current
		// enrolment, is the evidence; the new key becomes the only pin.
		out.completed, out.removed, out.early = r.KeyPin, st.completeRenewalPins(), true
		changed = true
	case r != nil && r.PinnedAt == nil && r.CertificateSHA256 != "" && staged == r.CertificateSHA256 && v.SignerPin == r.SignerPin:
		// The host staged exactly the certificate the manager had issued.
		if !slices.Contains(cur.KeyPins, r.KeyPin) {
			cur.KeyPins = append(cur.KeyPins, r.KeyPin)
		}
		at := metav1.NewTime(now.UTC().Truncate(time.Second))
		until := metav1.NewTime(at.Add(renewalOverlap))
		r.PinnedAt, r.OverlapUntil = &at, &until
		out.pinned = r.KeyPin
		changed = true
	}
	if pr := v.Renewal; pr != nil && st.Renewal != nil && st.Renewal.KeyPin != pr.KeyPin && st.Renewal.PinnedAt == nil {
		// The host offers another key than the one in progress (it discarded
		// its pending key): the renewal starts again for the new one.
		st.Renewal, changed = nil, true
	}
	if pr := v.Renewal; pr != nil && st.Renewal == nil && out.completed == "" {
		switch {
		case st.keyPreviouslyPinned(pr.KeyPin):
			out.keyWasPinned = pr.KeyPin
		case st.renewalHeldBack(pr.KeyPin, now):
			out.heldBack = pr.KeyPin
		default:
			st.Renewal = &renewalState{Request: pr.Blob, KeyPin: pr.KeyPin, SignerPin: pr.SignerPin,
				Since: metav1.NewTime(now.UTC().Truncate(time.Second))}
			out.requested, changed = pr.KeyPin, true
		}
	}
	if !changed {
		return out, nil
	}
	if err := saveHostEnrolment(ctx, p.Client, p.Config, h, &l); err != nil {
		return renewalOutcome{}, err
	}
	return out, nil
}

// renewalAsk returns the renewal version to ask host hostID of provider for:
// RenewalVersion once a verified answer showed an agent of the version that
// introduced it, 0 before.
func (h *HostAnswers) renewalAsk(provider *infravirtrigaudiov1beta1.Provider, hostID string) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.hosts[hostAnswerKey(provider, hostID)]; s != nil && s.hostAPIVersion >= renewalAgentAPIVersion {
		return hostidentity.RenewalVersion
	}
	return 0
}

// checkRenewal applies a verified host answer of the heartbeat to the host's
// renewal and returns the events to emit. Failures are logged; the next
// heartbeat tries again.
func (h *HostAnswers) checkRenewal(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string,
	v hostidentity.VerifiedAnswer) []hostEvent {
	rt, ok := h.Trust.(renewalTrust)
	if !ok {
		return nil
	}
	out, err := rt.RenewalStep(ctx, provider, hostID, v, h.now())
	if err != nil {
		log.FromContext(ctx).Info("Certificate renewal not advanced; tried again on the next heartbeat", "host", hostID, "error", err.Error())
		return nil
	}
	logger := log.FromContext(ctx)
	var evs []hostEvent
	if out.requested != "" {
		logger.Info("Recorded a host's co-signed certificate renewal request", "host", hostID, "newKeyPin", out.requested,
			"signerKeyPin", v.SignerPin)
		evs = append(evs, hostEvent{corev1.EventTypeNormal, eventReasonAgentRenewalRequested,
			"the host asked, signed with its pinned key, to renew its certificate for new key " + out.requested})
	}
	if out.keyWasPinned != "" {
		logger.Info("Refused a host's renewal request for a key that is, or was, pinned", "host", hostID, "keyPin", out.keyWasPinned)
		evs = append(evs, hostEvent{corev1.EventTypeWarning, eventReasonAgentRenewalKeyWasPinned,
			"the host's renewal request names key " + out.keyWasPinned + ", which is or was pinned for it; it is not renewed"})
	}
	if out.pinned != "" {
		logger.Info("Pinned a host's renewed key beside the current one: the host staged the issued certificate (signed answer)",
			"host", hostID, "newKeyPin", out.pinned, "answerSHA256", hex.EncodeToString(v.BlobSHA256[:]))
		evs = append(evs, hostEvent{corev1.EventTypeNormal, eventReasonAgentRenewalPinned,
			"the host staged its renewed certificate (signed answer); new key " + out.pinned + " is pinned beside the current one"})
	}
	if out.heldBack != "" {
		logger.V(1).Info("A host's renewal request for this key is held back after failures", "host", hostID, "keyPin", out.heldBack)
	}
	if out.early {
		evs = append(evs, hostEvent{corev1.EventTypeWarning, eventReasonAgentRenewalEarly, "the host put its renewed certificate in use " +
			"before its key was pinned (the Provider activated early); the answer signed by the issued certificate pins the new key"})
	}
	if out.completed != "" {
		logger.Info("Unpinned a host's old key: the new key signed the answer and nothing is staged", "host", hostID,
			"keyPin", out.completed, "removedKeyPins", out.removed, "answerSHA256", hex.EncodeToString(v.BlobSHA256[:]))
		evs = append(evs, hostEvent{corev1.EventTypeNormal, eventReasonAgentRenewed,
			fmt.Sprintf("the host uses its renewed certificate (answer signed by key %s); old key pins %v are removed", out.completed, out.removed)})
	}
	return evs
}

// hostEvent is an event to emit on a Host.
type hostEvent struct {
	typ, reason, message string
}

// reconcileRenewal advances an enrolled Host's renewal on the manager's own
// side: it creates the CertificateRequest of a recorded renewal under the
// manager's identity, restarts (under the backoff of failRenewal) a renewal
// whose request was denied, failed, deleted or changed, reports an overlap
// that outlasted renewalOverlap, and unpins the old key once the old
// certificate has expired (it can no longer authenticate the host). sec is
// the pins Secret, the owner of the requests it creates. It reports whether
// it changed the state and when to look again.
func (r *HostEnrolmentReconciler) reconcileRenewal(ctx context.Context, host *infravirtrigaudiov1beta1.Host,
	st *hostEnrolmentState, sec *corev1.Secret) (changed bool, recheck time.Duration, err error) {
	ren, cur := st.Renewal, st.Current
	if ren == nil || cur == nil {
		return false, 0, nil
	}
	now := r.now()
	if ren.PinnedAt != nil {
		if !now.Before(cur.NotAfter.Time) {
			removed := st.completeRenewalPins()
			log.FromContext(ctx).Info("Unpinned a host's old key at the old certificate's expiry; the renewed key stays pinned",
				"host", hostKey(host.Namespace, host.Name), "keyPin", ren.KeyPin, "removedKeyPins", removed)
			r.event(host, corev1.EventTypeWarning, eventReasonAgentRenewalAtExpiry, fmt.Sprintf("the old certificate expired before "+
				"the host showed its renewed certificate in use; old key pins %v are removed and key %s stays pinned", removed, ren.KeyPin))
			return true, 0, nil
		}
		if ren.OverlapUntil != nil && !now.Before(ren.OverlapUntil.Time) && !ren.OverlapExpiredReported {
			ren.OverlapExpiredReported, changed = true, true
			r.event(host, corev1.EventTypeWarning, eventReasonAgentRenewalOverlap, "the host has not shown its renewed certificate "+
				"in use within the overlap; both keys stay pinned until it does, or until the old certificate expires at "+
				cur.NotAfter.UTC().Format(time.RFC3339))
		}
		recheck = cur.NotAfter.Sub(now) + time.Second
		if ren.OverlapUntil != nil && !ren.OverlapExpiredReported {
			recheck = min(recheck, ren.OverlapUntil.Sub(now)+time.Second)
		}
		return changed, recheck, nil
	}
	if len(ren.Certificate) > 0 {
		// Issued, its key not pinned yet. At the old certificate's expiry the
		// old key can no longer authenticate the host; a host whose Provider
		// put the renewed certificate in use early, and whose evidence never
		// reached the manager, would stay unreachable for good. The issued key
		// (manager-issued, validated for the current enrolment, held by the
		// host by its proof of possession and the co-signature) is pinned
		// then, after checking the live state.
		if !now.Before(cur.NotAfter.Time) {
			pinned, err := r.pinRenewalAtExpiry(ctx, host, st)
			return pinned, 0, err
		}
		return false, cur.NotAfter.Sub(now) + time.Second, nil
	}
	if ren.CertificateRequestUID != "" {
		if why := r.renewalRequestBroken(ctx, ren); why != "" {
			a, gaveUp := st.failRenewal(now, why)
			for _, ev := range restartedEvents(a, gaveUp, why) {
				r.event(host, ev.typ, ev.reason, ev.message)
			}
			log.FromContext(ctx).Info("Restarted a host's certificate renewal", "host", hostKey(host.Namespace, host.Name),
				"keyPin", a.KeyPin, "reason", why, "failures", a.Failures, "givenUp", gaveUp)
			return true, 0, nil
		}
		return false, renewalRequestRecheck, nil
	}
	created, err := r.createRenewalRequest(ctx, host, st, sec)
	if errors.Is(err, errNoManagerIdentity) {
		if !ren.IdentityReported {
			ren.IdentityReported = true
			r.event(host, corev1.EventTypeWarning, eventReasonAgentRenewalNoIdentity, "the host asked to renew its certificate, but the "+
				"manager could not read its own identity (SelfSubjectReview); the request is created once it can")
			return true, renewalRequestRecheck, nil
		}
		return created, renewalRequestRecheck, nil
	}
	return created, 0, err
}

// pinRenewalAtExpiry pins the issued key of a renewal whose key was never
// pinned, once the old certificate has expired, on the live state only: the
// Host exists, is not deleting or revoked and still runs an agent of the
// enrolment's Provider; the pins Secret read from the API server holds the
// same current enrolment and the same renewal, still unpinned; and the
// issued certificate validates again now against the current enrolment and
// the renewal's key. The new key becomes the only pin (the old one belongs
// to an expired certificate), so the host is never left without a pin.
//
// The host may no longer hold that key (its agent discarded an unanswered
// request and drew a new key that the manager never saw): the host then
// stays unreachable, as it would have with only the expired pin, and a
// re-key of the Host is the recovery.
func (r *HostEnrolmentReconciler) pinRenewalAtExpiry(ctx context.Context, host *infravirtrigaudiov1beta1.Host,
	st *hostEnrolmentState) (bool, error) {
	ren, cur := st.Renewal, st.Current
	live := &infravirtrigaudiov1beta1.Host{}
	if err := r.liveReader().Get(ctx, types.NamespacedName{Namespace: host.Namespace, Name: host.Name}, live); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if live.UID != host.UID || !live.DeletionTimestamp.IsZero() || live.Spec.Revoked || live.Spec.Identity != host.Spec.Identity {
		return false, nil
	}
	provider, agent, err := hostRunsAgent(ctx, r.liveReader(), live)
	if err != nil || !agent || cur.Record.ProviderUID != string(provider.UID) {
		return false, err
	}
	l, err := loadHostEnrolment(ctx, r.liveReader(), r.Config, live)
	if err != nil || l.reset {
		return false, err
	}
	lc, lr := l.state.Current, l.state.Renewal
	if lc == nil || lc.Record != cur.Record || lr == nil || lr.PinnedAt != nil || lr.KeyPin != ren.KeyPin ||
		lr.CertificateSHA256 != ren.CertificateSHA256 || !bytes.Equal(lr.Certificate, ren.Certificate) {
		return false, nil
	}
	rec, err := cur.Record.toRecord()
	if err != nil {
		return false, err
	}
	hostURI, err := hostidentity.HostURI(r.Config.TrustDomain, live.Namespace, live.Name)
	if err != nil {
		return false, err
	}
	roots, err := hostidentity.RootPool(r.Config.CABundle)
	if err != nil {
		return false, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ren.Certificate})
	if _, err := hostidentity.ValidateIssued(hostidentity.Authorisation{HostName: live.Name, HostURI: hostURI, Record: rec,
		KeyPin: ren.KeyPin}, certPEM, roots, r.now()); err != nil {
		log.FromContext(ctx).Info("Not pinning a renewed key at the old certificate's expiry: the issued certificate no longer validates",
			"host", hostKey(host.Namespace, host.Name), "error", err.Error())
		return false, nil
	}
	removed := st.completeRenewalPins()
	log.FromContext(ctx).Info("Pinned a host's renewed key at the old certificate's expiry; the host showed no staged certificate in time",
		"host", hostKey(host.Namespace, host.Name), "keyPin", ren.KeyPin, "removedKeyPins", removed)
	r.event(host, corev1.EventTypeWarning, eventReasonAgentRenewalPinnedExpiry, fmt.Sprintf("the old certificate expired before the "+
		"host showed its renewed certificate staged; the issued key %s is pinned (the only pin) so a host that put it in use can "+
		"be reached again; old key pins %v are removed", ren.KeyPin, removed))
	return true, nil
}

// renewalRequestBroken returns why the recorded request of a renewal can no
// longer lead to an issued certificate ("" when it can): it is gone or
// replaced (another UID), it lost the renewal label, or it was denied,
// failed or refused. It reads the API server.
func (r *HostEnrolmentReconciler) renewalRequestBroken(ctx context.Context, ren *renewalState) string {
	ns, name, _ := strings.Cut(ren.CertificateRequest, "/")
	cr := certrequest.NewObject()
	err := r.liveReader().Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, cr)
	switch {
	case apierrors.IsNotFound(err):
		return "its certificate request was deleted"
	case err != nil:
		return ""
	case string(cr.GetUID()) != ren.CertificateRequestUID:
		return "its certificate request was replaced"
	case !certrequest.IsRenewal(cr):
		return "its certificate request lost the renewal label"
	case certrequest.IsTrue(cr, certrequest.ConditionDenied):
		return "its certificate request was denied"
	case certrequest.Failed(cr):
		return "its certificate request failed"
	case certrequest.IsTrue(cr, certrequest.ConditionRefused):
		return "its issued certificate was refused"
	}
	return ""
}

// createRenewalRequest creates the CertificateRequest of a recorded renewal
// in the enrolment namespace, under the manager's own identity, for the
// cert-manager issuer of the sub-CA, owned by the Host's pins Secret (so it
// goes with the Host), in two steps: a random name is recorded first (and
// saved by the caller), so the request is never seen before the pins Secret
// names it and nobody can predict it; the next reconcile creates it and
// records its UID. A request of that name that is already there is taken
// only when the manager created it for this Host with this request (a
// retried create); anything else of that name is left alone and a new name
// is drawn.
func (r *HostEnrolmentReconciler) createRenewalRequest(ctx context.Context, host *infravirtrigaudiov1beta1.Host,
	st *hostEnrolmentState, sec *corev1.Secret) (bool, error) {
	ren := st.Renewal
	id, haveID := r.Config.managerIdentity()
	switch {
	case r.Config.CertManagerIssuer == nil:
		if !ren.UnsupportedReported {
			ren.UnsupportedReported = true
			r.event(host, corev1.EventTypeWarning, eventReasonAgentRenewalNotSupported, "the host asked to renew its certificate, "+
				"but certificate renewal needs the cert-manager issuer of the host-agent sub-CA; re-key the Host before its "+
				"certificate expires")
			return true, nil
		}
		return false, nil
	case !haveID:
		return false, errNoManagerIdentity
	}
	body, current, err := ren.body()
	if err != nil {
		st.Renewal = nil
		return true, nil //nolint:nilerr // a stored request that does not decode is dropped; the host offers it again
	}
	if ren.CertificateRequest == "" {
		return true, r.drawRenewalRequestName(ren)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: body.GetCertificateRequest()})
	_, name, _ := strings.Cut(ren.CertificateRequest, "/")
	cr := certrequest.NewRenewal(certrequest.RenewalSpec{
		Namespace: r.Config.Namespace, Name: name, HostNamespace: host.Namespace, HostName: host.Name,
		HostHash: hostHashLabel(host.Namespace, host.Name), IssuerRef: *r.Config.CertManagerIssuer, CSRPEM: csrPEM,
		Duration: hostidentity.RenewalLifetime(current), Username: id.Username, Groups: id.Groups, UID: id.UID,
	})
	if sec != nil && sec.UID != "" {
		cr.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Secret", Name: sec.Name, UID: sec.UID}})
	}
	err = r.Create(ctx, cr)
	if apierrors.IsAlreadyExists(err) {
		existing := certrequest.NewObject()
		if gerr := r.liveReader().Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: name}, existing); gerr != nil {
			return false, fmt.Errorf("get renewal request %s/%s: %w", r.Config.Namespace, name, gerr)
		}
		got, cerr := certrequest.CSR(existing)
		in, rerr := certrequest.RequestOf(existing)
		ns, hn, ok := certrequest.HostOf(existing)
		if cerr != nil || rerr != nil || in.Username != id.Username || !ok || ns != host.Namespace || hn != host.Name ||
			!certrequest.IsRenewal(existing) || !bytes.Equal(got, csrPEM) {
			r.event(host, corev1.EventTypeWarning, eventReasonAgentRenewalCreateRefused, "an object named "+name+
				" that is not this renewal's request exists in the enrolment namespace; another name is drawn")
			return true, r.drawRenewalRequestName(ren)
		}
		cr, err = existing, nil
	}
	if err != nil {
		return false, fmt.Errorf("create renewal request %s/%s: %w", r.Config.Namespace, name, err)
	}
	ren.CertificateRequestUID = string(cr.GetUID())
	log.FromContext(ctx).Info("Created a certificate renewal request under the manager's identity", "host", hostKey(host.Namespace, host.Name),
		"request", ren.CertificateRequest, "newKeyPin", ren.KeyPin)
	return true, nil
}

// drawRenewalRequestName records a new random request name for ren.
func (r *HostEnrolmentReconciler) drawRenewalRequestName(ren *renewalState) error {
	src := r.Rand
	if src == nil {
		src = rand.Reader
	}
	name, err := newRenewalRequestName(src)
	if err != nil {
		return err
	}
	ren.CertificateRequest = r.Config.Namespace + "/" + name
	return nil
}

// collectRenewalRequests deletes the manager's finished renewal
// CertificateRequests of a Host beyond the newest renewalRequestsRetained
// (kept for audit). It runs after the reconcile's own save. A request is
// deleted only when all hold: it is not the request of the renewal in
// progress, read from the API server (never from a cache that may lag); it
// is finished (Denied, Failed, refused, or issued); it is older than
// renewalRequestRecheck; it is labelled as this Host's renewal and recorded
// under the manager's identity. Deletes carry UID and resourceVersion
// preconditions. Failures are logged; the next reconcile tries again.
func (r *HostEnrolmentReconciler) collectRenewalRequests(ctx context.Context, host *infravirtrigaudiov1beta1.Host) {
	id, ok := r.Config.managerIdentity()
	if !ok {
		return
	}
	list := certrequest.NewList()
	if err := r.List(ctx, list, client.InNamespace(r.Config.Namespace), client.MatchingLabels{
		certrequest.LabelAgentRenewal: "true", certrequest.LabelHostHash: hostHashLabel(host.Namespace, host.Name),
	}); err != nil {
		log.FromContext(ctx).Info("Cannot list a Host's renewal requests", "host", hostKey(host.Namespace, host.Name), "error", err.Error())
		return
	}
	now := r.now()
	var finished []*unstructured.Unstructured
	for i := range list.Items {
		cr := &list.Items[i]
		if !renewalRequestFinished(cr) {
			continue
		}
		in, err := certrequest.RequestOf(cr)
		ns, hn, ok := certrequest.HostOf(cr)
		if err != nil || in.Username != id.Username || !ok || ns != host.Namespace || hn != host.Name {
			continue
		}
		finished = append(finished, cr)
	}
	// The pins Secret is read from the API server only when something could
	// be deleted: more finished requests than are retained.
	if len(finished) <= renewalRequestsRetained {
		return
	}
	l, err := loadHostEnrolment(ctx, r.liveReader(), r.Config, host)
	if err != nil {
		return
	}
	if !l.reset && l.state.Renewal != nil {
		keep := l.state.Renewal.CertificateRequest
		finished = slices.DeleteFunc(finished, func(cr *unstructured.Unstructured) bool {
			return cr.GetNamespace()+"/"+cr.GetName() == keep
		})
	}
	slices.SortFunc(finished, func(a, b *unstructured.Unstructured) int {
		return b.GetCreationTimestamp().Compare(a.GetCreationTimestamp().Time)
	})
	for i, cr := range finished {
		if i < renewalRequestsRetained || now.Sub(cr.GetCreationTimestamp().Time) < renewalRequestRecheck {
			continue
		}
		uid, rv := cr.GetUID(), cr.GetResourceVersion()
		if err := r.Delete(ctx, cr, client.Preconditions{UID: &uid, ResourceVersion: &rv}); client.IgnoreNotFound(err) != nil {
			log.FromContext(ctx).Info("Cannot delete a finished renewal request", "request", cr.GetNamespace()+"/"+cr.GetName(), "error", err.Error())
			continue
		}
		log.FromContext(ctx).Info("Deleted a finished renewal request beyond the retained ones", "request", cr.GetNamespace()+"/"+cr.GetName(),
			"host", hostKey(host.Namespace, host.Name))
	}
}

// renewalRequestFinished reports whether a renewal request is finished:
// denied, failed, refused or issued.
func renewalRequestFinished(cr *unstructured.Unstructured) bool {
	return certrequest.IsTrue(cr, certrequest.ConditionDenied) || certrequest.Failed(cr) ||
		certrequest.IsTrue(cr, certrequest.ConditionRefused) || certrequest.IsTrue(cr, certrequest.ConditionReady)
}

// decideRenewal approves a renewal CertificateRequest only when all of these
// hold, and denies it otherwise: its recorded requester is the manager's own
// identity (checked first, before anything else is read); it targets the
// cert-manager issuer; the Host exists, runs an agent, has an identity, is
// not revoked and is enrolled; its renewal in progress is this request; the
// stored co-signed renewal request verifies again now against the current
// pins, enrolment, Provider and sub-CA (the co-signature by the pinned key,
// unchanged identities, a valid current certificate); the request carries
// exactly that request's CSR with agent usages and a bounded lifetime; and
// the new key was never pinned.
func (r *HostEnrolmentRequestReconciler) decideRenewal(ctx context.Context, cr *unstructured.Unstructured, iss HostCertificateIssuer) error {
	return r.judgeRenewal(ctx, cr, iss, false)
}

// judgeRenewal applies the renewal approval rules (decideRenewal). With
// approvedElsewhere, the request was already approved by another approver
// (approver-policy, say) before the manager decided it: the manager writes
// no condition (a request cannot be both approved and denied), and only
// records its own approval when every rule holds. A request it would deny
// is recorded as not approved by the manager, so its issued certificate is
// never published (completeRenewal marks it refused): "approved by someone
// else" is never taken as the manager's approval.
func (r *HostEnrolmentRequestReconciler) judgeRenewal(ctx context.Context, cr *unstructured.Unstructured, iss HostCertificateIssuer,
	approvedElsewhere bool) error {
	reqName := cr.GetNamespace() + "/" + cr.GetName()
	deny := func(host *infravirtrigaudiov1beta1.Host, reason, message string) error {
		if approvedElsewhere {
			return r.refuseApprovedElsewhere(ctx, cr, host, reason, message)
		}
		return r.denyRenewal(ctx, cr, host, reason, message)
	}
	in, err := certrequest.RequestOf(cr)
	if err != nil {
		return deny(nil, hostidentity.ReasonRequestInvalid, err.Error())
	}
	if id, ok := r.Config.managerIdentity(); !ok || in.Username != id.Username {
		return deny(nil, reasonRenewalRequester,
			"a renewal request is approved only when the manager created it; this one was created by another principal")
	}
	if iss.Name() != (certManagerIssuer{}).Name() {
		return deny(nil, reasonRenewalIssuer, "renewal requests are issued only by the cert-manager issuer")
	}
	ns, name, ok := certrequest.HostOf(cr)
	if !ok {
		return deny(nil, reasonEnrolHostUnnamed, "the request does not name a Host in annotation "+certrequest.AnnotationHost)
	}
	host := &infravirtrigaudiov1beta1.Host{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, host); err != nil {
		if apierrors.IsNotFound(err) {
			return deny(nil, reasonEnrolHostNotFound, fmt.Sprintf("Host %s/%s does not exist", ns, name))
		}
		return fmt.Errorf("get Host of renewal request %s: %w", reqName, err)
	}
	switch {
	case !host.DeletionTimestamp.IsZero():
		return deny(host, reasonEnrolHostNotFound, "the Host is being deleted")
	case host.Spec.Identity == "":
		return deny(host, reasonEnrolIdentityMissing, "the Host has no spec.identity")
	case host.Spec.Revoked:
		return deny(host, reasonEnrolHostRevoked, "the Host is revoked")
	}
	provider, agent, err := hostRunsAgent(ctx, r.Client, host)
	if err != nil {
		return err
	}
	if !agent {
		return deny(host, reasonEnrolHostNotAgent, "the Host's Provider does not run a per-host agent")
	}
	l, err := loadHostEnrolment(ctx, r.liveReader(), r.Config, host)
	if err != nil {
		return err
	}
	cur, ren := l.state.Current, l.state.Renewal
	switch {
	case l.reset || cur == nil:
		return deny(host, reasonRenewalNotEnrolled, "the Host is not enrolled")
	case ren == nil || ren.CertificateRequest != reqName || (ren.CertificateRequestUID != "" && ren.CertificateRequestUID != string(cr.GetUID())):
		return deny(host, reasonRenewalNotPending, "the Host has no renewal in progress for this request")
	case cur.Record.ProviderUID != string(provider.UID):
		return deny(host, reasonEnrolProviderChanged, "the Host is enrolled with another Provider than its own")
	}
	exp, err := renewalExpectation(r.Config, host, cur, string(provider.UID), r.now())
	if err != nil {
		return err
	}
	blob, err := ren.blob()
	if err != nil {
		return deny(host, reasonRenewalNotVerified, "the stored renewal request does not decode")
	}
	v, err := hostidentity.VerifyRenewal(blob, exp)
	if err != nil || v.KeyPin != ren.KeyPin {
		why := "the renewal names another key than recorded"
		if err != nil {
			why = err.Error()
		}
		return deny(host, reasonRenewalNotVerified, why)
	}
	if err := hostidentity.CheckRenewalRequestSpec(hostidentity.RenewalRequestSpec{CSRPEM: in.CSRPEM, Usages: in.Usages,
		Duration: in.Duration, IsCA: in.IsCA}, v); err != nil {
		if d, ok := hostidentity.AsDenial(err); ok {
			return deny(host, d.Reason, d.Message)
		}
		return err
	}
	if slices.Contains(l.state.PreviousKeyPins, v.KeyPin) || slices.Contains(cur.KeyPins, v.KeyPin) {
		return deny(host, reasonEnrolKeyPreviouslyPinned, "the renewal's key is, or was, pinned for this Host")
	}
	// Record the approval first, then approve: a crash in between leaves an
	// undecided request this reconcile approves again (its UID matches).
	// The approval is reported (log line, event) once, when it is first
	// saved to the pins Secret: the condition write below may lose to
	// another approver, and the retry then finds the approval recorded.
	recorded := ren.Approved && ren.CertificateRequestUID == string(cr.GetUID())
	ren.Approved, ren.CertificateRequestUID = true, string(cr.GetUID())
	if err := saveHostEnrolment(ctx, r.Client, r.Config, host, &l); err != nil {
		return err
	}
	if !recorded {
		if approvedElsewhere {
			log.FromContext(ctx).Info("A host-agent renewal request was approved by another approver first; the manager's rules hold, "+
				"so its certificate may be published", "request", reqName, "host", ns+"/"+name, "newKeyPin", v.KeyPin)
			r.event(host, corev1.EventTypeNormal, eventReasonAgentRenewalApproved, "certificate renewal request "+reqName+
				" (approved by another approver first) approved for new key "+v.KeyPin)
		} else {
			log.FromContext(ctx).Info("Approved a host-agent certificate renewal request", "request", reqName, "host", ns+"/"+name,
				"newKeyPin", v.KeyPin, "signerKeyPin", v.SignerPin)
			r.event(host, corev1.EventTypeNormal, eventReasonAgentRenewalApproved, "certificate renewal request "+reqName+" approved for new key "+v.KeyPin)
		}
	}
	if approvedElsewhere {
		return nil
	}
	if err := certrequest.SetCondition(cr, certrequest.ConditionApproved, certrequest.StatusTrue, reasonEnrolAuthorised,
		"certificate renewal authorised by VirtRigaud for Host "+ns+"/"+name, r.now()); err != nil {
		return err
	}
	if err := r.Status().Update(ctx, cr); err != nil {
		return fmt.Errorf("approve renewal request %s: %w", reqName, err)
	}
	return nil
}

// refuseApprovedElsewhere refuses a renewal request that another approver
// approved although the manager's rules deny it: nothing is recorded as the
// manager's approval, and the request is marked refused at once (it cannot
// also be denied), which is final: its certificate is never published, even
// if the rule that failed later passes (the renewal starts again with a new
// request).
func (r *HostEnrolmentRequestReconciler) refuseApprovedElsewhere(ctx context.Context, cr *unstructured.Unstructured,
	host *infravirtrigaudiov1beta1.Host, reason, message string) error {
	reqName := cr.GetNamespace() + "/" + cr.GetName()
	if err := markRefused(cr, fmt.Errorf("approved by another approver, but the manager denies it (%s): %s", reason, message), r.now()); err != nil {
		return err
	}
	if err := r.Status().Update(ctx, cr); err != nil {
		return fmt.Errorf("refuse renewal request %s: %w", reqName, err)
	}
	if host == nil {
		host = r.namedHost(ctx, cr)
	}
	log.FromContext(ctx).Info("A host-agent renewal request was approved by another approver, but the manager's rules deny it; "+
		"its certificate will not be published", "request", reqName, "reason", reason, "detail", message)
	if host != nil {
		r.event(host, corev1.EventTypeWarning, eventReasonAgentRenewalDenied, "certificate renewal request "+reqName+
			" was approved by another approver, but the manager denies it ("+reason+"): its certificate is not published")
	}
	return nil
}

// approvedElsewhereUnjudged reports whether cr, approved, is the Host's
// renewal in progress and the manager has not recorded its own approval of
// it: another approver decided it before the manager did.
func (r *HostEnrolmentRequestReconciler) approvedElsewhereUnjudged(ctx context.Context, cr *unstructured.Unstructured) (*infravirtrigaudiov1beta1.Host, bool, error) {
	ns, name, ok := certrequest.HostOf(cr)
	if !ok {
		return nil, false, nil
	}
	host := &infravirtrigaudiov1beta1.Host{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, host); err != nil {
		return nil, false, client.IgnoreNotFound(err)
	}
	l, err := loadHostEnrolment(ctx, r.liveReader(), r.Config, host)
	if err != nil {
		return nil, false, err
	}
	ren := l.state.Renewal
	return host, ren != nil && !ren.Approved && ren.CertificateRequest == cr.GetNamespace()+"/"+cr.GetName(), nil
}

// namedHost returns the Host a request names, nil when it names none or
// the Host does not exist (for an event only).
func (r *HostEnrolmentRequestReconciler) namedHost(ctx context.Context, cr *unstructured.Unstructured) *infravirtrigaudiov1beta1.Host {
	ns, name, ok := certrequest.HostOf(cr)
	if !ok {
		return nil
	}
	host := &infravirtrigaudiov1beta1.Host{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, host); err != nil {
		return nil
	}
	return host
}

// denyRenewal sets a renewal request's Denied condition, and emits a Warning
// event on the Host when one is passed (only once the requester is known to
// be the manager).
func (r *HostEnrolmentRequestReconciler) denyRenewal(ctx context.Context, cr *unstructured.Unstructured,
	host *infravirtrigaudiov1beta1.Host, reason, message string) error {
	reqName := cr.GetNamespace() + "/" + cr.GetName()
	if err := certrequest.SetCondition(cr, certrequest.ConditionDenied, certrequest.StatusTrue, reason, message, r.now()); err != nil {
		return err
	}
	if err := r.Status().Update(ctx, cr); err != nil {
		return fmt.Errorf("deny renewal request %s: %w", reqName, err)
	}
	log.FromContext(ctx).Info("Denied a host-agent certificate renewal request", "request", reqName, "reason", reason)
	if host != nil {
		r.event(host, corev1.EventTypeWarning, eventReasonAgentRenewalDenied, "certificate renewal request "+reqName+" denied: "+reason)
	}
	return nil
}

// completeRenewal validates the certificate issued for an approved renewal
// request against the renewal (the current enrolment, unchanged; the new
// key; the sub-CA) and records it for the Provider to stage, or refuses it
// (marked on the request; the renewal starts again). A request that is not
// the Host's renewal in progress is marked refused when it holds a
// certificate.
func (r *HostEnrolmentRequestReconciler) completeRenewal(ctx context.Context, cr *unstructured.Unstructured, iss HostCertificateIssuer) error {
	reqName := cr.GetNamespace() + "/" + cr.GetName()
	ns, name, ok := certrequest.HostOf(cr)
	if !ok {
		return nil
	}
	host := &infravirtrigaudiov1beta1.Host{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, host); err != nil {
		return client.IgnoreNotFound(err)
	}
	l, err := loadHostEnrolment(ctx, r.liveReader(), r.Config, host)
	if err != nil {
		return err
	}
	cur, ren := l.state.Current, l.state.Renewal
	chain, err := iss.Issued(cr)
	if err != nil || chain == nil {
		return err
	}
	if l.reset || cur == nil || ren == nil || !ren.Approved || ren.CertificateRequestUID != string(cr.GetUID()) {
		if issuedKeyWasPinned(chain, &l.state) {
			// A finished renewal (its certificate is, or was, the host's
			// pinned one; every manager restart reconciles its request again)
			// is never marked "never install".
			return nil
		}
		return r.markRenewalWithdrawn(ctx, cr, host, !wasHostRenewal(cr, &l))
	}
	if len(ren.Certificate) > 0 {
		return nil
	}
	issued, verr := r.validateRenewed(ctx, host, cur, ren, chain)
	if verr != nil {
		a, gaveUp := l.state.failRenewal(r.now(), "the issued certificate was refused: "+verr.Error())
		if err := saveHostEnrolment(ctx, r.Client, r.Config, host, &l); err != nil {
			return err
		}
		for _, ev := range restartedEvents(a, gaveUp, "the issued certificate was refused") {
			r.event(host, ev.typ, ev.reason, ev.message)
		}
		if _, err := iss.Complete(cr, verr, r.now()); err != nil {
			return err
		}
		if err := r.Status().Update(ctx, cr); err != nil {
			return fmt.Errorf("record the outcome on renewal request %s: %w", reqName, err)
		}
		log.FromContext(ctx).Info("Refused the certificate issued for a renewal request", "request", reqName, "host", ns+"/"+name)
		r.event(host, corev1.EventTypeWarning, eventReasonAgentRenewalRefused, "the certificate issued for renewal request "+reqName+
			" was refused")
		return nil
	}
	na := metav1.NewTime(issued.Leaf.NotAfter.UTC().Truncate(time.Second))
	ren.Certificate, ren.CertificateSHA256, ren.NotAfter = bytes.Clone(issued.Leaf.Raw), hostidentity.CertificateSHA256(issued.Leaf.Raw), &na
	ren.NotBefore = ptrTime(issued.Leaf.NotBefore)
	if err := saveHostEnrolment(ctx, r.Client, r.Config, host, &l); err != nil {
		return err
	}
	log.FromContext(ctx).Info("Validated a renewed host certificate; it is published for the host to stage", "request", reqName,
		"host", ns+"/"+name, "newKeyPin", ren.KeyPin, "sha256", ren.CertificateSHA256)
	r.event(host, corev1.EventTypeNormal, eventReasonAgentRenewalIssued, "the renewed certificate of request "+reqName+
		" is validated and published for the host to stage (sha256 "+ren.CertificateSHA256+")")
	return nil
}

// validateRenewed re-checks the Host and validates an issued renewed chain
// against the current enrolment and the renewal's key.
func (r *HostEnrolmentRequestReconciler) validateRenewed(ctx context.Context, host *infravirtrigaudiov1beta1.Host, cur *currentEnrolment,
	ren *renewalState, chain []byte) (hostidentity.Issued, error) {
	if host.Spec.Revoked || !host.DeletionTimestamp.IsZero() || host.Spec.Identity == "" {
		return hostidentity.Issued{}, errors.New("the Host is revoked, deleting or has no identity")
	}
	provider, agent, err := hostRunsAgent(ctx, r.Client, host)
	if err != nil || !agent || cur.Record.ProviderUID != string(provider.UID) {
		return hostidentity.Issued{}, errors.New("the Host's Provider does not run a per-host agent or is not the enrolment's")
	}
	rec, err := cur.Record.toRecord()
	if err != nil {
		return hostidentity.Issued{}, err
	}
	hostURI, err := hostidentity.HostURI(r.Config.TrustDomain, host.Namespace, host.Name)
	if err != nil {
		return hostidentity.Issued{}, err
	}
	roots, err := hostidentity.RootPool(r.Config.CABundle)
	if err != nil {
		return hostidentity.Issued{}, err
	}
	return hostidentity.ValidateIssued(hostidentity.Authorisation{HostName: host.Name, HostURI: hostURI, Record: rec, KeyPin: ren.KeyPin},
		chain, roots, r.now())
}

// issuedKeyWasPinned reports whether the leaf of an issued chain certifies a
// key that is pinned for the Host now (st.Current) or was pinned before
// (st.PreviousKeyPins): the certificate of a renewal that completed. A chain
// that does not parse is not one.
func issuedKeyWasPinned(chain []byte, st *hostEnrolmentState) bool {
	certs, err := hostidentity.ParseCertificates(chain)
	if err != nil || len(certs) == 0 {
		return false
	}
	pin, err := hostidentity.SPKIPin(certs[0].PublicKey)
	if err != nil {
		return false
	}
	if st.Current != nil && slices.Contains(st.Current.KeyPins, pin) {
		return true
	}
	return slices.Contains(st.PreviousKeyPins, pin)
}

// wasHostRenewal reports whether a request is, or was, a renewal the manager
// created for the Host: owned by its pins Secret (the manager creates every
// renewal request with that owner), or named by its renewal in progress.
func wasHostRenewal(cr *unstructured.Unstructured, l *loadedEnrolment) bool {
	if ren := l.state.Renewal; ren != nil && ren.CertificateRequest == cr.GetNamespace()+"/"+cr.GetName() {
		return true
	}
	if l.secret == nil || l.secret.UID == "" {
		return false
	}
	for _, o := range cr.GetOwnerReferences() {
		if o.UID == l.secret.UID {
			return true
		}
	}
	return false
}

// markRenewalWithdrawn marks as refused a renewal request that holds a
// certificate the manager will never publish (its renewal was dropped or
// replaced).
//
// When the request never was a renewal of the Host it names (neverRenewal:
// not created for it by the manager, a forged or copied request), a Warning
// AgentRenewalDenied on that Host says so, once: only the reconcile whose
// write marks the request emits it.
func (r *HostEnrolmentRequestReconciler) markRenewalWithdrawn(ctx context.Context, cr *unstructured.Unstructured,
	host *infravirtrigaudiov1beta1.Host, neverRenewal bool) error {
	if certrequest.IsTrue(cr, certrequest.ConditionRefused) {
		return nil
	}
	if err := markRefused(cr, errors.New("its renewal is no longer in progress"), r.now()); err != nil {
		return err
	}
	reqName := cr.GetNamespace() + "/" + cr.GetName()
	if err := r.Status().Update(ctx, cr); err != nil {
		return fmt.Errorf("mark renewal request %s refused: %w", reqName, err)
	}
	if neverRenewal && host != nil {
		log.FromContext(ctx).Info("Refused a renewal-shaped certificate request that was not the named Host's renewal",
			"request", reqName, "host", host.Namespace+"/"+host.Name)
		r.event(host, corev1.EventTypeWarning, eventReasonAgentRenewalDenied, "certificate request "+reqName+
			" names this Host as a renewal but was not the Host's renewal: it is marked refused and its certificate is not published")
	}
	return nil
}

// resolveHostRenewal returns the renewed certificate to render for a Host
// whose agent credentials resolved: the issued, validated certificate of its
// renewal in progress, nil when there is none.
func resolveHostRenewal(ctx context.Context, c client.Reader, cfg *HostEnrolmentConfig, host *infravirtrigaudiov1beta1.Host) []byte {
	if !devgate.Compiled || cfg == nil || host.Spec.Revoked {
		return nil
	}
	l, err := loadHostEnrolment(ctx, c, cfg, host)
	if err != nil || l.reset || l.state.Current == nil || l.state.Renewal == nil {
		return nil
	}
	return bytes.Clone(l.state.Renewal.Certificate)
}

// renewalStatus is the Host status mirror of a renewal (nil: none).
func renewalStatus(st *hostEnrolmentState) *infravirtrigaudiov1beta1.HostAgentRenewal {
	ren := st.Renewal
	if ren == nil || st.Current == nil {
		return nil
	}
	out := &infravirtrigaudiov1beta1.HostAgentRenewal{Phase: ren.phase(), KeyPin: ren.KeyPin}
	if ren.CertificateRequestUID != "" {
		out.CertificateRequest = ren.CertificateRequest
	}
	if ren.OverlapUntil != nil {
		u := *ren.OverlapUntil
		out.OverlapUntil = &u
	}
	return out
}

// ManagerIdentity is the manager's own authenticated identity, as the API
// server sees it.
type ManagerIdentity struct {
	// Username is the manager's username (its ServiceAccount's).
	Username string
	// Groups are its groups.
	Groups []string
	// UID is its user UID.
	UID string
}

// ResolveManagerIdentity asks the API server who the manager is (a
// SelfSubjectReview, which every authenticated user may create).
func ResolveManagerIdentity(ctx context.Context, c client.Client) (ManagerIdentity, error) {
	rev := &authenticationv1.SelfSubjectReview{}
	if err := c.Create(ctx, rev); err != nil {
		return ManagerIdentity{}, fmt.Errorf("self subject review: %w", err)
	}
	ui := rev.Status.UserInfo
	if ui.Username == "" {
		return ManagerIdentity{}, errors.New("self subject review: no username")
	}
	return ManagerIdentity{Username: ui.Username, Groups: slices.Clone(ui.Groups), UID: ui.UID}, nil
}

// SetManagerIdentity records the manager's own identity in the enrolment
// configuration: renewal requests are created under it and only requests
// recorded under it are approved as renewals. An identity without a
// username clears it (renewal off).
func (cfg *HostEnrolmentConfig) SetManagerIdentity(id ManagerIdentity) {
	if id.Username == "" {
		cfg.managerID.Store(nil)
		return
	}
	c := ManagerIdentity{Username: id.Username, Groups: slices.Clone(id.Groups), UID: id.UID}
	cfg.managerID.Store(&c)
}

// managerIdentity returns the manager's own identity, and whether it is
// known.
func (cfg *HostEnrolmentConfig) managerIdentity() (ManagerIdentity, bool) {
	id := cfg.managerID.Load()
	if id == nil {
		return ManagerIdentity{}, false
	}
	return *id, true
}

// ManagerIdentityResolver reads the manager's own identity until it has it:
// at once, then every Every while the API server cannot tell it (renewal
// stays off until then). It runs on every replica.
type ManagerIdentityResolver struct {
	// Client creates the SelfSubjectReview (a client that reads the API
	// server directly).
	Client client.Client
	// Config receives the identity.
	Config *HostEnrolmentConfig
	// Every is the pause between attempts (zero: one minute).
	Every time.Duration
}

// Start implements manager.Runnable.
func (m *ManagerIdentityResolver) Start(ctx context.Context) error {
	every := m.Every
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, ok := m.Config.managerIdentity(); ok {
			return nil
		}
		id, err := ResolveManagerIdentity(ctx, m.Client)
		if err == nil {
			m.Config.SetManagerIdentity(id)
			log.FromContext(ctx).Info("Per-host agent certificate renewal is on: the manager's identity is known", "username", id.Username)
			return nil
		}
		log.FromContext(ctx).Info("The manager's identity cannot be read yet; certificate renewal stays off", "error", err.Error())
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: every
// replica needs its identity.
func (m *ManagerIdentityResolver) NeedLeaderElection() bool { return false }
