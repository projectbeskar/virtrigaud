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
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	transportgrpc "github.com/projectbeskar/virtrigaud/internal/transport/grpc"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// routedAnswerServer answers ImportDisk with the scripted status.
type routedAnswerServer struct {
	providerv1.UnimplementedProviderServer
	err error
}

func (s *routedAnswerServer) ImportDisk(context.Context, *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	return nil, s.err
}

// routedStatus builds a provider status with ErrorInfo reasons (and, for
// VM_PREVIOUS_INCARNATION, optional metadata), as the clustered libvirt
// provider answers.
func routedStatus(t *testing.T, code codes.Code, meta map[string]string, reasons ...string) error {
	t.Helper()
	st := status.New(code, "importDisk: answer")
	for _, r := range reasons {
		next, err := st.WithDetails(&errdetails.ErrorInfo{Reason: r, Domain: contracts.ErrorInfoDomain, Metadata: meta})
		require.NoError(t, err)
		st = next
	}
	return st.Err()
}

// TestRoutedImportAnswers_ThroughTheTransport (#416 L6) drives one answer of
// each class of the clustered libvirt ImportDisk through the real transport
// client (a gRPC server on loopback, the production mapGRPCError and its
// untyped errors) and pins what the VMMigration controller makes of it: a
// definite or unknown outcome, a pre-write FailedPrecondition, and the action.
func TestRoutedImportAnswers_ThroughTheTransport(t *testing.T) {
	own := map[string]string{contracts.VMPreviousIncarnationKindKey: contracts.VMPreviousIncarnationKindOwn}
	for name, tc := range map[string]struct {
		err         error
		known       bool
		precond     bool
		storage     bool
		action      string
		invalidSpec bool
	}{
		"bad request (InvalidArgument)": {err: status.Error(codes.InvalidArgument, "bad"), known: true, invalidSpec: true},
		"previous incarnation": {err: routedStatus(t, codes.AlreadyExists, nil, contracts.VMPreviousIncarnationReason),
			known: true, action: routedActionHoldPreviousIncarnation},
		"own incarnation": {err: routedStatus(t, codes.AlreadyExists, own, contracts.VMPreviousIncarnationReason),
			known: true, action: routedActionHoldPreviousIncarnation},
		"foreign domain (plain AlreadyExists)": {err: status.Error(codes.AlreadyExists, "taken"), known: true, action: routedActionRelease},
		"host storage misconfigured": {err: routedStatus(t, codes.FailedPrecondition, nil, contracts.HostStorageMisconfiguredReason,
			contracts.VMOperationFailedReason), precond: true, storage: true},
		"landing file in use": {err: routedStatus(t, codes.FailedPrecondition, nil, contracts.VMDiskInUseReason,
			contracts.VMOperationFailedReason), known: true, action: routedActionHoldDiskInUse},
		"guard refusal (symlink, tool missing)": {err: routedStatus(t, codes.FailedPrecondition, nil, contracts.VMOperationFailedReason),
			precond: true},
		"host not over ssh (no reason)": {err: status.Error(codes.FailedPrecondition, "not ssh"), precond: true},
		"disk size above the limit": {err: routedStatus(t, codes.FailedPrecondition, nil, contracts.DiskSizeExceedsLimitReason),
			known: true},
		"host unavailable": {err: routedStatus(t, codes.Unavailable, nil, contracts.HostUnavailableReason), known: true,
			action: routedActionWaitHost},
		"disk check failed": {err: routedStatus(t, codes.Unavailable, nil, contracts.VMDiskCheckFailedReason,
			contracts.VMOperationFailedReason), known: true, action: routedActionWaitHost},
		"not performed": {err: routedStatus(t, codes.Unavailable, nil, contracts.VMOperationFailedReason), known: true,
			action: routedActionNotPerformed},
		"unknown outcome": {err: routedStatus(t, codes.Unknown, nil, contracts.VMOperationFailedReason)},
	} {
		t.Run(name, func(t *testing.T) {
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			srv := grpc.NewServer()
			providerv1.RegisterProviderServer(srv, &routedAnswerServer{err: tc.err})
			go func() { _ = srv.Serve(lis) }()
			t.Cleanup(srv.Stop)
			cli, err := transportgrpc.NewClient(context.Background(), lis.Addr().String(), "libvirt", "routed-answers", nil, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = cli.Close() })

			_, got := cli.ImportDisk(context.Background(), contracts.ImportDiskRequest{SourceURL: "nfs://n/e/o", BackendType: "nfs",
				TargetHostID: "host-b", TargetVM: contracts.ObjectIdentity{UID: "0b9c3a52-1f4e-4c55-9d1e-7b2f7f0e5a11",
					Namespace: "team-a", Name: "web"}})
			require.Error(t, got)

			assert.Equal(t, tc.precond, routedImportPreconditionRefused(got), "pre-write FailedPrecondition")
			assert.Equal(t, tc.storage, contracts.IsHostStorageMisconfigured(got), "host storage misconfigured")
			if tc.precond {
				return // decided before the outcome is classified
			}
			assert.Equal(t, tc.known, providerCallOutcomeKnownFor(got, callAttempt{}), "definite outcome")
			assert.False(t, providerCallOutcomeKnownFor(got, callAttempt{resend: true}), "an answer to a re-send is never definite")
			if tc.known {
				assert.Equal(t, tc.action, routedRefusalAction(got))
				assert.Equal(t, tc.invalidSpec, contracts.IsInvalidSpec(got))
			}
		})
	}
}
