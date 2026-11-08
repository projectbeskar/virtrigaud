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
	"errors"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
)

// A writer holding a stale copy of a record never overwrites a record that
// left the runnable states (here: recovery or a freeze made it
// OutcomeUnknown); the refusal does not quarantine the store.
func TestSaveOp_RefusesStaleCopy(t *testing.T) {
	ctx := context.Background()
	j, err := journal.Open(ctx, journal.Options{Dir: journaltest.FastTempDir(t), FS: journaltest.NewMemFS(), Buckets: buckets()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	if err := j.EstablishContinuity(ctx); err != nil {
		t.Fatal(err)
	}
	s := &Store{j: j, running: map[string]bool{}, dirs: map[string]bool{}}
	id := &pb.OpIdentity{EnrolmentId: make([]byte, 16), OpId: make([]byte, 16), VmUid: "vm"}
	key := opKey(id.GetEnrolmentId(), id.GetOpId())
	if _, err := j.Update(ctx, func(tx *journal.Tx) error {
		return put(tx, bucketOps, key, &pb.OpRecord{Identity: id, State: StateOutcomeUnknown, Reason: ReasonWitnessNoEffect})
	}); err != nil {
		t.Fatal(err)
	}
	stale := &pb.OpRecord{Identity: id, State: StateCompleted}
	if err := s.saveOp(ctx, stale, nil); !errors.Is(err, ErrConcurrentChange) {
		t.Fatalf("stale write: %v", err)
	}
	if st, _ := j.State(); st != journal.StateReady {
		t.Fatalf("a refused stale write quarantined the store: %v", st)
	}
	var got pb.OpRecord
	_ = j.View(ctx, func(tx *journal.Tx) error {
		_, err := get(tx, bucketOps, key, &got)
		return err
	})
	if got.GetState() != StateOutcomeUnknown {
		t.Fatalf("record overwritten: %v", got.GetState())
	}
}
