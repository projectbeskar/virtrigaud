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

package opstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Operation-store crash points, in addition to the journal's (a commit's
// PointAfterStoreCommit and PointAfterAnchor). Tests pass a crash hook in
// Options.Journal.Hooks.
const (
	// PointBeforeAccept: admission decided to accept; nothing is written.
	PointBeforeAccept journal.Point = "before-accept"
	// PointAfterIntent: a step intent is durable; no witness, no effect.
	PointAfterIntent journal.Point = "after-intent"
	// PointAfterWitness: a once step's witness is durable; no effect.
	PointAfterWitness journal.Point = "after-witness"
	// PointAfterEffect: the executor performed the step; done is not durable.
	PointAfterEffect journal.Point = "after-effect"
	// PointAfterLastDone: every step is done; the terminal record is not
	// written.
	PointAfterLastDone journal.Point = "after-last-done"
	// PointAfterTerminal: the terminal transaction is durable and anchored;
	// the caller has not answered.
	PointAfterTerminal journal.Point = "after-terminal"
	// PointMidCompaction: one compaction batch is durable, more remain.
	PointMidCompaction journal.Point = "mid-compaction"
	// PointMidReenrolment: the predecessor enrolment is frozen; the new one
	// is not active.
	PointMidReenrolment journal.Point = "mid-reenrolment"
	// PointMidFreeze: one chunk of a re-enrolment's freeze is durable and
	// the freeze-in-progress marker names the rest.
	PointMidFreeze journal.Point = "mid-freeze"
)

// DefaultMaxStoreBytes is the default store size above which new operations
// are refused (RefusalStoreFull). Terminal records are always written.
const DefaultMaxStoreBytes = 256 << 20

// Audit retention: the store keeps a bounded window of the newest audit
// events (each is also emitted to Options.Audit, the system journal, which is
// the durable audit record). TrimAudit removes the oldest beyond it.
const (
	// DefaultAuditRetainEvents is the default number of newest audit events
	// kept.
	DefaultAuditRetainEvents = 100_000
	// DefaultAuditRetainBytes is the default bound on the record bytes (key
	// and value) of the audit events kept.
	DefaultAuditRetainBytes = 32 << 20
)

// DefaultStepDeadline is the default time a step may run before it is
// resolved by inspection.
const DefaultStepDeadline = 10 * time.Minute

// Options configures Open.
type Options struct {
	// Journal configures the journal. Buckets is set by the operation store.
	Journal journal.Options
	// Executor performs the steps. Required.
	Executor executor.Executor
	// StepDeadlines bounds each step's run per kind. A missing kind uses
	// DefaultStepDeadline.
	StepDeadlines map[providerv1.OperationKind]time.Duration
	// MaxStoreBytes is the store size above which new operations are
	// refused. Zero uses DefaultMaxStoreBytes.
	MaxStoreBytes int64
	// AuditRetainEvents and AuditRetainBytes bound the audit events the
	// store keeps (TrimAudit): the newest events within both bounds stay.
	// Zero uses DefaultAuditRetainEvents and DefaultAuditRetainBytes.
	AuditRetainEvents int
	// AuditRetainBytes bounds the record bytes of the audit events kept.
	AuditRetainBytes int64
	// ReplayAudit replays at open the audit events above the persisted
	// audit cursor (replayAudit). Development builds set it; a release
	// build replays nothing (it runs no operation and never trims).
	ReplayAudit bool
	// FreezeChunk is the largest number of operations one re-enrolment
	// freeze transaction freezes. Zero uses DefaultFreezeChunk; tests set a
	// small value to exercise a freeze that takes several transactions.
	FreezeChunk int
	// FreezeScanCap is the most records (of any state) one re-enrolment
	// freeze transaction reads. Zero uses eight times the chunk size.
	FreezeScanCap int
	// Audit receives every admission decision and state change, including
	// refusals that write nothing, for example to write a structured
	// system-journal line. It never receives request bytes. Nil discards. It
	// may be called with the store lock held: it must not call back into the
	// store.
	Audit func(AuditEvent)
}

