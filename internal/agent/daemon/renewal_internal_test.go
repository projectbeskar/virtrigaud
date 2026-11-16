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
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// TestActivationSeq: a snapshot taken while an activation runs is not idle;
// one taken before an activation began, or while one ran, never validates
// afterwards; one with no activation in between does.
func TestActivationSeq(t *testing.T) {
	var q activationSeq
	v, idle := q.snapshot()
	if !idle || !q.unchanged(v) {
		t.Fatal("an idle snapshot with nothing in between must validate")
	}
	q.begin()
	if _, idle := q.snapshot(); idle {
		t.Fatal("a snapshot during an activation is not idle")
	}
	if q.unchanged(v) {
		t.Fatal("a snapshot from before an activation began must not validate during it")
	}
	q.end()
	if q.unchanged(v) {
		t.Fatal("a snapshot from before an activation must not validate after it")
	}
	v2, idle := q.snapshot()
	if !idle || !q.unchanged(v2) {
		t.Fatal("a new idle snapshot validates")
	}
}

// TestHealBackoff: the first failed heals are retried at the next tick, then
// the wait doubles up to a minute; a success resets it.
func TestHealBackoff(t *testing.T) {
	now := time.Now()
	r := &renewer{d: &daemon{o: Options{Now: func() time.Time { return now }}, log: logr.Discard()}}
	fail := errors.New("signer away")
	for i := 1; i <= healImmediateRetries; i++ {
		r.healBackoff(fail)
		if r.healNextTry.Load() != 0 {
			t.Fatalf("failure %d is retried at once", i)
		}
	}
	var last time.Duration
	for i := 0; i < 12; i++ {
		r.healBackoff(fail)
		wait := time.Duration(r.healNextTry.Load() - now.UnixNano())
		if wait < last || wait > healRetryMax {
			t.Fatalf("wait %s after %s", wait, last)
		}
		last = wait
	}
	if last != healRetryMax {
		t.Fatalf("the wait is capped at %s, got %s", healRetryMax, last)
	}
	r.healBackoff(nil)
	if r.healNextTry.Load() != 0 || r.healFailures.Load() != 0 {
		t.Fatal("a success resets the backoff")
	}
}
