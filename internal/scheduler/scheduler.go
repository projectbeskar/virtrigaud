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

// Package scheduler is the operator-side placement scheduler for ADR-0007's
// clustered/orchestrator provider (P1, decision D4). It exposes a single pure
// function, Schedule, that chooses which Host in a HostPool a VirtualMachine
// should run on.
//
// # The brain owns the decision
//
// ADR-0007 D1 puts every scheduling decision in the operator and none in the
// provider. This package is that decision, factored out as a pure function so it
// is unit-testable without a cluster: no Kubernetes client, no I/O, no clock, no
// package-level mutable state. Everything Schedule needs — the resolved resource
// request, the optional VMPlacementPolicy, the pool policy, the candidate Hosts
// with their live status, the VM's current binding, and the already-placed VMs —
// arrives in the Request; the caller (a later ADR-0007 slice's controller)
// resolves those inputs and records the Result on VirtualMachine.status.placement.
//
// # Filter, then score
//
// Schedule is a deliberately dumb filter+score scheduler (D4: "start deliberately
// dumb"). It first FILTERS candidates down to the feasible set by applying hard
// constraints, then SCORES the survivors by the pool strategy plus soft
// preferences, and picks the best with a deterministic tie-break. See filter.go
// and score.go for the exact predicates and ordering.
//
//   - Filter (hard, all must hold): host is Ready and schedulable (not cordoned);
//     capacity fits after the pool's overcommit ratios; the VMPlacementPolicy hard
//     constraints (host allow/deny list, node-selector against Host.spec.labels,
//     required CPU features, minimum per-host resources); D6 storage/network
//     visibility (the VM's required pool/network as a Host.spec.labels
//     requirement); the required machine type; strict host (anti-)affinity; and
//     required VM (anti-)affinity evaluated against the already-placed VMs.
//   - Score (soft, ranks the survivors): the pool Strategy (Spread favors the most
//     free capacity / fewest bound VMs; BinPack favors the tightest host that
//     still fits), with soft VMPlacementPolicy constraints and preferred
//     (anti-)affinity applied as a preference bonus/penalty that ranks above the
//     raw strategy score. Ties break by ascending host id, so the same inputs
//     always yield the same host.
//
// # Idempotent on re-run
//
// Per D4, Schedule is idempotent: when the VM is already bound (Request.CurrentBinding
// is set) and that host still passes every filter, Schedule re-selects it without
// scoring, so a re-reconcile never churns a healthy placement. The one exception is
// a drained host: a bound host that is now cordoned (schedulable=false) or NotReady
// fails the filter, drops out of the feasible set, and the VM is re-placed
// elsewhere — which is exactly what an operator-initiated host drain needs.
//
// # Scope
//
// This is the pure function and its tests only. Wiring it into the VM controller,
// the target_host_id gRPC field, and writing status.placement are later ADR-0007
// slices. VMPlacementPolicy constructs that do not map onto the flat Host+labels
// model (vSphere/Proxmox datastore/cluster/folder vocabulary, live-utilization
// caps, secure-boot/TPM security constraints) are honored where they map and
// documented where they are deferred; this package invents no new API.
package scheduler

