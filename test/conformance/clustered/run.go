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

package clustered

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Case is one conformance rule, checked against a Target.
type Case struct {
	// ID is the stable name of the case ("group.rule"). It is the subtest
	// name and the key of the coverage matrix.
	ID string
	// Rule states what the contract requires, in one line.
	Rule string
	// Source says where the requirement comes from (a proto comment, an ADR
	// section, a contracts doc comment). Every case cites one.
	Source string
	// Requires lists the Features the Target must offer; the case is skipped,
	// with the reason reported, when it does not.
	Requires []Feature
	// Run checks the rule. It fails through e.T.
	Run func(e *Env)
}

// Factory builds a fresh Target for one case. Every case gets its own, so no
// case depends on another's leftovers.
type Factory func(t T) Target

// Cases returns every case of the suite, in a stable order.
func Cases() []Case {
	var all []Case
	all = append(all, routingCases()...)
	all = append(all, ownerCases()...)
	all = append(all, hostDownCases()...)
	all = append(all, taskCases()...)
	all = append(all, listCases()...)
	all = append(all, capabilityCases()...)
	all = append(all, errorMappingCases()...)
	all = append(all, idempotencyCases()...)
	all = append(all, restartCases()...)
	all = append(all, providerDownCases()...)
	all = append(all, hostInventoryCases()...)
	all = append(all, createCases()...)
	return all
}

// SkipReason returns why c does not apply to a Target with capabilities caps,
// or "" when it does.
func (c Case) SkipReason(caps Capabilities) string {
	var missing []string
	for _, f := range c.Requires {
		if caps.Has(f) {
			continue
		}
		why := caps.Reasons[f]
		if why == "" {
			why = "not declared by the target"
		}
		missing = append(missing, fmt.Sprintf("feature %q: %s", f, why))
	}
	return strings.Join(missing, "; ")
}

// RunCase runs c against a Target from factory on t, which may be a
// *testing.T or a Recorder.
func RunCase(t T, factory Factory, c Case) {
	t.Helper()
	tg := factory(t)
	if why := c.SkipReason(tg.Capabilities()); why != "" {
		t.Skipf("%s not applicable to %s: %s", c.ID, tg.Name(), why)
		return
	}
	c.Run(newEnv(t, tg))
}

// Option configures Run.
type Option func(*runConfig)

type runConfig struct {
	expectedFailures map[string]string
}

// WithExpectedFailures marks cases a Target is known to fail, each with a
// pointer to where the deviation is recorded (an issue, an ADR section). Such
// a case must still FAIL: it is reported as an expected failure with its
// pointer, and the run fails if it starts passing, so the list cannot go
// stale. It is the way to record a genuine contract violation without
// hiding it and without changing the provider in the same change.
func WithExpectedFailures(m map[string]string) Option {
	return func(c *runConfig) { c.expectedFailures = m }
}

// Run runs the whole suite against Targets from factory, one subtest per
// case. Every case that does not apply is skipped with its reason, every
// expected failure is reported with its pointer, and both are listed again
// when the suite ends.
func Run(t *testing.T, factory Factory, opts ...Option) {
	t.Helper()
	var cfg runConfig
	for _, o := range opts {
		o(&cfg)
	}
	known := map[string]bool{}
	for _, c := range Cases() {
		known[c.ID] = true
	}
	for id := range cfg.expectedFailures {
		if !known[id] {
			t.Fatalf("expected failure for unknown case %q", id)
		}
	}
	var mu sync.Mutex
	notes := map[string]string{}
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		ids := make([]string, 0, len(notes))
		for id := range notes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			t.Logf("%s", notes[id])
		}
	})
	for _, c := range Cases() {
		t.Run(c.ID, func(t *testing.T) {
			tg := factory(t)
			if why := c.SkipReason(tg.Capabilities()); why != "" {
				mu.Lock()
				notes[c.ID] = fmt.Sprintf("SKIPPED %s: %s", c.ID, why)
				mu.Unlock()
				t.Skipf("%s not applicable to %s: %s", c.ID, tg.Name(), why)
				return
			}
			pointer, expected := cfg.expectedFailures[c.ID]
			if !expected {
				c.Run(newEnv(t, tg))
				return
			}
			rec := NewRecorder(c.ID)
			switch rec.RunRecorded(func(rt T) { c.Run(newEnv(rt, tg)) }) {
			case OutcomePassed:
				t.Errorf("%s is listed as an expected failure (%s) but passes: remove it from the list", c.ID, pointer)
			case OutcomeSkipped:
				t.Skipf("%s skipped: %s", c.ID, rec.SkipReason())
			default:
				mu.Lock()
				notes[c.ID] = fmt.Sprintf("EXPECTED FAILURE %s: %s\n    observed: %s", c.ID, pointer, strings.Join(strings.Fields(strings.Join(rec.Failures(), " | ")), " "))
				mu.Unlock()
				t.Skipf("expected failure: %s", pointer)
			}
		})
	}
}
