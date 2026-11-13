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
	"context"
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
)

// SchemaInfo is the schema recorded in a store.
type SchemaInfo struct {
	// SchemaVersion is the schema the store was last migrated to.
	SchemaVersion uint32
	// MinReaderVersion is the lowest reader schema that may open it.
	MinReaderVersion uint32
}

// Schema returns the schema recorded in the store.
func (s *Store) Schema(ctx context.Context) (SchemaInfo, error) {
	var info SchemaInfo
	err := s.View(ctx, func(tx *Tx) error {
		m := &journalpb.StoreMeta{}
		if err := proto.Unmarshal(tx.btx.Bucket([]byte(bucketMeta)).Get(keyStoreMeta), m); err != nil {
			return fmt.Errorf("decode store meta: %w", err)
		}
		info = SchemaInfo{SchemaVersion: m.GetSchemaVersion(), MinReaderVersion: m.GetMinReaderVersion()}
		return nil
	})
	return info, err
}

// migrateLocked brings the store to opts.SchemaVersion and creates missing client
// buckets, in one chained transaction. It commits nothing when nothing is
// due. Fields of the store meta that this binary does not know are kept.
func (s *Store) migrateLocked() error {
	migrations := append([]Migration(nil), s.opts.Migrations...)
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].ToVersion < migrations[j].ToVersion })
	_, err := s.updateLocked(func(tx *Tx) error {
		for _, b := range s.opts.Buckets {
			if err := tx.createBucket(b); err != nil {
				return err
			}
		}
		mb := tx.btx.Bucket([]byte(bucketMeta))
		meta := &journalpb.StoreMeta{}
		if err := proto.Unmarshal(mb.Get(keyStoreMeta), meta); err != nil {
			return fmt.Errorf("decode store meta: %w", err)
		}
		changed := false
		from := meta.GetSchemaVersion()
		if tx.legacy {
			// The records of a store from before the trailers get theirs
			// first, so that a client migration reads and writes verified
			// records. The store then needs a reader that verifies them.
			if err := tx.migrateRecords(); err != nil {
				return err
			}
			if meta.GetSchemaVersion() < recordSchema {
				meta.SchemaVersion = recordSchema
			}
			if meta.GetMinReaderVersion() < recordSchema {
				meta.MinReaderVersion = recordSchema
			}
			meta.WriterVersion = meta.GetSchemaVersion()
			changed = true
		}
		if from < s.opts.SchemaVersion {
			for _, m := range migrations {
				if m.ToVersion <= from || m.ToVersion > s.opts.SchemaVersion {
					continue
				}
				if m.Apply != nil {
					if err := m.Apply(tx); err != nil {
						return fmt.Errorf("migration to schema %d: %w", m.ToVersion, err)
					}
				}
				if m.MinReader > meta.GetMinReaderVersion() {
					meta.MinReaderVersion = m.MinReader
				}
			}
			meta.SchemaVersion = s.opts.SchemaVersion
			meta.WriterVersion = s.opts.SchemaVersion
			changed = true
		}
		if !changed {
			return nil
		}
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(meta)
		if err != nil {
			return fmt.Errorf("encode store meta: %w", err)
		}
		if err := mb.Put(keyStoreMeta, b); err != nil {
			return fmt.Errorf("write store meta: %w", err)
		}
		tx.writes = append(tx.writes, write{op: writePut, bucket: bucketMeta, key: keyStoreMeta, value: b})
		return nil
	}, true)
	if err == nil {
		s.legacy = false
	}
	return err
}
