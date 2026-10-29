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

package scheduler_test

import (
	"errors"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/scheduler"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/schedbench"
)

// Spike 1A scheduler-only benchmarks (docs/performance/spike-1a-scheduler-baseline.md).
// They time the pure Schedule function and its three phases on a Request whose
// PlacedVMs are already built; the committed-placement scan that builds them
// from the informer cache is timed by the controller package's benchmarks.
// Run with hack/bench/spike1a.sh.

// lastFixture memoises the most recent fixture: consecutive scenarios share
// one, and keeping only one bounds the benchmark's live heap.
var lastFixture *schedbench.Fixture

// fixtureFor builds (or reuses) the fixture of cfg.
func fixtureFor(cfg schedbench.Config) *schedbench.Fixture {
	if lastFixture == nil || lastFixture.Config != cfg {
		lastFixture = nil
		lastFixture = schedbench.Build(cfg)
	}
	return lastFixture
}

// scenarioRequest returns the full Request of s: the fixture's candidates and
// pool, the scenario's request changes, and the placed VMs seen from the
// scheduled VM's namespace.
func scenarioRequest(s schedbench.Scenario) scheduler.Request {
	f := fixtureFor(s.Config)
	req := f.Request(s.Class)
	s.Apply(f, &req)
	req.PlacedVMs = f.PlacedVMs(schedbench.SelfNamespace)
	req.VMUID = "new-vm"
	return req
}

// checkOutcome fails b when Schedule's verdict contradicts the scenario.
func checkOutcome(b *testing.B, s schedbench.Scenario, err error) {
	b.Helper()
	switch {
	case err == nil && s.Expect == schedbench.ExpectNoFit:
		b.Fatalf("%s: expected no feasible host, got one", s.Name)
	case err != nil && !errors.Is(err, scheduler.ErrNoFeasibleHost):
		b.Fatalf("%s: Schedule: %v", s.Name, err)
	case err != nil && s.Expect == schedbench.ExpectFit:
		b.Fatalf("%s: expected a feasible host: %v", s.Name, err)
	}
}

// BenchmarkSchedule times one complete Schedule call (index, filter, score).
func BenchmarkSchedule(b *testing.B) {
	for _, s := range schedbench.Scenarios() {
		b.Run(s.Name, func(b *testing.B) {
			req := scenarioRequest(s)
			_, err := scheduler.Schedule(req)
			checkOutcome(b, s, err)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = scheduler.Schedule(req)
			}
		})
	}
}

// BenchmarkSchedulePhases times Schedule's phases separately: index (merge and
// sum PlacedVMs per host), filter (every candidate), score (every feasible
// host).
func BenchmarkSchedulePhases(b *testing.B) {
	for _, s := range schedbench.Scenarios() {
		req := scenarioRequest(s)
		b.Run(s.Name+"/index", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := scheduler.PhaseIndex(req); err != nil {
					b.Fatal(err)
				}
			}
		})
		ev, err := scheduler.PhaseIndex(req)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(s.Name+"/filter", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, err := ev.PhaseFilter(); err != nil {
					b.Fatal(err)
				}
			}
		})
		feasible, _, err := ev.PhaseFilter()
		if err != nil {
			b.Fatal(err)
		}
		if len(feasible) == 0 {
			continue // nothing to score
		}
		b.Run(s.Name+"/score", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := ev.PhaseScore(feasible); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
