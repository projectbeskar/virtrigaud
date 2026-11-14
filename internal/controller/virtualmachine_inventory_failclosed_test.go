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

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A Provider that refuses every host because its host inventory is stale
// (fail-closed: HOST_UNAVAILABLE without a host name, for every call) never
// makes the manager re-place a VM: a Create on its pending host is held on
// that host, the host is not excluded, and the same attempt (same op_id) is
// sent to the same host again later.
func TestIdentityCreate_ProviderWideHostUnavailableNeverReplaces(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{}, contracts.NewHostUnavailableError(
			"no valid host inventory was applied within the Provider's fail-closed bound; every host is refused", nil)
	}
	reconcileN(t, r, req, 4)

	vm := idVM(t, r)
	require.NotNil(t, vm.Status.Placement)
	assert.Equal(t, "host-alpha", vm.Status.Placement.PendingHost, "kept on its pending host")
	assert.Empty(t, vm.Status.Placement.ExcludedHosts, "the host is not excluded")
	assert.Empty(t, vm.Status.Placement.Host, "not bound")
	a := findAttempt(vm, contracts.OperationKindCreate)
	require.NotNil(t, a)
	assert.NotEqual(t, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, a.Phase, "the attempt stays open")
	sends := stub.sent()
	require.NotEmpty(t, sends)
	for _, s := range sends {
		assert.Equal(t, "host-alpha", s.Identity.HostID, "never sent to another host")
		assert.Equal(t, sends[0].Identity.OpID, s.Identity.OpID, "the same attempt, never a new one")
	}
}
