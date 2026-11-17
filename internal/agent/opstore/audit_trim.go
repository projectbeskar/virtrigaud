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
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// errTrimBatchFull ends an audit trim scan that collected a full batch.
var errTrimBatchFull = errors.New("audit trim batch full")

// TrimAudit removes the oldest audit events beyond the retention bounds
// (Options.AuditRetainEvents and AuditRetainBytes): the newest events within
// both bounds stay. Audit keys are ordered by commit sequence, so the oldest
// go first. It touches the audit bucket only: op_id tombstones, VM
// tombstones, attempt marks and witness files stay, and so does every
// record compaction keeps. Each event is removed in a later transaction than
// the one that wrote it, batch events per transaction, each under the
// store's lock, so operations are admitted and run between batches. The
// journal's chain commits to the state digest, which a removal updates like
// any other change; nothing verifies against a removed event.
//
// Only events that reached the sink are removed. The sink (Options.Audit,
// the system journal) is the durable audit record, and is told only after a
// commit, so an event can be in the store and nowhere else: committed, and
// the agent stopped before the sink got it. The store therefore keeps an
// audit cursor, the highest commit sequence through which every event was
// emitted (it never passes a transaction whose events are not emitted yet),
// writes it to its meta bucket in a transaction of its own first, and then
// removes only events at or below the cursor it wrote. At open, every event
// above the persisted cursor is emitted again, marked Replayed (see
// replayAudit), so a crash between a commit and its emission duplicates
// journal lines and never loses one. It returns the number of events
// removed.
func (s *Store) TrimAudit(ctx context.Context, batch int) (int, error) {
	if batch <= 0 {
		batch = DefaultCompactBatch
	}
	cursor, err := s.persistAuditCursor(ctx)
	if err != nil {
		return 0, fmt.Errorf("trim audit: %w", err)
	}
	count, size, err := s.auditUsage(ctx)
	if err != nil {
		return 0, fmt.Errorf("trim audit: %w", err)
	}
	removed := 0
	var after []byte
	for count > s.opts.AuditRetainEvents || size > s.opts.AuditRetainBytes {
		n, bytes, next, err := s.trimAuditBatch(ctx, batch, after, count, size, cursor)
		removed += n
		if err != nil {
			return removed, fmt.Errorf("trim audit: %w", err)
		}
		if n == 0 {
			return removed, nil
		}
		count -= n
		size -= bytes
		after = next
	}
	return removed, nil
}

// auditUsage returns the number and the record bytes of the audit events.
func (s *Store) auditUsage(ctx context.Context) (count int, size int64, err error) {
	err = s.j.View(ctx, func(tx *journal.Tx) error {
		return tx.ForEach(bucketAudit, nil, func(k, v []byte) error {
			count++
			size += int64(len(k) + len(v))
			return nil
		})
	})
	return count, size, err
}

// persistAuditCursor writes the current audit cursor (auditEmittedThrough)
// to the meta bucket in its own transaction when it moved, and returns the
// cursor that is persisted.
func (s *Store) persistAuditCursor(ctx context.Context) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	through := s.auditEmittedThrough()
	s.auditMu.Lock()
	persisted := s.auditPersisted
	s.auditMu.Unlock()
	if through <= persisted {
		return persisted, nil
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], through)
	if _, err := s.j.Update(ctx, func(tx *journal.Tx) error {
		return tx.Put(bucketMeta, keyAuditEmitted, b[:])
	}); err != nil {
		return persisted, err
	}
	s.auditMu.Lock()
	s.auditPersisted = through
	s.auditMu.Unlock()
	return through, nil
}

// auditKeySeq returns the commit sequence of an audit key.
func auditKeySeq(k []byte) uint64 {
	if len(k) < 8 {
		return 0
	}
	return binary.BigEndian.Uint64(k[:8])
}

// replayAudit emits again, marked Replayed, every audit event above the
// persisted audit cursor: events that may not have reached the sink before
// the agent stopped. It runs at open and writes nothing (nothing may be
// committed before continuity is established): the cursor moves past the
// replayed events in memory, and TrimAudit persists it before it removes
// anything. A store whose audit events cannot be read replays nothing and
// keeps its cursor where it was, so nothing above it is ever removed.
func (s *Store) replayAudit(ctx context.Context) {
	var cursor uint64
	var events []AuditEvent
	high := uint64(0)
	err := s.j.View(ctx, func(tx *journal.Tx) error {
		b, err := tx.Get(bucketMeta, keyAuditEmitted)
		if err != nil {
			return err
		}
		if len(b) == 8 {
			cursor = binary.BigEndian.Uint64(b)
		}
		after := auditKey(cursor, math.MaxUint32)
		return tx.ForEachAfter(bucketAudit, nil, after, func(k, v []byte) error {
			rec := &pb.AuditEvent{}
			if err := unmarshalRecord(v, rec); err != nil {
				return err
			}
			events = append(events, AuditEvent{Event: rec.GetEvent(),
				Ref:   OpRef{EnrolmentID: rec.GetEnrolmentId(), OpID: rec.GetOpId()},
				VMUID: rec.GetVmUid(), Kind: providerv1.OperationKind(rec.GetKind()), Actor: rec.GetActor(),
				Reason: rec.GetReason(), State: rec.GetState(), Replayed: true})
			high = max(high, auditKeySeq(k))
			return nil
		})
	})
	s.auditMu.Lock()
	s.auditPersisted, s.auditHigh = cursor, cursor
	s.auditMu.Unlock()
	if err != nil {
		return
	}
	for _, ev := range events {
		s.emit(ev)
	}
	s.auditMu.Lock()
	s.auditHigh = max(s.auditHigh, high)
	s.auditMu.Unlock()
}

// trimAuditBatch removes at most batch of the oldest audit events after the
// key after, and none above the audit cursor, while count events of size
// bytes exceed a bound, and returns how many it removed, their bytes and the
// last key removed.
func (s *Store) trimAuditBatch(ctx context.Context, batch int, after []byte, count int, size int64, cursor uint64) (int, int64, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys [][]byte
	var bytes int64
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		return tx.ForEachAfter(bucketAudit, nil, after, func(k, v []byte) error {
			if len(keys) == batch || (count-len(keys) <= s.opts.AuditRetainEvents && size-bytes <= s.opts.AuditRetainBytes) {
				return errTrimBatchFull
			}
			if auditKeySeq(k) > cursor && !guard.Off(guard.AuditTrimEmittedOnly) {
				// Not known to have reached the sink: kept, and so is
				// everything after it.
				return errTrimBatchFull
			}
			keys = append(keys, k)
			bytes += int64(len(k) + len(v))
			return nil
		})
	}); err != nil && !errors.Is(err, errTrimBatchFull) {
		return 0, 0, nil, err
	}
	if len(keys) == 0 {
		return 0, 0, nil, nil
	}
	if _, err := s.j.Update(ctx, func(tx *journal.Tx) error {
		for _, k := range keys {
			if err := tx.Delete(bucketAudit, k); err != nil {
				return err
			}
		}
		if guard.Off(guard.AuditTrimOnlyAudit) {
			// The mutation: retention also takes the oldest tombstones.
			for _, b := range []string{bucketTombstones, bucketVMTombstones} {
				var drop [][]byte
				_ = tx.ForEach(b, nil, func(k, _ []byte) error {
					if len(drop) < len(keys) {
						drop = append(drop, k)
					}
					return nil
				})
				for _, k := range drop {
					_ = tx.Delete(b, k)
				}
			}
		}
		return nil
	}); err != nil {
		return 0, 0, nil, err
	}
	return len(keys), bytes, keys[len(keys)-1], nil
}
