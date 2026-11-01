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
	// Level says whether the rule is stated by the contract (Required) or is
	// good practice (Recommended). The zero value is Required.
	Level Level
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

// ExpectedFailure records a deviation a Target is known to have.
type ExpectedFailure struct {
	// Pointer says where the deviation is recorded (an issue, an ADR section,
	// a note in the adapter).
	Pointer string
	// Contains is a substring the case's failure output must contain, so a
	// different failure of the same case is not mistaken for the known one.
	Contains string
}

type runConfig struct {
	expectedFailures map[string]ExpectedFailure
}

// WithExpectedFailures marks cases a Target is known to fail. Such a case must
// still FAIL, and with a message that contains the entry's Contains: it is
// reported as an expected failure with its pointer, and the run fails if it
// starts passing or fails differently, so the list cannot go stale. It is the
// way to record a genuine contract violation without hiding it and without
// changing the provider in the same change.
func WithExpectedFailures(m map[string]ExpectedFailure) Option {
	return func(c *runConfig) { c.expectedFailures = m }
}

// Run runs the whole suite against Targets from factory, one subtest per
// case. A Required case that fails fails the run. A Recommended case that
// fails is reported, and does not. Every case that does not apply is skipped
// with its reason and every expected failure is reported with its pointer; all
// of them are listed again when the suite ends.
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
	for id, x := range cfg.expectedFailures {
		if !known[id] {
			t.Fatalf("expected failure for unknown case %q", id)
		}
		if x.Pointer == "" || x.Contains == "" {
			t.Fatalf("expected failure for %q needs a pointer and a substring", id)
		}
	}
	var mu sync.Mutex
	notes := map[string]string{}
	note := func(id, text string) {
		mu.Lock()
		notes[id] = text
		mu.Unlock()
	}
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
				note(c.ID, fmt.Sprintf("SKIPPED %s: %s", c.ID, why))
				t.Skipf("%s not applicable to %s: %s", c.ID, tg.Name(), why)
				return
			}
			expect, expected := cfg.expectedFailures[c.ID]
			if !expected && c.Level == Required {
				c.Run(newEnv(t, tg))
				return
			}
			rec := NewRecorder(c.ID)
			out := rec.RunRecorded(func(rt T) { c.Run(newEnv(rt, tg)) })
			failed := strings.Join(strings.Fields(strings.Join(rec.Failures(), " | ")), " ")
			for _, l := range rec.Logs() {
				note(c.ID+"/log", fmt.Sprintf("NOTE %s: %s", c.ID, l))
			}
			switch {
			case out == OutcomeSkipped:
				note(c.ID, fmt.Sprintf("SKIPPED %s: %s", c.ID, rec.SkipReason()))
				t.Skipf("%s skipped: %s", c.ID, rec.SkipReason())
			case expected && out == OutcomePassed:
				t.Errorf("%s is listed as an expected failure (%s) but passes: remove it from the list", c.ID, expect.Pointer)
			case expected && !strings.Contains(failed, expect.Contains):
				t.Errorf("%s is listed as an expected failure (%s: %q) but failed differently: %s", c.ID, expect.Pointer, expect.Contains, failed)
			case expected:
				note(c.ID, fmt.Sprintf("EXPECTED FAILURE %s: %s\n    observed: %s", c.ID, expect.Pointer, failed))
				t.Skipf("expected failure: %s", expect.Pointer)
			case out == OutcomeFailed:
				note(c.ID, fmt.Sprintf("RECOMMENDED FAILED %s: %s\n    observed: %s", c.ID, c.Rule, failed))
				t.Skipf("recommended rule not met: %s", c.Rule)
			}
		})
	}
}
