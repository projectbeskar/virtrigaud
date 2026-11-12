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

package opstore_test

import (
	"bytes"
	"testing"
)

// TestHostView: the view names the active enrolment and the committed
// position of the same read, and reports recovery separately.
func TestHostView(t *testing.T) {
	h := newHarness(t)
	s := h.open()
	if s.Recovered() {
		t.Fatal("Recovered before recovery")
	}
	_ = s.Close()
	h.boot()
	if !h.s.Recovered() {
		t.Fatal("not Recovered after recovery")
	}
	v, err := h.s.HostView(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if v.Enrolment != nil || v.LastStartEpoch != 0 {
		t.Fatalf("unenrolled store reports an enrolment: %+v", v)
	}
	_ = h.s.Close()
	h.fresh()
	v, err = h.s.HostView(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	e := v.Enrolment
	if e == nil || !bytes.Equal(e.ID, h.enrol) || e.ProviderUID != h.provider || e.HostID != "host-1" || e.StartEpoch != h.start {
		t.Fatalf("enrolment = %+v", e)
	}
	if v.Position != h.s.Position() || v.Position.Seq == 0 {
		t.Fatalf("position = %+v, store = %+v", v.Position, h.s.Position())
	}
	if v.CheckpointSeq > v.Position.Seq || len(v.ContinuityHead) != 32 {
		t.Fatalf("checkpoint %d, continuity head %x", v.CheckpointSeq, v.ContinuityHead)
	}
	if v.LastStartEpoch != h.start {
		t.Fatalf("last start epoch = %d", v.LastStartEpoch)
	}
	if v, _ := h.s.HostView(t.Context(), v.Position.Seq+1); v.ContinuityHead != nil {
		t.Fatal("continuity head beyond the position")
	}
}
