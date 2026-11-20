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
	"errors"
	"fmt"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// DefaultFreezeChunk is the largest number of operations one freeze
// transaction freezes (Options.FreezeChunk overrides it). A re-enrolment's
// freeze commits in as many transactions as the predecessor has open
// operations divided by this number, so the size and latency of one commit
// do not depend on how many operations were open. Each frozen operation
// writes a handful of records (its own, and for a retired one its tombstone,
// its VM record and an audit event), so one chunk is bounded at a few
// hundred records.
const DefaultFreezeChunk = 128

// freezeScanFactor is how many times DefaultFreezeChunk's records one freeze
// transaction may read, open or not.
const freezeScanFactor = 8

// errChunkFull stops the scan of a freeze chunk that found one more open
// operation than the chunk holds.
var errChunkFull = errors.New("freeze chunk full")

// freezeMarker is the freeze-in-progress marker: the predecessor enrolment
// whose freeze has more chunks to commit and the key of the last operation
// the committed chunks froze (empty before the first chunk). It is written in
// the same transaction as the chunk it describes, so a crash leaves the
// marker and the frozen operations in step.
type freezeMarker struct {
	prev   []byte
	cursor []byte
}

func (m *freezeMarker) encode() []byte {
	return append(append([]byte(nil), m.prev...), m.cursor...)
}

// readFreeze returns the freeze-in-progress marker, or nil when none exists.
func readFreeze(tx *journal.Tx) (*freezeMarker, error) {
	b, err := tx.Get(bucketMeta, keyFreeze)
	if err != nil {
		return nil, fmt.Errorf("read freeze marker: %w", err)
	}
	if b == nil {
		return nil, nil
	}
	if len(b) != idLen && len(b) != idLen+2*idLen {
		return nil, fmt.Errorf("%w: freeze marker has %d bytes", errCorruptRecord, len(b))
	}
	m := &freezeMarker{prev: append([]byte(nil), b[:idLen]...), cursor: append([]byte(nil), b[idLen:]...)}
	if len(m.cursor) > 0 && !bytes.HasPrefix(m.cursor, m.prev) {
		return nil, fmt.Errorf("%w: freeze cursor is not an operation of the frozen enrolment", errCorruptRecord)
	}
	return m, nil
}

// freezeScanCap is the most records one freeze transaction reads, of any
// state, so that a predecessor with very many finished operations cannot make
// one transaction scan them all.
func (s *Store) freezeScanCap() int {
	if s.opts.FreezeScanCap > 0 {
		return s.opts.FreezeScanCap
	}
	return freezeScanFactor * s.freezeChunk()
}

func (s *Store) freezeChunk() int {
	if s.opts.FreezeChunk > 0 {
		return s.opts.FreezeChunk
	}
	return DefaultFreezeChunk
}

// Reenrol replaces the active enrolment with e:
//
//  1. freeze: every non-terminal operation of the predecessor enrolment
//     with no step intent at all is retired (Retired{provider_changed}, with
//     its tombstone); every other one becomes
//     OutcomeUnknown{provider_changed} and keeps holding its VM; the
//     predecessor enrolment becomes frozen and no enrolment is active. The
//     freeze commits in chunks of at most Options.FreezeChunk operations
//     (DefaultFreezeChunk), in key order. The first chunk also freezes the
//     enrolment and writes the freeze-in-progress marker, which every chunk
//     advances and the last one removes, so a crash between chunks leaves a
//     marker that names where the freeze resumes;
//  2. activate: e becomes the active enrolment, only after the freeze is
//     complete.
//
// A crash during the freeze or between the two steps leaves the host with no
// active enrolment, which refuses every operation. Recover, or calling
// Reenrol again, finishes the freeze; every operation is frozen exactly once.
// The frozen enrolment's tombstones, VM tombstones, attempt marks and
// witnesses are kept, read-only, until ConfirmFreeze, and their op_ids are
// answered from them. e.StartEpoch must exceed every earlier start epoch. No
// runner may be active.
func (s *Store) Reenrol(ctx context.Context, e Enrolment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.running) > 0 {
		return fmt.Errorf("re-enrol: %w", ErrRunning)
	}
	var last uint64
	var prev []byte
	var pending *freezeMarker
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		m, err := readAgentMeta(tx)
		if err != nil {
			return err
		}
		last, prev = m.GetLastStartEpoch(), m.GetCurrentEnrolmentId()
		if pending, err = readFreeze(tx); err != nil {
			return err
		}
		// Refuse a used id before freezing anything.
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
		return nil
	}); err != nil {
		return fmt.Errorf("re-enrol: %w", err)
	}
	if last == 0 {
		return fmt.Errorf("re-enrol: %w: the host was never enrolled", ErrEnrolment)
	}
	if err := e.validate(last); err != nil {
		return fmt.Errorf("re-enrol: %w", err)
	}
	switch {
	case pending != nil && !guard.Off(guard.FreezeResumeReenrol):
		// A freeze that an earlier call or process began: finish it first.
		if err := s.freezeLocked(ctx, pending.prev); err != nil {
			return fmt.Errorf("re-enrol: resume freeze: %w", err)
		}
		if err := s.j.CrashPoint(PointMidReenrolment); err != nil {
			return err
		}
	case len(prev) > 0:
		if err := s.freezeLocked(ctx, prev); err != nil {
			return fmt.Errorf("re-enrol: freeze: %w", err)
		}
		if err := s.j.CrashPoint(PointMidReenrolment); err != nil {
			return err
		}
	}
	if err := s.activateBound(ctx, e, prev); err != nil {
		return fmt.Errorf("re-enrol: %w", err)
	}
	return nil
}

