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

package server_test

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
	"github.com/projectbeskar/virtrigaud/internal/agent/inventory"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// sourceFunc adapts a function to server.InventorySource.
type sourceFunc func(uint32) *providerv1.HostInventory

func (f sourceFunc) Inventory(r uint32) *providerv1.HostInventory { return f(r) }

// collector returns a collector over an empty root; collected says whether it
// has finished its first collection.
func collector(t *testing.T, collected bool) *inventory.Collector {
	t.Helper()
	c, err := inventory.New(inventory.Options{
		FS: inventory.OSFS{Root: t.TempDir()}, Host: inventory.SystemHost{}, Boot: clockfloor.SystemBootClock{},
		Log: logr.Discard(), AgentVersion: "test",
	})
	require.NoError(t, err)
	if collected {
		c.CollectNow(context.Background())
	}
	return c
}

func inventoryEnv(t *testing.T, src server.InventorySource) *env { return inventoryEnvLimit(t, src, 0) }

// inventoryEnvLimit lowers the encoded size bound, to reach the size fallback
// that the other bounds make unreachable with real data.
func inventoryEnvLimit(t *testing.T, src server.InventorySource, limit int) *env {
	t.Helper()
	return newEnvWith(t, server.Throttle{}, func(o *server.Options) { o.Inventory = src; o.InventoryMaxBytes = limit })
}

// TestGetHostInfoInventory: only GetHostInfo, only with a nonce and an
// inventory_version of at least 1, carries the inventory; it echoes the
// requested version and uses min(requested, supported).
func TestGetHostInfoInventory(t *testing.T) {
	e := inventoryEnv(t, collector(t, true))
	pc := providerv1.NewProviderClient(e.dial(e.provider(), 0))

	for _, requested := range []uint32{1, 2, 7} {
		info, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(3), InventoryVersion: requested})
		require.NoError(t, err)
		a := verifyAnswer(t, e, info.GetHostAnswer())
		inv := a.GetInventory()
		require.NotNil(t, inv, "requested %d", requested)
		assert.Equal(t, requested, inv.GetRequestedVersion(), "echoed")
		assert.EqualValues(t, inventory.SupportedVersion, inv.GetSupportedVersion())
		assert.EqualValues(t, 1, inv.GetInventoryVersion(), "min(requested, supported)")
		assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_COMPLETE, inv.GetState())
		require.NoError(t, inventory.Validate(inv, requested))
		assert.EqualValues(t, 2, a.GetAgentApiVersion())
	}

	t.Run("requested 0 gives no inventory", func(t *testing.T) {
		info, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(4)})
		require.NoError(t, err)
		assert.Nil(t, verifyAnswer(t, e, info.GetHostAnswer()).GetInventory())
	})
	t.Run("a version without a nonce is refused", func(t *testing.T) {
		_, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{InventoryVersion: 1})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})
	t.Run("no inventory in the agent service", func(t *testing.T) {
		ac := agentv1.NewAgentServiceClient(e.dial(e.provider(), 0))
		h, err := ac.Hello(ctx(t), &agentv1.HelloRequest{AnswerNonce: nonce(5), CallerApiVersion: 1})
		require.NoError(t, err)
		assert.Nil(t, verifyAnswer(t, e, h.GetHostAnswer()).GetInventory())
		s, err := ac.AgentStatus(ctx(t), &agentv1.AgentStatusRequest{AnswerNonce: nonce(6)})
		require.NoError(t, err)
		assert.Nil(t, verifyAnswer(t, e, s.GetHostAnswer()).GetInventory())
	})
	t.Run("unsigned reads never carry one", func(t *testing.T) {
		info, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{})
		require.NoError(t, err)
		assert.Nil(t, info.GetHostAnswer())
	})
}

func TestGetHostInfoInventoryPending(t *testing.T) {
	e := inventoryEnv(t, collector(t, false))
	pc := providerv1.NewProviderClient(e.dial(e.provider(), 0))
	info, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(3), InventoryVersion: 1})
	require.NoError(t, err)
	inv := verifyAnswer(t, e, info.GetHostAnswer()).GetInventory()
	require.NotNil(t, inv)
	assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_PENDING, inv.GetState())
	assert.Zero(t, inv.GetSnapshotAgeSeconds())
}

