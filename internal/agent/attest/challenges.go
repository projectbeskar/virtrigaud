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

// Package attest holds the per-host agent's outstanding attestation
// challenges: the single-use nonces the agent issued for a first
// attestation after a start (purpose ATTEST) or for a re-attestation of an
// attested agent (purpose REATTEST), which a manager-signed attestation grant
// must name.
//
// The set lives in memory only: a restart forgets every challenge, so a
// grant for a challenge of an earlier run is refused. At most MaxOutstanding
// challenges per purpose are held; issuing one more evicts the oldest of
// that purpose. A challenge is valid for Validity of CLOCK_MONOTONIC_RAW,
// which a wall-clock change does not move, and is consumed by the first
// attestation that names it.
//
// It imports no Kubernetes package.
package attest

import (
	"bytes"
	"crypto/subtle"
	"sync"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Limits of the challenge set.
const (
	// MaxOutstanding is the most challenges held per purpose. It equals the
	// most attestation sessions a manager keeps pending for a host, so that
	// a challenge a manager may still use is not evicted by the manager's own
	// requests.
	MaxOutstanding = 4
	// Validity is how long a challenge stays usable, on CLOCK_MONOTONIC_RAW.
	Validity = 300 * time.Second
	// NonceLen is the length of a challenge nonce.
	NonceLen = 16
)

// Clock reads a monotonic clock that no wall-clock change moves: the
// agent's CLOCK_MONOTONIC_RAW (RawClock).
type Clock interface {
	// Now returns the clock's reading.
	Now() time.Duration
}

// ClockFunc adapts a function to a Clock.
type ClockFunc func() time.Duration

// Now calls f.
func (f ClockFunc) Now() time.Duration { return f() }

// Lookup is what the set knows of a nonce.
type Lookup int

// Lookup results.
const (
	// Unknown: the set holds no challenge with the nonce (never issued by
	// this run, evicted, or consumed).
	Unknown Lookup = iota + 1
	// Expired: the challenge is held but its validity has passed.
	Expired
	// Outstanding: the challenge is held and valid.
	Outstanding
)

// entry is one outstanding challenge.
type entry struct {
	nonce  []byte
	issued time.Duration
}

// Challenges is the set of outstanding challenges. It is safe for concurrent
// use.
type Challenges struct {
	clock Clock
	mu    sync.Mutex
	set   map[providerv1.ChallengePurpose][]entry
}

// New returns an empty set on clock; a nil clock uses RawClock.
func New(clock Clock) *Challenges {
	if clock == nil {
		clock = RawClock{}
	}
	return &Challenges{clock: clock, set: map[providerv1.ChallengePurpose][]entry{}}
}

// Issue records nonce as an outstanding challenge of purpose, evicting the
// oldest challenge of that purpose beyond MaxOutstanding and every expired
// one.
func (c *Challenges) Issue(purpose providerv1.ChallengePurpose, nonce []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	kept := c.set[purpose][:0]
	for _, e := range c.set[purpose] {
		if !c.expired(e, now) {
			kept = append(kept, e)
		}
	}
	kept = append(kept, entry{nonce: bytes.Clone(nonce), issued: now})
	if len(kept) > MaxOutstanding {
		kept = kept[len(kept)-MaxOutstanding:]
	}
	c.set[purpose] = kept
}

// Find returns the purpose of the challenge with nonce and whether it is
// outstanding or expired; Unknown when the set holds none. It consumes
// nothing.
func (c *Challenges) Find(nonce []byte) (providerv1.ChallengePurpose, Lookup) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	for p, es := range c.set {
		for _, e := range es {
			if subtle.ConstantTimeCompare(e.nonce, nonce) == 1 {
				if c.expired(e, now) {
					return p, Expired
				}
				return p, Outstanding
			}
		}
	}
	return providerv1.ChallengePurpose_CHALLENGE_PURPOSE_UNSPECIFIED, Unknown
}

// Consume removes the outstanding challenge with nonce of purpose and
// reports whether it was there and valid. Only the first call for a nonce
// returns true: a challenge is single-use.
func (c *Challenges) Consume(purpose providerv1.ChallengePurpose, nonce []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	es := c.set[purpose]
	for i, e := range es {
		if subtle.ConstantTimeCompare(e.nonce, nonce) != 1 {
			continue
		}
		if !guard.Off(guard.ChallengeSingleUse) {
			c.set[purpose] = append(es[:i:i], es[i+1:]...)
		}
		return !c.expired(e, now)
	}
	return false
}

// Outstanding returns the number of valid challenges of purpose.
func (c *Challenges) Outstanding(purpose providerv1.ChallengePurpose) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	n := 0
	for _, e := range c.set[purpose] {
		if !c.expired(e, now) {
			n++
		}
	}
	return n
}

// expired reports whether e's validity has passed at now. A clock reading
// below the issue time (a clock that cannot go back, read wrong) counts as
// expired: the set fails closed.
func (c *Challenges) expired(e entry, now time.Duration) bool {
	if guard.Off(guard.ChallengeExpiry) {
		return false
	}
	return now < e.issued || now-e.issued >= Validity
}
