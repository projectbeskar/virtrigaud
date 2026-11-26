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

// The host ledger writes (development builds only).
//
// HostLedger is the manager's library over the ledger objects
// (host_ledger.go). Every read is uncached (the API reader) and every change
// is one update carrying the resourceVersion it read, so a concurrent writer
// makes it fail and it is retried from a fresh read: nothing is ever
// overwritten. A record is returned only after its tag verified, or as the
// object the API server returned for this manager's own successful write;
// a caller signs only from such a record.
//
// The writes, in the order an attestation driver uses them:
//
//   - Initialise: the first ledger object of a store, from the host's last
//     verified position once the host confirmed it in a signed answer, or,
//     for a store the host's agent says was created under the current
//     enrolment and never held another, from first contact. A first contact
//     archives the ledger of the Host's previous store.
//   - Rebind: a ledger object that served an earlier Host object of the same
//     machine moves to its replacement, once the earlier Host is deleted or
//     revoked, quiet, and no other Host carries the identity.
//   - BeginSession: an attestation session (a challenge nonce) is added to
//     the pending sessions.
//   - Raise: the ledger position is raised from verified positions of the
//     current session (receipts and answers), in batches.
//   - Adopt: a pending session is made current once a nonce-bound signed
//     answer proves the agent holds it.
//
// A check that fails holds the host: the write returns a
// *HostLedgerHoldError (errors.Is ErrHostLedgerHeld), an Event is emitted on
// the Host, and the holds that cannot be recomputed from the stored objects
// (a regression or foreign change seen by this process, a fork, a store
// mismatch under one enrolment) are persisted as a store-regression hold
// that ends only with a new enrolment.
//
// Nothing here is used to sign anything yet: the attestation driver that
// calls these writes, and the signing gate that reads the ledger, come
// later. No key material, full store id, chain head or session id is logged,
// recorded in an Event or written to a status.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
)

// Reasons a host ledger holds a host. They are Event messages' and the
// AgentLedgerHeld condition's reasons, and the store-regression hold reason
// of a persisted hold.
const (
	// LedgerReasonTampered: a ledger object or the ledger index failed its
	// tag or form, or a ledger object changed in a way this manager did not
	// write.
	LedgerReasonTampered = "LedgerTampered"
	// LedgerReasonRegressed: a ledger object went back (a lower position or
	// write counter than this manager saw).
	LedgerReasonRegressed = "LedgerRegressed"
	// LedgerReasonMissing: the store has no ledger object and none may be
	// started (the index names it, or no evidence allows a first contact).
	LedgerReasonMissing = "ledger_missing"
	// LedgerReasonAhead: the ledger is above the host's chain tip.
	LedgerReasonAhead = "ledger_ahead"
	// LedgerReasonFork: two verified positions name one sequence number
	// with different heads.
	LedgerReasonFork = "ledger_fork"
	// LedgerReasonStoreMismatch: the host shows another store under an
	// enrolment that already has a ledger.
	LedgerReasonStoreMismatch = "ledger_store_mismatch"
	// LedgerReasonUnconfirmed: the host has not confirmed, in a signed
	// answer, the last position the manager would start its ledger from.
	LedgerReasonUnconfirmed = "ledger_unconfirmed"
	// LedgerReasonStoreForeign: the ledger is bound to another machine.
	LedgerReasonStoreForeign = "ledger_store_foreign"
	// LedgerReasonStoreBound: the ledger serves another Host object that is
	// live, not revoked, recently answering, or whose identity another Host
	// also carries.
	LedgerReasonStoreBound = "ledger_store_bound"
)

// Event reasons of the host ledger, emitted on the Host.
const (
	eventReasonLedgerHeld        = "AgentLedgerHeld"
	eventReasonLedgerInitialised = "AgentLedgerInitialised"
	eventReasonLedgerArchived    = "AgentLedgerArchived"
	eventReasonLedgerRebound     = "AgentLedgerRebound"
)

// HostConditionAgentLedgerHeld is True while the host's ledger holds the
// host (the reason names the hold), False once the ledger verified with no
// hold. Set only by a manager with host ledger keys. Development builds
// only.
const HostConditionAgentLedgerHeld = "AgentLedgerHeld"

// reasonLedgerNotHeld is the AgentLedgerHeld condition's reason while the
// ledger holds nothing.
const reasonLedgerNotHeld = "NotHeld"

// Timing of the host ledger.
const (
	// ledgerHeartbeatRaiseInterval is the least time between two raises of
	// one host's ledger made only from heartbeat answers.
	ledgerHeartbeatRaiseInterval = 30 * time.Second
	// ledgerIndexPersistInterval is the least time between two index writes
	// of one host that only record a higher write counter.
	ledgerIndexPersistInterval = storePersistInterval
	// maxLedgerWriteAttempts bounds the read-modify-write attempts of one
	// write that keeps losing to concurrent writers.
	maxLedgerWriteAttempts = 5
)

// ErrHostLedgerHeld is wrapped by every *HostLedgerHoldError.
var ErrHostLedgerHeld = errors.New("the host is held by its ledger")

// Errors of HostLedger that are not holds.
var (
	// ErrHostLedgerNotFound: the store has no ledger object.
	ErrHostLedgerNotFound = errors.New("the store has no host ledger object")
	// ErrHostLedgerNotBound: the ledger object serves another Host object
	// (Rebind moves it).
	ErrHostLedgerNotBound = errors.New("the host ledger serves another Host object")
	// ErrHostLedgerArchived: the ledger object is archived and never
	// written again.
	ErrHostLedgerArchived = errors.New("the host ledger is archived")
	// ErrHostLedgerNotAttested: the write needs an adopted session.
	ErrHostLedgerNotAttested = errors.New("the host ledger has no adopted attestation session")
	// ErrHostLedgerSessionNotPending: the session to adopt is not pending.
	ErrHostLedgerSessionNotPending = errors.New("the attestation session is not pending in the host ledger")
	// errHostLedgerInput: the caller passed an invalid argument.
	errHostLedgerInput = errors.New("invalid host ledger input")
	// errHostLedgerConflict: every attempt of a write lost to a concurrent
	// writer.
	errHostLedgerConflict = errors.New("the host ledger write kept conflicting with concurrent writers")
	// errHostLedgerHost: the Host cannot be served by a ledger now (gone,
	// revoked, of another Provider, without an identity).
	errHostLedgerHost = errors.New("the Host cannot be served by a host ledger")
)

// HostLedgerHoldError is the error of a write or read that holds the host.
type HostLedgerHoldError struct {
	// Reason is one of the LedgerReason* constants.
	Reason string
	// StorePrefix is the first 8 hexadecimal characters of the store id.
	StorePrefix string
	// Persistent reports that the hold was persisted: it ends only with a
	// new enrolment of the host.
	Persistent bool
	// Detail says what failed, without secrets.
	Detail string
}

// Error implements error.
func (e *HostLedgerHoldError) Error() string {
	return fmt.Sprintf("%s (%s, store %s): %s", ErrHostLedgerHeld.Error(), e.Reason, e.StorePrefix, e.Detail)
}

// Unwrap returns ErrHostLedgerHeld.
func (e *HostLedgerHoldError) Unwrap() error { return ErrHostLedgerHeld }

// HostLedgerSessionPurpose is why an attestation session is begun.
type HostLedgerSessionPurpose int

const (
	// LedgerSessionAttest: the host's agent awaits attestation (after a
	// start); the ledger becomes Pending.
	LedgerSessionAttest HostLedgerSessionPurpose = iota + 1
	// LedgerSessionReattest: the agent is attested in a session the ledger
	// does not hold; the current session stays current and Attested until
	// the new one is adopted.
	LedgerSessionReattest
)

