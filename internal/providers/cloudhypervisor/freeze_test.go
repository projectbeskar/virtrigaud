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

// freezeClockEnv is a Provider with a settable clock in front of one fake
// agent whose host carries a freeze confirmation of ids.
type freezeClockEnv struct {
	a        *fakeAgent
	prov     *Provider
	describe func()
	try      func() error
	advance  func(time.Duration)
	setIDs   func(ids ...string)
}

func newFreezeClockEnv(t *testing.T, ids ...string) *freezeClockEnv {
	t.Helper()
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	env := &freezeClockEnv{a: a, advance: func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }}
	// Connections outlive the clock steps, so that only the backoff decides.
	env.prov = newProvider(t, p, Options{Now: clock, MaxConnectionAge: 1000 * time.Hour})
	env.setIDs = func(ids ...string) {
		t.Helper()
		h := a.entry()
		h.FreezeConfirmation = &hostsecret.FreezeConfirmation{ForEnrolmentID: strings.Repeat("a1", 16), EnrolmentIDs: ids}
		require.NoError(t, env.prov.Reconcile(hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: []hostsecret.Host{h}}))
	}
	env.setIDs(ids...)
	client := providerv1.NewProviderClient(served(t, env.prov))
	env.try = func() error {
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		return err
	}
	env.describe = func() {
		t.Helper()
		require.NoError(t, env.try(), "a relay never fails the call")
	}
	return env
}

// TestFreezeConfirmationRefusedOverCapacityIsRedelivered: a confirmation the
// agent refuses over capacity is not marked delivered: it is relayed again
// after the backoff, not with every call, until the agent accepts it; then
// it is not relayed again on that connection.
func TestFreezeConfirmationRefusedOverCapacityIsRedelivered(t *testing.T) {
	env := newFreezeClockEnv(t, strings.Repeat("b2", 16))
	env.a.statusOutcome.Store(int32(agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_REFUSED_OVER_CAPACITY))
	env.describe()
	env.describe()
	require.EqualValues(t, 1, env.a.statusCalls.Load(), "not again before the backoff")
	env.advance(freezeRetryMin + time.Second)
	env.describe()
	require.EqualValues(t, 2, env.a.statusCalls.Load(), "a refused confirmation stays pending and is relayed again")
	env.advance(freezeRetryMin + time.Second)
	env.describe()
	require.EqualValues(t, 2, env.a.statusCalls.Load(), "the backoff doubled")

	env.a.statusOutcome.Store(int32(agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_ACCEPTED))
	env.advance(2 * freezeRetryMin)
	env.describe()
	require.EqualValues(t, 3, env.a.statusCalls.Load(), "relayed once capacity frees")
	env.advance(freezeRetryMax + time.Second)
	env.describe()
	env.describe()
	assert.EqualValues(t, 3, env.a.statusCalls.Load(), "accepted: delivered, not relayed again on this connection")
}

// TestFreezeConfirmationToAnAgentWithoutResults: an agent that answers
// AgentStatus without freeze_results (it predates them) is never assumed to
// have applied the confirmation: it is relayed again after the backoff,
// which grows up to freezeRetryMax, and never with every call.
func TestFreezeConfirmationToAnAgentWithoutResults(t *testing.T) {
	env := newFreezeClockEnv(t, strings.Repeat("b2", 16))
	env.a.statusOutcome.Store(statusNoResults)
	env.describe()
	env.describe()
	require.EqualValues(t, 1, env.a.statusCalls.Load())
	want := int32(1)
	for i := 0; i < 12; i++ {
		env.advance(freezeRetryMax + time.Second)
		env.describe()
		env.describe()
		want++
		require.EqualValues(t, want, env.a.statusCalls.Load(), "one relay per backoff period, round %d", i)
	}
	env.advance(freezeRetryMax - time.Second)
	env.describe()
	assert.EqualValues(t, want, env.a.statusCalls.Load(), "the backoff stays at freezeRetryMax")
}

