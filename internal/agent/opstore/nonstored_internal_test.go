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
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNonStoredGate_FloodThatStopsIsSummarised: the last window of a flood
// that stopped is summarised by a flush once the window has closed (and not
// before, unless forced), with the counts by kind and refusal reason and the
// actor named most often; concurrent admissions and flushes are safe.
func TestNonStoredGate_FloodThatStopsIsSummarised(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	g := &nonStoredGate{now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				ev := AuditEvent{Event: "refused", Reason: ReasonVMBusy, Actor: "flooder"}
				if j%5 == 0 {
					ev = AuditEvent{Event: "known", Actor: fmt.Sprintf("caller-%d", i)}
				}
				g.admit(ev)
				_ = g.flush(false) // the window is still open: nothing
			}
		}(i)
	}
	wg.Wait()
	if s := g.flush(false); s != nil {
		t.Fatalf("summary before the window closed: %+v", s)
	}
	mu.Lock()
	now = now.Add(2 * nonStoredWindow)
	mu.Unlock()
	s := g.flush(false)
	if s == nil || s.Event != EventNonStoredSuppressed {
		t.Fatalf("no summary after the flood stopped: %+v", s)
	}
	if want := fmt.Sprintf("%d re-delivery", 100-nonStoredBurst); !strings.HasPrefix(s.Reason, want) {
		t.Fatalf("summary %q, want it to start %q", s.Reason, want)
	}
	if !strings.Contains(s.Reason, "refused:"+ReasonVMBusy+"=") || !strings.Contains(s.Reason, "known=") || s.Actor != "flooder" {
		t.Fatalf("summary without the counts by kind or the top actor: %+v", s)
	}
	if g.flush(true) != nil {
		t.Fatal("a second summary of the same events")
	}
}

// TestNonStoredGate_ForcedFlushAndActorBound: a forced flush (the drain)
// summarises an open window, and the distinct actors counted are bounded.
func TestNonStoredGate_ForcedFlushAndActorBound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := &nonStoredGate{now: func() time.Time { return now }}
	for i := 0; i < nonStoredBurst+maxSummaryActors+10; i++ {
		g.admit(AuditEvent{Event: "known", Actor: fmt.Sprintf("actor-%d-%s", i, strings.Repeat("x", 100))})
	}
	if len(g.byActor) > maxSummaryActors+1 {
		t.Fatalf("%d actors counted, bound %d", len(g.byActor), maxSummaryActors+1)
	}
	for a := range g.byActor {
		if len(a) > maxSummaryActorLen {
			t.Fatalf("actor of %d bytes kept", len(a))
		}
	}
	if s := g.flush(true); s == nil || !strings.Contains(s.Reason, "known=") {
		t.Fatalf("forced flush: %+v", s)
	}
}
