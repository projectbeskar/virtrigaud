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
	"fmt"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// heldAt reports whether the store holds a head at seq.
func heldAt(t *testing.T, s *journal.Store, seq uint64) bool {
	t.Helper()
	var ok bool
	if err := s.View(context.Background(), func(tx *journal.Tx) error {
		_, ok = tx.HeadAt(seq)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return ok
}

func commitN(t *testing.T, s *journal.Store, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		put(t, s, fmt.Sprint(i), "v")
	}
}

// Without ledger retention a checkpoint prunes entries older than the
// previous checkpoint; with it, nothing is pruned until a floor is set, and
// then nothing at or above the floor.
func TestLedgerRetention_FloorKeepsEntries(t *testing.T) {
	plain := newEnv(t)
	plain.opts.CheckpointEvery = 4
	ps := plain.ready()
	commitN(t, ps, 0, 40)
	if heldAt(t, ps, 10) {
		t.Fatal("without ledger retention the entry at 10 should be pruned")
	}

	e := newEnv(t)
	e.opts.CheckpointEvery = 4
	e.opts.LedgerRetention = true
	s := e.ready()
	commitN(t, s, 0, 40)
	for _, seq := range []uint64{1, 10, 39} {
		if !heldAt(t, s, seq) {
			t.Fatalf("no floor set: the entry at %d was pruned", seq)
		}
	}
	s.SetRetentionFloor(10)
	commitN(t, s, 40, 20)
	if heldAt(t, s, 9) {
		t.Fatal("the entry at 9 (below the floor and older than the previous checkpoint) should be pruned")
	}
	for _, seq := range []uint64{10, 11, 30, 59} {
		if !heldAt(t, s, seq) {
			t.Fatalf("the entry at %d (at or above the floor 10) was pruned", seq)
		}
	}
	if err := s.VerifyChain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerRetention_FloorNeverGoesDown(t *testing.T) {
	e := newEnv(t)
	e.opts.LedgerRetention = true
	s := e.ready()
	s.SetRetentionFloor(20)
	s.SetRetentionFloor(5)
	if got := s.RetentionFloor(); got != 20 {
		t.Fatalf("floor = %d, want 20", got)
	}
}

func activation(t *testing.T, s *journal.Store, id []byte, first bool) {
	t.Helper()
	if _, err := s.Update(context.Background(), func(tx *journal.Tx) error {
		if err := tx.RecordEnrolmentActivation(id, first); err != nil {
			return err
		}
		return tx.Put(testBucket, id, []byte("enrolment"))
	}); err != nil {
		t.Fatal(err)
	}
}

func origin(t *testing.T, s *journal.Store) ([]byte, uint32) {
	t.Helper()
	var o []byte
	var n uint32
	if err := s.View(context.Background(), func(tx *journal.Tx) error {
		var err error
		o, n, err = tx.StoreOrigin()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return o, n
}

// The first activation of a never-enrolled store records its origin; every
// later activation counts; the origin never changes and survives a reopen.
func TestStoreOrigin_FirstActivationAndCount(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	if o, n := origin(t, s); o != nil || n != 0 {
		t.Fatalf("fresh store: origin %x count %d, want none", o, n)
	}
	first, second, third := bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 16), bytes.Repeat([]byte{3}, 16)
	activation(t, s, first, true)
	activation(t, s, second, false)
	// A caller that wrongly says "first" again does not move the origin.
	activation(t, s, third, true)
	o, n := origin(t, s)
	if !bytes.Equal(o, first) || n != 3 {
		t.Fatalf("origin %x count %d, want %x and 3", o, n, first)
	}
	if err := s.VerifyChain(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s2 := e.ready()
	if o, n := origin(t, s2); !bytes.Equal(o, first) || n != 3 {
		t.Fatalf("after reopen: origin %x count %d", o, n)
	}
	if m := readMetaOpen(t, e, s2); !bytes.Equal(m, first) {
		t.Fatalf("store meta origin %x", m)
	}
}

// readMetaOpen closes s and reads the origin straight from the store meta.
func readMetaOpen(t *testing.T, e *env, s *journal.Store) []byte {
	t.Helper()
	_ = s.Close()
	return readMeta(t, e.dir).GetOriginEnrolmentId()
}

// A store that was enrolled before origin evidence existed records none,
// whatever later activations happen: its origin is never inferred.
func TestStoreOrigin_LegacyStoreNeverInferred(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	activation(t, s, bytes.Repeat([]byte{7}, 16), false)
	activation(t, s, bytes.Repeat([]byte{8}, 16), false)
	if o, n := origin(t, s); o != nil || n != 0 {
		t.Fatalf("legacy store: origin %x count %d, want none", o, n)
	}
}

func TestStoreOrigin_Refusals(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	if _, err := s.Update(context.Background(), func(tx *journal.Tx) error {
		return tx.RecordEnrolmentActivation([]byte{1}, true)
	}); err == nil {
		t.Fatal("a short enrolment id was accepted")
	}
	if err := s.View(context.Background(), func(tx *journal.Tx) error {
		return tx.RecordEnrolmentActivation(bytes.Repeat([]byte{1}, 16), true)
	}); err == nil {
		t.Fatal("a read transaction wrote the origin")
	}
}

// An attestation's quarantine reasons are durable store loss reasons.
func TestLedgerReasons_Durable(t *testing.T) {
	for _, r := range journal.LedgerReasons {
		t.Run(string(r), func(t *testing.T) {
			if !journal.IsLedgerReason(r) {
				t.Fatalf("%s is not a ledger reason", r)
			}
			e := newEnv(t)
			s := e.ready()
			put(t, s, "a", "1")
			s.Quarantine(r, "test")
			wantLost(t, s, r)
			_ = s.Close()
			wantLost(t, e.open(), r)
		})
	}
	if journal.IsLedgerReason(journal.LostRolledBack) {
		t.Fatal("rolled_back is not a ledger reason")
	}
}
