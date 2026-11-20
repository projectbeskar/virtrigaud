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
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// TestCapacity_MatchesAdmission: Capacity reports the live size admission
// compares and the threshold, and Full is true exactly when admission refuses
// a new operation for a full store: at the threshold it still accepts, one
// byte below the live size it refuses.
func TestCapacity_MatchesAdmission(t *testing.T) {
	h := newHarness(t)
	var limit int64 = 1 << 40
	h.opts = func(o *opstore.Options) { o.MaxStoreBytes = limit }
	h.fresh()
	for i := 0; i < 50; i++ {
		h.do(h.req(fmt.Sprintf("vm-%d", i), rid(), kCreate, 1))
	}
	c := h.s.Capacity()
	if c.LiveBytes != h.s.Journal().LiveSize() || c.LiveBytes <= 0 || c.MaxBytes != limit || c.Full() {
		t.Fatalf("Capacity = %+v (live %d, limit %d)", c, h.s.Journal().LiveSize(), limit)
	}

	limit = c.LiveBytes
	_ = h.s.Close()
	h.boot()
	c = h.s.Capacity()
	if c.MaxBytes != limit || c.Full() {
		t.Fatalf("at the threshold: Capacity = %+v, want not full", c)
	}
	d := h.admit(h.req("vm-at", rid(), kCreate, 1))
	if d.Refusal == opstore.RefusalStoreFull {
		t.Fatal("admission refused a store at its threshold, which Capacity reports not full")
	}

	limit = h.s.Capacity().LiveBytes - 1
	_ = h.s.Close()
	h.boot()
	if c = h.s.Capacity(); !c.Full() {
		t.Fatalf("above the threshold: Capacity = %+v, want full", c)
	}
	wantRefusal(t, h.admit(h.req("vm-above", rid(), kCreate, 1)), opstore.RefusalStoreFull)

	if (opstore.Capacity{LiveBytes: 10}).Full() {
		t.Error("an unmeasured capacity (MaxBytes 0) reports full")
	}
}

// TestCapacity_LostStoreNotMeasured: a store that lost its persistence
// guarantee reports no capacity (the zero Capacity), never a size, and a
// closed one reports no live size of 0 that would read as empty.
func TestCapacity_LostStoreNotMeasured(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	h.do(h.req("vm", rid(), kCreate, 1))
	h.mem.Put(filepath.Join(h.dir, "witness", fmt.Sprintf("%x", h.enrol), fmt.Sprintf("%x.1", rid())), nil)
	_ = h.s.Close()
	s := h.open()
	defer func() { _ = s.Close() }()
	if c := s.Capacity(); c.MaxBytes == 0 || c.LiveBytes <= 0 {
		t.Fatalf("an open store before recovery: Capacity = %+v", c)
	}
	if err := s.EstablishContinuity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recover(context.Background()); !errors.Is(err, journal.ErrLost) {
		t.Fatalf("recover: %v", err)
	}
	if c := s.Capacity(); c != (opstore.Capacity{}) {
		t.Fatalf("a lost store: Capacity = %+v, want not measured", c)
	}
	_ = s.Close()
	if c := s.Capacity(); c != (opstore.Capacity{}) {
		t.Fatalf("a closed store: Capacity = %+v, want not measured", c)
	}
}
