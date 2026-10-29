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

// Package schedbench builds deterministic synthetic clustered-Provider
// fixtures — Hosts, a HostPool, VMClasses and placed VirtualMachines — for the
// scheduler benchmarks and the contention harness of Spike 1A
// (docs/performance/spike-1a-scheduler-baseline.md).
//
// It is a test fixture only: no binary imports it. Every fixture is a pure
// function of its Config (seeded math/rand/v2 PCG), so a scenario is
// reproducible across runs and machines.
//
// # How utilization is controlled
//
// Build first places every VM on a host (weighted by the host's capacity
// class, so a heterogeneous pool gets proportionally more VMs on its larger
// hosts), then sizes each host so that what is committed on it is
// Config.Utilization of its capacity (overcommit 1.0). Utilization is
// therefore exact per host, not an average, and a request either fits a host
// or does not for a reason the scenario controls.
package schedbench

import (
	"fmt"
	"math"
	"math/rand/v2"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
)

// Label keys and values the fixture puts on Hosts and VMs. Scenarios with
// placement constraints select on them.
const (
	// LabelZone is the Host label carrying the host's zone (zone-a..zone-d).
	LabelZone = "topology.virtrigaud.io/zone"
	// LabelRack is the Host label carrying the host's rack.
	LabelRack = "topology.virtrigaud.io/rack"
	// LabelApp is the VM label carrying its application (app-0..app-199).
	LabelApp = "app"
	// LabelTier is the VM label carrying its tier.
	LabelTier = "tier"
	// FeatureAVX2 is the CPU feature 80% of the fixture's hosts advertise.
	FeatureAVX2 = "avx2"
	// FeatureAVX512 is the CPU feature 25% of the fixture's hosts advertise.
	FeatureAVX512 = "avx512f"
	// StoragePool is the storage pool every host sees.
	StoragePool = "nfs01"
	// Network is the network 90% of the hosts carry.
	Network = "br-vlan100"
	// MachineType is the machine type every host advertises.
	MachineType = "pc-q35-8.2"
	// overcommitOne is the pool's 1.0 overcommit ratio (CPU and memory).
	overcommitOne = "1.0"
	// mib is one MiB in bytes.
	mib = 1024 * 1024
	// appCount is how many distinct LabelApp values VMs carry.
	appCount = 200
)

// zones are the LabelZone values, assigned round-robin.
var zones = []string{"zone-a", "zone-b", "zone-c", "zone-d"}

// baseFeatures are CPU features every host advertises (a realistic
// virsh-capabilities-sized list, so feature filters scan a real-length slice).
var baseFeatures = []string{
	"sse", "sse2", "sse3", "ssse3", "sse4.1", "sse4.2", "popcnt", "aes", "pclmulqdq",
	"xsave", "osxsave", "fma", "f16c", "rdrand", "bmi1", "bmi2", "erms", "invpcid",
	"adx", "smap", "smep", "clflushopt", "xsaveopt", "xsavec", "xgetbv1", "xsaves",
	"rdseed", "movbe", "lahf_lm", "abm", "3dnowprefetch", "vmx", "x2apic", "tsc-deadline",
}

// Class is one VMClass shape of the fixture and its share of the VMs.
type Class struct {
	// Name is the VMClass name.
	Name string
	// CPU is its vCPU count.
	CPU int32
	// MemoryMiB is its memory in MiB.
	MemoryMiB int64
	// Weight is its relative share of the fixture's VMs.
	Weight int
}

// DefaultClasses is the VM size mix: mostly small and medium VMs, a tail of
// large ones.
var DefaultClasses = []Class{
	{Name: "small", CPU: 1, MemoryMiB: 1024, Weight: 40},
	{Name: "medium", CPU: 2, MemoryMiB: 4096, Weight: 35},
	{Name: "large", CPU: 4, MemoryMiB: 16384, Weight: 20},
	{Name: "xlarge", CPU: 8, MemoryMiB: 32768, Weight: 5},
}

