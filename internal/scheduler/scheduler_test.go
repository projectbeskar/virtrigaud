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

package scheduler

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// --- test builders ----------------------------------------------------------

func p32(v int32) *int32 { return &v }
func p64(v int64) *int64 { return &v }

type hostOpt func(*v1beta1.Host)

func hHealth(x v1beta1.HostHealth) hostOpt { return func(h *v1beta1.Host) { h.Status.Health = x } }
func hSchedulable(b bool) hostOpt          { return func(h *v1beta1.Host) { h.Spec.Schedulable = b } }
func hLabels(m map[string]string) hostOpt  { return func(h *v1beta1.Host) { h.Spec.Labels = m } }
func hFeatures(f ...string) hostOpt        { return func(h *v1beta1.Host) { h.Status.CPUFeatures = f } }
func hMachineTypes(m ...string) hostOpt    { return func(h *v1beta1.Host) { h.Status.MachineTypes = m } }
func hStorageBytes(b int64) hostOpt {
	return func(h *v1beta1.Host) { h.Status.AllocatableStorageBytes = p64(b) }
}

// newHost builds a Ready, schedulable host with the given allocatable CPU/mem.
func newHost(name string, cpu int32, memMiB int64, opts ...hostOpt) v1beta1.Host {
	h := v1beta1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1beta1.HostSpec{Schedulable: true},
		Status: v1beta1.HostStatus{
			Health:               v1beta1.HostHealthReady,
			AllocatableCPU:       p32(cpu),
			AllocatableMemoryMiB: p64(memMiB),
		},
	}
	for _, o := range opts {
		o(&h)
	}
	return h
}

func oc(cpu, mem string) *v1beta1.OvercommitRatios {
	return &v1beta1.OvercommitRatios{CPU: cpu, Memory: mem}
}

func placed(name, host string, labels map[string]string) PlacedVM {
	return PlacedVM{Name: name, HostID: host, Labels: labels}
}

// selEq builds a metav1 label selector matching a single label equality.
func selEq(k, v string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{k: v}}
}

// policy wraps a spec into a named VMPlacementPolicy.
func policy(spec v1beta1.VMPlacementPolicySpec) *v1beta1.VMPlacementPolicy {
	return &v1beta1.VMPlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy"},
		Spec:       spec,
	}
}

// baseReq is a Spread request for a 2 vCPU / 2048 MiB VM over the given candidates.
func baseReq(candidates ...v1beta1.Host) Request {
	return Request{
		Resources:  ResourceRequest{CPU: 2, MemoryMiB: 2048},
		Pool:       v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategySpread},
		Candidates: candidates,
	}
}

// --- main table -------------------------------------------------------------