// freezeLocked freezes the open operations of the enrolment prev, one chunk
// per transaction, until none remain, and clears the freeze-in-progress
// marker with the last chunk. s.mu is held. s.freezing stays set until the
// last chunk commits, so a failure leaves the host refusing every operation.
func (s *Store) freezeLocked(ctx context.Context, prev []byte) error {
	s.freezing.Store(true)
	for {
		buf := s.newAuditBuf()
		var more bool
		_, err := s.j.Update(ctx, func(tx *journal.Tx) error {
			var err error
			more, err = freezeChunk(tx, prev, s.freezeChunk(), s.freezeScanCap(), buf)
			return err
		})
		s.settle(buf, err)
		if err != nil {
			return err
		}
		if !more {
			s.freezing.Store(false)
			return nil
		}
		if err := s.j.CrashPoint(PointMidFreeze); err != nil {
			return err
		}
	}
}

// freezeChunk freezes up to limit open operations of prev, in key order,
// after those the marker's cursor already passed. The first chunk (no marker)
// also marks the enrolment frozen and clears the active enrolment. It writes
// the marker with the new cursor and reports more when open operations
// may remain; the last chunk deletes the marker instead. The scan stops
// after scanCap records of any state; the cursor then advances to the last
// record read, so a later chunk never reads it again, and a chunk may freeze
// no operation at all (progress is the cursor).
func freezeChunk(tx *journal.Tx, prev []byte, limit, scanCap int, buf *auditBuf) (more bool, err error) {
	fm, err := readFreeze(tx)
	if err != nil {
		return false, err
	}
	begun := fm != nil
	switch {
	case !begun:
		e := &pb.Enrolment{}
		ok, err := get(tx, bucketEnrolments, prev, e)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, fmt.Errorf("%w: enrolment to freeze is unknown", ErrEnrolment)
		}
		e.State = pb.EnrolmentState_ENROLMENT_STATE_FROZEN
		if err := put(tx, bucketEnrolments, prev, e); err != nil {
			return false, err
		}
		m, err := readAgentMeta(tx)
		if err != nil {
			return false, err
		}
		m.CurrentEnrolmentId = nil
		if err := put(tx, bucketMeta, keyAgentMeta, m); err != nil {
			return false, err
		}
		fm = &freezeMarker{prev: append([]byte(nil), prev...)}
	case !sameBytes(fm.prev, prev):
		return false, fmt.Errorf("%w: a freeze of another enrolment is in progress", ErrEnrolment)
	}
	var open []*pb.OpRecord
	lastKey := fm.cursor
	scanned := 0
	err = tx.ForEachAfter(bucketOps, prev, fm.cursor, func(k, v []byte) error {
		if scanned == scanCap {
			more = true
			return errChunkFull
		}
		scanned++
		rec := &pb.OpRecord{}
		if err := unmarshalRecord(v, rec); err != nil {
			return err
		}
		if rec.GetState() != StateAccepted && rec.GetState() != StateExecuting {
			lastKey = k
			return nil
		}
		if len(open) == limit {
			more = true
			return errChunkFull
		}
		open = append(open, rec)
		lastKey = k
		return nil
	})
	if err != nil && !errors.Is(err, errChunkFull) {
		return false, err
	}
	for _, rec := range open {
		if err := freezeOp(tx, rec, buf); err != nil {
			return false, err
		}
	}
	switch {
	case more:
		fm.cursor = lastKey
		return true, tx.Put(bucketMeta, keyFreeze, fm.encode())
	case begun:
		return false, tx.Delete(bucketMeta, keyFreeze)
	}
	return false, nil
}

