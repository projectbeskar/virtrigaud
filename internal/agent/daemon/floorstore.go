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

package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// FloorPersistInterval is how far the clock floor advances before the agent
// commits it to the operation store again. One commit per interval is the
// ceiling on the floor's write rate (plus one at start and one at drain), and
// a crash can lose at most this interval and one observe period of the
// floor's advance, never a value that was committed.
const FloorPersistInterval = time.Minute

var (
	// errFloorUnreadable wraps a clock floor record that exists and cannot be
	// read.
	errFloorUnreadable = errors.New("clock floor unreadable")
	// errFloorWrite wraps a clock floor that cannot be committed.
	errFloorWrite = errors.New("clock floor not written")
	// errFloorLower wraps a commit of a floor below the stored one, which the
	// store refuses.
	errFloorLower = errors.New("clock floor below the stored floor")
)

// floorStore is a clockfloor.Store over the clock floor record of the
// operation store. The record is part of the chain-anchored journal: it is
// covered by the record trailers and the live-state digest, so a rollback of
// the floor is a rollback of the store, which the store's anchor checks
// detect. Load is a read and works before continuity; Save is a commit and
// works only after it.
type floorStore struct {
	st *opstore.Store
}

var _ clockfloor.Store = floorStore{}

// Load implements clockfloor.Store.
func (f floorStore) Load(ctx context.Context) (clockfloor.State, error) {
	rec, found, err := f.st.LoadClockFloor(ctx)
	if err != nil {
		return clockfloor.State{}, fmt.Errorf("%w: %w", errFloorUnreadable, err)
	}
	if !found {
		return clockfloor.State{}, clockfloor.ErrNotFound
	}
	var s clockfloor.State
	if err := s.UnmarshalBinary(rec); err != nil {
		return clockfloor.State{}, fmt.Errorf("%w: %w", errFloorUnreadable, err)
	}
	return s, nil
}

// Save implements clockfloor.Store. The commit's transaction reads the stored
// record and refuses a floor below it, so the stored floor never goes down,
// whatever the caller does.
func (f floorStore) Save(ctx context.Context, s clockfloor.State) error {
	rec, err := s.MarshalBinary()
	if err != nil {
		return fmt.Errorf("%w: %w", errFloorWrite, err)
	}
	err = f.st.UpdateClockFloor(ctx, func(stored []byte) ([]byte, error) {
		if stored != nil {
			var old clockfloor.State
			if err := old.UnmarshalBinary(stored); err != nil {
				return nil, fmt.Errorf("%w: stored record: %w", errFloorUnreadable, err)
			}
			if s.Floor.Before(old.Floor) && !guard.Off(guard.ClockFloorNeverLower) {
				return nil, fmt.Errorf("%w: %s < %s", errFloorLower, s.Floor.Format(time.RFC3339), old.Floor.Format(time.RFC3339))
			}
		}
		return rec, nil
	})
	if err != nil {
		if st, _ := f.st.Journal().State(); st == journal.StateLost {
			// The store was quarantined (before this call or by it): it cannot
			// commit, and the floor goes on in memory.
			return fmt.Errorf("%w: %w", clockfloor.ErrStoreLost, err)
		}
		return fmt.Errorf("%w: %w", errFloorWrite, err)
	}
	return nil
}
