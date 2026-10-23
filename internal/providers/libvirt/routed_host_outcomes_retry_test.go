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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
)

// outcomeUnknownBreaker is a manager breaker that opens at the first counted
// failure: an answer that wrongly counted would open it.
func outcomeUnknownBreaker() *resilience.CircuitBreaker {
	return resilience.NewCircuitBreaker("r25-m1", "libvirt", "clustered", &resilience.Config{
		FailureThreshold: 1, ResetTimeout: time.Hour, HalfOpenMaxCalls: 1,
	})
}

// assertOutcomeUnknownRetry pins how the manager's transport client reads the
// "outcome unknown, safe to retry" answer (R25 M1): retryable, NOT nothing
// done, tagged as a lost host connection, and kept out of the breaker.
func assertOutcomeUnknownRetry(t *testing.T, err error, cb *resilience.CircuitBreaker) {
	t.Helper()
	require.Error(t, err)
	assert.True(t, contracts.IsRetryable(err), "retried: %v", err)
	assert.True(t, contracts.IsOutcomeUnknownRetry(err), "marked outcome-unknown-retry: %v", err)
	assert.False(t, contracts.IsNothingDone(err), "the call may have changed the host: %v", err)
	assert.False(t, contracts.IsHostUnavailable(err))
	assert.False(t, contracts.IsRequestNotSent(err))
	assert.Equal(t, resilience.StateClosed, cb.GetState(), "kept out of the Provider's circuit breaker")
}

// TestRoutedHostOutcomes_RetryAnswersThroughTheTransport: a Clone whose copy
// was sent, a SnapshotCreate whose snapshot command was sent, and a
// DeleteLandedDisk whose rm was sent, each lose the host's connection. The
// provider answers "outcome unknown, safe to retry" with its own reason, and
// the manager's transport client maps it to a retryable error that is not
// "nothing done".
func TestRoutedHostOutcomes_RetryAnswersThroughTheTransport(t *testing.T) {
	t.Run("Clone", func(t *testing.T) {
		fx := sourceWeb(t, nil)
		fx.script("local", "dead-qemu-img-convert", "")
		cb := outcomeUnknownBreaker()
		c := startBreakerGRPC(t, fx.p, cb)
		_, err := c.Clone(context.Background(), contracts.CloneRequest{
			Source:       contracts.VMRef{ID: "web", HostID: "host-b", Owner: ownerTeamA},
			TargetName:   "copy",
			TargetVM:     cloneTarget,
			TargetHostID: "host-b",
		})
		assertOutcomeUnknownRetry(t, err, cb)
		assert.True(t, contracts.IsHostConnectionLost(err), "paced as an unavailable host: %v", err)
	})

	t.Run("SnapshotCreate", func(t *testing.T) {
		fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
		fx.script("host-b", "dead-snapshot-create-as", "")
		cb := outcomeUnknownBreaker()
		c := startBreakerGRPC(t, fx.p, cb)
		_, err := c.SnapshotCreate(context.Background(), contracts.SnapshotCreateRequest{
			VM:           contracts.VMRef{ID: "web", HostID: "host-b", Owner: ownerTeamA},
			NameHint:     "pre",
			Description:  "d",
			RequestToken: "uid-snap-1",
		})
		assertOutcomeUnknownRetry(t, err, cb)
		assert.True(t, contracts.IsHostConnectionLost(err))
	})

	t.Run("SnapshotCreate whose budget ran out", func(t *testing.T) {
		// The routed budget ends before the caller's deadline; the virsh client
		// is abandoned while libvirtd's job may still complete.
		fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
		fx.script("host-b", "dead-snapshot-create-as", "")
		cb := outcomeUnknownBreaker()
		c := startBreakerGRPC(t, fx.p, cb)
		ctx, cancel := context.WithTimeout(context.Background(), routedBudgetMargin/2)
		defer cancel()
		_, err := c.SnapshotCreate(ctx, contracts.SnapshotCreateRequest{
			VM: contracts.VMRef{ID: "web", HostID: "host-b", Owner: ownerTeamA}, NameHint: "pre", RequestToken: "uid-snap-1"})
		assertOutcomeUnknownRetry(t, err, cb)
	})

	t.Run("SnapshotCreate without a request token is Unknown", func(t *testing.T) {
		fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
		fx.script("host-b", "dead-snapshot-create-as", "")
		cb := outcomeUnknownBreaker()
		c := startBreakerGRPC(t, fx.p, cb)
		_, err := c.SnapshotCreate(context.Background(), contracts.SnapshotCreateRequest{
			VM: contracts.VMRef{ID: "web", HostID: "host-b", Owner: ownerTeamA}, NameHint: "pre"})
		require.Error(t, err)
		assert.False(t, contracts.IsOutcomeUnknownRetry(err), "a retry could take a second snapshot")
		assert.False(t, contracts.IsNothingDone(err))
		assert.False(t, contracts.IsRetryable(err), "not retried blindly: %v", err)
		assert.Equal(t, codes.Unknown, status.Code(err), "%v", err)
		assert.Equal(t, resilience.StateClosed, cb.GetState())
	})

	t.Run("DeleteLandedDisk", func(t *testing.T) {
		f := newImportFixtureOf(t, []string{"host-b"})
		f.rec.failOn("rm", 0, virshFailure(-1, "reset"))
		cb := outcomeUnknownBreaker()
		c := startBreakerGRPC(t, f.p, cb)
		_, err := c.DeleteLandedDisk(context.Background(), contracts.LandedDiskDeleteRequest{
			Migration:    contracts.ObjectIdentity{UID: "uid-mig", Namespace: "team-a", Name: "mig"},
			TargetVM:     importTarget,
			TargetHostID: "host-b",
		})
		assertOutcomeUnknownRetry(t, err, cb)
	})
}

// TestRoutedHostOutcomes_PowerHostLossIsPacedAsAnUnavailableHost (L3): a
// Power or Reconfigure answered "not performed" or "unknown" because the
// host's connection was lost carries HOST_CONNECTION_LOST through the
// transport client, which the manager paces like an unavailable host (30 s),
// while any other provider error keeps the 5 s cadence.
func TestRoutedHostOutcomes_PowerHostLossIsPacedAsAnUnavailableHost(t *testing.T) {
	for name, script := range map[string]string{
		"not performed (a read drops)": "dead-dumpxml",
		"unknown (destroy is sent)":    "dead-destroy",
	} {
		t.Run(name, func(t *testing.T) {
			fx, p := clusteredOpsFixture(t)
			fx.script("host-b", "state", "running\n")
			fx.script("host-b", script, "")
			cb := outcomeUnknownBreaker()
			c := startBreakerGRPC(t, p, cb)
			_, err := c.Power(context.Background(), webOnHostB, contracts.PowerOpOff)
			require.Error(t, err)
			assert.True(t, contracts.IsHostConnectionLost(err), "tagged: %v", err)
			assert.False(t, contracts.IsHostUnavailable(err), "not the definite HOST_UNAVAILABLE: %v", err)
			assert.Equal(t, resilience.StateClosed, cb.GetState())
		})
	}
}
