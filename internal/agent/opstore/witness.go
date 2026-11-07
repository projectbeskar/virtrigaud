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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
)

// Witness files live outside the store, one empty file per once step:
// <state dir>/witness/<enrolment hex>/<op hex>.<step index>. A witness is
// created with O_EXCL and made durable (file and directory fsync) before the
// step's effect, and is removed only when its enrolment's records are
// discarded after a confirmed re-enrolment freeze. Compaction never removes
// one.

// witnessPath is the path of the witness of step i of the operation.
func (s *Store) witnessPath(id *pb.OpIdentity, i int) string {
	return filepath.Join(s.witnessDir(id.GetEnrolmentId()), fmt.Sprintf("%x.%d", id.GetOpId(), i))
}

// witnessExists reports whether the witness of step i exists.
func (s *Store) witnessExists(id *pb.OpIdentity, i int) (bool, error) {
	return s.fsys.Exists(s.witnessPath(id, i))
}

// createWitness creates the witness of step i durably. created is false when
// it already existed. A write or fsync failure quarantines the store.
func (s *Store) createWitness(id *pb.OpIdentity, i int) (created bool, err error) {
	if guard.Off(guard.Witness) {
		return true, nil
	}
	dir := s.witnessDir(id.GetEnrolmentId())
	fail := func(what string, err error) (bool, error) {
		s.j.Quarantine(journal.LostIOError, "witness "+what+" failed")
		return false, fmt.Errorf("witness %s: %w", what, errors.Join(err, &journal.LostError{Reason: journal.LostIOError}))
	}
	s.mu.Lock()
	known := s.dirs[dir]
	s.mu.Unlock()
	if !known {
		if err := s.fsys.MkdirAll(dir, 0o700); err != nil {
			return fail("directory", err)
		}
		if err := s.fsys.SyncDir(filepath.Dir(dir)); err != nil {
			return fail("directory fsync", err)
		}
		if err := s.fsys.SyncDir(filepath.Dir(filepath.Dir(dir))); err != nil {
			return fail("directory fsync", err)
		}
		s.mu.Lock()
		s.dirs[dir] = true
		s.mu.Unlock()
	}
	f, err := s.fsys.OpenFile(s.witnessPath(id, i), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return fail("create", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fail("fsync", err)
	}
	if err := f.Close(); err != nil {
		return fail("close", err)
	}
	if err := s.fsys.SyncDir(dir); err != nil {
		return fail("directory fsync", err)
	}
	return true, nil
}
