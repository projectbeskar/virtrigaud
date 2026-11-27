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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// ledgerReceiptServer answers a Create with an operation receipt of a chosen body
// type.
type ledgerReceiptServer struct {
	opRecorderServer
	bodyType string
}

func (s *ledgerReceiptServer) Create(ctx context.Context, r *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	resp, err := s.opRecorderServer.Create(ctx, r)
	if err != nil {
		return nil, err
	}
	resp.OperationReceipt = &providerv1.SignedBlob{BodyType: s.bodyType, Body: []byte{1}, Signature: []byte{2},
		SignerCertificate: []byte{3}}
	return resp, nil
}

// The grant version and expect_known chosen for a send are signed into its
// grant, the receipt of the response is relayed (only with the receipt body
// type) and the answer names the digest of the grant sent.
func TestSendOperation_LedgerGrantAndReceipt(t *testing.T) {
	for _, bt := range []string{hostidentity.BodyTypeOperationReceipt, hostidentity.BodyTypeOperationAnswer} {
		t.Run(bt, func(t *testing.T) {
			srv := &ledgerReceiptServer{bodyType: bt}
			cli := identityClient(t, srv, newDevSigner(t))
			ctx := context.Background()
			p, err := cli.BuildOperation(ctx, opTestIdentity(), opTestRequest(contracts.OperationKindCreate))
			require.NoError(t, err)
			p.GrantVersion, p.ExpectKnown = grantverify.LedgerGrantVersion, true
			a, err := cli.SendOperation(ctx, p)
			require.NoError(t, err)
			req, ok := srv.last(t, providerv1.Provider_Create_FullMethodName).(*providerv1.CreateRequest)
			require.True(t, ok)
			g := req.GetOperationEnvelope().GetGrant()
			body := &providerv1.GrantBody{}
			require.NoError(t, proto.Unmarshal(g.GetBody(), body))
			assert.Equal(t, grantverify.LedgerGrantVersion, body.GetGrantVersion())
			assert.True(t, body.GetExpectKnown())
			assert.Equal(t, grantverify.GrantDigest(g), a.GrantDigest)
			if bt == hostidentity.BodyTypeOperationReceipt {
				require.NotNil(t, a.Receipt)
			} else {
				assert.Nil(t, a.Receipt, "only a receipt body type is relayed as a receipt")
			}
		})
	}
}

// The ledger version of a signed read is sent; the host authority calls
// refuse without a signer and send nothing.
func TestHostAuthority_NoSigner(t *testing.T) {
	cli := newTestClientForVMOps(t, &opRecorderServer{}, "cloudhypervisor", "identity")
	ctx := context.Background()
	_, err := cli.Challenge(ctx, contracts.ChallengeQuery{HostID: "h", Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST})
	require.Error(t, err)
	_, err = cli.Attest(ctx, contracts.AttestOrder{HostID: "h"})
	require.Error(t, err)
	_, err = cli.LedgerMark(ctx, contracts.LedgerMarkOrder{HostID: "h"})
	require.Error(t, err)
}
