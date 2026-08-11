/*
Copyright 2025.

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

package client

import (
	"context"
	"testing"
	"time"
)

// TestWithTimeout_NoTimeoutConfigured verifies that withTimeout returns the
// caller's context unmodified, plus a non-nil no-op cancel func, when the
// client has no Timeout configuration at all. Every RPC method
// unconditionally calls `defer cancel()`, so the returned func must always
// be safe to invoke even when it has nothing to release.
func TestWithTimeout_NoTimeoutConfigured(t *testing.T) {
	c := &Client{config: &Config{}}

	parent := context.Background()
	ctx, cancel := c.withTimeout(parent, "/provider.v1.Provider/Validate")
	if cancel == nil {
		t.Fatal("cancel func must never be nil")
	}
	if ctx != parent {
		t.Errorf("context should be returned unchanged when Timeout is nil, got %v want %v", ctx, parent)
	}

	// Must be safe to call, including more than once.
	cancel()
	cancel()
	if err := ctx.Err(); err != nil {
		t.Errorf("no-op cancel must not cancel the caller's context, got Err() = %v", err)
	}
}

// TestWithTimeout_CallTimeoutApplied is the core regression test for the
// context leak this file fixes: withTimeout previously discarded the cancel
// func returned by context.WithTimeout (`timeoutCtx, _ := context.WithTimeout(...)`),
// so go vet's lostcancel check flagged it and the only way the timer was
// ever released was the full CallTimeout elapsing. This verifies the
// default CallTimeout is applied, and that invoking the now-returned cancel
// releases the context immediately instead of waiting out the deadline.
func TestWithTimeout_CallTimeoutApplied(t *testing.T) {
	c := &Client{config: &Config{
		Timeout: &TimeoutConfig{
			CallTimeout: time.Minute,
		},
	}}

	ctx, cancel := c.withTimeout(context.Background(), "/provider.v1.Provider/Create")
	if cancel == nil {
		t.Fatal("cancel func must never be nil")
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected a deadline to be set")
	}
	if until := time.Until(deadline); until <= 0 || until > time.Minute {
		t.Errorf("deadline out of expected range: %v from now", until)
	}

	select {
	case <-ctx.Done():
		t.Fatal("context should not be done before cancel is called")
	default:
	}

	cancel()

	select {
	case <-ctx.Done():
		// expected: cancel released the context immediately.
	case <-time.After(time.Second):
		t.Fatal("context was not released promptly after calling cancel — the leak this test guards against")
	}
	if err := ctx.Err(); err != context.Canceled {
		t.Errorf("ctx.Err() = %v, want context.Canceled", err)
	}
}

// TestWithTimeout_PerMethodOverride verifies a method-specific timeout takes
// precedence over the default CallTimeout, and that its cancel func is also
// wired up correctly (not discarded).
func TestWithTimeout_PerMethodOverride(t *testing.T) {
	const method = "/provider.v1.Provider/SnapshotCreate"
	c := &Client{config: &Config{
		Timeout: &TimeoutConfig{
			CallTimeout: time.Hour,
			PerMethodTimeouts: map[string]time.Duration{
				method: 2 * time.Second,
			},
		},
	}}

	ctx, cancel := c.withTimeout(context.Background(), method)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected a deadline to be set")
	}
	if until := time.Until(deadline); until <= 0 || until > 2*time.Second {
		t.Errorf("deadline out of expected range: %v from now, want ~2s", until)
	}
}

// TestWithTimeout_ZeroCallTimeout verifies that a Timeout config with
// neither a per-method entry nor a positive CallTimeout leaves the context
// untouched, still returning a safe-to-call cancel func.
func TestWithTimeout_ZeroCallTimeout(t *testing.T) {
	c := &Client{config: &Config{
		Timeout: &TimeoutConfig{},
	}}

	parent := context.Background()
	ctx, cancel := c.withTimeout(parent, "/provider.v1.Provider/Delete")
	if cancel == nil {
		t.Fatal("cancel func must never be nil")
	}
	if ctx != parent {
		t.Errorf("context should be returned unchanged, got %v want %v", ctx, parent)
	}
	cancel()
	if err := ctx.Err(); err != nil {
		t.Errorf("no-op cancel must not cancel the caller's context, got Err() = %v", err)
	}
}

// TestWithTimeout_ParentCancellationPropagates verifies withTimeout still
// derives from the caller's context, so cancelling the parent (e.g. the
// controller's reconcile context) cancels the in-flight RPC's context too.
func TestWithTimeout_ParentCancellationPropagates(t *testing.T) {
	c := &Client{config: &Config{
		Timeout: &TimeoutConfig{CallTimeout: time.Minute},
	}}

	parent, parentCancel := context.WithCancel(context.Background())
	ctx, cancel := c.withTimeout(parent, "/provider.v1.Provider/Describe")
	defer cancel()

	parentCancel()

	select {
	case <-ctx.Done():
		// expected
	case <-time.After(time.Second):
		t.Fatal("derived context did not observe parent cancellation")
	}
	if err := ctx.Err(); err != context.Canceled {
		t.Errorf("ctx.Err() = %v, want context.Canceled", err)
	}
}
