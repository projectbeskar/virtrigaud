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
	"strconv"
	"strings"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// bytesPerMiB converts resource.Quantity byte values to the MiB unit HostStatus
// reports memory in.
const bytesPerMiB = 1024 * 1024

// evalContext holds the parsed, precomputed inputs Schedule reuses across every
// candidate: the overcommit ratios (parsed once), the pool's placed-VM set indexed
// by host, and the non-nil policy sub-structs. Building it once keeps filterHost
// and pickBest allocation-light and keeps the per-host code free of nil-guards.
type evalContext struct {
	req      Request
	cpuRatio float64
	memRatio float64

	// placedByHost maps a host id to the VMs placed on it that are in the
	// scheduled VM's affinity scope (Request.PlacedVMs not marked CapacityOnly),
	// used for VM (anti-)affinity and bound-VM counts.
	placedByHost map[string][]PlacedVM

	// committedByHost maps a host id to the resources every PlacedVM on it
	// holds (bound, pending or assumed; any namespace), each (UID, host) pair
	// counted once. It is subtracted from the host's effective capacity.
	committedByHost map[string]committed

	// Policy sub-structs, nil when the policy (or that section) is absent.
	hard         *v1beta1.PlacementConstraints
	soft         *v1beta1.PlacementConstraints
	affinity     *v1beta1.AffinityRules
	antiAffinity *v1beta1.AntiAffinityRules
	resources    *v1beta1.ResourceConstraints
}

// committed is the CPU (vCPUs) and memory (MiB) summed over the VMs on one host.
type committed struct {
	cpu    int64
	memMiB int64
}

// placedKey identifies one PlacedVM entry for de-duplication: a VM counts once
// per host.
type placedKey struct {
	uid, host string
}

// newEvalContext parses the request into an evalContext, returning an error for a
// malformed input the caller must fix (an unparseable overcommit ratio). Selector
// parsing happens lazily during filter/score so a malformed selector surfaces with
// its own context.
func newEvalContext(req Request) (*evalContext, error) {
	cpuRatio, memRatio, err := parseOvercommit(req.Pool.Overcommit)
	if err != nil {
		return nil, err
	}

	placedByHost := make(map[string][]PlacedVM, len(req.PlacedVMs))
	committedByHost := make(map[string]committed, len(req.PlacedVMs))
	seen := make(map[placedKey]struct{}, len(req.PlacedVMs))
	for _, p := range req.PlacedVMs {
		if p.HostID == "" {
			continue
		}
		if p.UID != "" {
			// The VM being scheduled never competes with itself (its own
			// binding, pending host or an earlier assumption of its own).
			if p.UID == req.VMUID {
				continue
			}
			// A VM listed twice for one host (its durable record and an
			// assumption not yet cleared, or host == pendingHost) counts once.
			k := placedKey{uid: p.UID, host: p.HostID}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
		}
		c := committedByHost[p.HostID]
		c.cpu += nonNegative(int64(p.Resources.CPU))
		c.memMiB += nonNegative(p.Resources.MemoryMiB)
		committedByHost[p.HostID] = c
		if !p.CapacityOnly {
			placedByHost[p.HostID] = append(placedByHost[p.HostID], p)
		}
	}

	ec := &evalContext{
		req:             req,
		cpuRatio:        cpuRatio,
		memRatio:        memRatio,
		placedByHost:    placedByHost,
		committedByHost: committedByHost,
	}
	if req.Policy != nil {
		ec.hard = req.Policy.Spec.Hard
		ec.soft = req.Policy.Spec.Soft
		ec.affinity = req.Policy.Spec.Affinity
		ec.antiAffinity = req.Policy.Spec.AntiAffinity
		ec.resources = req.Policy.Spec.ResourceConstraints
	}
	return ec, nil
}

