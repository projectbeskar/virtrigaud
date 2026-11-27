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

// The host ledger library (development builds only): reads, holds, the
// read-modify-write of a ledger object and the ledger index. The writes are
// in host_ledger_ops.go, the re-tag sweep in host_ledger_retag.go.
//
// HostLedger is the manager's library over the ledger objects
// (host_ledger.go). Every read is uncached (the API reader) and every change
// is one update carrying the resourceVersion it read, so a concurrent writer
// makes it fail and it is retried from a fresh read: nothing is ever
// overwritten. A record is returned only after its tag verified, or as the
// object the API server returned for this manager's own successful write;
// a caller signs only from such a record.
//
// A check that fails holds the host: the call returns a
// *HostLedgerHoldError (errors.Is ErrHostLedgerHeld) and an Event is emitted
// on the Host. The holds that cannot be recomputed from the stored objects
// (a regression, a deleted ledger or a foreign change seen by this process,
// a fork, a store mismatch under one enrolment) are persistent: they are
// written into the tagged ledger object (when there is one), into the tagged
// ledger index and as the pins Secret's store-regression hold, and end only
// with a new enrolment. A persistent hold that cannot be written is returned
// joined with the write's error, and stays held in this process.
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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
)

// Reasons a host ledger holds a host. They are Event messages' and the
// AgentLedgerHeld condition's reasons, and the stored reason of a
// persistent hold.
const (
	// LedgerReasonTampered: a ledger object or the ledger index failed its
	// tag or form, is missing although tagged evidence shows it existed, or
	// changed in a way this manager did not write.
	LedgerReasonTampered = "LedgerTampered"
	// LedgerReasonRegressed: a ledger object went back (a lower position or
	// write counter than this manager or the index saw) or disappeared while
	// this manager held it.
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
// host (the reason names the hold), False once it holds nothing. Set only by
// a manager with host ledger keys. Development builds only.
const HostConditionAgentLedgerHeld = "AgentLedgerHeld"

// reasonLedgerNotHeld is the AgentLedgerHeld condition's reason while the
// ledger holds nothing.
const reasonLedgerNotHeld = "NotHeld"

// Timing and bounds of the host ledger.
const (
	// ledgerHeartbeatRaiseInterval is the least time between two raises of
	// one host's ledger made only from heartbeat answers.
	ledgerHeartbeatRaiseInterval = 30 * time.Second
	// ledgerIndexPersistInterval is the least time between two index writes
	// of one host after a heartbeat-only raise.
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
	// errHostLedgerHoldNotPersisted: a persistent hold could not be written.
	errHostLedgerHoldNotPersisted = errors.New("the host ledger hold could not be persisted")
)

// HostLedgerHoldError is the error of a write or read that holds the host.
type HostLedgerHoldError struct {
	// Reason is one of the LedgerReason* constants (or the stored reason of
	// a persistent hold).
	Reason string
	// StorePrefix is the first 8 hexadecimal characters of the store id.
	StorePrefix string
	// Persistent reports a hold that ends only with a new enrolment.
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

// hostLedgerHold is a hold this process decided for a host.
type hostLedgerHold struct {
	reason     string
	persistent bool
	enrolment  []byte
}

// HostLedger is the manager's host ledger library. Set Client, APIReader,
// Trust, Keys and Config; the rest is optional. It is safe for concurrent
// use; writes of one store are serialised.
type HostLedger struct {
	// Client writes the ledger Secrets.
	Client client.Client
	// APIReader reads the ledger Secrets, pins Secrets and Hosts uncached.
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
	// RetagInterval is the least time between two writes of the re-tag
	// sweep (zero: 100 ms).
	RetagInterval time.Duration

	// lastAnswer returns when a verified answer of the Host object hostUID
	// (Provider UID, host id) last arrived (zero: none known). Set by
	// AttachLedger.
	lastAnswer func(providerUID, hostID, hostUID string) time.Time
	// markHeld writes a store-regression hold of a host. Set by
	// AttachLedger; nil: written through Trust.
	markHeld func(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID, reason string, enrolment []byte) error
	// updateStoreState writes a host's store state. Set by AttachLedger;
	// nil: Trust with conflict retries.
	updateStoreState func(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string,
		update func(*HostStoreState)) error

	mu             sync.Mutex
	locks          map[[16]byte]*sync.Mutex
	guards         map[[16]byte]*hostLedgerGuard
	holds          map[string]hostLedgerHold
	hadIndex       map[string]bool
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
// answer times by Host object (a rebind waits for the earlier Host to be
// quiet), writes store states through h's per-host writer and persists
// holds through h (so h holds the host at once), and the heartbeat mirrors
// l into the Hosts' status. A nil l detaches.
func (h *HostAnswers) AttachLedger(l *HostLedger) {
	h.Ledger = l
	if l == nil {
		return
	}
	l.lastAnswer = h.lastVerifiedAnswer
	l.markHeld = h.markRegressedErr
	l.updateStoreState = func(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string,
		update func(*HostStoreState)) error {
		return h.updateStore(ctx, provider, hostID, nil, update)
	}
}

// lastVerifiedAnswer returns when the last verified host answer of the Host
// object hostUID arrived (zero: none in this process). An answer recorded
// for another object of the same name (a replacement) does not count; one
// recorded without a Host UID counts for every object.
func (h *HostAnswers) lastVerifiedAnswer(providerUID, hostID, hostUID string) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.hosts[providerUID+"/"+hostID]
	if s == nil || (s.hostAnswerHostUID != "" && s.hostAnswerHostUID != hostUID) {
		return time.Time{}
	}
	return s.hostAnswerAt
}

// markRegressedErr is markRegressed reporting whether the hold was written.
func (h *HostAnswers) markRegressedErr(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID, reason string,
	enrolment []byte) error {
	h.markRegressed(ctx, provider, hostID, reason, enrolment)
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.hosts[hostAnswerKey(provider, hostID)]; s != nil && s.regressionPending {
		return fmt.Errorf("store-regression hold of host %s: %w", hostID, errHostLedgerHoldNotPersisted)
	}
	return nil
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

// uid returns the Host's UID.
func (lh *ledgerHost) uid() string { return string(lh.host.UID) }

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

// ledgerObject is a verified ledger object as read: the record and its
// Secret.
type ledgerObject struct {
	rec *HostLedgerRecord
	sec *corev1.Secret
}

// hold holds the host lh for reason: in memory, by an Event when the reason
// changed, and, when persistent, written (into obj when not nil, the index
// and the pins Secret). obj is nil for a ledger found regressed or changed
// behind this manager's back: such a copy is left untouched as evidence
// (rewriting it would re-tag it and raise its counter). It returns the hold error, joined with the error of
// a persistent hold that could not be written.
func (l *HostLedger) hold(ctx context.Context, lh *ledgerHost, reason, storePrefix string, persistent bool, detail string,
	obj *ledgerObject) error {
	l.mu.Lock()
	if l.holds == nil {
		l.holds = map[string]hostLedgerHold{}
	}
	prev := l.holds[lh.key]
	changed := prev.reason != reason
	if !prev.persistent || persistent {
		l.holds[lh.key] = hostLedgerHold{reason: reason, persistent: persistent, enrolment: bytes.Clone(lh.enrolment)}
	}
	l.mu.Unlock()
	log.FromContext(ctx).Info("The host's ledger holds the host; nothing is signed from it", "host", lh.hostID,
		"reason", reason, "store", storePrefix, "detail", detail)
	if changed && l.Recorder != nil {
		l.Recorder.Event(lh.host, corev1.EventTypeWarning, eventReasonLedgerHeld,
			fmt.Sprintf("the host is held by its ledger (%s) for store %s: %s", reason, storePrefix, detail))
	}
	herr := &HostLedgerHoldError{Reason: reason, StorePrefix: storePrefix, Persistent: persistent, Detail: detail}
	if !persistent {
		return herr
	}
	if err := l.persistHold(ctx, lh, reason, obj); err != nil {
		return errors.Join(herr, err)
	}
	return herr
}

// persistHold writes a persistent hold of lh under its current enrolment:
// into the ledger object obj (when not nil), into lh's ledger index, and as
// the pins Secret's store-regression hold. Every part is attempted; the
// error joins those that failed.
func (l *HostLedger) persistHold(ctx context.Context, lh *ledgerHost, reason string, obj *ledgerObject) error {
	var errs []error
	if obj != nil && (obj.rec.HeldReason != reason || !bytes.Equal(obj.rec.HeldEnrolment, lh.enrolment)) {
		next := obj.rec.clone()
		next.HeldReason, next.HeldEnrolment = reason, bytes.Clone(lh.enrolment)
		if _, err := l.writeObject(ctx, lh, obj.sec, next, nil, false); err != nil {
			errs = append(errs, fmt.Errorf("into the ledger of store %s: %w", obj.rec.StorePrefix(), err))
		}
	}
	if err := l.persistIndex(ctx, lh, indexChange{holdReason: reason}); err != nil && !errors.Is(err, ErrHostLedgerHeld) {
		errs = append(errs, fmt.Errorf("into the ledger index: %w", err))
	}
	if l.markHeld != nil {
		if err := l.markHeld(ctx, lh.provider, lh.hostID, reason, lh.enrolment); err != nil {
			errs = append(errs, err)
		}
	} else if err := l.writeStoreState(ctx, lh, func(st *HostStoreState) {
		st.RegressedReason, st.RegressedEnrolmentID = reason, bytes.Clone(lh.enrolment)
	}); err != nil {
		errs = append(errs, fmt.Errorf("into the pins Secret: %w", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", errHostLedgerHoldNotPersisted, errors.Join(errs...))
	}
	return nil
}

// clearHold forgets a transient in-memory hold of lh (a later call
// succeeded). A persistent hold is never cleared here: only a new enrolment
// ends it.
func (l *HostLedger) clearHold(lh *ledgerHost) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if h, ok := l.holds[lh.key]; ok && !h.persistent {
		delete(l.holds, lh.key)
	}
}

// memoryHold returns a persistent hold this process holds for lh under its
// current enrolment.
func (l *HostLedger) memoryHold(lh *ledgerHost) (hostLedgerHold, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	h, ok := l.holds[lh.key]
	return h, ok && h.persistent && bytes.Equal(h.enrolment, lh.enrolment)
}

// HeldReason returns the reason the host's ledger holds the host in this
// process, or "".
func (l *HostLedger) HeldReason(provider *infravirtrigaudiov1beta1.Provider, hostID string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holds[hostAnswerKey(provider, hostID)].reason
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

// storeState reads lh's store state and checks it: a persistent hold under
// the current enrolment (in memory, in the index or in the pins Secret)
// holds the host, and so does an index that fails its tag or form, or that
// is gone although this process saw it.
func (l *HostLedger) storeState(ctx context.Context, lh *ledgerHost, store [16]byte) (HostStoreState, error) {
	prefix := storeIDPrefix(store)
	if h, ok := l.memoryHold(lh); ok {
		return HostStoreState{}, &HostLedgerHoldError{Reason: h.reason, StorePrefix: prefix, Persistent: true,
			Detail: "the host is held by its ledger under its current enrolment"}
	}
	st, err := l.Trust.LoadStoreState(ctx, lh.provider, lh.hostID)
	if err != nil {
		return st, fmt.Errorf("store state of Host %s: %w", lh.hostID, err)
	}
	if st.RegressedReason != "" && bytes.Equal(st.RegressedEnrolmentID, lh.enrolment) {
		return st, &HostLedgerHoldError{Reason: st.RegressedReason, StorePrefix: prefix, Persistent: true,
			Detail: "the host is held for a store regression under its current enrolment"}
	}
	x := st.LedgerIndex
	switch {
	case len(st.ledgerIndexRaw) > 0:
		return st, l.hold(ctx, lh, LedgerReasonTampered, prefix, false, "the ledger index in the pins Secret is malformed", nil)
	case x != nil && !x.verifyTag(l.Keys, lh.uid()):
		return st, l.hold(ctx, lh, LedgerReasonTampered, prefix, false, "the ledger index in the pins Secret fails its tag", nil)
	case x == nil && l.sawIndex(lh.uid()):
		return st, l.hold(ctx, lh, LedgerReasonTampered, prefix, true, "the ledger index this manager saw is gone from the pins Secret", nil)
	case x != nil && x.HeldReason != "" && bytes.Equal(x.HeldEnrolment, lh.enrolment):
		return st, &HostLedgerHoldError{Reason: x.HeldReason, StorePrefix: prefix, Persistent: true,
			Detail: "the ledger index holds the host under its current enrolment"}
	}
	if x != nil {
		l.noteIndex(lh.uid())
	}
	return st, nil
}

// sawIndex reports whether this process saw a verified ledger index of the
// Host hostUID.
func (l *HostLedger) sawIndex(hostUID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hadIndex[hostUID]
}

// noteIndex records that this process saw a verified ledger index of the
// Host hostUID.
func (l *HostLedger) noteIndex(hostUID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hadIndex == nil {
		l.hadIndex = map[string]bool{}
	}
	l.hadIndex[hostUID] = true
}

// guardOf returns this process's guard of a store, if any.
func (l *HostLedger) guardOf(store [16]byte) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.guards[store] != nil
}

// readRecord reads and verifies the ledger object of store for lh: its
// labels, floor, form, store, tag, a hold stored in it, its write counter
// against lh's index, and this process's guard. A ledger bound to lh must be
// named by lh's index. idx is lh's verified index (nil: none). A missing
// object this process held is a regression.
func (l *HostLedger) readRecord(ctx context.Context, lh *ledgerHost, store [16]byte, idx *HostLedgerIndex) (*ledgerObject, error) {
	prefix := storeIDPrefix(store)
	sec := &corev1.Secret{}
	name := hostLedgerSecretName(store)
	if err := l.APIReader.Get(ctx, types.NamespacedName{Namespace: l.Config.Namespace, Name: name}, sec); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get host ledger of store %s: %w", prefix, err)
		}
		if l.guardOf(store) {
			return nil, l.hold(ctx, lh, LedgerReasonRegressed, prefix, true,
				"the ledger object this manager held is gone", nil)
		}
		return nil, fmt.Errorf("store %s: %w", prefix, ErrHostLedgerNotFound)
	}
	rec, err := l.verifySecret(ctx, sec, store)
	if err != nil {
		var tamper *ledgerTamper
		if errors.As(err, &tamper) {
			return nil, l.hold(ctx, lh, LedgerReasonTampered, prefix, false, tamper.detail, nil)
		}
		return nil, err
	}
	obj := &ledgerObject{rec: rec, sec: sec}
	if rec.HeldReason != "" && bytes.Equal(rec.HeldEnrolment, lh.enrolment) {
		return nil, &HostLedgerHoldError{Reason: rec.HeldReason, StorePrefix: prefix, Persistent: true,
			Detail: "the ledger holds the host under its current enrolment"}
	}
	if e, ok := idx.entry(store); ok && rec.WriteCounter < e.Counter {
		return nil, l.hold(ctx, lh, LedgerReasonRegressed, prefix, true,
			"the ledger's write counter is below the highest the manager recorded: an older copy was restored", nil)
	}
	if rec.HostUID == lh.uid() && !idx.Reserves(store) {
		return nil, l.hold(ctx, lh, LedgerReasonTampered, prefix, false,
			"the ledger serves this Host but the Host's ledger index does not name it", nil)
	}
	if reason, detail := l.checkGuard(rec); reason != "" {
		return nil, l.hold(ctx, lh, reason, prefix, true, detail, nil)
	}
	return obj, nil
}

// ledgerTamper is verifySecret's refusal of an object that is not a valid
// ledger object.
type ledgerTamper struct{ detail string }

// Error implements error.
func (t *ledgerTamper) Error() string { return t.detail }

// verifySecret verifies a ledger Secret of store (store zero: any store, as
// named by the Secret): labels, floor, form, name, tag. A failure is a
// *ledgerTamper (with a fixed detail: nothing a Secret writer chose is
// echoed), except a floor above this manager (errManagerTooOld).
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
		return nil, &ledgerTamper{"the ledger is malformed"}
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
	if lh == nil || rec.HostUID != lh.uid() || rec.Archived {
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

// setLedgerLabels sets the labels and Host annotation of a ledger Secret for
// the record it holds. enrolment is the enrolment label (nil: kept).
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

// writeObject writes next over the read Secret sec with sec's
// resourceVersion (a conflict is returned as is), verifies the object the
// API server returned and remembers it. enrolment is the enrolment label
// (nil: kept).
func (l *HostLedger) writeObject(ctx context.Context, lh *ledgerHost, sec *corev1.Secret, next *HostLedgerRecord, enrolment []byte,
	adopted bool) (*HostLedgerRecord, error) {
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
			return nil, err
		}
		return nil, fmt.Errorf("write the host ledger of store %s: %w", next.StorePrefix(), err)
	}
	return l.written(ctx, lh, out, next.StoreID, adopted)
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

// updateOptions are the options of one update.
type updateOptions struct {
	// enrolment is the enrolment label to set (nil: kept).
	enrolment []byte
	// adopt reports an adoption (for the mirror's attestedAt).
	adopt bool
	// lazyIndex records the write counter in the index at most every
	// ledgerIndexPersistInterval (heartbeat-only raises); otherwise the
	// index is written with every write.
	lazyIndex bool
}

// update is the read-modify-write of one store's ledger for the Host lh:
// a verified uncached read, mutate on a copy (it reports whether it
// changed anything, or an error that stops the write; it may hold the host
// through obj), and an update that carries the read resourceVersion; a
// conflict starts again from a fresh read. The index is then brought to the
// written counter. It returns the record as written (or as read when
// nothing changed).
func (l *HostLedger) update(ctx context.Context, lh *ledgerHost, store [16]byte, opts updateOptions,
	mutate func(next *HostLedgerRecord, obj *ledgerObject) (bool, error)) (*HostLedgerRecord, error) {
	m := l.storeLock(store)
	m.Lock()
	defer m.Unlock()
	for range maxLedgerWriteAttempts {
		st, err := l.storeState(ctx, lh, store)
		if err != nil {
			return nil, err
		}
		obj, err := l.readRecord(ctx, lh, store, st.LedgerIndex)
		if err != nil {
			return nil, err
		}
		next := obj.rec.clone()
		changed, err := mutate(next, obj)
		if err != nil {
			return nil, err
		}
		if !changed {
			l.remember(lh, obj.rec, false)
			return obj.rec, nil
		}
		got, err := l.writeObject(ctx, lh, obj.sec, next, opts.enrolment, opts.adopt)
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if got.HostUID == lh.uid() && !got.Archived {
			if err := l.recordCounter(ctx, lh, got, st.LedgerIndex, opts.lazyIndex); err != nil {
				return got, err
			}
		}
		return got, nil
	}
	return nil, fmt.Errorf("store %s: %w", storeIDPrefix(store), errHostLedgerConflict)
}

// recordCounter records a written record's write counter in lh's index:
// at once, or, with lazy, at most every ledgerIndexPersistInterval while
// the index already names the store.
func (l *HostLedger) recordCounter(ctx context.Context, lh *ledgerHost, rec *HostLedgerRecord, idx *HostLedgerIndex, lazy bool) error {
	if lazy && idx.Names(rec.StoreID) {
		l.mu.Lock()
		at := l.indexPersisted[lh.key]
		l.mu.Unlock()
		if !at.IsZero() && l.now().Sub(at) < ledgerIndexPersistInterval {
			return nil
		}
	}
	if err := l.persistIndex(ctx, lh, indexChange{stores: map[[16]byte]uint64{rec.StoreID: rec.WriteCounter},
		generation: rec.Generation}); err != nil {
		return fmt.Errorf("record store %s in the host's ledger index: %w", rec.StorePrefix(), err)
	}
	return nil
}

// requireBound refuses a write to a ledger that does not serve the Host lh
// or is archived.
func requireBound(lh *ledgerHost, rec *HostLedgerRecord) error {
	switch {
	case rec.Archived:
		return fmt.Errorf("store %s: %w", rec.StorePrefix(), ErrHostLedgerArchived)
	case rec.HostUID != lh.uid() || rec.HostIdentityMAC != lh.identityMAC:
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
	obj, err := l.readRecord(ctx, lh, store, st.LedgerIndex)
	if err != nil {
		return nil, err
	}
	l.remember(lh, obj.rec, false)
	l.clearHold(lh)
	return obj.rec, nil
}

// indexChange is a change of a ledger index: store counters to raise (0
// reserves a store not yet named), a generation to raise the high-water
// mark to, and a hold to record under the current enrolment.
type indexChange struct {
	stores     map[[16]byte]uint64
	generation uint64
	holdReason string
}

// apply returns x changed by c for the Host lh (x unchanged when nothing
// changes).
func (c indexChange) apply(x *HostLedgerIndex, lh *ledgerHost) *HostLedgerIndex {
	next := x.clone()
	if next == nil {
		next = &HostLedgerIndex{}
	}
	ids := make([][16]byte, 0, len(c.stores))
	for id := range c.stores {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b [16]byte) int { return bytes.Compare(a[:], b[:]) })
	for _, id := range ids {
		counter := c.stores[id]
		found := false
		for i := range next.Stores {
			if next.Stores[i].ID == id {
				next.Stores[i].Counter = max(next.Stores[i].Counter, counter)
				found = true
			}
		}
		if !found {
			next.Stores = append(next.Stores, HostLedgerIndexStore{ID: id, Counter: counter})
			if n := len(next.Stores); n > maxLedgerIndexStores {
				next.Stores = next.Stores[n-maxLedgerIndexStores:]
			}
		}
	}
	next.MaxGeneration = max(next.MaxGeneration, c.generation)
	if c.holdReason != "" {
		next.HeldReason, next.HeldEnrolment = c.holdReason, bytes.Clone(lh.enrolment)
	}
	return next
}

// persistIndex applies c to lh's ledger index, re-tagged, and sets the
// format's downgrade floor in store.json, in one write that never
// overwrites a concurrent one. Nothing is written when nothing changes. An
// index that fails its tag or form is never rewritten: it holds the host.
func (l *HostLedger) persistIndex(ctx context.Context, lh *ledgerHost, c indexChange) error {
	st, err := l.Trust.LoadStoreState(ctx, lh.provider, lh.hostID)
	if err != nil {
		return fmt.Errorf("store state of Host %s: %w", lh.hostID, err)
	}
	if len(st.ledgerIndexRaw) == 0 && (st.LedgerIndex == nil || st.LedgerIndex.verifyTag(l.Keys, lh.uid())) {
		cur := st.LedgerIndex
		if cur != nil && cur.equalContent(c.apply(cur, lh)) && cur.TagKeyID == l.Keys.CurrentKeyID() &&
			st.MinManagerVersion == l.floor() {
			return nil
		}
	}
	var tampered bool
	err = l.writeStoreState(ctx, lh, func(st *HostStoreState) {
		tampered = false
		x := st.LedgerIndex
		if len(st.ledgerIndexRaw) > 0 || (x != nil && !x.verifyTag(l.Keys, lh.uid())) {
			tampered = true
			return
		}
		next := c.apply(x, lh)
		next.sign(l.Keys, lh.uid())
		st.LedgerIndex = next
		st.MinManagerVersion = l.floor()
	})
	if err != nil {
		return err
	}
	if tampered {
		return l.hold(ctx, lh, LedgerReasonTampered, "", false, "the ledger index in the pins Secret fails its tag or form", nil)
	}
	l.noteIndex(lh.uid())
	l.mu.Lock()
	if l.indexPersisted == nil {
		l.indexPersisted = map[string]time.Time{}
	}
	l.indexPersisted[lh.key] = l.now()
	l.mu.Unlock()
	return nil
}
