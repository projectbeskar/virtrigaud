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
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// newDevSigner is a development signer with a fresh key.
func newDevSigner(t *testing.T) *operation.DevSigner {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	installID, err := operation.NewRandomID()
	require.NoError(t, err)
	s, err := operation.NewDevSigner(key, installID, nil)
	require.NoError(t, err)
	return s
}

// identityClient is a test client in operation-identity mode with s.
func identityClient(t *testing.T, srv providerv1.ProviderServer, s operation.Signer) *Client {
	t.Helper()
	cli := newTestClientForVMOps(t, srv, "cloudhypervisor", "identity")
	cli.opSigner = s
	return cli
}

// kindMethod is the RPC a contracts kind is sent on.
func kindMethod(kind contracts.OperationKind) string {
	switch kind {
	case contracts.OperationKindCreate:
		return providerv1.Provider_Create_FullMethodName
	case contracts.OperationKindDelete:
		return providerv1.Provider_Delete_FullMethodName
	case contracts.OperationKindReconfigure:
		return providerv1.Provider_Reconfigure_FullMethodName
	default:
		return providerv1.Provider_Power_FullMethodName
	}
}

// onlyEnvelopeSet reports whether m has operation_envelope set and nothing
// else.
func onlyEnvelopeSet(m proto.Message) bool {
	r := m.ProtoReflect()
	only := len(r.GetUnknown()) == 0
	n := 0
	r.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		n++
		if fd.Name() != "operation_envelope" {
			only = false
		}
		return true
	})
	return only && n == 1
}

// TestNewOperationIdentityClient_DevBuild: a development build builds one
// with a signer, and refuses a nil signer.
func TestNewOperationIdentityClient_DevBuild(t *testing.T) {
	require.True(t, operation.SignerCompiled)
	_, err := NewOperationIdentityClient(context.Background(), "localhost:1", "cloudhypervisor", "p", nil, testVerifiedTLS(), nil)
	assert.ErrorIs(t, err, operation.ErrNoSigner)
	signer := newDevSigner(t)
	for name, cfg := range map[string]*TLSConfig{
		"plaintext (nil)":      nil,
		"insecure":             {Insecure: true},
		"skip verify prebuilt": {PrebuiltConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}}, // #nosec G402 -- the refused case under test
	} {
		_, err := NewOperationIdentityClient(context.Background(), "localhost:1", "cloudhypervisor", "p", nil, cfg, signer)
		assert.ErrorIs(t, err, ErrOperationIdentityNeedsTLS, name)
	}
	cli, err := NewOperationIdentityClient(context.Background(), "localhost:1", "cloudhypervisor", "p", nil, testVerifiedTLS(), signer)
	require.NoError(t, err)
	require.NotNil(t, cli.opSigner)
	require.NoError(t, cli.Close())
}

// TestSendOperation_BytesForwardedUnchanged: for every kind, the envelope
// reaches the Provider on the kind's RPC with no other outer field set, its
// request is byte for byte the prepared bytes, and the shared verifier
// accepts it. A re-send of the same prepared operation sends the same bytes
// and op_id under a fresh grant.
func TestSendOperation_BytesForwardedUnchanged(t *testing.T) {
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
	for _, kind := range allOperationKinds {
		t.Run(string(kind), func(t *testing.T) {
			p, err := cli.BuildOperation(ctx, id, opTestRequest(kind))
			require.NoError(t, err)
			for send := 0; send < 2; send++ {
				_, err = cli.SendOperation(ctx, p)
				require.NoError(t, err)
				got := srv.last(t, kindMethod(kind))
				require.True(t, onlyEnvelopeSet(got), "%s: an outer field is set beside the envelope", kind)
				carrier, ok := got.(interface {
					GetOperationEnvelope() *providerv1.OperationEnvelope
				})
				require.True(t, ok)
				env := carrier.GetOperationEnvelope()
				assert.True(t, bytes.Equal(p.RequestBytes, env.GetRequest()), "%s: the request bytes changed", kind)
				verified, err := v.Verify(kindMethod(kind), got)
				require.NoError(t, err, "%s", kind)
				assert.Equal(t, id.OpID, verified.Body.GetOpId())
				assert.Equal(t, operation.IntentDigest(p.DigestSalt, p.RequestBytes), verified.Body.GetIntentDigest())
			}
		})
	}
}

// TestSendOperation_Answers: the answer carries the Provider's VM id, task
// reference and Reconfigure result.
func TestSendOperation_Answers(t *testing.T) {
	cli := identityClient(t, &opRecorderServer{}, newDevSigner(t))
	ctx := context.Background()
	p, err := cli.BuildOperation(ctx, opTestIdentity(), opTestRequest(contracts.OperationKindCreate))
	require.NoError(t, err)
	a, err := cli.SendOperation(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, contracts.OperationAnswer{ID: "vm-id-1", TaskRef: "task-create"}, a)

	p, err = cli.BuildOperation(ctx, opTestIdentity(), opTestRequest(contracts.OperationKindReconfigure))
	require.NoError(t, err)
	a, err = cli.SendOperation(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, contracts.OperationAnswer{RestartRequired: true, Honest: true}, a)
}

// TestSendOperation_Refusals: a client with no signer, a kind with no signed
// path and a tampered prepared operation are refused and send nothing.
func TestSendOperation_Refusals(t *testing.T) {
	srv := &opRecorderServer{}
	ctx := context.Background()
	plain := newTestClientForVMOps(t, srv, "libvirt", "plain")
	p, err := plain.BuildOperation(ctx, opTestIdentity(), opTestRequest(contracts.OperationKindDelete))
	require.NoError(t, err)
	_, err = plain.SendOperation(ctx, p)
	assert.ErrorIs(t, err, operation.ErrNoSigner)

	cli := identityClient(t, srv, newDevSigner(t))
	_, err = cli.SendOperation(ctx, contracts.PreparedOperation{Kind: "Clone"})
	assert.ErrorIs(t, err, contracts.ErrOperationIdentityNotImplemented)

	tampered := p
	tampered.Identity.Attempt = 0
	_, err = cli.SendOperation(ctx, tampered)
	assert.Error(t, err)

	wrongType := p
	wrongType.Kind = contracts.OperationKindPowerOn // DeleteRequest bytes under a Power kind
	_, err = cli.SendOperation(ctx, wrongType)
	assert.Error(t, err)
	assert.Empty(t, srv.all())
}
