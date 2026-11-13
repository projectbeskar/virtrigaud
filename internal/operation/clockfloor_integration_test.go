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

package operation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// hostClocks is the wall clock, boot clock and boot_id of a test host.
type hostClocks struct {
	wall   time.Time
	boot   time.Duration
	bootID string
}

func (h *hostClocks) Now() time.Time                   { return h.wall }
func (h *hostClocks) BootTime() (time.Duration, error) { return h.boot, nil }
func (h *hostClocks) BootID() (string, error)          { return h.bootID, nil }

// floorVerifier verifies for pki with the time the agent's floor reports.
func floorVerifier(t *testing.T, pki *testPKI, f *clockfloor.Floor) *Verifier {
	t.Helper()
	cfg := VerifierConfig{
		Anchors: nil, InstallID: pki.installID, ProviderUID: testProviderUID, HostID: testHostID,
		EnrolmentID: testEnrolment, Clock: f,
	}
	cfg.Anchors = append(cfg.Anchors, pki.anchor)
	v, err := NewVerifier(cfg)
	require.NoError(t, err)
	return v
}

// TestVerify_WallClockRolledBackPastKeyExpiry: the wall clock moves back to a
// time at which the grant-signing key was valid. The wall-clock verifier
// accepts the grant; the agent's floor refuses it.
func TestVerify_WallClockRolledBackPastKeyExpiry(t *testing.T) {
	ctx := context.Background()
	pki := validPKI(t) // the leaf is valid until testEpoch + 1h
	kind := providerv1.OperationKind_OPERATION_KIND_POWER_ON
	call := newCall(t, pki, kind)
	outer := call.outer(t, pki)

	// Control: with the wall clock at testEpoch the grant verifies.
	_, err := testVerifier(t, pki).Verify(call.method, outer)
	require.NoError(t, err)

	// The agent ran for two hours (the key expired), then the wall clock was
	// set back to testEpoch.
	h := &hostClocks{wall: testEpoch, boot: time.Hour, bootID: "boot-a"}
	store := &clockfloor.MemStore{}
	f, err := clockfloor.Open(ctx, clockfloor.Config{Store: store, Wall: h, Boot: h, BootID: h, FirstUse: true, MaxAhead: 24 * time.Hour})
	require.NoError(t, err)
	require.NoError(t, f.StartPersisting(ctx))
	h.wall, h.boot = h.wall.Add(2*time.Hour), h.boot+2*time.Hour
	require.NoError(t, f.Flush(ctx))
	h.wall = testEpoch // rolled back

	_, err = floorVerifier(t, pki, f).Verify(call.method, outer)
	r, ok := AsRefusal(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, "GRANT_UNAUTHENTICATED", r.Reason)
	assert.Equal(t, RuleChain, r.Rule, "the expired signing key is not revived")

	// The same rollback after a restart of the process, in a new boot.
	h.bootID, h.boot = "boot-b", 5*time.Second
	f2, err := clockfloor.Open(ctx, clockfloor.Config{Store: store, Wall: h, Boot: h, BootID: h, MaxAhead: 24 * time.Hour})
	require.NoError(t, err)
	_, err = floorVerifier(t, pki, f2).Verify(call.method, outer)
	r, ok = AsRefusal(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, "GRANT_UNAUTHENTICATED", r.Reason)
}

// TestVerify_FloorAheadRefusesEveryGrant: with the floor ahead of the wall
// clock by more than the tolerance the verifier refuses with
// GRANT_UNAUTHENTICATED and the floor reports ClockFloorAhead.
func TestVerify_FloorAheadRefusesEveryGrant(t *testing.T) {
	ctx := context.Background()
	pki := validPKI(t)
	call := newCall(t, pki, providerv1.OperationKind_OPERATION_KIND_POWER_ON)
	outer := call.outer(t, pki)

	h := &hostClocks{wall: testEpoch.Add(30 * 24 * time.Hour), boot: time.Hour, bootID: "boot-a"}
	store := &clockfloor.MemStore{}
	f1, err := clockfloor.Open(ctx, clockfloor.Config{Store: store, Wall: h, Boot: h, BootID: h, FirstUse: true})
	require.NoError(t, err)
	require.NoError(t, f1.StartPersisting(ctx)) // the clock jumped forward a month and was saved
	h.bootID, h.boot, h.wall = "boot-b", 3*time.Second, testEpoch
	f, err := clockfloor.Open(ctx, clockfloor.Config{Store: store, Wall: h, Boot: h, BootID: h})
	require.NoError(t, err)

	_, err = floorVerifier(t, pki, f).Verify(call.method, outer)
	r, ok := AsRefusal(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, "GRANT_UNAUTHENTICATED", r.Reason)
	assert.Equal(t, RuleUntrustedClock, r.Rule)
	assert.Equal(t, clockfloor.StateClockFloorAhead, f.Status().State)
}
