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
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestHostAnswerPendingRenewalOnlyWhenAsked: a pending renewal request goes
// into the signed answer only of a request that asked for it
// (renewal_version ≥ 1); an answer to any other request never carries it,
// whatever the caller passed.
func TestHostAnswerPendingRenewalOnlyWhenAsked(t *testing.T) {
	p := trusttest.New(t)
	pending := func() *providerv1.SignedBlob {
		return &providerv1.SignedBlob{Body: []byte("renewal request body"), Signature: []byte("signature")}
	}

	t.Run("asked", func(t *testing.T) {
		h := testHost(p)
		h.RenewalRequested, h.PendingRenewal = 1, pending()
		a, err := answer.HostAnswer(h)
		require.NoError(t, err)
		assert.True(t, proto.Equal(pending(), a.GetPendingRenewal()))
		a.PendingRenewal.Body[0] = 'X'
		assert.Equal(t, byte('r'), h.PendingRenewal.GetBody()[0], "the answer holds a copy")
	})
	t.Run("not asked: never a pending renewal", func(t *testing.T) {
		h := testHost(p)
		h.PendingRenewal = pending()
		a, err := answer.HostAnswer(h)
		require.NoError(t, err)
		assert.Nil(t, a.GetPendingRenewal())
	})
	t.Run("asked, nothing pending", func(t *testing.T) {
		h := testHost(p)
		h.RenewalRequested = 1
		a, err := answer.HostAnswer(h)
		require.NoError(t, err)
		assert.Nil(t, a.GetPendingRenewal())
	})
}
