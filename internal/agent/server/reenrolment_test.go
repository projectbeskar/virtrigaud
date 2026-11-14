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
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// reenrol gives the host a certificate for a new enrolment id (same host,
// key and Provider), re-enrols the store with the next start epoch, and puts
// the new certificate in use, as the daemon does on a reload. It returns the
// new enrolment id.
func (e *opsEnv) reenrol() []byte {
	e.t.Helper()
	rec := e.pki.Record
	rec.EnrolmentID = randID()
	pem := e.pki.IssueHost(e.t, e.pki.HostKey, hostidentity.RequestURIs(e.pki.HostURI, rec), hostidentitytest.IssueOptions{})
	dir := filepath.Join(e.t.TempDir(), "pki")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, trust.FileHostCert), pem, 0o600); err != nil {
		e.t.Fatal(err)
	}
	cur := e.creds.Load()
	id, err := trust.LoadIdentity(dir, cur.Trust, time.Now(), nil, cur.Identity.Signer)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.store.Reenrol(ctx(e.t), opstore.Enrolment{ID: rec.EnrolmentID, ProviderUID: rec.ProviderUID,
		HostID: trusttest.HostName, InstallID: rec.InstallID, StartEpoch: startEpoch + 1}); err != nil {
		e.t.Fatal(err)
	}
	e.creds.Store(&server.Credentials{Identity: id, Trust: cur.Trust})
	return rec.EnrolmentID
}

// admitOnly records o in the store as accepted without running it: an
// operation the agent accepted and had not started when the host was
// re-enrolled.
func (e *opsEnv) admitOnly(o *op) {
	e.t.Helper()
	r := opstore.Request{ProviderUID: o.id.ProviderUID, EnrolmentID: o.id.EnrolmentID, OpID: o.id.OpID, VMUID: o.id.VMUID,
		BindingID: o.id.BindingID, Kind: o.id.Kind, Attempt: o.id.Attempt, IntentDigest: grantverify.IntentDigest(o.salt, o.request),
		MintEpoch: opstore.Epoch{Counter: o.id.MintEpoch.Counter, Holder: o.id.MintEpoch.Holder}, Actor: "test"}
	if dec, err := e.store.Admit(ctx(e.t), r); err != nil || dec.Outcome != opstore.OutcomeAccepted {
		e.t.Fatalf("admit: %+v %v", dec, err)
	}
}

// TestReenrolment_OldEnrolmentAnsweredUntilFreezeConfirmed: after a
// re-enrolment, operations of the old enrolment are answered from their
// records (signed, under the new enrolment) until the freeze is confirmed:
// a completed one stays completed, and one that was accepted but never
// started is retired. A replay of an old enrolment's grant is answered from
// the record and never runs again. Once the freeze is confirmed, the same
// op_ids are UNKNOWN_TO_AGENT, never a definite not-performed, and a replay
// still has no effect.
func TestReenrolment_OldEnrolmentAnsweredUntilFreezeConfirmed(t *testing.T) {
	e := newOpsEnv(t)
	old := e.pki.Record.EnrolmentID
	binding := randID()
	done := e.newOp(kCreate, "vm-1", binding, 1, createReq("vm-1", nil))
	task, err := e.send(done)
	if err != nil {
		t.Fatal(err)
	}
	if st := e.waitDone(task); st.GetOutcome() != providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED {
		t.Fatalf("create: %v", st)
	}
	open := e.newOp(kOff, "vm-1", binding, 1, powerReq("vm-1", providerv1.PowerOp_POWER_OP_OFF))
	e.admitOnly(open)
	effects := e.exec.total()

	next := e.reenrol()

	// Before the freeze is confirmed: the records answer, signed, and the
	// answer names the new enrolment.
	_, a, err := e.signedStatus(done, nonce(1))
	if err != nil || a.GetState() != providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED ||
		!bytes.Equal(a.GetContext().GetEnrolmentId(), next) {
		t.Fatalf("completed op before the confirmation: %v %v", a, err)
	}
	_, a, err = e.signedStatus(open, nonce(2))
	if a.GetState() != providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED {
		t.Fatalf("open op before the confirmation: %v %v", a, err)
	}
	// A replay of the old enrolment's grants: answered from the record (the
	// completed Create's task reference), nothing runs.
	if again, err := e.send(done); err != nil || again != task {
		t.Fatalf("replay of the completed op: %q %v, want %q", again, err, task)
	}
	// The retired op: answered from its record with its task reference,
	// whose status is the recorded terminal one; it is not admitted again.
	retiredTask, err := e.send(open)
	if err != nil {
		t.Fatalf("replay of the retired op: %v", err)
	}
	if st := e.waitDone(retiredTask); !st.GetDone() || st.GetOutcome() != providerv1.TaskOutcome_TASK_OUTCOME_RETIRED ||
		st.GetOutcomeReason() != string(opstore.ReasonProviderChanged) {
		t.Fatalf("the retired op's status after its replay: %v, want done, RETIRED, reason %s", st, opstore.ReasonProviderChanged)
	}
	if _, a, _ := e.signedStatus(open, nonce(3)); a.GetState() != providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED {
		t.Fatalf("the retired op after its replay: %v", a)
	}
	if n := e.exec.total(); n != effects {
		t.Fatalf("effects = %d after the replays, want %d", n, effects)
	}

	// The manager confirms the freeze: the old enrolment's records are
	// discarded.
	if err := e.store.ConfirmFreeze(ctx(t), old); err != nil {
		t.Fatal(err)
	}
	for i, o := range []*op{done, open} {
		_, a, err := e.signedStatus(o, nonce(byte(10+i)))
		if status.Code(err) != codes.NotFound || reasonOf(err) != contracts.UnknownToAgentReason ||
			a.GetState() != providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNKNOWN_TO_AGENT {
			t.Fatalf("op %d after the confirmation: %v %v", i, a, err)
		}
		_, err = e.send(o)
		if err == nil || contracts.IsDefiniteNotPerformedReason(reasonOf(err)) {
			t.Fatalf("replay %d after the confirmation: %v, want a refusal that is not definite", i, err)
		}
	}
	if n := e.exec.total(); n != effects {
		t.Fatalf("effects = %d after the confirmation, want %d", n, effects)
	}
}
