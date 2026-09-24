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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// routingRecorderServer records every per-VM request exactly as it arrived on
// the wire, so the tests below prove the manager threads the ADR-0007 Addendum A
// routing fields (target_host_id / source_host_id / owner) through a real gRPC
// round trip — and sends nothing for a single-host call.
type routingRecorderServer struct {
	providerv1.UnimplementedProviderServer
	mu   sync.Mutex
	reqs map[string]proto.Message
}

func (s *routingRecorderServer) record(name string, m proto.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reqs == nil {
		s.reqs = map[string]proto.Message{}
	}
	s.reqs[name] = proto.Clone(m)
}

func (s *routingRecorderServer) got(name string) proto.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[name]
}

func (s *routingRecorderServer) Delete(_ context.Context, r *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	s.record("Delete", r)
	return &providerv1.TaskResponse{}, nil
}

func (s *routingRecorderServer) Power(_ context.Context, r *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	s.record("Power", r)
	return &providerv1.TaskResponse{}, nil
}

func (s *routingRecorderServer) Reconfigure(_ context.Context, r *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	s.record("Reconfigure", r)
	return &providerv1.TaskResponse{}, nil
}

func (s *routingRecorderServer) Describe(_ context.Context, r *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	s.record("Describe", r)
	return &providerv1.DescribeResponse{Exists: true}, nil
}

func (s *routingRecorderServer) SnapshotCreate(_ context.Context, r *providerv1.SnapshotCreateRequest) (*providerv1.SnapshotCreateResponse, error) {
	s.record("SnapshotCreate", r)
	return &providerv1.SnapshotCreateResponse{SnapshotId: "s"}, nil
}

func (s *routingRecorderServer) SnapshotDelete(_ context.Context, r *providerv1.SnapshotDeleteRequest) (*providerv1.TaskResponse, error) {
	s.record("SnapshotDelete", r)
	return &providerv1.TaskResponse{}, nil
}

func (s *routingRecorderServer) SnapshotRevert(_ context.Context, r *providerv1.SnapshotRevertRequest) (*providerv1.TaskResponse, error) {
	s.record("SnapshotRevert", r)
	return &providerv1.TaskResponse{}, nil
}

func (s *routingRecorderServer) Clone(_ context.Context, r *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	s.record("Clone", r)
	return &providerv1.CloneResponse{TargetVmId: r.TargetName}, nil
}

func (s *routingRecorderServer) ExportDisk(_ context.Context, r *providerv1.ExportDiskRequest) (*providerv1.ExportDiskResponse, error) {
	s.record("ExportDisk", r)
	return &providerv1.ExportDiskResponse{}, nil
}

func (s *routingRecorderServer) GetDiskInfo(_ context.Context, r *providerv1.GetDiskInfoRequest) (*providerv1.GetDiskInfoResponse, error) {
	s.record("GetDiskInfo", r)
	return &providerv1.GetDiskInfoResponse{}, nil
}

// driveEveryPerVMCall issues every per-VM contracts call once for vm.
func driveEveryPerVMCall(t *testing.T, cli *Client, vm contracts.VMRef) {
	t.Helper()
	ctx := context.Background()
	_, err := cli.Delete(ctx, vm)
	require.NoError(t, err)
	_, err = cli.Power(ctx, vm, contracts.PowerOpOn)
	require.NoError(t, err)
	_, err = cli.Reconfigure(ctx, vm, contracts.CreateRequest{Name: vm.ID})
	require.NoError(t, err)
	_, err = cli.Describe(ctx, vm)
	require.NoError(t, err)
	_, err = cli.SnapshotCreate(ctx, contracts.SnapshotCreateRequest{VM: vm, NameHint: "s"})
	require.NoError(t, err)
	_, err = cli.SnapshotDelete(ctx, vm, "s")
	require.NoError(t, err)
	_, err = cli.SnapshotRevert(ctx, vm, "s")
	require.NoError(t, err)
	_, err = cli.Clone(ctx, contracts.CloneRequest{Source: vm, TargetName: "copy"})
	require.NoError(t, err)
	_, err = cli.ExportDisk(ctx, contracts.ExportDiskRequest{VM: vm})
	require.NoError(t, err)
	_, err = cli.GetDiskInfo(ctx, contracts.GetDiskInfoRequest{VM: vm})
	require.NoError(t, err)
}

// wireRouting extracts (id, host) from each recorded request.
func wireRouting(t *testing.T, srv *routingRecorderServer) map[string][2]string {
	t.Helper()
	out := map[string][2]string{}
	for _, name := range []string{"Delete", "Power", "Reconfigure", "Describe", "SnapshotCreate",
		"SnapshotDelete", "SnapshotRevert", "Clone", "ExportDisk", "GetDiskInfo"} {
		m := srv.got(name)
		require.NotNil(t, m, "%s never reached the server", name)
		var id, host string
		switch r := m.(type) {
		case *providerv1.DeleteRequest:
			id, host = r.Id, r.TargetHostId
		case *providerv1.PowerRequest:
			id, host = r.Id, r.TargetHostId
		case *providerv1.ReconfigureRequest:
			id, host = r.Id, r.TargetHostId
		case *providerv1.DescribeRequest:
			id, host = r.Id, r.TargetHostId
		case *providerv1.SnapshotCreateRequest:
			id, host = r.VmId, r.TargetHostId
		case *providerv1.SnapshotDeleteRequest:
			id, host = r.VmId, r.TargetHostId
		case *providerv1.SnapshotRevertRequest:
			id, host = r.VmId, r.TargetHostId
		case *providerv1.CloneRequest:
			id, host = r.SourceVmId, r.SourceHostId
		case *providerv1.ExportDiskRequest:
			id, host = r.VmId, r.TargetHostId
		case *providerv1.GetDiskInfoRequest:
			id, host = r.VmId, r.TargetHostId
		default:
			t.Fatalf("unexpected recorded request %T", m)
		}
		out[name] = [2]string{id, host}
	}
	return out
}

