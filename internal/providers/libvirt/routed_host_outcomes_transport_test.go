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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
)

// TestRoutedHostOutcomes_ExportThroughTheTransport drives a routed ExportDisk
// over a real gRPC hop and the manager's transport client (the production
// mapGRPCError) and pins what the manager reads: HOST_UNAVAILABLE and "not
// performed" are definite ("nothing done": IsNothingDone, so the export may be
// re-sent), an export whose command was sent and whose session dropped is not
// (it keeps the manager's marker and is re-sent only once the settle time has
// passed). None counts toward the Provider's circuit breaker.
func TestRoutedHostOutcomes_ExportThroughTheTransport(t *testing.T) {
	for _, tc := range []struct {
		name        string
		host        []string
		local       map[string]string
		nothingDone bool
		hostDown    bool
	}{
		{name: "the host never answers", host: []string{"dead"}, nothingDone: true, hostDown: true},
		{name: "reads answered, then the host drops", host: []string{"dead-dumpxml"}, nothingDone: true},
		{name: "the convert is sent and the session drops", local: map[string]string{"dead-qemu-img-convert": ""}},
		{name: "the convert is killed (137)", local: map[string]string{"exit-qemu-img-convert": "137"}},
		{name: "the convert is stopped by timeout(1) (124)", local: map[string]string{"timeout-expire": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
			fx.p.hostDiskTransportFn = anyTransport
			for _, f := range tc.host {
				fx.script("host-b", f, "")
			}
			for f, content := range tc.local {
				fx.script("local", f, content)
			}
			cb := resilience.NewCircuitBreaker("r25", "libvirt", "clustered", &resilience.Config{
				FailureThreshold: 1, ResetTimeout: time.Hour, HalfOpenMaxCalls: 1,
			})
			c := startBreakerGRPC(t, fx.p, cb)

			_, err := c.ExportDisk(context.Background(), contracts.ExportDiskRequest{
				VM:          contracts.VMRef{ID: "web", HostID: "host-b", Owner: ownerTeamA},
				BackendType: "nfs", DestinationURL: "nfs://nas/e/web.qcow2",
			})
			require.Error(t, err)
			assert.Equal(t, tc.nothingDone, contracts.IsNothingDone(err), "nothing-done reading of %v", err)
			assert.Equal(t, tc.hostDown, contracts.IsHostUnavailable(err), "HOST_UNAVAILABLE reading of %v", err)
			assert.False(t, contracts.IsRequestNotSent(err), "the request reached the provider")
			assert.False(t, !tc.nothingDone && contracts.IsRetryable(err), "an unknown outcome is not a retryable answer: %v", err)
			assert.Equal(t, resilience.StateClosed, cb.GetState(), "a host-scoped answer never counts toward the Provider's breaker")
		})
	}
}
