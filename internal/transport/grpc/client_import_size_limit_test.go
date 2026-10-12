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
	"github.com/projectbeskar/virtrigaud/internal/providers/importlimit"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// sizeLimitImportServer records ImportDisk and answers it with refusal.
type sizeLimitImportServer struct {
	routingRecorderServer
	refusal error
}

func (s *sizeLimitImportServer) ImportDisk(_ context.Context, r *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	s.record("ImportDisk", r)
	if s.refusal != nil {
		return nil, s.refusal
	}
	return &providerv1.ImportDiskResponse{DiskId: "d", Path: "/p"}, nil
}

// TestClient_ImportDiskSizeLimitOnTheWire: the limit travels as
// max_virtual_size_bytes, and a provider's DISK_SIZE_EXCEEDS_LIMIT or
// DISK_SIZE_UNVERIFIABLE refusal comes back marked (a non-retryable Conflict),
// never counted toward the per-Provider circuit breaker.
func TestClient_ImportDiskSizeLimitOnTheWire(t *testing.T) {
	ctx := context.Background()
	exceeds := importlimit.Check(2<<40, nil, 100<<30)
	unverifiable := importlimit.Check(0, nil, 100<<30)
	require.Error(t, exceeds)
	require.Error(t, unverifiable)

	srv := &sizeLimitImportServer{}
	cli := newTestClientForVMOps(t, srv, "libvirt", "import-limit")
	_, err := cli.ImportDisk(ctx, contracts.ImportDiskRequest{SourceURL: "s3://b/o", MaxVirtualSizeBytes: 100 << 30})
	require.NoError(t, err)
	imp, ok := srv.got("ImportDisk").(*providerv1.ImportDiskRequest)
	require.True(t, ok)
	assert.Equal(t, int64(100)<<30, imp.GetMaxVirtualSizeBytes())

	for name, tc := range map[string]struct {
		refusal error
		is      func(error) bool
	}{
		"exceeds":      {exceeds, contracts.IsDiskSizeExceedsLimit},
		"unverifiable": {unverifiable, contracts.IsDiskSizeUnverifiable},
	} {
		t.Run(name, func(t *testing.T) {
			srv := &sizeLimitImportServer{refusal: tc.refusal}
			cli := newTestClientForVMOps(t, srv, "libvirt", "import-limit-"+name)
			_, err := cli.ImportDisk(ctx, contracts.ImportDiskRequest{SourceURL: "s3://b/o", MaxVirtualSizeBytes: 100 << 30})
			require.Error(t, err)
			assert.True(t, tc.is(err), "marked: %v", err)
			assert.True(t, contracts.IsConflict(err))
			assert.False(t, contracts.IsRetryable(err), "nothing was landed; not retried as a transient failure")
			assert.False(t, isInfraFailure(tc.refusal))
			assert.False(t, countsTowardBreaker(providerv1.Provider_ImportDisk_FullMethodName, tc.refusal))
		})
	}
}
