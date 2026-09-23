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
	"fmt"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// Soft-preference weights. The preference score is the PRIMARY ranking axis (a
// satisfied soft preference ranks a host above raw strategy packing), so these are
// only ever compared against each other, never against a capacity value. VM
// (anti-)affinity PreferredDuringScheduling terms additionally contribute their own
// API-defined Weight (1..100).
const (
	// scoreHostAffinityPreferred rewards a host in Affinity.HostAffinity.PreferredHosts.
	scoreHostAffinityPreferred int64 = 100
	// scoreHostAntiAffinityPenalty penalizes a host at/over the HostAntiAffinity cap.
	scoreHostAntiAffinityPenalty int64 = 100
	// scoreSoftHost rewards Soft.Hosts membership and penalizes Soft.ExcludedHosts.
	scoreSoftHost int64 = 50
	// scoreSoftNodeSelectorPerHit rewards each matching Soft.NodeSelector entry.
	scoreSoftNodeSelectorPerHit int64 = 10
	// scorePreferredFeaturePerHit rewards each ResourceConstraints.PreferredFeatures hit.
	scorePreferredFeaturePerHit int64 = 10
)

// hostScore is the computed ranking input for one feasible host.
type hostScore struct {
	host       *v1beta1.Host
	preference int64
	freeCPU    int64
	freeMem    int64
	boundVMs   int
}

// pickBest scores every feasible host and returns the winner with a decision
// trace. Selection is a single deterministic min-scan under a total ordering whose
// final tie-break is the host id, so the same inputs always yield the same host
// regardless of candidate order.
func (ec *evalContext) pickBest(feasible []*v1beta1.Host) (Result, error) {
	binPack := ec.req.Pool.Strategy == v1beta1.PoolStrategyBinPack

	var best *hostScore
	for _, h := range feasible {
		pref, err := ec.computePreference(h)
		if err != nil {
			return Result{}, err
		}
		cpu, mem := ec.effectiveCapacity(h)
		s := &hostScore{
			host:       h,
			preference: pref,
			freeCPU:    cpu,
			freeMem:    mem,
			boundVMs:   ec.boundCount(h.Name),
		}
		if best == nil || betterScore(s, best, binPack) {
			best = s
		}
	}

	return Result{
		HostID: best.host.Name,
		Reason: fmt.Sprintf(
			"selected host %q via %s strategy (freeCPU=%d, freeMemMiB=%d, boundVMs=%d, preferenceScore=%d) from %d feasible of %d candidate host(s)%s",
			best.host.Name, strategyName(binPack), best.freeCPU, best.freeMem, best.boundVMs,
			best.preference, len(feasible), len(ec.req.Candidates), ec.policySuffix()),
	}, nil
}

// betterScore reports whether a should rank ahead of b. Preference is the primary
// axis (higher wins); then the strategy axis (Spread favors more free capacity and
// fewer bound VMs, BinPack favors the tightest host that still fits); then the host
// id ascending, which makes the ordering total and the choice deterministic.
func betterScore(a, b *hostScore, binPack bool) bool {
	if a.preference != b.preference {
		return a.preference > b.preference
	}
	if binPack {
		if a.freeMem != b.freeMem {
			return a.freeMem < b.freeMem
		}
		if a.freeCPU != b.freeCPU {
			return a.freeCPU < b.freeCPU
		}
		if a.boundVMs != b.boundVMs {
			return a.boundVMs > b.boundVMs
		}
	} else {
		if a.freeMem != b.freeMem {
			return a.freeMem > b.freeMem
		}
		if a.freeCPU != b.freeCPU {
			return a.freeCPU > b.freeCPU
		}
		if a.boundVMs != b.boundVMs {
			return a.boundVMs < b.boundVMs
		}
	}
	return a.host.Name < b.host.Name
}

// strategyName renders the strategy for the decision trace.
func strategyName(binPack bool) string {
	if binPack {
		return v1beta1.PoolStrategyBinPack
	}
	return v1beta1.PoolStrategySpread
}

// computePreference sums the soft-constraint and preferred-affinity score for a
// host. It returns an error only for a malformed affinity label selector.
func (ec *evalContext) computePreference(h *v1beta1.Host) (int64, error) {
	var pref int64
	pref += ec.softConstraintScore(h)
	affPref, err := ec.affinityPreferenceScore(h)
	if err != nil {
		return 0, err
	}
	pref += affPref
	return pref, nil
}

// softConstraintScore scores VMPlacementPolicy.Soft and PreferredFeatures for a
// host: Soft.Hosts / ExcludedHosts membership, per-entry Soft.NodeSelector matches,
// and per-feature ResourceConstraints.PreferredFeatures hits.
func (ec *evalContext) softConstraintScore(h *v1beta1.Host) int64 {
	var score int64
	if ec.soft != nil {
		if containsString(ec.soft.Hosts, h.Name) {
			score += scoreSoftHost
		}
		if containsString(ec.soft.ExcludedHosts, h.Name) {
			score -= scoreSoftHost
		}
		for k, v := range ec.soft.NodeSelector {
			if hasLabelValue(h.Spec.Labels, k, v) {
				score += scoreSoftNodeSelectorPerHit
			}
		}
	}
	if ec.resources != nil {
		for _, feat := range ec.resources.PreferredFeatures {
			if containsString(h.Status.CPUFeatures, feat) {
				score += scorePreferredFeaturePerHit
			}
		}
	}
	return score
}

// affinityPreferenceScore scores the preferred (soft) host and VM
// (anti-)affinity for a host. Strict host (anti-)affinity is a hard filter
// (filter.go), so a strict rule's survivors carry a uniform bonus/no-penalty here
// and the term is effectively a no-op at score time.
func (ec *evalContext) affinityPreferenceScore(h *v1beta1.Host) (int64, error) {
	var score int64

	if ec.affinity != nil {
		if ha := ec.affinity.HostAffinity; ha != nil && ha.Enabled && containsString(ha.PreferredHosts, h.Name) {
			score += scoreHostAffinityPreferred
		}
		if va := ec.affinity.VMAffinity; va != nil {
			for _, wt := range va.PreferredDuringScheduling {
				ok, err := ec.hostRunsMatchingVM(h.Name, wt.VMAffinityTerm)
				if err != nil {
					return 0, err
				}
				if ok {
					score += int64(wt.Weight)
				}
			}
		}
	}

	if ec.antiAffinity != nil {
		if haa := ec.antiAffinity.HostAntiAffinity; haa != nil && haa.Enabled {
			if ec.boundCount(h.Name) >= hostAntiAffinityCap(haa) {
				score -= scoreHostAntiAffinityPenalty
			}
		}
		if vaa := ec.antiAffinity.VMAntiAffinity; vaa != nil {
			for _, wt := range vaa.PreferredDuringScheduling {
				ok, err := ec.hostRunsMatchingVM(h.Name, wt.VMAffinityTerm)
				if err != nil {
					return 0, err
				}
				if ok {
					score -= int64(wt.Weight)
				}
			}
		}
	}

	return score, nil
}
