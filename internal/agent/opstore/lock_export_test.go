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

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// HoldLock takes the store lock, for a test of a caller that must not block
// on it.
func HoldLock(s *Store) { s.mu.Lock() }

// ReleaseLock releases the lock HoldLock took.
func ReleaseLock(s *Store) { s.mu.Unlock() }

// CompactBatch runs one compaction batch after the key after (test only).
func CompactBatch(ctx context.Context, s *Store, batch int, after []byte) (dropped int, more bool, next []byte, err error) {
	return s.compactBatch(ctx, batch, after)
}

// CompactScanFactor is compactScanFactor (test only).
const CompactScanFactor = compactScanFactor

// BucketUsage is the number of keys and the key-plus-value bytes of one
// bucket (test only).
type BucketUsage struct {
	Keys  int
	Bytes int
}

// BucketUsages reads every bucket of the store (test only).
func BucketUsages(ctx context.Context, s *Store) (map[string]BucketUsage, error) {
	out := map[string]BucketUsage{}
	err := s.j.View(ctx, func(tx *journal.Tx) error {
		for _, b := range buckets() {
			u := BucketUsage{}
			if err := tx.ForEach(b, nil, func(k, v []byte) error {
				u.Keys++
				u.Bytes += len(k) + len(v)
				return nil
			}); err != nil {
				return err
			}
			out[b] = u
		}
		return nil
	})
	return out, err
}

// AuditKeys returns the keys of the audit events, oldest first (test only).
func AuditKeys(ctx context.Context, s *Store) ([][]byte, error) {
	var out [][]byte
	err := s.j.View(ctx, func(tx *journal.Tx) error {
		return tx.ForEach(bucketAudit, nil, func(k, _ []byte) error {
			out = append(out, k)
			return nil
		})
	})
	return out, err
}
