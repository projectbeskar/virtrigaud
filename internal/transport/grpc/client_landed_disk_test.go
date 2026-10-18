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
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// landedDiskServer answers DeleteLandedDisk from a script and records the
// requests (rc2 R7).
type landedDiskServer struct {
	providerv1.UnimplementedProviderServer
	resp *providerv1.DeleteLandedDiskResponse
	err  error
	caps *providerv1.GetCapabilitiesResponse

	mu    sync.Mutex
	calls []*providerv1.DeleteLandedDiskRequest
}

func (s *landedDiskServer) DeleteLandedDisk(_ context.Context, r *providerv1.DeleteLandedDiskRequest) (*providerv1.DeleteLandedDiskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := proto.Clone(r).(*providerv1.DeleteLandedDiskRequest); ok {
		s.calls = append(s.calls, c)
	}
	return s.resp, s.err
}

func (s *landedDiskServer) GetCapabilities(context.Context, *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	if s.caps == nil {
		return &providerv1.GetCapabilitiesResponse{}, nil
	}
	return s.caps, nil
}

var landedReq = contracts.LandedDiskDeleteRequest{
	Migration: contracts.ObjectIdentity{UID: "uid-1", Namespace: "team-a", Name: "mig"},
	TargetVM:  contracts.ObjectIdentity{Namespace: "team-a", Name: "web"},
	Path:      "[ds1] team-a_web-migrated/team-a_web-migrated.vmdk",
}

// TestClient_DeleteLandedDisk_SendsTheTargetAndMapsTheOutcome: the request
// carries the migration, the target VM and the recorded path; every outcome
// maps, and the capability is reported.
func TestClient_DeleteLandedDisk_SendsTheTargetAndMapsTheOutcome(t *testing.T) {
	for outcome, want := range map[providerv1.LandedDiskDeleteOutcome]contracts.LandedDiskDeleteOutcome{
		providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_DELETED:   contracts.LandedDiskDeleted,
		providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_NOT_FOUND: contracts.LandedDiskNotFound,
	} {
		t.Run(outcome.String(), func(t *testing.T) {
			srv := &landedDiskServer{
				resp: &providerv1.DeleteLandedDiskResponse{Outcome: outcome},
				caps: &providerv1.GetCapabilitiesResponse{SupportsLandedDiskDelete: true},
			}
			dialer, cleanup := startBufconnServer(t, srv)
			defer cleanup()
			c := newTestClient(t, dialer, "vsphere")

			var deleter contracts.LandedDiskDeleter = c
			got, err := deleter.DeleteLandedDisk(context.Background(), landedReq)
			require.NoError(t, err)
			assert.Equal(t, want, got)

			srv.mu.Lock()
			require.Len(t, srv.calls, 1)
			r := srv.calls[0]
			srv.mu.Unlock()
			assert.Equal(t, "uid-1", r.GetMigration().GetUid())
			assert.Equal(t, "mig", r.GetMigration().GetName())
			assert.Equal(t, "team-a", r.GetTargetVm().GetNamespace())
			assert.Equal(t, "web", r.GetTargetVm().GetName())
			assert.Equal(t, landedReq.Path, r.GetPath())

			caps, err := c.GetCapabilities(context.Background())
			require.NoError(t, err)
			assert.True(t, caps.SupportsLandedDiskDelete)
		})
	}
}

// TestClient_DeleteLandedDisk_Errors: an older provider (Unimplemented) is
// NotSupported, a refused request is InvalidSpec, a disk in use is a Conflict
// marked ErrVMDiskInUse, and an answer without an outcome is an error.
func TestClient_DeleteLandedDisk_Errors(t *testing.T) {
	inUse, err := status.New(codes.FailedPrecondition, "in use").WithDetails(
		&errdetails.ErrorInfo{Reason: contracts.VMDiskInUseReason, Domain: contracts.ErrorInfoDomain})
	require.NoError(t, err)
	for name, tc := range map[string]struct {
		srv   *landedDiskServer
		check func(t *testing.T, err error)
	}{
		"older provider": {&landedDiskServer{err: status.Error(codes.Unimplemented, "method DeleteLandedDisk not implemented")}, func(t *testing.T, err error) {
			assert.True(t, contracts.IsNotSupported(err), "got %v", err)
		}},
		"refused": {&landedDiskServer{err: status.Error(codes.InvalidArgument, "bad")}, func(t *testing.T, err error) {
			assert.True(t, contracts.IsInvalidSpec(err), "got %v", err)
		}},
		"in use": {&landedDiskServer{err: inUse.Err()}, func(t *testing.T, err error) {
			assert.True(t, contracts.IsVMDiskInUse(err), "got %v", err)
			assert.False(t, contracts.IsInvalidSpec(err))
		}},
		"cannot delete in its configuration": {&landedDiskServer{err: status.Error(codes.FailedPrecondition, "no SSH data plane")}, func(t *testing.T, err error) {
			assert.True(t, contracts.IsInvalidSpec(err), "refused, never retried blindly: got %v", err)
			assert.False(t, contracts.IsVMDiskInUse(err))
		}},
		"no outcome": {&landedDiskServer{resp: &providerv1.DeleteLandedDiskResponse{}}, func(t *testing.T, err error) {
			assert.False(t, contracts.IsNotSupported(err))
			assert.False(t, contracts.IsVMDiskInUse(err))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			dialer, cleanup := startBufconnServer(t, tc.srv)
			defer cleanup()
			_, err := newTestClient(t, dialer, "libvirt").DeleteLandedDisk(context.Background(), landedReq)
			require.Error(t, err)
			tc.check(t, err)
		})
	}
}

// TestClient_GetCapabilities_LandedDiskDeleteFalseFromOlderProvider: a
// provider that predates the field does not report it.
func TestClient_GetCapabilities_LandedDiskDeleteFalseFromOlderProvider(t *testing.T) {
	dialer, cleanup := startBufconnServer(t, &landedDiskServer{})
	defer cleanup()
	caps, err := newTestClient(t, dialer, "libvirt").GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.False(t, caps.SupportsLandedDiskDelete)
}