// AuditEvent describes one decision or state change. It holds identity fields
// only.
type AuditEvent struct {
	// Event names what happened: "accepted", "known", "refused", "step",
	// "terminal", "outcome_unknown", "frozen", "quarantined".
	Event string
	// Ref names the operation, if any.
	Ref OpRef
	// VMUID is the VM, if any.
	VMUID string
	// Kind is the operation kind, if any.
	Kind providerv1.OperationKind
	// Actor is the caller named in the request, if any.
	Actor string
	// Reason qualifies the event: a refusal reason or a state reason.
	Reason string
	// State is the operation's state after the event.
	State OpState
	// Replayed marks an event the store emits again at open because it may
	// not have reached the sink before the agent stopped (it was committed
	// after the last persisted audit cursor). The sink may have it already.
	Replayed bool
	// FirstSeq, on an EventReplaySkipped event, is the sequence of the first
	// skipped event (Seq is the last).
	FirstSeq uint64
	// Seq is the commit sequence of the transaction that stored the event
	// (its audit key's), the same in its first emission and in a replay;
	// zero for an event the store does not keep (a refusal, a re-delivery).
	// The commit time is not stored.
	Seq uint64
}

// Errors returned by the store.
var (
	// ErrNotReady is returned before Recover has run.
	ErrNotReady = errors.New("operation store not ready: recovery has not run or a re-enrolment freeze is incomplete")
	// ErrNoEnrolment is returned when no enrolment is active.
	ErrNoEnrolment = errors.New("operation store has no active enrolment")
	// ErrUnknownOperation is returned for an operation the store has no
	// record of.
	ErrUnknownOperation = errors.New("operation unknown to the store")
	// ErrRunning is returned when an operation already has a runner.
	ErrRunning = errors.New("operation already has a runner")
)

// Store is the per-host agent's durable operation store. See the package
// documentation.
type Store struct {
	j     *journal.Store
	exec  executor.Executor
	opts  Options
	fsys  journal.FS
	audit func(AuditEvent)

	// mu serialises every read-decide-write sequence: admissions, step
	// transitions, enrolment changes and compaction batches.
	mu    sync.Mutex
	ready bool
	// freezing is set while a re-enrolment's freeze is incomplete (its
	// marker is in the store, or the marker cannot be read). The host refuses
	// every operation while it is set; Recover and Reenrol finish the freeze.
	// It is written under mu and read without it (Freezing).
	freezing atomic.Bool
	// recovered mirrors ready for readers that must not wait for s.mu
	// (Ready), which Recover holds while it runs.
	recovered atomic.Bool
	running   map[string]bool
	dirs      map[string]bool

	// auditMu guards the audit cursor state: auditPending counts, by commit
	// sequence, the transactions whose audit events are not emitted yet;
	// auditHigh is the highest sequence registered or replayed;
	// auditPersisted is the cursor last written to bucketMeta.
	auditMu        sync.Mutex
	auditPending   map[uint64]int
	auditHigh      uint64
	auditPersisted uint64
	// auditCeiling, when auditCeilingSet, bounds the audit cursor for the
	// life of the process: the replay at open failed after emitting every
	// event through it, so nothing above it may be called emitted, persisted
	// or trimmed. auditFrozen (the cursor itself could not be read) forbids
	// persisting the cursor and trimming altogether.
	auditCeiling    uint64
	auditCeilingSet bool
	auditFrozen     bool
	// nonStored limits the audit events emitted without being stored.
	nonStored nonStoredGate
}

