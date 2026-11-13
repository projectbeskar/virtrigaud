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
	"errors"
	"fmt"
	"strings"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
)

// Tx is a journal transaction. A write transaction records every write so
// that the commit can hash exactly the bytes written into the chain.
// Returned slices are copies and stay valid after the transaction.
type Tx struct {
	btx      *bolt.Tx
	writable bool
	seq      uint64
	writes   []write
	buckets  map[string]*bolt.Bucket

	// storeID binds every record digest to this store.
	storeID [16]byte
	// bound and boundSet cache the committed position of a read snapshot.
	bound    uint64
	boundSet bool
	// state is the additive digest of the live client records, as updated by
	// this transaction's writes.
	state [32]byte
	// bad is the first record that did not verify; the store quarantines
	// itself at the end of the transaction.
	bad error
	// legacy marks a store whose values carry no trailer yet (before its
	// migration): reads return them raw and writes are refused.
	legacy bool
}

// lookup returns the named bucket, or nil, caching the handle for the life
// of the transaction.
func (t *Tx) lookup(name string) *bolt.Bucket {
	if b, ok := t.buckets[name]; ok {
		return b
	}
	b := t.btx.Bucket([]byte(name))
	if b != nil {
		if t.buckets == nil {
			t.buckets = map[string]*bolt.Bucket{}
		}
		t.buckets[name] = b
	}
	return b
}

// errReadOnly is returned by a write in a read transaction.
var errReadOnly = errors.New("journal: write in a read transaction")

// Seq returns the chain position this write transaction will commit as. It is
// 0 in a read transaction.
func (t *Tx) Seq() uint64 { return t.seq }

func (t *Tx) bucket(name string) (*bolt.Bucket, error) {
	b := t.lookup(name)
	if b == nil {
		return nil, fmt.Errorf("journal: bucket %s missing", name)
	}
	return b, nil
}

func checkClientBucket(name string) error {
	if strings.HasPrefix(name, reservedPrefix) {
		return fmt.Errorf("journal: bucket %s is reserved", name)
	}
	return nil
}

// errNotMigrated is returned by a write before the record trailers exist.
var errNotMigrated = errors.New("journal: store records not migrated yet")

// Get returns a copy of the value of key in bucket, or nil when the key is
// absent. A record whose trailer does not verify (a changed byte, another
// record's value, a short value, a bad version, a position beyond the committed
// one) is an error that wraps ErrCorruptRecord, never a missing key; the store
// is quarantined with LostJournalCorrupt when the transaction ends.
func (t *Tx) Get(bucket string, key []byte) ([]byte, error) {
	b := t.lookup(bucket)
	if b == nil {
		return nil, nil
	}
	v := b.Get(key)
	if v == nil {
		return nil, nil
	}
	if t.raw(bucket) {
		return append([]byte{}, v...), nil
	}
	val, _, err := t.unseal(bucket, key, v)
	return val, err
}

// Put writes key = value in a client bucket. The stored value carries a
// trailer that binds it to the store, bucket, key and this commit, and the
// store's live-state digest follows the change. An existing record that does
// not verify is an error wrapping ErrCorruptRecord.
func (t *Tx) Put(bucket string, key, value []byte) error {
	if !t.writable {
		return errReadOnly
	}
	if err := checkClientBucket(bucket); err != nil {
		return err
	}
	if t.legacy {
		return errNotMigrated
	}
	b, err := t.bucket(bucket)
	if err != nil {
		return err
	}
	k := append([]byte(nil), key...)
	if old := b.Get(k); old != nil {
		_, od, err := t.unseal(bucket, k, old)
		if err != nil {
			return err
		}
		t.state = subSum(t.state, od)
	}
	sealed, d := sealRecord(t.storeID, bucket, k, t.seq, value)
	if err := b.Put(k, sealed); err != nil {
		return fmt.Errorf("journal: put %s: %w", bucket, err)
	}
	t.state = addSum(t.state, d)
	t.writes = append(t.writes, write{op: writePut, bucket: bucket, key: k, value: sealed})
	return nil
}

