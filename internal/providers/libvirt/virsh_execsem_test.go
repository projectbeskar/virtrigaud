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
