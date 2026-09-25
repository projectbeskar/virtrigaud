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

package scheduler

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// Committed capacity (ADR-0007 Addendum A, scheduler-accuracy amendment): every
// PlacedVM's Resources are subtracted from its host's effective capacity.

// holding is a PlacedVM with a UID and resources on host.
func holding(uid, host string, cpu int32, memMiB int64) PlacedVM {
	return PlacedVM{Name: "vm-" + uid, UID: uid, HostID: host, Resources: ResourceRequest{CPU: cpu, MemoryMiB: memMiB}}
}

// requireNoFit asserts err is a no-fit and returns it.
func requireNoFit(t *testing.T, err error) *NoFeasibleHostError {
	t.Helper()
	require.Error(t, err)
	var nf *NoFeasibleHostError
	require.True(t, errors.As(err, &nf), "want *NoFeasibleHostError, got %T: %v", err, err)
	return nf
}

func TestCommitted_HostFillsUp(t *testing.T) {
	host := newHost("host-a", 8, 65536)
	req := baseReq(host) // 2 vCPU / 2048 MiB

	// 3 x 2 vCPU committed: 2 of 8 free, the 4th VM still fits.
	req.PlacedVMs = []PlacedVM{
		holding("a", "host-a", 2, 2048), holding("b", "host-a", 2, 2048), holding("c", "host-a", 2, 2048),
	}
	res, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-a", res.HostID)

	// 4 x 2 vCPU committed: the host is full.
	req.PlacedVMs = append(req.PlacedVMs, holding("d", "host-a", 2, 2048))
	_, err = Schedule(req)
	nf := requireNoFit(t, err)
	require.Len(t, nf.Rejections, 1)
	assert.Equal(t, rejInsufficientCPU, nf.Rejections[0].Reason)
	require.NotNil(t, nf.Rejections[0].Shortfall)
	assert.Equal(t, CapacityShortfall{Requested: 2, Capacity: 8, Committed: 8}, *nf.Rejections[0].Shortfall)
	assert.Contains(t, err.Error(), "requested 2 vCPU and 2048 MiB, which exceeds the free capacity of every candidate host")
	assert.Equal(t, []string{"host-a: " + rejInsufficientCPU + ": requested 2, free 0 (committed 8 of 8)"}, nf.CapacityDetail(),
		"the arithmetic is kept for the manager log")
	assert.True(t, InsufficientCapacity(err))
}

func TestCommitted_MemoryFillsUp(t *testing.T) {
	req := baseReq(newHost("host-a", 64, 8192))
	req.PlacedVMs = []PlacedVM{holding("a", "host-a", 1, 4096), holding("b", "host-a", 1, 3072)}
	_, err := Schedule(req)
	nf := requireNoFit(t, err)
	assert.Equal(t, rejInsufficientMem, nf.Rejections[0].Reason)
	assert.Equal(t, CapacityShortfall{Requested: 2048, Capacity: 8192, Committed: 7168}, *nf.Rejections[0].Shortfall)
	assert.Contains(t, err.Error(), "requested 2 vCPU and 2048 MiB, which exceeds the free capacity of every candidate host")
}

// TestCommitted_TenantVisibleTextCarriesNoCommittedFigures (review M3): the
// success trace (status.placement.reason) and the no-fit message (the Placed
// condition) carry no committed, capacity or free figure — those are derived
// from other tenants' VMs.
func TestCommitted_TenantVisibleTextCarriesNoCommittedFigures(t *testing.T) {
	// 13 vCPU committed of 16 leaves 3 free: none of 13, 16 or 3 may appear.
	req := baseReq(newHost("host-a", 16, 65536))
	req.PlacedVMs = []PlacedVM{holding("a", "host-a", 13, 0)}
	res, err := Schedule(req)
	require.NoError(t, err)
	for _, figure := range []string{"13", "16", "3", "free"} {
		assert.NotContains(t, res.Reason, figure, "success trace: %q", res.Reason)
	}

	req.Resources.CPU = 4
	_, err = Schedule(req)
	_ = requireNoFit(t, err)
	msg := err.Error()
	for _, figure := range []string{"13", "16", "committed", "at most"} {
		assert.NotContains(t, msg, figure, "no-fit message: %q", msg)
	}
	assert.Contains(t, msg, "requested 4 vCPU and 2048 MiB")
}

func TestCommitted_VMIsExcludedFromItsOwnSum(t *testing.T) {
	req := baseReq(newHost("host-a", 2, 2048))
	req.VMUID = "self"
	// The VM's own binding / pending host / assumption, however many times it
	// is listed, never counts against it.
	req.PlacedVMs = []PlacedVM{holding("self", "host-a", 2, 2048), holding("self", "host-a", 2, 2048)}
	req.CurrentBinding = "host-a"

	res, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-a", res.HostID, "a bound VM re-selects its host; its own resources are not committed against it")

	// Another VM with the same resources does count.
	req.PlacedVMs = append(req.PlacedVMs, holding("other", "host-a", 1, 0))
	_, err = Schedule(req)
	_ = requireNoFit(t, err)
}