// HostLedgerAdoptEvidence is the kind of nonce-bound signed answer that
// proves the agent holds a pending session. An operation receipt is not
// nonce-bound and is never evidence; there is no value for it.
type HostLedgerAdoptEvidence int

const (
	// LedgerEvidenceHostAnswer: a verified HostAnswer.
	LedgerEvidenceHostAnswer HostLedgerAdoptEvidence = iota + 1
	// LedgerEvidenceOperationAnswer: a verified OperationAnswer.
	LedgerEvidenceOperationAnswer
	// LedgerEvidenceVMAnswer: a verified VMAnswer.
	LedgerEvidenceVMAnswer
	// LedgerEvidenceChallengeAnswer: a verified ChallengeAnswer.
	LedgerEvidenceChallengeAnswer
	// LedgerEvidenceAuthorityAnswer: a verified AuthorityAnswer.
	LedgerEvidenceAuthorityAnswer
)

// valid reports whether e is one of the defined kinds.
func (e HostLedgerAdoptEvidence) valid() bool {
	return e >= LedgerEvidenceHostAnswer && e <= LedgerEvidenceAuthorityAnswer
}

// HostLedgerOrigin is the store-origin evidence of a verified
// ChallengeAnswer (its HostLedgerState): the enrolment under which the
// store was created and how many enrolments it ever held.
type HostLedgerOrigin struct {
	// OriginEnrolmentID is the store's origin enrolment (16 bytes); empty
	// for a store that predates the record.
	OriginEnrolmentID []byte
	// EnrolmentCount is how many enrolments were ever activated in the
	// store; 0 for a store that predates the record.
	EnrolmentCount uint32
}

// HostLedgerContinuity is the confirmation of a verified host answer to a
// request whose continuity_seq named Seq: the head the host signed at Seq.
type HostLedgerContinuity struct {
	// Seq is the request's continuity_seq.
	Seq uint64
	// Head is the answer's continuity_head.
	Head [32]byte
}

// HostLedgerInit is the input of Initialise.
type HostLedgerInit struct {
	// StoreID is the host's store (from the verified answer).
	StoreID [16]byte
	// Position and Checkpoint are the store's chain position and checkpoint
	// in the verified answer the origin evidence came from (a first contact
	// starts the ledger there).
	Position, Checkpoint HostLedgerPosition
	// Origin is the store-origin evidence of a verified ChallengeAnswer;
	// nil when none.
	Origin *HostLedgerOrigin
	// Continuity is the host's confirmation of the last verified position
	// (see Initialise); nil when none.
	Continuity *HostLedgerContinuity
}

// HostLedgerInitSource says where Initialise took a ledger from.
type HostLedgerInitSource string

const (
	// LedgerInitExisting: the store already had a ledger object.
	LedgerInitExisting HostLedgerInitSource = "Existing"
	// LedgerInitLastPosition: started from the host's last verified
	// position, confirmed by the host.
	LedgerInitLastPosition HostLedgerInitSource = "LastPosition"
	// LedgerInitFirstContact: started from the agent's signed position for
	// a store created under the current enrolment.
	LedgerInitFirstContact HostLedgerInitSource = "FirstContact"
)

// HostLedgerInitResult is the result of Initialise.
type HostLedgerInitResult struct {
	// Record is the store's ledger.
	Record *HostLedgerRecord
	// Source says where it came from.
	Source HostLedgerInitSource
	// Archived are the ledgers of the Host's earlier stores that a first
	// contact archived: the caller holds the open attempts of their
	// enrolments as lost with the store.
	Archived []*HostLedgerRecord
}

// HostLedgerRaise is one verified position of a store that Raise may raise
// the ledger to.
type HostLedgerRaise struct {
	// Session is the attestation session the position was signed in;
	// positions of any session other than the ledger's current one are
	// dropped.
	Session [16]byte
	// Position is the verified chain position (a receipt's accepted
	// position, or an answer's chain position).
	Position HostLedgerPosition
	// Checkpoint is the verified checkpoint beside it; Seq 0 when none.
	Checkpoint HostLedgerPosition
	// Heartbeat reports a position from a heartbeat answer rather than a
	// receipt: a batch of heartbeat positions only raises the ledger at
	// most once per 30 seconds per host.
	Heartbeat bool
}

// hostLedgerGuard is what this process last wrote or verified of one
// ledger object.
type hostLedgerGuard struct {
	counter     uint64
	seq         uint64
	attestation [16]byte
	pending     [][16]byte
}

// hostLedgerMirror is what the status mirror shows of a host's ledger.
type hostLedgerMirror struct {
	storePrefix string
	seq         uint64
	checkpoint  uint64
	state       infravirtrigaudiov1beta1.HostAgentLedgerAttestationState
	attestation [16]byte
	attestedAt  time.Time
	generation  uint64
}

// HostLedger is the manager's host ledger library. Set Client, APIReader,
// Trust, Keys and Config; the rest is optional. It is safe for concurrent
// use; writes of one store are serialised.
type HostLedger struct {
	// Client writes the ledger Secrets.
	Client client.Client
	// APIReader reads the ledger Secrets and Hosts uncached.
	APIReader client.Reader
	// Trust is the source of the hosts' enrolments and store states (the
	// pins Secrets).
	Trust HostTrustSource
	// Keys are the host ledger keys.
	Keys *HostLedgerKeys
	// Config is the enrolment configuration (namespace, install id).
	Config *HostEnrolmentConfig
	// Recorder emits events on Hosts (nil: none).
	Recorder record.EventRecorder
	// Now is the clock (nil: time.Now).
	Now func() time.Time
	// ManagerVersion is this manager's version for the downgrade floor
	// (nil: the build version).
	ManagerVersion func() string
	// MinManagerVersion is the floor written into objects (empty: the
	// format's).
	MinManagerVersion string

	// lastAnswer returns when a verified answer of a host (Provider UID,
	// host id) last arrived (zero: none known). Set by AttachLedger.
	lastAnswer func(providerUID, hostID string) time.Time
	// markHeld persists a store-regression hold of a host. Set by
	// AttachLedger; nil: written through Trust.
	markHeld func(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID, reason string, enrolment []byte)
	// updateStoreState writes a host's store state. Set by AttachLedger;
	// nil: Trust with conflict retries.
	updateStoreState func(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string,
		update func(*HostStoreState)) error

	mu             sync.Mutex
	locks          map[[16]byte]*sync.Mutex
	guards         map[[16]byte]*hostLedgerGuard
	holds          map[string]string
	mirrors        map[string]hostLedgerMirror
	goneSince      map[string]time.Time
	heartbeatAt    map[string]time.Time
	indexPersisted map[string]time.Time
}

// NewHostLedger returns the host ledger library of a manager with host
// ledger keys (nil keys or no enrolment: nil, no ledger).
func NewHostLedger(c client.Client, reader client.Reader, trust HostTrustSource, keys *HostLedgerKeys, cfg *HostEnrolmentConfig,
	rec record.EventRecorder) *HostLedger {
	if keys == nil || cfg == nil {
		return nil
	}
	return &HostLedger{Client: c, APIReader: reader, Trust: trust, Keys: keys, Config: cfg, Recorder: rec}
}

// AttachLedger makes l the host ledger of h: l reads h's last verified
// answer times (a rebind waits for the earlier Host to be quiet), writes
// store states through h's per-host writer and persists holds through h (so
// h holds the host at once), and the heartbeat mirrors l into the Hosts'
// status. A nil l detaches.
func (h *HostAnswers) AttachLedger(l *HostLedger) {
	h.Ledger = l
	if l == nil {
		return
	}
	l.lastAnswer = h.lastVerifiedAnswer
	l.markHeld = h.markRegressed
	l.updateStoreState = func(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string,
		update func(*HostStoreState)) error {
		return h.updateStore(ctx, provider, hostID, nil, update)
	}
}

