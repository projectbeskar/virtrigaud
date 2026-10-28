//go:build schedbench_envtest

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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/schedbench"
)

// Spike 1A seam fidelity check (opt-in: -tags schedbench_envtest).
//
// It loads a fixture into a real API server (envtest), reads it back through
// a real controller-runtime informer cache with the placement-provider index,
// and times committedPlacementsFrom through that cache and through the
// informerStore seam loaded with the very objects the cache decoded (managed
// fields and all). It also reports the live heap the real cache holds.
//
//	KUBEBUILDER_ASSETS=$(bin/setup-envtest use -p path ...) \
//	SCHEDBENCH_ENVTEST_VMS=10000 go test -tags schedbench_envtest ./internal/controller/ \
//	  -run '^$' -bench BenchmarkEnvtestScan -benchtime 20x

// envtestVMsEnv sets the fixture size (default 10,000 VMs on 100 hosts).
const envtestVMsEnv = "SCHEDBENCH_ENVTEST_VMS"

// BenchmarkEnvtestScan compares the real informer cache with the seam.
func BenchmarkEnvtestScan(b *testing.B) {
	vms := 10000
	if v, err := strconv.Atoi(os.Getenv(envtestVMsEnv)); err == nil && v > 0 {
		vms = v
	}
	hosts := max(vms/100, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		b.Fatal(err)
	}
	if err := infravirtrigaudiov1beta1.AddToScheme(scheme); err != nil {
		b.Fatal(err)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
		env.BinaryAssetsDirectory = dir
	}
	cfg, err := env.Start()
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = env.Stop() }()
	cfg.QPS, cfg.Burst = 5000, 10000
	writer, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		b.Fatal(err)
	}

	f := schedbench.Build(schedbench.Config{
		Hosts: hosts, VMs: vms, Utilization: 0.7, Strategy: infravirtrigaudiov1beta1.PoolStrategySpread,
		Namespaces: 50, PendingFraction: 0.01, UnrecordedFraction: 0.01,
		ProviderNamespace: schedbench.ProviderNamespace, ProviderName: schedbench.ProviderName, Seed: 42,
	})
	load := time.Now()
	loadFixture(ctx, b, writer, f)
	b.Logf("loaded %d VMs into envtest in %s", vms, time.Since(load).Round(time.Second))

	before := heapMiB()
	c, err := cache.New(cfg, cache.Options{Scheme: scheme})
	if err != nil {
		b.Fatal(err)
	}
	if err := c.IndexField(ctx, &infravirtrigaudiov1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue); err != nil {
		b.Fatal(err)
	}
	// Start the VMClass informer too (the accounting reads classes).
	if _, err := c.GetInformer(ctx, &infravirtrigaudiov1beta1.VMClass{}); err != nil {
		b.Fatal(err)
	}
	go func() { _ = c.Start(ctx) }()
	if !c.WaitForCacheSync(ctx) {
		b.Fatal("cache did not sync")
	}
	reader, err := client.New(cfg, client.Options{Scheme: scheme, Cache: &client.CacheOptions{Reader: c}})
	if err != nil {
		b.Fatal(err)
	}
	key := types.NamespacedName{Namespace: schedbench.ProviderNamespace, Name: schedbench.ProviderName}
	self := f.NewVM("probe", schedbench.SelfNamespace, schedbench.MediumRequest, selfApp)
	// The initial list can predate the last status writes; wait until the
	// cache holds every placement.
	deadline := time.Now().Add(5 * time.Minute)
	for {
		snap, err := committedPlacementsFrom(ctx, reader, key, self, nil, false)
		if err != nil {
			b.Fatal(err)
		}
		if len(snap.placed)+summedVMs(snap.byHost) == vms {
			break
		}
		if time.Now().After(deadline) {
			b.Fatalf("cache holds %d of %d placements after 5 minutes", len(snap.placed)+summedVMs(snap.byHost), vms)
		}
		time.Sleep(time.Second)
	}
	cacheHeap := heapMiB() - before

	// The seam, loaded with the objects the real cache decoded.
	var cached infravirtrigaudiov1beta1.VirtualMachineList
	if err := c.List(ctx, &cached); err != nil {
		b.Fatal(err)
	}
	var classes infravirtrigaudiov1beta1.VMClassList
	if err := c.List(ctx, &classes); err != nil {
		b.Fatal(err)
	}
	seam := newInformerStore()
	for i := range cached.Items {
		seam.mustAdd(&cached.Items[i])
	}
	for i := range classes.Items {
		seam.mustAdd(&classes.Items[i])
	}
	sc := &storeClient{store: seam}

	for _, rc := range []struct {
		name   string
		reader client.Reader
	}{{"real-cache", reader}, {"seam", sc}} {
		b.Run(fmt.Sprintf("v%d/%s", vms, rc.name), func(b *testing.B) {
			snap, err := committedPlacementsFrom(ctx, rc.reader, key, self, nil, false)
			if err != nil {
				b.Fatal(err)
			}
			if len(snap.placed)+summedVMs(snap.byHost) != vms {
				b.Fatalf("%s: %d placements, want %d", rc.name, len(snap.placed)+summedVMs(snap.byHost), vms)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := committedPlacementsFrom(ctx, rc.reader, key, self, nil, false); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(cacheHeap, "real-cache-heap-MiB")
		})
	}
}

// loadFixture creates the fixture's namespaces, VMClasses and VMs (with their
// placement status) in the API server.
func loadFixture(ctx context.Context, b *testing.B, c client.Client, f *schedbench.Fixture) {
	b.Helper()
	nss := map[string]bool{schedbench.ProviderNamespace: true}
	for i := range f.VMs {
		nss[f.VMs[i].Namespace] = true
	}
	for ns := range nss {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			b.Fatal(err)
		}
	}
	for i := range f.Classes {
		if err := c.Create(ctx, f.Classes[i].DeepCopy()); err != nil {
			b.Fatal(err)
		}
	}
	work := make(chan int)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	for w := 0; w < 64; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				src := &f.VMs[i]
				vm := src.DeepCopy()
				vm.UID = ""
				vm.Status = infravirtrigaudiov1beta1.VirtualMachineStatus{}
				if err := c.Create(ctx, vm); err != nil {
					once.Do(func() { firstErr = err })
					continue
				}
				vm.Status = src.Status
				if err := c.Status().Update(ctx, vm); err != nil {
					once.Do(func() { firstErr = err })
				}
			}
		}()
	}
	for i := range f.VMs {
		work <- i
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		b.Fatal(firstErr)
	}
}