import (
	"fmt"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// Host label conventions from ADR-0007 D6. The VM's required storage pools and
// networks (resolved by the caller from the VM's disks/attachments) become a
// Host.spec.labels requirement: only a host that asserts the matching label with
// value labelValueTrue can see that pool / carry that network, and only such a
// host is a feasible placement.
const (
	// LabelStoragePoolPrefix + pool name is the Host.spec.labels key that asserts
	// the host can see that storage pool, e.g. "storage.virtrigaud.io/pool-nfs01".
	LabelStoragePoolPrefix = "storage.virtrigaud.io/pool-"
	// LabelNetworkPrefix + network name is the Host.spec.labels key that asserts
	// the host has that network, e.g. "net.virtrigaud.io/br-vlan100".
	LabelNetworkPrefix = "net.virtrigaud.io/"
	// labelValueTrue is the value a visibility label must carry to count as present
	// (ADR-0007 D6 examples use "true").
	labelValueTrue = "true"
)

// scopeStrict is the VMPlacementPolicy affinity/anti-affinity Scope value that
// makes a rule a HARD filter. Any other scope (including the empty default and
// "preferred") is treated as a SOFT score preference — the conservative choice, so
// a mis-set scope can never accidentally make every host infeasible.
const scopeStrict = "strict"

// ResourceRequest is the VM's resolved resource demand. The caller resolves these
// numbers from VirtualMachine.spec.resources / the referenced VMClass before
// calling; the scheduler takes plain numbers and does no CRD lookups. Capacity fit
// is checked on CPU and MemoryMiB only (ADR-0007 D4); storage is a visibility
// constraint (D6), not a capacity-fit dimension here.
type ResourceRequest struct {
	// CPU is the number of vCPUs the VM needs.
	CPU int32
	// MemoryMiB is the memory the VM needs, in MiB.
	MemoryMiB int64
}

// PlacedVM is one already-placed VM and where it runs, used to evaluate VM
// affinity/anti-affinity (Request.PlacedVMs) and per-host bound-VM counts. The
// caller supplies the pool's placement set, already namespace-scoped; the
// scheduler matches the policy's VM (anti-)affinity label selectors against each
// PlacedVM's Labels and counts placements per host for the spread/binpack
// tie-break and the host-anti-affinity cap.
type PlacedVM struct {
	// Name is the VM's name (for the decision trace only).
	Name string
	// HostID is the host this VM is bound to (a Host CR name).
	HostID string
	// Labels are the VM's labels, matched by (anti-)affinity selectors.
	Labels map[string]string
}

// Request is the pure, self-contained input to Schedule. It carries no Kubernetes
// client and no I/O handle: the caller resolves everything before calling.
type Request struct {
	// Resources is the VM's resolved CPU/memory demand.
	Resources ResourceRequest

	// Policy is the optional VMPlacementPolicy governing this VM (from
	// spec.placementRef). nil means "no policy": fit + strategy only.
	Policy *v1beta1.VMPlacementPolicy

	// Pool carries the scheduling strategy and overcommit ratios for the HostPool
	// the VM is being placed into.
	Pool v1beta1.HostPoolSpec

	// Candidates are the pool's Hosts with their live status (populated by the
	// inventory-sync controller). The scheduler does not fetch them.
	Candidates []v1beta1.Host

	// CurrentBinding is the host id this VM is already bound to
	// (VirtualMachine.status.placement.host), or "" when unbound. It drives the
	// idempotent re-selection in D4.
	CurrentBinding string

	// PlacedVMs is the pool's already-placed VM set (name -> host + labels), used
	// for VM (anti-)affinity matching and per-host bound-VM counts. Caller-supplied
	// and namespace-scoped.
	PlacedVMs []PlacedVM

	// RequiredStoragePools are the storage pools the VM's disks need (resolved by
	// the caller). Each becomes a required Host.spec.labels visibility check
	// (LabelStoragePoolPrefix + name), ADR-0007 D6.
	RequiredStoragePools []string

	// RequiredNetworks are the networks the VM's attachments need (resolved by the
	// caller). Each becomes a required Host.spec.labels visibility check
	// (LabelNetworkPrefix + name), ADR-0007 D6.
	RequiredNetworks []string

	// RequiredMachineType, when non-empty, requires the host to advertise it in
	// status.machineTypes. Caller-resolved (VMClass/firmware); VMPlacementPolicy
	// carries no machine-type vocabulary, so this is a Request input, not a policy
	// field.
	RequiredMachineType string
}

// Result is the scheduler's decision for a feasible placement.
type Result struct {
	// HostID is the chosen host — the Host CR name to bind the VM to and to send
	// as target_host_id on the wire (a later slice).
	HostID string

	// Reason is a human-readable decision trace suitable for
	// VirtualMachine.status.placement.reason: which strategy ran, the winning
	// host's score inputs, and how many hosts were feasible.
	Reason string
}

// Schedule chooses a Host for the VM described by req. It filters the candidates
// to the feasible set, honors idempotent re-selection of the current binding, then
// scores and picks the best survivor with a deterministic tie-break.
//
// It returns a *NoFeasibleHostError (matching ErrNoFeasibleHost) when no candidate
// survives the filters, with the per-host breakdown of which filter eliminated
// each one. It returns an ordinary error only for a malformed input the caller
// must fix (an unparseable pool overcommit ratio, a malformed affinity label
// selector). It never panics.
func Schedule(req Request) (Result, error) {
	ec, err := newEvalContext(req)
	if err != nil {
		return Result{}, err
	}

	// FILTER: reduce candidates to the feasible set, recording why each rejected
	// host was eliminated so a no-fit is explainable.
	feasible := make([]*v1beta1.Host, 0, len(req.Candidates))
	rejections := make([]HostRejection, 0, len(req.Candidates))
	for i := range req.Candidates {
		h := &req.Candidates[i]
		reason, detail, ferr := ec.filterHost(h)
		if ferr != nil {
			return Result{}, ferr
		}
		if reason == "" {
			feasible = append(feasible, h)
			continue
		}
		rejections = append(rejections, HostRejection{HostID: h.Name, Reason: reason, Detail: detail})
	}

	if len(feasible) == 0 {
		return Result{}, newNoFeasibleHostError(len(req.Candidates), rejections)
	}

	// IDEMPOTENCY (D4): a VM already bound re-selects its current host when that
	// host is still feasible — no scoring, no churn. A drained host (cordoned /
	// NotReady) already failed the filter above and is not in `feasible`, so this
	// naturally falls through to re-placement.
	if req.CurrentBinding != "" {
		for _, h := range feasible {
			if h.Name == req.CurrentBinding {
				return Result{
					HostID: h.Name,
					Reason: fmt.Sprintf("re-selected current binding %q (idempotent; host still feasible among %d of %d candidate host(s))%s",
						h.Name, len(feasible), len(req.Candidates), ec.policySuffix()),
				}, nil
			}
		}
	}

	// SCORE: rank the survivors by pool strategy + soft preferences, deterministic
	// tie-break by host id.
	best, err := ec.pickBest(feasible)
	if err != nil {
		return Result{}, err
	}
	return best, nil
}
