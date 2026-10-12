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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// powerOpsSent counts the power-operation POSTs the fake PVE received.
func powerOpsSent(srv *pvefake.Server) int {
	n := 0
	for _, r := range srv.MutatingRequests() {
		if r.Method == http.MethodPost && containsStatusOp(r.Path) {
			n++
		}
	}
	return n
}

// containsStatusOp reports whether path is a qemu status/<op> power endpoint.
func containsStatusOp(path string) bool {
	for _, op := range []string{"/status/start", "/status/stop", "/status/shutdown", "/status/reboot"} {
		if strings.HasSuffix(path, op) {
			return true
		}
	}
	return false
}

// vmStatus reads VM vmid's status from the fake PVE.
func vmStatus(t *testing.T, srv *pvefake.Server, vmid int) string {
	t.Helper()
	vm, ok := srv.GetVM(vmid)
	require.True(t, ok)
	return vm.Status
}

// TestProxmoxProvider_PowerIdempotentInRequestedState: Power is idempotent (v0.4.0-rc1
// F3). A VM already in the requested state is success with no PVE power
// operation; a failed operation re-reads the status and succeeds when the VM
// got there anyway (another actor won the race); a failure that left the VM
// elsewhere stands.
func TestProxmoxProvider_PowerIdempotentInRequestedState(t *testing.T) {
	ctx := context.Background()
	srv, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	p := createTestProvider(endpoint)
	power := func(op providerv1.PowerOp) error {
		_, err := p.Power(ctx, &providerv1.PowerRequest{Id: "100", Op: op})
		return err
	}
	require.Equal(t, "running", vmStatus(t, srv, 100))

	require.NoError(t, power(providerv1.PowerOp_POWER_OP_ON), "a start of a running VM succeeds")
	assert.Zero(t, powerOpsSent(srv), "and sends no start")

	require.NoError(t, power(providerv1.PowerOp_POWER_OP_OFF))
	assert.Equal(t, 1, powerOpsSent(srv))
	assert.Equal(t, "stopped", vmStatus(t, srv, 100))

	require.NoError(t, power(providerv1.PowerOp_POWER_OP_OFF), "a stop of a stopped VM succeeds")
	require.NoError(t, power(providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL), "a shutdown of a stopped VM succeeds")
	assert.Equal(t, 1, powerOpsSent(srv), "with no further power operation")

	require.NoError(t, power(providerv1.PowerOp_POWER_OP_ON))
	assert.Equal(t, "running", vmStatus(t, srv, 100))
}

// TestProxmoxProvider_PowerRaceAndRealFailure: a stop that PVE applied but
// answered with an error (the race) succeeds; one that left the VM running
// fails.
func TestProxmoxProvider_PowerRaceAndRealFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("applied, then answered an error", func(t *testing.T) {
		srv, endpoint, err := pvefake.StartFakeServer()
		require.NoError(t, err)
		srv.InjectFault(pvefake.Fault{Method: http.MethodPost, PathContains: "/qemu/100/status/stop",
			Status: http.StatusInternalServerError, Body: "VM 100 not running", AfterApply: true})
		p := createTestProvider(endpoint)
		_, err = p.Power(ctx, &providerv1.PowerRequest{Id: "100", Op: providerv1.PowerOp_POWER_OP_OFF})
		require.NoError(t, err)
		assert.Equal(t, "stopped", vmStatus(t, srv, 100))
	})

	t.Run("not applied", func(t *testing.T) {
		srv, endpoint, err := pvefake.StartFakeServer()
		require.NoError(t, err)
		srv.InjectFault(pvefake.Fault{Method: http.MethodPost, PathContains: "/qemu/100/status/stop",
			Status: http.StatusInternalServerError, Body: "unable to stop"})
		p := createTestProvider(endpoint)
		_, err = p.Power(ctx, &providerv1.PowerRequest{Id: "100", Op: providerv1.PowerOp_POWER_OP_OFF})
		require.Error(t, err)
		assert.Equal(t, "running", vmStatus(t, srv, 100))
	})
}
