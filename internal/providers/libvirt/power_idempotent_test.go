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

package libvirt

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestClustered_PowerIdempotent: on a clustered provider too, Power is
// idempotent (v0.4.0-rc1 F3 — on the lab two `virsh destroy` of one domain ran
// concurrently and the second failed): a failed destroy / shutdown of a domain
// that is shut off, or a failed start of a running one, re-reads domstate on
// the bound host and succeeds; a failed destroy of a domain still running
// fails retryably.
func TestClustered_PowerIdempotent(t *testing.T) {
	for name, tc := range map[string]struct {
		state   string
		fail    []string
		op      contracts.PowerOp
		wantErr bool
	}{
		"off, destroy fails (already off)":            {state: "shut off\n", fail: []string{"fail-destroy"}, op: contracts.PowerOpOff},
		"graceful, shutdown and destroy fail (off)":   {state: "shut off\n", fail: []string{"fail-shutdown", "fail-destroy"}, op: contracts.PowerOpShutdownGraceful},
		"on, start fails (already running)":           {state: "running\n", fail: []string{"fail-start"}, op: contracts.PowerOpOn},
		"off, destroy fails, still running":           {state: "running\n", fail: []string{"fail-destroy"}, op: contracts.PowerOpOff, wantErr: true},
		"on, start fails, still shut off":             {state: "shut off\n", fail: []string{"fail-start"}, op: contracts.PowerOpOn, wantErr: true},
		"reboot is never satisfied by a state":        {state: "running\n", fail: []string{"fail-start"}, op: contracts.PowerOpReboot, wantErr: true},
		"off, destroy fails, state unreadable stands": {state: "shut off\n", fail: []string{"fail-destroy", "fail-domstate"}, op: contracts.PowerOpOff, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			fx, p := clusteredOpsFixture(t)
			fx.script("host-b", "state", tc.state)
			for _, f := range tc.fail {
				fx.script("host-b", f, "")
			}
			_, err := p.Power(context.Background(), webOnHostB, tc.op)
			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, contracts.IsRetryable(err), "a real power failure stays retryable: %v", err)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, fx.calls(), "host-b domstate "+routingDomainUUID, "the state is re-read on the bound host")
		})
	}
}
