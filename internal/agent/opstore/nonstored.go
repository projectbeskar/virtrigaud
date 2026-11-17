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
	"sort"
	"strings"
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
	// maxSummaryActors bounds the distinct actors one summary counts; the
	// rest are counted together.
	maxSummaryActors = 64
	// maxSummaryActorLen bounds an actor's length in a summary.
	maxSummaryActorLen = 64
)

// EventNonStoredSuppressed is the event emitted for the non-stored events
// that were not emitted: at the first non-stored event of a later window,
// when the agent flushes a closed window (FlushNonStored), or at its drain.
// Reason gives their count, the counts by kind ("known", "refused:<reason>")
// and the actor named most often.
const EventNonStoredSuppressed = "events_suppressed"

// otherActors counts the actors beyond maxSummaryActors.
const otherActors = "(other actors)"

// nonStoredGate limits the non-stored audit events: at most nonStoredBurst
// per window, and a summary of the rest.
type nonStoredGate struct {
	mu         sync.Mutex
	now        func() time.Time
	start      time.Time
	count      int
	suppressed uint64
	byKind     map[string]uint64
	byActor    map[string]uint64
}

func (g *nonStoredGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// admit reports whether to emit the non-stored event ev now, and returns a
// summary event to emit first when earlier events were suppressed.
func (g *nonStoredGate) admit(ev AuditEvent) (bool, *AuditEvent) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	var summary *AuditEvent
	if g.start.IsZero() || now.Sub(g.start) >= nonStoredWindow {
		summary = g.summaryLocked()
		g.start, g.count = now, 0
	}
	if g.count >= nonStoredBurst {
		g.suppressLocked(ev)
		return false, summary
	}
	g.count++
	return true, summary
}

// flush returns the summary of the suppressed events once their window has
// closed, or at once with force; nil when there is none.
func (g *nonStoredGate) flush(force bool) *AuditEvent {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.suppressed == 0 || (!force && g.clock().Sub(g.start) < nonStoredWindow) {
		return nil
	}
	return g.summaryLocked()
}

// suppressLocked counts a suppressed event by kind and actor.
func (g *nonStoredGate) suppressLocked(ev AuditEvent) {
	if g.byKind == nil {
		g.byKind, g.byActor = map[string]uint64{}, map[string]uint64{}
	}
	g.suppressed++
	kind := ev.Event
	if ev.Reason != "" {
		kind += ":" + ev.Reason
	}
	g.byKind[kind]++
	actor := ev.Actor
	if len(actor) > maxSummaryActorLen {
		actor = actor[:maxSummaryActorLen]
	}
	if _, ok := g.byActor[actor]; !ok && len(g.byActor) >= maxSummaryActors {
		actor = otherActors
	}
	g.byActor[actor]++
}

// summaryLocked returns the summary of the suppressed events and resets
// their counts; nil when none was suppressed.
func (g *nonStoredGate) summaryLocked() *AuditEvent {
	if g.suppressed == 0 {
		return nil
	}
	kinds := make([]string, 0, len(g.byKind))
	for k, n := range g.byKind {
		kinds = append(kinds, fmt.Sprintf("%s=%d", k, n))
	}
	sort.Strings(kinds)
	top, topN := "", uint64(0)
	for a, n := range g.byActor {
		if n > topN || (n == topN && a < top) {
			top, topN = a, n
		}
	}
	ev := &AuditEvent{Event: EventNonStoredSuppressed, Actor: top, Reason: fmt.Sprintf("%d re-delivery (known) or refusal events "+
		"were not logged (%s; most from actor %q: %d): at most %d per %s are; stored events are never limited",
		g.suppressed, strings.Join(kinds, ", "), top, topN, nonStoredBurst, nonStoredWindow)}
	g.suppressed, g.byKind, g.byActor = 0, nil, nil
	return ev
}
