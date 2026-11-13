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
	"crypto/rand"
	"crypto/sha256"
	"path/filepath"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/fake"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func randID() []byte {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return b
}

// TestRecoverPopulatedStore: an agent started on a store that holds a
// completed operation and an accepted one that never ran recovers it, comes
// up READY, keeps both records as they were (it runs no operation), and
// leaves the fake VM untouched.
func TestRecoverPopulatedStore(t *testing.T) {
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

	// Populate the enrolled store as an agent that executes operations would.
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
	binding := randID()
	req := func(kind providerv1.OperationKind, attempt uint64) opstore.Request {
		d := sha256.Sum256(randID())
		return opstore.Request{ProviderUID: trusttest.ProviderUID, EnrolmentID: h.pki.Record.EnrolmentID, OpID: randID(),
			VMUID: "vm-1", BindingID: binding, Kind: kind, Attempt: attempt, IntentDigest: d[:],
			MintEpoch: opstore.Epoch{Counter: h.dev.StartEpoch, Holder: randID()}, Actor: "test"}
	}
	create := req(providerv1.OperationKind_OPERATION_KIND_CREATE, 1)
	if d, err := st.Admit(ctx, create); err != nil || d.Outcome != opstore.OutcomeAccepted {
		t.Fatalf("admit create: %+v %v", d, err)
	}
	ref := opstore.OpRef{EnrolmentID: create.EnrolmentID, OpID: create.OpID}
	if v, err := st.Execute(ctx, ref, nil); err != nil || v.State != opstore.StateCompleted {
		t.Fatalf("execute create: %+v %v", v, err)
	}
	off := req(providerv1.OperationKind_OPERATION_KIND_POWER_OFF, 1)
	if d, err := st.Admit(ctx, off); err != nil || d.Outcome != opstore.OutcomeAccepted {
		t.Fatalf("admit power off: %+v %v", d, err)
	}
	_ = st.Close()

	r, state := h.run()
	if state != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("state = %v\n%s", state, h.logs.text())
	}
	waitLog(t, h, "operations remain runnable; this agent starts no runner")
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	vms, err := exec.VMs()
	if err != nil || vms["vm-1"].Power != fake.PowerOn {
		t.Fatalf("fake VM changed: %+v %v", vms, err)
	}
	st, err = opstore.Open(ctx, opstore.Options{Journal: journal.Options{Dir: h.stateDir}, Executor: exec})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if v, ok, err := st.Lookup(ctx, ref); err != nil || !ok || v.State != opstore.StateCompleted {
		t.Fatalf("create after restart: %+v %v %v", v, ok, err)
	}
	if v, ok, err := st.Lookup(ctx, opstore.OpRef{EnrolmentID: off.EnrolmentID, OpID: off.OpID}); err != nil || !ok || v.State != opstore.StateAccepted {
		t.Fatalf("power off after restart: %+v %v %v", v, ok, err)
	}
}
