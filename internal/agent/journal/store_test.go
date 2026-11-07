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
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
)

const testBucket = "data"

// env is one store directory with its in-memory file system, reopened across
// simulated crashes.
type env struct {
	t     *testing.T
	dir   string
	mem   *journaltest.MemFS
	opts  journal.Options
	crash *journaltest.Crasher
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, dir: journaltest.FastTempDir(t), mem: journaltest.NewMemFS()}
	e.opts = journal.Options{Buckets: []string{testBucket}, OpenTimeout: 200 * time.Millisecond}
	return e
}

func (e *env) open() *journal.Store {
	e.t.Helper()
	o := e.opts
	o.Dir, o.FS = e.dir, e.mem
	if e.crash != nil {
		o.Hooks.Crash = e.crash.At
	}
	s, err := journal.Open(context.Background(), o)
	if err != nil {
		e.t.Fatalf("open: %v", err)
	}
	e.t.Cleanup(func() { _ = s.Close() })
	return s
}

// ready opens the store and establishes continuity; the store must be
// consistent.
func (e *env) ready() *journal.Store {
	e.t.Helper()
	s := e.open()
	if st, r := s.State(); st != journal.StateUnattested {
		e.t.Fatalf("state after open = %v (%s), want unattested", st, r)
	}
	if err := s.EstablishContinuity(context.Background()); err != nil {
		e.t.Fatalf("establish continuity: %v", err)
	}
	return s
}

// powerCut closes the store without writing and drops unsynchronised data.
func (e *env) powerCut(s *journal.Store) {
	e.t.Helper()
	_ = s.Close()
	e.mem.Crash()
}

func put(t *testing.T, s *journal.Store, k, v string) journal.Position {
	t.Helper()
	pos, err := s.Update(context.Background(), func(tx *journal.Tx) error {
		return tx.Put(testBucket, []byte(k), []byte(v))
	})
	if err != nil {
		t.Fatalf("update %s: %v", k, err)
	}
	return pos
}

func wantLost(t *testing.T, s *journal.Store, want journal.LostReason) {
	t.Helper()
	st, r := s.State()
	if st != journal.StateLost || r != want {
		t.Fatalf("state = %v{%s}, want lost{%s}", st, r, want)
	}
	if _, err := s.Update(context.Background(), func(tx *journal.Tx) error {
		return tx.Put(testBucket, []byte("x"), []byte("y"))
	}); !errors.Is(err, journal.ErrLost) {
		t.Fatalf("commit on a lost store: err = %v, want ErrLost", err)
	}
}

func anchorRecords(t *testing.T, e *env) []journal.AnchorRecord {
	t.Helper()
	raw, err := e.mem.ReadFile(filepath.Join(e.dir, journal.AnchorFile))
	if err != nil {
		t.Fatalf("read anchor: %v", err)
	}
	l, err := journal.ParseAnchorLog(raw)
	if err != nil {
		t.Fatalf("parse anchor: %v", err)
	}
	return l.Records
}

func TestOpen_FreshCommitReopen(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	var last journal.Position
	for i := 1; i <= 5; i++ {
		last = put(t, s, fmt.Sprint(i), "v")
		if last.Seq != uint64(i) {
			t.Fatalf("seq = %d, want %d (counters start at 1)", last.Seq, i)
		}
	}
	if err := s.VerifyChain(context.Background()); err != nil {
		t.Fatalf("verify chain: %v", err)
	}
	_ = s.Close()
	s2 := e.open()
	if st, _ := s2.State(); st != journal.StateUnattested {
		t.Fatalf("reopened state = %v, want unattested", st)
	}
	if got := s2.Position(); got != last {
		t.Fatalf("position after reopen = %+v, want %+v", got, last)
	}
	var v []byte
	_ = s2.View(context.Background(), func(tx *journal.Tx) error { v = tx.Get(testBucket, []byte("3")); return nil })
	if string(v) != "v" {
		t.Fatalf("value = %q", v)
	}
}

func TestUpdate_EmptyTransactionCommitsNothing(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	before := s.Position()
	pos, err := s.Update(context.Background(), func(*journal.Tx) error { return nil })
	if err != nil || pos != before {
		t.Fatalf("empty update: pos %+v err %v, want %+v", pos, err, before)
	}
}