// Config describes one fixture.
type Config struct {
	// Hosts is the number of Hosts in the pool.
	Hosts int
	// VMs is the number of VirtualMachines already placed on the pool.
	VMs int
	// Utilization is the committed fraction of every host's capacity, in
	// (0, 1].
	Utilization float64
	// Heterogeneous gives hosts capacity classes 1x, 2x and 4x (weights
	// 50/35/15%) instead of one size.
	Heterogeneous bool
	// Strategy is the HostPool strategy (v1beta1.PoolStrategySpread or
	// PoolStrategyBinPack).
	Strategy string
	// Namespaces is how many tenant namespaces the VMs are spread across
	// (at least 1). Only VMs in the scheduled VM's namespace are visible to
	// (anti-)affinity; the rest count toward capacity only.
	Namespaces int
	// PendingFraction is the fraction of VMs whose create is in flight
	// (placement.pendingHost, no status.id).
	PendingFraction float64
	// UnrecordedFraction is the fraction of bound VMs without
	// status.currentResources (bound by an older manager), which the
	// accounting sizes from their VMClass.
	UnrecordedFraction float64
	// ProviderNamespace and ProviderName name the clustered Provider.
	ProviderNamespace string
	// ProviderName is the Provider's name.
	ProviderName string
	// Seed seeds the generator; the same Config always yields the same
	// fixture.
	Seed uint64
	// NamePrefix prefixes every Host, VM name and UID, so the fixtures of
	// several Providers can share one store (empty for a single Provider).
	NamePrefix string
}

// Fixture is a generated clustered pool and the VMs placed on it.
type Fixture struct {
	// Config is the configuration it was built from.
	Config Config
	// Hosts are the pool's Hosts, all Ready and schedulable.
	Hosts []v1beta1.Host
	// Pool is the HostPool spec (strategy, overcommit 1.0).
	Pool v1beta1.HostPoolSpec
	// Classes are the VMClasses, one copy of each DefaultClasses shape per
	// tenant namespace.
	Classes []v1beta1.VMClass
	// VMs are the placed VirtualMachines.
	VMs []v1beta1.VirtualMachine
	// MaxHostMemoryMiB is the largest host's memory capacity.
	MaxHostMemoryMiB int64
}

// Namespace returns the name of tenant namespace i.
func Namespace(i int) string { return fmt.Sprintf("tenant-%02d", i) }

// HostName returns the name of host i of a fixture whose NamePrefix is prefix.
func HostName(prefix string, i int) string { return fmt.Sprintf("%shost-%04d", prefix, i) }

