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
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/fake"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// populate runs n Create operations to completion on the agent's enrolled
// store, as an agent that executes operations would, and returns them.
func populate(t *testing.T, h *host, exec *fake.Executor, n int) []opstore.Request {
	t.Helper()
	ctx := context.Background()
	st, err := opstore.Open(ctx, opstore.Options{Journal: journal.Options{Dir: h.stateDir}, Executor: exec})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.EstablishContinuity(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	var out []opstore.Request
	for i := 0; i < n; i++ {
		d := sha256.Sum256(randID())
		r := opstore.Request{ProviderUID: trusttest.ProviderUID, EnrolmentID: h.pki.Record.EnrolmentID, OpID: randID(),
			VMUID: fmt.Sprintf("vm-%d", i), BindingID: randID(), Kind: providerv1.OperationKind_OPERATION_KIND_CREATE, Attempt: 1,
			IntentDigest: d[:], MintEpoch: opstore.Epoch{Counter: h.dev.StartEpoch, Holder: randID()}, Actor: "test"}
		if dec, err := st.Admit(ctx, r); err != nil || dec.Outcome != opstore.OutcomeAccepted {
			t.Fatalf("admit: %+v %v", dec, err)
		}
		if v, err := st.Execute(ctx, opstore.OpRef{EnrolmentID: r.EnrolmentID, OpID: r.OpID}, nil); err != nil || v.State != opstore.StateCompleted {
			t.Fatalf("execute: %+v %v", v, err)
		}
		out = append(out, r)
	}
	return out
}

// newFakeHost is a host whose agent runs the fake executor, started once so
// that its store is enrolled.
func newFakeHost(t *testing.T) (*host, *fake.Executor) {
	t.Helper()
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
	return h, exec
}

// TestAgentCompactsTheStore: the agent compacts its store once it is ready:
// the step journals of finished operations go, their tombstones stay, so a
// re-delivery after the compaction is answered from the tombstone and never
// runs again.
func TestAgentCompactsTheStore(t *testing.T) {
	h, exec := newFakeHost(t)
	reqs := populate(t, h, exec, 20)

	r, _ := h.run()
	waitLog(t, h, `"msg"="store compacted" "dropped"=20`)
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := opstore.Open(ctx, opstore.Options{Journal: journal.Options{Dir: h.stateDir}, Executor: exec})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.EstablishContinuity(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := st.Compact(ctx, 0); err != nil || n != 0 {
		t.Fatalf("a second compaction dropped %d (%v): the agent's left records behind", n, err)
	}
	for _, req := range reqs {
		d, err := st.Admit(ctx, req)
		if err != nil || d.Outcome != opstore.OutcomeKnown || d.Op.State != opstore.StateCompleted {
			t.Fatalf("re-delivery after compaction: %+v %v", d, err)
		}
	}
	effects, err := fake.Effects(filepath.Join(h.stateDir, config.FakeExecutorDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(effects) != 2*len(reqs) {
		t.Fatalf("effects = %d, want %d (define and start once each)", len(effects), 2*len(reqs))
	}
}

// TestAgentReportsAStoreCompactionCannotRelieve: near its size threshold,
// the agent compacts at once; a store that stays near the threshold
// afterwards (it holds tombstones only) is reported once.
func TestAgentReportsAStoreCompactionCannotRelieve(t *testing.T) {
	h, exec := newFakeHost(t)
	populate(t, h, exec, 5)
	// Any real store is above a one-byte threshold.
	h.maxStoreBytes = 1
	r, _ := h.run()
	waitLog(t, h, `"msg"="store compacted" "dropped"=5`)
	waitLog(t, h, "stays near its size threshold after compaction")
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}

// TestAgentTrimsTheAuditWindow: the agent's compaction also trims the audit
// events beyond the configured retention and logs each bucket's size.
func TestAgentTrimsTheAuditWindow(t *testing.T) {
	h, exec := newFakeHost(t)
	populate(t, h, exec, 10)
	h.cfg.AuditRetainEvents = 5
	r, _ := h.run()
	waitLog(t, h, `"msg"="store compacted" "dropped"=10`)
	text := h.logs.text()
	if !strings.Contains(text, `"bucketBytes"`) || !strings.Contains(text, `"audit"`) || strings.Contains(text, `"auditEventsTrimmed"=0 `) {
		t.Fatalf("no audit trim or bucket sizes in the compaction log:\n%s", text)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}

// TestRestartAfterReadyDoesNotReplay: the agent replays at open the audit
// events of a store that never persisted its audit cursor, logs them with
// their sequence, persists the cursor once it is ready, and so replays
// nothing when it starts again.
func TestRestartAfterReadyDoesNotReplay(t *testing.T) {
	h, exec := newFakeHost(t)
	populate(t, h, exec, 3)
	r, _ := h.run()
	waitLog(t, h, `"msg"="store compacted"`)
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	first := h.logs.text()
	if !hasLine(first, `"replayed"=true`, `"seq"=`) {
		t.Fatalf("the first start replayed nothing:\n%s", first)
	}
	before := strings.Count(first, `"replayed"=true`)
	r, _ = h.run()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if after := strings.Count(h.logs.text(), `"replayed"=true`); after != before {
		t.Fatalf("%d events replayed again after a restart that followed READY", after-before)
	}
}
