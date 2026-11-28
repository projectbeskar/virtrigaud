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

package controller

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/operation"
)

// The write cost of the host ledger per receipt raise, per heartbeat raise
// and per ledger mark (the ledger written again by a manager that has no
// write of its own), and of an uncached bound read, against a fake API
// server. Besides time, each benchmark reports the API calls per operation:
// ledger Secret writes, ledger Secret reads, and pins state (store state
// and ledger index) writes. Run with:
//
//	go test -run '^$' -bench BenchmarkHostLedger ./internal/controller

// benchLedger is a host ledger with one attested host over a counting fake
// API server.
type benchLedger struct {
	l       *HostLedger
	trust   *staticTrust
	store   [16]byte
	session [16]byte
	now     time.Time
	gets    atomic.Int64
	updates atomic.Int64
}

func newBenchLedger(b *testing.B) *benchLedger {
	b.Helper()
	s := k8sruntime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = infravirtrigaudiov1beta1.AddToScheme(s)
	provider := ledgerTestProvider()
	host := &infravirtrigaudiov1beta1.Host{ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: provider.Namespace,
		UID: "00000000-0000-4000-8000-000000000001"},
		Spec: infravirtrigaudiov1beta1.HostSpec{ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: provider.Name},
			Identity: "serial-h"}}
	bl := &benchLedger{now: time.Now().UTC().Truncate(time.Second), store: [16]byte{0x5b}, session: [16]byte{0x5e}}
	cli := interceptor.NewClient(fake.NewClientBuilder().WithScheme(s).WithObjects(host).Build(), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				bl.gets.Add(1)
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				bl.updates.Add(1)
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	enrol := bytes.Repeat([]byte{0xe1}, 16)
	bl.trust = &staticTrust{trusts: map[string]HostTrust{"h": {Enrolment: operation.Enrolment{EnrolmentID: enrol}, HostUID: string(host.UID)}}}
	keys, err := NewHostLedgerKeys(bytes.Repeat([]byte{1}, 32), nil, bytes.Repeat([]byte{2}, 32))
	if err != nil {
		b.Fatal(err)
	}
	bl.l = &HostLedger{Client: cli, APIReader: cli, Trust: bl.trust, Keys: keys,
		Config: &HostEnrolmentConfig{Namespace: "virtrigaud-enrolment", InstallID: bytes.Repeat([]byte{0x42}, 16)},
		Now: func() time.Time { return bl.now }, ManagerVersion: func() string { return "v0.5.0" }}
	ctx := context.Background()
	ev := signedLedgerEvidence(b, provider, "h", enrol, nil, bl.now, testLedgerInit{StoreID: bl.store,
		Position: HostLedgerPosition{Seq: 3, Head: [32]byte{3}}, Attestation: hostidentity.StoreAttestation{Reported: true},
		Origin: &testLedgerOrigin{OriginEnrolmentID: enrol, EnrolmentCount: 1}})
	bl.l.Observe(provider, "h", ev)
	if _, err := bl.l.Initialise(ctx, provider, "h", HostLedgerInit{StoreID: bl.store, Evidence: ev}); err != nil {
		b.Fatal(err)
	}
	if _, err := bl.l.BeginSession(ctx, provider, "h", bl.store, bl.session, LedgerSessionAttest, HostLedgerPosition{Seq: 3}); err != nil {
		b.Fatal(err)
	}
	if _, err := bl.l.Adopt(ctx, provider, "h", bl.store, bl.session, LedgerEvidenceChallengeAnswer); err != nil {
		b.Fatal(err)
	}
	return bl
}

// run runs op b.N times and reports the API calls per operation.
func (bl *benchLedger) run(b *testing.B, op func(i int) error) {
	b.Helper()
	gets, updates, pins := bl.gets.Load(), bl.updates.Load(), bl.trust.writes
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := op(i); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	n := float64(b.N)
	b.ReportMetric(float64(bl.updates.Load()-updates)/n, "ledger-writes/op")
	b.ReportMetric(float64(bl.gets.Load()-gets)/n, "ledger-reads/op")
	b.ReportMetric(float64(bl.trust.writes-pins)/n, "pins-writes/op")
}

