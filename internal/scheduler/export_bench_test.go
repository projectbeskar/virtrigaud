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
	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// This file is a test seam for the Spike 1A phase benchmarks
// (schedule_bench_test.go, package scheduler_test). Those benchmarks live in
// the external test package because their fixture package (schedbench)
// imports this one, and an internal test cannot import it. The seam exposes
// Schedule's three phases separately; it calls the same unexported functions
// Schedule does and adds no logic of its own except PhaseFilter, which repeats
// Schedule's filter loop line for line (scheduler.go, FILTER) so its cost can
// be timed apart from scoring.

// PhaseEval is one Request's evaluation context, built by PhaseIndex.
type PhaseEval struct{ ec *evalContext }

// PhaseIndex is Schedule's first step: newEvalContext (overcommit parsing and
// indexPlaced, which merges and sums Request.PlacedVMs per host).
func PhaseIndex(req Request) (*PhaseEval, error) {
	ec, err := newEvalContext(req)
	if err != nil {
		return nil, err
	}
	return &PhaseEval{ec: ec}, nil
}

// PhaseFilter is Schedule's filter loop: filterHost on every candidate, and the
// capacity shortfall of each capacity rejection. It returns the feasible hosts
// and how many were rejected.
func (p *PhaseEval) PhaseFilter() (feasible []*v1beta1.Host, rejected []HostRejection, err error) {
	req := p.ec.req
	feasible = make([]*v1beta1.Host, 0, len(req.Candidates))
	rejected = make([]HostRejection, 0, len(req.Candidates))
	for i := range req.Candidates {
		h := &req.Candidates[i]
		reason, detail, ferr := p.ec.filterHost(h)
		if ferr != nil {
			return nil, nil, ferr
		}
		if reason == "" {
			feasible = append(feasible, h)
			continue
		}
		rej := HostRejection{HostID: h.Name, Reason: reason, Detail: detail}
		if sf := p.ec.capacityShortfall(h, reason); sf != nil {
			rej.Shortfall = sf
			rej.Detail = sf.String()
		}
		rejected = append(rejected, rej)
	}
	return feasible, rejected, nil
}

// PhaseScore is Schedule's scoring step: pickBest over feasible.
func (p *PhaseEval) PhaseScore(feasible []*v1beta1.Host) (Result, error) {
	return p.ec.pickBest(feasible)
}
