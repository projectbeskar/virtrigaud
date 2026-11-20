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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestGetHostInfoStoreCapacity: only GetHostInfo, only with a nonce and a
// store_capacity_version of at least 1, carries the store capacity, signed,
// with the level the contract's rule gives for the agent's sizes.
func TestGetHostInfoStoreCapacity(t *testing.T) {
	e := newEnv(t)
	pc := providerv1.NewProviderClient(e.dial(e.provider(), 0))
	ask := func(t *testing.T, req *providerv1.GetHostInfoRequest) *providerv1.HostAnswer {
		t.Helper()
		info, err := pc.GetHostInfo(ctx(t), req)
		require.NoError(t, err)
		return verifyAnswer(t, e, info.GetHostAnswer())
	}

	for _, tc := range []struct {
		live  int64
		level providerv1.StoreLevel
	}{
		{100, providerv1.StoreLevel_STORE_LEVEL_OK},
		{749, providerv1.StoreLevel_STORE_LEVEL_OK},
		{750, providerv1.StoreLevel_STORE_LEVEL_WARNING},
		{1000, providerv1.StoreLevel_STORE_LEVEL_WARNING},
		{1001, providerv1.StoreLevel_STORE_LEVEL_FULL},
	} {
		e.agent.setCapacity(opstore.Capacity{LiveBytes: tc.live, MaxBytes: 1000})
		c := ask(t, &providerv1.GetHostInfoRequest{AnswerNonce: nonce(3), StoreCapacityVersion: 1}).GetStoreCapacity()
		require.NotNil(t, c, "live %d", tc.live)
		assert.EqualValues(t, tc.live, c.GetLiveBytes())
		assert.EqualValues(t, 1000, c.GetThresholdBytes())
		assert.Equal(t, tc.level, c.GetLevel(), "live %d", tc.live)
	}

	t.Run("requested 0 gives none", func(t *testing.T) {
		assert.Nil(t, ask(t, &providerv1.GetHostInfoRequest{AnswerNonce: nonce(4)}).GetStoreCapacity())
		assert.Nil(t, ask(t, &providerv1.GetHostInfoRequest{AnswerNonce: nonce(4), InventoryVersion: 1, RenewalVersion: 1}).GetStoreCapacity())
	})
	t.Run("not measured gives none", func(t *testing.T) {
		e.agent.setCapacity(opstore.Capacity{})
		assert.Nil(t, ask(t, &providerv1.GetHostInfoRequest{AnswerNonce: nonce(4), StoreCapacityVersion: 1}).GetStoreCapacity())
		e.agent.setCapacity(opstore.Capacity{LiveBytes: 10, MaxBytes: 1000})
	})
	t.Run("a version without a nonce is refused", func(t *testing.T) {
		_, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{StoreCapacityVersion: 1})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})
	t.Run("never in the agent service", func(t *testing.T) {
		ac := agentv1.NewAgentServiceClient(e.dial(e.provider(), 0))
		h, err := ac.Hello(ctx(t), &agentv1.HelloRequest{AnswerNonce: nonce(5), CallerApiVersion: 1})
		require.NoError(t, err)
		assert.Nil(t, verifyAnswer(t, e, h.GetHostAnswer()).GetStoreCapacity())
		s, err := ac.AgentStatus(ctx(t), &agentv1.AgentStatusRequest{AnswerNonce: nonce(6)})
		require.NoError(t, err)
		assert.Nil(t, verifyAnswer(t, e, s.GetHostAnswer()).GetStoreCapacity())
	})
}
