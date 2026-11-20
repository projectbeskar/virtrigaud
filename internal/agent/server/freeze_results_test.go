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

package server_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// stubConfirmer answers ConfirmFrozen with fixed outcomes and records the
// calls.
type stubConfirmer struct {
	mu       sync.Mutex
	outcomes []opstore.FreezeOutcome
	err      error
	calls    int
}

func (s *stubConfirmer) ConfirmFrozen(_ context.Context, _ []byte, _ [][]byte) ([]opstore.FreezeOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return append([]opstore.FreezeOutcome(nil), s.outcomes...), s.err
}

func (s *stubConfirmer) set(err error, outcomes ...opstore.FreezeOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes, s.err = outcomes, err
}

func (s *stubConfirmer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestAgentStatus_FreezeResults: AgentStatus answers each confirmed id with
// the store's outcome, in order and echoing the id. A refusal over capacity
// is reported as such (and logged), never as applied; while the agent is
// not ready every id is deferred and the store is not asked; a store that
// answers with the wrong number of outcomes, or fails, reports nothing as
// applied that it did not vouch for; a request without ids, or with too
// many, gets no results.
func TestAgentStatus_FreezeResults(t *testing.T) {
	stub := &stubConfirmer{}
	logs := &logLines{}
	e := newEnvWith(t, server.Throttle{}, func(o *server.Options) {
		o.Freezes = stub
		o.Log = funcrLogger(logs)
	})
	cc := e.dial(e.provider(), 0)
	t.Cleanup(func() { _ = cc.Close() })
	client := agentv1.NewAgentServiceClient(cc)
	cur := e.pki.Record.EnrolmentID
	a, b, c := randID(), randID(), randID()
	status := func(ids ...[]byte) []agentv1.FreezeConfirmationOutcome {
		t.Helper()
		resp, err := client.AgentStatus(ctx(t), &agentv1.AgentStatusRequest{AnswerNonce: nonce(0x44),
			ConfirmForEnrolmentId: cur, ConfirmFrozenEnrolmentIds: ids})
		if err != nil {
			t.Fatalf("AgentStatus: %v", err)
		}
		var out []agentv1.FreezeConfirmationOutcome
		for i, r := range resp.GetFreezeResults() {
			if i >= len(ids) || !bytes.Equal(r.GetEnrolmentId(), ids[i]) {
				t.Fatalf("result %d does not echo its id", i)
			}
			out = append(out, r.GetOutcome())
		}
		return out
	}

	stub.set(nil, opstore.FreezeAccepted, opstore.FreezeRefusedOverCapacity, opstore.FreezeAlreadyApplied)
	wantOutcomes(t, "ready", status(a, b, c), fcAccepted, fcOverCapacity, fcAlready)
	var refusedLogged bool
	for _, l := range logs.all() {
		refusedLogged = refusedLogged || (strings.Contains(l, "freeze confirmation refused") && strings.Contains(l, hex.EncodeToString(b)))
	}
	if !refusedLogged {
		t.Fatal("the refusal over capacity is not logged with its enrolment")
	}

	stub.set(nil, opstore.FreezeNotApplicable, opstore.FreezeDeferred)
	wantOutcomes(t, "not applicable and deferred", status(a, b), fcNotApplicable, fcDeferred)

	stub.set(nil, opstore.FreezeAccepted)
	wantOutcomes(t, "a short answer from the store", status(a, b), fcDeferred, fcDeferred)

	stub.set(errors.New("disk"), opstore.FreezeAccepted, opstore.FreezeDeferred)
	wantOutcomes(t, "a store that failed partway", status(a, b), fcAccepted, fcDeferred)

	wantOutcomes(t, "no ids", status())
	many := make([][]byte, opstore.MaxFreezeConfirmations+1)
	for i := range many {
		many[i] = randID()
	}
	before := stub.callCount()
	wantOutcomes(t, "too many ids", status(many...))

	e.agent.mu.Lock()
	e.agent.state = providerv1.AgentState_AGENT_STATE_UNATTESTED
	e.agent.mu.Unlock()
	stub.set(nil, opstore.FreezeAccepted, opstore.FreezeAccepted)
	wantOutcomes(t, "not ready", status(a, b), fcDeferred, fcDeferred)
	if n := stub.callCount(); n != before {
		t.Fatalf("the store was asked %d times for requests it must not apply", n-before)
	}
}
