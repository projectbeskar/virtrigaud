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
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/schedbench"
)

// F4/F5 micro-benchmarks: the old and new per-event reads, over the Spike 1A
// fixtures loaded into the informer-store seam (placement_bench_seam_test.go).
// One Provider holds every VM, the indexed paths' worst case. Run with
//
//	go test ./internal/controller/ -run '^$' -bench 'Benchmark(CountConnectedVMs|CommittedOnHost)' -benchmem

// accountingShapes are the fixture sizes measured.
var accountingShapes = []schedbench.Config{
	{Hosts: 100, VMs: 10000},
	{Hosts: 1000, VMs: 100000},
}

// accountingWorld builds (or reuses) the world of shape.
func accountingWorld(shape schedbench.Config) *benchWorld {
	cfg := schedbench.Config{
		Hosts: shape.Hosts, VMs: shape.VMs, Utilization: 0.7, Strategy: infravirtrigaudiov1beta1.PoolStrategySpread,
		Namespaces: 50, PendingFraction: 0.01, UnrecordedFraction: 0.01,
		ProviderNamespace: schedbench.ProviderNamespace, ProviderName: schedbench.ProviderName,
		Seed: uint64(shape.Hosts)*1_000_003 + uint64(shape.VMs),
	}
	return worldFor(cfg)
}

// shapeLabel names a shape in benchmark output.
func shapeLabel(c schedbench.Config) string {
	if c.VMs >= 100000 {
		return "h1000_v100k"
	}
	return "h100_v10k"
}

// BenchmarkCountConnectedVMs: the Provider's VM count, old (every VM in the
// cluster, deep-copied) and new (specProviderIndex, shallow).
func BenchmarkCountConnectedVMs(b *testing.B) {
	ctx := context.Background()
	for _, shape := range accountingShapes {
		w := accountingWorld(shape)
		p := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{
			Namespace: w.providers[0].provider.Namespace, Name: w.providers[0].provider.Name}}
		r := &ProviderReconciler{Client: w.r.Client}
		b.Run(shapeLabel(shape)+"/legacy", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := legacyCountConnectedVMs(ctx, w.r.Client, p); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(shapeLabel(shape)+"/indexed", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := r.countConnectedVMs(ctx, p); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCommittedOnHost: one Host heartbeat's committed-capacity read, old
// (every VM of the Provider) and new (placementHostIndex, that host's VMs).
func BenchmarkCommittedOnHost(b *testing.B) {
	ctx := context.Background()
	for _, shape := range accountingShapes {
		w := accountingWorld(shape)
		p := w.providers[0]
		host := p.fixture.Hosts[0].Name
		b.Run(shapeLabel(shape)+"/legacy", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, err := legacyCommittedOnHost(ctx, w.r.Client, p.provider, host); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(shapeLabel(shape)+"/indexed", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, err := committedOnHost(ctx, w.r.Client, p.provider, host); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
