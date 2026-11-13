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
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/daemon"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/fake"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered/agenttarget"
)

const (
	hostA  = "host-a"
	vmUID  = "0b8f5e0e-7a53-4f6b-9a0e-1d2c3b4a5f60"
	kindCr = providerv1.OperationKind_OPERATION_KIND_CREATE
	kindDl = providerv1.OperationKind_OPERATION_KIND_DELETE
)

// oneHost starts a target with one agent whose steps are not held.
func oneHost(t *testing.T) (*agenttarget.Target, *agenttarget.Host) {
	t.Helper()
	tg, err := agenttarget.New(t, agenttarget.Config{Hosts: []string{hostA}})
	if err != nil {
		t.Fatal(err)
	}
	h := tg.Host(hostA)
	if err := fake.SetHold(h.FakeDir(), false); err != nil {
		t.Fatal(err)
	}
	return tg, h
}

func owner() *providerv1.ObjectIdentity {
	return &providerv1.ObjectIdentity{Uid: vmUID, Namespace: "team-a", Name: "web"}
}

// effectsOf counts the effects of op_id per step.
func effectsOf(t *testing.T, h *agenttarget.Host, opID []byte) map[string]int {
	t.Helper()
	all, err := h.Effects()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, e := range all {
		if e.OpID == hex.EncodeToString(opID) {
			out[e.Step]++
		}
	}
	return out
}

// signedState asks the agent for a signed answer about id and verifies it.
func signedState(t *testing.T, tg *agenttarget.Target, h *agenttarget.Host, id operation.Identity) *providerv1.OperationAnswer {
	t.Helper()
	nonce := bytes.Repeat([]byte{7}, 16)
	resp, err := h.Client().TaskStatus(context.Background(), &providerv1.TaskStatusRequest{AnswerNonce: nonce, OpId: id.OpID,
		OpEnrolmentId: id.EnrolmentID, TargetHostId: id.HostID})
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
		t.Fatalf("no signed answer (%v)", err)
	}
	in, err := answer.SignatureInput(blob.GetBodyType(), blob.GetBody())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := parseCert(blob.GetSignerCertificate(), tg)
	if err != nil {
		t.Fatal(err)
	}
	d := sha256.Sum256(in)
	pub, _ := cert.PublicKey.(*ecdsa.PublicKey)
	if pub == nil || !ecdsa.VerifyASN1(pub, d[:], blob.GetSignature()) {
		t.Fatal("the answer's signature does not verify")
	}
	a := &providerv1.OperationAnswer{}
	if err := proto.Unmarshal(blob.GetBody(), a); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.GetContext().GetNonce(), nonce) || !bytes.Equal(a.GetOpId(), id.OpID) {
		t.Fatalf("the answer is not for this request: %v", a)
	}
	return a
}

// crashCase is one crash point of the operation path, killed as a process.
type crashCase struct {
	// points is the agent's crash sequence (daemon.DevCrashEnv).
	points string
	// kind is the operation crashed.
	kind providerv1.OperationKind
	// state is the operation's state after the restart, and its end.
	state providerv1.OperationRecordState
	// effects are the effects the operation has after it ended, per step.
	effects map[string]int
}

// TestCrashPointsAsProcessKills kills the agent process with SIGKILL at each
// crash point of the operation path, starts it again on its kept state, and
// requires: the agent comes back ready (its chain and anchor verify), the
// operation ends in the state its recovery rule gives, a once-only step's
// effect happened at most once, a re-delivery is answered from the record
// with no new effect, and the signed answer names the operation's tombstone
// when it ended.
func TestCrashPointsAsProcessKills(t *testing.T) {
	completed := providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED
	cases := map[string]crashCase{
		"before accept":                {points: string(opstore.PointBeforeAccept), kind: kindCr, state: completed, effects: map[string]int{"define": 1}},
		"accept committed, no anchor":  {points: "before-accept,after-store-commit", kind: kindCr, state: completed, effects: map[string]int{"define": 1}},
		"accept anchored, no reply":    {points: "before-accept,after-anchor", kind: kindCr, state: completed, effects: map[string]int{"define": 1}},
		"after intent, no witness":     {points: string(opstore.PointAfterIntent), kind: kindCr, state: completed, effects: map[string]int{"define": 1}},
		"after witness, no effect":     {points: string(opstore.PointAfterWitness), kind: kindCr, state: providerv1.OperationRecordState_OPERATION_RECORD_STATE_OUTCOME_UNKNOWN, effects: map[string]int{"define": 0}},
		"after effect, no done":        {points: string(opstore.PointAfterEffect), kind: kindCr, state: completed, effects: map[string]int{"define": 1}},
		"after last done, no terminal": {points: string(opstore.PointAfterLastDone), kind: kindCr, state: completed, effects: map[string]int{"define": 1}},
		"after terminal, no reply":     {points: string(opstore.PointAfterTerminal), kind: kindCr, state: completed, effects: map[string]int{"define": 1}},
		"irreversible: after intent":   {points: string(opstore.PointAfterIntent), kind: kindDl, state: providerv1.OperationRecordState_OPERATION_RECORD_STATE_OUTCOME_UNKNOWN, effects: map[string]int{"destroy": 0}},
		"irreversible: after effect":   {points: string(opstore.PointAfterEffect), kind: kindDl, state: completed, effects: map[string]int{"destroy": 1}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) { runCrashCase(t, c) })
	}
}

