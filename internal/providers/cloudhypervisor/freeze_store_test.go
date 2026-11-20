//go:build virtrigaud_dev

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

package cloudhypervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor/none"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
)

// storeID returns a random 16-byte enrolment id.
func storeID(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 16)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

// TestFreezeConfirmationRedeliveredUntilTheStoreAcceptsIt runs the relay end
// to end against the agent's operation store: the fake agent answers
// AgentStatus with the store's own outcomes (opstore.Store.ConfirmFrozen,
// in the agent's wire form, server.FreezeResults). With MaxPendingDiscards
// discards pending, the store refuses the confirmation over capacity and
// the Provider keeps it pending; it is relayed again after the backoff,
// still refused, and the predecessor stays frozen. Once compaction has freed
// capacity, the next relay is accepted, once: the Provider then settles the
// confirmation and relays nothing more on that connection, and a relay on a
// new connection is already applied, changing nothing.
func TestFreezeConfirmationRedeliveredUntilTheStoreAcceptsIt(t *testing.T) {
	bg := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	require.NoError(t, os.Mkdir(dir, 0o700))
	st, err := opstore.Open(bg, opstore.Options{Journal: journal.Options{Dir: dir}, Executor: none.Executor{}, DiscardChunk: 2})
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.EstablishContinuity(bg))
	enrol, epoch := storeID(t), uint64(1)
	require.NoError(t, st.Enrol(bg, opstore.Enrolment{HostID: "host-a", ID: enrol, ProviderUID: "provider-uid", InstallID: storeID(t), StartEpoch: epoch}))
	_, err = st.Recover(bg)
	require.NoError(t, err)
	// rekey leaves the active enrolment with more records than one discard
	// chunk removes, re-enrols, and returns the predecessor.
	rekey := func() []byte {
		t.Helper()
		for i := 0; i < 3; i++ {
			_, err := st.RecordPanic(bg, opstore.OpRef{EnrolmentID: enrol, OpID: storeID(t)})
			require.NoError(t, err)
		}
		prev := enrol
		enrol, epoch = storeID(t), epoch+1
		require.NoError(t, st.Reenrol(bg, opstore.Enrolment{HostID: "host-a", ID: enrol, ProviderUID: "provider-uid", StartEpoch: epoch}))
		return prev
	}
	for i := 0; i < opstore.MaxPendingDiscards; i++ {
		require.NoError(t, st.ConfirmFreeze(bg, rekey()))
	}
	last := rekey()
	require.True(t, st.Discarding())

	env := newFreezeClockEnv(t, hex.EncodeToString(last))
	var outcomes []agentv1.FreezeConfirmationOutcome
	env.a.mu.Lock()
	env.a.confirmFunc = func(req *agentv1.AgentStatusRequest) []*agentv1.FreezeConfirmationResult {
		// The inventory names a fixed current enrolment; the store is asked
		// for its own active one, which the agent checks the request against.
		got, err := st.ConfirmFrozen(bg, enrol, req.GetConfirmFrozenEnrolmentIds())
		assert.NoError(t, err)
		res := server.FreezeResults(req.GetConfirmFrozenEnrolmentIds(), got)
		for _, r := range res {
			outcomes = append(outcomes, r.GetOutcome())
		}
		return res
	}
	env.a.mu.Unlock()
	seen := func() []agentv1.FreezeConfirmationOutcome {
		env.a.mu.Lock()
		defer env.a.mu.Unlock()
		return append([]agentv1.FreezeConfirmationOutcome(nil), outcomes...)
	}
	const (
		refused  = agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_REFUSED_OVER_CAPACITY
		accepted = agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_ACCEPTED
		already  = agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_ALREADY_APPLIED
	)
	env.describe()
	env.describe()
	require.Equal(t, []agentv1.FreezeConfirmationOutcome{refused}, seen(), "refused over capacity, relayed once")
	env.advance(freezeRetryMin + time.Second)
	env.describe()
	require.Equal(t, []agentv1.FreezeConfirmationOutcome{refused, refused}, seen(), "still pending: relayed again after the backoff")

	_, err = st.Compact(bg, opstore.DefaultCompactBatch)
	require.NoError(t, err)
	require.False(t, st.Discarding())
	env.advance(freezeRetryMax + time.Second)
	env.describe()
	env.describe()
	env.advance(freezeRetryMax + time.Second)
	env.describe()
	require.Equal(t, []agentv1.FreezeConfirmationOutcome{refused, refused, accepted}, seen(), "accepted once, then settled")

	env.a.lis.dropAll()
	require.Eventually(t, func() bool {
		return env.try() == nil && len(seen()) == 4
	}, testTimeout, 10*time.Millisecond, "a new connection relays it again")
	assert.Equal(t, already, seen()[3], "a redelivery is a no-op")
}
