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
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	kCreate = providerv1.OperationKind_OPERATION_KIND_CREATE
	kDelete = providerv1.OperationKind_OPERATION_KIND_DELETE
	kOn     = providerv1.OperationKind_OPERATION_KIND_POWER_ON
	kOff    = providerv1.OperationKind_OPERATION_KIND_POWER_OFF
)

// verifyBlob checks a signed answer against the host identity issuer and
// decodes its body into m, refusing unknown fields.
func verifyBlob(t testing.TB, e *opsEnv, blob *providerv1.SignedBlob, bodyType string, m proto.Message) {
	t.Helper()
	if blob == nil {
		t.Fatal("no signed answer")
	}
	if blob.GetBodyType() != bodyType {
		t.Fatalf("body type %q, want %q", blob.GetBodyType(), bodyType)
	}
	cert, err := x509.ParseCertificate(blob.GetSignerCertificate())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: e.pki.HostCAPool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatal(err)
	}
	in, err := answer.SignatureInput(blob.GetBodyType(), blob.GetBody())
	if err != nil {
		t.Fatal(err)
	}
	d := sha256.Sum256(in)
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !ecdsa.VerifyASN1(pub, d[:], blob.GetSignature()) {
		t.Fatal("the answer's signature does not verify")
	}
	if err := proto.Unmarshal(blob.GetBody(), m); err != nil {
		t.Fatal(err)
	}
	if len(m.ProtoReflect().GetUnknown()) != 0 {
		t.Fatal("unknown fields in the answer")
	}
}

// signedStatus asks for a signed answer about o.
func (e *opsEnv) signedStatus(o *op, n []byte) (*providerv1.TaskStatusResponse, *providerv1.OperationAnswer, error) {
	e.t.Helper()
	resp, err := e.client().TaskStatus(ctx(e.t), &providerv1.TaskStatusRequest{AnswerNonce: n, OpId: o.id.OpID,
		OpEnrolmentId: o.id.EnrolmentID, TargetHostId: trusttest.HostName})
	a := &providerv1.OperationAnswer{}
	var blob *providerv1.SignedBlob
	if err == nil {
		blob = resp.GetHostAnswer()
	} else {
		st, _ := status.FromError(err)
		for _, d := range st.Details() {
			if b, ok := d.(*providerv1.SignedBlob); ok {
				blob = b
			}
		}
	}
	verifyBlob(e.t, e, blob, answer.BodyTypeOperationAnswer, a)
	if !bytes.Equal(a.GetContext().GetNonce(), n) {
		e.t.Fatal("the answer does not echo the request's nonce")
	}
	return resp, a, err
}

// TestOperation_AcceptRunAnswer: a signed Create is accepted, run once and
// answered; a re-delivery of the same attempt gets the same task reference
// and has no second effect; TaskStatus answers by task reference and, signed,
// by op_id, from the operation's tombstone once it is over.
func TestOperation_AcceptRunAnswer(t *testing.T) {
	e := newOpsEnv(t)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	task, err := e.send(o)
	if err != nil {
		t.Fatal(err)
	}
	if st := e.waitDone(task); st.GetOutcome() != providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED || !st.GetDone() {
		t.Fatalf("status: %v", st)
	}
	again, err := e.send(o)
	if err != nil || again != task {
		t.Fatalf("re-delivery: %q %v, want %q", again, err, task)
	}
	if n := e.exec.total(); n != 2 {
		t.Fatalf("effects = %d, want define and start once each", n)
	}
	resp, a, err := e.signedStatus(o, nonce(5))
	if err != nil {
		t.Fatal(err)
	}
	if a.GetState() != providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED || !a.GetTombstoned() ||
		!bytes.Equal(a.GetOpId(), o.id.OpID) || a.GetAttempt() != 1 || a.GetVmUid() != "vm-1" ||
		!bytes.Equal(a.GetBindingId(), o.id.BindingID) || a.GetKind() != int32(kCreate) ||
		a.GetContext().GetAgentState() != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("answer: %v", a)
	}
	if resp.GetOutcome() != providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED {
		t.Fatalf("unsigned fields: %v", resp)
	}
	// An op_id of the active enrolment the host never saw: NO_RECORD, NotFound
	// with UNKNOWN_TO_AGENT, the signed answer as a status detail.
	unknown := e.newOp(kOn, "vm-1", o.id.BindingID, 1, powerReq("vm-1", providerv1.PowerOp_POWER_OP_ON))
	_, a, err = e.signedStatus(unknown, nonce(6))
	if status.Code(err) != codes.NotFound || reasonOf(err) != contracts.UnknownToAgentReason ||
		a.GetState() != providerv1.OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD || a.GetKind() != 0 {
		t.Fatalf("unknown op: %v %v", err, a)
	}
}

