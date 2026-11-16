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
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestFreezeConfirmationRelayed: a host's freeze confirmation from the
// inventory is relayed to its agent with AgentStatus before the next call,
// once per connection; a changed confirmation, or a new connection, relays
// it again; a host without one gets none; a malformed one makes the entry
// unroutable.
func TestFreezeConfirmationRelayed(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	cur, prev := strings.Repeat("a1", 16), strings.Repeat("b2", 16)
	withFreeze := func(ids ...string) hostsecret.Inventory {
		h := a.entry()
		h.FreezeConfirmation = &hostsecret.FreezeConfirmation{ForEnrolmentID: cur, EnrolmentIDs: ids}
		return hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: []hostsecret.Host{h}}
	}
	prov := newProvider(t, p, Options{}, a)
	client := providerv1.NewProviderClient(served(t, prov))
	describe := func() {
		t.Helper()
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		require.NoError(t, err)
	}
	describe()
	assert.Empty(t, a.statusRequests(), "no confirmation, no AgentStatus")

	require.NoError(t, prov.Reconcile(withFreeze(prev)))
	describe()
	describe()
	got := a.statusRequests()
	require.Len(t, got, 1, "relayed once per connection")
	assert.Equal(t, cur, hex.EncodeToString(got[0].GetConfirmForEnrolmentId()))
	require.Len(t, got[0].GetConfirmFrozenEnrolmentIds(), 1)
	assert.Equal(t, prev, hex.EncodeToString(got[0].GetConfirmFrozenEnrolmentIds()[0]))
	assert.Len(t, got[0].GetAnswerNonce(), nonceLen)
	assert.EqualValues(t, 1, a.lis.accepted.Load(), "a confirmation change keeps the connection")

	third := strings.Repeat("c3", 16)
	require.NoError(t, prov.Reconcile(withFreeze(prev, third)))
	describe()
	require.Len(t, a.statusRequests(), 2, "a changed confirmation is relayed again")

	a.lis.dropAll()
	require.Eventually(t, func() bool {
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		return err == nil
	}, testTimeout, 10*time.Millisecond)
	assert.GreaterOrEqual(t, len(a.statusRequests()), 3, "a new connection relays it again")

	// Malformed (the current enrolment named as a predecessor): dropped, the
	// host stays routed and nothing is relayed.
	before := len(a.statusRequests())
	require.NoError(t, prov.Reconcile(withFreeze(cur)))
	a.lis.dropAll()
	require.Eventually(t, func() bool {
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		return err == nil
	}, testTimeout, 10*time.Millisecond, "the host stays routed")
	assert.Len(t, a.statusRequests(), before, "an invalid confirmation is not relayed")
}

// TestFreezeConfirmationRelayBacksOff: a failed relay is tried again only
// after a backoff that grows, not with every call; the calls themselves go
// through.
func TestFreezeConfirmationRelayBacksOff(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	a.statusFails.Store(true)
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	h := a.entry()
	h.FreezeConfirmation = &hostsecret.FreezeConfirmation{ForEnrolmentID: strings.Repeat("a1", 16), EnrolmentIDs: []string{strings.Repeat("b2", 16)}}
	prov := newProvider(t, p, Options{Now: clock})
	require.NoError(t, prov.Reconcile(hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: []hostsecret.Host{h}}))
	client := providerv1.NewProviderClient(served(t, prov))
	describe := func() {
		t.Helper()
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		require.NoError(t, err)
	}
	describe()
	describe()
	assert.EqualValues(t, 1, a.statusCalls.Load(), "not again before the backoff")
	advance(freezeRetryMin + time.Second)
	describe()
	assert.EqualValues(t, 2, a.statusCalls.Load())
	advance(freezeRetryMin + time.Second)
	describe()
	assert.EqualValues(t, 2, a.statusCalls.Load(), "the backoff doubled")
	a.statusFails.Store(false)
	advance(2 * freezeRetryMin)
	describe()
	describe()
	assert.EqualValues(t, 3, a.statusCalls.Load(), "delivered, then not again on this connection")
}

// TestFreezeConfirmationToAnAgentThatServesNoStatus: an agent that refuses
// AgentStatus never confirms anything, and the calls to it still succeed and
// do not trip its breaker.
func TestFreezeConfirmationToAnAgentThatServesNoStatus(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	a.noStatus.Store(true)
	h := a.entry()
	h.FreezeConfirmation = &hostsecret.FreezeConfirmation{ForEnrolmentID: strings.Repeat("a1", 16), EnrolmentIDs: []string{strings.Repeat("b2", 16)}}
	prov := newProvider(t, p, Options{})
	require.NoError(t, prov.Reconcile(hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: []hostsecret.Host{h}}))
	client := providerv1.NewProviderClient(served(t, prov))
	for i := 0; i < 2*DefaultBreakerThreshold+1; i++ {
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		require.NoError(t, err, "call %d", i)
	}
	assert.Empty(t, a.statusRequests())
	assert.EqualValues(t, 1, a.statusCalls.Load(), "Unimplemented: not asked again on the same connection")
}

// TestFreezeConfirmationBoundsAgree: the inventory, the agent and the wire
// comment agree on the most ids one confirmation may carry.
func TestFreezeConfirmationBoundsAgree(t *testing.T) {
	assert.Equal(t, hostsecret.MaxFreezeConfirmations, opstore.MaxFreezeConfirmations)
	f := agentv1.File_agent_v1_agent_proto.Messages().ByName("AgentStatusRequest").Fields().ByName("confirm_frozen_enrolment_ids")
	require.NotNil(t, f)
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "proto", "agent", "v1", "agent.proto"))
	require.NoError(t, err)
	assert.Contains(t, string(src), fmt.Sprintf("At most %d ids.", opstore.MaxFreezeConfirmations))
}
