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
	"fmt"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// TestReadTxPosition: a read transaction reports the committed position, the
// newest checkpoint at or below a sequence number and the head at a retained
// sequence number, all from its own snapshot.
func TestReadTxPosition(t *testing.T) {
	e := newEnv(t)
	e.opts.CheckpointEvery = 4
	s := e.ready()
	var positions []journal.Position
	for i := range 10 {
		positions = append(positions, put(t, s, fmt.Sprintf("k%d", i), "v"))
	}
	want := s.Position()
	err := s.View(t.Context(), func(tx *journal.Tx) error {
		pos, ok := tx.CommittedPosition()
		if !ok || pos != want {
			t.Errorf("CommittedPosition = %+v, %v; want %+v", pos, ok, want)
		}
		cp, head, ok := tx.LastCheckpoint(pos.Seq)
		if !ok || cp == 0 || cp > pos.Seq || cp%4 != 0 {
			t.Errorf("LastCheckpoint(%d) = %d, %v", pos.Seq, cp, ok)
		}
		for _, p := range positions {
			if p.Seq == cp && p.Head != head {
				t.Errorf("checkpoint head at %d differs from the committed head", cp)
			}
		}
		last := positions[len(positions)-1]
		if h, ok := tx.HeadAt(last.Seq); !ok || h != last.Head {
			t.Errorf("HeadAt(%d) = %x, %v", last.Seq, h, ok)
		}
		if _, ok := tx.HeadAt(pos.Seq + 100); ok {
			t.Error("HeadAt beyond the position reports a head")
		}
		if _, ok := tx.HeadAt(0); ok {
			t.Error("HeadAt(0) reports a head")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
