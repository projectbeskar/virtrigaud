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

package vsphere

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/simulator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin, against vcsim, that deleting a snapshot that is not there
// is answered gRPC NotFound — the manager's idempotent "already gone" — and
// never an Unknown error a VMSnapshot or VMMigration finalizer would retry
// forever.

// newSnapshotSim returns a Provider on a fresh vcsim VPX model and the MOID of
// one of its seeded VMs.
func newSnapshotSim(t *testing.T) (*Provider, string) {
	t.Helper()
	model := simulator.VPX()
	require.NoError(t, model.Create())
	server := model.Service.NewServer()
	u := server.URL
	pw, _ := u.User.Password()
	cfg := &Config{
		Endpoint:           (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String(),
		Username:           u.User.Username(),
		Password:           pw,
		InsecureSkipVerify: true,
	}
	client, finder, err := createVSphereClient(cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = client.Logout(context.Background())
		server.Close()
		model.Remove()
	})
	p := &Provider{client: client, finder: finder, config: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	ctx := context.Background()
	dc, err := finder.DefaultDatacenter(ctx)
	require.NoError(t, err)
	finder.SetDatacenter(dc)
	vms, err := finder.VirtualMachineList(ctx, "*")
	require.NoError(t, err)
	require.NotEmpty(t, vms)
	return p, vms[0].Reference().Value
}

func TestSnapshotDelete_MissingSnapshotIsNotFound(t *testing.T) {
	ctx := context.Background()
	p, vmID := newSnapshotSim(t)

	t.Run("VM has no snapshots", func(t *testing.T) {
		_, err := p.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: vmID, SnapshotId: "snapshot-404"})
		assert.Equal(t, codes.NotFound, status.Code(err), "%v", err)
	})

	created, err := p.SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{VmId: vmID, NameHint: "mig-snap"})
	require.NoError(t, err)
	other, err := p.SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{VmId: vmID, NameHint: "keep"})
	require.NoError(t, err)

	t.Run("snapshot not in the tree", func(t *testing.T) {
		_, err := p.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: vmID, SnapshotId: "snapshot-404"})
		assert.Equal(t, codes.NotFound, status.Code(err), "%v", err)
	})

	t.Run("deleted, then deleted again", func(t *testing.T) {
		_, err := p.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: vmID, SnapshotId: created.SnapshotId})
		require.NoError(t, err)
		_, err = p.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: vmID, SnapshotId: created.SnapshotId})
		assert.Equal(t, codes.NotFound, status.Code(err), "a repeated delete is the manager's idempotent success: %v", err)
	})

	t.Run("VM gone", func(t *testing.T) {
		_, err := p.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "vm-does-not-exist", SnapshotId: other.SnapshotId})
		assert.Equal(t, codes.NotFound, status.Code(err), "%v", err)
	})

	t.Run("revert to a missing snapshot", func(t *testing.T) {
		_, err := p.SnapshotRevert(ctx, &providerv1.SnapshotRevertRequest{VmId: vmID, SnapshotId: "snapshot-404"})
		assert.Equal(t, codes.NotFound, status.Code(err), "%v", err)
	})
}
