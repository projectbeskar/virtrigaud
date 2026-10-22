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

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the manager side of the ADR-0007 Addendum B wire (R11,
// slices 1 and 2): the routed import's target host and target VM UID, the
// recorded host of the landed and staged disk deletes, and the
// supports_routed_import capability all cross the wire unchanged, and are
// absent when the caller does not set them (a single-host or thin-client
// target).

const routedImportUID = "0b9c3a52-1f4e-4c55-9d1e-7b2f7f0e5a11"

// TestClient_ImportDisk_RoutedTargetOnTheWire: a clustered target's import
// carries its pending host and the target VM's UID; a single-host target's
// carries neither.
func TestClient_ImportDisk_RoutedTargetOnTheWire(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		req      contracts.ImportDiskRequest
		wantHost string
		wantUID  string
	}{
		"clustered target": {
			req: contracts.ImportDiskRequest{SourceURL: "s3://b/o", BackendType: "s3", TargetHostID: "host-b",
				TargetVM: contracts.ObjectIdentity{UID: routedImportUID, Namespace: "team-a", Name: "web"}},
			wantHost: "host-b", wantUID: routedImportUID,
		},
		"single-host target": {
			req: contracts.ImportDiskRequest{SourceURL: "s3://b/o", BackendType: "s3",
				TargetVM: contracts.ObjectIdentity{Namespace: "team-a", Name: "web"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := &targetVMRecorderServer{}
			cli := newTestClientForVMOps(t, srv, "libvirt", "routed-import")
			_, err := cli.ImportDisk(ctx, tc.req)
			require.NoError(t, err)
			imp, ok := srv.got("ImportDisk").(*providerv1.ImportDiskRequest)
			require.True(t, ok)
			assert.Equal(t, tc.wantHost, imp.GetTargetHostId())
			assert.Equal(t, tc.wantUID, imp.GetTargetVm().GetUid())
			assert.Equal(t, "team-a", imp.GetTargetVm().GetNamespace())
			assert.Equal(t, "web", imp.GetTargetVm().GetName())
		})
	}
}

// TestClient_DeleteLandedDisk_RoutedHostOnTheWire: the recorded import host
// reaches DeleteLandedDiskRequest.target_host_id; none is sent when unset.
func TestClient_DeleteLandedDisk_RoutedHostOnTheWire(t *testing.T) {
	for name, host := range map[string]string{"clustered": "host-b", "single-host": ""} {
		t.Run(name, func(t *testing.T) {
			srv := &landedDiskServer{resp: &providerv1.DeleteLandedDiskResponse{
				Outcome: providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_DELETED}}
			dialer, cleanup := startBufconnServer(t, srv)
			defer cleanup()
			req := landedReq
			req.TargetHostID = host
			_, err := newTestClient(t, dialer, "libvirt").DeleteLandedDisk(context.Background(), req)
			require.NoError(t, err)
			srv.mu.Lock()
			defer srv.mu.Unlock()
			require.Len(t, srv.calls, 1)
			assert.Equal(t, host, srv.calls[0].GetTargetHostId())
		})
	}
}

// TestClient_DeleteStagedDisk_RoutedHostOnTheWire: the recorded import host
// reaches DeleteStagedDiskRequest.target_host_id; none is sent when unset.
func TestClient_DeleteStagedDisk_RoutedHostOnTheWire(t *testing.T) {
	for name, host := range map[string]string{"clustered": "host-b", "single-host": ""} {
		t.Run(name, func(t *testing.T) {
			srv := &stagedDiskServer{resp: &providerv1.DeleteStagedDiskResponse{
				Outcome: providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_EMPTIED}}
			dialer, cleanup := startBufconnServer(t, srv)
			defer cleanup()
			req := stagedReq
			req.BackendType = "nfs"
			req.TargetHostID = host
			_, err := newTestClient(t, dialer, "libvirt").DeleteStagedDisk(context.Background(), req)
			require.NoError(t, err)
			srv.mu.Lock()
			defer srv.mu.Unlock()
			require.Len(t, srv.calls, 1)
			assert.Equal(t, host, srv.calls[0].GetTargetHostId())
		})
	}
}

// TestClient_GetCapabilities_RoutedImport: supports_routed_import reaches
// Capabilities.SupportsRoutedImport, and is false from a provider that
// predates it.
func TestClient_GetCapabilities_RoutedImport(t *testing.T) {
	for name, tc := range map[string]struct {
		caps *providerv1.GetCapabilitiesResponse
		want bool
	}{
		"reported":       {&providerv1.GetCapabilitiesResponse{SupportsRoutedImport: true, SupportsClustering: true}, true},
		"older provider": {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			dialer, cleanup := startBufconnServer(t, &landedDiskServer{caps: tc.caps})
			defer cleanup()
			caps, err := newTestClient(t, dialer, "libvirt").GetCapabilities(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tc.want, caps.SupportsRoutedImport)
		})
	}
}
