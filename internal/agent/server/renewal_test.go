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
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// fakeRenewals is a server.Renewals with a fixed pending request and staged
// digest.
type fakeRenewals struct {
	mu        sync.Mutex
	pending   *providerv1.SignedBlob
	staged    []byte
	asked     []uint32
	installed [][]byte
	activated []bool
	err       error
}

func (f *fakeRenewals) PendingRequest(_ context.Context, requested uint32) *providerv1.SignedBlob {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, requested)
	return f.pending
}

func (f *fakeRenewals) StagedSHA256() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.staged
}

func (f *fakeRenewals) Install(_ context.Context, der []byte, activate bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installed = append(f.installed, der)
	f.activated = append(f.activated, activate)
	return f.err
}

// TestRenewalServed: the pending renewal request goes only into the answer of
// a GetHostInfo that asked for it; the staged digest goes into every host
// answer; InstallRenewedCertificate needs a nonce and a certificate, passes
// the request on, and answers signed.
func TestRenewalServed(t *testing.T) {
	staged := bytes.Repeat([]byte{0xee}, 32)
	pending := &providerv1.SignedBlob{BodyType: "provider.v1.RenewalRequest", Body: []byte{1}, Signature: []byte{2}, SignerCertificate: []byte{3}}
	fr := &fakeRenewals{pending: pending, staged: staged}
	e := newEnvWith(t, server.Throttle{}, func(o *server.Options) { o.Renewals = fr })
	cc := e.dial(e.provider(), 0)
	pc := providerv1.NewProviderClient(cc)
	ac := agentv1.NewAgentServiceClient(cc)

	info, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(1), RenewalVersion: 1})
	require.NoError(t, err)
	a := verifyAnswer(t, e, info.GetHostAnswer())
	require.NotNil(t, a.GetPendingRenewal())
	assert.Equal(t, staged, a.GetStagedCertificateSha256())

	info, err = pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(2)})
	require.NoError(t, err)
	a = verifyAnswer(t, e, info.GetHostAnswer())
	assert.Nil(t, a.GetPendingRenewal(), "not asked for")
	assert.Equal(t, staged, a.GetStagedCertificateSha256())

	st, err := ac.AgentStatus(ctx(t), &agentv1.AgentStatusRequest{AnswerNonce: nonce(3)})
	require.NoError(t, err)
	a = verifyAnswer(t, e, st.GetHostAnswer())
	assert.Nil(t, a.GetPendingRenewal(), "never in the internal service")
	assert.Equal(t, staged, a.GetStagedCertificateSha256())

	_, err = pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{RenewalVersion: 1})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "renewal_version without a nonce")

	resp, err := ac.InstallRenewedCertificate(ctx(t), &agentv1.InstallRenewedCertificateRequest{AnswerNonce: nonce(4), Certificate: []byte{9}, Activate: true})
	require.NoError(t, err)
	verifyAnswer(t, e, resp.GetHostAnswer())
	assert.Equal(t, [][]byte{{9}}, fr.installed)
	assert.Equal(t, []bool{true}, fr.activated)

	_, err = ac.InstallRenewedCertificate(ctx(t), &agentv1.InstallRenewedCertificateRequest{Certificate: []byte{9}})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "no nonce")
	_, err = ac.InstallRenewedCertificate(ctx(t), &agentv1.InstallRenewedCertificateRequest{AnswerNonce: nonce(5)})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "no certificate")
	fr.mu.Lock()
	fr.err = status.Error(codes.FailedPrecondition, "nothing staged")
	fr.mu.Unlock()
	_, err = ac.InstallRenewedCertificate(ctx(t), &agentv1.InstallRenewedCertificateRequest{AnswerNonce: nonce(6), Certificate: []byte{9}})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err), "the renewal's refusal is passed on")
}

// TestRenewalNotServed: without Renewals (a release build) the install is
// Unimplemented and no answer carries a renewal.
func TestRenewalNotServed(t *testing.T) {
	e := newEnv(t)
	cc := e.dial(e.provider(), 0)
	_, err := agentv1.NewAgentServiceClient(cc).InstallRenewedCertificate(ctx(t),
		&agentv1.InstallRenewedCertificateRequest{AnswerNonce: nonce(1), Certificate: []byte{1}})
	assert.Equal(t, codes.Unimplemented, status.Code(err))
	info, err := providerv1.NewProviderClient(cc).GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(2), RenewalVersion: 1})
	require.NoError(t, err)
	a := verifyAnswer(t, e, info.GetHostAnswer())
	assert.Nil(t, a.GetPendingRenewal())
	assert.Empty(t, a.GetStagedCertificateSha256())
}
