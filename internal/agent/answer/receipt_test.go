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

package answer_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// testReceipt is a receipt for an accepted operation of p's enrolment.
func testReceipt(p *trusttest.PKI) answer.Receipt {
	v := opView(opstore.StateAccepted, false)
	v.Op.Ref.EnrolmentID = p.Record.EnrolmentID
	v.Op.AcceptedSeq = 42
	pos := journal.Position{Seq: 42}
	pos.StoreID[0], pos.Head[0] = 0x51, 0x4E
	return answer.Receipt{Record: p.Record, HostID: trusttest.HostName, Op: v.Op,
		Acceptance: opstore.Acceptance{Position: pos, Session: testSession}, GrantDigest: bytes.Repeat([]byte{0xD6}, 32)}
}

// TestSignReceipt: a receipt names the operation, where it was accepted, the
// session and the delivery, and is signed under the host-signed domain with
// its own body type.
func TestSignReceipt(t *testing.T) {
	p := trusttest.New(t)
	r := testReceipt(p)
	blob, err := answer.SignReceipt(p.HostKey, leafDER(t, p), r)
	if err != nil {
		t.Fatal(err)
	}
	if blob.GetBodyType() != answer.BodyTypeOperationReceipt || answer.BodyTypeOperationReceipt != "provider.v1.OperationReceipt" {
		t.Fatalf("body type %q", blob.GetBodyType())
	}
	m, err := answer.SignatureInput(blob.GetBodyType(), blob.GetBody())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(blob.GetSignerCertificate())
	if err != nil {
		t.Fatal(err)
	}
	d := sha256.Sum256(m)
	if !ecdsa.VerifyASN1(cert.PublicKey.(*ecdsa.PublicKey), d[:], blob.GetSignature()) {
		t.Fatal("the receipt signature does not verify")
	}
	got := &providerv1.OperationReceipt{}
	if err := proto.Unmarshal(blob.GetBody(), got); err != nil {
		t.Fatal(err)
	}
	if got.GetReceiptVersion() != 1 || got.GetProviderUid() != p.Record.ProviderUID || got.GetHostId() != trusttest.HostName ||
		!bytes.Equal(got.GetInstallId(), p.Record.InstallID) || !bytes.Equal(got.GetEnrolmentId(), p.Record.EnrolmentID) ||
		!bytes.Equal(got.GetAttestationId(), testSession) || got.GetStoreId()[0] != 0x51 || len(got.GetStoreId()) != 16 ||
		!bytes.Equal(got.GetOpId(), r.Op.Ref.OpID) || got.GetKind() != int32(r.Op.Kind) || got.GetAttempt() != 3 ||
		got.GetVmUid() != "vm-1" || !bytes.Equal(got.GetBindingId(), r.Op.BindingID) || !bytes.Equal(got.GetIntentDigest(), r.Op.IntentDigest) ||
		got.GetAcceptedSeq() != 42 || got.GetAcceptedHead()[0] != 0x4E || len(got.GetAcceptedHead()) != 32 ||
		!bytes.Equal(got.GetGrantDigest(), r.GrantDigest) {
		t.Fatalf("receipt %v", got)
	}
	// The generic answer signer never signs a receipt body.
	if _, err := answer.Sign(p.HostKey, leafDER(t, p), got); !errors.Is(err, answer.ErrSign) {
		t.Fatalf("Sign of a receipt: %v", err)
	}
}

