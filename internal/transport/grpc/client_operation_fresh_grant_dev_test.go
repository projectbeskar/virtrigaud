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

package grpc

import (
	"context"
	"crypto/x509"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestSendOperation_EverySendMintsAFreshGrant: the manager never stores,
// caches or re-sends a grant. Two sends of one PreparedOperation carry two
// different grants, each issued at send time, over the same request bytes
// and op_id: a grant that has expired, or whose anchor rotated, is never
// delivered again by the manager.
func TestSendOperation_EverySendMintsAFreshGrant(t *testing.T) {
	signer := newDevSigner(t)
	srv := &opRecorderServer{}
	cli := identityClient(t, srv, signer)
	id := opTestIdentity()
	v, err := operation.NewVerifier(operation.VerifierConfig{
		Anchors: []*x509.Certificate{signer.Anchor()}, InstallID: signer.InstallID(),
		ProviderUID: id.ProviderUID, HostID: id.HostID, EnrolmentID: id.EnrolmentID,
	})
	require.NoError(t, err)
	ctx := context.Background()
	p, err := cli.BuildOperation(ctx, id, opTestRequest(contracts.OperationKindPowerOn))
	require.NoError(t, err)
	var grants []*providerv1.OperationGrant
	for send := 0; send < 2; send++ {
		before := time.Now().Truncate(time.Second)
		_, err = cli.SendOperation(ctx, p)
		require.NoError(t, err)
		got := srv.last(t, providerv1.Provider_Power_FullMethodName)
		power, ok := got.(*providerv1.PowerRequest)
		require.True(t, ok)
		env := power.GetOperationEnvelope()
		verified, err := v.Verify(providerv1.Provider_Power_FullMethodName, got)
		require.NoError(t, err)
		assert.False(t, verified.Body.GetIssuedAt().AsTime().Before(before), "each grant is issued at its send")
		assert.Equal(t, id.OpID, verified.Body.GetOpId())
		grants = append(grants, proto.CloneOf(env.GetGrant()))
	}
	assert.False(t, proto.Equal(grants[0], grants[1]), "a re-send carries a newly minted grant, never the earlier one")
}
