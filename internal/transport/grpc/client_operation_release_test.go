//go:build !virtrigaud_dev

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

	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestReleaseBuild_RefusesToSendSignedOperations: a release build (no
// virtrigaud_dev tag) builds no operation-identity client, even with a
// signer, and SendOperation refuses with ErrNoSigner without sending, even on
// a client forced into operation-identity mode.
func TestReleaseBuild_RefusesToSendSignedOperations(t *testing.T) {
	require.False(t, operation.SignerCompiled)
	cli, err := NewOperationIdentityClient(context.Background(), "localhost:1", "cloudhypervisor", "p", nil, testVerifiedTLS(), stubSigner{})
	assert.Nil(t, cli)
	assert.ErrorIs(t, err, operation.ErrNoSigner)

	srv := &opRecorderServer{}
	forced := newTestClientForVMOps(t, srv, "cloudhypervisor", "forced")
	forced.opSigner = stubSigner{}
	for _, kind := range allOperationKinds {
		p, err := forced.BuildOperation(context.Background(), opTestIdentity(), opTestRequest(kind))
		require.NoError(t, err)
		_, err = forced.SendOperation(context.Background(), p)
		assert.ErrorIs(t, err, operation.ErrNoSigner, string(kind))
	}
	plain := newTestClientForVMOps(t, srv, "libvirt", "plain")
	_, err = plain.SendOperation(context.Background(), contracts.PreparedOperation{Kind: contracts.OperationKindDelete})
	assert.ErrorIs(t, err, operation.ErrNoSigner)
	assert.Empty(t, srv.all(), "a release build sent a signed operation")
}