// Delete removes key from a client bucket. Deleting a missing key is recorded
// and is not an error. An existing record that does not verify is an error
// wrapping ErrCorruptRecord.
func (t *Tx) Delete(bucket string, key []byte) error {
	if !t.writable {
		return errReadOnly
	}
	if err := checkClientBucket(bucket); err != nil {
		return err
	}
	if t.legacy {
		return errNotMigrated
	}
	b, err := t.bucket(bucket)
	if err != nil {
		return err
	}
	k := append([]byte(nil), key...)
	if old := b.Get(k); old != nil {
		_, od, err := t.unseal(bucket, k, old)
		if err != nil {
			return err
		}
		t.state = subSum(t.state, od)
	}
	if err := b.Delete(k); err != nil {
		return fmt.Errorf("journal: delete %s: %w", bucket, err)
	}
	t.writes = append(t.writes, write{op: writeDelete, bucket: bucket, key: k})
	return nil
}

// createBucket creates a client bucket if it does not exist.
func (t *Tx) createBucket(name string) error {
	if t.btx.Bucket([]byte(name)) != nil {
		return nil
	}
	if _, err := t.btx.CreateBucket([]byte(name)); err != nil {
		return fmt.Errorf("journal: create bucket %s: %w", name, err)
	}
	t.writes = append(t.writes, write{op: writeCreateBucket, bucket: name})
	return nil
}

// ForEach calls fn for every key of bucket that starts with prefix, in key
// order, with copies of the key and value. Writing to the bucket inside fn is
// not allowed. A record that does not verify stops the iteration with an
// error that wraps ErrCorruptRecord (see Get).
func (t *Tx) ForEach(bucket string, prefix []byte, fn func(k, v []byte) error) error {
	b := t.lookup(bucket)
	if b == nil {
		return nil
	}
	c := b.Cursor()
	for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
		val := append([]byte{}, v...)
		if !t.raw(bucket) {
			if v == nil {
				return t.fail(&recordError{bucket: bucket, key: append([]byte(nil), k...), why: "nested bucket in a client bucket"})
			}
			var err error
			if val, _, err = t.unseal(bucket, k, v); err != nil {
				return err
			}
		}
		if err := fn(append([]byte(nil), k...), val); err != nil {
			return err
		}
	}
	return nil
}

// Last returns a copy of the last key of bucket, or nil.
func (t *Tx) Last(bucket string) []byte {
	b := t.lookup(bucket)
	if b == nil {
		return nil
	}
	k, _ := b.Cursor().Last()
	if k == nil {
		return nil
	}
	return append([]byte(nil), k...)
}

// CommittedPosition returns the store's committed chain position as recorded
// in this transaction's snapshot: in a read transaction, the position the
// rest of what the transaction reads belongs to. ok is false when the store
// records no valid position or store identifier.
func (t *Tx) CommittedPosition() (pos Position, ok bool) {
	m := t.btx.Bucket([]byte(bucketMeta))
	if m == nil {
		return Position{}, false
	}
	pos, ok = decodePosition(m.Get(keyPosition))
	if !ok {
		return Position{}, false
	}
	meta := &journalpb.StoreMeta{}
	if err := proto.Unmarshal(m.Get(keyStoreMeta), meta); err != nil || len(meta.GetStoreId()) != len(pos.StoreID) {
		return Position{}, false
	}
	copy(pos.StoreID[:], meta.GetStoreId())
	return pos, true
}

// LastCheckpoint returns the newest chain checkpoint at or below seq and its
// head. ok is false when there is none.
func (t *Tx) LastCheckpoint(seq uint64) (cpSeq uint64, head [32]byte, ok bool) {
	if t.btx.Bucket([]byte(bucketCheckpoints)) == nil {
		return 0, head, false
	}
	return lastCheckpoint(t.btx, seq)
}

// HeadAt returns the chain head the store holds for seq, from a retained
// chain entry or a checkpoint. ok is false when the store holds none.
func (t *Tx) HeadAt(seq uint64) (head [32]byte, ok bool) {
	if seq == 0 || t.btx.Bucket([]byte(bucketChain)) == nil || t.btx.Bucket([]byte(bucketCheckpoints)) == nil {
		return head, false
	}
	return headAt(t.btx, seq)
}
