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

package daemon_test

import (
	"context"
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/attest/attesttest"
	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/fake"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestLedgerGate_RestartWithPendingReceipt: an operation accepted before a
// restart whose acceptance the manager's ledger does not cover (its receipt
// was lost with the crash) is recovered but not run when the agent is
// attested below its acceptance; it runs once a ledger mark covers it. A
// development build gates operations on the ledger mark.
func TestLedgerGate_RestartWithPendingReceipt(t *testing.T) {
	h := newHost(t)
	exec, err := fake.New(filepath.Join(h.stateDir, config.FakeExecutorDirName))
	if err != nil {
		t.Fatal(err)
	}
	h.exec = exec
	r, _ := h.run()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	st, err := opstore.Open(ctx, opstore.Options{Journal: journal.Options{Dir: h.stateDir}, Executor: exec})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EstablishContinuity(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	before := st.Position()
	d := sha256.Sum256(randID())
	create := opstore.Request{ProviderUID: trusttest.ProviderUID, EnrolmentID: h.pki.Record.EnrolmentID, OpID: randID(),
		VMUID: "vm-1", BindingID: randID(), Kind: providerv1.OperationKind_OPERATION_KIND_CREATE, Attempt: 1, IntentDigest: d[:],
		MintEpoch: opstore.Epoch{Counter: h.dev.StartEpoch, Holder: randID()}, Actor: "test"}
	if dec, err := st.Admit(ctx, create); err != nil || dec.Outcome != opstore.OutcomeAccepted {
		t.Fatalf("admit: %+v %v", dec, err)
	}
	_ = st.Close()

	h.noAttest = true
	r = h.start()
	select {
	case <-r.awaiting:
	case <-time.After(10 * time.Second):
		t.Fatalf("the agent did not await its attestation\n%s", h.logs.text())
	}
	ledger := &providerv1.LedgerPosition{StoreId: before.StoreID[:], Seq: before.Seq, Head: before.Head[:]}
	a, err := attesttest.AttestAt(callCtx(t), r.conn(), h.grants, trusttest.HostName, ledger)
	if err != nil || a.GetOutcome() != providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED {
		t.Fatalf("attest below the acceptance: %v %v", a, err)
	}
	select {
	case s := <-r.started:
		if s != providerv1.AgentState_AGENT_STATE_READY {
			t.Fatalf("state %v", s)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the agent did not start\n%s", h.logs.text())
	}
	waitLog(t, h, "operation parked by the ledger gate")
	task := server.TaskRef(opstore.OpRef{EnrolmentID: create.EnrolmentID, OpID: create.OpID})
	pc := providerv1.NewProviderClient(r.conn())
	time.Sleep(50 * time.Millisecond)
	ts, err := pc.TaskStatus(callCtx(t), &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: task}})
	if err != nil || ts.GetOutcome() != providerv1.TaskOutcome_TASK_OUTCOME_RUNNING {
		t.Fatalf("a parked operation: %v %v", ts, err)
	}
	if effects, _ := fake.Effects(filepath.Join(h.stateDir, config.FakeExecutorDirName)); len(effects) != 0 {
		t.Fatalf("a parked operation ran: %+v", effects)
	}

	m, err := attesttest.Mark(callCtx(t), r.conn(), h.grants, trusttest.HostName, nil)
	if err != nil || m.GetOutcome() != providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED {
		t.Fatalf("ledger mark: %v %v", m, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ts, err := pc.TaskStatus(callCtx(t), &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: task}})
		if err == nil && ts.GetOutcome() == providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the operation did not run after the mark: %v %v\n%s", ts, err, h.logs.text())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}