func TestCommitted_EveryEntryCounts_BoundPendingAndAssumed(t *testing.T) {
	// The scheduler does not care where an entry came from: a bound VM, a
	// pendingHost-only VM and an assumption each hold their resources.
	req := baseReq(newHost("host-a", 6, 65536))
	req.PlacedVMs = []PlacedVM{
		holding("bound", "host-a", 2, 0),
		holding("pending-only", "host-a", 2, 0),
		holding("assumed", "host-a", 2, 0),
	}
	_, err := Schedule(req)
	nf := requireNoFit(t, err)
	assert.Equal(t, int64(6), nf.Rejections[0].Shortfall.Committed)
}

func TestCommitted_SameVMSameHostCountedOnce(t *testing.T) {
	// placement.host == placement.pendingHost, or a durable record plus an
	// assumption not yet cleared: listed twice, counted once.
	req := baseReq(newHost("host-a", 4, 65536))
	req.PlacedVMs = []PlacedVM{holding("x", "host-a", 2, 0), holding("x", "host-a", 2, 0)}
	res, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-a", res.HostID, "2 of 4 committed, not 4 of 4")
}

func TestCommitted_SameVMOnTwoHostsCountsOnEach(t *testing.T) {
	// A VM recorded against two different hosts (host and a different
	// pendingHost) may hold resources on both, so each host counts it.
	req := baseReq(newHost("host-a", 2, 65536), newHost("host-b", 2, 65536))
	req.PlacedVMs = []PlacedVM{holding("x", "host-a", 2, 0), holding("x", "host-b", 2, 0)}
	_, err := Schedule(req)
	nf := requireNoFit(t, err)
	assert.Len(t, nf.Rejections, 2)
}

func TestCommitted_EmptyUIDsAreNotDeduplicated(t *testing.T) {
	req := baseReq(newHost("host-a", 4, 65536))
	req.PlacedVMs = []PlacedVM{holding("", "host-a", 2, 0), holding("", "host-a", 2, 0)}
	_, err := Schedule(req)
	_ = requireNoFit(t, err)
}

func TestCommitted_OvercommitScalesCapacityNotTheCommittedSum(t *testing.T) {
	// 4 physical vCPU x 2.0 = 8 bookable. 6 committed leaves 2: a 2 vCPU VM
	// fits. (Scaling the committed sum too — 12 of 8 — would reject it.)
	host := newHost("host-a", 4, 65536)
	req := baseReq(host)
	req.Pool.Overcommit = oc("2.0", "")
	req.PlacedVMs = []PlacedVM{holding("a", "host-a", 2, 0), holding("b", "host-a", 2, 0), holding("c", "host-a", 2, 0)}
	res, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-a", res.HostID)

	req.Resources.CPU = 3
	_, err = Schedule(req)
	nf := requireNoFit(t, err)
	assert.Equal(t, CapacityShortfall{Requested: 3, Capacity: 8, Committed: 6}, *nf.Rejections[0].Shortfall)
}

func TestCommitted_OverCommittedHostFitsNothing(t *testing.T) {
	// A lowered overcommit ratio can leave more committed than the pool now
	// allows: free is negative, rendered as 0.
	req := baseReq(newHost("host-a", 4, 65536))
	req.Pool.Overcommit = oc("0.5", "")
	req.Resources.CPU = 1
	req.PlacedVMs = []PlacedVM{holding("a", "host-a", 3, 0)}
	_, err := Schedule(req)
	nf := requireNoFit(t, err)
	assert.Equal(t, int64(-1), nf.Rejections[0].Shortfall.Free())
	assert.Equal(t, "requested 1, free 0 (committed 3 of 2)", nf.Rejections[0].Shortfall.String())
}

func TestCommitted_SpreadAndBinPackRankByFreeCapacity(t *testing.T) {
	// Equal hosts; host-a already holds 4 vCPU / 8 GiB.
	hosts := []v1beta1.Host{newHost("host-a", 8, 16384), newHost("host-b", 8, 16384)}
	placed := []PlacedVM{holding("a", "host-a", 4, 8192)}

	spread := baseReq(hosts...)
	spread.PlacedVMs = placed
	res, err := Schedule(spread)
	require.NoError(t, err)
	assert.Equal(t, "host-b", res.HostID, "Spread picks the host with the most uncommitted capacity")

	binPack := baseReq(hosts...)
	binPack.Pool.Strategy = v1beta1.PoolStrategyBinPack
	binPack.PlacedVMs = placed
	res, err = Schedule(binPack)
	require.NoError(t, err)
	assert.Equal(t, "host-a", res.HostID, "BinPack picks the tightest host that still fits")
}