// lastVerifiedAnswer returns when the last verified host answer of a host
// arrived (zero: none in this process).
func (h *HostAnswers) lastVerifiedAnswer(providerUID, hostID string) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.hosts[providerUID+"/"+hostID]; s != nil {
		return s.hostAnswerAt
	}
	return time.Time{}
}

// now returns the clock.
func (l *HostLedger) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// version returns this manager's version.
func (l *HostLedger) version() string {
	if l.ManagerVersion != nil {
		return l.ManagerVersion()
	}
	return buildVersion()
}

// floor returns the floor written into objects.
func (l *HostLedger) floor() string {
	if l.MinManagerVersion != "" {
		return l.MinManagerVersion
	}
	return hostLedgerMinManagerVersion
}

// storeLock returns the write lock of a store.
func (l *HostLedger) storeLock(store [16]byte) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.locks == nil {
		l.locks = map[[16]byte]*sync.Mutex{}
	}
	m := l.locks[store]
	if m == nil {
		m = &sync.Mutex{}
		l.locks[store] = m
	}
	return m
}

// ledgerHost is one Host a ledger call is for, resolved from live reads.
type ledgerHost struct {
	provider    *infravirtrigaudiov1beta1.Provider
	hostID      string
	host        *infravirtrigaudiov1beta1.Host
	key         string
	enrolment   []byte
	identityMAC [32]byte
}

// resolve reads the Host of a Provider's host id uncached and its current
// enrolment. The Host must be the Provider's, not revoked or deleting, and
// carry an identity.
func (l *HostLedger) resolve(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string) (*ledgerHost, error) {
	h := &infravirtrigaudiov1beta1.Host{}
	if err := l.APIReader.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: hostID}, h); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("Host %s/%s: %w", provider.Namespace, hostID, errHostLedgerHost)
		}
		return nil, fmt.Errorf("get Host %s/%s: %w", provider.Namespace, hostID, err)
	}
	if h.Spec.ProviderRef.Name != provider.Name || h.Spec.Revoked || !h.DeletionTimestamp.IsZero() || h.Spec.Identity == "" {
		return nil, fmt.Errorf("Host %s/%s is revoked, deleting, without an identity or not the Provider's: %w",
			provider.Namespace, hostID, errHostLedgerHost)
	}
	t, err := l.Trust.HostTrust(ctx, provider, hostID)
	if err != nil {
		return nil, fmt.Errorf("enrolment of Host %s/%s: %w", provider.Namespace, hostID, err)
	}
	return &ledgerHost{provider: provider, hostID: hostID, host: h, key: hostAnswerKey(provider, hostID),
		enrolment: bytes.Clone(t.Enrolment.EnrolmentID), identityMAC: l.Keys.IdentityMAC(l.Config.InstallID, h.Spec.Identity)}, nil
}

// hold holds the host lh for reason: in memory, by an Event when the reason
// changed, and persisted when persistent. It returns the hold error.
func (l *HostLedger) hold(ctx context.Context, lh *ledgerHost, reason, storePrefix string, persistent bool, detail string) error {
	l.mu.Lock()
	if l.holds == nil {
		l.holds = map[string]string{}
	}
	changed := l.holds[lh.key] != reason
	l.holds[lh.key] = reason
	l.mu.Unlock()
	log.FromContext(ctx).Info("The host's ledger holds the host; nothing is signed from it", "host", lh.hostID,
		"reason", reason, "store", storePrefix, "detail", detail)
	if changed && l.Recorder != nil {
		l.Recorder.Event(lh.host, corev1.EventTypeWarning, eventReasonLedgerHeld,
			fmt.Sprintf("the host is held by its ledger (%s) for store %s: %s", reason, storePrefix, detail))
	}
	if persistent {
		l.persistHold(ctx, lh, reason)
	}
	return &HostLedgerHoldError{Reason: reason, StorePrefix: storePrefix, Persistent: persistent, Detail: detail}
}

// persistHold persists a store-regression hold of lh under its current
// enrolment.
func (l *HostLedger) persistHold(ctx context.Context, lh *ledgerHost, reason string) {
	if l.markHeld != nil {
		l.markHeld(ctx, lh.provider, lh.hostID, reason, lh.enrolment)
		return
	}
	if err := l.writeStoreState(ctx, lh, func(st *HostStoreState) {
		st.RegressedReason, st.RegressedEnrolmentID = reason, bytes.Clone(lh.enrolment)
	}); err != nil {
		log.FromContext(ctx).Info("Cannot persist the host's ledger hold; it is detected again on the next read", "host", lh.hostID,
			"error", err.Error())
	}
}

// clearHold forgets the in-memory hold of lh (a later call succeeded).
func (l *HostLedger) clearHold(lh *ledgerHost) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.holds, lh.key)
}

// HeldReason returns the reason the host's ledger last held the host in
// this process, or "" (persisted holds also show as store-regression
// holds).
func (l *HostLedger) HeldReason(provider *infravirtrigaudiov1beta1.Provider, hostID string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holds[hostAnswerKey(provider, hostID)]
}

// writeStoreState applies update to lh's store state, as one write that
// never overwrites a concurrent one.
func (l *HostLedger) writeStoreState(ctx context.Context, lh *ledgerHost, update func(*HostStoreState)) error {
	if l.updateStoreState != nil {
		return l.updateStoreState(ctx, lh.provider, lh.hostID, update)
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return l.Trust.UpdateStoreState(ctx, lh.provider, lh.hostID, update)
	})
}

// storeState reads lh's store state and checks it: a store-regression hold
// under the current enrolment, or an index that fails its tag or form,
// holds the host.
func (l *HostLedger) storeState(ctx context.Context, lh *ledgerHost, store [16]byte) (HostStoreState, error) {
	st, err := l.Trust.LoadStoreState(ctx, lh.provider, lh.hostID)
	if err != nil {
		return st, fmt.Errorf("store state of Host %s: %w", lh.hostID, err)
	}
	if st.RegressedReason != "" && bytes.Equal(st.RegressedEnrolmentID, lh.enrolment) {
		return st, &HostLedgerHoldError{Reason: st.RegressedReason, StorePrefix: storeIDPrefix(store), Persistent: true,
			Detail: "the host is held for a store regression under its current enrolment"}
	}
	switch {
	case len(st.ledgerIndexRaw) > 0:
		return st, l.hold(ctx, lh, LedgerReasonTampered, storeIDPrefix(store), false, "the ledger index in the pins Secret is malformed")
	case st.LedgerIndex != nil && !st.LedgerIndex.verifyTag(l.Keys, string(lh.host.UID)):
		return st, l.hold(ctx, lh, LedgerReasonTampered, storeIDPrefix(store), false, "the ledger index in the pins Secret fails its tag")
	}
	return st, nil
}

// readRecord reads and verifies the ledger object of store: its labels,
// floor, form, store, tag, the index's write counter (for the index's
// newest store) and this process's guard. idx is lh's verified index (nil:
// none).
func (l *HostLedger) readRecord(ctx context.Context, lh *ledgerHost, store [16]byte, idx *HostLedgerIndex) (*HostLedgerRecord,
	*corev1.Secret, error) {
	sec := &corev1.Secret{}
	name := hostLedgerSecretName(store)
	if err := l.APIReader.Get(ctx, types.NamespacedName{Namespace: l.Config.Namespace, Name: name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("store %s: %w", storeIDPrefix(store), ErrHostLedgerNotFound)
		}
		return nil, nil, fmt.Errorf("get host ledger of store %s: %w", storeIDPrefix(store), err)
	}
	rec, err := l.verifySecret(ctx, sec, store)
	if err != nil {
		var tamper *ledgerTamper
		if errors.As(err, &tamper) {
			return nil, nil, l.hold(ctx, lh, LedgerReasonTampered, storeIDPrefix(store), false, tamper.detail)
		}
		return nil, nil, err
	}
	if n := len(idx.storeIDsOrNil()); n > 0 && idx.StoreIDs[n-1] == store && rec.WriteCounter < idx.CounterSeen {
		return nil, nil, l.hold(ctx, lh, LedgerReasonRegressed, rec.StorePrefix(), true,
			"the ledger's write counter is below the highest the manager recorded: an older copy was restored")
	}
	if reason, detail := l.checkGuard(rec); reason != "" {
		return nil, nil, l.hold(ctx, lh, reason, rec.StorePrefix(), true, detail)
	}
	return rec, sec, nil
}

