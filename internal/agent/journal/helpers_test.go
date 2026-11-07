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
package journal_test

import (
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
)

func withBolt(t *testing.T, dir string, fn func(tx *bolt.Tx) error) {
	t.Helper()
	db, err := bolt.Open(filepath.Join(dir, journal.StoreFile), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Update(fn); err != nil {
		t.Fatal(err)
	}
}

func readMeta(t *testing.T, dir string) *journalpb.StoreMeta {
	t.Helper()
	m := &journalpb.StoreMeta{}
	withBolt(t, dir, func(tx *bolt.Tx) error {
		return proto.Unmarshal(tx.Bucket([]byte(journal.BucketMetaForTest)).Get(journal.KeyStoreMetaForTest), m)
	})
	return m
}

func rewriteMeta(t *testing.T, dir string, f func(*journalpb.StoreMeta)) {
	t.Helper()
	withBolt(t, dir, func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(journal.BucketMetaForTest))
		m := &journalpb.StoreMeta{}
		if err := proto.Unmarshal(b.Get(journal.KeyStoreMetaForTest), m); err != nil {
			return err
		}
		f(m)
		v, err := proto.Marshal(m)
		if err != nil {
			return err
		}
		return b.Put(journal.KeyStoreMetaForTest, v)
	})
}

func tamperChainEntry(t *testing.T, dir string, seq uint64) {
	t.Helper()
	withBolt(t, dir, func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(journal.BucketChainForTest))
		k := make([]byte, 8)
		k[7] = byte(seq)
		v := append([]byte(nil), b.Get(k)...)
		v[0] ^= 1
		return b.Put(k, v)
	})
}
