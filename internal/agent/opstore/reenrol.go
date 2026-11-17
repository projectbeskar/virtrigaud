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

// ConfirmFrozen applies a relayed confirmation that the manager holds no
// open operation under the frozen enrolments ids: each id the store holds as
// a frozen predecessor is discarded (ConfirmFreeze), and its id returned.
// The confirmation is unsigned, so it is applied only when it was made for
// the store's active enrolment (forEnrolment), names at most
// MaxFreezeConfirmations ids, and only to ids that are frozen; the active
// enrolment, unknown, discarded, repeated and malformed ids are skipped. A
// false confirmation costs only liveness: a discarded enrolment's operations
// are answered UNKNOWN_TO_AGENT, never run again (a new operation under a
// frozen enrolment is refused by its mint epoch). It is idempotent.
func (s *Store) ConfirmFrozen(ctx context.Context, forEnrolment []byte, ids [][]byte) ([][]byte, error) {
	if len(ids) == 0 || len(ids) > MaxFreezeConfirmations {
		return nil, nil
	}
	v, err := s.HostView(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("confirm frozen: %w", err)
	}
	if v.Enrolment == nil || (!sameBytes(v.Enrolment.ID, forEnrolment) && !guard.Off(guard.ConfirmForActive)) {
		return nil, nil
	}
	var done [][]byte
	seen := map[string]bool{}
	for _, id := range ids {
		if len(id) != idLen || seen[string(id)] || sameBytes(id, v.Enrolment.ID) {
			continue
		}
		seen[string(id)] = true
		switch err := s.ConfirmFreeze(ctx, id); {
		case err == nil:
			done = append(done, append([]byte(nil), id...))
		case errors.Is(err, ErrEnrolment):
			// Not a frozen predecessor (unknown, already discarded, or a
			// freeze still in progress): nothing to do.
		default:
			return done, err
		}
	}
	return done, nil
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

// readDiscard returns the discard-in-progress marker (the retired
// enrolment whose records are still being removed), or nil.
func readDiscard(tx *journal.Tx) ([]byte, error) {
	b, err := tx.Get(bucketMeta, keyDiscard)
	if err != nil {
		return nil, fmt.Errorf("read discard marker: %w", err)
	}
	if b == nil {
		return nil, nil
	}
	if len(b) != idLen {
		return nil, fmt.Errorf("%w: discard marker has %d bytes", errCorruptRecord, len(b))
	}
	return append([]byte(nil), b...), nil
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
// The first transaction retires prev, releases its VMs and writes the
// discard-in-progress marker: from then on the store answers prev's
// operations as discarded (UNKNOWN_TO_AGENT), whatever records remain.
// The records are then removed in transactions of at most
// Options.DiscardChunk records (DefaultDiscardChunk), and the last one
// removes the enrolment record and the marker. A small predecessor is
// discarded in that first transaction. A crash, or ctx ending, between
// chunks leaves the marker: Compact, the next ConfirmFreeze or a relayed
// confirmation of prev finishes the removal. A discard left by an earlier
// confirmation is finished before a new one starts.
func (s *Store) ConfirmFreeze(ctx context.Context, prev []byte) error {
	s.discardMu.Lock()
	defer s.discardMu.Unlock()
	pending, _, err := s.sweepPendingLocked(ctx)
	if err != nil {
		return fmt.Errorf("confirm freeze: %w", err)
	}
	if pending != nil && sameBytes(pending, prev) {
		// prev was confirmed before and its removal is now complete.
		return nil
	}
	if pending != nil && ctx.Err() != nil {
		return fmt.Errorf("confirm freeze: %w: an earlier discard is still in progress", ErrEnrolment)
	}
	done, _, err := s.discardStep(ctx, prev, true)
	if err != nil {
		return fmt.Errorf("confirm freeze: %w", err)
	}
	if err := s.fsys.RemoveAll(s.witnessDir(prev)); err != nil {
		return fmt.Errorf("confirm freeze: remove witnesses: %w", err)
	}
	if done {
		return nil
	}
	if err := s.j.CrashPoint(PointMidDiscard); err != nil {
		return err
	}
	if _, _, err := s.sweepPendingLocked(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("confirm freeze: %w", err)
	}
	// prev is retired: its operations are answered as discarded. Records
	// left by a cancelled ctx are removed by the next Compact.
	return nil
}

// resumeDiscard finishes a confirmed discard that a crash or a cancelled
// context interrupted, and returns the records it removed. Compact calls it.
func (s *Store) resumeDiscard(ctx context.Context) (int, error) {
	if guard.Off(guard.DiscardResume) {
		return 0, nil
	}
	s.discardMu.Lock()
	defer s.discardMu.Unlock()
	_, n, err := s.sweepPendingLocked(ctx)
	return n, err
}

// sweepPendingLocked removes, chunk by chunk, the records of the enrolment
// the discard-in-progress marker names, until none remain or ctx ends. It
// returns that enrolment (nil when no discard was in progress) and the
// records removed. The caller holds discardMu.
func (s *Store) sweepPendingLocked(ctx context.Context) ([]byte, int, error) {
	var pending []byte
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		var err error
		pending, err = readDiscard(tx)
		return err
	}); err != nil {
		return nil, 0, err
	}
	if pending == nil {
		return nil, 0, nil
	}
	total := 0
	for ctx.Err() == nil {
		done, n, err := s.discardStep(ctx, pending, false)
		total += n
		if err != nil {
			return pending, total, err
		}
		if done {
			return pending, total, nil
		}
		if err := s.j.CrashPoint(PointMidDiscard); err != nil {
			return pending, total, err
		}
	}
	return pending, total, ctx.Err()
}

// discardStep commits one discard transaction for prev and reports whether
// prev's records are all gone, and how many it removed. With first set it
// starts the discard: prev must be frozen, another enrolment active and no
// discard in progress; it retires prev, releases the VMs its operations
// held and writes the marker, in the same transaction as the first chunk.
// Without first it continues the discard the marker names (done when the
// marker no longer names prev).
func (s *Store) discardStep(ctx context.Context, prev []byte, first bool) (bool, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if first && s.freezing.Load() {
		return false, 0, fmt.Errorf("%w: a freeze is still in progress", ErrEnrolment)
	}
	done, removed := false, 0
	_, err := s.j.Update(ctx, func(tx *journal.Tx) error {
		done, removed = false, 0
		pending, err := readDiscard(tx)
		if err != nil {
			return err
		}
		if first {
			if pending != nil {
				return fmt.Errorf("%w: another discard is in progress", ErrEnrolment)
			}
			if err := startDiscard(tx, prev); err != nil {
				return err
			}
		} else if !sameBytes(pending, prev) {
			done = true
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
			return nil
		}
		// Nothing of prev remains: remove its enrolment record and the
		// marker.
		done = true
		if err := tx.Delete(bucketEnrolments, prev); err != nil {
			return err
		}
		return tx.Delete(bucketMeta, keyDiscard)
	})
	if err != nil {
		return false, 0, err
	}
	s.discarding.Store(!done)
	return done, removed, nil
}

// startDiscard is the start of a confirmed discard of prev, inside the
// transaction of its first chunk: prev must be frozen and another enrolment
// active; prev is retired (from now on answered as discarded), the VMs its
// operations held are released, and the discard-in-progress marker names
// prev.
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
		if err := put(tx, bucketMeta, keyAgentMeta, m); err != nil {
			return err
		}
	}
	return tx.Put(bucketMeta, keyDiscard, append([]byte(nil), prev...))
}
