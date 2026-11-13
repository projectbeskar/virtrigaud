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
package journal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"runtime/debug"

	bolt "go.etcd.io/bbolt"
)

// recordDomain separates the per-record digest from every other SHA-256 use.
const recordDomain = "vrtg-journal-rec-v1"

// recordVersion is the format version byte of a record trailer.
const recordVersion byte = 1

// trailerSize is the size of a record trailer: version || be64(seq) ||
// digest.
const trailerSize = 1 + 8 + sha256.Size

// scanCtxEvery is the number of records a scan reads between context checks.
const scanCtxEvery = 1024

// ErrCorruptRecord is wrapped by every error that reports a record, or the
// live-state digest, that does not verify. The store is quarantined with
// LostJournalCorrupt before such an error is returned.
var ErrCorruptRecord = errors.New("journal record does not verify")

// recordError names a record that does not verify. It never carries the
// record's value.
type recordError struct {
	bucket string
	key    []byte
	why    string
}

// Error implements error. The key is hex-encoded; the value is never named.
func (e *recordError) Error() string {
	if e.bucket == "" {
		return fmt.Sprintf("journal: live state: %s", e.why)
	}
	return fmt.Sprintf("journal: record in bucket %s with key %x: %s", e.bucket, e.key, e.why)
}

// Is makes errors.Is(err, ErrCorruptRecord) true.
func (e *recordError) Is(target error) bool { return target == ErrCorruptRecord }

// recordDigest is SHA-256(recordDomain || store_id || be32(len bucket) ||
// bucket || be32(len key) || key || be64(seq) || value).
func recordDigest(storeID [16]byte, bucket string, key []byte, seq uint64, value []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(recordDomain))
	h.Write(storeID[:])
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(bucket))) // #nosec G115 -- bucket names are short constants
	h.Write(n[:])
	h.Write([]byte(bucket))
	binary.BigEndian.PutUint32(n[:], uint32(len(key))) // #nosec G115 -- bbolt keys are at most 32 KiB
	h.Write(n[:])
	h.Write(key)
	var s [8]byte
	binary.BigEndian.PutUint64(s[:], seq)
	h.Write(s[:])
	h.Write(value)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// sealRecord returns value || trailer and the record's digest.
func sealRecord(storeID [16]byte, bucket string, key []byte, seq uint64, value []byte) ([]byte, [32]byte) {
	d := recordDigest(storeID, bucket, key, seq, value)
	out := make([]byte, 0, len(value)+trailerSize)
	out = append(out, value...)
	out = append(out, recordVersion)
	var s [8]byte
	binary.BigEndian.PutUint64(s[:], seq)
	out = append(out, s[:]...)
	out = append(out, d[:]...)
	return out, d
}

// unsealRecord verifies the trailer of a stored value and returns a copy of
// the value and its digest. bound is the highest commit position the record
// may name.
func unsealRecord(storeID [16]byte, bucket string, key, stored []byte, bound uint64) (value []byte, d [32]byte, why string) {
	if len(stored) < trailerSize {
		return nil, d, "value shorter than its trailer"
	}
	body, tr := stored[:len(stored)-trailerSize], stored[len(stored)-trailerSize:]
	if tr[0] != recordVersion {
		return nil, d, fmt.Sprintf("unknown record format version %d", tr[0])
	}
	seq := binary.BigEndian.Uint64(tr[1:9])
	if seq > bound {
		return nil, d, fmt.Sprintf("record names commit %d, after the committed position %d", seq, bound)
	}
	d = recordDigest(storeID, bucket, key, seq, body)
	if !bytes.Equal(d[:], tr[9:]) {
		return nil, d, "record digest mismatch"
	}
	return append([]byte{}, body...), d, ""
}

// addSum returns a+b mod 2^256, both big-endian.
func addSum(a, b [32]byte) [32]byte {
	var out [32]byte
	var carry uint64
	for i := 24; i >= 0; i -= 8 {
		s, c := bits.Add64(binary.BigEndian.Uint64(a[i:]), binary.BigEndian.Uint64(b[i:]), carry)
		binary.BigEndian.PutUint64(out[i:], s)
		carry = c
	}
	return out
}

// subSum returns a-b mod 2^256, both big-endian.
func subSum(a, b [32]byte) [32]byte {
	var out [32]byte
	var borrow uint64
	for i := 24; i >= 0; i -= 8 {
		s, c := bits.Sub64(binary.BigEndian.Uint64(a[i:]), binary.BigEndian.Uint64(b[i:]), borrow)
		binary.BigEndian.PutUint64(out[i:], s)
		borrow = c
	}
	return out
}

// fail remembers the first verification failure of the transaction and
// returns err. The store quarantines itself when the transaction ends, even if
// the caller dropped the error.
func (t *Tx) fail(err error) error {
	if t.bad == nil {
		t.bad = err
	}
	return err
}

// recordBound is the highest commit position a record read in this
// transaction may name: the transaction's own position when writing, the
// committed position of the snapshot when reading.
func (t *Tx) recordBound() (uint64, error) {
	if t.writable {
		return t.seq, nil
	}
	if t.boundSet {
		return t.bound, nil
	}
	m := t.btx.Bucket([]byte(bucketMeta))
	if m == nil {
		return 0, t.fail(&recordError{why: "meta bucket missing"})
	}
	pos, ok := decodePosition(m.Get(keyPosition))
	if !ok {
		return 0, t.fail(&recordError{why: "store position invalid"})
	}
	t.bound, t.boundSet = pos.Seq, true
	return t.bound, nil
}