// Open opens the journal and the store. The store starts not ready: call
// EstablishContinuity, then Recover, before admitting operations. A store
// whose journal is lost is returned too; it serves reads and refuses every
// mutation.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.Executor == nil {
		return nil, errors.New("open operation store: no executor")
	}
	if opts.MaxStoreBytes == 0 {
		opts.MaxStoreBytes = DefaultMaxStoreBytes
	}
	if opts.AuditRetainEvents <= 0 {
		opts.AuditRetainEvents = DefaultAuditRetainEvents
	}
	if opts.AuditRetainBytes <= 0 {
		opts.AuditRetainBytes = DefaultAuditRetainBytes
	}
	jo := opts.Journal
	jo.Buckets = append(append([]string(nil), jo.Buckets...), buckets()...)
	j, err := journal.Open(ctx, jo)
	if err != nil {
		return nil, fmt.Errorf("open operation store: %w", err)
	}
	s := &Store{j: j, exec: opts.Executor, opts: opts, fsys: j.FS(), audit: opts.Audit,
		running: map[string]bool{}, dirs: map[string]bool{}}
	if err := s.checkMarker(ctx); err != nil {
		_ = j.Close()
		return nil, fmt.Errorf("open operation store: %w", err)
	}
	s.replayAudit(ctx)
	if err := j.View(ctx, func(tx *journal.Tx) error {
		fm, err := readFreeze(tx)
		s.freezing.Store(fm != nil)
		return err
	}); err != nil {
		var lostErr *journal.LostError
		if st, _ := j.State(); errors.As(err, &lostErr) || st == journal.StateLost {
			// The journal is already lost (no database, one bbolt cannot open):
			// the store is returned as before, quarantined and serving what
			// reads it can, and the host refuses every operation.
			s.freezing.Store(true)
			return s, nil
		}
		if !errors.Is(err, journal.ErrCorruptRecord) && !errors.Is(err, errCorruptRecord) {
			// Not damage (a cancelled context, a closed store): the caller
			// retries; nothing is quarantined on it.
			_ = j.Close()
			return nil, fmt.Errorf("open operation store: read freeze marker: %w", err)
		}
		// Fail closed: a marker that is damaged may name an incomplete freeze.
		// The host refuses every operation, the store is quarantined (unless
		// the failed read already did it) and keeps serving reads.
		s.freezing.Store(true)
		if st, _ := j.State(); st != journal.StateLost {
			j.Quarantine(journal.LostInconsistentStore, "freeze marker unreadable")
		}
	}
	return s, nil
}

// Freezing reports whether a re-enrolment's freeze is incomplete: the host
// refuses every operation until Recover or Reenrol finishes it. It does not
// wait for a freeze in progress.
func (s *Store) Freezing() bool { return s.freezing.Load() }

// Journal returns the underlying journal.
func (s *Store) Journal() *journal.Store { return s.j }

// MaxStoreBytes returns the store size above which new operations are
// refused (Options.MaxStoreBytes, or DefaultMaxStoreBytes).
func (s *Store) MaxStoreBytes() int64 { return s.opts.MaxStoreBytes }

// Close closes the store. It writes nothing.
func (s *Store) Close() error { return s.j.Close() }

// EstablishContinuity establishes the journal's continuity (see
// journal.Store.EstablishContinuity). With the local store alone this is the
// set of checks Open made.
func (s *Store) EstablishContinuity(ctx context.Context) error {
	if err := s.j.EstablishContinuity(ctx); err != nil {
		return fmt.Errorf("operation store: %w", err)
	}
	return nil
}

// emitNonStored emits an audit event the store does not keep, within the
// non-stored limit (nonStoredGate).
func (s *Store) emitNonStored(ev AuditEvent) {
	ok, summary := s.nonStored.admit(ev)
	if summary != nil {
		s.emit(*summary)
	}
	if ok {
		s.emit(ev)
	}
}

// FlushNonStored emits the summary of the non-stored audit events that
// were not emitted, once their window has closed, or at once with force
// (the agent's drain), so that the last window of a flood that stopped is
// summarised too. The agent calls it from its periodic tick.
func (s *Store) FlushNonStored(force bool) {
	if summary := s.nonStored.flush(force); summary != nil {
		s.emit(*summary)
	}
}

func (s *Store) emit(ev AuditEvent) {
	if s.audit != nil {
		s.audit(ev)
	}
}

// auditKey is be64(seq) || be32(index).
func auditKey(seq uint64, i int) []byte {
	k := make([]byte, 12)
	binary.BigEndian.PutUint64(k, seq)
	binary.BigEndian.PutUint32(k[8:], uint32(i)) // #nosec G115 -- far fewer than 2^32 events per transaction
	return k
}

// auditBuf collects the audit events one transaction writes, for the sink,
// which is told only after the commit (settle). The transaction's sequence
// is registered as pending with the store from the first event on, so the
// audit cursor never passes an event the sink has not received.
type auditBuf struct {
	s          *Store
	n          int
	events     []AuditEvent
	seq        uint64
	registered bool
}

// newAuditBuf returns an empty audit buffer of s.
func (s *Store) newAuditBuf() *auditBuf { return &auditBuf{s: s} }