// Build generates the fixture described by cfg.
func Build(cfg Config) *Fixture {
	if cfg.Namespaces < 1 {
		cfg.Namespaces = 1
	}
	if cfg.Strategy == "" {
		cfg.Strategy = v1beta1.PoolStrategySpread
	}
	rng := rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15)) // #nosec G404 -- deterministic fixture, not a secret
	f := &Fixture{Config: cfg}

	// Host capacity classes and the cumulative weights VMs are placed by.
	weights := make([]float64, cfg.Hosts)
	for i := range weights {
		weights[i] = 1
		if cfg.Heterogeneous {
			switch r := rng.IntN(100); {
			case r < 50:
				weights[i] = 1
			case r < 85:
				weights[i] = 2
			default:
				weights[i] = 4
			}
		}
	}
	cumulative := make([]float64, cfg.Hosts)
	sum := 0.0
	for i, w := range weights {
		sum += w
		cumulative[i] = sum
	}

	classTotal := 0
	for _, c := range DefaultClasses {
		classTotal += c.Weight
	}
	pickClass := func() Class {
		r := rng.IntN(classTotal)
		for _, c := range DefaultClasses {
			if r < c.Weight {
				return c
			}
			r -= c.Weight
		}
		return DefaultClasses[len(DefaultClasses)-1]
	}
	pickHost := func() int {
		x := rng.Float64() * sum
		lo, hi := 0, len(cumulative)-1
		for lo < hi {
			mid := (lo + hi) / 2
			if cumulative[mid] < x {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		return lo
	}

	for n := 0; n < cfg.Namespaces; n++ {
		for _, c := range DefaultClasses {
			f.Classes = append(f.Classes, v1beta1.VMClass{
				ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: Namespace(n)},
				Spec: v1beta1.VMClassSpec{
					CPU:    c.CPU,
					Memory: *resource.NewQuantity(c.MemoryMiB*mib, resource.BinarySI),
				},
			})
		}
	}

	committedCPU := make([]int64, cfg.Hosts)
	committedMem := make([]int64, cfg.Hosts)
	zero := int64(0)
	f.VMs = make([]v1beta1.VirtualMachine, 0, cfg.VMs)
	for i := 0; i < cfg.VMs; i++ {
		c := pickClass()
		h := pickHost()
		committedCPU[h] += int64(c.CPU)
		committedMem[h] += c.MemoryMiB
		ns := Namespace(i % cfg.Namespaces)
		vm := baseVM(fmt.Sprintf("%svm-%06d", cfg.NamePrefix, i), ns, cfg, c.Name, rng.IntN(appCount))
		vm.UID = types.UID(fmt.Sprintf("%suid-%08d", cfg.NamePrefix, i))
		vm.Status.BoundProvider = &v1beta1.BoundProviderRef{Namespace: cfg.ProviderNamespace, Name: cfg.ProviderName}
		cpu, mem := c.CPU, c.MemoryMiB
		switch r := rng.Float64(); {
		case r < cfg.PendingFraction:
			vm.Status.Placement = &v1beta1.PlacementStatus{
				PendingHost:      HostName(cfg.NamePrefix, h),
				PendingResources: &v1beta1.PlacementResources{CPU: cpu, MemoryMiB: mem},
				MemoryCeilingMiB: &zero,
			}
		case r < cfg.PendingFraction+cfg.UnrecordedFraction:
			vm.Status.ID = fmt.Sprintf("%s.%s", ns, vm.Name)
			vm.Status.Placement = &v1beta1.PlacementStatus{Host: HostName(cfg.NamePrefix, h)}
		default:
			vm.Status.ID = fmt.Sprintf("%s.%s", ns, vm.Name)
			vm.Status.Placement = &v1beta1.PlacementStatus{Host: HostName(cfg.NamePrefix, h), MemoryCeilingMiB: &zero}
			vm.Status.CurrentResources = &v1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}
		}
		f.VMs = append(f.VMs, *vm)
	}

	// Size each host so its committed resources are Utilization of its
	// capacity. An empty host gets the capacity its weight implies for the
	// average host, so a sparse fixture still has room.
	avgMem := 0.0
	avgCPU := 0.0
	if cfg.Hosts > 0 {
		for i := range committedMem {
			avgMem += float64(committedMem[i]) / weights[i]
			avgCPU += float64(committedCPU[i]) / weights[i]
		}
		avgMem /= float64(cfg.Hosts)
		avgCPU /= float64(cfg.Hosts)
	}
	for i := 0; i < cfg.Hosts; i++ {
		memCap := capacityFor(committedMem[i], avgMem*weights[i], cfg.Utilization, 8192)
		cpuCap := capacityFor(committedCPU[i], avgCPU*weights[i], cfg.Utilization, 4)
		f.MaxHostMemoryMiB = max(f.MaxHostMemoryMiB, memCap)
		f.Hosts = append(f.Hosts, buildHost(i, cfg, cpuCap, memCap, rng))
	}
	f.Pool = v1beta1.HostPoolSpec{
		ProviderRef: v1beta1.LocalObjectReference{Name: cfg.ProviderName},
		Strategy:    cfg.Strategy,
		Overcommit:  &v1beta1.OvercommitRatios{CPU: overcommitOne, Memory: overcommitOne},
	}
	return f
}

// capacityFor sizes one host resource: committed/utilization, or the
// weight-scaled average when nothing is committed, never below floor.
func capacityFor(committed int64, avg float64, utilization float64, floor int64) int64 {
	base := float64(committed)
	if committed == 0 {
		base = avg
	}
	return max(int64(math.Ceil(base/utilization)), floor)
}

// buildHost returns host i: Ready, schedulable, with zone/rack/storage/network
// labels and a CPU feature list.
func buildHost(i int, cfg Config, cpu, memMiB int64, rng *rand.Rand) v1beta1.Host {
	labels := map[string]string{
		LabelZone: zones[i%len(zones)],
		LabelRack: fmt.Sprintf("rack-%02d", i/20),
		scheduler.LabelStoragePoolPrefix + StoragePool: "true",
	}
	if rng.IntN(10) != 0 {
		labels[scheduler.LabelNetworkPrefix+Network] = "true"
	}
	features := append([]string(nil), baseFeatures...)
	if rng.IntN(5) != 0 {
		features = append(features, FeatureAVX2)
	}
	if rng.IntN(4) == 0 {
		features = append(features, FeatureAVX512)
	}
	cpu32 := int32(math.MaxInt32)
	if cpu >= 0 && cpu <= math.MaxInt32 {
		cpu32 = int32(cpu)
	}
	storage := int64(4) << 40
	return v1beta1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: HostName(cfg.NamePrefix, i), Namespace: cfg.ProviderNamespace},
		Spec: v1beta1.HostSpec{
			ProviderRef: v1beta1.LocalObjectReference{Name: cfg.ProviderName},
			PoolRef:     v1beta1.LocalObjectReference{Name: "pool"},
			Endpoint:    fmt.Sprintf("qemu+ssh://root@%s/system", HostName(cfg.NamePrefix, i)),
			Labels:      labels,
			Schedulable: true,
		},
		Status: v1beta1.HostStatus{
			Health:                  v1beta1.HostHealthReady,
			AllocatableCPU:          &cpu32,
			AllocatableMemoryMiB:    &memMiB,
			AllocatableStorageBytes: &storage,
			CPUModel:                "Icelake-Server",
			CPUFeatures:             features,
			MachineTypes:            []string{"pc-i440fx-8.2", MachineType},
		},
	}
}

