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
// any other change; nothing verifies against a removed event. Every event
// was also emitted to Options.Audit (the system journal), which is the
// durable audit record; the store keeps a bounded recent window. It returns
// the number of events removed.
func (s *Store) TrimAudit(ctx context.Context, batch int) (int, error) {
	if batch <= 0 {
		batch = DefaultCompactBatch
	}
	count, size, err := s.auditUsage(ctx)
	if err != nil {
		return 0, fmt.Errorf("trim audit: %w", err)
	}
	removed := 0
	var after []byte
	for count > s.opts.AuditRetainEvents || size > s.opts.AuditRetainBytes {
		n, bytes, next, err := s.trimAuditBatch(ctx, batch, after, count, size)
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

// trimAuditBatch removes at most batch of the oldest audit events after the
// key after while count events of size bytes exceed a bound, and returns
// how many it removed, their bytes and the last key removed.
func (s *Store) trimAuditBatch(ctx context.Context, batch int, after []byte, count int, size int64) (int, int64, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys [][]byte
	var bytes int64
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		return tx.ForEachAfter(bucketAudit, nil, after, func(k, v []byte) error {
			if len(keys) == batch || (count-len(keys) <= s.opts.AuditRetainEvents && size-bytes <= s.opts.AuditRetainBytes) {
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