func runCrashCase(t *testing.T, c crashCase) {
	ctx := context.Background()
	tg, h := oneHost(t)
	if c.kind == kindDl {
		// The VM to delete exists first.
		env, _, err := tg.Mint(ctx, hostA, vmUID, kindCr, &providerv1.CreateRequest{Name: "web", TargetHostId: hostA, Owner: owner()})
		if err != nil {
			t.Fatal(err)
		}
		task, err := tg.SendDirect(ctx, hostA, kindCr, env)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tg.WaitTask(ctx, hostA, task, 20*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(daemon.DevCrashEnv + "=" + c.points); err != nil {
		t.Fatal(err)
	}
	var req proto.Message = &providerv1.CreateRequest{Name: "web", TargetHostId: hostA, Owner: owner(), UserData: []byte("#cloud-config\n")}
	if c.kind == kindDl {
		req = &providerv1.DeleteRequest{Id: "web", TargetHostId: hostA, Owner: owner()}
	}
	env, id, err := tg.Mint(ctx, hostA, vmUID, c.kind, req)
	if err != nil {
		t.Fatal(err)
	}
	_, sendErr := tg.SendDirect(ctx, hostA, c.kind, env)
	killed, err := h.Wait(30 * time.Second)
	if err != nil || !killed {
		t.Fatalf("the agent was not killed at %s: %v (send: %v)", c.points, err, sendErr)
	}
	if err := h.Restart(); err != nil {
		t.Fatal(err)
	}
	if h.State() != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("after the kill at %s the agent started %s", c.points, h.State())
	}
	ref := server.TaskRef(opstore.OpRef{EnrolmentID: id.EnrolmentID, OpID: id.OpID})
	st, err := tg.WaitTask(ctx, hostA, ref, 20*time.Second)
	if status.Code(err) == codes.NotFound {
		// Nothing was recorded before the kill: the same attempt is
		// delivered again.
		got, err := tg.SendDirect(ctx, hostA, c.kind, env)
		if err != nil || got != ref {
			t.Fatalf("re-delivery after a kill before acceptance: %q %v", got, err)
		}
		st, err = tg.WaitTask(ctx, hostA, ref, 20*time.Second)
		if err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	a := signedState(t, tg, h, id)
	if a.GetState() != c.state {
		t.Fatalf("state %s (task %v), want %s", a.GetState(), st, c.state)
	}
	if a.GetContext().GetAgentState() != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("agent state %s", a.GetContext().GetAgentState())
	}
	if c.state == providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED && !a.GetTombstoned() {
		t.Fatal("a finished operation is answered from its tombstone")
	}
	if c.state == providerv1.OperationRecordState_OPERATION_RECORD_STATE_OUTCOME_UNKNOWN && st.GetDone() {
		t.Fatal("an unknown outcome is never done: it is never read as not performed")
	}
	before := effectsOf(t, h, id.OpID)
	for step, n := range c.effects {
		if before[step] != n {
			t.Fatalf("step %s: %d effects, want %d (all: %v)", step, before[step], n, before)
		}
	}
	got, err := tg.SendDirect(ctx, hostA, c.kind, env)
	if err != nil || got != ref {
		t.Fatalf("re-delivery after the end: %q %v", got, err)
	}
	if after := effectsOf(t, h, id.OpID); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("a re-delivery had an effect: %v, then %v", before, after)
	}
}

