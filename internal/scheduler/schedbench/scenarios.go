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

package schedbench

import (
	"fmt"
	"os"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
)

// Fixed names of the benchmark Provider and of the namespace the scheduled VM
// lives in.
const (
	// ProviderNamespace is the clustered Provider's namespace.
	ProviderNamespace = "infra"
	// ProviderName is the clustered Provider's name.
	ProviderName = "kvm-cluster"
	// SelfNamespace is the scheduled VM's namespace (tenant 0).
	SelfNamespace = "tenant-00"
	// excludedHostCap is the most hosts status.placement.excludedHosts holds.
	excludedHostCap = 16
	// FilterEnv selects scenarios by name substring (comma-separated);
	// unset runs every scenario.
	FilterEnv = "SCHEDBENCH_SCENARIOS"
	// SkipLargeEnv, when "true", skips scenarios with 100,000 VMs.
	SkipLargeEnv = "SCHEDBENCH_SKIP_LARGE"
)

// Scenario is one benchmark case: a fixture, the class of the VM being
// scheduled, and an optional change to the Request.
type Scenario struct {
	// Name is "h<hosts>_v<vms>/<variant>".
	Name string
	// Config builds the fixture.
	Config Config
	// Class is the scheduled VM's class.
	Class Class
	// Expect is "fit" when a host should be found, "nofit" when none should.
	Expect string
	// apply changes the Request (policy, exclusions, constraints).
	apply func(f *Fixture, req *scheduler.Request)
}

// Apply applies the scenario's Request change, if any.
func (s Scenario) Apply(f *Fixture, req *scheduler.Request) {
	if s.apply != nil {
		s.apply(f, req)
	}
}

// Request sizes for the request-size variants.
var (
	// SmallRequest is a 1 vCPU / 1 GiB microVM-sized request.
	SmallRequest = Class{Name: "small", CPU: 1, MemoryMiB: 1024}
	// MediumRequest is the default request.
	MediumRequest = Class{Name: "medium", CPU: 2, MemoryMiB: 4096}
	// LargeRequest is a 16 vCPU / 64 GiB request.
	LargeRequest = Class{Name: "large-request", CPU: 16, MemoryMiB: 65536}
)

// Expect values.
const (
	// ExpectFit means a host must be found.
	ExpectFit = "fit"
	// ExpectNoFit means no host may be found.
	ExpectNoFit = "nofit"
	// ExpectAny means either outcome is valid (request near the free capacity).
	ExpectAny = "any"
)

// grid is the (hosts, VMs) matrix. Combinations whose density is not a
// plausible pool are left out: 10 hosts with 10,000+ VMs (1,000+ VMs per
// host), and 1,000 VMs on 500+ hosts (setup dominates a ~2-VM-per-host pool).
// 100 hosts with 100,000 VMs is kept as the dense microVM case.
var grid = []struct{ hosts, vms int }{
	{10, 1000},
	{100, 1000},
	{100, 10000},
	{100, 100000},
	{500, 10000},
	{500, 100000},
	{1000, 10000},
	{1000, 100000},
}

// variantShapes are the shapes the variants run on: one mid-size and the
// largest.
var variantShapes = []struct{ hosts, vms int }{{100, 10000}, {1000, 100000}}

// baseConfig is the default fixture: 70% utilization, uniform hosts, Spread,
// 50 tenant namespaces, 1% pending creates, 1% bound VMs without a recorded
// size.
func baseConfig(hosts, vms int) Config {
	return Config{
		Hosts:              hosts,
		VMs:                vms,
		Utilization:        0.7,
		Strategy:           v1beta1.PoolStrategySpread,
		Namespaces:         50,
		PendingFraction:    0.01,
		UnrecordedFraction: 0.01,
		ProviderNamespace:  ProviderNamespace,
		ProviderName:       ProviderName,
		Seed:               uint64(hosts)*1_000_003 + uint64(vms),
	}
}

// Scenarios returns every scenario, filtered by FilterEnv and SkipLargeEnv.
func Scenarios() []Scenario {
	var all []Scenario
	for _, g := range grid {
		all = append(all, Scenario{
			Name: shapeName(g.hosts, g.vms) + "/base", Config: baseConfig(g.hosts, g.vms),
			Class: MediumRequest, Expect: ExpectFit,
		})
	}
	for _, g := range variantShapes {
		all = append(all, variants(g.hosts, g.vms)...)
	}
	return filter(all)
}

// shapeName renders a grid point.
func shapeName(hosts, vms int) string {
	v := fmt.Sprintf("%d", vms)
	if vms%1000 == 0 {
		v = fmt.Sprintf("%dk", vms/1000)
	}
	return fmt.Sprintf("h%d_v%s", hosts, v)
}