func TestSchedule(t *testing.T) {
	tests := []struct {
		name string
		req  Request
		// wantHost != "" asserts a feasible selection of that host.
		// wantHost == "" asserts a *NoFeasibleHostError.
		wantHost           string
		wantReasonContains string
		wantRejReason      string // for no-fit: a category expected in the tally
	}{
		// ---- basic fit / no-fit ----
		{
			name:               "basic single-host fit",
			req:                baseReq(newHost("host-a", 8, 16384)),
			wantHost:           "host-a",
			wantReasonContains: "Spread strategy",
		},
		{
			name:          "no-fit: single host lacks CPU",
			req:           baseReq(newHost("host-a", 1, 16384)),
			wantHost:      "",
			wantRejReason: rejInsufficientCPU,
		},
		{
			name:          "no-fit: single host lacks memory",
			req:           baseReq(newHost("host-a", 8, 1024)),
			wantHost:      "",
			wantRejReason: rejInsufficientMem,
		},

		// ---- capacity boundary ----
		{
			name:     "capacity boundary: request exactly equals allocatable",
			req:      Request{Resources: ResourceRequest{CPU: 4, MemoryMiB: 4096}, Pool: v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategySpread}, Candidates: []v1beta1.Host{newHost("host-a", 4, 4096)}},
			wantHost: "host-a",
		},
		{
			name:          "capacity boundary: one MiB over allocatable is no-fit",
			req:           Request{Resources: ResourceRequest{CPU: 4, MemoryMiB: 4097}, Pool: v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategySpread}, Candidates: []v1beta1.Host{newHost("host-a", 4, 4096)}},
			wantHost:      "",
			wantRejReason: rejInsufficientMem,
		},
		{
			name: "overcommit lets a smaller host fit",
			req: Request{
				Resources:  ResourceRequest{CPU: 8, MemoryMiB: 4096},
				Pool:       v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategySpread, Overcommit: oc("2.0", "1.0")},
				Candidates: []v1beta1.Host{newHost("host-a", 4, 4096)},
			},
			wantHost: "host-a", // 4 vCPU * 2.0 = 8 effective, exactly fits
		},
		{
			name: "overcommit boundary: one vCPU over effective is no-fit",
			req: Request{
				Resources:  ResourceRequest{CPU: 9, MemoryMiB: 4096},
				Pool:       v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategySpread, Overcommit: oc("2.0", "1.0")},
				Candidates: []v1beta1.Host{newHost("host-a", 4, 4096)},
			},
			wantHost:      "",
			wantRejReason: rejInsufficientCPU,
		},

		// ---- Spread vs BinPack ----
		{
			name:     "spread picks the host with most free memory",
			req:      baseReq(newHost("host-a", 8, 16384), newHost("host-b", 16, 32768)),
			wantHost: "host-b",
		},
		{
			name: "binpack picks the tightest host that still fits",
			req: Request{
				Resources:  ResourceRequest{CPU: 2, MemoryMiB: 2048},
				Pool:       v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategyBinPack},
				Candidates: []v1beta1.Host{newHost("host-a", 8, 16384), newHost("host-b", 16, 32768)},
			},
			wantHost:           "host-a",
			wantReasonContains: "BinPack strategy",
		},
		{
			name: "spread breaks a capacity tie by fewest bound VMs",
			req: Request{
				Resources:  ResourceRequest{CPU: 2, MemoryMiB: 2048},
				Pool:       v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategySpread},
				Candidates: []v1beta1.Host{newHost("host-a", 8, 16384), newHost("host-b", 8, 16384)},
				PlacedVMs:  []PlacedVM{placed("vm1", "host-a", nil), placed("vm2", "host-a", nil)},
			},
			wantHost: "host-b", // equal capacity, host-b has 0 bound VMs
		},
		{
			name: "binpack breaks a capacity tie by most bound VMs",
			req: Request{
				Resources:  ResourceRequest{CPU: 2, MemoryMiB: 2048},
				Pool:       v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategyBinPack},
				Candidates: []v1beta1.Host{newHost("host-a", 8, 16384), newHost("host-b", 8, 16384)},
				PlacedVMs:  []PlacedVM{placed("vm1", "host-a", nil), placed("vm2", "host-a", nil)},
			},
			wantHost: "host-a", // equal capacity, host-a is more loaded -> consolidate
		},

		// ---- hard node-selector ----
		{
			name: "hard nodeSelector match",
			req: func() Request {
				r := baseReq(
					newHost("host-a", 8, 16384, hLabels(map[string]string{"zone": "z1"})),
					newHost("host-b", 8, 16384, hLabels(map[string]string{"zone": "z2"})),
				)
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{Hard: &v1beta1.PlacementConstraints{NodeSelector: map[string]string{"zone": "z1"}}})
				return r
			}(),
			wantHost: "host-a",
		},
		{
			name: "hard nodeSelector mismatch is no-fit",
			req: func() Request {
				r := baseReq(newHost("host-b", 8, 16384, hLabels(map[string]string{"zone": "z2"})))
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{Hard: &v1beta1.PlacementConstraints{NodeSelector: map[string]string{"zone": "z1"}}})
				return r
			}(),
			wantHost:      "",
			wantRejReason: rejNodeSelector,
		},

		// ---- hard host allow / deny lists ----
		{
			name: "hard host allow-list excludes non-listed host",
			req: func() Request {
				r := baseReq(newHost("host-a", 8, 16384), newHost("host-b", 8, 16384))
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{Hard: &v1beta1.PlacementConstraints{Hosts: []string{"host-b"}}})
				return r
			}(),
			wantHost: "host-b",
		},
		{
			name: "hard excluded-hosts removes a host",
			req: func() Request {
				r := baseReq(newHost("host-a", 8, 16384), newHost("host-b", 8, 16384))
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{Hard: &v1beta1.PlacementConstraints{ExcludedHosts: []string{"host-b"}}})
				return r
			}(),
			wantHost: "host-a",
		},

		// ---- required CPU feature ----
		{
			name: "required CPU feature filters hosts lacking it",
			req: func() Request {
				r := baseReq(
					newHost("host-a", 8, 16384, hFeatures("avx2")),
					newHost("host-b", 8, 16384, hFeatures("avx2", "avx512")),
				)
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{ResourceConstraints: &v1beta1.ResourceConstraints{RequiredFeatures: []string{"avx512"}}})
				return r
			}(),
			wantHost: "host-b",
		},
		{
			name: "required CPU feature absent everywhere is no-fit",
			req: func() Request {
				r := baseReq(newHost("host-a", 8, 16384, hFeatures("avx2")))
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{ResourceConstraints: &v1beta1.ResourceConstraints{RequiredFeatures: []string{"avx512"}}})
				return r
			}(),
			wantHost:      "",
			wantRejReason: rejMissingFeature,
		},

		// ---- minimum per-host resource floor ----
		{
			name: "minCPUPerHost floor excludes an undersized host",
			req: func() Request {
				r := baseReq(newHost("host-a", 4, 16384), newHost("host-b", 16, 16384))
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{ResourceConstraints: &v1beta1.ResourceConstraints{MinCPUPerHost: p32(8)}})
				return r
			}(),
			wantHost: "host-b",
		},
		{
			name: "minMemoryPerHost floor excludes an undersized host",
			req: func() Request {
				r := baseReq(newHost("host-a", 8, 8192), newHost("host-b", 8, 65536))
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{ResourceConstraints: &v1beta1.ResourceConstraints{MinMemoryPerHost: quantityPtr("32Gi")}})
				return r
			}(),
			wantHost: "host-b",
		},
		{
			name: "minDiskSpacePerHost floor excludes an undersized host",
			req: func() Request {
				r := baseReq(
					newHost("host-a", 8, 16384, hStorageBytes(10*1024*1024*1024)),
					newHost("host-b", 8, 16384, hStorageBytes(500*1024*1024*1024)),
				)
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{ResourceConstraints: &v1beta1.ResourceConstraints{MinDiskSpacePerHost: quantityPtr("100Gi")}})
				return r
			}(),
			wantHost: "host-b",
		},

		// ---- required machine type ----
		{
			name: "required machine type filters hosts lacking it",
			req: func() Request {
				r := baseReq(
					newHost("host-a", 8, 16384, hMachineTypes("pc-i440fx-8.2")),
					newHost("host-b", 8, 16384, hMachineTypes("pc-q35-8.2")),
				)
				r.RequiredMachineType = "pc-q35-8.2"
				return r
			}(),
			wantHost: "host-b",
		},
		{
			name: "required machine type absent everywhere is no-fit",
			req: func() Request {
				r := baseReq(newHost("host-a", 8, 16384, hMachineTypes("pc-i440fx-8.2")))
				r.RequiredMachineType = "pc-q35-8.2"
				return r
			}(),
			wantHost:      "",
			wantRejReason: rejMachineType,
		},

		// ---- D6 storage / network visibility ----
		{
			name: "storage-pool visibility label required",
			req: func() Request {
				r := baseReq(
					newHost("host-a", 8, 16384),
					newHost("host-b", 8, 16384, hLabels(map[string]string{LabelStoragePoolPrefix + "nfs01": "true"})),
				)
				r.RequiredStoragePools = []string{"nfs01"}
				return r
			}(),
			wantHost: "host-b",
		},
		{
			name: "network visibility label required",
			req: func() Request {
				r := baseReq(
					newHost("host-a", 8, 16384),
					newHost("host-b", 8, 16384, hLabels(map[string]string{LabelNetworkPrefix + "br-vlan100": "true"})),
				)
				r.RequiredNetworks = []string{"br-vlan100"}
				return r
			}(),
			wantHost: "host-b",
		},
		{
			name: "missing storage visibility everywhere is no-fit",
			req: func() Request {
				r := baseReq(newHost("host-a", 8, 16384))
				r.RequiredStoragePools = []string{"nfs01"}
				return r
			}(),
			wantHost:      "",
			wantRejReason: rejStorageVisibility,
		},

		// ---- anti-affinity exclusion / affinity preference ----
		{
			name: "required VM anti-affinity excludes the co-located host",
			req: func() Request {
				r := baseReq(newHost("host-a", 8, 16384), newHost("host-b", 8, 16384))
				r.PlacedVMs = []PlacedVM{placed("vm-db", "host-a", map[string]string{"app": "db"})}
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{AntiAffinity: &v1beta1.AntiAffinityRules{
					VMAntiAffinity: &v1beta1.VMAntiAffinity{RequiredDuringScheduling: []v1beta1.VMAffinityTerm{
						{LabelSelector: selEq("app", "db"), TopologyKey: "kubernetes.io/hostname"},
					}},
				}})
				return r
			}(),
			wantHost: "host-b",
		},
		{
			name: "required VM anti-affinity with only the co-located host is no-fit",
			req: func() Request {
				r := baseReq(newHost("host-a", 8, 16384))
				r.PlacedVMs = []PlacedVM{placed("vm-db", "host-a", map[string]string{"app": "db"})}
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{AntiAffinity: &v1beta1.AntiAffinityRules{
					VMAntiAffinity: &v1beta1.VMAntiAffinity{RequiredDuringScheduling: []v1beta1.VMAffinityTerm{
						{LabelSelector: selEq("app", "db"), TopologyKey: "kubernetes.io/hostname"},
					}},
				}})
				return r
			}(),
			wantHost:      "",
			wantRejReason: rejReqVMAntiAffinity,
		},
		{
			name: "required VM affinity keeps only the co-located host",
			req: func() Request {
				r := baseReq(newHost("host-a", 8, 16384), newHost("host-b", 8, 16384))
				r.PlacedVMs = []PlacedVM{placed("vm-web", "host-a", map[string]string{"app": "web"})}
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{Affinity: &v1beta1.AffinityRules{
					VMAffinity: &v1beta1.VMAffinity{RequiredDuringScheduling: []v1beta1.VMAffinityTerm{
						{LabelSelector: selEq("app", "web"), TopologyKey: "kubernetes.io/hostname"},
					}},
				}})
				return r
			}(),
			wantHost: "host-a",
		},
		{
			name: "required VM affinity with no matching placed VM is no-fit",
			req: func() Request {
				r := baseReq(newHost("host-a", 8, 16384), newHost("host-b", 8, 16384))
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{Affinity: &v1beta1.AffinityRules{
					VMAffinity: &v1beta1.VMAffinity{RequiredDuringScheduling: []v1beta1.VMAffinityTerm{
						{LabelSelector: selEq("app", "web"), TopologyKey: "kubernetes.io/hostname"},
					}},
				}})
				return r
			}(),
			wantHost:      "",
			wantRejReason: rejReqVMAffinity,
		},
		{
			name: "preferred VM affinity outranks the default tie-break",
			req: func() Request {
				// Identical hosts: a bare Spread tie breaks to host-a alphabetically.
				// A preferred-affinity match on host-b outranks that.
				r := baseReq(newHost("host-a", 8, 16384), newHost("host-b", 8, 16384))
				r.PlacedVMs = []PlacedVM{placed("vm-cache", "host-b", map[string]string{"app": "cache"})}
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{Affinity: &v1beta1.AffinityRules{
					VMAffinity: &v1beta1.VMAffinity{PreferredDuringScheduling: []v1beta1.WeightedVMAffinityTerm{
						{Weight: 50, VMAffinityTerm: v1beta1.VMAffinityTerm{LabelSelector: selEq("app", "cache"), TopologyKey: "kubernetes.io/hostname"}},
					}},
				}})
				return r
			}(),
			wantHost: "host-b",
		},
		{
			name: "preferred host-affinity outranks the default tie-break",
			req: func() Request {
				r := baseReq(newHost("host-a", 8, 16384), newHost("host-b", 8, 16384))
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{Affinity: &v1beta1.AffinityRules{
					HostAffinity: &v1beta1.HostAffinityRule{Enabled: true, Scope: "preferred", PreferredHosts: []string{"host-b"}},
				}})
				return r
			}(),
			wantHost: "host-b",
		},

		// ---- strict host (anti-)affinity as hard filters ----
		{
			name: "strict host-affinity is a hard filter",
			req: func() Request {
				r := baseReq(newHost("host-a", 8, 16384), newHost("host-b", 8, 16384))
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{Affinity: &v1beta1.AffinityRules{
					HostAffinity: &v1beta1.HostAffinityRule{Enabled: true, Scope: scopeStrict, PreferredHosts: []string{"host-b"}},
				}})
				return r
			}(),
			wantHost: "host-b",
		},
		{
			name: "strict host anti-affinity cap excludes a host at the cap",
			req: func() Request {
				r := baseReq(newHost("host-a", 8, 16384), newHost("host-b", 8, 16384))
				r.PlacedVMs = []PlacedVM{placed("vm1", "host-a", nil)}
				r.Policy = policy(v1beta1.VMPlacementPolicySpec{AntiAffinity: &v1beta1.AntiAffinityRules{
					HostAntiAffinity: &v1beta1.HostAntiAffinityRule{Enabled: true, Scope: scopeStrict, MaxVMsPerHost: p32(1)},
				}})
				return r
			}(),
			wantHost: "host-b",
		},

		// ---- cordoned / NotReady exclusion ----
		{
			name:     "NotReady host is excluded",
			req:      baseReq(newHost("host-a", 8, 16384, hHealth(v1beta1.HostHealthNotReady)), newHost("host-b", 8, 16384)),
			wantHost: "host-b",
		},
		{
			name:     "cordoned host is excluded",
			req:      baseReq(newHost("host-a", 8, 16384, hSchedulable(false)), newHost("host-b", 8, 16384)),
			wantHost: "host-b",
		},
		{
			name:          "all hosts NotReady is no-fit",
			req:           baseReq(newHost("host-a", 8, 16384, hHealth(v1beta1.HostHealthNotReady))),
			wantHost:      "",
			wantRejReason: rejNotReady,
		},

		// ---- empty candidate set ----
		{
			name:     "empty candidate set is no-fit",
			req:      baseReq(),
			wantHost: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Schedule(tt.req)
			if tt.wantHost != "" {
				require.NoError(t, err)
				assert.Equal(t, tt.wantHost, got.HostID)
				assert.NotEmpty(t, got.Reason, "a feasible selection must carry a decision trace")
				if tt.wantReasonContains != "" {
					assert.Contains(t, got.Reason, tt.wantReasonContains)
				}
				return
			}

			// no-fit expectations
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrNoFeasibleHost), "no-fit must match ErrNoFeasibleHost")
			var nfe *NoFeasibleHostError
			require.True(t, errors.As(err, &nfe), "no-fit must be a *NoFeasibleHostError")
			assert.Equal(t, len(tt.req.Candidates), nfe.Candidates)
			if tt.wantRejReason != "" {
				assert.Contains(t, nfe.Tally(), tt.wantRejReason, "expected a rejection in category %q; got %v", tt.wantRejReason, nfe.Tally())
			}
		})
	}
}