// effectiveCapacity returns the host's schedulable CPU and memory AFTER the pool's
// overcommit ratios, before anything committed is subtracted. Host.status
// allocatableCPU / allocatableMemoryMiB are host TOTALS (libvirt: virsh
// nodeinfo), so this is the total the pool lets VMs book. A nil allocatable
// field reads as 0, so an un-synced host fails fit for any positive request
// (honesty-first: unknown capacity is not bookable).
func (ec *evalContext) effectiveCapacity(h *v1beta1.Host) (cpu int64, memMiB int64) {
	cpu = int64(float64(int32Deref(h.Status.AllocatableCPU)) * ec.cpuRatio)
	memMiB = int64(float64(int64Deref(h.Status.AllocatableMemoryMiB)) * ec.memRatio)
	return cpu, memMiB
}

// committedOn returns the resources already committed to the host by bound,
// pending and assumed VMs (Request.PlacedVMs, the scheduled VM excluded).
func (ec *evalContext) committedOn(hostID string) committed {
	return ec.committedByHost[hostID]
}

// freeCapacity returns the host's effective capacity minus what is committed to
// it: the capacity the fit check and the strategy score use. The overcommit
// ratio scales the capacity only, never the committed sum. The result is
// negative when more is committed than the pool now allows (e.g. a lowered
// overcommit ratio); such a host fits nothing.
func (ec *evalContext) freeCapacity(h *v1beta1.Host) (cpu int64, memMiB int64) {
	effCPU, effMem := ec.effectiveCapacity(h)
	c := ec.committedOn(h.Name)
	return effCPU - c.cpu, effMem - c.memMiB
}

// boundCount is the number of VMs in the scheduled VM's affinity scope placed
// on the host (bound, pending or assumed), computed from the authoritative
// Request.PlacedVMs set (the operator owns the binding, ADR-0007 D1). It drives
// the spread/binpack tie-break and the host-anti-affinity VM cap.
func (ec *evalContext) boundCount(hostID string) int {
	return len(ec.placedByHost[hostID])
}

// policySuffix renders a short ", policy <name>" fragment for decision traces, or
// "" when the VM has no placement policy.
func (ec *evalContext) policySuffix() string {
	if ec.req.Policy == nil {
		return ""
	}
	return fmt.Sprintf(", policy %q", ec.req.Policy.Name)
}

// parseOvercommit parses the pool's CPU and memory overcommit ratios, defaulting
// an empty ratio to 1.0 (no overcommit) and rejecting a malformed or non-positive
// one — a silently-wrong capacity multiplier is dangerous in a regulated
// deployment, so it is surfaced as an error rather than defaulted.
func parseOvercommit(o *v1beta1.OvercommitRatios) (cpu, mem float64, err error) {
	cpu, mem = 1.0, 1.0
	if o == nil {
		return cpu, mem, nil
	}
	if cpu, err = parseRatio(o.CPU, "cpu"); err != nil {
		return 0, 0, err
	}
	if mem, err = parseRatio(o.Memory, "memory"); err != nil {
		return 0, 0, err
	}
	return cpu, mem, nil
}

// parseRatio parses one decimal overcommit ratio string. Empty means 1.0.
func parseRatio(s, name string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 1.0, nil
	}
	r, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse pool %s overcommit ratio %q: %w", name, s, err)
	}
	if r <= 0 {
		return 0, fmt.Errorf("pool %s overcommit ratio %q must be > 0", name, s)
	}
	return r, nil
}

// int32Deref returns the pointed-to int32 as an int64, or 0 for a nil pointer.
func int32Deref(p *int32) int64 {
	if p == nil {
		return 0
	}
	return int64(*p)
}

// nonNegative returns v, or 0 when v is negative (a malformed demand never
// frees capacity).
func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// int64Deref returns the pointed-to int64, or 0 for a nil pointer.
func int64Deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// hasLabelValue reports whether m has key set to the exact value v.
func hasLabelValue(m map[string]string, key, v string) bool {
	got, ok := m[key]
	return ok && got == v
}

// containsString reports whether s is in list.
func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