// storeIDsOrNil returns the index's stores (nil for a nil index).
func (x *HostLedgerIndex) storeIDsOrNil() [][16]byte {
	if x == nil {
		return nil
	}
	return x.StoreIDs
}

// ledgerTamper is verifySecret's refusal of an object that is not a valid
// ledger object.
type ledgerTamper struct{ detail string }

// Error implements error.
func (t *ledgerTamper) Error() string { return t.detail }

// verifySecret verifies a ledger Secret of store (store zero: any store, as
// named by the Secret): labels, floor, form, name, tag. A failure is a
// *ledgerTamper, except a floor above this manager (errManagerTooOld).
func (l *HostLedger) verifySecret(_ context.Context, sec *corev1.Secret, store [16]byte) (*HostLedgerRecord, error) {
	if !isHostLedgerSecret(sec) {
		return nil, &ledgerTamper{"a Secret with the ledger's name is not a host ledger Secret"}
	}
	data := sec.Data[hostLedgerDataKey]
	if err := checkManagerFloorFor("host ledger Secret "+sec.Name, data, l.version()); err != nil {
		return nil, err
	}
	rec, err := decodeHostLedger(data)
	if err != nil {
		return nil, &ledgerTamper{"the ledger is malformed: " + err.Error()}
	}
	if sec.Name != hostLedgerSecretName(rec.StoreID) || (store != [16]byte{} && rec.StoreID != store) {
		return nil, &ledgerTamper{"the ledger names another store than its Secret"}
	}
	if !rec.verifyTag(l.Keys) {
		return nil, &ledgerTamper{"the ledger fails its tag (or names an unknown tag key)"}
	}
	rec.resourceVersion = sec.ResourceVersion
	return rec, nil
}

// checkGuard checks a verified record against what this process last wrote
// or verified of it: a lower position or write counter is a regression; a
// change of the sessions this process did not write, or a rewrite under
// the same write counter, is tampering. It returns the hold reason and
// detail, or "".
func (l *HostLedger) checkGuard(rec *HostLedgerRecord) (reason, detail string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	g := l.guards[rec.StoreID]
	if g == nil {
		return "", ""
	}
	pending := pendingIDs(rec)
	sessionsChanged := rec.AttestationID != g.attestation || !slices.Equal(pending, g.pending)
	switch {
	case rec.Position.Seq < g.seq:
		return LedgerReasonRegressed, "the ledger position is below one this manager wrote or verified"
	case rec.WriteCounter < g.counter:
		return LedgerReasonRegressed, "the ledger's write counter is below one this manager wrote or verified"
	case rec.WriteCounter == g.counter && (sessionsChanged || rec.Position.Seq != g.seq):
		return LedgerReasonTampered, "the ledger changed without its write counter"
	case rec.WriteCounter > g.counter && sessionsChanged:
		return LedgerReasonTampered, "the ledger's attestation sessions changed in a write this manager did not make"
	}
	return "", ""
}

// pendingIDs returns the ids of a record's pending sessions.
func pendingIDs(rec *HostLedgerRecord) [][16]byte {
	out := make([][16]byte, 0, len(rec.PendingSessions))
	for _, p := range rec.PendingSessions {
		out = append(out, p.ID)
	}
	return out
}

// remember records a record this process wrote or verified: its guard and
// its mirror (for the Host lh, when not nil).
func (l *HostLedger) remember(lh *ledgerHost, rec *HostLedgerRecord, adopted bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.guards == nil {
		l.guards = map[[16]byte]*hostLedgerGuard{}
	}
	g := l.guards[rec.StoreID]
	if g == nil || rec.WriteCounter >= g.counter {
		l.guards[rec.StoreID] = &hostLedgerGuard{counter: rec.WriteCounter, seq: rec.Position.Seq, attestation: rec.AttestationID,
			pending: pendingIDs(rec)}
	}
	if lh == nil || rec.HostUID != string(lh.host.UID) || rec.Archived {
		return
	}
	if l.mirrors == nil {
		l.mirrors = map[string]hostLedgerMirror{}
	}
	m := hostLedgerMirror{storePrefix: rec.StorePrefix(), seq: rec.Position.Seq, checkpoint: rec.Checkpoint.Seq,
		state: rec.AttestationState, attestation: rec.AttestationID, generation: rec.Generation}
	if prev, ok := l.mirrors[lh.key]; ok && prev.attestation == rec.AttestationID {
		m.attestedAt = prev.attestedAt
	}
	if adopted {
		m.attestedAt = l.now()
	}
	l.mirrors[lh.key] = m
}

// mirrorOf returns the mirror of a host's ledger, if this process knows it.
func (l *HostLedger) mirrorOf(key string) (hostLedgerMirror, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.mirrors[key]
	return m, ok
}

// setLabels sets the labels and Host annotation of a ledger Secret for the
// record it holds. enrolment is the enrolment label (nil: kept).
func setLedgerLabels(sec *corev1.Secret, rec *HostLedgerRecord, enrolment []byte) {
	labels := sec.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[k8sLabelComponent] = hostLedgerComponent
	labels[k8sLabelManagedBy] = k8sManagedByVirtRigaud
	labels[LabelHostLedger] = hostLedgerFormat
	labels[LabelHostLedgerIdentityMAC] = hex.EncodeToString(rec.HostIdentityMAC[:])[:hostLedgerIdentityLabelLen]
	if enrolment != nil {
		labels[LabelHostLedgerEnrolment] = hex.EncodeToString(enrolment)
	}
	sec.SetLabels(labels)
	ann := sec.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[certrequest.AnnotationHost] = rec.HostKey
	sec.SetAnnotations(ann)
}

// prepare stamps a record for writing: write counter, time, floor and tag.
func (l *HostLedger) prepare(rec *HostLedgerRecord) ([]byte, error) {
	rec.WriteCounter++
	rec.UpdatedAt = l.now().UTC().Truncate(time.Second)
	rec.MinManagerVersion = l.floor()
	rec.sign(l.Keys)
	return rec.encode()
}

// written verifies the object the API server returned for this process's
// own write and remembers it.
func (l *HostLedger) written(ctx context.Context, lh *ledgerHost, sec *corev1.Secret, store [16]byte, adopted bool) (*HostLedgerRecord, error) {
	rec, err := l.verifySecret(ctx, sec, store)
	if err != nil {
		return nil, fmt.Errorf("verify the host ledger of store %s as written: %w", storeIDPrefix(store), err)
	}
	l.remember(lh, rec, adopted)
	return rec, nil
}

