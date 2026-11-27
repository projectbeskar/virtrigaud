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

// The attestation driver of the host ledger (development builds only: it
// runs only for a manager with host ledger keys, and only for hosts whose
// agent speaks the host ledger protocol).
//
// On every verified host answer to a request that asked for the ledger
// state, the driver:
//
//  1. starts or finds the store's ledger (Initialise from the answer's
//     ledger evidence, or Rebind for a replaced Host);
//  2. for an agent awaiting attestation, asks it for an attestation
//     challenge, verifies the signed challenge, begins the session in the
//     ledger and has an attestation grant signed from the ledger record its
//     own write returned (sendAttest); a verified, nonce-bound answer naming
//     a pending session adopts it;
//  3. for an attested agent in the ledger's current session, raises the
//     ledger to the agent's verified chain position (at most once per 30
//     seconds per host) and has a ledger mark signed from the record of its
//     own write (sendLedgerMark) when the agent's mark is behind;
//  4. for an attested agent in a session the ledger does not hold (a
//     delayed attestation, a restored ledger), asks for a re-attestation,
//     at most once a minute; the ledger's current session stays current
//     until the new one is adopted.
//
// A challenge the agent refuses is asked again on a later answer; an agent
// that reports its store quarantined holds the host. Authority traffic
// (challenges, attestations, heartbeat raises and their marks) and
// operation traffic (receipt raises and their marks) draw from separate
// token buckets, so neither can starve the other. Nothing here logs or
// records a key, a tag, a MAC, a chain head or a session id.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// ledgerAgentAPIVersion is the first agent API version the manager drives
// a host ledger for: the version that serves ledger marks and signs
// receipts. Hosts below it get no ledger version in their requests and
// nothing signed at grant version 2.
const ledgerAgentAPIVersion uint32 = 6

// Pacing of the attestation driver per host.
const (
	// attestRetryInterval is the least time between two attestation
	// challenges of one host for an agent awaiting attestation.
	attestRetryInterval = 2 * time.Second
	// reattestInterval is the least time between two re-attestation
	// challenges of one host.
	reattestInterval = time.Minute
)

// Token bucket rates of the host ledger's API and agent traffic:
// attestation (challenges and attestation grants), heartbeat raises and
// their marks, and operation traffic (receipt raises and their marks) have a
// bucket each, so none can starve another.
const (
	attestBucketRate     = 20
	attestBucketBurst    = 40
	heartbeatBucketRate  = 50
	heartbeatBucketBurst = 100
	operationBucketRate  = 200
	operationBucketBurst = 400
)

// LedgerReasonAgentQuarantined is the hold reason while the host's agent reports its store
// quarantined (an attestation or ledger mark it refused); nothing is signed
// for the host.
const LedgerReasonAgentQuarantined = "agent_quarantined"

// errLedgerNotAttested is grantReady's answer for a host whose agent speaks
// the host ledger protocol and is not attested in the ledger's adopted
// session.
var errLedgerNotAttested = errors.New("the host's agent is not attested in its host ledger's adopted session")

// errLedgerBucket: the host ledger's token bucket for the traffic is empty;
// the step is taken on a later answer.
var errLedgerBucket = errors.New("the host ledger traffic bucket is empty")

// tokenBucket is a token bucket refilled at rate per second up to burst.
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

