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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// The soft-preference tests isolate a single preference: the "loser" host is made
// strictly more attractive on the base Spread strategy (more free memory), so the
// only way the "winner" can be chosen is the preference outranking strategy.

func TestScoreSoftHostsBonus(t *testing.T) {
	// host-b has more free memory, so bare Spread picks host-b; Soft.Hosts=[host-a]
	// flips it.
	r := baseReq(newHost("host-a", 8, 16384), newHost("host-b", 16, 32768))
	r.Policy = policy(v1beta1.VMPlacementPolicySpec{Soft: &v1beta1.PlacementConstraints{Hosts: []string{"host-a"}}})
	got, err := Schedule(r)
	require.NoError(t, err)
	assert.Equal(t, "host-a", got.HostID)
}

func TestScoreSoftExcludedHostsPenalty(t *testing.T) {
	// Identical hosts tie to host-a alphabetically; excluding host-a flips it.
	r := baseReq(newHost("host-a", 8, 16384), newHost("host-b", 8, 16384))
	r.Policy = policy(v1beta1.VMPlacementPolicySpec{Soft: &v1beta1.PlacementConstraints{ExcludedHosts: []string{"host-a"}}})
	got, err := Schedule(r)
	require.NoError(t, err)
	assert.Equal(t, "host-b", got.HostID)
}

func TestScoreSoftNodeSelectorBonus(t *testing.T) {
	r := baseReq(
		newHost("host-a", 8, 16384),
		newHost("host-b", 8, 16384, hLabels(map[string]string{"tier": "gold"})),
	)
	r.Policy = policy(v1beta1.VMPlacementPolicySpec{Soft: &v1beta1.PlacementConstraints{NodeSelector: map[string]string{"tier": "gold"}}})
	got, err := Schedule(r)
	require.NoError(t, err)
	assert.Equal(t, "host-b", got.HostID)
}

func TestScorePreferredFeaturesBonus(t *testing.T) {
	r := baseReq(
		newHost("host-a", 8, 16384),
		newHost("host-b", 8, 16384, hFeatures("avx512")),
	)
	r.Policy = policy(v1beta1.VMPlacementPolicySpec{ResourceConstraints: &v1beta1.ResourceConstraints{PreferredFeatures: []string{"avx512"}}})
	got, err := Schedule(r)
	require.NoError(t, err)
	assert.Equal(t, "host-b", got.HostID)
}

func TestScorePreferredVMAntiAffinityPenalty(t *testing.T) {
	// host-a has more free memory (bare Spread winner); a preferred anti-affinity
	// match on host-a penalizes it enough to lose to host-b.
	r := baseReq(newHost("host-a", 16, 32768), newHost("host-b", 8, 16384))
	r.PlacedVMs = []PlacedVM{placed("vm-noisy", "host-a", map[string]string{"app": "noisy"})}
	r.Policy = policy(v1beta1.VMPlacementPolicySpec{AntiAffinity: &v1beta1.AntiAffinityRules{
		VMAntiAffinity: &v1beta1.VMAntiAffinity{PreferredDuringScheduling: []v1beta1.WeightedVMAffinityTerm{
			{Weight: 100, VMAffinityTerm: v1beta1.VMAffinityTerm{LabelSelector: selEq("app", "noisy"), TopologyKey: "kubernetes.io/hostname"}},
		}},
	}})
	got, err := Schedule(r)
	require.NoError(t, err)
	assert.Equal(t, "host-b", got.HostID)
}

func TestScorePreferredHostAntiAffinityPenalty(t *testing.T) {
	// host-a is the bare-Spread winner (more free memory); a preferred host
	// anti-affinity cap it exceeds penalizes it below host-b.
	r := baseReq(newHost("host-a", 16, 32768), newHost("host-b", 8, 16384))
	r.PlacedVMs = []PlacedVM{placed("vm1", "host-a", nil)}
	r.Policy = policy(v1beta1.VMPlacementPolicySpec{AntiAffinity: &v1beta1.AntiAffinityRules{
		HostAntiAffinity: &v1beta1.HostAntiAffinityRule{Enabled: true, Scope: "preferred", MaxVMsPerHost: p32(1)},
	}})
	got, err := Schedule(r)
	require.NoError(t, err)
	assert.Equal(t, "host-b", got.HostID)
}

func TestStrictHostAntiAffinityDefaultCapIsOne(t *testing.T) {
	// HostAntiAffinity enabled, strict, MaxVMsPerHost unset -> cap defaults to 1,
	// so a host already running one VM is excluded.
	r := baseReq(newHost("host-a", 8, 16384), newHost("host-b", 8, 16384))
	r.PlacedVMs = []PlacedVM{placed("vm1", "host-a", nil)}
	r.Policy = policy(v1beta1.VMPlacementPolicySpec{AntiAffinity: &v1beta1.AntiAffinityRules{
		HostAntiAffinity: &v1beta1.HostAntiAffinityRule{Enabled: true, Scope: scopeStrict},
	}})
	got, err := Schedule(r)
	require.NoError(t, err)
	assert.Equal(t, "host-b", got.HostID)
}

func TestBinPackDeterministicTieBreak(t *testing.T) {
	// Two identical hosts under BinPack, no placed VMs: every axis ties, so the
	// choice is the lowest host id.
	r := Request{
		Resources:  ResourceRequest{CPU: 2, MemoryMiB: 2048},
		Pool:       v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategyBinPack},
		Candidates: []v1beta1.Host{newHost("host-b", 8, 16384), newHost("host-a", 8, 16384)},
	}
	got, err := Schedule(r)
	require.NoError(t, err)
	assert.Equal(t, "host-a", got.HostID)
}

