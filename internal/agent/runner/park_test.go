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

package runner_test

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/runner"
)

// TestParkedRunWaitsAndIsNeverGivenUp: a run the ledger gate parks waits for
// a wake-up, counts no failure and is never given up, and asks again with
// no request bytes (the store holds them).
func TestParkedRunWaitsAndIsNeverGivenUp(t *testing.T) {
	s := &stubStore{parks: 3}
	opts := fast
	opts.ParkRecheck = time.Hour
	p := runner.New(context.Background(), s, logr.Discard(), opts)
	ref := opstore.OpRef{EnrolmentID: []byte("e"), OpID: []byte("o")}
	if err := p.Start(ref, []byte("req")); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		waitCalls(t, s, i)
		time.Sleep(5 * time.Millisecond) // far beyond GiveUpAfter
		if calls, resolved := s.snapshot(); calls != i || resolved != "" {
			t.Fatalf("parked run: calls %d resolved %q", calls, resolved)
		}
		s.wakeUp()
	}
	drain(t, p)
	calls, resolved := s.snapshot()
	if calls != 4 || resolved != "" {
		t.Fatalf("after the wake-ups: calls %d resolved %q", calls, resolved)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if string(s.payloads[0]) != "req" || s.payloads[1] != nil || s.payloads[3] != nil {
		t.Fatalf("payloads %q", s.payloads)
	}
}

// TestParkedRunRechecks: a parked run asks again after ParkRecheck without a
// wake-up.
func TestParkedRunRechecks(t *testing.T) {
	s := &stubStore{parks: 2}
	opts := fast
	opts.ParkRecheck = time.Millisecond
	p := runner.New(context.Background(), s, logr.Discard(), opts)
	if err := p.Start(opstore.OpRef{EnrolmentID: []byte("e"), OpID: []byte("o")}, nil); err != nil {
		t.Fatal(err)
	}
	waitCalls(t, s, 3)
	drain(t, p)
	if calls, resolved := s.snapshot(); calls != 3 || resolved != "" {
		t.Fatalf("calls %d resolved %q", calls, resolved)
	}
}

// TestParkedRunStopsOnDrain: a parked run ends when the pool's context ends.
func TestParkedRunStopsOnDrain(t *testing.T) {
	s := &stubStore{parks: 1 << 30}
	opts := fast
	opts.ParkRecheck = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	p := runner.New(ctx, s, logr.Discard(), opts)
	if err := p.Start(opstore.OpRef{EnrolmentID: []byte("e"), OpID: []byte("o")}, nil); err != nil {
		t.Fatal(err)
	}
	waitCalls(t, s, 1)
	cancel()
	drain(t, p)
	if calls, resolved := s.snapshot(); calls != 1 || resolved != "" {
		t.Fatalf("calls %d resolved %q", calls, resolved)
	}
}

// TestStartOfARunningOperationDeposits: a second start of an operation with
// a run adds no run and hands its request bytes to the store; once the run
// ended, a start runs it again.
func TestStartOfARunningOperationDeposits(t *testing.T) {
	s := &stubStore{parks: 1}
	opts := fast
	opts.ParkRecheck = time.Hour
	p := runner.New(context.Background(), s, logr.Discard(), opts)
	ref := opstore.OpRef{EnrolmentID: []byte("e"), OpID: []byte("o")}
	if err := p.Start(ref, nil); err != nil {
		t.Fatal(err)
	}
	waitCalls(t, s, 1)
	if err := p.Start(ref, []byte("again")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	s.mu.Lock()
	calls, deposits := s.calls, len(s.deposits)
	s.mu.Unlock()
	if calls != 1 || deposits != 1 {
		t.Fatalf("calls %d deposits %d, want one run and one deposit", calls, deposits)
	}
	s.wakeUp()
	waitCalls(t, s, 2)
	drain(t, p)
	p2 := runner.New(context.Background(), s, logr.Discard(), opts)
	if err := p2.Start(ref, nil); err != nil {
		t.Fatal(err)
	}
	waitCalls(t, s, 3)
	drain(t, p2)
}

// waitCalls waits until s saw at least n Execute calls.
func waitCalls(t *testing.T, s *stubStore, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if calls, _ := s.snapshot(); calls >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fewer than %d calls", n)
		}
		time.Sleep(time.Millisecond)
	}
}
