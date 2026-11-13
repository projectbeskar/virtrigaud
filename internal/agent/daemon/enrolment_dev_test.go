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

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/fake"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestReenrolmentFreezesTheOpenOperations: a new certificate (a new
// enrolment id) re-enrols a store that holds an operation that was accepted
// and never ran. The old enrolment's open operation is frozen (retired: it
// had no step intent), a terminal one is left as it was, the new enrolment is
// active with the next start epoch, and the old enrolment is frozen.
func TestReenrolmentFreezesTheOpenOperations(t *testing.T) {
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
	binding := randID()
	req := func(kind providerv1.OperationKind) opstore.Request {
		d := sha256.Sum256(randID())
		return opstore.Request{ProviderUID: trusttest.ProviderUID, EnrolmentID: h.pki.Record.EnrolmentID, OpID: randID(),
			VMUID: "vm-1", BindingID: binding, Kind: kind, Attempt: 1, IntentDigest: d[:],
			MintEpoch: opstore.Epoch{Counter: h.dev.StartEpoch, Holder: randID()}, Actor: "test"}
	}
	create := req(providerv1.OperationKind_OPERATION_KIND_CREATE)
	if d, err := st.Admit(ctx, create); err != nil || d.Outcome != opstore.OutcomeAccepted {
		t.Fatalf("admit create: %+v %v", d, err)
	}
	createRef := opstore.OpRef{EnrolmentID: create.EnrolmentID, OpID: create.OpID}
	if v, err := st.Execute(ctx, createRef, nil); err != nil || v.State != opstore.StateCompleted {
		t.Fatalf("execute create: %+v %v", v, err)
	}
	off := req(providerv1.OperationKind_OPERATION_KIND_POWER_OFF)
	if d, err := st.Admit(ctx, off); err != nil || d.Outcome != opstore.OutcomeAccepted {
		t.Fatalf("admit power off: %+v %v", d, err)
	}
	offRef := opstore.OpRef{EnrolmentID: off.EnrolmentID, OpID: off.OpID}
	_ = st.Close()

	h.installCert(certWithEnrolment(h, 0x55))
	r, _ = h.run()
	if got := answerEnrolment(r); len(got) != 16 || got[0] != 0x55 {
		t.Fatalf("answer names %x", got)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}

	st, err = opstore.Open(ctx, opstore.Options{Journal: journal.Options{Dir: h.stateDir}, Executor: exec})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if v, ok, err := st.Lookup(ctx, offRef); err != nil || !ok || v.State != opstore.StateRetired {
		t.Fatalf("the open operation after the re-enrolment: %+v %v %v", v, ok, err)
	}
	if v, ok, err := st.Lookup(ctx, createRef); err != nil || !ok || v.State != opstore.StateCompleted {
		t.Fatalf("the completed operation after the re-enrolment: %+v %v %v", v, ok, err)
	}
	hv, err := st.HostView(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hv.Enrolment == nil || hv.Enrolment.ID[0] != 0x55 || hv.LastStartEpoch != h.dev.StartEpoch+1 {
		t.Fatalf("host view: %+v", hv)
	}
	if st.Freezing() {
		t.Fatal("the freeze is not complete")
	}
}