// No commit can happen between Open and the end of the continuity checks.
func TestUpdate_RefusedBeforeContinuity(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	_ = s.Close()
	s2 := e.open()
	before := s2.Position()
	_, err := s2.Update(context.Background(), func(tx *journal.Tx) error {
		return tx.Put(testBucket, []byte("b"), []byte("2"))
	})
	if !errors.Is(err, journal.ErrUnattested) {
		t.Fatalf("commit before continuity: err = %v, want ErrUnattested", err)
	}
	if s2.Position() != before {
		t.Fatal("a commit happened before continuity was established")
	}
}

// A commit returns only after its anchor record is in the anchor log.
func TestCommit_AnchorAppendedBeforeReturn(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	for i := 0; i < 3; i++ {
		pos := put(t, s, fmt.Sprint(i), "v")
		recs := anchorRecords(t, e)
		if got := recs[len(recs)-1]; got.Seq != pos.Seq || got.Head != pos.Head {
			t.Fatalf("after commit %d the anchor's last record is seq %d, want %d", i, got.Seq, pos.Seq)
		}
	}
}

func TestCommit_AnchorFsyncedPerCommit(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	isAnchor := journaltest.HasSuffix(journal.AnchorFile)
	before := e.mem.Syncs(isAnchor)
	for i := 0; i < 4; i++ {
		put(t, s, fmt.Sprint(i), "v")
	}
	if got := e.mem.Syncs(isAnchor) - before; got != 4 {
		t.Fatalf("anchor fsyncs for 4 commits = %d, want 4", got)
	}
}