// TestOperation_LookupBeforeEnrolment: a grant naming an enrolment the store
// has no record of is UNKNOWN_TO_AGENT (never a definite refusal), and a known
// op_id is answered from the store whatever enrolment check would refuse.
func TestOperation_LookupBeforeEnrolment(t *testing.T) {
	e := newOpsEnv(t)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	o.id.EnrolmentID = randID()
	_, err := e.send(o)
	if status.Code(err) != codes.NotFound || reasonOf(err) != contracts.UnknownToAgentReason {
		t.Fatalf("unknown enrolment: %v, want NotFound UNKNOWN_TO_AGENT", err)
	}
	if contracts.IsDefiniteNotPerformedReason(reasonOf(err)) {
		t.Fatal("an enrolment the host holds no record of must never be refused as definitely not performed")
	}
	if n := e.exec.total(); n != 0 {
		t.Fatalf("effects = %d", n)
	}
}

// TestOperation_AdversarialGrants: each grant a host must not act on is
// refused with its published reason and has no effect.
func TestOperation_AdversarialGrants(t *testing.T) {
	e := newOpsEnv(t)
	create := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	task, err := e.send(create)
	if err != nil {
		t.Fatal(err)
	}
	e.waitDone(task)
	base := e.exec.total()
	b := create.id.BindingID
	on := func() *op { return e.newOp(kOn, "vm-1", b, 1, powerReq("vm-1", providerv1.PowerOp_POWER_OP_ON)) }
	rogue := hostidentitytest.NewCA(t)
	floorAhead := false
	for name, tc := range map[string]struct {
		send   func() error
		code   codes.Code
		reason string
	}{
		"forged": {send: func() error {
			o := on()
			g := e.grant(o, time.Now(), 0)
			g.Signature[len(g.Signature)-1] ^= 1
			_, err := e.sendEnvelope(o.method, &providerv1.OperationEnvelope{Request: o.request, Grant: g}, nil)
			return err
		}, code: codes.FailedPrecondition, reason: contracts.GrantUnauthenticatedReason},
		"rogue-signed": {send: func() error {
			o := on()
			saved := e.grants
			e.grants = newGrantSigner(t, rogue.Cert, rogue.Key, e.pki.Record.InstallID)
			defer func() { e.grants = saved }()
			_, err := e.send(o)
			return err
		}, code: codes.FailedPrecondition, reason: contracts.GrantUnauthenticatedReason},
		"other host": {send: func() error {
			o := on()
			o.id.HostID = "host-b"
			_, err := e.send(o)
			return err
		}, code: codes.FailedPrecondition, reason: contracts.GrantMismatchReason},
		"other Provider": {send: func() error {
			o := on()
			o.id.ProviderUID = "another-provider"
			_, err := e.send(o)
			return err
		}, code: codes.FailedPrecondition, reason: contracts.GrantMismatchReason},
		"other enrolment": {send: func() error {
			o := on()
			o.id.EnrolmentID = randID()
			_, err := e.send(o)
			return err
		}, code: codes.NotFound, reason: contracts.UnknownToAgentReason},
		"expired at the clock floor": {send: func() error {
			o := on()
			g := e.grant(o, time.Now().Add(-50*time.Minute), time.Hour)
			floorAhead = true
			e.agent.floor.Store(time.Now().Add(20 * time.Minute).Unix())
			defer e.agent.floor.Store(0)
			_, err := e.sendEnvelope(o.method, &providerv1.OperationEnvelope{Request: o.request, Grant: g}, nil)
			return err
		}, code: codes.FailedPrecondition, reason: contracts.GrantUnauthenticatedReason},
		"wrong kind": {send: func() error {
			o := on()
			g := e.resign(o, func(b *providerv1.GrantBody) { b.Kind = kOff })
			_, err := e.sendEnvelope(o.method, &providerv1.OperationEnvelope{Request: o.request, Grant: g}, nil)
			return err
		}, code: codes.FailedPrecondition, reason: contracts.GrantMismatchReason},
		"outer field": {send: func() error {
			o := on()
			_, err := e.sendEnvelope(o.method, &providerv1.OperationEnvelope{Request: o.request, Grant: e.grant(o, time.Now(), 0)},
				func(m proto.Message) {
					if r, ok := m.(*providerv1.PowerRequest); ok {
						r.Id = "vm-1"
					}
				})
			return err
		}, code: codes.InvalidArgument, reason: contracts.GrantMismatchReason},
		"digest altered": {send: func() error {
			o := on()
			g := e.grant(o, time.Now(), 0)
			other, _ := proto.Marshal(powerReq("vm-2", providerv1.PowerOp_POWER_OP_ON))
			_, err := e.sendEnvelope(o.method, &providerv1.OperationEnvelope{Request: other, Grant: g}, nil)
			return err
		}, code: codes.FailedPrecondition, reason: contracts.GrantMismatchReason},
		"inner owner differs": {send: func() error {
			o := e.newOp(kOn, "vm-1", b, 1, powerReq("vm-2", providerv1.PowerOp_POWER_OP_ON))
			_, err := e.send(o)
			return err
		}, code: codes.FailedPrecondition, reason: contracts.GrantMismatchReason},
		"VM UID not a UID": {send: func() error {
			o := e.newOp(kOn, "VM/1", b, 1, powerReq("VM/1", providerv1.PowerOp_POWER_OP_ON))
			_, err := e.send(o)
			return err
		}, code: codes.FailedPrecondition, reason: contracts.GrantMismatchReason},
		"inner owner missing": {send: func() error {
			req := powerReq("vm-1", providerv1.PowerOp_POWER_OP_ON)
			req.Owner = nil
			o := e.newOp(kOn, "vm-1", b, 1, req)
			_, err := e.send(o)
			return err
		}, code: codes.FailedPrecondition, reason: contracts.GrantMismatchReason},
		"unsigned": {send: func() error {
			_, err := e.client().Power(ctx(t), powerReq("vm-1", providerv1.PowerOp_POWER_OP_ON))
			return err
		}, code: codes.FailedPrecondition, reason: contracts.IdentityRequiredReason},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.send()
			if status.Code(err) != tc.code || reasonOf(err) != tc.reason {
				t.Fatalf("got %v (%s), want %s %s", err, reasonOf(err), tc.code, tc.reason)
			}
			if n := e.exec.total(); n != base {
				t.Fatalf("effects %d, want %d", n, base)
			}
		})
	}
	if !floorAhead {
		t.Fatal("the clock floor case did not run")
	}
	// Replayed after terminal: the same answer, no second effect.
	again, err := e.send(create)
	if err != nil || again != task || e.exec.total() != base {
		t.Fatalf("replay after terminal: %q %v, effects %d", again, err, e.exec.total())
	}
}

