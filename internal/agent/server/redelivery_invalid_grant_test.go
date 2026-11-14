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
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestOperation_RedeliveryOnAnInvalidGrantNeverResumes characterises the
// agent for the manager's rule that no refusal of a mutating call decides an
// attempt: a known op_id, COMPLETED or EXECUTING, re-delivered under a grant
// that no longer verifies (past its not_after, or signed by an authority the
// host does not pin, as after an anchor rotation) is refused before the host
// looks the op_id up: the refusal is unsigned, nothing runs or resumes, the
// store does not move, and a following signed TaskStatus by op_id answers
// from the operation's record.
func TestOperation_RedeliveryOnAnInvalidGrantNeverResumes(t *testing.T) {
	for _, executing := range []bool{false, true} {
		name := map[bool]string{false: "completed", true: "executing"}[executing]
		t.Run(name, func(t *testing.T) {
			e := newOpsEnv(t)
			create := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
			gate := make(chan struct{})
			if executing {
				e.exec.mu.Lock()
				e.exec.gate = gate
				e.exec.mu.Unlock()
				t.Cleanup(func() { close(gate) })
			}
			task, err := e.send(create)
			if err != nil {
				t.Fatal(err)
			}
			want := providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED
			if executing {
				want = providerv1.OperationRecordState_OPERATION_RECORD_STATE_EXECUTING
				deadline := time.Now().Add(5 * time.Second)
				for {
					_, a, _ := e.signedStatus(create, randID())
					if a.GetState() == want || time.Now().After(deadline) {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			} else {
				e.waitDone(task)
			}
			rotated := hostidentitytest.NewCA(t)
			foreign := newGrantSigner(t, rotated.Cert, rotated.Key, e.pki.Record.InstallID)
			for refusal, grant := range map[string]*providerv1.OperationGrant{
				"past not_after": e.grant(create, time.Now().Add(-2*time.Hour), 5*time.Minute),
				"unpinned authority": func() *providerv1.OperationGrant {
					own := e.grants
					e.grants = foreign
					defer func() { e.grants = own }()
					return e.grant(create, time.Now(), 0)
				}(),
			} {
				before, seq := e.exec.total(), e.store.Position().Seq
				_, err := e.sendEnvelope(create.method, &providerv1.OperationEnvelope{Request: create.request, Grant: grant}, nil)
				if err == nil {
					t.Fatalf("%s: a re-delivery under an invalid grant was accepted", refusal)
				}
				if e.exec.total() != before {
					t.Fatalf("%s: the refused re-delivery had an effect", refusal)
				}
				if got := e.store.Position().Seq; got != seq {
					t.Fatalf("%s: the store moved from %d to %d", refusal, seq, got)
				}
				_, a, _ := e.signedStatus(create, randID())
				if a.GetState() != want {
					t.Fatalf("%s: signed state %v, want %v", refusal, a.GetState(), want)
				}
			}
		})
	}
}