// corruptNewestMeta damages the checksum of the newest bbolt meta page, so
// that bbolt silently falls back to the previous commit.
func corruptNewestMeta(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pageSize := int(binary.LittleEndian.Uint32(b[24:28]))
	tx0 := binary.LittleEndian.Uint64(b[64:72])
	tx1 := binary.LittleEndian.Uint64(b[pageSize+64 : pageSize+72])
	off := 72
	if tx1 > tx0 {
		off += pageSize
	}
	b[off] ^= 0xff
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Rollback detection: every row of the design table.
func TestRollback(t *testing.T) {
	ctx := context.Background()

	t.Run("meta page fallback after ack is rolled_back", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		put(t, s, "a", "1")
		put(t, s, "b", "2")
		e.powerCut(s) // acknowledged commits survive only through fsync
		corruptNewestMeta(t, filepath.Join(e.dir, journal.StoreFile))
		wantLost(t, e.open(), journal.LostRolledBack)
	})

	t.Run("store file restored without the anchor is rolled_back", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		put(t, s, "a", "1")
		_ = s.Close()
		saved, err := os.ReadFile(filepath.Join(e.dir, journal.StoreFile))
		if err != nil {
			t.Fatal(err)
		}
		s = e.ready()
		put(t, s, "b", "2")
		put(t, s, "c", "3")
		_ = s.Close()
		if err := os.WriteFile(filepath.Join(e.dir, journal.StoreFile), saved, 0o600); err != nil {
			t.Fatal(err)
		}
		wantLost(t, e.open(), journal.LostRolledBack)
	})

	t.Run("anchor deleted is anchor_missing", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		put(t, s, "a", "1")
		_ = s.Close()
		if err := e.mem.Remove(filepath.Join(e.dir, journal.AnchorFile)); err != nil {
			t.Fatal(err)
		}
		wantLost(t, e.open(), journal.LostAnchorMissing)
	})

	t.Run("zero-filled last anchor record is a torn tail and accepted", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		pos := put(t, s, "a", "1")
		_ = s.Close()
		p := filepath.Join(e.dir, journal.AnchorFile)
		raw, _ := e.mem.ReadFile(p)
		e.mem.Put(p, append(raw, make([]byte, journal.AnchorRecordSize)...))
		s2 := e.open()
		if st, r := s2.State(); st != journal.StateUnattested {
			t.Fatalf("state = %v{%s}, want unattested", st, r)
		}
		if err := s2.EstablishContinuity(ctx); err != nil {
			t.Fatal(err)
		}
		next := put(t, s2, "b", "2")
		if next.Seq != pos.Seq+1 {
			t.Fatalf("seq = %d", next.Seq)
		}
		_ = s2.Close()
		// The torn tail was cut, so the log is valid end to end.
		if recs := anchorRecords(t, e); recs[len(recs)-1].Seq != next.Seq {
			t.Fatal("anchor log not repaired")
		}
		if st, _ := e.open().State(); st != journal.StateUnattested {
			t.Fatal("repaired store does not reopen")
		}
	})

	t.Run("partial last anchor record is a torn tail and accepted", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		put(t, s, "a", "1")
		_ = s.Close()
		p := filepath.Join(e.dir, journal.AnchorFile)
		raw, _ := e.mem.ReadFile(p)
		e.mem.Put(p, append(raw, 1, 2, 3))
		if st, r := e.open().State(); st != journal.StateUnattested {
			t.Fatalf("state = %v{%s}", st, r)
		}
	})

	t.Run("zero-filled anchor record before the end is anchor_corrupt", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		put(t, s, "a", "1")
		put(t, s, "b", "2")
		_ = s.Close()
		p := filepath.Join(e.dir, journal.AnchorFile)
		raw, _ := e.mem.ReadFile(p)
		copy(raw[journal.AnchorRecordSize:2*journal.AnchorRecordSize], make([]byte, journal.AnchorRecordSize))
		e.mem.Put(p, raw)
		wantLost(t, e.open(), journal.LostAnchorCorrupt)
	})

	t.Run("store more than one ahead of the anchor is inconsistent_store", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		put(t, s, "a", "1")
		put(t, s, "b", "2")
		put(t, s, "c", "3")
		_ = s.Close()
		p := filepath.Join(e.dir, journal.AnchorFile)
		raw, _ := e.mem.ReadFile(p)
		e.mem.Put(p, raw[:len(raw)-2*journal.AnchorRecordSize])
		wantLost(t, e.open(), journal.LostInconsistentStore)
	})

	t.Run("same seq with another head is inconsistent_store", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		put(t, s, "a", "1")
		_ = s.Close()
		p := filepath.Join(e.dir, journal.AnchorFile)
		raw, _ := e.mem.ReadFile(p)
		last, _ := journal.DecodeAnchorRecord(raw[len(raw)-journal.AnchorRecordSize:])
		last.Head[0] ^= 1
		copy(raw[len(raw)-journal.AnchorRecordSize:], last.Encode())
		e.mem.Put(p, raw)
		wantLost(t, e.open(), journal.LostInconsistentStore)
	})

	t.Run("anchor of another store is inconsistent_store", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		put(t, s, "a", "1")
		_ = s.Close()
		other := newEnv(t)
		o := other.ready()
		put(t, o, "a", "1")
		_ = o.Close()
		raw, _ := other.mem.ReadFile(filepath.Join(other.dir, journal.AnchorFile))
		e.mem.Put(filepath.Join(e.dir, journal.AnchorFile), raw)
		wantLost(t, e.open(), journal.LostInconsistentStore)
	})

	t.Run("store deleted is journal_missing", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		put(t, s, "a", "1")
		_ = s.Close()
		if err := os.Remove(filepath.Join(e.dir, journal.StoreFile)); err != nil {
			t.Fatal(err)
		}
		wantLost(t, e.open(), journal.LostJournalMissing)
	})

	t.Run("store and anchor deleted with the marker kept is journal_missing", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		put(t, s, "a", "1")
		_ = s.Close()
		_ = os.Remove(filepath.Join(e.dir, journal.StoreFile))
		_ = e.mem.Remove(filepath.Join(e.dir, journal.AnchorFile))
		wantLost(t, e.open(), journal.LostJournalMissing)
	})

	// The stated limit: a store and its anchor log restored together, to a
	// consistent earlier pair, cannot be detected by the host alone.
	t.Run("store and anchor restored together is undetectable locally", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		old := put(t, s, "a", "1")
		_ = s.Close()
		savedDB, _ := os.ReadFile(filepath.Join(e.dir, journal.StoreFile))
		savedFS := e.mem.Snapshot()
		s = e.ready()
		put(t, s, "b", "2")
		_ = s.Close()
		_ = os.WriteFile(filepath.Join(e.dir, journal.StoreFile), savedDB, 0o600)
		e.mem.Restore(savedFS)
		s2 := e.open()
		if st, _ := s2.State(); st != journal.StateUnattested {
			t.Fatalf("state = %v: a joint restore was detected; update the package documentation", st)
		}
		if s2.Position() != old {
			t.Fatal("unexpected position")
		}
	})
}

