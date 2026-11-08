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
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
)

// diskState is every file of the state directory, on disk and in the
// in-memory file system, with its content.
func diskState(t *testing.T, e *env) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range entries {
		b, _ := os.ReadFile(filepath.Join(e.dir, d.Name()))
		out["disk:"+d.Name()] = string(b)
	}
	for p, b := range e.mem.Snapshot() {
		out["mem:"+p] = string(b)
	}
	return out
}

func sameState(t *testing.T, before, after map[string]string) {
	t.Helper()
	var keys []string
	for k := range before {
		keys = append(keys, k)
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if before[k] != after[k] {
			t.Errorf("%s changed", k)
		}
	}
}

// A second Open refused with ErrLocked changes nothing on disk, even with an
// anchor rotation in progress.
func TestOpen_LockedSecondOpenChangesNothing(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	e.mem.Put(filepath.Join(e.dir, "anchor.log.next"), []byte("rotation in progress"))
	before := diskState(t, e)
	o := e.opts
	o.Dir, o.FS = e.dir, e.mem
	if _, err := journal.Open(context.Background(), o); !errors.Is(err, journal.ErrLocked) {
		t.Fatalf("second open: %v", err)
	}
	sameState(t, before, diskState(t, e))
	put(t, s, "b", "2") // the first writer is unaffected
}