// update is the read-modify-write of one store's ledger for the Host lh:
// a verified uncached read, mutate on a copy (it reports whether it
// changed anything, or an error that stops the write), and an update that
// carries the read resourceVersion; a conflict starts again from a fresh
// read. enrolment is the enrolment label to set (nil: kept). It returns the
// record as written (or as read when nothing changed).
func (l *HostLedger) update(ctx context.Context, lh *ledgerHost, store [16]byte, enrolment []byte, adopt bool,
	mutate func(*HostLedgerRecord) (bool, error)) (*HostLedgerRecord, error) {
	m := l.storeLock(store)
	m.Lock()
	defer m.Unlock()
	for range maxLedgerWriteAttempts {
		st, err := l.storeState(ctx, lh, store)
		if err != nil {
			return nil, err
		}
		rec, sec, err := l.readRecord(ctx, lh, store, st.LedgerIndex)
		if err != nil {
			return nil, err
		}
		next := rec.clone()
		changed, err := mutate(next)
		if err != nil {
			return nil, err
		}
		if !changed {
			l.remember(lh, rec, false)
			return rec, nil
		}
		data, err := l.prepare(next)
		if err != nil {
			return nil, err
		}
		out := sec.DeepCopy()
		out.Data = map[string][]byte{}
		for k, v := range sec.Data {
			out.Data[k] = v
		}
		out.Data[hostLedgerDataKey] = data
		setLedgerLabels(out, next, enrolment)
		if err := l.Client.Update(ctx, out); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return nil, fmt.Errorf("write the host ledger of store %s: %w", storeIDPrefix(store), err)
		}
		got, err := l.written(ctx, lh, out, store, adopt)
		if err != nil {
			return nil, err
		}
		l.maybePersistIndex(ctx, lh, got, st.LedgerIndex)
		return got, nil
	}
	return nil, fmt.Errorf("store %s: %w", storeIDPrefix(store), errHostLedgerConflict)
}

// requireBound refuses a write to a ledger that does not serve the Host lh
// or is archived.
func requireBound(lh *ledgerHost, rec *HostLedgerRecord) error {
	switch {
	case rec.Archived:
		return fmt.Errorf("store %s: %w", rec.StorePrefix(), ErrHostLedgerArchived)
	case rec.HostUID != string(lh.host.UID) || rec.HostIdentityMAC != lh.identityMAC:
		return fmt.Errorf("store %s: %w", rec.StorePrefix(), ErrHostLedgerNotBound)
	}
	return nil
}

// Load returns the verified ledger of store for the host hostID of
// provider, read uncached (ErrHostLedgerNotFound when it has none). The
// record may serve another Host object (compare HostUID; Rebind moves it).
// A check that fails holds the host (*HostLedgerHoldError).
func (l *HostLedger) Load(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, store [16]byte) (*HostLedgerRecord, error) {
	lh, err := l.resolve(ctx, provider, hostID)
	if err != nil {
		return nil, err
	}
	m := l.storeLock(store)
	m.Lock()
	defer m.Unlock()
	st, err := l.storeState(ctx, lh, store)
	if err != nil {
		return nil, err
	}
	rec, _, err := l.readRecord(ctx, lh, store, st.LedgerIndex)
	if err != nil {
		return nil, err
	}
	l.remember(lh, rec, false)
	l.clearHold(lh)
	return rec, nil
}

// nextGeneration returns the generation of a ledger started afresh after a
// predecessor of generation pred (0: none): one above it, and never below
// the current Unix time in seconds, so a generation is never handed out
// twice for a store even when an earlier ledger object of it was deleted.
func (l *HostLedger) nextGeneration(pred uint64) uint64 {
	now := l.now().Unix()
	if now < 1 {
		now = 1
	}
	return max(pred+1, uint64(now))
}

// headOf returns the head a store record holds at seq.
func headOf(r hostidentity.StoreRecord, seq uint64) ([32]byte, bool) {
	switch {
	case seq == 0:
		return [32]byte{}, false
	case seq == r.Seq:
		return r.Head, true
	case seq == r.CheckpointSeq:
		return r.CheckpointHead, true
	}
	return [32]byte{}, false
}

// validPosition checks a position and its checkpoint.
func validPosition(pos, cp HostLedgerPosition) error {
	switch {
	case pos.Seq < 1:
		return fmt.Errorf("%w: a position needs a sequence number of at least 1", errHostLedgerInput)
	case cp.Seq%hostidentity.CheckpointInterval != 0 || cp.Seq > pos.Seq:
		return fmt.Errorf("%w: a checkpoint must be a multiple of %d not above its position", errHostLedgerInput,
			hostidentity.CheckpointInterval)
	}
	return nil
}

// Initialise returns the ledger of store in.StoreID for the host hostID of
// provider, starting it when the store has none:
//
//   - an existing ledger object is returned (Source Existing); one that
//     serves another Host object is ErrHostLedgerNotBound (Rebind) and an
//     archived one ErrHostLedgerArchived;
//   - another store with a ledger under the host's current enrolment holds
//     the host (ledger_store_mismatch, persisted): one enrolment has one
//     store;
//   - a store the host's ledger index names, but whose ledger object is
//     gone, holds the host (ledger_missing): a lost ledger is never
//     restarted;
//   - when the host's last verified position (the pins Secret's store
//     state) is of this store, the ledger starts there, but only once the
//     host has confirmed it: in.Continuity must be the verified answer to a
//     request whose continuity_seq named that position (or its checkpoint,
//     after an honest pruning) carrying the same head; otherwise the host
//     is held (ledger_unconfirmed);
//   - otherwise only a first contact may start it: in.Origin must show the
//     store was created under the current enrolment and held no other. The
//     ledgers of the Host's earlier stores are then archived (Archived) and
//     the new ledger's generation is above theirs. Any other case holds the
//     host (ledger_missing).
//
// The store is then added to the host's ledger index.
func (l *HostLedger) Initialise(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string,
	in HostLedgerInit) (HostLedgerInitResult, error) {
	if in.StoreID == ([16]byte{}) {
		return HostLedgerInitResult{}, fmt.Errorf("%w: no store id", errHostLedgerInput)
	}
	if err := validPosition(in.Position, in.Checkpoint); err != nil {
		return HostLedgerInitResult{}, err
	}
	lh, err := l.resolve(ctx, provider, hostID)
	if err != nil {
		return HostLedgerInitResult{}, err
	}
	m := l.storeLock(in.StoreID)
	m.Lock()
	defer m.Unlock()
	for range maxLedgerWriteAttempts {
		res, retry, err := l.initialiseOnce(ctx, lh, in)
		if retry {
			continue
		}
		if err == nil {
			l.clearHold(lh)
		}
		return res, err
	}
	return HostLedgerInitResult{}, fmt.Errorf("store %s: %w", storeIDPrefix(in.StoreID), errHostLedgerConflict)
}