// A crash after the store commit and before the anchor append leaves the
// store one ahead; that commit was never acknowledged, so it is accepted and
// anchored at the next open.
func TestCrash_AfterStoreCommitBeforeAnchor(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	_ = s.Close()
	e.crash = &journaltest.Crasher{Point: journal.PointAfterStoreCommit, N: 1}
	s = e.ready()
	_, err := s.Update(context.Background(), func(tx *journal.Tx) error {
		return tx.Put(testBucket, []byte("b"), []byte("2"))
	})
	if !errors.Is(err, journal.ErrCrashed) {
		t.Fatalf("err = %v, want crash", err)
	}
	e.powerCut(s)
	e.crash = nil
	s2 := e.open()
	if st, r := s2.State(); st != journal.StateUnattested {
		t.Fatalf("state = %v{%s}", st, r)
	}
	if recs := anchorRecords(t, e); recs[len(recs)-1].Seq != s2.Position().Seq || s2.Position().Seq != 2 {
		t.Fatalf("unacknowledged commit not re-anchored: anchor %d store %d", recs[len(recs)-1].Seq, s2.Position().Seq)
	}
}

func TestCrash_AfterAnchorBeforeReturn(t *testing.T) {
	e := newEnv(t)
	e.crash = &journaltest.Crasher{Point: journal.PointAfterAnchor, N: 2}
	s := e.ready()
	put(t, s, "a", "1")
	if _, err := s.Update(context.Background(), func(tx *journal.Tx) error {
		return tx.Put(testBucket, []byte("b"), []byte("2"))
	}); !errors.Is(err, journal.ErrCrashed) {
		t.Fatalf("err = %v", err)
	}
	e.powerCut(s)
	e.crash = nil
	s2 := e.ready()
	if s2.Position().Seq != 2 {
		t.Fatalf("seq = %d, want 2", s2.Position().Seq)
	}
}

func TestAnchorRotation(t *testing.T) {
	e := newEnv(t)
	e.opts.AnchorRotateBytes = 4 * journal.AnchorRecordSize
	s := e.ready()
	var last journal.Position
	for i := 0; i < 20; i++ {
		last = put(t, s, fmt.Sprint(i), "v")
	}
	recs := anchorRecords(t, e)
	if len(recs) >= 20 || recs[len(recs)-1].Seq != last.Seq {
		t.Fatalf("anchor not rotated or stale: %d records, last %d", len(recs), recs[len(recs)-1].Seq)
	}
	_ = s.Close()
	if st, r := e.open().State(); st != journal.StateUnattested {
		t.Fatalf("after rotation: %v{%s}", st, r)
	}
}

func TestAnchorRotation_CrashMidRotation(t *testing.T) {
	e := newEnv(t)
	e.opts.AnchorRotateBytes = 4 * journal.AnchorRecordSize
	e.crash = &journaltest.Crasher{Point: journal.PointMidAnchorRotation, N: 1}
	s := e.ready()
	var err error
	for i := 0; i < 10 && err == nil; i++ {
		_, err = s.Update(context.Background(), func(tx *journal.Tx) error {
			return tx.Put(testBucket, []byte(fmt.Sprint(i)), []byte("v"))
		})
	}
	if !errors.Is(err, journal.ErrCrashed) {
		t.Fatalf("no crash mid rotation: %v", err)
	}
	crashedAt := s.Position()
	e.powerCut(s)
	e.crash = nil
	s2 := e.ready()
	if s2.Position() != crashedAt {
		t.Fatalf("position %+v, want %+v", s2.Position(), crashedAt)
	}
	put(t, s2, "after", "v")
	_ = s2.Close()
	if st, r := e.open().State(); st != journal.StateUnattested {
		t.Fatalf("%v{%s}", st, r)
	}
}