// First boot: a directory whose lock another process holds is left alone.
func TestOpen_LockedFirstBootChangesNothing(t *testing.T) {
	e := newEnv(t)
	lock, err := os.OpenFile(filepath.Join(e.dir, journal.LockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { // #nosec G115 -- descriptor
		t.Fatal(err)
	}
	before := diskState(t, e)
	o := e.opts
	o.Dir, o.FS = e.dir, e.mem
	if _, err := journal.Open(context.Background(), o); !errors.Is(err, journal.ErrLocked) {
		t.Fatalf("open: %v", err)
	}
	sameState(t, before, diskState(t, e))
	_ = lock.Close()
	if st, _ := e.open().State(); st != journal.StateUnattested {
		t.Fatal("first boot after the lock was released failed")
	}
}

func TestOpen_RefusesSharedDirectory(t *testing.T) {
	dir := filepath.Join(journaltest.FastTempDir(t), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	_, err := journal.Open(context.Background(), journal.Options{Dir: dir, FS: journaltest.NewMemFS()})
	if !errors.Is(err, journal.ErrInsecureDir) {
		t.Fatalf("err = %v", err)
	}
}

// Every quarantine record keeps the store quarantined on its own; each open
// that finds a loss writes the missing ones again.
func TestQuarantine_EachRecordAloneKeepsItAndIsPropagated(t *testing.T) {
	type source int
	const (
		file source = iota
		anchor
		store
	)
	for _, keep := range []source{file, anchor, store} {
		e := newEnv(t)
		s := e.ready()
		put(t, s, "a", "1")
		// An anchor fsync failure: the commit's anchor record and the
		// anchor's quarantine record never reach the disk.
		e.mem.Fail(journaltest.OpSync, journaltest.HasSuffix(journal.AnchorFile), 0)
		e.mem.Fail(journaltest.OpSync, journaltest.HasSuffix(journal.AnchorFile), 0)
		if _, err := s.Update(context.Background(), func(tx *journal.Tx) error {
			return tx.Put(testBucket, []byte("b"), []byte("2"))
		}); !errors.Is(err, journal.ErrLost) {
			t.Fatal(err)
		}
		e.powerCut(s)
		// The next open re-writes every record (the store record too).
		s2 := e.open()
		wantLost(t, s2, journal.LostIOError)
		_ = s2.Close()
		if !hasFileRecord(e) || !hasAnchorRecord(t, e) || !hasStoreRecord(t, e) {
			t.Fatalf("records after reopen: file %v anchor %v store %v", hasFileRecord(e), hasAnchorRecord(t, e), hasStoreRecord(t, e))
		}
		// Remove the two others; reopen twice.
		if keep != file {
			_ = e.mem.Remove(filepath.Join(e.dir, journal.QuarantineFile))
		}
		if keep != anchor {
			stripAnchorQuarantine(t, e)
		}
		if keep != store {
			deleteStoreRecord(t, e)
		}
		for i := 0; i < 2; i++ {
			s3 := e.open()
			wantLost(t, s3, journal.LostIOError)
			_ = s3.Close()
		}
		if !hasFileRecord(e) || !hasAnchorRecord(t, e) || !hasStoreRecord(t, e) {
			t.Fatalf("keeping %d: records not re-written", keep)
		}
	}
}

func hasFileRecord(e *env) bool {
	ok, _ := e.mem.Exists(filepath.Join(e.dir, journal.QuarantineFile))
	return ok
}

func hasAnchorRecord(t *testing.T, e *env) bool {
	t.Helper()
	raw, _ := e.mem.ReadFile(filepath.Join(e.dir, journal.AnchorFile))
	for off := 0; off+journal.AnchorRecordSize <= len(raw); off += journal.AnchorRecordSize {
		if r, err := journal.DecodeAnchorRecord(raw[off : off+journal.AnchorRecordSize]); err == nil && r.Type == journal.AnchorQuarantine {
			return true
		}
	}
	return false
}

func stripAnchorQuarantine(t *testing.T, e *env) {
	t.Helper()
	p := filepath.Join(e.dir, journal.AnchorFile)
	raw, _ := e.mem.ReadFile(p)
	var out []byte
	for off := 0; off+journal.AnchorRecordSize <= len(raw); off += journal.AnchorRecordSize {
		r, err := journal.DecodeAnchorRecord(raw[off : off+journal.AnchorRecordSize])
		if err == nil && r.Type == journal.AnchorQuarantine {
			continue
		}
		out = append(out, raw[off:off+journal.AnchorRecordSize]...)
	}
	e.mem.Put(p, out)
}

func hasStoreRecord(t *testing.T, e *env) bool {
	t.Helper()
	found := false
	withBolt(t, e.dir, func(tx *bolt.Tx) error {
		found = tx.Bucket([]byte(journal.BucketMetaForTest)).Get(journal.KeyQuarantineForTest) != nil
		return nil
	})
	return found
}

func deleteStoreRecord(t *testing.T, e *env) {
	t.Helper()
	withBolt(t, e.dir, func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(journal.BucketMetaForTest)).Delete(journal.KeyQuarantineForTest)
	})
}

// A store that has committed must find its marker.
func TestMarker_MissingAfterCommitIsALoss(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	_ = s.Close()
	_ = e.mem.Remove(filepath.Join(e.dir, journal.MarkerFile))
	wantLost(t, e.open(), journal.LostInconsistentStore)
}

func TestMarker_MissingOnEmptyStoreIsWrittenAgain(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	_ = s.Close()
	_ = e.mem.Remove(filepath.Join(e.dir, journal.MarkerFile))
	if st, r := e.open().State(); st != journal.StateUnattested {
		t.Fatalf("%v{%s}", st, r)
	}
	if ok, _ := e.mem.Exists(filepath.Join(e.dir, journal.MarkerFile)); !ok {
		t.Fatal("marker not written again")
	}
}

func TestMarker_OfAnotherStoreIsALoss(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	_ = s.Close()
	other := newEnv(t)
	_ = other.ready().Close()
	m, _ := other.mem.ReadFile(filepath.Join(other.dir, journal.MarkerFile))
	e.mem.Put(filepath.Join(e.dir, journal.MarkerFile), m)
	wantLost(t, e.open(), journal.LostInconsistentStore)
}

func TestMarker_BindingIsDurable(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	b := journal.MarkerBinding{HostID: "host-a", EnrolmentID: bytes.Repeat([]byte{1}, 16)}
	if err := s.BindMarker(b); err != nil {
		t.Fatal(err)
	}
	e.powerCut(s)
	got := e.open().Marker()
	if got.HostID != b.HostID || !bytes.Equal(got.EnrolmentID, b.EnrolmentID) {
		t.Fatalf("marker after power cut: %+v", got)
	}
}

// A first boot that crashed after creating an empty anchor log starts again.
func TestOpen_EmptyAnchorOnFirstBootStartsAgain(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	_ = s.Close()
	e.mem.Put(filepath.Join(e.dir, journal.AnchorFile), nil)
	_ = e.mem.Remove(filepath.Join(e.dir, journal.MarkerFile))
	if st, r := e.open().State(); st != journal.StateUnattested {
		t.Fatalf("%v{%s}", st, r)
	}
}

func TestAnchor_CommitRecordsMustIncrease(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	put(t, s, "b", "2")
	_ = s.Close()
	p := filepath.Join(e.dir, journal.AnchorFile)
	raw, _ := e.mem.ReadFile(p)
	n := journal.AnchorRecordSize
	// Swap records 1 and 2: same records, seq out of order.
	r1 := append([]byte(nil), raw[n:2*n]...)
	copy(raw[n:2*n], raw[2*n:3*n])
	copy(raw[2*n:3*n], r1)
	e.mem.Put(p, raw)
	wantLost(t, e.open(), journal.LostAnchorCorrupt)
}

// A failed migration leaves the store unattested and commits nothing.
func TestSchema_FailedMigrationLeavesStoreUnattested(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	_ = s.Close()
	e.opts.SchemaVersion = 2
	e.opts.Migrations = []journal.Migration{{ToVersion: 2, Apply: func(*journal.Tx) error { return errors.New("bad migration") }}}
	s2 := e.open()
	before := s2.Position()
	if err := s2.EstablishContinuity(context.Background()); err == nil {
		t.Fatal("a failed migration established continuity")
	}
	if st, _ := s2.State(); st != journal.StateUnattested || s2.Position() != before {
		t.Fatalf("state %v, position moved %v", st, s2.Position() != before)
	}
}

func TestLiveSize_FallsWhenRecordsAreDeleted(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	val := bytes.Repeat([]byte{7}, 1024)
	for i := 0; i < 200; i++ {
		if _, err := s.Update(context.Background(), func(tx *journal.Tx) error {
			return tx.Put(testBucket, []byte{byte(i), byte(i >> 8)}, val)
		}); err != nil {
			t.Fatal(err)
		}
	}
	full := s.LiveSize()
	if _, err := s.Update(context.Background(), func(tx *journal.Tx) error {
		for i := 0; i < 200; i++ {
			if err := tx.Delete(testBucket, []byte{byte(i), byte(i >> 8)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	put(t, s, "x", "y") // releases the pending pages
	if got := s.LiveSize(); got >= full/2 || s.Size() < full {
		t.Fatalf("live size %d after deleting from %d (high-water %d)", got, full, s.Size())
	}
}

func TestPosition_BinaryRoundTrip(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	pos := put(t, s, "a", "1")
	b, _ := pos.MarshalBinary()
	if len(b) != journal.PositionSize {
		t.Fatalf("%d bytes", len(b))
	}
	var back journal.Position
	if err := back.UnmarshalBinary(b); err != nil || back != pos {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	if err := back.UnmarshalBinary(b[1:]); err == nil {
		t.Fatal("short position accepted")
	}
}
