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
	"sync"
	"time"
)

// Bound on the audit events the store emits without storing them (a
// re-delivery answered "known", an admission "refused"): a caller with a
// valid operation signature can repeat those as fast as it likes, and the
// agent's unit does not let the system journal rate-limit it (stored events
// must never be dropped). Stored events are never limited.
const (
	// nonStoredBurst is the number of non-stored events emitted per
	// nonStoredWindow.
	nonStoredBurst = 20
	// nonStoredWindow is the window of nonStoredBurst.
	nonStoredWindow = time.Second
)

// EventNonStoredSuppressed is the event emitted, at the first non-stored
// event of a new window, for the non-stored events of earlier windows that
// were not emitted (Reason gives their count).
const EventNonStoredSuppressed = "events_suppressed"

// nonStoredGate limits the non-stored audit events: at most nonStoredBurst
// per window, and a summary of the rest.
type nonStoredGate struct {
	mu         sync.Mutex
	now        func() time.Time
	start      time.Time
	count      int
	suppressed uint64
}

// admit reports whether to emit a non-stored event now, and returns a
// summary event to emit first when earlier events were suppressed.
func (g *nonStoredGate) admit() (bool, *AuditEvent) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if g.now != nil {
		now = g.now()
	}
	var summary *AuditEvent
	if g.start.IsZero() || now.Sub(g.start) >= nonStoredWindow {
		if g.suppressed > 0 {
			summary = &AuditEvent{Event: EventNonStoredSuppressed, Reason: fmt.Sprintf("%d re-delivery (known) or refusal "+
				"events were not logged: at most %d per %s are; stored events are never limited", g.suppressed, nonStoredBurst,
				nonStoredWindow)}
		}
		g.start, g.count, g.suppressed = now, 0, 0
	}
	if g.count >= nonStoredBurst {
		g.suppressed++
		return false, summary
	}
	g.count++
	return true, summary
}