// TestInventoryThatBreaksARuleIsNeverSigned: a source that returns a report
// breaking a field rule gets a signed UNREADABLE (probe_failed) inventory in
// its place, in an answer that is still valid for every other decision.
func TestInventoryThatBreaksARuleIsNeverSigned(t *testing.T) {
	for name, src := range map[string]server.InventorySource{
		"unsorted features": sourceFunc(func(r uint32) *providerv1.HostInventory {
			inv := inventory.OverLimit(r, inventory.ReasonTooManyFiles)
			inv.State, inv.StateReason = providerv1.InventoryState_INVENTORY_STATE_COMPLETE, ""
			inv.ProcessIdentityTier = providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_START_TIME
			inv.Features = []*providerv1.HostFeature{{Name: "pidfd", Result: 1}, {Name: "cgroup_v2", Result: 1}}
			return inv
		}),
		"echo of another version": sourceFunc(func(uint32) *providerv1.HostInventory {
			return inventory.OverLimit(9, inventory.ReasonTooManyFiles)
		}),
	} {
		t.Run(name, func(t *testing.T) {
			e := inventoryEnv(t, src)
			pc := providerv1.NewProviderClient(e.dial(e.provider(), 0))
			info, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(3), InventoryVersion: 1})
			require.NoError(t, err)
			a := verifyAnswer(t, e, info.GetHostAnswer())
			require.NotNil(t, a.GetInventory())
			assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_UNREADABLE, a.GetInventory().GetState())
			assert.Equal(t, inventory.ReasonProbeFailed, a.GetInventory().GetStateReason())
			require.NoError(t, inventory.Validate(a.GetInventory(), 1))
			assert.Equal(t, nonce(3), a.GetContext().GetNonce())
		})
	}
	t.Run("nothing to report", func(t *testing.T) {
		e := inventoryEnv(t, sourceFunc(func(uint32) *providerv1.HostInventory { return nil }))
		pc := providerv1.NewProviderClient(e.dial(e.provider(), 0))
		info, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(3), InventoryVersion: 1})
		require.NoError(t, err)
		assert.Nil(t, verifyAnswer(t, e, info.GetHostAnswer()).GetInventory())
	})
}

// TestInventoryTooLargeFallsBack: a report above the size bound is replaced by
// a signed OVER_LIMIT inventory; when even that cannot be signed the answer
// goes out without an inventory, still signed.
func TestInventoryTooLargeFallsBack(t *testing.T) {
	big := sourceFunc(func(r uint32) *providerv1.HostInventory {
		inv := inventory.OverLimit(r, inventory.ReasonTooManyFiles)
		inv.State, inv.StateReason = providerv1.InventoryState_INVENTORY_STATE_COMPLETE, ""
		inv.ProcessIdentityTier = providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_START_TIME
		for i := range 20 {
			inv.Components = append(inv.Components, &providerv1.HostComponent{
				Name: "c" + string(rune('a'+i)), State: providerv1.ComponentState_COMPONENT_STATE_UNMANIFESTED,
				StateReason: inventory.ReasonPresentUndeclared, ManifestOrigin: providerv1.ManifestOrigin_MANIFEST_ORIGIN_NONE,
			})
		}
		return inv
	})
	t.Run("replaced by OVER_LIMIT", func(t *testing.T) {
		e := inventoryEnvLimit(t, big, 300)
		pc := providerv1.NewProviderClient(e.dial(e.provider(), 0))
		info, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(3), InventoryVersion: 1})
		require.NoError(t, err)
		inv := verifyAnswer(t, e, info.GetHostAnswer()).GetInventory()
		require.NotNil(t, inv)
		assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_OVER_LIMIT, inv.GetState())
		assert.Equal(t, inventory.ReasonInventoryTooLarge, inv.GetStateReason())
		assert.Empty(t, inv.GetComponents())
	})
	t.Run("a second failure leaves no inventory, not the answer", func(t *testing.T) {
		e := inventoryEnvLimit(t, big, 10)
		pc := providerv1.NewProviderClient(e.dial(e.provider(), 0))
		info, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(3), InventoryVersion: 1})
		require.NoError(t, err)
		a := verifyAnswer(t, e, info.GetHostAnswer())
		assert.Nil(t, a.GetInventory())
		assert.Equal(t, nonce(3), a.GetContext().GetNonce())
	})
}

func TestNoInventorySource(t *testing.T) {
	e := newEnv(t)
	pc := providerv1.NewProviderClient(e.dial(e.provider(), 0))
	info, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(3), InventoryVersion: 1})
	require.NoError(t, err)
	assert.Nil(t, verifyAnswer(t, e, info.GetHostAnswer()).GetInventory())
}