// TestOperation_ConcurrentDuplicatesAndReorderedAttempts: twelve concurrent
// deliveries of one attempt make one record and one effect and get one task
// reference; an attempt that arrives after a higher one is refused.
func TestOperation_ConcurrentDuplicatesAndReorderedAttempts(t *testing.T) {
	e := newOpsEnv(t)
	create := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	var wg sync.WaitGroup
	tasks := make([]string, 12)
	errs := make([]error, 12)
	for i := range tasks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tasks[i], errs[i] = e.send(create)
		}(i)
	}
	wg.Wait()
	for i := range tasks {
		if errs[i] != nil || tasks[i] != tasks[0] {
			t.Fatalf("delivery %d: %q %v", i, tasks[i], errs[i])
		}
	}
	e.waitDone(tasks[0])
	if n := e.exec.total(); n != 2 {
		t.Fatalf("effects = %d, want 2", n)
	}
	b := create.id.BindingID
	second := e.newOp(kOn, "vm-1", b, 2, powerReq("vm-1", providerv1.PowerOp_POWER_OP_ON))
	task, err := e.send(second)
	if err != nil {
		t.Fatal(err)
	}
	e.waitDone(task)
	first := e.newOp(kOn, "vm-1", b, 1, powerReq("vm-1", providerv1.PowerOp_POWER_OP_ON))
	_, err = e.send(first)
	if status.Code(err) != codes.FailedPrecondition || reasonOf(err) != contracts.OpIDConflictReason {
		t.Fatalf("attempt 1 after attempt 2: %v", err)
	}
	if n := e.exec.total(); n != 3 {
		t.Fatalf("effects = %d, want 3", n)
	}
}

