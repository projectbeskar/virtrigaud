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

package opstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// Request bytes never reach the store, the anchor log, witnesses, markers or
// the audit trail: only the salted digest does.
func TestRedaction_RequestBytesNeverStored(t *testing.T) {
	canary := []byte("CANARY-user-data-7f3c9a1e-do-not-store")
	h := newHarness(t)
	h.fresh()
	ctx := context.Background()
	salt := rid()
	digest := sha256.Sum256(append(append([]byte(nil), salt...), canary...))
	b := rid()
	for i, kind := range kindsInOrder() {
		r := h.req("vm-redaction", b, kind, uint64(i+1))
		r.IntentDigest = digest[:]
		h.mustAccept(r)
		if _, err := h.s.Execute(ctx, ref(r), canary); err != nil {
			t.Fatal(err)
		}
		if got := h.exec.payload[effKey(r.OpID, testPlans[kind][0].Name)]; !bytes.Equal(got, canary) {
			t.Fatalf("%v: the executor did not get the payload", kind)
		}
	}
	// A crash with a payload in flight, and a quarantine, write more files.
	r := h.req("vm-redaction-2", rid(), kCreate, 1)
	h.mustAccept(r)
	h.crash = &journaltest.Crasher{Point: opstore.PointAfterWitness, N: 1}
	_, _ = h.s.Execute(ctx, ref(r), canary)
	h.restart()
	h.s.Journal().Quarantine(journal.LostRolledBack, "redaction test")
	if _, err := h.s.Compact(ctx, 0); err == nil {
		t.Log("compaction on a lost store is refused as expected or empty")
	}
	_ = h.s.Close()

	db, err := os.ReadFile(filepath.Join(h.dir, journal.StoreFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(db, canary) {
		t.Fatal("canary found in the bbolt store")
	}
	for name, data := range h.mem.Snapshot() {
		if bytes.Contains(data, canary) || bytes.Contains([]byte(name), canary) {
			t.Fatalf("canary found in %s", name)
		}
	}
	h.auditMu.Lock()
	defer h.auditMu.Unlock()
	if len(h.audit) == 0 {
		t.Fatal("no audit events")
	}
	for _, ev := range h.audit {
		if bytes.Contains([]byte(fmt.Sprintf("%+v", ev)), canary) {
			t.Fatalf("canary in audit event %+v", ev)
		}
	}
}

func kindsInOrder() []providerKind {
	return []providerKind{kCreate, kPowerOn, kPowerOff, kReconfig, kDelete}
}
