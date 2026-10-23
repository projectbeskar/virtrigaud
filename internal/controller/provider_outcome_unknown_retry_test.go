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

package controller

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	transportgrpc "github.com/projectbeskar/virtrigaud/internal/transport/grpc"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// unknownRetryServer answers SnapshotCreate and Clone with the scripted
// status.
type unknownRetryServer struct {
	providerv1.UnimplementedProviderServer
	err error
}

func (s *unknownRetryServer) SnapshotCreate(context.Context, *providerv1.SnapshotCreateRequest) (*providerv1.SnapshotCreateResponse, error) {
	return nil, s.err
}

func (s *unknownRetryServer) Clone(context.Context, *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	return nil, s.err
}

// TestOutcomeUnknownRetry_ThroughTheTransport (R25 M1) drives the clustered
// libvirt provider's "outcome unknown, safe to retry" answer
// (codes.Unavailable with VM_OUTCOME_UNKNOWN_RETRY and VM_OPERATION_FAILED)
// through the real transport client and pins what the controllers make of it:
// retried, never read as nothing done, never as a definite outcome. The same
// status without the new reason (an older provider) stays "not performed".
func TestOutcomeUnknownRetry_ThroughTheTransport(t *testing.T) {
	unknownRetry := routedStatus(t, codes.Unavailable, nil, contracts.OutcomeUnknownRetryReason,
		contracts.VMOperationFailedReason, contracts.HostConnectionLostReason)
	notPerformed := routedStatus(t, codes.Unavailable, nil, contracts.VMOperationFailedReason)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &unknownRetryServer{}
	gsrv := grpc.NewServer()
	providerv1.RegisterProviderServer(gsrv, srv)
	go func() { _ = gsrv.Serve(lis) }()
	t.Cleanup(gsrv.Stop)
	cli, err := transportgrpc.NewClient(context.Background(), lis.Addr().String(), "libvirt", "unknown-retry", nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })

	calls := map[string]func() error{
		"SnapshotCreate": func() error {
			_, e := cli.SnapshotCreate(context.Background(), contracts.SnapshotCreateRequest{
				VM: contracts.VMRef{ID: "web", HostID: "host-b"}, NameHint: "s", RequestToken: "tok"})
			return e
		},
		"Clone": func() error {
			_, e := cli.Clone(context.Background(), contracts.CloneRequest{
				Source: contracts.VMRef{ID: "web", HostID: "host-b"}, TargetName: "copy", TargetHostID: "host-b"})
			return e
		},
	}
	for name, call := range calls {
		t.Run(name+": outcome unknown, retry", func(t *testing.T) {
			srv.err = unknownRetry
			got := call()
			require.Error(t, got)
			assert.True(t, contracts.IsRetryable(got))
			assert.True(t, contracts.IsOutcomeUnknownRetry(got))
			assert.False(t, contracts.IsNothingDone(got))
			assert.True(t, contracts.IsHostConnectionLost(got))
			assert.False(t, providerCallOutcomeKnown(got), "the marker is kept")
			assert.False(t, providerCallOutcomeKnownFor(got, callAttempt{}))
			assert.False(t, providerCallOutcomeKnownFor(got, callAttempt{resend: true}))
		})
		t.Run(name+": not performed (the same status without the new reason)", func(t *testing.T) {
			srv.err = notPerformed
			got := call()
			require.Error(t, got)
			assert.True(t, contracts.IsNothingDone(got))
			assert.False(t, contracts.IsOutcomeUnknownRetry(got))
			assert.True(t, providerCallOutcomeKnown(got))
		})
	}
}
