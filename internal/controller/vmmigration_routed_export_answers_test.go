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

// exportAnswerServer answers ExportDisk with the scripted status.
type exportAnswerServer struct {
	providerv1.UnimplementedProviderServer
	err error
}

func (s *exportAnswerServer) ExportDisk(context.Context, *providerv1.ExportDiskRequest) (*providerv1.ExportDiskResponse, error) {
	return nil, s.err
}

// TestRoutedExportAnswers_ThroughTheTransport (R25) drives the three answers
// of a clustered libvirt ExportDisk to a host that stops answering through the
// real transport client and pins what the VMMigration controller makes of
// them: HOST_UNAVAILABLE and "not performed" prove that nothing was exported
// (the marker is replaced by the failure), an unknown outcome — the export's
// command was sent and its session dropped, or it was stopped by timeout(1) —
// does not (the marker is kept; the export is re-sent only after the settle
// time, and only for a re-send-safe backend). The provider text is its own
// (internal/providers/libvirt wireHost*Fmt); only the status and ErrorInfo
// matter here.
func TestRoutedExportAnswers_ThroughTheTransport(t *testing.T) {
	for name, tc := range map[string]struct {
		err   error
		known bool
	}{
		"the host never answered (HOST_UNAVAILABLE)": {
			err: routedStatus(t, codes.Unavailable, nil, contracts.HostUnavailableReason), known: true},
		"only reads ran (not performed)": {
			err: routedStatus(t, codes.Unavailable, nil, contracts.VMOperationFailedReason), known: true},
		"the export's command was sent and its session dropped (unknown)": {
			err: routedStatus(t, codes.Unknown, nil, contracts.VMOperationFailedReason)},
	} {
		t.Run(name, func(t *testing.T) {
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			srv := grpc.NewServer()
			providerv1.RegisterProviderServer(srv, &exportAnswerServer{err: tc.err})
			go func() { _ = srv.Serve(lis) }()
			t.Cleanup(srv.Stop)
			cli, err := transportgrpc.NewClient(context.Background(), lis.Addr().String(), "libvirt", "routed-export-answers", nil, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = cli.Close() })

			_, got := cli.ExportDisk(context.Background(), contracts.ExportDiskRequest{
				VM: contracts.VMRef{ID: "web", HostID: "host-b"}, BackendType: "nfs", DestinationURL: "nfs://n/e/o"})
			require.Error(t, got)
			assert.Equal(t, tc.known, providerCallOutcomeKnownFor(got, callAttempt{}), "definite outcome")
			assert.False(t, providerCallOutcomeKnownFor(got, callAttempt{resend: true}), "an answer to a re-send is never definite")
		})
	}
}
