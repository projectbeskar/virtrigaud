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
	"fmt"
	"sort"
	"strings"
)

// ErrNoFeasibleHost is the sentinel that every no-fit outcome matches via
// errors.Is. Callers that only need to branch on "the scheduler found nowhere to
// place this VM" can test errors.Is(err, ErrNoFeasibleHost); callers that want
// the per-host breakdown for an event or a status message use errors.As with
// *NoFeasibleHostError.
var ErrNoFeasibleHost = errors.New("no feasible host")

// HostRejection records why one candidate host was filtered out. Reason is the
// coarse category (a rejectReason* constant) so a caller can tally causes; Detail
// optionally names the specific item that failed (a feature, a pool, a VM) for a
// human reading the trace. Neither ever carries a credential or other secret
// (ADR-0007 Security) — a Host's schedulable facts are capacity, labels, and
// health only.
type HostRejection struct {
	// HostID is the rejected host (its Host CR name).
	HostID string
	// Reason is the coarse rejection category.
	Reason string
	// Detail optionally names the specific item that caused the rejection.
	Detail string
	// Shortfall is set for an insufficient-CPU or insufficient-memory
	// rejection: the request, the host's effective capacity and what is
	// already committed to it.
	Shortfall *CapacityShortfall
}

// CapacityShortfall is the arithmetic behind an insufficient-capacity
// rejection, in the unit of the resource (vCPUs or MiB). It carries numbers
// only — never the names of the VMs that make up Committed, which may belong to
// other tenants.
type CapacityShortfall struct {
	// Requested is the scheduled VM's demand.
	Requested int64
	// Capacity is the host's effective capacity: its allocatable total times
	// the pool's overcommit ratio.
	Capacity int64
	// Committed is what bound, pending and assumed VMs already hold on the
	// host.
	Committed int64
}

// Free is Capacity minus Committed. It is negative when more is committed than
// the pool now allows (e.g. after its overcommit ratio was lowered).
func (s CapacityShortfall) Free() int64 {
	return s.Capacity - s.Committed
}

// String renders the shortfall, e.g. "requested 4, free 2 (committed 14 of 16)".
func (s CapacityShortfall) String() string {
	return fmt.Sprintf("requested %d, free %d (committed %d of %d)", s.Requested, max(s.Free(), 0), s.Committed, s.Capacity)
}

// NoFeasibleHostError is the typed "nowhere to place this VM" error. It carries
// the full filter breakdown — how many candidates were considered, which host
// each filter eliminated, and a per-category tally — so the caller can surface an
// actionable reason (which filter eliminated everyone) on VM.status /events
// rather than a bare "unschedulable". It matches ErrNoFeasibleHost via errors.Is.
type NoFeasibleHostError struct {
	// Candidates is the number of hosts the scheduler was given.
	Candidates int
	// Rejections is the per-host breakdown, sorted by host id for determinism.
	Rejections []HostRejection
}

// Tally returns the rejection counts grouped by coarse reason category. The map
// is convenient for metrics or a compact event; the deterministic string form is
// in Error.
func (e *NoFeasibleHostError) Tally() map[string]int {
	out := make(map[string]int, len(e.Rejections))
	for _, r := range e.Rejections {
		out[r.Reason]++
	}
	return out
}