// variants returns the scenario variants for one shape.
func variants(hosts, vms int) []Scenario {
	prefix := shapeName(hosts, vms) + "/"
	with := func(name string, mutate func(*Config), class Class, expect string, apply func(*Fixture, *scheduler.Request)) Scenario {
		cfg := baseConfig(hosts, vms)
		if mutate != nil {
			mutate(&cfg)
		}
		return Scenario{Name: prefix + name, Config: cfg, Class: class, Expect: expect, apply: apply}
	}
	return []Scenario{
		with("util-low", func(c *Config) { c.Utilization = 0.3 }, MediumRequest, ExpectFit, nil),
		with("util-high", func(c *Config) { c.Utilization = 0.95 }, MediumRequest, ExpectAny, nil),
		with("hetero", func(c *Config) { c.Heterogeneous = true }, MediumRequest, ExpectFit, nil),
		with("binpack", func(c *Config) { c.Strategy = v1beta1.PoolStrategyBinPack }, MediumRequest, ExpectFit, nil),
		with("req-small", nil, SmallRequest, ExpectFit, nil),
		with("req-large", nil, LargeRequest, ExpectAny, nil),
		with("unschedulable", nil, MediumRequest, ExpectNoFit, func(f *Fixture, req *scheduler.Request) {
			req.Resources.MemoryMiB = 2 * f.MaxHostMemoryMiB
		}),
		with("constrained", nil, MediumRequest, ExpectFit, applyConstraints),
		with("affinity", func(c *Config) { c.Namespaces = 1 }, MediumRequest, ExpectFit, applyAffinity),
	}
}

// applyConstraints excludes 16 hosts (the excludedHosts cap) and adds a hard
// zone node-selector, a required CPU feature, and storage/network visibility
// requirements. The controller does not fill RequiredStoragePools today
// (TODO(ADR-0007 D6)); the scenario sets it to measure the filter as designed.
func applyConstraints(f *Fixture, req *scheduler.Request) {
	for i := 0; i < excludedHostCap && i < len(f.Hosts); i++ {
		req.ExcludedHosts = append(req.ExcludedHosts, f.Hosts[i].Name)
	}
	req.RequiredStoragePools = []string{StoragePool}
	req.RequiredNetworks = []string{Network}
	req.Policy = &v1beta1.VMPlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "constrained", Namespace: SelfNamespace},
		Spec: v1beta1.VMPlacementPolicySpec{
			Hard:                &v1beta1.PlacementConstraints{NodeSelector: map[string]string{LabelZone: "zone-b"}},
			ResourceConstraints: &v1beta1.ResourceConstraints{RequiredFeatures: []string{FeatureAVX2}},
		},
	}
}

// applyAffinity adds a required VM anti-affinity to a rare app (most hosts
// must scan all their VMs to pass) and a preferred VM affinity to a common
// tier (scored on every feasible host). With one tenant namespace every
// placed VM is in affinity scope.
func applyAffinity(_ *Fixture, req *scheduler.Request) {
	req.Policy = &v1beta1.VMPlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "affinity", Namespace: SelfNamespace},
		Spec: v1beta1.VMPlacementPolicySpec{
			AntiAffinity: &v1beta1.AntiAffinityRules{VMAntiAffinity: &v1beta1.VMAntiAffinity{
				RequiredDuringScheduling: []v1beta1.VMAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{LabelApp: "app-7"}},
				}},
			}},
			Affinity: &v1beta1.AffinityRules{VMAffinity: &v1beta1.VMAffinity{
				PreferredDuringScheduling: []v1beta1.WeightedVMAffinityTerm{{
					Weight: 50,
					VMAffinityTerm: v1beta1.VMAffinityTerm{
						LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{LabelTier: "web"}},
					},
				}},
			}},
		},
	}
}

// filter applies FilterEnv and SkipLargeEnv.
func filter(all []Scenario) []Scenario {
	skipLarge := os.Getenv(SkipLargeEnv) == "true"
	var subs []string
	if v := os.Getenv(FilterEnv); v != "" {
		subs = strings.Split(v, ",")
	}
	var out []Scenario
	for _, s := range all {
		if skipLarge && s.Config.VMs >= 100000 {
			continue
		}
		if len(subs) > 0 && !matchesAny(s.Name, subs) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// matchesAny reports whether name contains any of subs.
func matchesAny(name string, subs []string) bool {
	for _, s := range subs {
		if strings.Contains(name, strings.TrimSpace(s)) {
			return true
		}
	}
	return false
}
