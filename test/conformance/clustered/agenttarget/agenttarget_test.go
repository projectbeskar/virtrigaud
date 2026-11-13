//go:build virtrigaud_dev

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

package agenttarget_test

import (
	"os"
	"testing"

	"github.com/projectbeskar/virtrigaud/test/conformance/clustered"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered/agenttarget"
)

func TestMain(m *testing.M) {
	if agenttarget.RunChildIfRequested() {
		return
	}
	os.Exit(m.Run())
}

// agentExpectedFailures are the identity cases the per-host agent target
// cannot drive. The waiver is pinned to the outcome, so a different failure,
// or a pass, fails the run.
var agentExpectedFailures = map[string]clustered.ExpectedFailure{
	"identity.outcome-retired": {
		Pointer:  "a per-host agent records RETIRED only when a re-enrolment freezes an operation that never started; the agent process cannot re-enrol yet",
		Contains: "cannot script this outcome: TASK_OUTCOME_RETIRED",
	},
}

// TestAgentConformance runs every identity.* case against real per-host
// agent processes behind the in-test router.
func TestAgentConformance(t *testing.T) {
	clustered.Run(t, func(rt clustered.T) clustered.Target {
		tg, err := agenttarget.New(rt, agenttarget.Config{})
		if err != nil {
			rt.Errorf("start the agents: %v", err)
			rt.FailNow()
		}
		return tg
	}, clustered.OnlyCases("identity."), clustered.WithExpectedFailures(agentExpectedFailures))
}