// freezeOp freezes one open operation: OutcomeUnknown when a step intent is
// durable, Retired (with its tombstone) otherwise.
func freezeOp(tx *journal.Tx, rec *pb.OpRecord, buf *auditBuf) error {
	id := rec.GetIdentity()
	key := opKey(id.GetEnrolmentId(), id.GetOpId())
	rec.Reason = ReasonProviderChanged
	if anyIntent(rec) {
		rec.State = StateOutcomeUnknown
		if err := put(tx, bucketOps, key, rec); err != nil {
			return err
		}
		return buf.write(tx, AuditEvent{Event: "frozen", Ref: OpRef{EnrolmentID: id.GetEnrolmentId(), OpID: id.GetOpId()},
			VMUID: id.GetVmUid(), Kind: providerv1.OperationKind(id.GetKind()), Reason: rec.GetReason(), State: rec.GetState()})
	}
	rec.State = StateRetired
	if err := put(tx, bucketOps, key, rec); err != nil {
		return err
	}
	return terminalWrites(tx, rec, buf)
}

func anyIntent(rec *pb.OpRecord) bool {
	for _, st := range rec.GetSteps() {
		if st.GetIntent() {
			return true
		}
	}
	return false
}

// MaxFreezeConfirmations bounds the enrolment ids one relayed freeze
// confirmation may name.
const MaxFreezeConfirmations = 16

// FreezeOutcome is what ConfirmFrozen did with one confirmed enrolment id.
// Its zero value is FreezeDeferred: an outcome that was not decided is
// never read as applied.
type FreezeOutcome int

const (
	// FreezeDeferred: not applied now, for a reason that can pass (the
	// confirmation was made for an enrolment that is not active, a freeze is
	// in progress, the predecessor is not frozen yet, the store could not be
	// read or written in time). Nothing changed; the confirmation is to be
	// relayed again.
	FreezeDeferred FreezeOutcome = iota
	// FreezeAccepted: this call retired the frozen predecessor, in a
	// committed transaction; compaction removes what remains of its records.
	FreezeAccepted
	// FreezeAlreadyApplied: the predecessor was retired before (its records
	// are being removed, or are gone). Nothing changed.
	FreezeAlreadyApplied
	// FreezeRefusedOverCapacity: MaxPendingDiscards discards are pending
	// (ErrTooManyDiscards). The predecessor stays frozen and nothing
	// changed; the confirmation is to be relayed again.
	FreezeRefusedOverCapacity
	// FreezeNotApplicable: the id is not an enrolment the store holds or
	// retired (unknown), is malformed, repeats an earlier id of the same
	// confirmation, or is the active enrolment. Relaying the same
	// confirmation again changes nothing.
	FreezeNotApplicable
)

// Settled reports whether the confirmation of the id needs no further
// relay: it was applied, or applying it again cannot change anything.
func (o FreezeOutcome) Settled() bool {
	return o == FreezeAccepted || o == FreezeAlreadyApplied || o == FreezeNotApplicable
}

// String names the outcome for logs.
func (o FreezeOutcome) String() string {
	switch o {
	case FreezeAccepted:
		return "accepted"
	case FreezeAlreadyApplied:
		return "already_applied"
	case FreezeRefusedOverCapacity:
		return "refused_over_capacity"
	case FreezeNotApplicable:
		return "not_applicable"
	default:
		return "deferred"
	}
}

