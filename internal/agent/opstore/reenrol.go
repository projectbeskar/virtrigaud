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
		buf := &auditBuf{}
		var more bool
		if _, err := s.j.Update(ctx, func(tx *journal.Tx) error {
			var err error
			more, err = freezeChunk(tx, prev, s.freezeChunk(), s.freezeScanCap(), buf)
			return err
		}); err != nil {
			return err
		}
		s.flush(buf)
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

// ConfirmFreeze discards a frozen enrolment's records, once the manager has
// confirmed that it holds every operation of that enrolment: the operation
// records, op_id tombstones, attempt marks, panic counts, the enrolment record
// and the witness directory. VM tombstones are kept. VM records that the discarded
// operations held are released (the manager now holds those operations). The
// enrolment id is remembered as retired: it is never activated again, and a
// grant naming it is refused UNKNOWN_TO_AGENT, never as definitely not
// performed. It fails unless prev is frozen and another enrolment is active.
func (s *Store) ConfirmFreeze(ctx context.Context, prev []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.freezing.Load() {
		return fmt.Errorf("confirm freeze: %w: a freeze is still in progress", ErrEnrolment)
	}
	_, err := s.j.Update(ctx, func(tx *journal.Tx) error {
		e := &pb.Enrolment{}
		ok, err := get(tx, bucketEnrolments, prev, e)
		if err != nil {
			return err
		}
		if !ok || e.GetState() != pb.EnrolmentState_ENROLMENT_STATE_FROZEN {
			return fmt.Errorf("%w: enrolment is not frozen", ErrEnrolment)
		}
		act, err := activeEnrolment(tx)
		if err != nil {
			return err
		}
		if act == nil {
			return fmt.Errorf("%w: no successor enrolment is active", ErrEnrolment)
		}
		for _, b := range []struct {
			bucket string
			prefix []byte
		}{{bucketOps, prev}, {bucketTombstones, prev}, {bucketAttempts, prev},
			{bucketMeta, append([]byte(panicKeyPrefix), prev...)}} {
			var keys [][]byte
			if err := tx.ForEach(b.bucket, b.prefix, func(k, _ []byte) error {
				keys = append(keys, k)
				return nil
			}); err != nil {
				return err
			}
			for _, k := range keys {
				if err := tx.Delete(b.bucket, k); err != nil {
					return err
				}
			}
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
		m.RetiredEnrolmentIds = append(m.RetiredEnrolmentIds, append([]byte(nil), prev...))
		if err := put(tx, bucketMeta, keyAgentMeta, m); err != nil {
			return err
		}
		return tx.Delete(bucketEnrolments, prev)
	})
	if err != nil {
		return fmt.Errorf("confirm freeze: %w", err)
	}
	dir := s.witnessDir(prev)
	if err := s.fsys.RemoveAll(dir); err != nil {
		return fmt.Errorf("confirm freeze: remove witnesses: %w", err)
	}
	return nil
}