func TestCheckpoints_ChainVerifiesAcrossPruning(t *testing.T) {
	e := newEnv(t)
	e.opts.CheckpointEvery = 4
	s := e.ready()
	for i := 0; i < 23; i++ {
		put(t, s, fmt.Sprint(i), "v")
	}
	if err := s.VerifyChain(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if st, r := e.open().State(); st != journal.StateUnattested {
		t.Fatalf("%v{%s}", st, r)
	}
}

// Quarantine durability under an injected fsync error.
func TestQuarantine_AnchorFsyncErrorIsDurable(t *testing.T) {
	e := newEnv(t)
	var events []journal.Event
	e.opts.Events = func(ev journal.Event) { events = append(events, ev) }
	s := e.ready()
	put(t, s, "a", "1")
	e.mem.Fail(journaltest.OpSync, journaltest.HasSuffix(journal.AnchorFile), 0)
	_, err := s.Update(context.Background(), func(tx *journal.Tx) error {
		return tx.Put(testBucket, []byte("b"), []byte("2"))
	})
	var le *journal.LostError
	if !errors.As(err, &le) || le.Reason != journal.LostIOError {
		t.Fatalf("err = %v, want lost{io_error}", err)
	}
	wantLost(t, s, journal.LostIOError)
	if len(events) == 0 || events[0].Reason != journal.LostIOError {
		t.Fatalf("no quarantine event: %+v", events)
	}
	e.powerCut(s)
	s2 := e.open()
	wantLost(t, s2, journal.LostIOError)
	_ = s2.Close()

	// Each durable record is enough on its own.
	if err := e.mem.Remove(filepath.Join(e.dir, journal.QuarantineFile)); err != nil {
		t.Fatal(err)
	}
	wantLost(t, e.open(), journal.LostIOError)
}

func TestQuarantine_MarkerAloneIsEnough(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	s.Quarantine(journal.LostInconsistentStore, "test")
	e.powerCut(s)
	// Strip the anchor's quarantine record; the marker file still holds it.
	p := filepath.Join(e.dir, journal.AnchorFile)
	raw, _ := e.mem.ReadFile(p)
	e.mem.Put(p, raw[:len(raw)-journal.AnchorRecordSize])
	wantLost(t, e.open(), journal.LostInconsistentStore)
}

func TestQuarantine_StoreCommitErrorIsDurable(t *testing.T) {
	e := newEnv(t)
	fail := false
	e.opts.Hooks.StoreCommitError = func() error {
		if fail {
			return errors.New("EIO")
		}
		return nil
	}
	s := e.ready()
	put(t, s, "a", "1")
	fail = true
	if _, err := s.Update(context.Background(), func(tx *journal.Tx) error {
		return tx.Put(testBucket, []byte("b"), []byte("2"))
	}); !errors.Is(err, journal.ErrLost) {
		t.Fatalf("err = %v", err)
	}
	fail = false
	e.powerCut(s)
	wantLost(t, e.open(), journal.LostIOError)
}

func TestQuarantine_WitnessOfLostStoreRefusesContinuity(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	s.Quarantine(journal.LostRolledBack, "test")
	if err := s.EstablishContinuity(context.Background()); !errors.Is(err, journal.ErrLost) {
		t.Fatalf("continuity on a lost store: %v", err)
	}
}

func TestSchema_MinReaderTooNewRefusesToStart(t *testing.T) {
	e := newEnv(t)
	_ = e.ready().Close()
	e.opts.SchemaVersion = 3
	e.opts.Migrations = []journal.Migration{{ToVersion: 3, MinReader: 2}}
	s := e.open()
	if err := s.EstablishContinuity(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	o := journal.Options{Dir: e.dir, FS: e.mem, Buckets: []string{testBucket}}
	_, err := journal.Open(context.Background(), o)
	if !errors.Is(err, journal.ErrSchemaTooNew) {
		t.Fatalf("err = %v, want ErrSchemaTooNew", err)
	}
	if want := "agent schema >= 2"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not name the required version", err)
	}
}

// A fresh store is created at the binary's schema; a store made by an older
// binary is migrated, after continuity only, and keeps unknown meta fields.
func TestSchema_AdditiveMigrationAfterContinuityKeepsUnknownFields(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	_ = s.Close()
	// Add an unknown field to the store meta, as a newer writer would.
	addUnknownMetaField(t, e)

	e.opts.SchemaVersion = 2
	e.opts.Buckets = append(e.opts.Buckets, "added")
	applied := false
	e.opts.Migrations = []journal.Migration{{ToVersion: 2, Apply: func(tx *journal.Tx) error {
		applied = true
		return tx.Put("added", []byte("k"), []byte("v"))
	}}}
	s2 := e.open()
	if applied {
		t.Fatal("migration ran before continuity")
	}
	info, _ := s2.Schema(context.Background())
	if info.SchemaVersion != 1 {
		t.Fatalf("schema before continuity = %d", info.SchemaVersion)
	}
	if err := s2.EstablishContinuity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("migration did not run")
	}
	info, _ = s2.Schema(context.Background())
	if info.SchemaVersion != 2 {
		t.Fatalf("schema after = %d", info.SchemaVersion)
	}
	if !hasUnknownMetaField(t, e, s2) {
		t.Fatal("unknown meta field lost by the migration rewrite")
	}
}