// unseal verifies one stored value of a client bucket.
func (t *Tx) unseal(bucket string, key, stored []byte) (value []byte, d [32]byte, err error) {
	bound, err := t.recordBound()
	if err != nil {
		return nil, d, err
	}
	value, d, why := unsealRecord(t.storeID, bucket, key, stored, bound)
	if why != "" {
		return nil, d, t.fail(&recordError{bucket: bucket, key: append([]byte(nil), key...), why: why})
	}
	return value, d, nil
}

// finishState writes the live-state digest into the meta bucket. The write is
// not part of the transaction digest: the chain step hashes the state digest
// itself (see nextHead), so recording the meta write too would say the same
// thing twice.
func (t *Tx) finishState() error {
	if err := t.btx.Bucket([]byte(bucketMeta)).Put(keyStateDigest, append([]byte(nil), t.state[:]...)); err != nil {
		return fmt.Errorf("write live-state digest: %w", err)
	}
	return nil
}

// loadState reads the live-state digest of the snapshot into the transaction.
func (t *Tx) loadState() error {
	m := t.btx.Bucket([]byte(bucketMeta))
	if m == nil {
		return t.fail(&recordError{why: "meta bucket missing"})
	}
	v := m.Get(keyStateDigest)
	if len(v) != len(t.state) {
		return t.fail(&recordError{why: "live-state digest missing or damaged"})
	}
	copy(t.state[:], v)
	return nil
}

// scan verifies every record of every client bucket and compares the sum of
// their digests with the live-state digest recorded in the store. A failure is
// remembered in the transaction (see fail) and returned.
func (t *Tx) scan(ctx context.Context) error {
	m := t.btx.Bucket([]byte(bucketMeta))
	if m == nil {
		return t.fail(&recordError{why: "meta bucket missing"})
	}
	var want [32]byte
	v := m.Get(keyStateDigest)
	if len(v) != len(want) {
		return t.fail(&recordError{why: "live-state digest missing or damaged"})
	}
	copy(want[:], v)
	var sum [32]byte
	n := 0
	err := t.btx.ForEach(func(name []byte, b *bolt.Bucket) error {
		bucket := string(name)
		if checkClientBucket(bucket) != nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if n++; n%scanCtxEvery == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if v == nil {
				return t.fail(&recordError{bucket: bucket, key: append([]byte(nil), k...), why: nestedBucketWhy})
			}
			_, d, err := t.unseal(bucket, k, v)
			if err != nil {
				return err
			}
			sum = addSum(sum, d)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if sum != want {
		return t.fail(&recordError{why: "digest of the live records differs from the recorded one (a record was lost, restored or added)"})
	}
	return nil
}

// verifyChainState verifies the chain at the snapshot's committed position
// against the live-state digest recorded in the snapshot. A failure is
// remembered in the transaction (see fail) and returned.
func (t *Tx) verifyChainState() error {
	m := t.btx.Bucket([]byte(bucketMeta))
	if m == nil {
		return t.fail(&recordError{why: "meta bucket missing"})
	}
	pos, ok := decodePosition(m.Get(keyPosition))
	if !ok {
		return t.fail(&recordError{why: "store position invalid"})
	}
	if _, err := verifyChain(t.btx, pos); err != nil {
		return t.fail(&recordError{why: err.Error()})
	}
	return nil
}

// panicOnFault turns a memory fault on the calling goroutine (SIGBUS on a
// bbolt page whose backing blocks are unreadable, or whose file was
// truncated) into a recoverable panic, and returns the function that
// restores the previous setting. The setting is per goroutine: every entry
// point that touches bbolt's mapping calls it.
func panicOnFault() func() {
	old := debug.SetPanicOnFault(true)
	return func() { debug.SetPanicOnFault(old) }
}

// dbView runs fn in a read transaction of db. A panic before bbolt handed
// out the transaction (reading the meta page) leaves bbolt's locks held and
// the database unusable: it is recorded in s.tainted and the panic goes on.
// Once tainted, no call enters bbolt again: it would wait for those locks for
// good.
func (s *Store) dbView(db *bolt.DB, fn func(btx *bolt.Tx) error) error {
	if s.tainted.Load() {
		return &LostError{Reason: LostJournalCorrupt}
	}
	begun := false
	defer func() {
		if r := recover(); r != nil {
			if !begun {
				s.tainted.Store(true)
			}
			panic(r)
		}
	}()
	return db.View(func(btx *bolt.Tx) error {
		begun = true
		return fn(btx)
	})
}

// dbUpdate is dbView for a write transaction.
func (s *Store) dbUpdate(db *bolt.DB, fn func(btx *bolt.Tx) error) error {
	if s.tainted.Load() {
		return &LostError{Reason: LostJournalCorrupt}
	}
	begun := false
	defer func() {
		if r := recover(); r != nil {
			if !begun {
				s.tainted.Store(true)
			}
			panic(r)
		}
	}()
	return db.Update(func(btx *bolt.Tx) error {
		begun = true
		return fn(btx)
	})
}

// abandonLocked stops using a database that panicked while bbolt was
// starting a transaction. Closing it through bbolt would wait for locks that
// panic left held, so only the file descriptor is closed, which releases
// the file lock (dropped explicitly: bbolt's mapping, which stays until the
// process exits, would otherwise keep the open file description and its lock
// alive). The caller
// holds s.mu, or is Open, where no other goroutine can reach the store.
func (s *Store) abandonLocked() {
	s.db = nil
	if s.boltFile != nil {
		unlockFile(s.boltFile)
		_ = s.boltFile.Close()
		s.boltFile = nil
	}
}

// abandonIfTaintedLocked abandons the database when a panic left it
// unusable.
func (s *Store) abandonIfTaintedLocked() {
	if s.tainted.Load() {
		s.abandonLocked()
	}
}
