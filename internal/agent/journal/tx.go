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

// Get returns a copy of the value of key in bucket, or nil.
func (t *Tx) Get(bucket string, key []byte) []byte {
	b := t.lookup(bucket)
	if b == nil {
		return nil
	}
	v := b.Get(key)
	if v == nil {
		return nil
	}
	return append([]byte{}, v...)
}

// Put writes key = value in a client bucket.
func (t *Tx) Put(bucket string, key, value []byte) error {
	if !t.writable {
		return errReadOnly
	}
	if err := checkClientBucket(bucket); err != nil {
		return err
	}
	b, err := t.bucket(bucket)
	if err != nil {
		return err
	}
	k, v := append([]byte(nil), key...), append([]byte{}, value...)
	if err := b.Put(k, v); err != nil {
		return fmt.Errorf("journal: put %s: %w", bucket, err)
	}
	t.writes = append(t.writes, write{op: writePut, bucket: bucket, key: k, value: v})
	return nil
}

// Delete removes key from a client bucket. Deleting a missing key is recorded
// and is not an error.
func (t *Tx) Delete(bucket string, key []byte) error {
	if !t.writable {
		return errReadOnly
	}
	if err := checkClientBucket(bucket); err != nil {
		return err
	}
	b, err := t.bucket(bucket)
	if err != nil {
		return err
	}
	k := append([]byte(nil), key...)
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
// not allowed.
func (t *Tx) ForEach(bucket string, prefix []byte, fn func(k, v []byte) error) error {
	b := t.lookup(bucket)
	if b == nil {
		return nil
	}
	c := b.Cursor()
	for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
		if err := fn(append([]byte(nil), k...), append([]byte{}, v...)); err != nil {
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