func (a *auditBuf) write(tx *journal.Tx, ev AuditEvent) error {
	if !a.registered && a.s != nil {
		a.seq, a.registered = tx.Seq(), true
		a.s.auditPendingAdd(a.seq)
	}
	ev.Seq = tx.Seq()
	rec := &pb.AuditEvent{
		Event:       ev.Event,
		OpId:        ev.Ref.OpID,
		EnrolmentId: ev.Ref.EnrolmentID,
		VmUid:       ev.VMUID,
		Kind:        int32(ev.Kind),
		Actor:       ev.Actor,
		Reason:      ev.Reason,
		State:       ev.State,
	}
	if err := put(tx, bucketAudit, auditKey(tx.Seq(), a.n), rec); err != nil {
		return err
	}
	a.n++
	a.events = append(a.events, ev)
	return nil
}

// settle ends an audit buffer: after a commit (err nil) its events are
// emitted to the sink and only then is its sequence released, so the audit
// cursor can pass it. A transaction that failed releases its sequence
// without emitting: it committed nothing, or (an error after the commit) the
// journal is lost and refuses every later write, trims included, and the
// next open replays what it holds above the persisted cursor.
func (s *Store) settle(a *auditBuf, err error) {
	if err == nil {
		for _, ev := range a.events {
			s.emit(ev)
		}
	}
	if a.registered {
		s.auditPendingDone(a.seq)
	}
}

// auditPendingAdd registers seq as committed (or committing) without its
// events emitted yet.
func (s *Store) auditPendingAdd(seq uint64) {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	if s.auditPending == nil {
		s.auditPending = map[uint64]int{}
	}
	s.auditPending[seq]++
	s.auditHigh = max(s.auditHigh, seq)
}

// auditPendingDone releases one registration of seq.
func (s *Store) auditPendingDone(seq uint64) {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	if s.auditPending[seq] <= 1 {
		delete(s.auditPending, seq)
		return
	}
	s.auditPending[seq]--
}

// auditEmittedThrough returns the highest sequence through which every audit
// event was emitted: just below the oldest pending transaction, or the
// highest sequence registered (or replayed at open) when none is pending.
func (s *Store) auditEmittedThrough() uint64 {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	through := s.auditHigh
	for seq := range s.auditPending {
		through = min(through, seq-1)
	}
	if s.auditCeilingSet && !guard.Off(guard.AuditReplayCeiling) {
		through = min(through, s.auditCeiling)
	}
	return through
}

// Enrolment describes one enrolment of the host.
type Enrolment struct {
	// ID is the 16-byte enrolment identifier.
	ID []byte
	// ProviderUID is the Provider metadata.uid.
	ProviderUID string
	// HostID is the identity of the host.
	HostID string
	// InstallID is the installation identifier.
	InstallID []byte
	// Predecessors are earlier enrolment ids of this host that the store
	// never held (for example those of a store that was lost). They are
	// recorded as retired: never activated, and answered UNKNOWN_TO_AGENT.
	Predecessors [][]byte
	// StartEpoch is the epoch counter the enrolment starts at. It must be
	// greater than every earlier enrolment's.
	StartEpoch uint64
}

// ErrEnrolment reports an enrolment that cannot be made.
var ErrEnrolment = errors.New("enrolment refused")

func readAgentMeta(tx *journal.Tx) (*pb.AgentMeta, error) {
	m := &pb.AgentMeta{}
	if _, err := get(tx, bucketMeta, keyAgentMeta, m); err != nil {
		return nil, err
	}
	return m, nil
}

// activeEnrolment returns the active enrolment, or nil.
func activeEnrolment(tx *journal.Tx) (*pb.Enrolment, error) {
	m, err := readAgentMeta(tx)
	if err != nil {
		return nil, err
	}
	if len(m.GetCurrentEnrolmentId()) == 0 {
		return nil, nil
	}
	e := &pb.Enrolment{}
	ok, err := get(tx, bucketEnrolments, m.GetCurrentEnrolmentId(), e)
	if err != nil {
		return nil, err
	}
	if !ok || e.GetState() != pb.EnrolmentState_ENROLMENT_STATE_ACTIVE {
		return nil, nil
	}
	return e, nil
}

func (e Enrolment) validate(last uint64) error {
	switch {
	case len(e.ID) != idLen:
		return fmt.Errorf("%w: enrolment id must be %d bytes", ErrEnrolment, idLen)
	case e.ProviderUID == "" || len(e.ProviderUID) > maxNameLen:
		return fmt.Errorf("%w: provider uid empty or too long", ErrEnrolment)
	case e.HostID == "" || len(e.HostID) > maxNameLen:
		return fmt.Errorf("%w: host id empty or too long", ErrEnrolment)
	case e.StartEpoch == 0 || e.StartEpoch <= last:
		return fmt.Errorf("%w: start epoch %d must be above %d", ErrEnrolment, e.StartEpoch, last)
	}
	return nil
}