// ConfirmFrozen applies a relayed confirmation that the manager holds no
// open operation under the frozen enrolments ids: each id the store holds as
// a frozen predecessor is discarded (ConfirmFreeze). It returns one outcome
// for each id, in order (nil when ids is empty or names more than
// MaxFreezeConfirmations ids, which applies nothing). The confirmation is
// unsigned, so it is applied only when it was made for the store's active
// enrolment (forEnrolment; otherwise every id is FreezeDeferred) and only to
// ids that are frozen; the active enrolment, unknown, repeated and
// malformed ids are FreezeNotApplicable, and an id retired before is
// FreezeAlreadyApplied. An id refused because MaxPendingDiscards discards
// are pending is FreezeRefusedOverCapacity, never reported as applied: the
// caller relays it again. A false confirmation costs only liveness: a
// discarded enrolment's operations are answered UNKNOWN_TO_AGENT, never run
// again (a new operation under a frozen enrolment is refused by its mint
// epoch). It is idempotent: confirming a retired enrolment again changes no
// record. The error, when not nil, is the store failure that stopped it; the
// ids it did not reach are FreezeDeferred.
func (s *Store) ConfirmFrozen(ctx context.Context, forEnrolment []byte, ids [][]byte) ([]FreezeOutcome, error) {
	if len(ids) == 0 || len(ids) > MaxFreezeConfirmations {
		return nil, nil
	}
	out := make([]FreezeOutcome, len(ids))
	v, err := s.HostView(ctx, 0)
	if err != nil {
		return out, fmt.Errorf("confirm frozen: %w", err)
	}
	if v.Enrolment == nil || (!sameBytes(v.Enrolment.ID, forEnrolment) && !guard.Off(guard.ConfirmForActive)) {
		return out, nil
	}
	seen := map[string]bool{}
	full := false
	for i, id := range ids {
		if len(id) != idLen || seen[string(id)] || sameBytes(id, v.Enrolment.ID) {
			out[i] = FreezeNotApplicable
			continue
		}
		seen[string(id)] = true
		if full {
			// Each further new discard would be refused the same way; one
			// read tells a retired or unknown id from a refused one.
			if out[i], err = s.unappliedOutcome(ctx, id, FreezeRefusedOverCapacity); err != nil {
				return out, err
			}
			continue
		}
		applied, err := s.confirmFreeze(ctx, id)
		switch {
		case err == nil && applied:
			out[i] = FreezeAccepted
		case err == nil:
			out[i] = FreezeAlreadyApplied
		case errors.Is(err, ErrTooManyDiscards):
			full = true
			if guard.Off(guard.FreezeRefusalReported) {
				out[i] = FreezeAccepted
				continue
			}
			if out[i], err = s.unappliedOutcome(ctx, id, FreezeRefusedOverCapacity); err != nil {
				return out, err
			}
		case errors.Is(err, ErrEnrolment):
			// Not a frozen predecessor now: retired before, unknown, or not
			// frozen yet (a freeze still in progress).
			if out[i], err = s.unappliedOutcome(ctx, id, FreezeDeferred); err != nil {
				return out, err
			}
		default:
			return out, err
		}
	}
	return out, nil
}

// unappliedOutcome is the outcome of a confirmed id that was not applied
// for the reason refused: FreezeAlreadyApplied when the store retired it
// before, FreezeNotApplicable when the store holds no enrolment record of
// it (it never knew it), refused otherwise. It reads the store once.
func (s *Store) unappliedOutcome(ctx context.Context, id []byte, refused FreezeOutcome) (FreezeOutcome, error) {
	out := refused
	err := s.j.View(ctx, func(tx *journal.Tx) error {
		retired, err := isRetired(tx, id)
		if err != nil {
			return err
		}
		if retired {
			out = FreezeAlreadyApplied
			return nil
		}
		known, err := has(tx, bucketEnrolments, id)
		if err != nil {
			return err
		}
		if !known {
			out = FreezeNotApplicable
		}
		return nil
	})
	if err != nil {
		return FreezeDeferred, fmt.Errorf("confirm frozen: read enrolment: %w", err)
	}
	return out, nil
}

// DefaultDiscardChunk is the most records one transaction of a confirmed
// discard removes (Options.DiscardChunk overrides it). The memory and the
// latency of one commit then do not depend on how many records the
// predecessor left: a full store holds hundreds of thousands, and removing
// them in one transaction made the agent exceed its unit's memory limit.
const DefaultDiscardChunk = 2048

// errDiscardChunkFull stops the scan of a discard chunk that collected as
// many keys as the chunk holds.
var errDiscardChunkFull = errors.New("discard chunk full")