// initialiseOnce is one attempt of Initialise; retry reports that a
// concurrent writer created the object first.
func (l *HostLedger) initialiseOnce(ctx context.Context, lh *ledgerHost, in HostLedgerInit) (HostLedgerInitResult, bool, error) {
	prefix := storeIDPrefix(in.StoreID)
	st, err := l.storeState(ctx, lh, in.StoreID)
	if err != nil {
		return HostLedgerInitResult{}, false, err
	}
	rec, _, err := l.readRecord(ctx, lh, in.StoreID, st.LedgerIndex)
	switch {
	case err == nil:
		l.remember(lh, rec, false)
		if err := requireBound(lh, rec); err != nil {
			return HostLedgerInitResult{Record: rec, Source: LedgerInitExisting}, false, err
		}
		return HostLedgerInitResult{Record: rec, Source: LedgerInitExisting}, false, nil
	case !errors.Is(err, ErrHostLedgerNotFound):
		return HostLedgerInitResult{}, false, err
	}
	others, err := l.relatedLedgers(ctx, lh, in.StoreID)
	if err != nil {
		return HostLedgerInitResult{}, false, err
	}
	enrolmentLabel := hex.EncodeToString(lh.enrolment)
	for _, o := range others {
		if !o.rec.Archived && (o.enrolmentLabel == enrolmentLabel || o.rec.HasEnrolment(lh.enrolment)) {
			return HostLedgerInitResult{}, false, l.hold(ctx, lh, LedgerReasonStoreMismatch, prefix, true,
				"another store already has a ledger under the host's current enrolment; re-enrol the host with a fresh store")
		}
	}
	if st.LedgerIndex.Names(in.StoreID) {
		return HostLedgerInitResult{}, false, l.hold(ctx, lh, LedgerReasonMissing, prefix, false,
			"the store had a ledger and its ledger object is gone; a lost ledger is never restarted")
	}
	now := l.now().UTC().Truncate(time.Second)
	rec = &HostLedgerRecord{StoreID: in.StoreID, HostIdentityMAC: lh.identityMAC, HostUID: string(lh.host.UID),
		HostKey: hostKey(lh.host.Namespace, lh.host.Name), ProviderUID: string(lh.provider.UID), GenerationStartedAt: now,
		GenerationEnrolments: [][]byte{bytes.Clone(lh.enrolment)}}
	if st.LedgerIndex != nil {
		rec.WriteCounter = st.LedgerIndex.CounterSeen
	}
	res := HostLedgerInitResult{}
	if last := st.Current; !last.Empty() && last.StoreID == in.StoreID {
		want := last.ContinuitySeq()
		head, ok := headOf(last, want)
		c := in.Continuity
		if !ok || c == nil || c.Seq != want || c.Head != head {
			return HostLedgerInitResult{}, false, l.hold(ctx, lh, LedgerReasonUnconfirmed, prefix, false,
				"the host has not confirmed, in a signed answer, the last position the ledger would start from")
		}
		rec.Position = HostLedgerPosition{Seq: last.Seq, Head: last.Head}
		rec.Checkpoint = HostLedgerPosition{Seq: last.CheckpointSeq, Head: last.CheckpointHead}
		rec.Generation = l.nextGeneration(0)
		res.Source = LedgerInitLastPosition
	} else {
		o := in.Origin
		if o == nil || o.EnrolmentCount != 1 || !bytes.Equal(o.OriginEnrolmentID, lh.enrolment) {
			return HostLedgerInitResult{}, false, l.hold(ctx, lh, LedgerReasonMissing, prefix, false,
				"the store has no ledger and its agent does not show it was created under the current enrolment alone")
		}
		var pred uint64
		for _, p := range others {
			if p.rec.Archived || p.rec.HostIdentityMAC != lh.identityMAC {
				continue
			}
			if p.rec.HostUID != string(lh.host.UID) {
				if reason, detail := l.boundHostReleased(ctx, p.rec); reason != "" {
					return HostLedgerInitResult{}, false, l.hold(ctx, lh, reason, p.rec.StorePrefix(), false, detail)
				}
			}
			archived, err := l.archive(ctx, lh, p.rec.StoreID, in.StoreID)
			if err != nil {
				return HostLedgerInitResult{}, false, err
			}
			res.Archived = append(res.Archived, archived)
			pred = max(pred, archived.Generation)
		}
		rec.Position, rec.Checkpoint = in.Position, in.Checkpoint
		rec.Generation = l.nextGeneration(pred)
		res.Source = LedgerInitFirstContact
	}
	data, err := l.prepare(rec)
	if err != nil {
		return HostLedgerInitResult{}, false, err
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: hostLedgerSecretName(in.StoreID), Namespace: l.Config.Namespace},
		Type: corev1.SecretTypeOpaque, Data: map[string][]byte{hostLedgerDataKey: data}}
	setLedgerLabels(sec, rec, lh.enrolment)
	if err := l.Client.Create(ctx, sec); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return HostLedgerInitResult{}, true, nil
		}
		return HostLedgerInitResult{}, false, fmt.Errorf("create the host ledger of store %s: %w", prefix, err)
	}
	got, err := l.written(ctx, lh, sec, in.StoreID, false)
	if err != nil {
		return HostLedgerInitResult{}, false, err
	}
	res.Record = got
	if l.Recorder != nil {
		l.Recorder.Event(lh.host, corev1.EventTypeNormal, eventReasonLedgerInitialised,
			fmt.Sprintf("the ledger of store %s was started (%s) at generation %d", prefix, res.Source, got.Generation))
	}
	if err := l.persistIndex(ctx, lh, &in.StoreID, got.WriteCounter); err != nil {
		return res, false, fmt.Errorf("record store %s in the host's ledger index: %w", prefix, err)
	}
	return res, false, nil
}

// relatedLedger is a verified ledger object found by its labels.
type relatedLedger struct {
	rec            *HostLedgerRecord
	enrolmentLabel string
}

// relatedLedgers returns the verified ledger objects of stores other than
// store labelled with lh's current enrolment or identity MAC. Labels only
// find them; each is decided on its verified body. One that fails its
// checks holds the host.
func (l *HostLedger) relatedLedgers(ctx context.Context, lh *ledgerHost, store [16]byte) ([]relatedLedger, error) {
	seen := map[string]bool{}
	var out []relatedLedger
	for _, sel := range []client.MatchingLabels{
		{k8sLabelComponent: hostLedgerComponent, LabelHostLedgerEnrolment: hex.EncodeToString(lh.enrolment)},
		{k8sLabelComponent: hostLedgerComponent, LabelHostLedgerIdentityMAC: hex.EncodeToString(lh.identityMAC[:])[:hostLedgerIdentityLabelLen]},
	} {
		var list corev1.SecretList
		if err := l.APIReader.List(ctx, &list, client.InNamespace(l.Config.Namespace), sel); err != nil {
			return nil, fmt.Errorf("list host ledgers: %w", err)
		}
		for i := range list.Items {
			sec := &list.Items[i]
			if seen[sec.Name] || sec.Name == hostLedgerSecretName(store) {
				continue
			}
			seen[sec.Name] = true
			rec, err := l.verifySecret(ctx, sec, [16]byte{})
			if err != nil {
				var tamper *ledgerTamper
				if errors.As(err, &tamper) {
					return nil, l.hold(ctx, lh, LedgerReasonTampered, storeIDPrefix(store), false,
						"a related ledger object fails its checks: "+tamper.detail)
				}
				return nil, err
			}
			if reason, detail := l.checkGuard(rec); reason != "" {
				return nil, l.hold(ctx, lh, reason, rec.StorePrefix(), true, detail)
			}
			out = append(out, relatedLedger{rec: rec, enrolmentLabel: sec.Labels[LabelHostLedgerEnrolment]})
		}
	}
	return out, nil
}

// archive marks the ledger of store as replaced by successor. Its labels
// are kept.
func (l *HostLedger) archive(ctx context.Context, lh *ledgerHost, store, successor [16]byte) (*HostLedgerRecord, error) {
	m := l.storeLock(store)
	m.Lock()
	defer m.Unlock()
	for range maxLedgerWriteAttempts {
		sec := &corev1.Secret{}
		if err := l.APIReader.Get(ctx, types.NamespacedName{Namespace: l.Config.Namespace, Name: hostLedgerSecretName(store)}, sec); err != nil {
			return nil, fmt.Errorf("get host ledger of store %s: %w", storeIDPrefix(store), err)
		}
		rec, err := l.verifySecret(ctx, sec, store)
		if err != nil {
			var tamper *ledgerTamper
			if errors.As(err, &tamper) {
				return nil, l.hold(ctx, lh, LedgerReasonTampered, storeIDPrefix(store), false, tamper.detail)
			}
			return nil, err
		}
		if reason, detail := l.checkGuard(rec); reason != "" {
			return nil, l.hold(ctx, lh, reason, rec.StorePrefix(), true, detail)
		}
		if rec.Archived {
			return rec, nil
		}
		next := rec.clone()
		next.Archived, next.SuccessorStoreID, next.ArchivedAt = true, successor, l.now().UTC().Truncate(time.Second)
		data, err := l.prepare(next)
		if err != nil {
			return nil, err
		}
		out := sec.DeepCopy()
		out.Data[hostLedgerDataKey] = data
		setLedgerLabels(out, next, nil)
		if err := l.Client.Update(ctx, out); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return nil, fmt.Errorf("archive the host ledger of store %s: %w", storeIDPrefix(store), err)
		}
		got, err := l.written(ctx, lh, out, store, false)
		if err != nil {
			return nil, err
		}
		if l.Recorder != nil {
			l.Recorder.Event(lh.host, corev1.EventTypeNormal, eventReasonLedgerArchived,
				fmt.Sprintf("the ledger of store %s was archived: the host has a new store %s", got.StorePrefix(), storeIDPrefix(successor)))
		}
		return got, nil
	}
	return nil, fmt.Errorf("store %s: %w", storeIDPrefix(store), errHostLedgerConflict)
}