// TestOperation_DeletedAnswer: a signed Describe answers DELETED only after
// the placement's Delete completed, and NO_RECORD for a placement the host
// never had.
func TestOperation_DeletedAnswer(t *testing.T) {
	e := newOpsEnv(t)
	b := randID()
	describe := func(uid string, binding []byte, n byte) (*providerv1.DescribeResponse, *providerv1.VMAnswer) {
		t.Helper()
		resp, err := e.client().Describe(ctx(t), &providerv1.DescribeRequest{Owner: ownerOf(uid), TargetHostId: trusttest.HostName,
			AnswerNonce: nonce(n), BindingId: binding})
		if err != nil {
			t.Fatal(err)
		}
		a := &providerv1.VMAnswer{}
		verifyBlob(t, e, resp.GetHostAnswer(), answer.BodyTypeVMAnswer, a)
		return resp, a
	}
	if resp, a := describe("vm-never", randID(), 1); resp.GetExists() || a.GetPresence() != providerv1.VMPresence_VM_PRESENCE_NO_RECORD {
		t.Fatalf("never placed: %v %v", resp, a)
	}
	task, err := e.send(e.newOp(kCreate, "vm-1", b, 1, createReq("vm-1", nil)))
	if err != nil {
		t.Fatal(err)
	}
	e.waitDone(task)
	if resp, a := describe("vm-1", b, 2); !resp.GetExists() || a.GetPresence() != providerv1.VMPresence_VM_PRESENCE_PRESENT || resp.GetPowerState() != server.PowerOn {
		t.Fatalf("present: %v %v", resp, a)
	}
	del := e.newOp(kDelete, "vm-1", b, 1, deleteReq("vm-1"))
	task, err = e.send(del)
	if err != nil {
		t.Fatal(err)
	}
	e.waitDone(task)
	resp, a := describe("vm-1", b, 3)
	if resp.GetExists() || a.GetPresence() != providerv1.VMPresence_VM_PRESENCE_DELETED || !bytes.Equal(a.GetDeleteOpId(), del.id.OpID) {
		t.Fatalf("deleted: %v %v", resp, a)
	}
	if _, a := describe("vm-1", randID(), 4); a.GetPresence() != providerv1.VMPresence_VM_PRESENCE_NO_RECORD {
		t.Fatalf("another placement of a deleted VM: %v", a)
	}
}

// TestOperation_StepDeadline: a step that does not end by its deadline is
// resolved by inspection, so the operation ends instead of running forever.
func TestOperation_StepDeadline(t *testing.T) {
	e := newOpsEnv(t)
	b := randID()
	task, err := e.send(e.newOp(kCreate, "vm-1", b, 1, createReq("vm-1", nil)))
	if err != nil {
		t.Fatal(err)
	}
	e.waitDone(task)
	e.exec.mu.Lock()
	e.exec.gate = make(chan struct{})
	e.exec.mu.Unlock()
	task, err = e.send(e.newOp(kOff, "vm-1", b, 1, powerReq("vm-1", providerv1.PowerOp_POWER_OP_OFF)))
	if err != nil {
		t.Fatal(err)
	}
	st := e.waitDone(task)
	if st.GetOutcome() == providerv1.TaskOutcome_TASK_OUTCOME_RUNNING || st.GetOutcome() == providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED {
		t.Fatalf("a stalled step: %v", st)
	}
}