func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// --- idempotency ------------------------------------------------------------

func TestScheduleIdempotentReSelect(t *testing.T) {
	// host-b has strictly more free capacity, so a fresh Spread schedule would pick
	// it. But the VM is already bound to host-a and host-a is still feasible, so
	// Schedule must re-select host-a and not churn the placement.
	req := Request{
		Resources:      ResourceRequest{CPU: 2, MemoryMiB: 2048},
		Pool:           v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategySpread},
		Candidates:     []v1beta1.Host{newHost("host-a", 8, 16384), newHost("host-b", 64, 131072)},
		CurrentBinding: "host-a",
	}
	got, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-a", got.HostID)
	assert.Contains(t, got.Reason, "re-selected current binding")
}

func TestScheduleReplacesBoundVMOnCordonedHost(t *testing.T) {
	// The bound host is now cordoned (drained). Idempotency must NOT keep it; the
	// VM is re-placed onto the remaining feasible host.
	req := Request{
		Resources:      ResourceRequest{CPU: 2, MemoryMiB: 2048},
		Pool:           v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategySpread},
		Candidates:     []v1beta1.Host{newHost("host-a", 8, 16384, hSchedulable(false)), newHost("host-b", 8, 16384)},
		CurrentBinding: "host-a",
	}
	got, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-b", got.HostID)
	assert.NotContains(t, got.Reason, "re-selected current binding")
}

