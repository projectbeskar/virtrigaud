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
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
)

// Deleting the lock file while the store is held does not let a second Open
// in: it is refused and changes nothing on disk.
func TestOpen_LockFileDeletedWhileHeld(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	if err := os.Remove(filepath.Join(e.dir, journal.LockFile)); err != nil {
		t.Fatal(err)
	}
	e.mem.Put(filepath.Join(e.dir, "anchor.log.next"), []byte("rotation in progress"))
	before := diskState(t, e)
	o := e.opts
	o.Dir, o.FS = e.dir, e.mem
	if _, err := journal.Open(context.Background(), o); !errors.Is(err, journal.ErrLocked) {
		t.Fatalf("second open: %v", err)
	}
	sameState(t, before, diskState(t, e))
	put(t, s, "b", "2")
}

// After an I/O failure the store serves no reads through bbolt's in-memory
// state; a reopen reads what is on disk.
func TestQuarantine_IOErrorStopsReads(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	e.mem.Fail(journaltest.OpSync, journaltest.HasSuffix(journal.AnchorFile), 0)
	if _, err := s.Update(context.Background(), func(tx *journal.Tx) error {
		return tx.Put(testBucket, []byte("b"), []byte("2"))
	}); !errors.Is(err, journal.ErrLost) {
		t.Fatal(err)
	}
	err := s.View(context.Background(), func(tx *journal.Tx) error { return nil })
	var le *journal.LostError
	if !errors.As(err, &le) || le.Reason != journal.LostIOError {
		t.Fatalf("read after an I/O failure: %v", err)
	}
	e.powerCut(s)
	s2 := e.open()
	wantLost(t, s2, journal.LostIOError)
	if err := s2.View(context.Background(), func(tx *journal.Tx) error { return nil }); err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
}

// A missing anchor log is recorded without writing into the store: the store
// file is left byte for byte, and a new anchor log holds the quarantine.
func TestQuarantine_AnchorMissingLeavesStoreUntouched(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	_ = s.Close()
	_ = e.mem.Remove(filepath.Join(e.dir, journal.AnchorFile))
	db := filepath.Join(e.dir, journal.StoreFile)
	before, _ := os.ReadFile(db)
	s2 := e.open()
	wantLost(t, s2, journal.LostAnchorMissing)
	_ = s2.Close()
	after, _ := os.ReadFile(db)
	if string(before) != string(after) {
		t.Fatal("the store was written at open")
	}
	if !hasAnchorRecord(t, e) {
		t.Fatal("no quarantine record in a new anchor log")
	}
	_ = e.mem.Remove(filepath.Join(e.dir, journal.QuarantineFile))
	s3 := e.open()
	wantLost(t, s3, journal.LostAnchorMissing)
}

// A rotation that fails after its rename records the quarantine in the anchor
// log now at the path.
func TestAnchorRotation_FailureAfterRenameRecordsQuarantine(t *testing.T) {
	e := newEnv(t)
	e.opts.AnchorRotateBytes = 3 * journal.AnchorRecordSize
	s := e.ready()
	put(t, s, "a", "1")
	// The commit path calls SyncDir only in the rotation, after the rename.
	e.mem.Fail(journaltest.OpSyncDir, func(p string) bool { return p == e.dir }, 0)
	var err error
	for i := 0; i < 5 && err == nil; i++ {
		_, err = s.Update(context.Background(), func(tx *journal.Tx) error {
			return tx.Put(testBucket, []byte{byte(i)}, []byte("v"))
		})
	}
	if !errors.Is(err, journal.ErrLost) {
		t.Fatalf("rotation failure: %v", err)
	}
	_ = s.Close()
	if !hasAnchorRecord(t, e) {
		t.Fatal("quarantine not recorded in the anchor log at the path")
	}
	_ = e.mem.Remove(filepath.Join(e.dir, journal.QuarantineFile))
	wantLost(t, e.open(), journal.LostIOError)
}
