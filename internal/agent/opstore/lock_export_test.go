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
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// HoldLock takes the store lock, for a test of a caller that must not block
// on it.
func HoldLock(s *Store) { s.mu.Lock() }

// ReleaseLock releases the lock HoldLock took.
func ReleaseLock(s *Store) { s.mu.Unlock() }

// HoldDiscard takes the discard semaphore, for a test of a caller that must
// not wait for it past its context.
func HoldDiscard(s *Store) { s.discardSem <- struct{}{} }

// ReleaseDiscard releases what HoldDiscard took.
func ReleaseDiscard(s *Store) { <-s.discardSem }

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

// HoldAuditPending registers seq as a transaction whose audit events are not
// emitted yet, and returns its release (test only).
func HoldAuditPending(s *Store, seq uint64) (release func()) {
	s.auditPendingAdd(seq)
	return func() { s.auditPendingDone(seq) }
}

// AuditKeySeq is auditKeySeq (test only).
func AuditKeySeq(k []byte) uint64 { return auditKeySeq(k) }

// PersistedAuditCursor reads the audit cursor from the meta bucket (test
// only).
func PersistedAuditCursor(ctx context.Context, s *Store) (uint64, bool, error) {
	var out uint64
	var found bool
	err := s.j.View(ctx, func(tx *journal.Tx) error {
		b, err := tx.Get(bucketMeta, keyAuditEmitted)
		if len(b) == 8 {
			out, found = binary.BigEndian.Uint64(b), true
		}
		return err
	})
	return out, found, err
}

// SetNonStoredClock replaces the clock of the non-stored event limit (test
// only).
func SetNonStoredClock(s *Store, now func() time.Time) { s.nonStored.now = now }

// NonStoredBurst is nonStoredBurst (test only).
const NonStoredBurst = nonStoredBurst

// SetReplaySkipFault makes the replay's count fail with err (test only); the
// returned function restores it.
func SetReplaySkipFault(err error) (restore func()) {
	old := replaySkipFault
	replaySkipFault = func(n int, e error) (int, error) {
		if err != nil {
			return 0, err
		}
		return n, e
	}
	return func() { replaySkipFault = old }
}

// PutRawAudit writes value as the audit record of key (seq, i), as if a
// commit had stored it (test only).
func PutRawAudit(ctx context.Context, s *Store, seq uint64, i int, value []byte) error {
	_, err := s.j.Update(ctx, func(tx *journal.Tx) error { return tx.Put(bucketAudit, auditKey(seq, i), value) })
	return err
}

// PutRawAuditCursor writes value as the audit cursor (test only).
func PutRawAuditCursor(ctx context.Context, s *Store, value []byte) error {
	_, err := s.j.Update(ctx, func(tx *journal.Tx) error { return tx.Put(bucketMeta, keyAuditEmitted, value) })
	return err
}