// boundHostReleased reports whether the Host object a ledger serves has
// released it: deleted (gone, replaced by another object of the same name,
// or deleting) or revoked, with no verified answer younger than
// hostAnswerTTL, and seen released by this process for at least
// hostAnswerTTL (so a restart waits again). It returns the hold reason and
// detail when not.
func (l *HostLedger) boundHostReleased(ctx context.Context, rec *HostLedgerRecord) (reason, detail string) {
	ns, name, ok := strings.Cut(rec.HostKey, "/")
	if !ok {
		return LedgerReasonStoreBound, "the ledger's Host reference is malformed"
	}
	h := &infravirtrigaudiov1beta1.Host{}
	err := l.APIReader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, h)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return LedgerReasonStoreBound, "the Host the ledger serves cannot be read"
	case string(h.UID) == rec.HostUID && h.DeletionTimestamp.IsZero() && !h.Spec.Revoked:
		l.mu.Lock()
		delete(l.goneSince, rec.HostUID)
		l.mu.Unlock()
		return LedgerReasonStoreBound, "the Host the ledger serves is live and not revoked"
	}
	now := l.now()
	if l.lastAnswer != nil {
		if at := l.lastAnswer(rec.ProviderUID, name); !at.IsZero() && now.Sub(at) <= hostAnswerTTL {
			return LedgerReasonStoreBound, "the Host the ledger serves answered recently"
		}
	}
	l.mu.Lock()
	if l.goneSince == nil {
		l.goneSince = map[string]time.Time{}
	}
	since, seen := l.goneSince[rec.HostUID]
	if !seen {
		since = now
		l.goneSince[rec.HostUID] = now
	}
	l.mu.Unlock()
	if now.Sub(since) < hostAnswerTTL {
		return LedgerReasonStoreBound, "the Host the ledger serves was released less than the answer lifetime ago"
	}
	return "", ""
}