// Error renders a deterministic, secret-free summary: the count that passed (zero,
// by definition) out of the candidates, plus the per-category tally sorted by
// category name so the same inputs always produce the same message. When hosts
// were rejected for capacity, one clause per resource adds the arithmetic of the
// host with the most free capacity of that resource (CapacitySummary). The
// message is bounded — its size does not grow with the number of hosts or VMs —
// and names no host and no VM, so it can be shown to the VM's owner.
func (e *NoFeasibleHostError) Error() string {
	if e.Candidates == 0 {
		return "no feasible host: pool has no candidate hosts"
	}
	tally := e.Tally()
	cats := make([]string, 0, len(tally))
	for c := range tally {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	parts := make([]string, 0, len(cats))
	for _, c := range cats {
		parts = append(parts, fmt.Sprintf("%s: %d", c, tally[c]))
	}
	msg := fmt.Sprintf("no feasible host: 0 of %d candidate host(s) passed the filters [%s]",
		e.Candidates, strings.Join(parts, "; "))
	if summary := e.CapacitySummary(); summary != "" {
		msg += "; " + summary
	}
	return msg
}

// capacityResources are the capacity rejection categories CapacitySummary
// reports, in order, with the noun and unit each is rendered with.
var capacityResources = []struct {
	reason, noun, unit string
}{
	{reason: rejInsufficientCPU, noun: "CPU", unit: "vCPU"},
	{reason: rejInsufficientMem, noun: "memory", unit: "MiB"},
}

// CapacitySummary renders, for each resource that eliminated at least one host
// on capacity, how many hosts it eliminated and the arithmetic of the one with
// the most free capacity of it, e.g. "insufficient CPU on 3 of 3 candidate
// host(s): requested 4 vCPU, at most 2 free (committed 14 of 16 after
// overcommit)". It is "" when no host was rejected for capacity. Hosts are
// scanned in host-id order, so ties resolve deterministically.
func (e *NoFeasibleHostError) CapacitySummary() string {
	var clauses []string
	for _, res := range capacityResources {
		var (
			hosts int
			best  *CapacityShortfall
		)
		for i := range e.Rejections {
			r := &e.Rejections[i]
			if r.Reason != res.reason || r.Shortfall == nil {
				continue
			}
			hosts++
			if best == nil || r.Shortfall.Free() > best.Free() {
				best = r.Shortfall
			}
		}
		if best == nil {
			continue
		}
		clauses = append(clauses, fmt.Sprintf(
			"insufficient %s on %d of %d candidate host(s): requested %d %s, at most %d free (committed %d of %d after overcommit)",
			res.noun, hosts, e.Candidates, best.Requested, res.unit, max(best.Free(), 0), best.Committed, best.Capacity))
	}
	return strings.Join(clauses, "; ")
}

// InsufficientCapacity reports whether err is a no-fit in which at least one
// candidate host was rejected for insufficient CPU or memory, i.e. capacity
// (possibly committed to other VMs) is part of why the VM cannot be placed.
func InsufficientCapacity(err error) bool {
	var nf *NoFeasibleHostError
	if !errors.As(err, &nf) {
		return false
	}
	for _, r := range nf.Rejections {
		if r.Reason == rejInsufficientCPU || r.Reason == rejInsufficientMem {
			return true
		}
	}
	return false
}

// Is reports whether target is the ErrNoFeasibleHost sentinel, so
// errors.Is(err, ErrNoFeasibleHost) holds for any *NoFeasibleHostError.
func (e *NoFeasibleHostError) Is(target error) bool {
	return target == ErrNoFeasibleHost
}

// AllExcluded reports whether err is a no-fit in which EVERY candidate host was
// rejected because the caller excluded it (Request.ExcludedHosts,
// RejectionExcludedForVM). It is false for an empty pool, for any other error,
// and when at least one candidate was rejected for another reason (capacity,
// health, policy, ...) — that is an ordinary no-fit which may clear by itself.
func AllExcluded(err error) bool {
	var nf *NoFeasibleHostError
	if !errors.As(err, &nf) || nf.Candidates == 0 || len(nf.Rejections) != nf.Candidates {
		return false
	}
	for _, r := range nf.Rejections {
		if r.Reason != RejectionExcludedForVM {
			return false
		}
	}
	return true
}

// newNoFeasibleHostError builds the typed error from the collected rejections,
// sorting them by host id so the error is deterministic regardless of candidate
// input order.
func newNoFeasibleHostError(candidates int, rejections []HostRejection) *NoFeasibleHostError {
	sorted := make([]HostRejection, len(rejections))
	copy(sorted, rejections)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].HostID < sorted[j].HostID })
	return &NoFeasibleHostError{Candidates: candidates, Rejections: sorted}
}
