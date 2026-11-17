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
	"errors"
	"fmt"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
)

// DefaultCompactBatch is the default number of records dropped per
// compaction transaction.
const DefaultCompactBatch = 256

// Compact drops the payload (the step journal) of finished operations,
// batch records per transaction. It keeps:
//
//   - every op_id tombstone (the tombstone already holds the terminal
//     state, irrev, kind, VM, binding, attempt and intent digest);
//   - every witness file;
//   - every VM tombstone;
//   - the full record of an operation that still holds its VM
//     (OutcomeUnknown, and a dirty failure until it is resolved).
//
// A record is dropped only when its tombstone is present. Compact returns the
// number of records dropped. It is safe to interrupt between batches, and to
// run while the store admits and runs operations: each batch holds the
// store's lock for one scan and one transaction, and drops only records of
// finished operations, which nothing changes again. The scan resumes after
// the last key the previous batch reached, so one Compact reads the bucket
// once, and a batch reads at most compactScanFactor times batch records
// before it lets go of the lock, so a store full of records that cannot be
// dropped never holds admission for a whole scan. The agent runs it
// periodically and when the store nears its size threshold (package daemon).
func (s *Store) Compact(ctx context.Context, batch int) (int, error) {
	if batch <= 0 {
		batch = DefaultCompactBatch
	}
	total := 0
	var after []byte
	for {
		n, more, next, err := s.compactBatch(ctx, batch, after)
		total += n
		if err != nil {
			return total, fmt.Errorf("compact: %w", err)
		}
		if !more {
			return total, nil
		}
		after = next
		if err := s.j.CrashPoint(PointMidCompaction); err != nil {
			return total, err
		}
	}
}

// compactScanFactor bounds the records one compaction batch reads, as a
// multiple of the batch size.
const compactScanFactor = 16

// errBatchFull ends a compaction scan that found one more record to drop
// than the batch holds, or read its share of records.
var errBatchFull = errors.New("compaction batch full")

// compactBatch drops at most batch records, scanning after the key after. It
// returns the records dropped, whether the scan stopped before the end of
// the bucket, and the key to resume after.
func (s *Store) compactBatch(ctx context.Context, batch int, after []byte) (int, bool, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys [][]byte
	var ids []*pb.OpIdentity
	more := false
	var last []byte
	visited := 0
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		return tx.ForEachAfter(bucketOps, nil, after, func(k, v []byte) error {
			if len(keys) == batch || visited == batch*compactScanFactor {
				more = true
				return errBatchFull
			}
			visited++
			last = k
			rec := &pb.OpRecord{}
			if err := unmarshalRecord(v, rec); err != nil {
				return err
			}
			if !Terminal(rec.GetState()) || holdsVM(rec.GetState()) {
				return nil
			}
			tombstoned, err := has(tx, bucketTombstones, k)
			if err != nil {
				return err
			}
			if !tombstoned {
				return nil
			}
			keys = append(keys, k)
			ids = append(ids, rec.GetIdentity())
			return nil
		})
	}); err != nil && !errors.Is(err, errBatchFull) {
		return 0, false, nil, err
	}
	if len(keys) == 0 {
		return 0, more, last, nil
	}
	if _, err := s.j.Update(ctx, func(tx *journal.Tx) error {
		for _, k := range keys {
			if err := tx.Delete(bucketOps, k); err != nil {
				return err
			}
			if guard.Off(guard.TombstoneKept) {
				if err := tx.Delete(bucketTombstones, k); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return 0, false, nil, err
	}
	if guard.Off(guard.WitnessKept) {
		for _, id := range ids {
			for i := 0; i < 16; i++ {
				_ = s.fsys.Remove(s.witnessPath(id, i))
			}
		}
	}
	return len(keys), more, last, nil
}