// isRetired reports whether id names an enrolment whose records were
// discarded after a confirmed freeze.
func isRetired(tx *journal.Tx, id []byte) (bool, error) {
	m, err := readAgentMeta(tx)
	if err != nil {
		return false, err
	}
	for _, r := range m.GetRetiredEnrolmentIds() {
		if sameBytes(r, id) {
			return true, nil
		}
	}
	return false, nil
}

// activate writes e as the active enrolment of the store storeID.
func activate(tx *journal.Tx, e Enrolment, storeID []byte) error {
	retired, err := isRetired(tx, e.ID)
	if err != nil {
		return err
	}
	used, err := has(tx, bucketEnrolments, e.ID)
	if err != nil {
		return err
	}
	if retired || used {
		return fmt.Errorf("%w: enrolment id already used", ErrEnrolment)
	}
	rec := &pb.Enrolment{EnrolmentId: e.ID, ProviderUid: e.ProviderUID, InstallId: e.InstallID,
		StartEpoch: e.StartEpoch, State: pb.EnrolmentState_ENROLMENT_STATE_ACTIVE, HostId: e.HostID,
		StoreId: storeID}
	if err := put(tx, bucketEnrolments, e.ID, rec); err != nil {
		return err
	}
	m, err := readAgentMeta(tx)
	if err != nil {
		return err
	}
	m.CurrentEnrolmentId = append([]byte(nil), e.ID...)
	m.LastStartEpoch = e.StartEpoch
	for _, p := range e.Predecessors {
		if len(p) != idLen || sameBytes(p, e.ID) {
			return fmt.Errorf("%w: invalid predecessor enrolment id", ErrEnrolment)
		}
		known, err := has(tx, bucketEnrolments, p)
		if err != nil {
			return err
		}
		if !known {
			m.RetiredEnrolmentIds = append(m.RetiredEnrolmentIds, append([]byte(nil), p...))
		}
	}
	return put(tx, bucketMeta, keyAgentMeta, m)
}

// Enrol makes the host's first enrolment in this store. It fails when any
// enrolment exists; use Reenrol to replace one.
//
// A fresh store cannot know whether e.ID was used before on this host: after
// the whole state set is lost, an Enrol with the old enrolment id would accept
// that enrolment's op_ids again. The caller must never re-use an enrolment id
// for a new store; the defence against a wiped host is outside it (a manager
// that remembers the host's store_id and chain position).
func (s *Store) Enrol(ctx context.Context, e Enrolment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := e.validate(0); err != nil {
		return fmt.Errorf("enrol: %w", err)
	}
	var enrolled bool
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		m, err := readAgentMeta(tx)
		enrolled = m.GetLastStartEpoch() != 0
		return err
	}); err != nil {
		return fmt.Errorf("enrol: %w", err)
	}
	if enrolled {
		return fmt.Errorf("enrol: %w: the host is already enrolled", ErrEnrolment)
	}
	return s.activateBound(ctx, e, nil)
}

// activateBound activates e with the marker protocol: the marker names e as
// pending, the activation commits, the marker names e as active. current is
// the enrolment the marker names now (nil for none). A crash at any point
// leaves a marker that Open accepts.
func (s *Store) activateBound(ctx context.Context, e Enrolment, current []byte) error {
	if err := s.j.BindMarker(journal.MarkerBinding{HostID: e.HostID, EnrolmentID: current, PendingEnrolmentID: e.ID}); err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	storeID := s.j.Position().StoreID
	if _, err := s.j.Update(ctx, func(tx *journal.Tx) error { return activate(tx, e, storeID[:]) }); err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	if err := s.j.BindMarker(journal.MarkerBinding{HostID: e.HostID, EnrolmentID: e.ID}); err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	return nil
}

