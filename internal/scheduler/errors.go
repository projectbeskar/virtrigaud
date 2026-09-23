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
// category name so the same inputs always produce the same message.
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
	return fmt.Sprintf("no feasible host: 0 of %d candidate host(s) passed the filters [%s]",
		e.Candidates, strings.Join(parts, "; "))
}

// Is reports whether target is the ErrNoFeasibleHost sentinel, so
// errors.Is(err, ErrNoFeasibleHost) holds for any *NoFeasibleHostError.
func (e *NoFeasibleHostError) Is(target error) bool {
	return target == ErrNoFeasibleHost
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
