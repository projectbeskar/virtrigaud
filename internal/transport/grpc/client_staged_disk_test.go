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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// stagedDiskServer answers DeleteStagedDisk from a script and records the
// requests (ADR-0011).
type stagedDiskServer struct {
	providerv1.UnimplementedProviderServer
	resp *providerv1.DeleteStagedDiskResponse
	err  error
	caps *providerv1.GetCapabilitiesResponse

	mu    sync.Mutex
	calls []*providerv1.DeleteStagedDiskRequest
}

func (s *stagedDiskServer) DeleteStagedDisk(_ context.Context, r *providerv1.DeleteStagedDiskRequest) (*providerv1.DeleteStagedDiskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := proto.Clone(r).(*providerv1.DeleteStagedDiskRequest); ok {
		s.calls = append(s.calls, c)
	}
	return s.resp, s.err
}

func (s *stagedDiskServer) GetCapabilities(context.Context, *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	if s.caps == nil {
		return &providerv1.GetCapabilitiesResponse{}, nil
	}
	return s.caps, nil
}

var stagedReq = contracts.StagedDiskDeleteRequest{
	Migration:          contracts.ObjectIdentity{UID: "uid-1", Namespace: "team-a", Name: "web"},
	Stage:              "export",
	BackendType:        "s3",
	StorageOptionsJSON: `{"backend":"s3","bucket":"b"}`,
	Credentials:        map[string]string{"accessKeyID": "AK", "secretAccessKey": "SK"},
	Layout:             "v2",
}

// TestClient_DeleteStagedDisk_SendsTheMigrationAndMapsTheOutcome: the request
// carries the migration identity, stage, backend, options and credentials, and
// every outcome maps.
func TestClient_DeleteStagedDisk_SendsTheMigrationAndMapsTheOutcome(t *testing.T) {
	for outcome, want := range map[providerv1.StagedDiskDeleteOutcome]contracts.StagedDiskDeleteOutcome{
		providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_DELETED:   contracts.StagedDiskDeleted,
		providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_NOT_FOUND: contracts.StagedDiskNotFound,
		providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_EMPTIED:   contracts.StagedDiskEmptied,
	} {
		t.Run(outcome.String(), func(t *testing.T) {
			srv := &stagedDiskServer{
				resp: &providerv1.DeleteStagedDiskResponse{Outcome: outcome},
				caps: &providerv1.GetCapabilitiesResponse{SupportedStagedDiskDeleteBackends: []string{"nfs", "s3"}},
			}
			dialer, cleanup := startBufconnServer(t, srv)
			defer cleanup()
			c := newTestClient(t, dialer, "libvirt")

			var deleter contracts.StagedDiskDeleter = c
			got, err := deleter.DeleteStagedDisk(context.Background(), stagedReq)
			require.NoError(t, err)
			assert.Equal(t, want, got)

			srv.mu.Lock()
			require.Len(t, srv.calls, 1)
			r := srv.calls[0]
			srv.mu.Unlock()
			assert.Equal(t, "uid-1", r.GetMigration().GetUid())
			assert.Equal(t, "team-a", r.GetMigration().GetNamespace())
			assert.Equal(t, "web", r.GetMigration().GetName())
			assert.Equal(t, "export", r.GetStage())
			assert.Equal(t, "s3", r.GetBackendType())
			assert.Equal(t, stagedReq.StorageOptionsJSON, r.GetStorageOptionsJson())
			assert.Equal(t, stagedReq.Credentials, r.GetCredentials())
			assert.Equal(t, "v2", r.GetLayout())

			caps, err := c.GetCapabilities(context.Background())
			require.NoError(t, err)
			assert.Equal(t, []string{"nfs", "s3"}, caps.SupportedStagedDiskDeleteBackends)
		})
	}
}

// TestClient_DeleteStagedDisk_Errors: an older provider (Unimplemented) is
// NotSupported, a refused request is InvalidSpec, a provider-side NotFound
// stays NotFound, and an answer without an outcome is an error.
func TestClient_DeleteStagedDisk_Errors(t *testing.T) {
	for name, tc := range map[string]struct {
		srv   *stagedDiskServer
		check func(t *testing.T, err error)
	}{
		"older provider": {&stagedDiskServer{err: status.Error(codes.Unimplemented, "method DeleteStagedDisk not implemented")}, func(t *testing.T, err error) {
			assert.True(t, contracts.IsNotSupported(err), "got %v", err)
		}},
		"refused": {&stagedDiskServer{err: status.Error(codes.InvalidArgument, "bad")}, func(t *testing.T, err error) {
			assert.True(t, contracts.IsInvalidSpec(err), "got %v", err)
		}},
		"not found": {&stagedDiskServer{err: status.Error(codes.NotFound, "gone")}, func(t *testing.T, err error) {
			assert.True(t, contracts.IsNotFound(err), "got %v", err)
		}},
		"no outcome": {&stagedDiskServer{resp: &providerv1.DeleteStagedDiskResponse{}}, func(t *testing.T, err error) {
			assert.False(t, contracts.IsNotFound(err))
			assert.False(t, contracts.IsNotSupported(err))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			dialer, cleanup := startBufconnServer(t, tc.srv)
			defer cleanup()
			c := newTestClient(t, dialer, "libvirt")
			_, err := c.DeleteStagedDisk(context.Background(), stagedReq)
			require.Error(t, err)
			tc.check(t, err)
		})
	}
}

// TestClient_GetCapabilities_StagedDiskDeleteEmptyFromOlderProvider: a provider
// that predates the field reports no backends.
func TestClient_GetCapabilities_StagedDiskDeleteEmptyFromOlderProvider(t *testing.T) {
	dialer, cleanup := startBufconnServer(t, &stagedDiskServer{})
	defer cleanup()
	c := newTestClient(t, dialer, "libvirt")
	caps, err := c.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.Empty(t, caps.SupportedStagedDiskDeleteBackends)
}
