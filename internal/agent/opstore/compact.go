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
// number of records dropped. It is safe to interrupt between batches.
func (s *Store) Compact(ctx context.Context, batch int) (int, error) {
	if batch <= 0 {
		batch = DefaultCompactBatch
	}
	total := 0
	for {
		n, more, err := s.compactBatch(ctx, batch)
		total += n
		if err != nil {
			return total, fmt.Errorf("compact: %w", err)
		}
		if !more {
			return total, nil
		}
		if err := s.j.CrashPoint(PointMidCompaction); err != nil {
			return total, err
		}
	}
}

func (s *Store) compactBatch(ctx context.Context, batch int) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys [][]byte
	var ids []*pb.OpIdentity
	more := false
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		return tx.ForEach(bucketOps, nil, func(k, v []byte) error {
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
			if len(keys) == batch {
				more = true
				return nil
			}
			keys = append(keys, k)
			ids = append(ids, rec.GetIdentity())
			return nil
		})
	}); err != nil {
		return 0, false, err
	}
	if len(keys) == 0 {
		return 0, false, nil
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
		return 0, false, err
	}
	if guard.Off(guard.WitnessKept) {
		for _, id := range ids {
			for i := 0; i < 16; i++ {
				_ = s.fsys.Remove(s.witnessPath(id, i))
			}
		}
	}
	return len(keys), more, nil
}