// recordedDelete returns the DeleteRequest the server received.
func recordedDelete(t *testing.T, srv *routingRecorderServer) *providerv1.DeleteRequest {
	t.Helper()
	del, ok := srv.got("Delete").(*providerv1.DeleteRequest)
	require.True(t, ok, "Delete never reached the server")
	return del
}

// recordedDescribe returns the DescribeRequest the server received.
func recordedDescribe(t *testing.T, srv *routingRecorderServer) *providerv1.DescribeRequest {
	t.Helper()
	d, ok := srv.got("Describe").(*providerv1.DescribeRequest)
	require.True(t, ok, "Describe never reached the server")
	return d
}

// recordedPower returns the PowerRequest the server received.
func recordedPower(t *testing.T, srv *routingRecorderServer) *providerv1.PowerRequest {
	t.Helper()
	p, ok := srv.got("Power").(*providerv1.PowerRequest)
	require.True(t, ok, "Power never reached the server")
	return p
}

// recordedReconfigure returns the ReconfigureRequest the server received.
func recordedReconfigure(t *testing.T, srv *routingRecorderServer) *providerv1.ReconfigureRequest {
	t.Helper()
	r, ok := srv.got("Reconfigure").(*providerv1.ReconfigureRequest)
	require.True(t, ok, "Reconfigure never reached the server")
	return r
}

// routedOwners returns the owner every owner-carrying per-VM request arrived
// with (Describe and Delete from slice 1; Power and Reconfigure from slice 2).
func routedOwners(t *testing.T, srv *routingRecorderServer) map[string]*providerv1.ObjectIdentity {
	t.Helper()
	return map[string]*providerv1.ObjectIdentity{
		"Delete":      recordedDelete(t, srv).Owner,
		"Describe":    recordedDescribe(t, srv).Owner,
		"Power":       recordedPower(t, srv).Owner,
		"Reconfigure": recordedReconfigure(t, srv).Owner,
	}
}

// TestClient_PerVMCalls_ThreadHostAndOwner is the transport round trip for a
// clustered VM: every per-VM request carries the host (source_host_id for a
// clone source), and Describe, Delete, Power and Reconfigure carry the owner
// the provider checks against the VM's owner stamp.
func TestClient_PerVMCalls_ThreadHostAndOwner(t *testing.T) {
	srv := &routingRecorderServer{}
	cli := newTestClientForVMOps(t, srv, "libvirt", "routing")
	owner := contracts.ObjectIdentity{UID: "uid-1", Namespace: "team-a", Name: "web"}

	driveEveryPerVMCall(t, cli, contracts.VMRef{ID: "web", HostID: "host-a", Owner: owner})

	for name, idHost := range wireRouting(t, srv) {
		assert.Equal(t, [2]string{"web", "host-a"}, idHost, "%s must carry the VM id and its host", name)
	}
	for name, got := range routedOwners(t, srv) {
		require.NotNil(t, got, "%s must carry the owner", name)
		assert.Equal(t, "uid-1", got.Uid, name)
		assert.Equal(t, "team-a", got.Namespace, name)
		assert.Equal(t, "web", got.Name, name)
	}
}

// TestClient_PerVMCalls_SingleHostSendsNoHost proves D9 on the wire: a
// single-host VMRef (empty HostID) puts nothing in target_host_id /
// source_host_id, and no owner is sent — even if one were set — so single-host
// requests are exactly what they were before routing.
func TestClient_PerVMCalls_SingleHostSendsNoHost(t *testing.T) {
	srv := &routingRecorderServer{}
	cli := newTestClientForVMOps(t, srv, "libvirt", "single")

	driveEveryPerVMCall(t, cli, contracts.VMRef{ID: "legacy", Owner: contracts.ObjectIdentity{UID: "uid-x", Namespace: "ns", Name: "legacy"}})

	for name, idHost := range wireRouting(t, srv) {
		assert.Equal(t, [2]string{"legacy", ""}, idHost, "%s must carry no host for a single-host VM", name)
	}
	for name, got := range routedOwners(t, srv) {
		assert.Nil(t, got, "a single-host %s carries no owner", name)
	}
}

// TestClient_RoutedOwnerWithoutUIDIsNotSent pins that an owner with no UID is
// never put on the wire, even on a routed call: it could authorize nothing.
func TestClient_RoutedOwnerWithoutUIDIsNotSent(t *testing.T) {
	srv := &routingRecorderServer{}
	cli := newTestClientForVMOps(t, srv, "libvirt", "routing-nouid")
	driveEveryPerVMCall(t, cli, contracts.VMRef{ID: "web", HostID: "host-a", Owner: contracts.ObjectIdentity{Namespace: "ns", Name: "web"}})
	for name, got := range routedOwners(t, srv) {
		assert.Nil(t, got, "%s: an owner without a UID is never sent", name)
	}
}
