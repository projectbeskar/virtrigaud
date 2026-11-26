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

package attest_test

import (
	"bytes"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/attest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	pAttest   = providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST
	pReattest = providerv1.ChallengePurpose_CHALLENGE_PURPOSE_REATTEST
)

type fakeClock struct{ now atomic.Int64 }

func (c *fakeClock) Now() time.Duration  { return time.Duration(c.now.Load()) }
func (c *fakeClock) add(d time.Duration) { c.now.Add(int64(d)) }
func nonce(b byte) []byte                { return bytes.Repeat([]byte{b}, attest.NonceLen) }
func newSet() (*attest.Challenges, *fakeClock) {
	c := &fakeClock{}
	c.now.Store(int64(time.Hour))
	return attest.New(c), c
}

func TestChallenges_SingleUse(t *testing.T) {
	s, _ := newSet()
	s.Issue(pAttest, nonce(1))
	if p, l := s.Find(nonce(1)); p != pAttest || l != attest.Outstanding {
		t.Fatalf("find: %v %v", p, l)
	}
	if !s.Consume(pAttest, nonce(1)) {
		t.Fatal("the first use was refused")
	}
	if s.Consume(pAttest, nonce(1)) {
		t.Fatal("a challenge was used twice")
	}
	if _, l := s.Find(nonce(1)); l != attest.Unknown {
		t.Fatalf("a consumed challenge is %v", l)
	}
}

func TestChallenges_PurposeBound(t *testing.T) {
	s, _ := newSet()
	s.Issue(pReattest, nonce(2))
	if s.Consume(pAttest, nonce(2)) {
		t.Fatal("a REATTEST challenge was consumed as ATTEST")
	}
	if p, l := s.Find(nonce(2)); p != pReattest || l != attest.Outstanding {
		t.Fatalf("find: %v %v", p, l)
	}
}

func TestChallenges_Expiry(t *testing.T) {
	s, c := newSet()
	s.Issue(pAttest, nonce(3))
	c.add(attest.Validity - time.Nanosecond)
	if _, l := s.Find(nonce(3)); l != attest.Outstanding {
		t.Fatalf("just before expiry: %v", l)
	}
	c.add(time.Nanosecond)
	if _, l := s.Find(nonce(3)); l != attest.Expired {
		t.Fatalf("at expiry: %v", l)
	}
	if s.Consume(pAttest, nonce(3)) {
		t.Fatal("an expired challenge was consumed")
	}
}

func TestChallenges_ClockBackwardsFailsClosed(t *testing.T) {
	s, c := newSet()
	s.Issue(pAttest, nonce(4))
	c.add(-time.Second)
	if s.Consume(pAttest, nonce(4)) {
		t.Fatal("a challenge was consumed on a clock below its issue time")
	}
}

func TestChallenges_BoundedPerPurpose(t *testing.T) {
	s, _ := newSet()
	for i := 0; i < attest.MaxOutstanding+2; i++ {
		s.Issue(pAttest, nonce(byte(10+i)))
	}
	s.Issue(pReattest, nonce(50))
	if got := s.Outstanding(pAttest); got != attest.MaxOutstanding {
		t.Fatalf("outstanding ATTEST = %d, want %d", got, attest.MaxOutstanding)
	}
	if got := s.Outstanding(pReattest); got != 1 {
		t.Fatalf("outstanding REATTEST = %d", got)
	}
	// The oldest were evicted, the newest kept.
	for i, want := range []attest.Lookup{attest.Unknown, attest.Unknown, attest.Outstanding} {
		if _, l := s.Find(nonce(byte(10 + i))); l != want {
			t.Fatalf("challenge %d: %v, want %v", i, l, want)
		}
	}
}

func TestRawClockAdvances(t *testing.T) {
	var c attest.RawClock
	a := c.Now()
	time.Sleep(time.Millisecond)
	if b := c.Now(); b <= a {
		t.Fatalf("raw clock did not advance: %v then %v", a, b)
	}
}
