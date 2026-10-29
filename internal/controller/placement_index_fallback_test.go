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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/schedbench"
)

// These tests pin the direct index read's failure modes (indexedReader): it
// never turns a missing or renamed index into an empty Provider (zero
// committed capacity), and without an indexer the List path is used.

// fallbackWorld is a small fixture in a fake client (the List path) and the
// probe VM.
func fallbackWorld(t *testing.T) (client.Client, *schedbench.Fixture, *infravirtrigaudiov1beta1.VirtualMachine) {
	t.Helper()
	f := smallFixture(schedbench.ProviderName, 41)
	objs := make([]client.Object, 0, len(f.VMs)+len(f.Classes))
	for i := range f.VMs {
		objs = append(objs, f.VMs[i].DeepCopy())
	}
	for i := range f.Classes {
		objs = append(objs, f.Classes[i].DeepCopy())
	}
	fc := fake.NewClientBuilder().WithScheme(coverageTestScheme(t)).WithObjects(objs...).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).Build()
	return fc, f, f.NewVM("probe", schedbench.SelfNamespace, schedbench.MediumRequest, selfApp)
}

// indexerOf loads f's VMs into a client-go indexer with indexers.
func indexerOf(t *testing.T, f *schedbench.Fixture, indexers toolscache.Indexers) toolscache.Indexer {
	t.Helper()
	ix := toolscache.NewIndexer(toolscache.MetaNamespaceKeyFunc, indexers)
	for i := range f.VMs {
		require.NoError(t, ix.Add(f.VMs[i].DeepCopy()))
	}
	return ix
}

func TestIndexedReader_MissingIndexIsAnError(t *testing.T) {
	fc, f, probe := fallbackWorld(t)
	r := withPlacementIndex(fc, indexerOf(t, f, toolscache.Indexers{}))
	_, err := committedPlacementsFrom(context.Background(), r, schedbenchProvider(), probe, nil, false)
	require.Error(t, err, "an indexer without the placement index fails the read; it is never an empty Provider")
}

func TestIndexedReader_RenamedIndexKeyFallsBackToList(t *testing.T) {
	fc, f, probe := fallbackWorld(t)
	// The index exists but under another key scheme (as if controller-runtime
	// renamed its cluster-wide key): the direct read finds nothing.
	renamed := indexerOf(t, f, toolscache.Indexers{
		crFieldIndexPrefix + placementProviderIndex: func(obj interface{}) ([]string, error) {
			vm, _ := obj.(*infravirtrigaudiov1beta1.VirtualMachine)
			return []string{"renamed/" + placementProviderKey(vm).String()}, nil
		},
	})
	r := withPlacementIndex(fc, renamed)
	ix, ok := r.(providerVMIndex)
	require.True(t, ok)
	direct, err := ix.providerVMs(schedbenchProvider())
	require.NoError(t, err)
	require.Empty(t, direct)

	got, err := committedPlacementsFrom(context.Background(), r, schedbenchProvider(), probe, nil, false)
	require.NoError(t, err)
	ref, err := refCommittedPlacements(context.Background(), fc, schedbenchProvider(), probe)
	require.NoError(t, err)
	self := vmSchedulingUID(probe)
	assert.Equal(t, committedTotals(ref.placed, nil, self), committedTotals(got.placed, got.byHost, self),
		"the empty direct read is confirmed through the List: the Provider's VMs are all counted")
	assert.Equal(t, len(f.VMs), len(got.placed)+summedVMs(got.byHost))
}

func TestWithPlacementIndex_NoIndexerKeepsTheListPath(t *testing.T) {
	fc, _, _ := fallbackWorld(t)
	r := withPlacementIndex(fc, nil)
	_, direct := r.(providerVMIndex)
	assert.False(t, direct, "no indexer (an informer that does not expose one): the List path")
	assert.Same(t, fc, r)
}

// TestPlacementSnapshot_BytesStayBelowAVMCopy guards the benchmarked path:
// through the direct index a snapshot allocates far less than one
// VirtualMachine struct (728 bytes) per VM. The List path copies every VM
// into the list (apimeta.SetList), so a regression to it — or to per-VM
// records — fails here.
func TestPlacementSnapshot_BytesStayBelowAVMCopy(t *testing.T) {
	cfg := schedbench.Scenarios()[0].Config // 10 hosts
	cfg.VMs = 2000
	w := newBenchWorld(schedbench.Build(cfg))
	p := w.providers[0]
	vm := w.scenarioVM(p, "bytes", schedbench.MediumRequest)
	_, direct := w.r.Client.(providerVMIndex)
	require.True(t, direct, "the benchmark seam reads the index directly, as production does")
	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := committedPlacementsFrom(context.Background(), w.r.Client, p.provider, vm, nil, false); err != nil {
				b.Fatal(err)
			}
		}
	})
	perVM := float64(res.AllocedBytesPerOp()) / float64(cfg.VMs)
	assert.Less(t, perVM, 200.0, "bytes allocated per VM per snapshot (a List copies at least 728)")
}

// schedbenchProvider is the fixtures' Provider.
func schedbenchProvider() types.NamespacedName {
	return types.NamespacedName{Namespace: schedbench.ProviderNamespace, Name: schedbench.ProviderName}
}