func TestScheduleReplacesBoundVMOnNotReadyHost(t *testing.T) {
	req := Request{
		Resources:      ResourceRequest{CPU: 2, MemoryMiB: 2048},
		Pool:           v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategySpread},
		Candidates:     []v1beta1.Host{newHost("host-a", 8, 16384, hHealth(v1beta1.HostHealthNotReady)), newHost("host-b", 8, 16384)},
		CurrentBinding: "host-a",
	}
	got, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-b", got.HostID)
}

func TestScheduleIdempotentUnknownBindingFallsThrough(t *testing.T) {
	// CurrentBinding names a host no longer in the candidate set (e.g. removed from
	// the pool). Schedule ignores it and picks fresh.
	req := Request{
		Resources:      ResourceRequest{CPU: 2, MemoryMiB: 2048},
		Pool:           v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategySpread},
		Candidates:     []v1beta1.Host{newHost("host-b", 8, 16384)},
		CurrentBinding: "host-gone",
	}
	got, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-b", got.HostID)
	assert.NotContains(t, got.Reason, "re-selected")
}

// --- determinism ------------------------------------------------------------

func TestScheduleDeterministicTieBreak(t *testing.T) {
	// Three identical hosts: every score axis ties, so the choice is the lowest
	// host id, regardless of candidate order.
	mk := func(order ...string) Request {
		hosts := make([]v1beta1.Host, 0, len(order))
		for _, n := range order {
			hosts = append(hosts, newHost(n, 8, 16384))
		}
		return baseReq(hosts...)
	}
	orderings := [][]string{
		{"host-a", "host-b", "host-c"},
		{"host-c", "host-b", "host-a"},
		{"host-b", "host-c", "host-a"},
		{"host-c", "host-a", "host-b"},
	}
	for _, ord := range orderings {
		got, err := Schedule(mk(ord...))
		require.NoError(t, err)
		assert.Equal(t, "host-a", got.HostID, "ordering %v must still pick host-a", ord)
	}
}

