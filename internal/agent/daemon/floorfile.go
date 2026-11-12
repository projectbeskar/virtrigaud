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
	"io/fs"
	"os"
	"path/filepath"

	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// FloorFile is the clock floor's file in the state directory.
const FloorFile = "clock.floor"

// maxFloorBytes bounds the clock floor file.
const maxFloorBytes = 4096

var (
	// errFloorUnreadable wraps a clock floor that exists and cannot be read.
	errFloorUnreadable = errors.New("clock floor unreadable")
	// errFloorWrite wraps a clock floor that cannot be written durably.
	errFloorWrite = errors.New("clock floor not written")
)

// floorFile is a clockfloor.Store over one file in the state directory,
// replaced atomically and durably: a temporary file is written and fsynced,
// renamed over the floor, and the directory is fsynced.
//
// The floor is a file beside the operation store, not a record in it,
// because the agent reads and raises the floor before the store's continuity
// is established, when the store must not commit. A restore of the state
// directory restores the floor with it; the floor resists rollback as the
// state directory does.
type floorFile struct {
	fsys journal.FS
	dir  string
}

var _ clockfloor.Store = floorFile{}

func (f floorFile) path() string { return filepath.Join(f.dir, FloorFile) }

// Load implements clockfloor.Store.
func (f floorFile) Load(context.Context) (clockfloor.State, error) {
	size, err := f.fsys.Size(f.path())
	if errors.Is(err, fs.ErrNotExist) {
		return clockfloor.State{}, clockfloor.ErrNotFound
	}
	if err != nil {
		return clockfloor.State{}, fmt.Errorf("%w: %v", errFloorUnreadable, err)
	}
	if size > maxFloorBytes {
		return clockfloor.State{}, fmt.Errorf("%w: %w: %d bytes", errFloorUnreadable, clockfloor.ErrCorrupt, size)
	}
	b, err := f.fsys.ReadFile(f.path())
	if err != nil {
		return clockfloor.State{}, fmt.Errorf("%w: %v", errFloorUnreadable, err)
	}
	var s clockfloor.State
	if err := s.UnmarshalBinary(b); err != nil {
		return clockfloor.State{}, fmt.Errorf("%w: %w", errFloorUnreadable, err)
	}
	return s, nil
}

// Save implements clockfloor.Store.
func (f floorFile) Save(_ context.Context, s clockfloor.State) error {
	b, err := s.MarshalBinary()
	if err != nil {
		return fmt.Errorf("%w: %v", errFloorWrite, err)
	}
	tmp := f.path() + ".tmp"
	w, err := f.fsys.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("%w: %v", errFloorWrite, err)
	}
	if _, err := w.Write(b); err != nil {
		_ = w.Close()
		return fmt.Errorf("%w: %v", errFloorWrite, err)
	}
	if err := w.Sync(); err != nil {
		_ = w.Close()
		return fmt.Errorf("%w: %v", errFloorWrite, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("%w: %v", errFloorWrite, err)
	}
	if err := f.fsys.Rename(tmp, f.path()); err != nil {
		return fmt.Errorf("%w: %v", errFloorWrite, err)
	}
	if err := f.fsys.SyncDir(f.dir); err != nil {
		return fmt.Errorf("%w: %v", errFloorWrite, err)
	}
	return nil
}