// Rebind moves the ledger of store to the host hostID of provider, a new
// Host object of the same machine, keeping its position, generation and
// sessions (never a fresh start). It is allowed only when the ledger is
// bound to the same identity MAC (else the host is held,
// ledger_store_foreign), the Host object it serves is deleted or revoked,
// has no verified answer younger than hostAnswerTTL and was seen released
// by this process at least that long, and no Host other than this one that
// is not revoked carries the identity (else ledger_store_bound). The store
// is then added to the new Host's ledger index. A ledger already bound to
// the Host is returned unchanged.
func (l *HostLedger) Rebind(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, store [16]byte) (*HostLedgerRecord, error) {
	lh, err := l.resolve(ctx, provider, hostID)
	if err != nil {
		return nil, err
	}
	rec, err := l.update(ctx, lh, store, lh.enrolment, false, func(r *HostLedgerRecord) (bool, error) {
		switch {
		case r.Archived:
			return false, fmt.Errorf("store %s: %w", r.StorePrefix(), ErrHostLedgerArchived)
		case r.HostIdentityMAC != lh.identityMAC:
			return false, l.hold(ctx, lh, LedgerReasonStoreForeign, r.StorePrefix(), false, "the ledger is bound to another machine identity")
		case r.HostUID == string(lh.host.UID):
			return false, nil
		}
		if reason, detail := l.boundHostReleased(ctx, r); reason != "" {
			return false, l.hold(ctx, lh, reason, r.StorePrefix(), false, detail)
		}
		others, err := hostsWithIdentity(ctx, l.APIReader, lh.host)
		if err != nil {
			return false, err
		}
		if others > 0 {
			return false, l.hold(ctx, lh, LedgerReasonStoreBound, r.StorePrefix(), false,
				"another Host that is not revoked carries the same identity")
		}
		r.HostUID, r.HostKey, r.ProviderUID = string(lh.host.UID), hostKey(lh.host.Namespace, lh.host.Name), string(lh.provider.UID)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if err := l.persistIndex(ctx, lh, &store, rec.WriteCounter); err != nil {
		return rec, fmt.Errorf("record store %s in the host's ledger index: %w", rec.StorePrefix(), err)
	}
	if l.Recorder != nil {
		l.Recorder.Event(lh.host, corev1.EventTypeNormal, eventReasonLedgerRebound,
			fmt.Sprintf("the ledger of store %s now serves this Host (generation %d kept)", rec.StorePrefix(), rec.Generation))
	}
	l.clearHold(lh)
	return rec, nil
}

// BeginSession adds attestation session session (the challenge nonce) to
// the pending sessions of the ledger of store, evicting the oldest beyond
// four. tip is the store's chain tip in the verified answer the challenge
// came with: a ledger above it holds the host (ledger_ahead) and nothing is
// added. For LedgerSessionAttest the ledger becomes Pending; for
// LedgerSessionReattest it must be Attested and stays so, with its current
// session, until Adopt. A session already pending is not added again.
func (l *HostLedger) BeginSession(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, store, session [16]byte,
	purpose HostLedgerSessionPurpose, tip HostLedgerPosition) (*HostLedgerRecord, error) {
	if session == ([16]byte{}) || (purpose != LedgerSessionAttest && purpose != LedgerSessionReattest) {
		return nil, fmt.Errorf("%w: a session needs an id and a purpose", errHostLedgerInput)
	}
	lh, err := l.resolve(ctx, provider, hostID)
	if err != nil {
		return nil, err
	}
	rec, err := l.update(ctx, lh, store, lh.enrolment, false, func(r *HostLedgerRecord) (bool, error) {
		if err := requireBound(lh, r); err != nil {
			return false, err
		}
		if tip.Seq < r.Position.Seq {
			return false, l.hold(ctx, lh, LedgerReasonAhead, r.StorePrefix(), false, "the ledger is above the host's chain tip")
		}
		if purpose == LedgerSessionReattest && r.AttestationState != infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested {
			return false, fmt.Errorf("store %s: a re-attestation needs an adopted session: %w", r.StorePrefix(), ErrHostLedgerNotAttested)
		}
		changed := false
		if !r.HasPendingSession(session) && !(r.AttestationID == session &&
			r.AttestationState == infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested) {
			r.PendingSessions = append(r.PendingSessions, HostLedgerPendingSession{ID: session, BegunAt: l.now().UTC().Truncate(time.Second)})
			if n := len(r.PendingSessions); n > maxLedgerPendingSessions {
				r.PendingSessions = r.PendingSessions[n-maxLedgerPendingSessions:]
			}
			changed = true
		}
		if purpose == LedgerSessionAttest && r.AttestationState != infravirtrigaudiov1beta1.HostAgentLedgerAttestationPending {
			r.AttestationState = infravirtrigaudiov1beta1.HostAgentLedgerAttestationPending
			changed = true
		}
		return changed, nil
	})
	if err != nil {
		return nil, err
	}
	l.clearHold(lh)
	return rec, nil
}

// Raise raises the ledger of store from a batch of verified positions: to
// the highest position, and the checkpoint to the highest checkpoint, of
// those signed in the ledger's current session (the others are dropped),
// and adds the host's current enrolment to the generation's enrolments. The
// ledger must be Attested. Two positions with one sequence number and
// different heads (among the batch, or against the ledger's position or
// checkpoint) hold the host (ledger_fork, persisted). A batch of heartbeat
// positions only raises the ledger at most once per 30 seconds per host.
// Nothing is written when nothing changes.
func (l *HostLedger) Raise(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, store [16]byte,
	batch []HostLedgerRaise) (*HostLedgerRecord, error) {
	for _, in := range batch {
		if err := validPosition(in.Position, in.Checkpoint); err != nil {
			return nil, err
		}
	}
	lh, err := l.resolve(ctx, provider, hostID)
	if err != nil {
		return nil, err
	}
	heartbeatOnly := len(batch) > 0
	for _, in := range batch {
		heartbeatOnly = heartbeatOnly && in.Heartbeat
	}
	if heartbeatOnly {
		l.mu.Lock()
		at := l.heartbeatAt[lh.key]
		l.mu.Unlock()
		if !at.IsZero() && l.now().Sub(at) < ledgerHeartbeatRaiseInterval {
			return l.Load(ctx, provider, hostID, store)
		}
	}
	rec, err := l.update(ctx, lh, store, lh.enrolment, false, func(r *HostLedgerRecord) (bool, error) {
		if err := requireBound(lh, r); err != nil {
			return false, err
		}
		if r.AttestationState != infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested {
			return false, fmt.Errorf("store %s: %w", r.StorePrefix(), ErrHostLedgerNotAttested)
		}
		heads := map[uint64][32]byte{r.Position.Seq: r.Position.Head}
		if r.Checkpoint.Seq > 0 {
			heads[r.Checkpoint.Seq] = r.Checkpoint.Head
		}
		fork := func(p HostLedgerPosition) bool {
			if h, ok := heads[p.Seq]; ok && h != p.Head {
				return true
			}
			heads[p.Seq] = p.Head
			return false
		}
		pos, cp := r.Position, r.Checkpoint
		for _, in := range batch {
			if in.Session != r.AttestationID {
				continue
			}
			if fork(in.Position) || (in.Checkpoint.Seq > 0 && fork(in.Checkpoint)) {
				return false, l.hold(ctx, lh, LedgerReasonFork, r.StorePrefix(), true,
					"two verified positions of one sequence number carry different heads")
			}
			if in.Position.Seq > pos.Seq {
				pos = in.Position
			}
			if in.Checkpoint.Seq > cp.Seq {
				cp = in.Checkpoint
			}
		}
		changed := pos != r.Position || cp != r.Checkpoint
		r.Position, r.Checkpoint = pos, cp
		if changed && !r.HasEnrolment(lh.enrolment) {
			r.GenerationEnrolments = append(r.GenerationEnrolments, bytes.Clone(lh.enrolment))
			if n := len(r.GenerationEnrolments); n > maxLedgerGenerationEnrolments {
				r.GenerationEnrolments = r.GenerationEnrolments[n-maxLedgerGenerationEnrolments:]
			}
		}
		return changed, nil
	})
	if err != nil {
		return nil, err
	}
	if heartbeatOnly {
		l.mu.Lock()
		if l.heartbeatAt == nil {
			l.heartbeatAt = map[string]time.Time{}
		}
		l.heartbeatAt[lh.key] = l.now()
		l.mu.Unlock()
	}
	l.clearHold(lh)
	return rec, nil
}

// Adopt makes pending session session the current session of the ledger of
// store, on the evidence of a verified nonce-bound answer of kind evidence
// that carries it (never an operation receipt): the previous current
// session, if any, moves to the session history fenced at the ledger's
// position now (at most eight are kept), the ledger becomes Attested and
// the pending sessions are cleared. A session that is not pending is
// ErrHostLedgerSessionNotPending; the current, adopted session is returned
// unchanged.
func (l *HostLedger) Adopt(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, store, session [16]byte,
	evidence HostLedgerAdoptEvidence) (*HostLedgerRecord, error) {
	if session == ([16]byte{}) || !evidence.valid() {
		return nil, fmt.Errorf("%w: adoption needs a session id and nonce-bound answer evidence", errHostLedgerInput)
	}
	lh, err := l.resolve(ctx, provider, hostID)
	if err != nil {
		return nil, err
	}
	rec, err := l.update(ctx, lh, store, lh.enrolment, true, func(r *HostLedgerRecord) (bool, error) {
		if err := requireBound(lh, r); err != nil {
			return false, err
		}
		if r.AttestationID == session && r.AttestationState == infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested {
			return false, nil
		}
		if !r.HasPendingSession(session) {
			return false, fmt.Errorf("store %s: %w", r.StorePrefix(), ErrHostLedgerSessionNotPending)
		}
		if r.AttestationID != ([16]byte{}) && r.AttestationID != session {
			r.SessionHistory = append(r.SessionHistory, HostLedgerFencedSession{ID: r.AttestationID, FencedAt: r.Position.Seq})
			if n := len(r.SessionHistory); n > maxLedgerSessionHistory {
				r.SessionHistory = r.SessionHistory[n-maxLedgerSessionHistory:]
			}
		}
		r.AttestationID, r.AttestationState, r.PendingSessions = session, infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested, nil
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	l.clearHold(lh)
	return rec, nil
}

// maybePersistIndex records a written record's write counter in lh's ledger
// index at most every ledgerIndexPersistInterval (and whenever the index
// does not name the store yet). A failure is logged: the next write retries.
func (l *HostLedger) maybePersistIndex(ctx context.Context, lh *ledgerHost, rec *HostLedgerRecord, idx *HostLedgerIndex) {
	if rec.HostUID != string(lh.host.UID) || rec.Archived {
		return
	}
	named := idx.Names(rec.StoreID)
	l.mu.Lock()
	at := l.indexPersisted[lh.key]
	l.mu.Unlock()
	if named && !at.IsZero() && l.now().Sub(at) < ledgerIndexPersistInterval {
		return
	}
	if named && idx.CounterSeen >= rec.WriteCounter {
		return
	}
	store := rec.StoreID
	if err := l.persistIndex(ctx, lh, &store, rec.WriteCounter); err != nil {
		log.FromContext(ctx).Info("Cannot record the ledger's write counter in the host's ledger index; retried on a later write",
			"host", lh.hostID, "store", rec.StorePrefix(), "error", err.Error())
	}
}

// persistIndex adds store (nil: none) to lh's ledger index and raises its
// write counter to at least counter, re-tagged, in one write that never
// overwrites a concurrent one. An index that fails its tag or form is never
// rewritten: it holds the host.
func (l *HostLedger) persistIndex(ctx context.Context, lh *ledgerHost, store *[16]byte, counter uint64) error {
	var tampered bool
	err := l.writeStoreState(ctx, lh, func(st *HostStoreState) {
		tampered = false
		x := st.LedgerIndex
		if len(st.ledgerIndexRaw) > 0 || (x != nil && !x.verifyTag(l.Keys, string(lh.host.UID))) {
			tampered = true
			return
		}
		next := x.clone()
		if next == nil {
			next = &HostLedgerIndex{}
		}
		if store != nil && !next.Names(*store) {
			next.StoreIDs = append(next.StoreIDs, *store)
			if n := len(next.StoreIDs); n > maxLedgerIndexStores {
				next.StoreIDs = next.StoreIDs[n-maxLedgerIndexStores:]
			}
		}
		next.CounterSeen = max(next.CounterSeen, counter)
		next.sign(l.Keys, string(lh.host.UID))
		st.LedgerIndex = next
	})
	if err != nil {
		return err
	}
	if tampered {
		prefix := ""
		if store != nil {
			prefix = storeIDPrefix(*store)
		}
		return l.hold(ctx, lh, LedgerReasonTampered, prefix, false, "the ledger index in the pins Secret fails its tag or form")
	}
	l.mu.Lock()
	if l.indexPersisted == nil {
		l.indexPersisted = map[string]time.Time{}
	}
	l.indexPersisted[lh.key] = l.now()
	l.mu.Unlock()
	return nil
}