// TestOperation_RequestRules: malformed read requests are refused before any
// read.
func TestOperation_RequestRules(t *testing.T) {
	e := newOpsEnv(t)
	c := e.client()
	for name, req := range map[string]*providerv1.TaskStatusRequest{
		"short nonce":         {AnswerNonce: []byte{1}, OpId: randID(), OpEnrolmentId: randID(), TargetHostId: trusttest.HostName},
		"task with nonce":     {AnswerNonce: nonce(1), Task: &providerv1.TaskRef{Id: "x"}, OpId: randID(), OpEnrolmentId: randID(), TargetHostId: trusttest.HostName},
		"other host":          {AnswerNonce: nonce(1), OpId: randID(), OpEnrolmentId: randID(), TargetHostId: "host-b"},
		"op id without nonce": {Task: &providerv1.TaskRef{Id: "x"}, OpId: randID()},
		"malformed task":      {Task: &providerv1.TaskRef{Id: "op-zz"}},
		"continuity alone":    {Task: &providerv1.TaskRef{Id: "x"}, ContinuitySeq: 3},
	} {
		if _, err := c.TaskStatus(ctx(t), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, req := range map[string]*providerv1.DescribeRequest{
		"no owner":        {TargetHostId: trusttest.HostName},
		"binding alone":   {Owner: ownerOf("vm-1"), BindingId: randID()},
		"nonce alone":     {Owner: ownerOf("vm-1"), AnswerNonce: nonce(1)},
		"other host":      {Owner: ownerOf("vm-1"), TargetHostId: "host-b"},
		"owner not a uid": {Owner: ownerOf("VM/1")},
	} {
		if _, err := c.Describe(ctx(t), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestOperation_RedactionCanary: request bytes such as cloud-init user data
// never reach the store, the anchor log, the logs, an error or an answer.
func TestOperation_RedactionCanary(t *testing.T) {
	e := newOpsEnv(t)
	canary := []byte("CANARY-user-data-7f3a9c")
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", canary))
	task, err := e.send(o)
	if err != nil {
		t.Fatal(err)
	}
	e.waitDone(task)
	var seen [][]byte
	_, a, _ := e.signedStatus(o, nonce(9))
	ab, _ := proto.Marshal(a)
	seen = append(seen, ab)
	// Refusals of requests that carry the canary.
	bad := e.newOp(kCreate, "vm-2", randID(), 1, createReq("vm-2", canary))
	g := e.grant(bad, time.Now(), 0)
	g.Signature[len(g.Signature)-1] ^= 1
	_, err = e.sendEnvelope(bad.method, &providerv1.OperationEnvelope{Request: bad.request, Grant: g}, nil)
	seen = append(seen, []byte(err.Error()))
	_, err = e.sendEnvelope(bad.method, &providerv1.OperationEnvelope{Request: bad.request, Grant: e.grant(bad, time.Now(), 0)},
		func(m proto.Message) {
			if r, ok := m.(*providerv1.CreateRequest); ok {
				r.UserData = canary
			}
		})
	seen = append(seen, []byte(err.Error()))
	for _, l := range e.logs.all() {
		seen = append(seen, []byte(l))
	}
	if err := filepath.Walk(e.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) // #nosec G304 -- the test's own store directory
		seen = append(seen, b)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for i, b := range seen {
		for _, enc := range canaryForms(canary) {
			if bytes.Contains(b, enc) {
				t.Fatalf("the canary leaked (item %d, form %q)", i, enc)
			}
		}
	}
	if len(e.logs.all()) == 0 {
		t.Fatal("no audit line was logged")
	}
}

// TestOperation_NewAdmissionNeedsTheCertificatesEnrolment: while the store's
// active enrolment is not the one the host certificate names, a new op_id is
// refused UNATTESTED (never definite) and nothing is recorded or performed.
func TestOperation_NewAdmissionNeedsTheCertificatesEnrolment(t *testing.T) {
	storeEnrolment := randID()
	e := newOpsEnvEnrolled(t, storeEnrolment)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	o.id.EnrolmentID = storeEnrolment
	_, err := e.send(o)
	if status.Code(err) != codes.Unavailable || reasonOf(err) != contracts.UnattestedReason {
		t.Fatalf("got %v, want Unavailable UNATTESTED", err)
	}
	if contracts.IsDefiniteNotPerformedReason(reasonOf(err)) {
		t.Fatal("an enrolment mismatch must not be a definite refusal")
	}
	if v, err := e.store.AnswerOp(ctx(t), opstore.OpRef{EnrolmentID: storeEnrolment, OpID: o.id.OpID}, 0); err != nil || v.Found {
		t.Fatalf("recorded: %+v %v", v, err)
	}
	if n := e.exec.total(); n != 0 {
		t.Fatalf("effects = %d", n)
	}
}

// canaryForms are the canary and its hexadecimal and base64 encodings.
func canaryForms(c []byte) [][]byte {
	h := hex.EncodeToString(c)
	return [][]byte{c, []byte(h), []byte(strings.ToUpper(h)),
		[]byte(base64.StdEncoding.EncodeToString(c)), []byte(base64.RawStdEncoding.EncodeToString(c)),
		[]byte(base64.URLEncoding.EncodeToString(c)), []byte(base64.RawURLEncoding.EncodeToString(c))}
}

// TestOperation_FloorLostAfterTheGateIsNotDefinite: when the clock floor
// stops being healthy after the readiness gate let a call through, a
// re-delivery of a known op_id is refused UNATTESTED with the floor's cause,
// never with a definite reason.
func TestOperation_FloorLostAfterTheGateIsNotDefinite(t *testing.T) {
	e := newOpsEnv(t)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	task, err := e.send(o)
	if err != nil {
		t.Fatal(err)
	}
	e.waitDone(task)
	// The gate's cause stays empty (the gate already passed); only the
	// floor's state has changed.
	for _, st := range []clockfloor.StateKind{clockfloor.StateClockFloorAhead, noFloor} {
		e.agent.setFloorState(st)
		_, err = e.send(o)
		if status.Code(err) != codes.Unavailable || reasonOf(err) != contracts.UnattestedReason {
			t.Fatalf("floor %q: %v, want Unavailable UNATTESTED", st, err)
		}
		if contracts.IsDefiniteNotPerformedReason(reasonOf(err)) {
			t.Fatal("a refusal for an untrusted clock must never be definite")
		}
	}
}