// TestFreezeConfirmationChangeIsRelayedAtOnce: a changed confirmation does
// not wait for the backoff of the previous one, and is settled by its own
// results; a result list that does not echo the ids it answers settles
// nothing.
func TestFreezeConfirmationChangeIsRelayedAtOnce(t *testing.T) {
	first, second := strings.Repeat("b2", 16), strings.Repeat("c3", 16)
	env := newFreezeClockEnv(t, first)
	env.a.statusOutcome.Store(int32(agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_DEFERRED))
	env.describe()
	env.describe()
	require.EqualValues(t, 1, env.a.statusCalls.Load())

	env.a.mu.Lock()
	env.a.confirmFunc = func(req *agentv1.AgentStatusRequest) []*agentv1.FreezeConfirmationResult {
		var out []*agentv1.FreezeConfirmationResult
		for range req.GetConfirmFrozenEnrolmentIds() {
			out = append(out, &agentv1.FreezeConfirmationResult{EnrolmentId: make([]byte, 16),
				Outcome: agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_ACCEPTED})
		}
		return out
	}
	env.a.mu.Unlock()
	env.setIDs(first, second)
	env.describe()
	require.EqualValues(t, 2, env.a.statusCalls.Load(), "a changed confirmation is relayed with the next call")
	env.describe()
	require.EqualValues(t, 2, env.a.statusCalls.Load(), "results that echo other ids settle nothing: backoff")

	env.a.mu.Lock()
	env.a.confirmFunc = nil
	env.a.mu.Unlock()
	env.advance(freezeRetryMin + time.Second)
	env.describe()
	env.describe()
	assert.EqualValues(t, 3, env.a.statusCalls.Load(), "accepted, then settled")
}

// TestFreezeSettled: only exactly one result per id, in order, echoing it,
// each ACCEPTED, ALREADY_APPLIED or NOT_APPLICABLE, settles a confirmation.
func TestFreezeSettled(t *testing.T) {
	a, b := []byte{0xa}, []byte{0xb}
	r := func(id []byte, o agentv1.FreezeConfirmationOutcome) *agentv1.FreezeConfirmationResult {
		return &agentv1.FreezeConfirmationResult{EnrolmentId: id, Outcome: o}
	}
	const (
		acc  = agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_ACCEPTED
		alr  = agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_ALREADY_APPLIED
		na   = agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_NOT_APPLICABLE
		cap_ = agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_REFUSED_OVER_CAPACITY
		def  = agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_DEFERRED
		uns  = agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_UNSPECIFIED
	)
	for _, c := range []struct {
		name    string
		results []*agentv1.FreezeConfirmationResult
		settled bool
		refused int
	}{
		{"accepted and already applied", []*agentv1.FreezeConfirmationResult{r(a, acc), r(b, alr)}, true, 0},
		{"not applicable", []*agentv1.FreezeConfirmationResult{r(a, na), r(b, acc)}, true, 0},
		{"refused over capacity", []*agentv1.FreezeConfirmationResult{r(a, acc), r(b, cap_)}, false, 1},
		{"deferred", []*agentv1.FreezeConfirmationResult{r(a, def), r(b, acc)}, false, 0},
		{"unspecified", []*agentv1.FreezeConfirmationResult{r(a, uns), r(b, acc)}, false, 0},
		{"an outcome this Provider does not know", []*agentv1.FreezeConfirmationResult{r(a, 99), r(b, acc)}, false, 0},
		{"no results (an older agent)", nil, false, 0},
		{"short", []*agentv1.FreezeConfirmationResult{r(a, acc)}, false, 0},
		{"long", []*agentv1.FreezeConfirmationResult{r(a, acc), r(b, acc), r(b, acc)}, false, 0},
		{"out of order", []*agentv1.FreezeConfirmationResult{r(b, acc), r(a, acc)}, false, 0},
		{"no echo", []*agentv1.FreezeConfirmationResult{r(nil, acc), r(b, acc)}, false, 0},
	} {
		settled, refused := freezeSettled([][]byte{a, b}, c.results)
		assert.Equal(t, c.settled, settled, c.name)
		assert.Equal(t, c.refused, refused, c.name)
	}
}
