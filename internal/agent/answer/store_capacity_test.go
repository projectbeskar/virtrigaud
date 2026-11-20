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

package answer_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestHostAnswerStoreCapacity: the store capacity goes into the answer only of
// a request that asked for it (store_capacity_version >= 1), whatever the
// caller passed, echoing the request's version; NOT_MEASURED when it was not
// measured; its level follows the contract's rule for the sizes.
func TestHostAnswerStoreCapacity(t *testing.T) {
	p := trusttest.New(t)
	for _, tc := range []struct {
		live  int64
		level providerv1.StoreLevel
	}{
		{1, providerv1.StoreLevel_STORE_LEVEL_OK},
		{767, providerv1.StoreLevel_STORE_LEVEL_OK},
		{768, providerv1.StoreLevel_STORE_LEVEL_WARNING},
		{1024, providerv1.StoreLevel_STORE_LEVEL_WARNING},
		{1025, providerv1.StoreLevel_STORE_LEVEL_FULL},
	} {
		h := testHost(p)
		h.StoreCapacityRequested, h.StoreCapacity = 1, opstore.Capacity{LiveBytes: tc.live, MaxBytes: 1024}
		a, err := answer.HostAnswer(h)
		require.NoError(t, err)
		c := a.GetStoreCapacity()
		require.NotNil(t, c, "live %d", tc.live)
		assert.EqualValues(t, tc.live, c.GetLiveBytes())
		assert.EqualValues(t, 1024, c.GetThresholdBytes())
		assert.Equal(t, tc.level, c.GetLevel(), "live %d", tc.live)
		assert.EqualValues(t, 1, c.GetRequestedVersion(), "the request's version, echoed")
		assert.EqualValues(t, 1, c.GetVersion())
	}
	t.Run("the echo is the request's version, byte for byte", func(t *testing.T) {
		h := testHost(p)
		h.StoreCapacityRequested, h.StoreCapacity = 7, opstore.Capacity{LiveBytes: 10, MaxBytes: 1024}
		a, err := answer.HostAnswer(h)
		require.NoError(t, err)
		assert.EqualValues(t, 7, a.GetStoreCapacity().GetRequestedVersion())
		assert.EqualValues(t, 1, a.GetStoreCapacity().GetVersion(), "min(requested, supported)")
	})
	t.Run("not asked: never a capacity", func(t *testing.T) {
		h := testHost(p)
		h.StoreCapacity = opstore.Capacity{LiveBytes: 2000, MaxBytes: 1024}
		a, err := answer.HostAnswer(h)
		require.NoError(t, err)
		assert.Nil(t, a.GetStoreCapacity())
	})
	t.Run("asked, not measured: NOT_MEASURED, never left out", func(t *testing.T) {
		h := testHost(p)
		h.StoreCapacityRequested = 1
		a, err := answer.HostAnswer(h)
		require.NoError(t, err)
		c := a.GetStoreCapacity()
		require.NotNil(t, c)
		assert.Equal(t, providerv1.StoreLevel_STORE_LEVEL_NOT_MEASURED, c.GetLevel())
		assert.Zero(t, c.GetLiveBytes())
		assert.Zero(t, c.GetThresholdBytes())
		assert.EqualValues(t, 1, c.GetRequestedVersion())
	})
}