func TestScheduleStableAcrossRepeatedRuns(t *testing.T) {
	req := baseReq(
		newHost("host-x", 16, 32768),
		newHost("host-y", 16, 32768),
		newHost("host-z", 8, 16384),
	)
	first, err := Schedule(req)
	require.NoError(t, err)
	for i := 0; i < 20; i++ {
		got, err := Schedule(req)
		require.NoError(t, err)
		assert.Equal(t, first.HostID, got.HostID)
	}
}

// --- input errors (not no-fit) ----------------------------------------------

func TestScheduleMalformedOvercommitIsError(t *testing.T) {
	req := Request{
		Resources:  ResourceRequest{CPU: 2, MemoryMiB: 2048},
		Pool:       v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategySpread, Overcommit: oc("not-a-number", "1.0")},
		Candidates: []v1beta1.Host{newHost("host-a", 8, 16384)},
	}
	_, err := Schedule(req)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNoFeasibleHost), "a malformed input is not a no-fit")
	assert.Contains(t, err.Error(), "overcommit")
}

func TestScheduleNonPositiveOvercommitIsError(t *testing.T) {
	req := Request{
		Resources:  ResourceRequest{CPU: 2, MemoryMiB: 2048},
		Pool:       v1beta1.HostPoolSpec{Overcommit: oc("0", "1.0")},
		Candidates: []v1beta1.Host{newHost("host-a", 8, 16384)},
	}
	_, err := Schedule(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be > 0")
}

func TestScheduleMalformedAffinitySelectorIsError(t *testing.T) {
	// An In operator with no values is an invalid label selector; because a placed
	// VM sits on the candidate host, the required anti-affinity term is evaluated
	// and the parse error surfaces from Schedule.
	req := baseReq(newHost("host-a", 8, 16384))
	req.PlacedVMs = []PlacedVM{placed("vm1", "host-a", map[string]string{"app": "db"})}
	req.Policy = policy(v1beta1.VMPlacementPolicySpec{AntiAffinity: &v1beta1.AntiAffinityRules{
		VMAntiAffinity: &v1beta1.VMAntiAffinity{RequiredDuringScheduling: []v1beta1.VMAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: nil},
			}},
			TopologyKey: "kubernetes.io/hostname",
		}}},
	}})
	_, err := Schedule(req)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNoFeasibleHost))
	assert.Contains(t, err.Error(), "selector")
}

