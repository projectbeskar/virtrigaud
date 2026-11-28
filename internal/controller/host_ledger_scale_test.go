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

package controller

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// At 10,000 hosts the heartbeat raises want far more than their share of
// the host ledger's write rate (one raise per host every 30 seconds is
// about 333 a second against a heartbeat bucket of 50 a second). A
// simulation over the real token buckets and the real per-host heartbeat
// throttle, in virtual time, shows that this never takes a token from
// receipt raises or from attestation: every receipt raise and every
// attestation within their own rates is allowed, every second, while the
// heartbeat share is used by the raises, and the ledger marks that follow
// them draw from their own share (tried again at every heartbeat until
// taken); and, because the per-host heartbeat interval stretches with the
// number of hosts, every host gets a heartbeat raise, and its mark, within
// a bounded time.
func TestLedgerBuckets_TenThousandHosts(t *testing.T) {
	const (
		hosts          = 10000
		simulated      = 15 * time.Minute
		receiptsPerSec = operationBucketRate - 10 // just within the operation share
		attestsPerSec  = attestBucketRate - 2     // agents restarting, within the attestation share
	)
	provider := ledgerTestProvider()
	start := time.Unix(1_000_000, 0)
	now := start
	l := &HostLedger{Now: func() time.Time { return now }, heartbeatAt: map[string]time.Time{}}
	var b ledgerBuckets
	rnd := rand.New(rand.NewPCG(1, 2))
	ids := make([]string, hosts)
	offset := make([]int, hosts)
	lastRaise := make([]time.Time, hosts)
	markPending := make([]bool, hosts)
	maxGap, maxMarkDelay := time.Duration(0), time.Duration(0)
	for i := range ids {
		ids[i] = fmt.Sprintf("host-%05d", i)
		offset[i] = rnd.IntN(int(hostHeartbeatInterval / time.Second)) // heartbeats are spread over the interval
		lastRaise[i] = start
	}
	var heartbeatAsked, heartbeatTaken, marksTaken, marksDeferred, receiptDenied, attestDenied int
	for sec := 0; sec < int(simulated/time.Second); sec++ {
		secStart := start.Add(time.Duration(sec) * time.Second)
		// Receipt raises and attestations arrive spread over the second,
		// interleaved with the heartbeats of this second.
		var hb []int
		for i := range ids {
			if (sec+offset[i])%int(hostHeartbeatInterval/time.Second) == 0 {
				hb = append(hb, i)
			}
		}
		events := len(hb) + receiptsPerSec + attestsPerSec
		order := rnd.Perm(events)
		for k, e := range order {
			at := secStart.Add(time.Duration(k) * time.Second / time.Duration(events))
			now = at
			switch {
			case e < len(hb):
				i := hb[e]
				if l.heartbeatDue(provider, ids[i]) {
					heartbeatAsked++
					if b.allow(trafficHeartbeat, at) {
						heartbeatTaken++
						l.mu.Lock()
						l.heartbeatAt[hostAnswerKey(provider, ids[i])] = at
						l.mu.Unlock()
						maxGap = max(maxGap, at.Sub(lastRaise[i]))
						lastRaise[i] = at
						markPending[i] = true
					}
				}
				// The agent's mark is behind the raised ledger: the driver's
				// mark takes a heartbeat token too (markBehind), at this
				// heartbeat or a later one.
				if markPending[i] {
					if b.allow(trafficHeartbeatMark, at) {
						marksTaken++
						markPending[i] = false
						maxMarkDelay = max(maxMarkDelay, at.Sub(lastRaise[i]))
					} else {
						marksDeferred++
					}
				}
			case e < len(hb)+receiptsPerSec:
				if sec > 0 && !b.allow(trafficOperation, at) {
					receiptDenied++
				}
			default:
				if sec > 0 && !b.allow(trafficAttest, at) {
					attestDenied++
				}
			}
		}
	}
	end := start.Add(simulated)
	for i := range ids {
		maxGap = max(maxGap, end.Sub(lastRaise[i]))
	}
	tokens := float64(heartbeatTaken) / simulated.Seconds()
	t.Logf("%d hosts, %v: heartbeat raises wanted %d, taken %d; marks taken %d, deferred %d; heartbeat raises %.1f/s; "+
		"receipt raises denied %d; attestations denied %d; longest time a host waited for a heartbeat raise %v, "+
		"for the mark after it %v", hosts, simulated, heartbeatAsked, heartbeatTaken, marksTaken, marksDeferred, tokens,
		receiptDenied, attestDenied, maxGap, maxMarkDelay)
	assert.Zero(t, receiptDenied, "heartbeat raises never starve receipt raises")
	assert.Zero(t, attestDenied, "heartbeat raises never starve attestation")
	assert.LessOrEqual(t, tokens, float64(heartbeatBucketRate)+1, "raises stay within the heartbeat share")
	assert.Greater(t, tokens, 0.8*heartbeatBucketRate, "raises use most of the heartbeat share")
	assert.LessOrEqual(t, float64(marksTaken)/simulated.Seconds(), float64(heartbeatMarkBucketRate)+1, "marks stay within their share")
	// The per-host interval stretches to hosts/rate (200 s here), so a host
	// waits at most that and a heartbeat or two for its raise (with a fixed
	// interval it was a random share of an overflowing bucket: some hosts
	// waited the whole run), and its mark follows within a heartbeat or two.
	assert.Less(t, maxGap, 6*time.Minute, "every host gets a heartbeat raise within six minutes")
	assert.Less(t, maxMarkDelay, 3*time.Minute, "and the mark after it within three")
}