// BenchmarkHostLedgerRaiseReceipt: one receipt raise per operation (the
// ledger raised by one position, the index brought to its counter).
func BenchmarkHostLedgerRaiseReceipt(b *testing.B) {
	bl := newBenchLedger(b)
	provider, ctx := ledgerTestProvider(), context.Background()
	bl.run(b, func(i int) error {
		_, err := bl.l.Raise(ctx, provider, "h", bl.store, []HostLedgerRaise{{Session: bl.session,
			Position: HostLedgerPosition{Seq: uint64(4 + i), Head: [32]byte{byte(i), byte(i >> 8), byte(i >> 16)}}}})
		return err
	})
}

// BenchmarkHostLedgerRaiseReceiptBatch64: one raise per batch of 64
// receipts (per operation: one receipt).
func BenchmarkHostLedgerRaiseReceiptBatch64(b *testing.B) {
	bl := newBenchLedger(b)
	provider, ctx := ledgerTestProvider(), context.Background()
	seq := uint64(3)
	batch := make([]HostLedgerRaise, 64)
	bl.run(b, func(i int) error {
		seq++
		batch[i%64] = HostLedgerRaise{Session: bl.session, Position: HostLedgerPosition{Seq: seq, Head: [32]byte{byte(seq), byte(seq >> 8), byte(seq >> 16)}}}
		if i%64 != 63 {
			return nil
		}
		_, err := bl.l.Raise(ctx, provider, "h", bl.store, batch)
		return err
	})
}

// BenchmarkHostLedgerRaiseHeartbeat: one heartbeat-only raise per
// operation, 30 seconds apart (the index written at most every
// ledgerIndexPersistInterval).
func BenchmarkHostLedgerRaiseHeartbeat(b *testing.B) {
	bl := newBenchLedger(b)
	provider, ctx := ledgerTestProvider(), context.Background()
	bl.run(b, func(i int) error {
		bl.now = bl.now.Add(ledgerHeartbeatRaiseInterval)
		_, err := bl.l.Raise(ctx, provider, "h", bl.store, []HostLedgerRaise{{Session: bl.session, Heartbeat: true,
			Position: HostLedgerPosition{Seq: uint64(4 + i), Head: [32]byte{byte(i), byte(i >> 8), byte(i >> 16)}}}})
		return err
	})
}

// BenchmarkHostLedgerMarkReassert: the ledger mark path of a manager with
// no own write of the ledger as it is (a restart or a new leader): the
// ledger written again, unchanged, and the mark signed from that write.
func BenchmarkHostLedgerMarkReassert(b *testing.B) {
	bl := newBenchLedger(b)
	provider, ctx := ledgerTestProvider(), context.Background()
	h := &HostAnswers{Ledger: bl.l}
	auth := &countingAuthority{}
	bl.run(b, func(int) error {
		rec, err := bl.l.Reassert(ctx, provider, "h", bl.store, bl.session)
		if err != nil {
			return err
		}
		_, err = h.sendLedgerMark(ctx, provider, auth, "h", HostTrust{}, rec)
		return err
	})
	if auth.marks != b.N {
		b.Fatalf("%d marks for %d operations", auth.marks, b.N)
	}
}

// BenchmarkHostLedgerLoadBound: one uncached, verified, bound read.
func BenchmarkHostLedgerLoadBound(b *testing.B) {
	bl := newBenchLedger(b)
	provider, ctx := ledgerTestProvider(), context.Background()
	bl.run(b, func(int) error {
		_, err := bl.l.LoadBound(ctx, provider, "h", bl.store)
		return err
	})
}