// allow takes a token at now, if there is one.
func (b *tokenBucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last.IsZero() {
		b.tokens, b.last = b.burst, now
	}
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = min(b.burst, b.tokens+el*b.rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// ledgerTraffic is the kind of host ledger traffic a step belongs to.
type ledgerTraffic int

const (
	// trafficAttest: attestation challenges and attestation grants.
	trafficAttest ledgerTraffic = iota
	// trafficHeartbeat: heartbeat raises and their marks.
	trafficHeartbeat
	// trafficOperation: receipt raises and their marks.
	trafficOperation
)

// ledgerBuckets are the split token buckets of the host ledger.
type ledgerBuckets struct {
	once      sync.Once
	attest    tokenBucket
	heartbeat tokenBucket
	operation tokenBucket
}

// allow takes a token of traffic t at now.
func (b *ledgerBuckets) allow(t ledgerTraffic, now time.Time) bool {
	b.once.Do(func() {
		b.attest.rate, b.attest.burst = attestBucketRate, attestBucketBurst
		b.heartbeat.rate, b.heartbeat.burst = heartbeatBucketRate, heartbeatBucketBurst
		b.operation.rate, b.operation.burst = operationBucketRate, operationBucketBurst
	})
	switch t {
	case trafficOperation:
		return b.operation.allow(now)
	case trafficHeartbeat:
		return b.heartbeat.allow(now)
	}
	return b.attest.allow(now)
}

// hostLedgerState is what the driver knows about one host: whether its
// last verified host answer is in the ledger's adopted session, the ledger
// (store and generation) it is in, and the driver's pacing.
type hostLedgerState struct {
	ready      bool
	session    [16]byte
	store      [16]byte
	generation uint64
	attestAt   time.Time
	reattestAt time.Time
	driveAt    time.Time
}

// inLedgerSession reports whether the verified answer v names the session
// the driver saw the host attested in.
func (h *HostAnswers) inLedgerSession(provider *infravirtrigaudiov1beta1.Provider, hostID string, v hostidentity.VerifiedAnswer) bool {
	st := h.ledgerOf(provider, hostID)
	s, ok := sessionOf(v.Context)
	return ok && st.ready && st.session == s && v.Ready()
}

// ledgerAsk returns the ledger version a host answer request of the host
// asks for: the manager's when a ledger is attached and the host's last
// verified answer came from an agent that speaks the protocol; else 0.
func (h *HostAnswers) ledgerAsk(provider *infravirtrigaudiov1beta1.Provider, hostID string) uint32 {
	if h.Ledger == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.hosts[hostAnswerKey(provider, hostID)]; s != nil && s.hostAPIVersion >= ledgerAgentAPIVersion {
		return hostidentity.LedgerVersion
	}
	return 0
}

// ledgerOf returns the driver's state of a host (zero when none). The
// caller must not hold h.mu.
func (h *HostAnswers) ledgerOf(provider *infravirtrigaudiov1beta1.Provider, hostID string) hostLedgerState {
	h.lmu.Lock()
	defer h.lmu.Unlock()
	return h.ledgers[hostAnswerKey(provider, hostID)]
}

// updateLedger applies f to the driver's state of a host.
func (h *HostAnswers) updateLedger(provider *infravirtrigaudiov1beta1.Provider, hostID string, f func(*hostLedgerState)) {
	h.lmu.Lock()
	defer h.lmu.Unlock()
	if h.ledgers == nil {
		h.ledgers = map[string]hostLedgerState{}
	}
	st := h.ledgers[hostAnswerKey(provider, hostID)]
	f(&st)
	h.ledgers[hostAnswerKey(provider, hostID)] = st
}

// paced reports whether the driver may take a paced step of a host now
// (the last one at *at is at least interval ago), and records it.
func (h *HostAnswers) paced(provider *infravirtrigaudiov1beta1.Provider, hostID string, interval time.Duration,
	at func(*hostLedgerState) *time.Time) bool {
	ok := false
	now := h.now()
	h.updateLedger(provider, hostID, func(st *hostLedgerState) {
		t := at(st)
		if t.IsZero() || now.Sub(*t) >= interval {
			*t, ok = now, true
		}
	})
	return ok
}

// positionOf is the ledger position of a verified answer's chain position
// and its checkpoint (Seq 0 when none).
func positionOf(v hostidentity.VerifiedAnswer) (HostLedgerPosition, HostLedgerPosition) {
	var cp HostLedgerPosition
	if seq := v.Context.GetCheckpointSeq(); seq > 0 && seq%hostidentity.CheckpointInterval == 0 {
		cp.Seq = seq
		copy(cp.Head[:], v.Context.GetCheckpointHead())
	}
	return HostLedgerPosition{Seq: v.Position.Seq, Head: v.Position.Head}, cp
}

// sessionOf is the attestation session an answer's context names.
func sessionOf(c *providerv1.AnswerContext) ([16]byte, bool) {
	var s [16]byte
	if len(c.GetAttestationId()) != len(s) {
		return s, false
	}
	copy(s[:], c.GetAttestationId())
	return s, true
}

// driveLedger is the attestation driver's step for a verified host answer
// v of a read begun with hr that asked for the ledger state (see the file
// comment). Every failure is logged and leaves the host unattested for
// signing; the next answer drives again.
func (h *HostAnswers) driveLedger(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, reader contracts.HostAnswerReader,
	hostID string, hr hostRead, v hostidentity.VerifiedAnswer) {
	logger := log.FromContext(ctx).WithValues("host", hostID)
	l := h.Ledger
	h.updateLedger(provider, hostID, func(st *hostLedgerState) { st.ready = false })
	ev, err := v.LedgerEvidence()
	if err != nil {
		return
	}
	l.Observe(provider, hostID, ev)
	auth, _ := reader.(contracts.HostAuthorityCaller)
	store := v.Position.StoreID
	rec, err := l.LoadBound(ctx, provider, hostID, store)
	switch {
	case errors.Is(err, ErrHostLedgerNotFound):
		var res HostLedgerInitResult
		res, err = l.Initialise(ctx, provider, hostID, HostLedgerInit{StoreID: store, Evidence: ev})
		rec = res.Record
		if err == nil && len(res.Archived) > 0 {
			logger.Info("A new store replaced the host's earlier stores; their attempts stay as they are", "archived", len(res.Archived))
		}
	case errors.Is(err, ErrHostLedgerNotBound):
		// The ledger serves an earlier Host object of this name or machine:
		// it moves to this one only under the rebind conditions, and is
		// Pending afterwards (the agent is attested again).
		rec, err = l.Rebind(ctx, provider, hostID, store)
	}
	if err != nil || rec == nil {
		logger.Info("The host's ledger cannot be used now; nothing is signed for the host", "error", errString(err))
		return
	}
	switch ev.SessionState() {
	case providerv1.LedgerSessionState_LEDGER_SESSION_STATE_AWAITING_ATTESTATION:
		if auth != nil && h.paced(provider, hostID, h.attestRetry(), func(st *hostLedgerState) *time.Time { return &st.attestAt }) {
			h.attest(ctx, provider, auth, hostID, hr.trust, rec, providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST)
		}
	case providerv1.LedgerSessionState_LEDGER_SESSION_STATE_ATTESTED:
		session := ev.Session()
		if rec.HasPendingSession(session) {
			if rec, err = l.Adopt(ctx, provider, hostID, store, session, LedgerEvidenceHostAnswer); err != nil {
				logger.Info("The host's attestation session was not adopted", "error", err.Error())
				return
			}
		}
		if rec.AttestationState == infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested && rec.AttestationID == session {
			h.inSession(ctx, provider, auth, hostID, hr.trust, rec, v, ev)
			return
		}
		if auth != nil && h.paced(provider, hostID, reattestInterval, func(st *hostLedgerState) *time.Time { return &st.reattestAt }) {
			h.attest(ctx, provider, auth, hostID, hr.trust, rec, providerv1.ChallengePurpose_CHALLENGE_PURPOSE_REATTEST)
		}
	}
}

// inSession is the driver's step for an agent attested in the ledger's
// adopted session rec: a heartbeat raise to the answer's position, a ledger
// mark when the agent's mark is behind, and the host marked ready for
// grant version 2 when its answer is READY.
func (h *HostAnswers) inSession(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, auth contracts.HostAuthorityCaller,
	hostID string, t HostTrust, rec *HostLedgerRecord, v hostidentity.VerifiedAnswer, ev hostidentity.LedgerEvidence) {
	logger := log.FromContext(ctx).WithValues("host", hostID)
	l := h.Ledger
	// The per-host throttle is checked before a heartbeat token is taken, so
	// throttled hosts never drain the bucket.
	if l.heartbeatDue(provider, hostID) && h.buckets.allow(trafficHeartbeat, h.now()) {
		pos, cp := positionOf(v)
		got, err := l.Raise(ctx, provider, hostID, rec.StoreID, []HostLedgerRaise{{Session: rec.AttestationID, Position: pos, Checkpoint: cp,
			Heartbeat: true}})
		if err != nil {
			logger.Info("The host's ledger was not raised from its answer", "error", err.Error())
			return
		}
		rec = got
	}
	if markSeq, _ := ev.Mark(); markSeq < rec.Position.Seq && auth != nil {
		if err := h.markHost(ctx, provider, auth, hostID, t, rec, trafficHeartbeat); err != nil {
			logger.Info("The host's ledger mark was not raised", "error", err.Error())
		}
	}
	h.markReady(ctx, provider, hostID, rec.StoreID, v)
}

// markReady marks the host ready for grant version 2 only after an explicit
// check, independent of any bucket or throttle: a fresh bound read of the
// store's ledger (bound to the Host object the host id names, with its
// identity, not archived), Attested in the session the verified answer v
// names, v READY, and nothing holding the host.
func (h *HostAnswers) markReady(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, store [16]byte,
	v hostidentity.VerifiedAnswer) {
	session, ok := sessionOf(v.Context)
	if !ok || !v.Ready() || v.Position.StoreID != store {
		return
	}
	rec, err := h.Ledger.LoadBound(ctx, provider, hostID, store)
	if err != nil || rec.Archived || rec.AttestationState != infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested ||
		rec.AttestationID != session || h.Ledger.HeldReason(provider, hostID) != "" {
		return
	}
	h.updateLedger(provider, hostID, func(st *hostLedgerState) {
		st.ready, st.session, st.store, st.generation = true, rec.AttestationID, rec.StoreID, rec.Generation
	})
}

// attest asks the host for an attestation challenge of purpose, begins its
// session in the ledger rec and delivers an attestation grant signed from
// the record of that write; a verified nonce-bound answer naming a pending
// session adopts it.
func (h *HostAnswers) attest(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, auth contracts.HostAuthorityCaller,
	hostID string, t HostTrust, rec *HostLedgerRecord, purpose providerv1.ChallengePurpose) {
	logger := log.FromContext(ctx).WithValues("host", hostID, "purpose", purpose.String())
	l := h.Ledger
	if !h.buckets.allow(trafficAttest, h.now()) {
		return
	}
	read, err := auth.Challenge(ctx, contracts.ChallengeQuery{HostID: hostID, Purpose: purpose})
	if read.Answer == nil {
		logger.Info("No signed attestation challenge from the host", "error", errString(err))
		return
	}
	exp := t.expectation(provider, hostID, hostidentity.BodyTypeChallengeAnswer, read.Nonce, h.now())
	exp.ChallengePurpose = purpose
	vc, err := hostidentity.VerifyAnswer(blobOf(read.Answer), exp)
	if err != nil {
		logger.Info("The host's attestation challenge did not verify; nothing is signed", "check", err.Error())
		return
	}
	if vc.Position.StoreID != rec.StoreID {
		logger.Info("The host's attestation challenge names another store than its ledger; nothing is signed")
		return
	}
	if ev, err := vc.LedgerEvidence(); err == nil {
		l.Observe(provider, hostID, ev)
	}
	ch := vc.Challenge
	switch ch.GetNotIssuedReason() {
	case "":
	case hostidentity.ChallengeNotIssuedQuarantined:
		h.holdQuarantined(ctx, provider, hostID, rec.StoreID)
		return
	default:
		logger.Info("The host issued no attestation challenge; it is asked again later", "reason", ch.GetNotIssuedReason())
		return
	}
	if s, ok := sessionOf(vc.Context); ok && rec.HasPendingSession(s) {
		if _, err := l.Adopt(ctx, provider, hostID, rec.StoreID, s, LedgerEvidenceChallengeAnswer); err != nil {
			logger.Info("The host's attestation session was not adopted", "error", err.Error())
		}
		return
	}
	var nonce [16]byte
	copy(nonce[:], ch.GetChallengeNonce())
	// An agent awaiting attestation begins a session that makes the ledger
	// Pending (it runs nothing until attested anyway). A re-attestation of
	// an attested agent keeps the ledger's current session current and
	// Attested until the new one is adopted; a ledger that is Pending (a
	// rebind) stays Pending.
	ledgerPurpose := LedgerSessionAttest
	if purpose == providerv1.ChallengePurpose_CHALLENGE_PURPOSE_REATTEST &&
		rec.AttestationState == infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested {
		ledgerPurpose = LedgerSessionReattest
	}
	begun, err := l.BeginSession(ctx, provider, hostID, rec.StoreID, nonce, ledgerPurpose,
		HostLedgerPosition{Seq: vc.Position.Seq, Head: vc.Position.Head})
	if err != nil {
		logger.Info("The attestation session was not begun in the host's ledger; nothing is signed", "error", err.Error())
		return
	}
	ar, err := h.sendAttest(ctx, provider, auth, hostID, t, begun, nonce)
	if ar.Answer == nil {
		logger.Info("No signed answer to the attestation", "error", errString(err))
		return
	}
	expA := t.expectation(provider, hostID, hostidentity.BodyTypeAuthorityAnswer, ar.Nonce, h.now())
	expA.GrantKind, expA.GrantDigest = providerv1.GrantKind_GRANT_KIND_ATTEST, ar.GrantDigest
	va, err := hostidentity.VerifyAnswer(blobOf(ar.Answer), expA)
	if err != nil {
		logger.Info("The host's answer to the attestation did not verify", "check", err.Error())
		return
	}
	switch a := va.Authority; a.GetOutcome() {
	case providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_ALREADY_APPLIED:
		if s, ok := sessionOf(va.Context); ok && s == nonce {
			if _, err := l.Adopt(ctx, provider, hostID, rec.StoreID, s, LedgerEvidenceAuthorityAnswer); err != nil {
				logger.Info("The host's attestation session was not adopted", "error", err.Error())
			}
		}
	case providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_QUARANTINED:
		h.holdQuarantined(ctx, provider, hostID, rec.StoreID)
	default:
		logger.Info("The host refused the attestation; it is challenged again later", "outcome", a.GetOutcome().String(),
			"reason", a.GetOutcomeReason())
	}
}

// errNotAttestable: sendAttest's refusal of a record it may not sign an
// attestation grant from.
var errNotAttestable = errors.New("no ledger object of the store holds this attestation session as begun by this manager")

// sendAttest is the only place the manager has an attestation grant
// signed. It signs only from rec, the ledger record its own successful
// BeginSession write returned for store rec.StoreID (a record carries a
// resourceVersion only when it was read from or written as an existing,
// tag-verified ledger object), holding nonce as a pending session; and only
// after re-reading that ledger object and finding it unchanged since. No
// ledger object for the store: no grant.
func (h *HostAnswers) sendAttest(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, auth contracts.HostAuthorityCaller,
	hostID string, t HostTrust, rec *HostLedgerRecord, nonce [16]byte) (contracts.AuthorityRead, error) {
	if h.Ledger == nil || rec == nil || rec.resourceVersion == "" || rec.Archived || !rec.HasPendingSession(nonce) {
		return contracts.AuthorityRead{}, errNotAttestable
	}
	cur, err := h.Ledger.Load(ctx, provider, hostID, rec.StoreID)
	if err != nil {
		return contracts.AuthorityRead{}, fmt.Errorf("%w: %w", errNotAttestable, err)
	}
	if cur.WriteCounter != rec.WriteCounter || cur.StoreID != rec.StoreID || !cur.HasPendingSession(nonce) {
		return contracts.AuthorityRead{}, fmt.Errorf("the ledger changed since its session was begun: %w", errNotAttestable)
	}
	o := contracts.AttestOrder{HostID: hostID, ProviderUID: string(provider.UID), EnrolmentID: bytes.Clone(t.Enrolment.EnrolmentID),
		Epoch:          contracts.OperationEpoch{Counter: t.Enrolment.Epoch.Counter, Holder: bytes.Clone(t.Enrolment.Epoch.Holder)},
		ChallengeNonce: bytes.Clone(nonce[:]), Ledger: contracts.LedgerPosition{StoreID: bytes.Clone(rec.StoreID[:]),
			Seq: rec.Position.Seq, Head: bytes.Clone(rec.Position.Head[:])}}
	if rec.Checkpoint.Seq > 0 {
		o.Checkpoint = &contracts.LedgerPosition{StoreID: bytes.Clone(rec.StoreID[:]), Seq: rec.Checkpoint.Seq,
			Head: bytes.Clone(rec.Checkpoint.Head[:])}
	}
	return auth.Attest(ctx, o)
}

// errNotMarkable: sendLedgerMark's refusal of a record it may not sign a
// ledger mark from.
var errNotMarkable = errors.New("no ledger record of this manager's own write in an adopted session to mark from")

// markHost raises the host's ledger mark to the ledger rec (traffic t) and
// verifies the answer.
func (h *HostAnswers) markHost(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, auth contracts.HostAuthorityCaller,
	hostID string, t HostTrust, rec *HostLedgerRecord, traffic ledgerTraffic) error {
	if !h.buckets.allow(traffic, h.now()) {
		return errLedgerBucket
	}
	ar, err := h.sendLedgerMark(ctx, provider, auth, hostID, t, rec)
	if ar.Answer == nil {
		if err == nil {
			// Above the agent's signing limit the mark is applied unsigned.
			return nil
		}
		return err
	}
	exp := t.expectation(provider, hostID, hostidentity.BodyTypeAuthorityAnswer, ar.Nonce, h.now())
	exp.GrantKind, exp.GrantDigest = providerv1.GrantKind_GRANT_KIND_LEDGER_MARK, ar.GrantDigest
	va, err := hostidentity.VerifyAnswer(blobOf(ar.Answer), exp)
	if err != nil {
		return fmt.Errorf("the answer to the ledger mark did not verify: %w", err)
	}
	switch a := va.Authority; a.GetOutcome() {
	case providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_ALREADY_APPLIED:
		return nil
	case providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_QUARANTINED:
		h.holdQuarantined(ctx, provider, hostID, rec.StoreID)
		return fmt.Errorf("the host's agent quarantined its store on the ledger mark")
	default:
		return fmt.Errorf("the host refused the ledger mark (%s, %s)", a.GetOutcome(), a.GetOutcomeReason())
	}
}

// sendLedgerMark is the only place the manager has a ledger mark grant
// signed. It signs only from the record of this manager's own last
// successful write of the store's ledger, and only while the read record
// rec is that write (nobody wrote since), in an adopted session.
func (h *HostAnswers) sendLedgerMark(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, auth contracts.HostAuthorityCaller,
	hostID string, t HostTrust, rec *HostLedgerRecord) (contracts.AuthorityRead, error) {
	if h.Ledger == nil || rec == nil {
		return contracts.AuthorityRead{}, errNotMarkable
	}
	own := h.Ledger.ownRecord(rec.StoreID)
	if own == nil || own.WriteCounter != rec.WriteCounter || own.Archived ||
		own.AttestationState != infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested || own.AttestationID == ([16]byte{}) {
		return contracts.AuthorityRead{}, errNotMarkable
	}
	o := contracts.LedgerMarkOrder{HostID: hostID, ProviderUID: string(provider.UID), EnrolmentID: bytes.Clone(t.Enrolment.EnrolmentID),
		Epoch:         contracts.OperationEpoch{Counter: t.Enrolment.Epoch.Counter, Holder: bytes.Clone(t.Enrolment.Epoch.Holder)},
		AttestationID: bytes.Clone(own.AttestationID[:]), Ledger: contracts.LedgerPosition{StoreID: bytes.Clone(own.StoreID[:]),
			Seq: own.Position.Seq, Head: bytes.Clone(own.Position.Head[:])}}
	return auth.LedgerMark(ctx, o)
}

// holdQuarantined holds the host while its agent reports its store
// quarantined.
func (h *HostAnswers) holdQuarantined(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, store [16]byte) {
	lh, err := h.Ledger.resolve(ctx, provider, hostID)
	if err != nil {
		return
	}
	_ = h.Ledger.hold(ctx, lh, LedgerReasonAgentQuarantined, storeIDPrefix(store), false,
		"the host's agent reports its store quarantined; nothing is signed for the host", nil)
	h.updateLedger(provider, hostID, func(st *hostLedgerState) { st.ready = false })
}

// ledgerGrantReady reports whether grant version 2 may be signed for the
// host: the driver saw its last verified host answer READY in the ledger's
// adopted session of the store that answer named, and the ledger holds
// nothing. The caller holds h.mu and passes the host's state.
func (h *HostAnswers) ledgerGrantReady(provider *infravirtrigaudiov1beta1.Provider, hostID string, s *hostAnswerState) error {
	st := h.ledgerOf(provider, hostID)
	switch {
	case !st.ready || st.session != s.hostSession || st.store != s.hostAnswerStore:
		return errLedgerNotAttested
	case h.Ledger.HeldReason(provider, hostID) != "":
		return fmt.Errorf("%w: the host's ledger holds it (%s)", errLedgerNotAttested, h.Ledger.HeldReason(provider, hostID))
	}
	return nil
}

// sendVersion is the grant version a send to the host is signed at: 0
// (the original version, as before) for a host the host ledger does not
// drive; LedgerGrantVersion for a host it drives that is attested in the
// ledger's adopted session; ok is false for a host it drives that is not,
// and nothing is sent to it.
func (h *HostAnswers) sendVersion(provider *infravirtrigaudiov1beta1.Provider, hostID string) (uint32, bool) {
	if h.Ledger == nil {
		return 0, true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.hosts[hostAnswerKey(provider, hostID)]
	if s == nil || s.hostAPIVersion < ledgerAgentAPIVersion || !slices.Contains(s.hostGrantVersions, grantverify.LedgerGrantVersion) {
		return 0, true
	}
	if h.ledgerGrantReady(provider, hostID, s) != nil {
		return 0, false
	}
	return grantverify.LedgerGrantVersion, true
}

// DriveHostLedger asks the host for a signed host answer with its ledger
// state and runs the attestation driver on it, twice at most (an
// attestation adopted on the first is seen in the second answer). It is a
// no-op without a host ledger or for a host the ledger does not drive.
func (h *HostAnswers) DriveHostLedger(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, reader contracts.HostAnswerReader,
	hostID string) {
	if h.Ledger == nil || !h.paced(provider, hostID, h.attestRetry(), func(st *hostLedgerState) *time.Time { return &st.driveAt }) {
		return
	}
	for range 2 {
		if _, err := h.refreshHost(ctx, provider, reader, hostID); err != nil || h.ledgerOf(provider, hostID).ready {
			return
		}
	}
}

// LedgerStamp returns the host ledger (store and generation) a new attempt
// on the host is prepared under: the one the driver saw the host attested
// in; ok is false when the host is not ready for grant version 2.
func (h *HostAnswers) LedgerStamp(provider *infravirtrigaudiov1beta1.Provider, hostID string) (store [16]byte, generation uint64, ok bool) {
	st := h.ledgerOf(provider, hostID)
	return st.store, st.generation, st.ready && st.generation > 0
}

// attestRetry is the least time between two attestation challenges (and
// on-demand drives) of one host for an agent awaiting attestation.
func (h *HostAnswers) attestRetry() time.Duration {
	if h.AttestRetryInterval > 0 {
		return h.AttestRetryInterval
	}
	return attestRetryInterval
}