// --- no-fit breakdown -------------------------------------------------------

func TestScheduleNoFeasibleHostBreakdown(t *testing.T) {
	// Three hosts, each eliminated by a different filter.
	req := baseReq(
		newHost("host-a", 8, 16384, hHealth(v1beta1.HostHealthNotReady)),
		newHost("host-b", 8, 16384, hSchedulable(false)),
		newHost("host-c", 1, 16384), // insufficient CPU for the 2 vCPU request
	)
	_, err := Schedule(req)
	require.Error(t, err)

	var nfe *NoFeasibleHostError
	require.True(t, errors.As(err, &nfe))
	assert.Equal(t, 3, nfe.Candidates)
	require.Len(t, nfe.Rejections, 3)

	// Rejections are sorted by host id for determinism.
	assert.Equal(t, "host-a", nfe.Rejections[0].HostID)
	assert.Equal(t, "host-b", nfe.Rejections[1].HostID)
	assert.Equal(t, "host-c", nfe.Rejections[2].HostID)

	tally := nfe.Tally()
	assert.Equal(t, 1, tally[rejNotReady])
	assert.Equal(t, 1, tally[rejCordoned])
	assert.Equal(t, 1, tally[rejInsufficientCPU])

	// The rendered message is deterministic and secret-free.
	msg := err.Error()
	assert.True(t, strings.HasPrefix(msg, "no feasible host: 0 of 3 candidate host(s)"), "got %q", msg)
}

func TestScheduleEmptyCandidatesMessage(t *testing.T) {
	_, err := Schedule(baseReq())
	require.Error(t, err)
	assert.Equal(t, "no feasible host: pool has no candidate hosts", err.Error())
}