func TestNilAllocatableCPUIsNotBookable(t *testing.T) {
	// A Ready, schedulable host that has not reported allocatable CPU reads as 0
	// capacity and fails fit for any positive request (honesty-first).
	h := v1beta1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: "host-a"},
		Spec:       v1beta1.HostSpec{Schedulable: true},
		Status: v1beta1.HostStatus{
			Health:               v1beta1.HostHealthReady,
			AllocatableMemoryMiB: p64(16384), // CPU deliberately nil
		},
	}
	_, err := Schedule(baseReq(h))
	require.Error(t, err)
	var nfe *NoFeasibleHostError
	require.True(t, errors.As(err, &nfe))
	assert.Contains(t, nfe.Tally(), rejInsufficientCPU)
}

func TestNilAllocatableMemoryIsNotBookable(t *testing.T) {
	h := v1beta1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: "host-a"},
		Spec:       v1beta1.HostSpec{Schedulable: true},
		Status: v1beta1.HostStatus{
			Health:         v1beta1.HostHealthReady,
			AllocatableCPU: p32(64), // memory deliberately nil
		},
	}
	_, err := Schedule(baseReq(h))
	require.Error(t, err)
	var nfe *NoFeasibleHostError
	require.True(t, errors.As(err, &nfe))
	assert.Contains(t, nfe.Tally(), rejInsufficientMem)
}

func TestMalformedSelectorInScoringSurfaces(t *testing.T) {
	// A malformed selector on a PREFERRED term (evaluated only at score time)
	// surfaces as a Schedule error, not a no-fit.
	r := baseReq(newHost("host-a", 8, 16384))
	r.PlacedVMs = []PlacedVM{placed("vm1", "host-a", map[string]string{"app": "db"})}
	r.Policy = policy(v1beta1.VMPlacementPolicySpec{Affinity: &v1beta1.AffinityRules{
		VMAffinity: &v1beta1.VMAffinity{PreferredDuringScheduling: []v1beta1.WeightedVMAffinityTerm{{
			Weight: 10,
			VMAffinityTerm: v1beta1.VMAffinityTerm{
				LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: nil},
				}},
				TopologyKey: "kubernetes.io/hostname",
			},
		}}},
	}})
	_, err := Schedule(r)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNoFeasibleHost))
	assert.Contains(t, err.Error(), "selector")
}

func TestMalformedSelectorInRequiredAffinitySurfaces(t *testing.T) {
	// The required-VM-affinity filter path also surfaces a malformed selector.
	r := baseReq(newHost("host-a", 8, 16384))
	r.PlacedVMs = []PlacedVM{placed("vm1", "host-a", map[string]string{"app": "web"})}
	r.Policy = policy(v1beta1.VMPlacementPolicySpec{Affinity: &v1beta1.AffinityRules{
		VMAffinity: &v1beta1.VMAffinity{RequiredDuringScheduling: []v1beta1.VMAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: nil},
			}},
			TopologyKey: "kubernetes.io/hostname",
		}}},
	}})
	_, err := Schedule(r)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNoFeasibleHost))
	assert.Contains(t, err.Error(), "selector")
}

func TestSpreadBreaksMemoryTieByFreeCPU(t *testing.T) {
	// Equal free memory, different free CPU: Spread favors the larger free CPU.
	r := baseReq(newHost("host-a", 8, 16384), newHost("host-b", 16, 16384))
	got, err := Schedule(r)
	require.NoError(t, err)
	assert.Equal(t, "host-b", got.HostID)
}

func TestBinPackBreaksMemoryTieByFreeCPU(t *testing.T) {
	// Equal free memory, different free CPU: BinPack favors the smaller free CPU.
	r := Request{
		Resources:  ResourceRequest{CPU: 2, MemoryMiB: 2048},
		Pool:       v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategyBinPack},
		Candidates: []v1beta1.Host{newHost("host-a", 8, 16384), newHost("host-b", 16, 16384)},
	}
	got, err := Schedule(r)
	require.NoError(t, err)
	assert.Equal(t, "host-a", got.HostID)
}

func TestMalformedSelectorInPreferredAntiAffinitySurfaces(t *testing.T) {
	// The preferred-VM-anti-affinity score path also surfaces a malformed selector.
	r := baseReq(newHost("host-a", 8, 16384))
	r.PlacedVMs = []PlacedVM{placed("vm1", "host-a", map[string]string{"app": "db"})}
	r.Policy = policy(v1beta1.VMPlacementPolicySpec{AntiAffinity: &v1beta1.AntiAffinityRules{
		VMAntiAffinity: &v1beta1.VMAntiAffinity{PreferredDuringScheduling: []v1beta1.WeightedVMAffinityTerm{{
			Weight: 10,
			VMAffinityTerm: v1beta1.VMAffinityTerm{
				LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: nil},
				}},
				TopologyKey: "kubernetes.io/hostname",
			},
		}}},
	}})
	_, err := Schedule(r)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNoFeasibleHost))
	assert.Contains(t, err.Error(), "selector")
}

func TestPlacedVMWithEmptyHostIDIgnored(t *testing.T) {
	// A PlacedVM with no HostID (e.g. an unbound VM) is ignored for counts and
	// affinity; it must not panic or skew placement.
	r := baseReq(newHost("host-a", 8, 16384))
	r.PlacedVMs = []PlacedVM{placed("vm-unbound", "", map[string]string{"app": "db"})}
	got, err := Schedule(r)
	require.NoError(t, err)
	assert.Equal(t, "host-a", got.HostID)
}