// checkMarker compares the store marker's binding with the enrolments, at
// open. An active enrolment must be the marker's enrolment or its pending
// one, on the same host; with no active enrolment, the marker may name none
// or a frozen predecessor. Anything else quarantines the store
// (inconsistent_store). A marker left mid-activation is completed.
func (s *Store) checkMarker(ctx context.Context) error {
	if st, _ := s.j.State(); st == journal.StateLost {
		return nil
	}
	mk := s.j.Marker()
	var act *pb.Enrolment
	var frozen bool
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		var err error
		if act, err = activeEnrolment(tx); err != nil {
			return err
		}
		if len(mk.EnrolmentID) > 0 {
			e := &pb.Enrolment{}
			ok, err := get(tx, bucketEnrolments, mk.EnrolmentID, e)
			if err != nil {
				return err
			}
			frozen = ok && e.GetState() == pb.EnrolmentState_ENROLMENT_STATE_FROZEN
		}
		return nil
	}); err != nil {
		return err
	}
	var ok bool
	if act != nil {
		ok = mk.HostID == act.GetHostId() &&
			(sameBytes(mk.EnrolmentID, act.GetEnrolmentId()) || sameBytes(mk.PendingEnrolmentID, act.GetEnrolmentId()))
	} else {
		ok = len(mk.EnrolmentID) == 0 || frozen
	}
	if !ok && !guard.Off(guard.MarkerCheck) {
		s.j.Quarantine(journal.LostInconsistentStore, "store marker does not match the enrolment")
		return nil
	}
	if act != nil && (!sameBytes(mk.EnrolmentID, act.GetEnrolmentId()) || len(mk.PendingEnrolmentID) > 0) {
		return s.j.BindMarker(journal.MarkerBinding{HostID: act.GetHostId(), EnrolmentID: act.GetEnrolmentId()})
	}
	return nil
}

// Position returns the store's chain position (store_id, seq, head); its
// MarshalBinary form is what an agent signs.
func (s *Store) Position() journal.Position { return s.j.Position() }

// Lookup returns what the store knows about an operation: its record or, once
// compacted, its tombstone. Reads are served in every state, including a
// lost store that could be opened, so that status queries are answered.
func (s *Store) Lookup(ctx context.Context, ref OpRef) (OpView, bool, error) {
	var v OpView
	var found bool
	err := s.j.View(ctx, func(tx *journal.Tx) error {
		k := opKey(ref.EnrolmentID, ref.OpID)
		rec := &pb.OpRecord{}
		ok, err := get(tx, bucketOps, k, rec)
		if err != nil {
			return err
		}
		if ok {
			v, found = viewOfRecord(rec), true
			return nil
		}
		t := &pb.Tombstone{}
		ok, err = get(tx, bucketTombstones, k, t)
		if err != nil {
			return err
		}
		if ok {
			v, found = viewOfTombstone(t), true
		}
		return nil
	})
	if err != nil {
		return OpView{}, false, fmt.Errorf("lookup %s: %w", ref, err)
	}
	return v, found, nil
}

// HasTombstone reports whether the operation has a tombstone.
func (s *Store) HasTombstone(ctx context.Context, ref OpRef) (bool, error) {
	var ok bool
	err := s.j.View(ctx, func(tx *journal.Tx) error {
		var err error
		ok, err = has(tx, bucketTombstones, opKey(ref.EnrolmentID, ref.OpID))
		return err
	})
	return ok, err
}

// VMDeleted reports whether a completed Delete of the VM placement is
// recorded, and its op_id. Only a VM tombstone, written in the Delete's
// terminal transaction, counts; the absence of a VM record never does. VM
// tombstones are kept across re-enrolments.
func (s *Store) VMDeleted(ctx context.Context, vmUID string, binding []byte) ([]byte, bool, error) {
	var opID []byte
	var ok bool
	err := s.j.View(ctx, func(tx *journal.Tx) error {
		t := &pb.VMTombstone{}
		found, err := get(tx, bucketVMTombstones, vmTombstoneKey(vmUID, binding), t)
		if err != nil {
			return err
		}
		if found {
			opID, ok = t.GetDeleteOpId(), true
		}
		return nil
	})
	return opID, ok, err
}

// witnessDir is the witness directory of an enrolment.
func (s *Store) witnessDir(enrolment []byte) string {
	return filepath.Join(s.j.Dir(), witnessDirName, fmt.Sprintf("%x", enrolment))
}

// runKey names an operation in the running set.
func runKey(ref OpRef) string { return string(ref.EnrolmentID) + string(ref.OpID) }

// sameBytes is bytes.Equal, named for readability at call sites.
func sameBytes(a, b []byte) bool { return bytes.Equal(a, b) }
