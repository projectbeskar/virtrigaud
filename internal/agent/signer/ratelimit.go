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

package signer

import (
	"sync"
	"time"
)

// Default rate limit: sustained signatures per second and burst size.
//
// The signer is a digest oracle, so a compromised agent process can ask it to
// sign anything. A limit does not stop that; it bounds how fast, which keeps
// a stolen oracle from being used as a bulk signing service, makes abuse
// show up as a rate in the log and caps the signer's CPU. The agent needs one
// signature per TLS handshake and one per signed answer; a host serving a
// few Providers needs a handful per second, so 50 per second with a burst of
// 100 leaves a wide margin for reconnect storms and still refuses a flood.
//
// The budgets are separate for TLS handshakes, which any peer that reaches
// the agent's port can trigger (the agent throttles them before the handshake
// and the port must be firewalled, but the budget is still shared with
// unauthenticated peers), and for signed answers, which only an authenticated
// Provider can ask for. A flood on the first cannot starve the second.
const (
	// DefaultSignsPerSecond is the sustained rate of the answer budget.
	DefaultSignsPerSecond = 50.0
	// DefaultSignBurst is the bucket size of the answer budget.
	DefaultSignBurst = 100
	// DefaultHandshakeSignsPerSecond is the sustained rate of the handshake
	// budget.
	DefaultHandshakeSignsPerSecond = 20.0
	// DefaultHandshakeSignBurst is the bucket size of the handshake budget.
	DefaultHandshakeSignBurst = 40
)

// bucket is a token bucket: capacity burst, refilled at rate tokens per
// second, one token per signature.
type bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

// newBucket returns a full bucket.
func newBucket(rate float64, burst int, now time.Time) *bucket {
	return &bucket{rate: rate, burst: float64(burst), tokens: float64(burst), last: now}
}

// allow takes a token at now and reports whether there was one.
func (b *bucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = min(b.burst, b.tokens+el*b.rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