// A rolled-back store is never migrated.
func TestSchema_NoMigrationOnRolledBackStore(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	_ = s.Close()
	saved, _ := os.ReadFile(filepath.Join(e.dir, journal.StoreFile))
	s = e.ready()
	put(t, s, "b", "2")
	_ = s.Close()
	_ = os.WriteFile(filepath.Join(e.dir, journal.StoreFile), saved, 0o600)
	e.opts.SchemaVersion = 2
	ran := false
	e.opts.Migrations = []journal.Migration{{ToVersion: 2, Apply: func(*journal.Tx) error { ran = true; return nil }}}
	s2 := e.open()
	wantLost(t, s2, journal.LostRolledBack)
	_ = s2.EstablishContinuity(context.Background())
	if ran {
		t.Fatal("migration ran on a rolled-back store")
	}
}

func TestOpen_SingleWriter(t *testing.T) {
	e := newEnv(t)
	_ = e.ready()
	o := e.opts
	o.Dir, o.FS = e.dir, e.mem
	o.OpenTimeout = 50 * time.Millisecond
	if _, err := journal.Open(context.Background(), o); !errors.Is(err, journal.ErrLocked) {
		t.Fatalf("second writer: err = %v, want ErrLocked", err)
	}
}

func TestOpen_InterruptedFirstOpenStartsAgain(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	_ = s.Close()
	_ = e.mem.Remove(filepath.Join(e.dir, journal.AnchorFile))
	_ = e.mem.Remove(filepath.Join(e.dir, journal.MarkerFile))
	if st, r := e.open().State(); st != journal.StateUnattested {
		t.Fatalf("%v{%s}", st, r)
	}
}

func TestOpen_TamperedChainIsJournalCorrupt(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	put(t, s, "b", "2")
	_ = s.Close()
	tamperChainEntry(t, e.dir, 1)
	wantLost(t, e.open(), journal.LostJournalCorrupt)
}

func TestOpen_GarbageStoreIsJournalCorruptNotPanic(t *testing.T) {
	for _, off := range []int{0, 4096, 8192, 12288} {
		e := newEnv(t)
		s := e.ready()
		for i := 0; i < 50; i++ {
			put(t, s, fmt.Sprint(i), "value-value-value")
		}
		_ = s.Close()
		p := filepath.Join(e.dir, journal.StoreFile)
		b, _ := os.ReadFile(p)
		for i := off; i < off+4096 && i < len(b); i++ {
			b[i] = byte(i*7 + 3)
		}
		_ = os.WriteFile(p, b, 0o600)
		o := e.opts
		o.Dir, o.FS = e.dir, e.mem
		st, err := journal.Open(context.Background(), o)
		if err != nil {
			continue // refusing to open is fine; panicking is not
		}
		if state, _ := st.State(); state == journal.StateLost {
			_ = st.Close()
			continue
		}
		// Undamaged enough to open: reading everything must not panic.
		_ = st.View(context.Background(), func(tx *journal.Tx) error {
			return tx.ForEach(testBucket, nil, func(_, _ []byte) error { return nil })
		})
		_ = st.Close()
	}
}

// unknownMetaFieldNumber is a field number journalpb.StoreMeta does not use.
const unknownMetaFieldNumber = 1000

func addUnknownMetaField(t *testing.T, e *env) {
	t.Helper()
	rewriteMeta(t, e.dir, func(m *journalpb.StoreMeta) {
		raw := protowire.AppendTag(nil, unknownMetaFieldNumber, protowire.BytesType)
		raw = protowire.AppendBytes(raw, []byte("from-a-newer-writer"))
		m.ProtoReflect().SetUnknown(raw)
	})
}

func hasUnknownMetaField(t *testing.T, e *env, s *journal.Store) bool {
	t.Helper()
	_ = s.Close()
	m := readMeta(t, e.dir)
	return len(m.ProtoReflect().GetUnknown()) > 0
}
