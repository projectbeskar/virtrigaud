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

package mock

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestPower_IdempotentInRequestedState: as on the real providers (v0.4.0-rc1
// F3), a power operation on a VM already in the requested state succeeds with
// no task; one that changes the state returns a task.
func TestPower_IdempotentInRequestedState(t *testing.T) {
	ctx := context.Background()
	p := NewProvider()

	for _, tc := range []struct {
		id string
		op providerv1.PowerOp
	}{
		{"vm-1", providerv1.PowerOp_POWER_OP_ON},                // demo-vm-1 is On
		{"vm-2", providerv1.PowerOp_POWER_OP_OFF},               // demo-vm-2 is Off
		{"vm-2", providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL}, // demo-vm-2 is Off
	} {
		resp, err := p.Power(ctx, &providerv1.PowerRequest{Id: tc.id, Op: tc.op})
		require.NoError(t, err, "%s %s", tc.id, tc.op)
		assert.Nil(t, resp.GetTask(), "%s %s: already in the requested state, no task", tc.id, tc.op)
	}

	resp, err := p.Power(ctx, &providerv1.PowerRequest{Id: "vm-1", Op: providerv1.PowerOp_POWER_OP_OFF})
	require.NoError(t, err)
	assert.NotNil(t, resp.GetTask(), "a state change is a task")
}
