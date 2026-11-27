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
	"os"
	"path/filepath"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// recordAttestation runs RecordFirstAttestation in its own transaction and
// returns what it reported and the position after the transaction.
func recordAttestation(t *testing.T, s *journal.Store, ledgerSeq uint64, enrolment []byte) (bool, journal.Position) {
	t.Helper()
	var recorded bool
	pos, err := s.Update(context.Background(), func(tx *journal.Tx) error {
		var err error
		recorded, err = tx.RecordFirstAttestation(ledgerSeq, enrolment)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return recorded, pos
}

func storeAttestation(t *testing.T, s *journal.Store) journal.StoreAttestation {
	t.Helper()
	var a journal.StoreAttestation
	if err := s.View(context.Background(), func(tx *journal.Tx) error {
		var err error
		a, err = tx.StoreAttestation()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return a
}

// The first record commits one chain step naming its own sequence number;
// every later call writes nothing (and commits nothing), and the record
// survives a reopen unchanged.
func TestStoreAttestation_RecordedOnce(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	if a := storeAttestation(t, s); a.EverAttested || a.FirstSeq != 0 || a.FirstEnrolmentID != nil || a.RecordSeq != 0 {
		t.Fatalf("fresh store: %+v, want no record", a)
	}
	before := s.Position()
	enrol := bytes.Repeat([]byte{5}, 16)
	recorded, pos := recordAttestation(t, s, before.Seq, enrol)
	if !recorded || pos.Seq != before.Seq+1 {
		t.Fatalf("first record: recorded %v at %d, want true at %d", recorded, pos.Seq, before.Seq+1)
	}
	want := journal.StoreAttestation{EverAttested: true, FirstSeq: before.Seq, FirstEnrolmentID: enrol, RecordSeq: pos.Seq}
	if a := storeAttestation(t, s); a.EverAttested != want.EverAttested || a.FirstSeq != want.FirstSeq ||
		!bytes.Equal(a.FirstEnrolmentID, want.FirstEnrolmentID) || a.RecordSeq != want.RecordSeq {
		t.Fatalf("record %+v, want %+v", a, want)
	}
	again, pos2 := recordAttestation(t, s, pos.Seq, bytes.Repeat([]byte{6}, 16))
	if again || pos2 != pos {
		t.Fatalf("second record: recorded %v at %+v, want false and no commit (%+v)", again, pos2, pos)
	}
	if err := s.VerifyChain(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s2 := e.ready()
	if a := storeAttestation(t, s2); a.EverAttested != want.EverAttested || a.FirstSeq != want.FirstSeq ||
		!bytes.Equal(a.FirstEnrolmentID, want.FirstEnrolmentID) || a.RecordSeq != want.RecordSeq {
		t.Fatalf("after reopen: %+v, want %+v", a, want)
	}
	_ = s2.Close()
	if m := readMeta(t, e.dir); !m.GetEverAttested() || m.GetFirstAttestedSeq() != before.Seq || m.GetAttestedRecordSeq() != pos.Seq {
		t.Fatalf("store meta: %+v", m)
	}
}

// The record keeps the store's origin evidence, and a later activation
// keeps the record.
func TestStoreAttestation_KeepsOriginAndSurvivesActivation(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	first := bytes.Repeat([]byte{1}, 16)
	activation(t, s, first, true)
	recorded, pos := recordAttestation(t, s, 1, first)
	if !recorded {
		t.Fatal("not recorded")
	}
	activation(t, s, bytes.Repeat([]byte{2}, 16), false)
	if o, n := origin(t, s); !bytes.Equal(o, first) || n != 2 {
		t.Fatalf("origin %x count %d", o, n)
	}
	if a := storeAttestation(t, s); !a.EverAttested || a.FirstSeq != 1 || a.RecordSeq != pos.Seq || !bytes.Equal(a.FirstEnrolmentID, first) {
		t.Fatalf("record after an activation: %+v", a)
	}
}

// No enrolment active: the record carries none.
func TestStoreAttestation_NoEnrolment(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	if recorded, _ := recordAttestation(t, s, 1, nil); !recorded {
		t.Fatal("not recorded")
	}
	if a := storeAttestation(t, s); !a.EverAttested || a.FirstEnrolmentID != nil {
		t.Fatalf("record %+v", a)
	}
}

func TestStoreAttestation_Refusals(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	for name, f := range map[string]func(tx *journal.Tx) error{
		"ledger position 0": func(tx *journal.Tx) error { _, err := tx.RecordFirstAttestation(0, nil); return err },
		"short enrolment":   func(tx *journal.Tx) error { _, err := tx.RecordFirstAttestation(1, []byte{1}); return err },
	} {
		if _, err := s.Update(context.Background(), f); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if err := s.View(context.Background(), func(tx *journal.Tx) error {
		_, err := tx.RecordFirstAttestation(1, nil)
		return err
	}); err == nil {
		t.Fatal("a read transaction wrote the record")
	}
	if a := storeAttestation(t, s); a.EverAttested {
		t.Fatalf("a refused record was written: %+v", a)
	}
}

// Rolling the store file back below the record's commit is detected at
// open (the anchor log names the commit): the record cannot be erased by
// restoring an older store file alone.
func TestStoreAttestation_RollbackBelowRecordDetected(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	_ = s.Close()
	old, err := os.ReadFile(filepath.Join(e.dir, journal.StoreFile))
	if err != nil {
		t.Fatal(err)
	}
	s = e.ready()
	if recorded, _ := recordAttestation(t, s, 1, nil); !recorded {
		t.Fatal("not recorded")
	}
	_ = s.Close()
	if err := os.WriteFile(filepath.Join(e.dir, journal.StoreFile), old, 0o600); err != nil {
		t.Fatal(err)
	}
	wantLost(t, e.open(), journal.LostRolledBack)
}
