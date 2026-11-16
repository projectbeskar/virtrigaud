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
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
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
	// NotAfter is when the renewed certificate expires.
	NotAfter *metav1.Time `json:"notAfter,omitempty"`
	// PinnedAt is when the new key was pinned beside the old one, and
	// OverlapUntil when the overlap is expected to end.
	PinnedAt     *metav1.Time `json:"pinnedAt,omitempty"`
	OverlapUntil *metav1.Time `json:"overlapUntil,omitempty"`
	// OverlapExpiredReported records that the overlap's expiry was reported.
	OverlapExpiredReported bool `json:"overlapExpiredReported,omitempty"`
	// UnsupportedReported records that the manager reported it cannot
	// renew (no cert-manager issuer).
	UnsupportedReported bool `json:"unsupportedReported,omitempty"`
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

// renewalRequestName returns the name of the CertificateRequest of a
// renewal: a hash of the Host, the new key and when the renewal was
// recorded, so a retried create finds its own request.
func renewalRequestName(namespace, name string, r *renewalState) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name + "/" + r.KeyPin + "/" + r.Since.UTC().Format(time.RFC3339)))
	return renewalRequestPrefix + hex.EncodeToString(sum[:12])
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
		cur.NotAfter = *r.NotAfter
	}
	st.Renewal = nil
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
		if st.keyPreviouslyPinned(pr.KeyPin) {
			out.keyWasPinned = pr.KeyPin
		} else {
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
// manager's identity, reports an overlap that outlasted renewalOverlap, and
// unpins the old key once the old certificate has expired (it can no longer
// authenticate the host). It reports whether it changed the state and when to
// look again.
func (r *HostEnrolmentReconciler) reconcileRenewal(ctx context.Context, host *infravirtrigaudiov1beta1.Host,
	st *hostEnrolmentState) (changed bool, recheck time.Duration, err error) {
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
	if ren.CertificateRequestUID != "" {
		return false, 0, nil
	}
	created, err := r.createRenewalRequest(ctx, host, st)
	return created, 0, err
}

// createRenewalRequest creates the CertificateRequest of a recorded renewal
// in the enrolment namespace, under the manager's own identity, for the
// cert-manager issuer of the sub-CA, in two steps: the request's name is
// recorded first (and saved by the caller), so the request is never seen
// before the pins Secret names it; the next reconcile creates it and records
// its UID. A request of the same name that is already there (a retried
// create) is taken when it is the manager's renewal request for this Host
// with this request; anything else of that name is left alone and the
// renewal is dropped.
func (r *HostEnrolmentReconciler) createRenewalRequest(ctx context.Context, host *infravirtrigaudiov1beta1.Host,
	st *hostEnrolmentState) (bool, error) {
	ren := st.Renewal
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
	case r.Config.ManagerUsername == "":
		return false, errNoManagerIdentity
	}
	body, current, err := ren.body()
	if err != nil {
		st.Renewal = nil
		return true, nil //nolint:nilerr // a stored request that does not decode is dropped; the host offers it again
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: body.GetCertificateRequest()})
	name := renewalRequestName(host.Namespace, host.Name, ren)
	if ren.CertificateRequest != r.Config.Namespace+"/"+name {
		ren.CertificateRequest = r.Config.Namespace + "/" + name
		return true, nil
	}
	cr := certrequest.NewRenewal(certrequest.RenewalSpec{
		Namespace: r.Config.Namespace, Name: name, HostNamespace: host.Namespace, HostName: host.Name,
		HostHash: hostHashLabel(host.Namespace, host.Name), IssuerRef: *r.Config.CertManagerIssuer, CSRPEM: csrPEM,
		Duration: hostidentity.RenewalLifetime(current), Username: r.Config.ManagerUsername, Groups: r.Config.ManagerGroups,
		UID: r.Config.ManagerUID,
	})
	err = r.Create(ctx, cr)
	if apierrors.IsAlreadyExists(err) {
		existing := certrequest.NewObject()
		if gerr := r.liveReader().Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: name}, existing); gerr != nil {
			return false, fmt.Errorf("get renewal request %s/%s: %w", r.Config.Namespace, name, gerr)
		}
		got, cerr := certrequest.CSR(existing)
		ns, hn, ok := certrequest.HostOf(existing)
		if cerr != nil || !ok || ns != host.Namespace || hn != host.Name || !certrequest.IsRenewal(existing) || !bytes.Equal(got, csrPEM) {
			st.Renewal = nil
			r.event(host, corev1.EventTypeWarning, eventReasonAgentRenewalCreateRefused, "an object named "+name+
				" that is not this renewal's request exists in the enrolment namespace; the renewal starts again")
			return true, nil
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
	reqName := cr.GetNamespace() + "/" + cr.GetName()
	in, err := certrequest.RequestOf(cr)
	if err != nil {
		return r.denyRenewal(ctx, cr, nil, hostidentity.ReasonRequestInvalid, err.Error())
	}
	if r.Config.ManagerUsername == "" || in.Username != r.Config.ManagerUsername {
		return r.denyRenewal(ctx, cr, nil, reasonRenewalRequester,
			"a renewal request is approved only when the manager created it; this one was created by another principal")
	}
	if iss.Name() != (certManagerIssuer{}).Name() {
		return r.denyRenewal(ctx, cr, nil, reasonRenewalIssuer, "renewal requests are issued only by the cert-manager issuer")
	}
	ns, name, ok := certrequest.HostOf(cr)
	if !ok {
		return r.denyRenewal(ctx, cr, nil, reasonEnrolHostUnnamed, "the request does not name a Host in annotation "+certrequest.AnnotationHost)
	}
	host := &infravirtrigaudiov1beta1.Host{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, host); err != nil {
		if apierrors.IsNotFound(err) {
			return r.denyRenewal(ctx, cr, nil, reasonEnrolHostNotFound, fmt.Sprintf("Host %s/%s does not exist", ns, name))
		}
		return fmt.Errorf("get Host of renewal request %s: %w", reqName, err)
	}
	switch {
	case !host.DeletionTimestamp.IsZero():
		return r.denyRenewal(ctx, cr, host, reasonEnrolHostNotFound, "the Host is being deleted")
	case host.Spec.Identity == "":
		return r.denyRenewal(ctx, cr, host, reasonEnrolIdentityMissing, "the Host has no spec.identity")
	case host.Spec.Revoked:
		return r.denyRenewal(ctx, cr, host, reasonEnrolHostRevoked, "the Host is revoked")
	}
	provider, agent, err := hostRunsAgent(ctx, r.Client, host)
	if err != nil {
		return err
	}
	if !agent {
		return r.denyRenewal(ctx, cr, host, reasonEnrolHostNotAgent, "the Host's Provider does not run a per-host agent")
	}
	l, err := loadHostEnrolment(ctx, r.liveReader(), r.Config, host)
	if err != nil {
		return err
	}
	cur, ren := l.state.Current, l.state.Renewal
	switch {
	case l.reset || cur == nil:
		return r.denyRenewal(ctx, cr, host, reasonRenewalNotEnrolled, "the Host is not enrolled")
	case ren == nil || ren.CertificateRequest != reqName || (ren.CertificateRequestUID != "" && ren.CertificateRequestUID != string(cr.GetUID())):
		return r.denyRenewal(ctx, cr, host, reasonRenewalNotPending, "the Host has no renewal in progress for this request")
	case cur.Record.ProviderUID != string(provider.UID):
		return r.denyRenewal(ctx, cr, host, reasonEnrolProviderChanged, "the Host is enrolled with another Provider than its own")
	}
	exp, err := renewalExpectation(r.Config, host, cur, string(provider.UID), r.now())
	if err != nil {
		return err
	}
	blob, err := ren.blob()
	if err != nil {
		return r.denyRenewal(ctx, cr, host, reasonRenewalNotVerified, "the stored renewal request does not decode")
	}
	v, err := hostidentity.VerifyRenewal(blob, exp)
	if err != nil || v.KeyPin != ren.KeyPin {
		why := "the renewal names another key than recorded"
		if err != nil {
			why = err.Error()
		}
		return r.denyRenewal(ctx, cr, host, reasonRenewalNotVerified, why)
	}
	if err := hostidentity.CheckRenewalRequestSpec(hostidentity.RenewalRequestSpec{CSRPEM: in.CSRPEM, Usages: in.Usages,
		Duration: in.Duration, IsCA: in.IsCA}, v); err != nil {
		if d, ok := hostidentity.AsDenial(err); ok {
			return r.denyRenewal(ctx, cr, host, d.Reason, d.Message)
		}
		return err
	}
	if slices.Contains(l.state.PreviousKeyPins, v.KeyPin) || slices.Contains(cur.KeyPins, v.KeyPin) {
		return r.denyRenewal(ctx, cr, host, reasonEnrolKeyPreviouslyPinned, "the renewal's key is, or was, pinned for this Host")
	}
	// Record the approval first, then approve: a crash in between leaves an
	// undecided request this reconcile approves again (its UID matches).
	ren.Approved, ren.CertificateRequestUID = true, string(cr.GetUID())
	if err := saveHostEnrolment(ctx, r.Client, r.Config, host, &l); err != nil {
		return err
	}
	if err := certrequest.SetCondition(cr, certrequest.ConditionApproved, certrequest.StatusTrue, reasonEnrolAuthorised,
		"certificate renewal authorised by VirtRigaud for Host "+ns+"/"+name, r.now()); err != nil {
		return err
	}
	if err := r.Status().Update(ctx, cr); err != nil {
		return fmt.Errorf("approve renewal request %s: %w", reqName, err)
	}
	log.FromContext(ctx).Info("Approved a host-agent certificate renewal request", "request", reqName, "host", ns+"/"+name,
		"newKeyPin", v.KeyPin, "signerKeyPin", v.SignerPin)
	r.event(host, corev1.EventTypeNormal, eventReasonAgentRenewalApproved, "certificate renewal request "+reqName+" approved for new key "+v.KeyPin)
	return nil
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
		return r.markRenewalWithdrawn(ctx, cr)
	}
	if len(ren.Certificate) > 0 {
		return nil
	}
	issued, verr := r.validateRenewed(ctx, host, cur, ren, chain)
	if verr != nil {
		l.state.Renewal = nil
		if err := saveHostEnrolment(ctx, r.Client, r.Config, host, &l); err != nil {
			return err
		}
		if _, err := iss.Complete(cr, verr, r.now()); err != nil {
			return err
		}
		if err := r.Status().Update(ctx, cr); err != nil {
			return fmt.Errorf("record the outcome on renewal request %s: %w", reqName, err)
		}
		log.FromContext(ctx).Info("Refused the certificate issued for a renewal request", "request", reqName, "host", ns+"/"+name)
		r.event(host, corev1.EventTypeWarning, eventReasonAgentRenewalRefused, "the certificate issued for renewal request "+reqName+
			" was refused; the renewal starts again")
		return nil
	}
	na := metav1.NewTime(issued.Leaf.NotAfter.UTC().Truncate(time.Second))
	ren.Certificate, ren.CertificateSHA256, ren.NotAfter = bytes.Clone(issued.Leaf.Raw), hostidentity.CertificateSHA256(issued.Leaf.Raw), &na
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

// markRenewalWithdrawn marks as refused a renewal request that holds a
// certificate the manager will never publish (its renewal was dropped or
// replaced).
func (r *HostEnrolmentRequestReconciler) markRenewalWithdrawn(ctx context.Context, cr *unstructured.Unstructured) error {
	if certrequest.IsTrue(cr, certrequest.ConditionRefused) {
		return nil
	}
	if err := markRefused(cr, errors.New("its renewal is no longer in progress"), r.now()); err != nil {
		return err
	}
	if err := r.Status().Update(ctx, cr); err != nil {
		return fmt.Errorf("mark renewal request %s/%s refused: %w", cr.GetNamespace(), cr.GetName(), err)
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
	out := &infravirtrigaudiov1beta1.HostAgentRenewal{Phase: ren.phase(), KeyPin: ren.KeyPin, CertificateRequest: ren.CertificateRequest}
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
// recorded under it are approved as renewals.
func (cfg *HostEnrolmentConfig) SetManagerIdentity(id ManagerIdentity) {
	cfg.ManagerUsername, cfg.ManagerGroups, cfg.ManagerUID = id.Username, slices.Clone(id.Groups), id.UID
}
