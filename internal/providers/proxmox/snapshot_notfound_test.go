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

package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin, against pvefake, when a Proxmox SnapshotDelete answers
// gRPC NotFound — the manager's idempotent "already gone", which releases a
// VMMigration or VMSnapshot — and when it must not: only a snapshot that is
// provably absent (not listed on the VM's ACTUAL node, or its VM absent
// cluster-wide) is NotFound. A VM moved to another node keeps its snapshot
// and its delete; a bare 404 is an error, never NotFound.

// takeSnapshot creates snapshot hint on VM 100 and waits for its task.
func takeSnapshot(t *testing.T, provider *Provider, hint string) string {
	t.Helper()
	ctx := context.Background()
	created, err := provider.SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{VmId: "100", NameHint: hint})
	require.NoError(t, err)
	if created.Task != nil {
		require.NoError(t, waitForTask(ctx, provider, created.Task.Id))
	}
	return created.SnapshotId
}

func TestSnapshotDelete_MissingSnapshotIsNotFound(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	t.Run("never taken", func(t *testing.T) {
		_, err := provider.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "100", SnapshotId: "mig-404"})
		assert.Equal(t, codes.NotFound, status.Code(err), "%v", err)
	})

	t.Run("the running-state pseudo-entry is not a snapshot", func(t *testing.T) {
		_, err := provider.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "100", SnapshotId: "current"})
		assert.Equal(t, codes.NotFound, status.Code(err), "%v", err)
	})

	t.Run("deleted, then deleted again", func(t *testing.T) {
		id := takeSnapshot(t, provider, "mig-snap")
		resp, err := provider.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "100", SnapshotId: id})
		require.NoError(t, err)
		if resp.Task != nil {
			require.NoError(t, waitForTask(ctx, provider, resp.Task.Id))
		}
		_, err = provider.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "100", SnapshotId: id})
		assert.Equal(t, codes.NotFound, status.Code(err), "a repeated delete is the manager's idempotent success: %v", err)
	})

	t.Run("VM absent cluster-wide", func(t *testing.T) {
		_, err := provider.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "999", SnapshotId: "mig-snap"})
		assert.Equal(t, codes.NotFound, status.Code(err), "%v", err)
	})
}

// TestSnapshotDelete_VMOnAnotherNodeKeepsItsSnapshot is the review's H-1: a
// VM moved to another node (HA) is answered "Configuration file ... does not
// exist" by the node the provider would otherwise pick. That is no proof the
// snapshot is gone: the delete goes to the VM's actual node and deletes it.
func TestSnapshotDelete_VMOnAnotherNodeKeepsItsSnapshot(t *testing.T) {
	fake, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	id := takeSnapshot(t, provider, "mig-ha")
	fake.MoveVM(100, "pve2") // the provider's first node is "pve"

	resp, err := provider.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "100", SnapshotId: id})
	require.NoError(t, err, "the snapshot exists on the VM's node: it is deleted there, never answered NotFound")
	if resp.Task != nil {
		require.NoError(t, waitForTask(ctx, provider, resp.Task.Id))
	}
	_, err = provider.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "100", SnapshotId: id})
	assert.Equal(t, codes.NotFound, status.Code(err), "now really gone: %v", err)
}

// TestSnapshotDelete_VMInvisibleToTheTokenIsNotAbsent (re-review M-1): PVE
// filters /cluster/resources by the token's VM.Audit privilege, so a VM
// missing from it may only be invisible. The VMID probe (/cluster/nextid)
// says it is in use: the snapshot is not answered NotFound.
func TestSnapshotDelete_VMInvisibleToTheTokenIsNotAbsent(t *testing.T) {
	fake, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	id := takeSnapshot(t, provider, "mig-acl")
	fake.HideFromResources(100)

	_, err = provider.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "100", SnapshotId: id})
	require.Error(t, err)
	assert.NotEqual(t, codes.NotFound, status.Code(err), "an existing VM's snapshot is never 'gone': %v", err)
	assert.Equal(t, codes.Unavailable, status.Code(err))
}

// TestSnapshotDelete_BareNotFoundIsAnError: an HTTP 404 that is not PVE's
// answer (no PVE JSON body — e.g. a proxy) is an error the manager retries,
// never NotFound.
func TestSnapshotDelete_BareNotFoundIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/cluster/resources"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"qemu/100","type":"qemu","vmid":100,"node":"pve"}]}`))
		case strings.HasSuffix(r.URL.Path, "/nodes"):
			_, _ = w.Write([]byte(`{"data":[{"node":"pve"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	provider := createTestProvider(srv.URL)

	_, err := provider.SnapshotDelete(context.Background(), &providerv1.SnapshotDeleteRequest{VmId: "100", SnapshotId: "mig-snap"})
	require.Error(t, err)
	assert.NotEqual(t, codes.NotFound, status.Code(err), "%v", err)
	assert.Equal(t, codes.Unavailable, status.Code(err), "retried by the manager: %v", err)
}