func (s *Store) discardChunk() int {
	if guard.Off(guard.DiscardChunked) {
		return int(^uint(0) >> 1)
	}
	if s.opts.DiscardChunk > 0 {
		return s.opts.DiscardChunk
	}
	return DefaultDiscardChunk
}

// discardPrefixes are the records of enrolment prev that a discard removes,
// in the order it removes them.
func discardPrefixes(prev []byte) []struct {
	bucket string
	prefix []byte
} {
	return []struct {
		bucket string
		prefix []byte
	}{{bucketOps, prev}, {bucketTombstones, prev}, {bucketAttempts, prev},
		{bucketMeta, append([]byte(panicKeyPrefix), prev...)}}
}

// readDiscard returns the discard-in-progress marker: the retired
// enrolments whose records are still being removed, oldest first (nil when
// none).
func readDiscard(tx *journal.Tx) ([][]byte, error) {
	b, err := tx.Get(bucketMeta, keyDiscard)
	if err != nil {
		return nil, fmt.Errorf("read discard marker: %w", err)
	}
	if b == nil {
		return nil, nil
	}
	if len(b) == 0 || len(b)%idLen != 0 || len(b)/idLen > MaxPendingDiscards {
		return nil, fmt.Errorf("%w: discard marker has %d bytes", errCorruptRecord, len(b))
	}
	var ids [][]byte
	for i := 0; i < len(b); i += idLen {
		ids = append(ids, append([]byte(nil), b[i:i+idLen]...))
	}
	return ids, nil
}

// writeDiscard writes the discard-in-progress marker, or removes it when ids
// is empty.
func writeDiscard(tx *journal.Tx, ids [][]byte) error {
	if len(ids) == 0 {
		return tx.Delete(bucketMeta, keyDiscard)
	}
	var b []byte
	for _, id := range ids {
		b = append(b, id...)
	}
	return tx.Put(bucketMeta, keyDiscard, b)
}

// MaxPendingDiscards bounds the confirmed discards whose records are still
// being removed. A confirmation beyond it is refused (ErrTooManyDiscards,
// reported by ConfirmFrozen as FreezeRefusedOverCapacity) until compaction
// has finished one; the predecessor stays frozen until the confirmation is
// relayed again, which the Provider does, with a backoff, until the agent
// reports it applied. Reaching it needs that many re-keys of one Host within
// one sweep, a few minutes.
const MaxPendingDiscards = 2 * MaxFreezeConfirmations

func containsID(ids [][]byte, id []byte) bool {
	for _, x := range ids {
		if sameBytes(x, id) {
			return true
		}
	}
	return false
}

// ConfirmFreeze discards a frozen enrolment's records, once the manager has
// confirmed that it holds every operation of that enrolment: the operation
// records, op_id tombstones, attempt marks, panic counts, the enrolment record
// and the witness directory. VM tombstones are kept. VM records that the
// discarded operations held are released (the manager now holds those
// operations). The enrolment id is remembered as retired: it is never
// activated again, and a grant naming it is refused UNKNOWN_TO_AGENT, never
// as definitely not performed. It fails unless prev is frozen and another
// enrolment is active.
//
// It commits one transaction and returns: that transaction retires prev
// (from then on its operations are answered as discarded, whatever records
// remain), releases the VMs its operations held, adds prev to the
// discard-in-progress marker and removes the first Options.DiscardChunk
// records (DefaultDiscardChunk). A predecessor that fits is discarded
// completely. Otherwise Compact removes the rest, chunk by chunk (Discarding
// reports it, and the agent then compacts at its next check). It never
// sweeps in the caller's call, which may be a relayed operation with its own
// deadline. A confirmation of an enrolment already being removed returns nil.
// It waits for another discard transaction (one chunk) only as long as ctx
// allows.
func (s *Store) ConfirmFreeze(ctx context.Context, prev []byte) error {
	_, err := s.confirmFreeze(ctx, prev)
	return err
}