func TestCommitted_CapacityOnlyCountsForCapacityButNotAffinity(t *testing.T) {
	// Another namespace's db VM on host-a: out of this VM's affinity scope, so
	// it does not trip the strict anti-affinity rule — but it holds capacity.
	antiDB := policy(v1beta1.VMPlacementPolicySpec{
		AntiAffinity: &v1beta1.AntiAffinityRules{
			VMAntiAffinity: &v1beta1.VMAntiAffinity{
				RequiredDuringScheduling: []v1beta1.VMAffinityTerm{{LabelSelector: selEq("app", "db")}},
			},
		},
	})
	foreign := holding("foreign", "host-a", 2, 0)
	foreign.Labels = map[string]string{"app": "db"}
	foreign.CapacityOnly = true

	req := baseReq(newHost("host-a", 4, 65536))
	req.Policy = antiDB
	req.PlacedVMs = []PlacedVM{foreign}
	res, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-a", res.HostID, "a CapacityOnly VM never matches an anti-affinity selector")
	assert.Contains(t, res.Reason, "boundVMs=0", "nor is it counted as a bound VM")
	req.Resources.CPU = 3
	_, err = Schedule(req)
	_ = requireNoFit(t, err) // but it does hold capacity: 2 of 4 committed, 3 does not fit
	req.Resources.CPU = 2

	// The same VM in scope trips the rule.
	foreign.CapacityOnly = false
	req.PlacedVMs = []PlacedVM{foreign}
	_, err = Schedule(req)
	nf := requireNoFit(t, err)
	assert.Equal(t, rejReqVMAntiAffinity, nf.Rejections[0].Reason)
}

func TestCommitted_NegativeResourcesNeverFreeCapacity(t *testing.T) {
	req := baseReq(newHost("host-a", 2, 65536))
	req.PlacedVMs = []PlacedVM{holding("neg", "host-a", -8, -8192), holding("b", "host-a", 1, 0)}
	_, err := Schedule(req)
	_ = requireNoFit(t, err)
}

func TestCommitted_NoFitMessageIsBoundedAndNamesNoVMOrHost(t *testing.T) {
	// 200 full hosts, 2000 VMs: the message stays the same size and never
	// names another tenant's VM or any host.
	var hosts []v1beta1.Host
	var placed []PlacedVM
	for i := 0; i < 200; i++ {
		h := fmt.Sprintf("host-%03d", i)
		hosts = append(hosts, newHost(h, 20, 65536))
		for j := 0; j < 10; j++ {
			placed = append(placed, PlacedVM{
				Name: fmt.Sprintf("tenant-secret-vm-%d-%d", i, j), UID: fmt.Sprintf("u-%d-%d", i, j),
				HostID: h, Resources: ResourceRequest{CPU: 2, MemoryMiB: 1024},
			})
		}
	}
	req := baseReq(hosts...)
	req.PlacedVMs = placed
	_, err := Schedule(req)
	_ = requireNoFit(t, err)
	msg := err.Error()
	assert.Less(t, len(msg), 400, "bounded message: %q", msg)
	assert.NotContains(t, msg, "tenant-secret-vm")
	assert.NotContains(t, msg, "host-0")
	assert.Contains(t, msg, "requested 2 vCPU and 2048 MiB, which exceeds the free capacity of every candidate host")
}

func TestCommitted_CapacitySummaryCountsTheCapacityRejections(t *testing.T) {
	req := baseReq(newHost("host-a", 8, 65536), newHost("host-b", 8, 65536), newHost("host-c", 1, 65536, hHealth(v1beta1.HostHealthNotReady)))
	req.Resources.CPU = 4
	req.PlacedVMs = []PlacedVM{holding("a", "host-a", 7, 0), holding("b", "host-b", 5, 0)}
	_, err := Schedule(req)
	nf := requireNoFit(t, err)
	msg := err.Error()
	assert.True(t, strings.Contains(msg, "requested 4 vCPU and 2048 MiB, which exceeds the free capacity of 2 of 3 candidate host(s)"), msg)
	assert.Contains(t, msg, rejNotReady+": 1")
	assert.Equal(t, []string{
		"host-a: " + rejInsufficientCPU + ": requested 4, free 1 (committed 7 of 8)",
		"host-b: " + rejInsufficientCPU + ": requested 4, free 3 (committed 5 of 8)",
	}, nf.CapacityDetail())
}

func TestInsufficientCapacity(t *testing.T) {
	assert.False(t, InsufficientCapacity(nil))
	assert.False(t, InsufficientCapacity(errors.New("x")))
	_, err := Schedule(baseReq(newHost("host-a", 8, 65536, hSchedulable(false))))
	nf := requireNoFit(t, err)
	assert.False(t, InsufficientCapacity(err), "a cordoned host is not a capacity shortfall")
	assert.Empty(t, nf.CapacitySummary())
}