// baseVM returns an unplaced VM of class in ns, labelled with app.
func baseVM(name, ns string, cfg Config, class string, app int) *v1beta1.VirtualMachine {
	return &v1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  ns,
			Labels:     map[string]string{LabelApp: fmt.Sprintf("app-%d", app), LabelTier: "web"},
			Finalizers: []string{v1beta1.VirtualMachineFinalizer},
		},
		Spec: v1beta1.VirtualMachineSpec{
			ProviderRef: v1beta1.ObjectRef{Name: cfg.ProviderName, Namespace: cfg.ProviderNamespace},
			ClassRef:    v1beta1.ObjectRef{Name: class},
		},
	}
}

// NewVM returns an unplaced VM to schedule: class c, in tenant namespace ns,
// labelled app-<app>, with a UID no placed VM has.
func (f *Fixture) NewVM(name, ns string, c Class, app int) *v1beta1.VirtualMachine {
	vm := baseVM(name, ns, f.Config, c.Name, app)
	vm.UID = types.UID("new-" + f.Config.NamePrefix + ns + "-" + name)
	return vm
}

// Request returns the scheduler.Request the VirtualMachine controller builds
// for a VM of class c (resolveClusterPlacement): the pool, every Host as a
// candidate, no policy. PlacedVMs and VMUID are left for the caller (the
// controller fills them under the assume lock).
func (f *Fixture) Request(c Class) scheduler.Request {
	return scheduler.Request{
		Resources:  scheduler.ResourceRequest{CPU: c.CPU, MemoryMiB: c.MemoryMiB},
		Pool:       f.Pool,
		Candidates: f.Hosts,
	}
}

// PlacedVMs returns the fixture's VMs as the scheduler's PlacedVM list seen
// from a VM in selfNamespace: one entry per (VM, host) its placement names, at
// the size the controller's accounting counts it (recorded
// status.currentResources, else pendingResources, else its class size); VMs in
// another namespace are CapacityOnly. It mirrors what the controller's
// committedPlacementsFrom produces for these fixtures, for the scheduler-only
// benchmarks that cannot import the controller package.
func (f *Fixture) PlacedVMs(selfNamespace string) []scheduler.PlacedVM {
	classSize := map[string]scheduler.ResourceRequest{}
	for _, c := range DefaultClasses {
		classSize[c.Name] = scheduler.ResourceRequest{CPU: c.CPU, MemoryMiB: c.MemoryMiB}
	}
	out := make([]scheduler.PlacedVM, 0, len(f.VMs))
	for i := range f.VMs {
		vm := &f.VMs[i]
		pl := vm.Status.Placement
		if pl == nil {
			continue
		}
		size := classSize[vm.Spec.ClassRef.Name]
		switch {
		case vm.Status.CurrentResources != nil:
			size = scheduler.ResourceRequest{CPU: *vm.Status.CurrentResources.CPU, MemoryMiB: *vm.Status.CurrentResources.MemoryMiB}
		case pl.PendingResources != nil:
			size = scheduler.ResourceRequest{CPU: pl.PendingResources.CPU, MemoryMiB: pl.PendingResources.MemoryMiB}
		}
		host := pl.Host
		if host == "" {
			host = pl.PendingHost
		}
		out = append(out, scheduler.PlacedVM{
			Name:         vm.Name,
			UID:          string(vm.UID),
			HostID:       host,
			Labels:       vm.Labels,
			Resources:    size,
			CapacityOnly: vm.Namespace != selfNamespace,
		})
	}
	return out
}
