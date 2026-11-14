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
	"errors"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/daemon"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/runner"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// panicExec panics in every step.
type panicExec struct{}

func (panicExec) Plan(providerv1.OperationKind) ([]executor.Step, error) {
	return []executor.Step{{Name: "power"}}, nil
}
func (panicExec) Run(context.Context, executor.Operation, executor.Step) error { panic("executor bug") }
func (panicExec) Inspect(context.Context, executor.Operation, executor.Step) (executor.Evidence, error) {
	return executor.EvidenceUnknown, nil
}
func (panicExec) Goal(context.Context, executor.Operation, executor.Step) (bool, error) {
	return false, nil
}
func (panicExec) StopAll(context.Context) error { return nil }

// TestRunPanicStopsTheAgent: a run that panics is recovered and counted, and
// the agent drains and stops with ErrRunPanicked, leaving the operation to the
// next start's recovery. After the limit of panicked runs the next start
// records the operation's outcome as unknown (run_panicked) without running
// it, and the agent stays up: an operation that always panics cannot keep
// the host's agent restarting.
func TestRunPanicStopsTheAgent(t *testing.T) {
	h := newHost(t)
	h.exec = panicExec{}
	r, _ := h.run()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := opstore.Open(ctx, opstore.Options{Journal: journal.Options{Dir: h.stateDir}, Executor: panicExec{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EstablishContinuity(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	d := sha256.Sum256(randID())
	req := opstore.Request{ProviderUID: trusttest.ProviderUID, EnrolmentID: h.pki.Record.EnrolmentID, OpID: randID(),
		VMUID: "vm-1", BindingID: randID(), Kind: providerv1.OperationKind_OPERATION_KIND_CREATE, Attempt: 1, IntentDigest: d[:],
		MintEpoch: opstore.Epoch{Counter: h.dev.StartEpoch, Holder: randID()}, Actor: "test"}
	if dec, err := st.Admit(ctx, req); err != nil || dec.Outcome != opstore.OutcomeAccepted {
		t.Fatalf("admit: %+v %v", dec, err)
	}
	_ = st.Close()

	// Each start runs the operation, which panics: the agent stops. The
	// panic is counted durably each time.
	for i := range runner.DefaultMaxPanics {
		r = h.start()
		select {
		case err := <-r.done:
			if !errors.Is(err, daemon.ErrRunPanicked) {
				t.Fatalf("start %d: Run returned %v, want ErrRunPanicked\n%s", i+1, err, h.logs.text())
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("start %d: the agent did not stop after a run panicked\n%s", i+1, h.logs.text())
		}
	}
	ref := opstore.OpRef{EnrolmentID: req.EnrolmentID, OpID: req.OpID}
	// The next start gives the operation up without running it, and the
	// agent stays up.
	r, state := h.run()
	if state != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("state %v", state)
	}
	waitLog(t, h, "the operation's runs panicked repeatedly")
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	st, err = opstore.Open(ctx, opstore.Options{Journal: journal.Options{Dir: h.stateDir}, Executor: panicExec{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	v, ok, err := st.Lookup(ctx, ref)
	if err != nil || !ok || v.State != opstore.StateOutcomeUnknown || v.Reason != opstore.ReasonRunPanicked {
		t.Fatalf("the operation after %d panics: %+v %v %v", runner.DefaultMaxPanics, v, ok, err)
	}
}