// TestCrashMidAnchorRotation kills the agent while it rotates its anchor log:
// the agent comes back ready and keeps every acknowledged operation.
func TestCrashMidAnchorRotation(t *testing.T) {
	ctx := context.Background()
	tg, h := oneHost(t)
	if err := h.Stop(); err != nil {
		t.Fatal(err)
	}
	// A rotation every few records.
	startErr := h.Start(daemon.DevCrashEnv+"=mid-anchor-rotation", daemon.DevAnchorRotateEnv+"=1024")
	var acked []operation.Identity
	for i := 0; startErr == nil && h.Running() && i < 40; i++ {
		kind, req := providerv1.OperationKind_OPERATION_KIND_POWER_ON, proto.Message(&providerv1.PowerRequest{Id: "web", Op: providerv1.PowerOp_POWER_OP_ON, TargetHostId: hostA, Owner: owner()})
		if i == 0 {
			kind, req = kindCr, &providerv1.CreateRequest{Name: "web", TargetHostId: hostA, Owner: owner()}
		}
		env, id, err := tg.Mint(ctx, hostA, vmUID, kind, req)
		if err != nil {
			t.Fatal(err)
		}
		task, err := tg.SendDirect(ctx, hostA, kind, env)
		if err != nil {
			break
		}
		if _, err := tg.WaitTask(ctx, hostA, task, 20*time.Second); err != nil {
			break
		}
		acked = append(acked, id)
	}
	if killed, err := h.Wait(30 * time.Second); err != nil || !killed {
		t.Fatalf("the agent was not killed mid-rotation: %v", err)
	}
	if err := h.Restart(); err != nil {
		t.Fatal(err)
	}
	if h.State() != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("after a kill mid-rotation the agent started %s", h.State())
	}
	if len(acked) == 0 {
		t.Fatal("the rotation came before any operation was acknowledged: raise the rotation size")
	}
	t.Logf("%d operations acknowledged before the kill", len(acked))
	for _, id := range acked {
		if a := signedState(t, tg, h, id); a.GetState() == providerv1.OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD {
			t.Fatalf("an acknowledged operation was lost: %x", id.OpID)
		}
	}
}

// TestConcurrentDuplicateDeliveries sends one attempt twelve times at once
// to an agent process: one record, one task reference, one once-only effect.
func TestConcurrentDuplicateDeliveries(t *testing.T) {
	ctx := context.Background()
	tg, h := oneHost(t)
	env, id, err := tg.Mint(ctx, hostA, vmUID, kindCr, &providerv1.CreateRequest{Name: "web", TargetHostId: hostA, Owner: owner()})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	refs := make([]string, 12)
	errs := make([]error, 12)
	for i := range refs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			refs[i], errs[i] = tg.SendDirect(ctx, hostA, kindCr, env)
		}(i)
	}
	wg.Wait()
	for i := range refs {
		if errs[i] != nil || refs[i] != refs[0] {
			t.Fatalf("delivery %d: %q %v", i, refs[i], errs[i])
		}
	}
	if _, err := tg.WaitTask(ctx, hostA, refs[0], 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if n := effectsOf(t, h, id.OpID)["define"]; n != 1 {
		t.Fatalf("define effects = %d", n)
	}
}

// TestRedactionCanaryAcrossTheProcess: cloud-init user data sent to the agent
// never appears in its store, its anchor log, its witnesses, its log output,
// its errors or its answers.
func TestRedactionCanaryAcrossTheProcess(t *testing.T) {
	ctx := context.Background()
	tg, h := oneHost(t)
	canary := []byte("CANARY-9b1e-user-data-secret")
	env, id, err := tg.Mint(ctx, hostA, vmUID, kindCr, &providerv1.CreateRequest{Name: "web", TargetHostId: hostA, Owner: owner(), UserData: canary})
	if err != nil {
		t.Fatal(err)
	}
	task, err := tg.SendDirect(ctx, hostA, kindCr, env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.WaitTask(ctx, hostA, task, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	a := signedState(t, tg, h, id)
	seen := [][]byte{}
	if b, err := proto.Marshal(a); err == nil {
		seen = append(seen, b)
	}
	// A refused delivery that carries the canary (a forged signature).
	bad, _, err := tg.Mint(ctx, hostA, "c4d1a9f2-3e6b-4c7d-8e9f-0a1b2c3d4e5f",
		kindCr, &providerv1.CreateRequest{Name: "db", TargetHostId: hostA, Owner: &providerv1.ObjectIdentity{Uid: "c4d1a9f2-3e6b-4c7d-8e9f-0a1b2c3d4e5f"}, UserData: canary})
	if err != nil {
		t.Fatal(err)
	}
	bad.Grant.Signature[len(bad.Grant.Signature)-1] ^= 1
	if _, err := tg.SendDirect(ctx, hostA, kindCr, bad); err == nil {
		t.Fatal("a forged grant was accepted")
	} else {
		seen = append(seen, []byte(err.Error()))
	}
	if err := h.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := filepath.Walk(h.StateDir(), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) // #nosec G304 -- the test's own state directory
		seen = append(seen, b)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	logs, err := os.ReadFile(h.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	seen = append(seen, logs)
	for i, b := range seen {
		if bytes.Contains(b, canary) {
			t.Fatalf("the canary leaked (item %d)", i)
		}
	}
	if !bytes.Contains(logs, []byte(`"decision":"accepted"`)) {
		t.Fatal("no audit line for the accepted operation")
	}
}

// parseCert parses an answer's signer certificate and checks it chains to the
// target's host identity issuer.
func parseCert(der []byte, tg *agenttarget.Target) (*x509.Certificate, error) {
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	if _, err := c.Verify(x509.VerifyOptions{Roots: tg.HostAnswerRoots(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return nil, err
	}
	return c, nil
}
