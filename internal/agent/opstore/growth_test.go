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
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// TestGrowthBreakdown drives create, power and delete cycles through the
// store's real admit and execute paths, compacts, trims the audit window to
// one event, and reports what each finished operation leaves behind, per
// bucket: what the store keeps for good (op_id tombstones, attempt marks, VM
// tombstones) once the audit window is full. go test -run
// TestGrowthBreakdown -v ./internal/agent/opstore/ prints the table;
// VIRTRIGAUD_GROWTH_CYCLES sets the number of cycles (300 by default; the
// documented figures used 3000).
func TestGrowthBreakdown(t *testing.T) {
	cycles := 300
	if v, err := strconv.Atoi(os.Getenv("VIRTRIGAUD_GROWTH_CYCLES")); err == nil && v > 0 {
		cycles = v
	}
	h := newHarness(t)
	h.opts = func(o *opstore.Options) { o.AuditRetainEvents = 1 }
	h.fresh()
	ctx := context.Background()
	before, err := opstore.BucketUsages(ctx, h.s)
	if err != nil {
		t.Fatal(err)
	}
	statsBefore := h.s.Journal().BucketStats()
	liveBefore := h.s.Journal().LiveSize()
	for i := 0; i < cycles; i++ {
		b := rid()
		vm := fmt.Sprintf("vm-%d", i)
		h.do(h.req(vm, b, kCreate, 1))
		h.do(h.req(vm, b, kPowerOn, 1))
		h.do(h.req(vm, b, kDelete, 1))
	}
	if _, err := h.s.Compact(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.TrimAudit(ctx, 0); err != nil {
		t.Fatal(err)
	}
	after, err := opstore.BucketUsages(ctx, h.s)
	if err != nil {
		t.Fatal(err)
	}
	ops := 3 * cycles
	names := make([]string, 0, len(after))
	for n := range after {
		names = append(names, n)
	}
	sort.Strings(names)
	total := 0
	for _, n := range names {
		d := after[n].Bytes - before[n].Bytes
		total += d
		t.Logf("%-14s keys %+6d  bytes %+8d  = %6.1f B/op  %6.1f B/VM", n, after[n].Keys-before[n].Keys, d, float64(d)/float64(ops), float64(d)/float64(cycles))
	}
	stats := h.s.Journal().BucketStats()
	pages, kept := int64(0), int64(0)
	for _, n := range sortedKeys(stats) {
		d := stats[n].Allocated - statsBefore[n].Allocated
		pages += d
		if !strings.HasPrefix(n, "journal.") {
			kept += d
		}
		t.Logf("pages %-20s keys %+6d  in use %+9d  allocated %+9d  = %6.1f B/op", n, stats[n].Keys-statsBefore[n].Keys,
			stats[n].InUse-statsBefore[n].InUse, d, float64(d)/float64(ops))
	}
	t.Logf("pages allocated %+d B = %.1f B/op", pages, float64(pages)/float64(ops))
	growth := h.s.Journal().LiveSize() - liveBefore
	t.Logf("records total %d B = %.1f B/op; live size %+d B = %.1f B/op (%d ops, %d VMs)", total, float64(total)/float64(ops),
		growth, float64(growth)/float64(ops), ops, cycles)
	// The store's own buckets after compaction and audit retention hold what
	// it keeps for good (the journal's chain is bounded and left out): far
	// below the 1.3 KB per operation an untrimmed audit bucket made it.
	if per := float64(kept) / float64(ops); per > 900 {
		t.Errorf("%.0f B per finished operation kept after compaction and audit retention", per)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
