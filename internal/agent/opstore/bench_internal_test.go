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

package opstore

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"os"
	"strconv"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
)

// BenchmarkTombstoneGrowth reports the store size per tombstone after
// VIRTRIGAUD_BENCH_TOMBSTONES tombstones (default 100000; set 1000000 for the
// larger measurement), written 1000 per commit, and the reopen time. Report
// only.
func BenchmarkTombstoneGrowth(b *testing.B) {
	n := 100000
	if v, err := strconv.Atoi(os.Getenv("VIRTRIGAUD_BENCH_TOMBSTONES")); err == nil && v > 0 {
		n = v
	}
	for i := 0; i < b.N; i++ {
		dir := b.TempDir()
		opts := journal.Options{Dir: dir, Buckets: buckets()}
		j, err := journal.Open(context.Background(), opts)
		if err != nil {
			b.Fatal(err)
		}
		if err := j.EstablishContinuity(context.Background()); err != nil {
			b.Fatal(err)
		}
		enrol := make([]byte, 16)
		_, _ = rand.Read(enrol)
		for done := 0; done < n; {
			if _, err := j.Update(context.Background(), func(tx *journal.Tx) error {
				for k := 0; k < 1000 && done < n; k++ {
					op := make([]byte, 16)
					binary.BigEndian.PutUint64(op, uint64(done)) // #nosec G115 -- benchmark counter
					id := &pb.OpIdentity{ProviderUid: "provider-uid", EnrolmentId: enrol, OpId: op, VmUid: "vm-uid-0123456789",
						BindingId: enrol, Kind: 3, Attempt: uint64(done + 1), IntentDigest: make([]byte, 32), MintEpoch: &pb.Epoch{Counter: 2, Holder: enrol}} // #nosec G115 -- benchmark counter
					if err := put(tx, bucketTombstones, opKey(enrol, op), &pb.Tombstone{Identity: id, State: StateCompleted, Irrev: true}); err != nil {
						return err
					}
					done++
				}
				return nil
			}); err != nil {
				b.Fatal(err)
			}
		}
		size := j.Size()
		_ = j.Close()
		b.ReportMetric(float64(size)/float64(n), "store-bytes/tombstone")
		b.ReportMetric(float64(size)/(1<<20), "store-MiB")
		j2, err := journal.Open(context.Background(), opts)
		if err != nil {
			b.Fatal(err)
		}
		_ = j2.Close()
	}
}