// confirmFreeze is ConfirmFreeze; applied is true when this call retired
// prev (its first discard transaction committed), false when prev was
// already being removed. An error after the commit (the witness directory,
// a crash point) is returned with applied true.
func (s *Store) confirmFreeze(ctx context.Context, prev []byte) (applied bool, err error) {
	if err := s.lockDiscard(ctx); err != nil {
		return false, fmt.Errorf("confirm freeze: %w", err)
	}
	defer s.unlockDiscard()
	done, _, err := s.discardStep(ctx, prev, true)
	if errors.Is(err, errAlreadyDiscarding) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("confirm freeze: %w", err)
	}
	if err := s.fsys.RemoveAll(s.witnessDir(prev)); err != nil {
		return true, fmt.Errorf("confirm freeze: remove witnesses: %w", err)
	}
	if !done {
		return true, s.j.CrashPoint(PointMidDiscard)
	}
	return true, nil
}

// ErrTooManyDiscards is returned when MaxPendingDiscards confirmed
// discards are still being removed: a further confirmation is refused
// (wrapped in ErrEnrolment) until compaction has finished one.
var ErrTooManyDiscards = errors.New("too many discards in progress")

// errAlreadyDiscarding: the enrolment a confirmation names is being removed.
var errAlreadyDiscarding = errors.New("already being discarded")

// lockDiscard takes the discard semaphore, or gives up when ctx ends.
func (s *Store) lockDiscard(ctx context.Context) error {
	select {
	case s.discardSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) unlockDiscard() { <-s.discardSem }

// resumeDiscard removes, chunk by chunk, the records of every enrolment the
// discard-in-progress marker names (and their witness directories, again),
// until none remain or ctx ends, and returns the records it removed.
// Compact calls it. It takes the discard semaphore for one chunk at a time,
// so a confirmation relayed meanwhile waits for at most one transaction.
func (s *Store) resumeDiscard(ctx context.Context) (int, error) {
	if guard.Off(guard.DiscardResume) {
		return 0, nil
	}
	total := 0
	var witnessed []byte
	for ctx.Err() == nil {
		n, done, prev, err := s.resumeStep(ctx, witnessed)
		total += n
		if err != nil || done {
			return total, err
		}
		witnessed = prev
		if err := s.j.CrashPoint(PointMidDiscard); err != nil {
			return total, err
		}
	}
	return total, ctx.Err()
}

// resumeStep commits one discard transaction for the oldest pending
// enrolment, holding the discard semaphore only for it, and returns the
// records removed, whether no discard is pending any more, and the
// enrolment it worked on. It removes that enrolment's witness directory
// first, unless witnessed names it (done earlier in this resume). An id
// the marker names that is not retired is never touched: the store is
// quarantined (inconsistent_store).
func (s *Store) resumeStep(ctx context.Context, witnessed []byte) (int, bool, []byte, error) {
	if err := s.lockDiscard(ctx); err != nil {
		return 0, false, nil, err
	}
	defer s.unlockDiscard()
	var pending [][]byte
	var retired bool
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		var err error
		if pending, err = readDiscard(tx); err != nil || len(pending) == 0 {
			return err
		}
		retired, err = isRetired(tx, pending[0])
		return err
	}); err != nil {
		return 0, false, nil, err
	}
	if len(pending) == 0 {
		return 0, true, nil, nil
	}
	prev := pending[0]
	if !retired && !guard.Off(guard.DiscardOnlyRetired) {
		// Only a confirmation writes the marker, and it retires the
		// enrolment in the same transaction: this is damage. Quarantine at
		// once, visibly, rather than stop compaction for good.
		s.j.Quarantine(journal.LostInconsistentStore, "the discard marker names an enrolment that is not retired")
		s.emit(AuditEvent{Event: "quarantined", Reason: string(journal.LostInconsistentStore)})
		return 0, false, nil, &journal.LostError{Reason: journal.LostInconsistentStore}
	}
	if !sameBytes(prev, witnessed) {
		// A removal that failed in ConfirmFreeze is retried here.
		if err := s.fsys.RemoveAll(s.witnessDir(prev)); err != nil {
			return 0, false, nil, fmt.Errorf("remove witnesses: %w", err)
		}
	}
	_, n, err := s.discardStep(ctx, prev, false)
	return n, false, prev, err
}

