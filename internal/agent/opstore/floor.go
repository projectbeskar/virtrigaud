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

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// maxClockFloorBytes bounds the clock floor record.
const maxClockFloorBytes = 256

// LoadClockFloor returns the stored clock floor record and whether one
// exists. The record is opaque to the store: the clock floor package encodes
// and checks it. It is a read, served in every state in which the journal
// could be opened, so the agent reads the floor before continuity is
// established.
func (s *Store) LoadClockFloor(ctx context.Context) (rec []byte, found bool, err error) {
	err = s.j.View(ctx, func(tx *journal.Tx) error {
		b, err := tx.Get(bucketMeta, keyClockFloor)
		if err != nil {
			return err
		}
		rec, found = b, b != nil
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("load clock floor: %w", err)
	}
	return rec, found, nil
}

// UpdateClockFloor commits the clock floor record next returns, in one
// transaction. next receives the record stored now (nil when there is none)
// inside that transaction, so a check of the stored value (the floor never
// goes down) is atomic with the write. An error from next aborts the
// transaction and is returned wrapped. Like every commit it needs continuity;
// before that, and in a quarantined store, it fails and writes nothing.
func (s *Store) UpdateClockFloor(ctx context.Context, next func(stored []byte) ([]byte, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.j.Update(ctx, func(tx *journal.Tx) error {
		stored, err := tx.Get(bucketMeta, keyClockFloor)
		if err != nil {
			return err
		}
		rec, err := next(stored)
		if err != nil {
			return err
		}
		if len(rec) == 0 || len(rec) > maxClockFloorBytes {
			return fmt.Errorf("clock floor record of %d bytes (1 to %d)", len(rec), maxClockFloorBytes)
		}
		return tx.Put(bucketMeta, keyClockFloor, rec)
	}); err != nil {
		return fmt.Errorf("update clock floor: %w", err)
	}
	return nil
}