func TestOperationReceipt_FieldRules(t *testing.T) {
	p := trusttest.New(t)
	for name, edit := range map[string]func(*answer.Receipt){
		"executing":         func(r *answer.Receipt) { r.Op.State = opstore.StateExecuting },
		"tombstoned":        func(r *answer.Receipt) { r.Op.Tombstoned = true },
		"another enrolment": func(r *answer.Receipt) { r.Op.Ref.EnrolmentID = bytes.Repeat([]byte{1}, 16) },
		"no Provider":       func(r *answer.Receipt) { r.Record.ProviderUID = "" },
		"no host":           func(r *answer.Receipt) { r.HostID = "" },
		"short install":     func(r *answer.Receipt) { r.Record.InstallID = r.Record.InstallID[:8] },
		"short op":          func(r *answer.Receipt) { r.Op.Ref.OpID = r.Op.Ref.OpID[:8] },
		"short binding":     func(r *answer.Receipt) { r.Op.BindingID = r.Op.BindingID[:8] },
		"no kind":           func(r *answer.Receipt) { r.Op.Kind = 0 },
		"unknown kind":      func(r *answer.Receipt) { r.Op.Kind = 999 },
		"attempt 0":         func(r *answer.Receipt) { r.Op.Attempt = 0 },
		"bad uid":           func(r *answer.Receipt) { r.Op.VMUID = "VM/1" },
		"short digest":      func(r *answer.Receipt) { r.Op.IntentDigest = r.Op.IntentDigest[:5] },
		"seq 0":             func(r *answer.Receipt) { r.Acceptance.Position.Seq, r.Op.AcceptedSeq = 0, 0 },
		"other seq":         func(r *answer.Receipt) { r.Acceptance.Position.Seq = 43 },
		"no store":          func(r *answer.Receipt) { r.Acceptance.Position.StoreID = [16]byte{} },
		"short session":     func(r *answer.Receipt) { r.Acceptance.Session = testSession[:8] },
		"no grant digest":   func(r *answer.Receipt) { r.GrantDigest = nil },
	} {
		r := testReceipt(p)
		edit(&r)
		if _, err := answer.SignReceipt(p.HostKey, leafDER(t, p), r); !errors.Is(err, answer.ErrSign) {
			t.Fatalf("%s: %v, want ErrSign", name, err)
		}
	}
}

// TestOperationAnswer_LedgerFields: only a request that asked for the ledger
// fields gets the acceptance position; the head and awaiting_request only
// while the operation is accepted.
func TestOperationAnswer_LedgerFields(t *testing.T) {
	p := trusttest.New(t)
	h := testHost(p)
	v := opView(opstore.StateAccepted, false)
	v.Host, v.Op.AcceptedSeq, v.AcceptedHead, v.AwaitingRequest = h.View, 9, bytes.Repeat([]byte{7}, 32), true
	build := func(h answer.Host, v opstore.OpAnswerView) *providerv1.OperationAnswer {
		t.Helper()
		a, err := answer.OperationAnswer(answer.Operation{Host: h, Ref: v.Op.Ref, View: v})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := answer.Sign(p.HostKey, leafDER(t, p), a); err != nil {
			t.Fatalf("sign: %v", err)
		}
		return a
	}
	if a := build(h, v); a.GetAcceptedSeq() != 0 || a.GetAcceptedHead() != nil || a.GetAwaitingRequest() {
		t.Fatalf("not asked: %v", a)
	}
	h.LedgerRequested = 1
	if a := build(h, v); a.GetAcceptedSeq() != 9 || !bytes.Equal(a.GetAcceptedHead(), v.AcceptedHead) || !a.GetAwaitingRequest() {
		t.Fatalf("asked, accepted: %v", a)
	}
	done := v
	done.Op.State, done.Op.Tombstoned = opstore.StateCompleted, true
	if a := build(h, done); a.GetAcceptedSeq() != 9 || a.GetAcceptedHead() != nil || a.GetAwaitingRequest() {
		t.Fatalf("asked, completed: %v", a)
	}
}

// TestHostLedgerState_Backlog: the ledger state reports what the gate holds
// back, the pending count capped.
func TestHostLedgerState_Backlog(t *testing.T) {
	p := trusttest.New(t)
	h := attestedHost(p)
	h.LedgerRequested = 1
	h.Backlog = opstore.LedgerBacklog{PendingReceipts: 70000, ParkedRequestBytes: 12, ParkedRequestBudgetBytes: 256 << 20}
	a, err := answer.HostAnswer(h)
	if err != nil {
		t.Fatal(err)
	}
	l := a.GetLedger()
	if l.GetPendingReceipts() != opstore.MaxReportedPendingReceipts || l.GetParkedRequestBytes() != 12 ||
		l.GetParkedRequestBudgetBytes() != 256<<20 {
		t.Fatalf("ledger %v", l)
	}
}