// discardStep commits one discard transaction for prev and reports whether
// prev's records are all gone, and how many it removed. With first set it
// starts the discard (startDiscard) in the same transaction as the first
// chunk; it returns errAlreadyDiscarding when prev is already pending.
// Without first it continues the discard of prev (done when prev is no
// longer pending).
func (s *Store) discardStep(ctx context.Context, prev []byte, first bool) (bool, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if first && s.freezing.Load() {
		return false, 0, fmt.Errorf("%w: a freeze is still in progress", ErrEnrolment)
	}
	done, removed, left := false, 0, 0
	_, err := s.j.Update(ctx, func(tx *journal.Tx) error {
		done, removed, left = false, 0, 0
		pending, err := readDiscard(tx)
		if err != nil {
			return err
		}
		switch {
		case first && containsID(pending, prev):
			return errAlreadyDiscarding
		case first && len(pending) >= MaxPendingDiscards:
			return fmt.Errorf("%w: %w", ErrEnrolment, ErrTooManyDiscards)
		case first:
			if err := startDiscard(tx, prev); err != nil {
				return err
			}
			pending = append(pending, append([]byte(nil), prev...))
			if err := writeDiscard(tx, pending); err != nil {
				return err
			}
		case !containsID(pending, prev):
			done, left = true, len(pending)
			return nil
		}
		limit := s.discardChunk()
		var keys []struct {
			bucket string
			key    []byte
		}
		for _, b := range discardPrefixes(prev) {
			if err := tx.ForEach(b.bucket, b.prefix, func(k, _ []byte) error {
				if len(keys) == limit {
					return errDiscardChunkFull
				}
				keys = append(keys, struct {
					bucket string
					key    []byte
				}{b.bucket, append([]byte(nil), k...)})
				return nil
			}); errors.Is(err, errDiscardChunkFull) {
				break
			} else if err != nil {
				return err
			}
		}
		for _, k := range keys {
			if err := tx.Delete(k.bucket, k.key); err != nil {
				return err
			}
		}
		removed = len(keys)
		if len(keys) == limit {
			left = len(pending)
			return nil
		}
		// Nothing of prev remains: remove its enrolment record and its
		// entry in the marker.
		done = true
		if err := tx.Delete(bucketEnrolments, prev); err != nil {
			return err
		}
		rest := make([][]byte, 0, len(pending))
		for _, id := range pending {
			if !sameBytes(id, prev) {
				rest = append(rest, id)
			}
		}
		left = len(rest)
		return writeDiscard(tx, rest)
	})
	if err != nil {
		return false, 0, err
	}
	s.discarding.Store(left > 0)
	return done, removed, nil
}

// startDiscard is the start of a confirmed discard of prev, inside the
// transaction of its first chunk: prev must be frozen and another enrolment
// active; prev is retired (from now on answered as discarded) and the VMs
// its operations held are released. The caller adds prev to the marker. The
// VM scan reads every VM record of the host once (read-only, bounded by the
// host's VMs, not by the predecessor's records).
func startDiscard(tx *journal.Tx, prev []byte) error {
	e := &pb.Enrolment{}
	ok, err := get(tx, bucketEnrolments, prev, e)
	if err != nil {
		return err
	}
	if !ok || (e.GetState() != pb.EnrolmentState_ENROLMENT_STATE_FROZEN && !guard.Off(guard.ConfirmOnlyFrozen)) {
		return fmt.Errorf("%w: enrolment is not frozen", ErrEnrolment)
	}
	act, err := activeEnrolment(tx)
	if err != nil {
		return err
	}
	if act == nil {
		return fmt.Errorf("%w: no successor enrolment is active", ErrEnrolment)
	}
	var held []*pb.VMRecord
	if err := tx.ForEach(bucketVMs, nil, func(_, v []byte) error {
		vm := &pb.VMRecord{}
		if err := unmarshalRecord(v, vm); err != nil {
			return err
		}
		if sameBytes(vm.GetBusyEnrolmentId(), prev) {
			held = append(held, vm)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, vm := range held {
		vm.BusyOpId, vm.BusyEnrolmentId = nil, nil
		if err := put(tx, bucketVMs, []byte(vm.GetVmUid()), vm); err != nil {
			return err
		}
	}
	m, err := readAgentMeta(tx)
	if err != nil {
		return err
	}
	retired := false
	for _, r := range m.GetRetiredEnrolmentIds() {
		retired = retired || sameBytes(r, prev)
	}
	if !retired {
		m.RetiredEnrolmentIds = append(m.RetiredEnrolmentIds, append([]byte(nil), prev...))
		return put(tx, bucketMeta, keyAgentMeta, m)
	}
	return nil
}
