/*
Copyright 2025.

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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestProxmoxProvider_CloneAppliesVMClassSizing is the #261 P1-2 fix: the Clone
// RPC applies the VMClass CPU/memory override on top of the source instead of
// silently inheriting it. The seeded source (vmid 100) is 2 CPU / 2048 MiB.
func TestProxmoxProvider_CloneAppliesVMClassSizing(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	cloneResp, err := provider.Clone(ctx, &providerv1.CloneRequest{
		SourceVmId: "100",
		TargetName: "clone-sized",
		ClassJson:  `{"CPU": 4, "MemoryMiB": 8192}`,
	})
	require.NoError(t, err)
	require.NotEmpty(t, cloneResp.TargetVmId)

	cores, memMiB := vmConfigSize(t, provider, cloneResp.TargetVmId)
	assert.Equal(t, 4, cores, "Clone must apply the VMClass CPU override")
	assert.Equal(t, 8192, memMiB, "Clone must apply the VMClass MemoryMiB override")
}

// TestProxmoxProvider_PowerGracefulForwardsTimeout verifies the #261 P1-4 fix: a
// graceful shutdown forwards the caller's timeout and asks PVE to escalate to a
// hard stop (forceStop=1) when it elapses.
func TestProxmoxProvider_PowerGracefulForwardsTimeout(t *testing.T) {
	srv, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)

	_, err = provider.Power(context.Background(), &providerv1.PowerRequest{
		Id:                     "100",
		Op:                     providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL,
		GracefulTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	last := srv.LastPowerOp()
	require.NotNil(t, last)
	assert.Equal(t, "shutdown", last.Operation)
	assert.Equal(t, "30", last.Timeout, "graceful shutdown must forward the timeout")
	assert.Equal(t, "1", last.ForceStop, "graceful shutdown must escalate to a hard stop")
}

// TestProxmoxProvider_PowerGracefulNoTimeout verifies that without a timeout the
// shutdown is issued with no timeout/forceStop params (PVE's default behavior).
func TestProxmoxProvider_PowerGracefulNoTimeout(t *testing.T) {
	srv, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)

	_, err = provider.Power(context.Background(), &providerv1.PowerRequest{
		Id: "100",
		Op: providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL,
	})
	require.NoError(t, err)

	last := srv.LastPowerOp()
	require.NotNil(t, last)
	assert.Equal(t, "shutdown", last.Operation)
	assert.Empty(t, last.Timeout, "no timeout requested ⇒ none forwarded")
	assert.Empty(t, last.ForceStop)
}

// TestProxmoxProvider_NodeDiscovery is the #261 P1-4 fix: with no NodeSelector
// configured the client discovers nodes from the cluster API instead of assuming
// "pve". The fake reports two nodes (incl. "pve2"), which the legacy hardcode
// could never produce.
func TestProxmoxProvider_NodeDiscovery(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint) // no NodeSelector configured
	ctx := context.Background()

	nodes, err := provider.client.ListNodes(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"pve", "pve2"}, nodes, "must discover the cluster's nodes via /nodes")

	node, err := provider.client.FindNode(ctx)
	require.NoError(t, err)
	assert.Equal(t, "pve", node, "FindNode returns the first discovered node, not a hardcoded default")
}
