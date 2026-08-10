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

package libvirt

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The execSem semaphore bounds concurrent virsh/ssh subprocess forks so a burst
// of concurrent reconciles cannot exhaust the remote host's fork limit
// (post-adoption Validate storm, #288).
//
// streamSem is the separate budget for long-lived disk-stream forks (S3
// import/export SSH relays, scp disk copy) added to close the 7 call sites
// that bypassed execSem entirely — a live latent instance of the same #288
// class (ADR-0008 PR 1). It is intentionally a distinct chan struct{} from
// execSem so a multi-minute transfer can never starve, or be starved by,
// short control-plane virsh calls.

func TestNewVirshProviderHasBoundedExecSem(t *testing.T) {
	v := NewVirshProvider(nil)
	require.NotNil(t, v.execSem)
	assert.GreaterOrEqual(t, cap(v.execSem), 1)
}

func TestAcquireExecSlotNilSemUnbounded(t *testing.T) {
	// A zero-value provider (e.g. constructed directly in a test) has no
	// semaphore and must not block.
	v := &VirshProvider{}
	release, err := v.acquireExecSlot(context.Background())
	require.NoError(t, err)
	release()
}

func TestAcquireExecSlotBoundsConcurrency(t *testing.T) {
	v := &VirshProvider{execSem: make(chan struct{}, 1)}

	rel1, err := v.acquireExecSlot(context.Background())
	require.NoError(t, err)

	// A second acquire must block while the only slot is held.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		r, e := v.acquireExecSlot(ctx)
		if e == nil {
			r()
		}
		done <- e
	}()

	select {
	case <-done:
		t.Fatal("acquireExecSlot should block while the slot is held")
	case <-time.After(50 * time.Millisecond):
	}

	// Cancelling the waiter's context unblocks it with an error (no slot leaked).
	cancel()
	require.Error(t, <-done)

	// Releasing the held slot lets a fresh acquire succeed.
	rel1()
	r, err := v.acquireExecSlot(context.Background())
	require.NoError(t, err)
	r()
}

func TestNewVirshProviderHasBoundedStreamSem(t *testing.T) {
	v := NewVirshProvider(nil)
	require.NotNil(t, v.streamSem)
	assert.GreaterOrEqual(t, cap(v.streamSem), 1)
}

func TestAcquireStreamSlotNilSemUnbounded(t *testing.T) {
	// A zero-value provider (e.g. constructed directly in a test) has no
	// semaphore and must not block.
	v := &VirshProvider{}
	release, err := v.acquireStreamSlot(context.Background())
	require.NoError(t, err)
	release()
}

func TestAcquireStreamSlotBoundsConcurrency(t *testing.T) {
	v := &VirshProvider{streamSem: make(chan struct{}, 1)}

	rel1, err := v.acquireStreamSlot(context.Background())
	require.NoError(t, err)

	// A second acquire must block while the only slot is held.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		r, e := v.acquireStreamSlot(ctx)
		if e == nil {
			r()
		}
		done <- e
	}()

	select {
	case <-done:
		t.Fatal("acquireStreamSlot should block while the slot is held")
	case <-time.After(50 * time.Millisecond):
	}

	// Cancelling the waiter's context unblocks it with an error (no slot leaked).
	cancel()
	require.Error(t, <-done)

	// Releasing the held slot lets a fresh acquire succeed.
	rel1()
	r, err := v.acquireStreamSlot(context.Background())
	require.NoError(t, err)
	r()
}

// TestStreamSemIndependentOfExecSem proves the two budgets are genuinely
// separate chan struct{} instances, not one shared pool: saturating streamSem
// must not block an execSem acquisition, and saturating execSem must not
// block a streamSem acquisition. This is the property the whole fix depends
// on — sharing a single semaphore (routing all seven bypass sites through
// execSem) would reintroduce the starvation this split exists to prevent (a
// multi-minute disk stream holding a control-call slot for its duration).
func TestStreamSemIndependentOfExecSem(t *testing.T) {
	t.Run("saturated streamSem does not block execSem", func(t *testing.T) {
		v := &VirshProvider{
			execSem:   make(chan struct{}, 1),
			streamSem: make(chan struct{}, 1),
		}

		// Saturate the streaming budget (simulates a long-running S3
		// export/import or scp disk copy in flight) and leave it held.
		streamRelease, err := v.acquireStreamSlot(context.Background())
		require.NoError(t, err)
		defer streamRelease()

		// A control-call acquisition must succeed immediately — it does not
		// share a channel with streamSem.
		execRelease, err := v.acquireExecSlot(context.Background())
		require.NoError(t, err)
		execRelease()
	})

	t.Run("saturated execSem does not block streamSem", func(t *testing.T) {
		v := &VirshProvider{
			execSem:   make(chan struct{}, 1),
			streamSem: make(chan struct{}, 1),
		}

		// Saturate the control budget (simulates a burst of short virsh
		// inventory/control calls in flight) and leave it held.
		execRelease, err := v.acquireExecSlot(context.Background())
		require.NoError(t, err)
		defer execRelease()

		// A stream acquisition must succeed immediately — it does not share a
		// channel with execSem.
		streamRelease, err := v.acquireStreamSlot(context.Background())
		require.NoError(t, err)
		streamRelease()
	})
}
