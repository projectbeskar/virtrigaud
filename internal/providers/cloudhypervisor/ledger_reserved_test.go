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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/clustered"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// The host ledger protocol is reserved, not implemented. The Provider relays
// what it relayed before the fields existed (they were unknown fields then,
// kept byte for byte), and does not serve the HostAuthority service.

// TestLedgerReserved_ReadsForwardedUnchanged: a signed read that carries a
// ledger_version reaches the agent byte for byte.
func TestLedgerReserved_ReadsForwardedUnchanged(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	client := providerv1.NewProviderClient(served(t, newProvider(t, p, Options{}, a)))
	nonce := make([]byte, nonceLen)

	reqs := []proto.Message{
		&providerv1.TaskStatusRequest{AnswerNonce: nonce, OpId: make([]byte, 16), OpEnrolmentId: make([]byte, 16), TargetHostId: "host-a", LedgerVersion: 1},
		&providerv1.DescribeRequest{Id: "vm-1", TargetHostId: "host-a", AnswerNonce: nonce, BindingId: make([]byte, 16), LedgerVersion: 1},
		&providerv1.GetHostInfoRequest{HostId: "host-a", AnswerNonce: nonce, LedgerVersion: 1},
	}
	_, err := client.TaskStatus(ctx(t), reqs[0].(*providerv1.TaskStatusRequest))
	require.NoError(t, err)
	_, err = client.Describe(ctx(t), reqs[1].(*providerv1.DescribeRequest))
	require.NoError(t, err)
	_, err = client.GetHostInfo(ctx(t), reqs[2].(*providerv1.GetHostInfoRequest))
	require.NoError(t, err)

	got := a.received()
	require.Len(t, got, len(reqs))
	for i, r := range reqs {
		want, err := proto.Marshal(r)
		require.NoError(t, err)
		assert.Equal(t, want, got[i].raw, "request %d forwarded byte for byte", i)
	}
}

// TestLedgerReserved_ReceiptRelayedAsBefore: an operation_receipt on the
// agent's accept response is relayed with the host-encoded task reference,
// exactly as the same bytes were relayed when the field was unknown; the
// Provider neither reads nor drops it.
func TestLedgerReserved_ReceiptRelayedAsBefore(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	receipt := &providerv1.SignedBlob{Body: []byte{1, 2, 3}, Signature: []byte{4}, SignerCertificate: []byte{5},
		BodyType: "provider.v1.OperationReceipt"}
	a.setRespond(func(string, []byte) (proto.Message, error) {
		return &providerv1.TaskResponse{Task: &providerv1.TaskRef{Id: "task-1"}, OperationReceipt: receipt}, nil
	})
	client := providerv1.NewProviderClient(served(t, newProvider(t, p, Options{}, a)))

	resp, err := client.Power(ctx(t), signedPower(t, "host-a"))
	require.NoError(t, err)
	assert.Equal(t, clustered.EncodeHostTaskRef("host-a", "task-1"), resp.GetTask().GetId())
	assert.True(t, proto.Equal(receipt, resp.GetOperationReceipt()), "relayed unchanged")
}

// TestLedgerReserved_HostAuthorityNotServed: the Provider registers no
// HostAuthority service, so each of its methods is Unimplemented.
func TestLedgerReserved_HostAuthorityNotServed(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	cc := served(t, newProvider(t, p, Options{}, a))
	ha := providerv1.NewHostAuthorityClient(cc)

	_, err := ha.Challenge(ctx(t), &providerv1.ChallengeRequest{TargetHostId: "host-a"})
	assert.Equal(t, codes.Unimplemented, status.Code(err), "Challenge: %v", err)
	_, err = ha.Attest(ctx(t), &providerv1.AttestRequest{TargetHostId: "host-a"})
	assert.Equal(t, codes.Unimplemented, status.Code(err), "Attest: %v", err)
	_, err = ha.LedgerMark(ctx(t), &providerv1.LedgerMarkRequest{TargetHostId: "host-a"})
	assert.Equal(t, codes.Unimplemented, status.Code(err), "LedgerMark: %v", err)
	_, err = ha.RetireIfUnknown(ctx(t), &providerv1.RetireIfUnknownRequest{TargetHostId: "host-a"})
	assert.Equal(t, codes.Unimplemented, status.Code(err), "RetireIfUnknown: %v", err)
	assert.Empty(t, a.received(), "nothing reaches the agent")
}
