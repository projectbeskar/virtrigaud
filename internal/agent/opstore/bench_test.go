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
	"context"
	"fmt"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// Benchmarks report costs; they are not a gate. Run with
// go test -run '^$' -bench . ./internal/agent/opstore/. They use the real file
// system under the test's temporary directory, so the two fsyncs per
// acknowledgement are real.

func benchStore(b *testing.B) (*opstore.Store, *harness) {
	b.Helper()
	h := newHarness(b)
	h.dir = b.TempDir() // real disk: real fsync
	h.mem = nil
	o := h.options()
	o.Journal.FS = journal.OSFS{}
	s, err := opstore.Open(context.Background(), o)
	if err != nil {
		b.Fatal(err)
	}
	h.s = s
	if err := s.EstablishContinuity(context.Background()); err != nil {
		b.Fatal(err)
	}
	if _, err := s.Recover(context.Background()); err != nil {
		b.Fatal(err)
	}
	if err := s.Enrol(context.Background(), opstore.Enrolment{ID: h.enrol, ProviderUID: h.provider, StartEpoch: h.start}); err != nil {
		b.Fatal(err)
	}
	return s, h
}

// BenchmarkAdmitAck is the latency of one acknowledged admission: one bbolt
// commit and one anchor append, each fsynced.
func BenchmarkAdmitAck(b *testing.B) {
	s, h := benchStore(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Admit(context.Background(), h.req(fmt.Sprintf("vm-%d", i), rid(), kCreate, 1)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOperationLifecycle is a whole Create: accept, two steps (intent
// and done each), witness, terminal.
func BenchmarkOperationLifecycle(b *testing.B) {
	s, h := benchStore(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := h.req(fmt.Sprintf("vm-%d", i), rid(), kCreate, 1)
		if _, err := s.Admit(context.Background(), r); err != nil {
			b.Fatal(err)
		}
		if _, err := s.Execute(context.Background(), ref(r), nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGroupCommit writes 64 records per acknowledged commit.
func BenchmarkGroupCommit(b *testing.B) {
	dir := b.TempDir()
	j, err := journal.Open(context.Background(), journal.Options{Dir: dir, Buckets: []string{"data"}})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	if err := j.EstablishContinuity(context.Background()); err != nil {
		b.Fatal(err)
	}
	val := make([]byte, 110)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := j.Update(context.Background(), func(tx *journal.Tx) error {
			for k := 0; k < 64; k++ {
				if err := tx.Put("data", []byte(fmt.Sprintf("%d-%d", i, k)), val); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(64, "records/commit")
}
