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
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/daemon"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/fake"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// signedOpState asks the running agent for a signed answer about an op_id of
// enrolment and returns the answer's record state, or -1 when the answer is
// not signed.
func signedOpState(t *testing.T, r *running, enrolment, opID []byte) providerv1.OperationRecordState {
	t.Helper()
	resp, err := providerv1.NewProviderClient(r.conn()).TaskStatus(callCtx(t), &providerv1.TaskStatusRequest{
		AnswerNonce: bytes.Repeat([]byte{9}, 16), OpId: opID, OpEnrolmentId: enrolment, TargetHostId: trusttest.HostName})
	var blob *providerv1.SignedBlob
	if err == nil {
		blob = resp.GetHostAnswer()
	} else if st, ok := status.FromError(err); ok {
		for _, d := range st.Details() {
			if b, ok := d.(*providerv1.SignedBlob); ok {
				blob = b
			}
		}
	}
	if blob == nil {
		return -1
	}
	a := &providerv1.OperationAnswer{}
	if err := proto.Unmarshal(blob.GetBody(), a); err != nil {
		t.Fatal(err)
	}
	return a.GetState()
}

// TestExpiredCertificateAfterOutage: the host certificate expires while the
// agent is down. The agent refuses to start (nothing serves, so no Provider
// can connect) and does not touch its store. A new enrolment restores it:
// the store re-enrols, the completed operation of the old enrolment is
// answered from its record, the one that was accepted and never started is
// retired, and nothing runs again.
func TestExpiredCertificateAfterOutage(t *testing.T) {
	h := newHost(t)
	exec, err := fake.New(filepath.Join(h.stateDir, config.FakeExecutorDirName))
	if err != nil {
		t.Fatal(err)
	}
	h.exec = exec
	old := h.pki.Record.EnrolmentID
	h.installCert(h.pki.IssueHost(t, h.pki.HostKey, hostidentity.RequestURIs(h.pki.HostURI, h.pki.Record),
		hostidentitytest.IssueOptions{Lifetime: time.Hour}))
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
		return opstore.Request{ProviderUID: trusttest.ProviderUID, EnrolmentID: old, OpID: randID(),
			VMUID: "vm-1", BindingID: binding, Kind: kind, Attempt: 1, IntentDigest: d[:],
			MintEpoch: opstore.Epoch{Counter: h.dev.StartEpoch, Holder: randID()}, Actor: "test"}
	}
	create := req(providerv1.OperationKind_OPERATION_KIND_CREATE)
	if d, err := st.Admit(ctx, create); err != nil || d.Outcome != opstore.OutcomeAccepted {
		t.Fatalf("admit create: %+v %v", d, err)
	}
	if v, err := st.Execute(ctx, opstore.OpRef{EnrolmentID: old, OpID: create.OpID}, nil); err != nil || v.State != opstore.StateCompleted {
		t.Fatalf("execute create: %+v %v", v, err)
	}
	off := req(providerv1.OperationKind_OPERATION_KIND_POWER_OFF)
	if d, err := st.Admit(ctx, off); err != nil || d.Outcome != opstore.OutcomeAccepted {
		t.Fatalf("admit power off: %+v %v", d, err)
	}
	before, err := st.HostView(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	effects, err := fake.Effects(filepath.Join(h.stateDir, config.FakeExecutorDirName))
	if err != nil {
		t.Fatal(err)
	}

	// A long outage: the certificate expired meanwhile.
	h.nowOff.Store(int64(2 * time.Hour))
	r = h.start()
	select {
	case err := <-r.done:
		if !errors.Is(err, daemon.ErrStartRefused) || !strings.Contains(err.Error(), daemon.ReasonIdentityInvalid) {
			t.Fatalf("start with an expired certificate: %v", err)
		}
	case <-r.started:
		t.Fatal("the agent started with an expired certificate")
	case <-time.After(10 * time.Second):
		t.Fatal("the agent neither started nor refused")
	}
	st, err = opstore.Open(ctx, opstore.Options{Journal: journal.Options{Dir: h.stateDir}, Executor: exec})
	if err != nil {
		t.Fatal(err)
	}
	after, err := st.HostView(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if after.Position != before.Position || !bytes.Equal(after.Enrolment.ID, old) {
		t.Fatalf("the refused start changed the store: %+v, was %+v", after, before)
	}

	// Re-enrolment: a new certificate (a new enrolment id), valid now.
	h.installCert(certWithEnrolment(h, 0x99))
	r, _ = h.run()
	if got := answerEnrolment(r); len(got) != hostidentity.IDLen || got[0] != 0x99 {
		t.Fatalf("answer after the re-enrolment names %x", got)
	}
	if s := signedOpState(t, r, old, create.OpID); s != providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED {
		t.Fatalf("the completed op after the re-enrolment: %v", s)
	}
	if s := signedOpState(t, r, old, off.OpID); s != providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED {
		t.Fatalf("the open op after the re-enrolment: %v", s)
	}
	now, err := fake.Effects(filepath.Join(h.stateDir, config.FakeExecutorDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(now) != len(effects) {
		t.Fatalf("effects %d after the re-enrolment, were %d: an operation ran again", len(now), len(effects))
	}
}
