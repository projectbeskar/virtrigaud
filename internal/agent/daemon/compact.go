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

package daemon

import (
	"context"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// Compaction of the operation store (development builds only: a release
// build runs no operation, so its store holds no operation record).
const (
	// DefaultCompactEvery is the period of the store compaction.
	DefaultCompactEvery = time.Hour
	// compactCheckEvery is how often the store size is compared with its
	// threshold between two scheduled compactions.
	compactCheckEvery = time.Minute
	// compactNearCapNum / compactNearCapDen is the share of the store size
	// threshold at which a compaction runs before its time.
	compactNearCapNum, compactNearCapDen = 3, 4
	// compactGrowthDen: near the threshold, after a compaction that dropped
	// nothing, the wait before the next early one doubles (from
	// compactCheckEvery up to the period) until the live size has grown by
	// a compactGrowthDen-th of the threshold since, which resets it.
	compactGrowthDen = 64
)

// nearBackoff is the wait between early compactions near the threshold.
type nearBackoff struct {
	wait     time.Duration
	baseLive int64
}

// after records a compaction's result: one that dropped nothing doubles the
// wait (bounded by every); one that dropped records resets it.
func (b *nearBackoff) after(dropped int, live int64, every time.Duration) {
	base := min(every, compactCheckEvery)
	switch {
	case dropped > 0:
		b.wait = base
	default:
		b.wait = min(2*max(b.wait, base), every)
	}
	b.baseLive = live
}

// grew resets the wait once the live size has grown by a compactGrowthDen-th
// of the threshold since the last compaction: new records may be droppable.
func (b *nearBackoff) grew(live, maxBytes int64, every time.Duration) {
	if maxBytes > 0 && live-b.baseLive >= maxBytes/compactGrowthDen {
		b.wait, b.baseLive = min(every, compactCheckEvery), live
	}
}

// compactEvery is the period of the store compaction.
func (d *daemon) compactEvery() time.Duration {
	if d.o.CompactEvery > 0 {
		return d.o.CompactEvery
	}
	return DefaultCompactEvery
}

// nearCap reports whether a store of live bytes is at or above the share of
// its threshold max at which compaction runs early.
func nearCap(live, maxBytes int64) bool {
	return maxBytes > 0 && live >= maxBytes/compactNearCapDen*compactNearCapNum
}

// compactDue reports whether a compaction is due at now: never ran, its
// period has passed since the last one, or the store is near its threshold
// and the last compaction (which did not take it below) is at least
// nearWait old.
func compactDue(now, last time.Time, every, nearWait time.Duration, near bool) bool {
	switch {
	case last.IsZero():
		return true
	case now.Sub(last) >= every:
		return true
	}
	return near && now.Sub(last) >= max(nearWait, min(every, compactCheckEvery))
}

// compactLoop compacts the operation store (opstore.Store.Compact: the step
// journal of finished operations goes; their op_id tombstones, VM tombstones
// and witness files are kept, so a re-delivery is still answered from the
// tombstone and never run again) once the agent is ready, then every
// compaction period, and earlier when the store reaches three quarters of
// its size threshold: a check period after the last one, doubling after
// each early compaction that dropped nothing until the store has grown by a
// sixty-fourth of the threshold, so that a store holding nothing droppable
// is not scanned every minute. It runs only while the store is recovered, not
// quarantined and not in a re-enrolment freeze; one goroutine, so two
// compactions never overlap; it stops with the agent's context. A store
// near its threshold that compaction cannot bring below it (it holds only
// tombstones and the records of unresolved operations) is logged once per
// crossing: new operations are refused (definitely not performed) once the
// threshold is reached.
func (d *daemon) compactLoop(ctx context.Context, st *opstore.Store) {
	log := d.log.WithName("compaction")
	every := d.compactEvery()
	ticker := time.NewTicker(min(every, compactCheckEvery))
	defer ticker.Stop()
	var last time.Time
	var backoff nearBackoff
	reportedFull := false
	for {
		j := st.Journal()
		live := j.LiveSize()
		near := nearCap(live, st.MaxStoreBytes())
		backoff.grew(live, st.MaxStoreBytes(), every)
		js, _ := j.State()
		if js != journal.StateLost && st.Recovered() && !st.Freezing() && compactDue(time.Now(), last, every, backoff.wait, near) {
			before := j.LiveSize()
			n, err := st.Compact(ctx, opstore.DefaultCompactBatch)
			last = time.Now()
			after := j.LiveSize()
			backoff.after(n, after, every)
			switch {
			case ctx.Err() != nil:
				return
			case err != nil:
				log.Error(err, "store compaction failed; it is retried at the next period", "dropped", n)
			case n > 0:
				log.Info("store compacted", "dropped", n, "liveBytesBefore", before, "liveBytesAfter", after,
					"maxStoreBytes", st.MaxStoreBytes())
			default:
				log.V(1).Info("store compaction found nothing to drop", "liveBytes", after)
			}
			switch stillNear := nearCap(after, st.MaxStoreBytes()); {
			case stillNear && !reportedFull:
				reportedFull = true
				log.Error(nil, "the operation store stays near its size threshold after compaction: it holds tombstones "+
					"and the records of unresolved operations, which are never dropped; new operations are refused once it "+
					"reaches the threshold", "liveBytes", after, "maxStoreBytes", st.MaxStoreBytes())
			case !stillNear:
				reportedFull = false
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
