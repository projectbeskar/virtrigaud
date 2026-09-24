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

// targetVMRecorderServer also records ImportDisk, so the round trip of the
// target VM identity (CloneRequest.target_vm, ImportDiskRequest.target_vm) can
// be checked on the wire.
type targetVMRecorderServer struct {
	routingRecorderServer
}

func (s *targetVMRecorderServer) ImportDisk(_ context.Context, r *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	s.record("ImportDisk", r)
	return &providerv1.ImportDiskResponse{DiskId: "d", Path: "/p"}, nil
}

// TestClient_TargetVMOnTheWire: the manager threads the identity of the
// VirtualMachine a clone or a migration import is for — namespace and name,
// without a UID (it does not exist yet) — so the provider can name the
// hypervisor-side object with its own rule. A partial identity is never sent.
func TestClient_TargetVMOnTheWire(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		target contracts.ObjectIdentity
		want   *providerv1.ObjectIdentity
	}{
		{"namespace and name", contracts.ObjectIdentity{Namespace: "team-a", Name: "web"},
			&providerv1.ObjectIdentity{Namespace: "team-a", Name: "web"}},
		{"none (older caller)", contracts.ObjectIdentity{}, nil},
		{"namespace only", contracts.ObjectIdentity{Namespace: "team-a"}, nil},
		{"name only", contracts.ObjectIdentity{Name: "web"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &targetVMRecorderServer{}
			cli := newTestClientForVMOps(t, srv, "libvirt", "target-vm")

			_, err := cli.Clone(ctx, contracts.CloneRequest{Source: contracts.VMRef{ID: "src"}, TargetName: "web", TargetVM: tc.target})
			require.NoError(t, err)
			_, err = cli.ImportDisk(ctx, contracts.ImportDiskRequest{SourceURL: "s3://b/o", TargetName: "web-migrated", TargetVM: tc.target})
			require.NoError(t, err)

			clone, ok := srv.got("Clone").(*providerv1.CloneRequest)
			require.True(t, ok)
			imp, ok := srv.got("ImportDisk").(*providerv1.ImportDiskRequest)
			require.True(t, ok)
			for rpc, got := range map[string]*providerv1.ObjectIdentity{"Clone": clone.GetTargetVm(), "ImportDisk": imp.GetTargetVm()} {
				if tc.want == nil {
					assert.Nil(t, got, rpc)
					continue
				}
				require.NotNil(t, got, rpc)
				assert.Equal(t, tc.want.GetNamespace(), got.GetNamespace(), rpc)
				assert.Equal(t, tc.want.GetName(), got.GetName(), rpc)
				assert.Empty(t, got.GetUid(), rpc)
			}
			assert.Equal(t, "web", clone.GetTargetName(), "target_name is unchanged")
			assert.Equal(t, "web-migrated", imp.GetTargetName(), "target_name is unchanged")
		})
	}
}
