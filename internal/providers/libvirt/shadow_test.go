/*
Copyright 2025.

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

package libvirt

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	golibvirt "github.com/digitalocean/go-libvirt"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	obsmetrics "github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// virshResp / nativeResp build the two DescribeResponses the comparator sees. The
// virsh side keys ProviderRaw the way `virsh dominfo` does (UUID/Name/CPU(s)/Max
// memory in KiB); the native side keys it the way buildNativeDescribe does
// (uuid/name/vcpu/memory_mib in MiB).
func virshResp(power string, raw map[string]string) contracts.DescribeResponse {
	return contracts.DescribeResponse{Exists: true, PowerState: power, ProviderRaw: raw}
}

func nativeResp(power string, raw map[string]string) contracts.DescribeResponse {
	return contracts.DescribeResponse{Exists: true, PowerState: power, ProviderRaw: raw}
}

// TestCompareDescribe covers the canonicalizing comparator: semantic-equal cases
// (including formatting-only differences that must NOT count) and genuinely
// divergent cases, plus the two ADR-0008 parity cases (#291 M2 power-state and D2
// memory-unit).
func TestCompareDescribe(t *testing.T) {
	tests := []struct {
		name     string
		virsh    contracts.DescribeResponse
		native   contracts.DescribeResponse
		wantDiff []string
	}{
		{
			name: "identical -> no divergence",
			virsh: virshResp("On", map[string]string{
				"UUID": "abc", "Name": "vm1", "CPU(s)": "2", "Max memory": "4194304 KiB",
			}),
			native: nativeResp("On", map[string]string{
				"uuid": "abc", "name": "vm1", "vcpu": "2", "memory_mib": "4096",
			}),
			wantDiff: nil,
		},
		{
			name: "formatting-only differences canonicalize equal (whitespace, case, unit)",
			virsh: virshResp("on", map[string]string{
				"UUID": "ABC-DEF", "Name": "vm1", "CPU(s)": " 2 ", "Max memory": "4194304 KiB",
			}),
			native: nativeResp("On", map[string]string{
				"uuid": "abc-def", "name": "vm1", "vcpu": "02", "memory_mib": "4096",
			}),
			wantDiff: nil,
		},
		{
			name:     "power_state divergence",
			virsh:    virshResp("On", map[string]string{"UUID": "abc"}),
			native:   nativeResp("Off", map[string]string{"uuid": "abc"}),
			wantDiff: []string{"power_state"},
		},
		{
			name: "vcpu and memory divergence",
			virsh: virshResp("On", map[string]string{
				"CPU(s)": "2", "Max memory": "4194304 KiB", // 4096 MiB
			}),
			native: nativeResp("On", map[string]string{
				"vcpu": "4", "memory_mib": "8192",
			}),
			wantDiff: []string{"vcpu", "memory_mib"},
		},
		{
			name:     "uuid divergence",
			virsh:    virshResp("On", map[string]string{"UUID": "abc"}),
			native:   nativeResp("On", map[string]string{"uuid": "xyz"}),
			wantDiff: []string{"uuid"},
		},
		{
			name:  "field present on only one side is skipped, not divergent",
			virsh: virshResp("On", map[string]string{"UUID": "abc", "CPU(s)": "2"}),
			// native omits uuid and vcpu entirely -> those fields are not compared
			native:   nativeResp("On", map[string]string{}),
			wantDiff: nil,
		},
		{
			name:     "exists divergence",
			virsh:    contracts.DescribeResponse{Exists: true, PowerState: "Off", ProviderRaw: map[string]string{}},
			native:   contracts.DescribeResponse{Exists: false, PowerState: "Off", ProviderRaw: map[string]string{}},
			wantDiff: []string{"exists"},
		},
		{
			// #291 M2: a domain in a non-running state. virsh coarsens it to Off; the
			// native path (mapNativeDomainState) coarsens DomainBlocked to Off too, so
			// there is NO divergence — the drift #291 flagged as benign is proven benign.
			name:     "M2 power-state coarsening agrees (blocked -> Off both sides)",
			virsh:    virshResp("Off", map[string]string{"UUID": "abc"}),
			native:   nativeResp(mapNativeDomainState(golibvirt.DomainBlocked), map[string]string{"uuid": "abc"}),
			wantDiff: nil,
		},
		{
			// The negative of M2: if the native mapping were WRONG (blocked -> On), the
			// comparator MUST catch it. This is what proves the harness detects real
			// text->typed drift rather than silently passing.
			name:     "M2 negative: a wrong native power-state mapping IS caught",
			virsh:    virshResp("Off", map[string]string{"UUID": "abc"}),
			native:   nativeResp("On", map[string]string{"uuid": "abc"}),
			wantDiff: []string{"power_state"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compareDescribe(tt.virsh, tt.native)
			assert.ElementsMatch(t, tt.wantDiff, got)
		})
	}
}

// TestMapNativeDomainState verifies the native coarsening matches the virsh path's
// mapLibvirtPowerState: only running is "On"; everything else (incl. blocked, the
// #291 M2 case) is "Off".
func TestMapNativeDomainState(t *testing.T) {
	assert.Equal(t, "On", mapNativeDomainState(golibvirt.DomainRunning))
	for _, s := range []golibvirt.DomainState{
		golibvirt.DomainNostate,
		golibvirt.DomainBlocked,
		golibvirt.DomainPaused,
		golibvirt.DomainShutdown,
		golibvirt.DomainShutoff,
		golibvirt.DomainCrashed,
		golibvirt.DomainPmsuspended,
	} {
		assert.Equal(t, "Off", mapNativeDomainState(s), "state %d should coarsen to Off", s)
	}
}

// shadowCounter reads a shadow metric counter series value from the provider's
// registry, or 0 if absent.
func shadowCounter(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := obsmetrics.GetRegistry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			got := make(map[string]string, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			if mapsEqual(got, labels) {
				return counterOrGauge(m)
			}
		}
	}
	return 0
}

func counterOrGauge(m *dto.Metric) float64 {
	if m.GetCounter() != nil {
		return m.GetCounter().GetValue()
	}
	return m.GetGauge().GetValue()
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// newShadowTestProvider builds a Provider wired for shadow-compare with a scripted
// native describe function — no live libvirtd, no registry.
func newShadowTestProvider(fn func(ctx context.Context, id string) (contracts.DescribeResponse, error)) *Provider {
	cfg, _ := parseNativeConfig("shadow:describe")
	return &Provider{
		nativeCfg:        cfg,
		shadowSampler:    &sampler{n: 1},
		describeNativeFn: fn,
	}
}

// TestRunShadowDescribeMeters verifies the synchronous core meters the right
// compare-run result and per-field divergences.
func TestRunShadowDescribeMeters(t *testing.T) {
	t.Run("equal meters result=equal, no divergence", func(t *testing.T) {
		virsh := virshResp("On", map[string]string{"UUID": "abc", "CPU(s)": "2"})
		native := nativeResp("On", map[string]string{"uuid": "abc", "vcpu": "2"})
		p := newShadowTestProvider(func(context.Context, string) (contracts.DescribeResponse, error) { return native, nil })

		before := shadowCounter(t, "virtrigaud_libvirt_shadow_compare_total", map[string]string{"family": "describe", "result": obsmetrics.ShadowResultEqual})
		p.runShadowDescribe(context.Background(), "vm", virsh)
		after := shadowCounter(t, "virtrigaud_libvirt_shadow_compare_total", map[string]string{"family": "describe", "result": obsmetrics.ShadowResultEqual})
		assert.Equal(t, before+1, after)
	})

	t.Run("divergent meters result=divergent + per-field divergence", func(t *testing.T) {
		virsh := virshResp("On", map[string]string{"CPU(s)": "2"})
		native := nativeResp("Off", map[string]string{"vcpu": "4"})
		p := newShadowTestProvider(func(context.Context, string) (contracts.DescribeResponse, error) { return native, nil })

		divBefore := shadowCounter(t, "virtrigaud_libvirt_shadow_compare_total", map[string]string{"family": "describe", "result": obsmetrics.ShadowResultDivergent})
		psBefore := shadowCounter(t, "virtrigaud_libvirt_shadow_divergence_total", map[string]string{"family": "describe", "field": "power_state"})
		vcpuBefore := shadowCounter(t, "virtrigaud_libvirt_shadow_divergence_total", map[string]string{"family": "describe", "field": "vcpu"})

		p.runShadowDescribe(context.Background(), "vm", virsh)

		assert.Equal(t, divBefore+1, shadowCounter(t, "virtrigaud_libvirt_shadow_compare_total", map[string]string{"family": "describe", "result": obsmetrics.ShadowResultDivergent}))
		assert.Equal(t, psBefore+1, shadowCounter(t, "virtrigaud_libvirt_shadow_divergence_total", map[string]string{"family": "describe", "field": "power_state"}))
		assert.Equal(t, vcpuBefore+1, shadowCounter(t, "virtrigaud_libvirt_shadow_divergence_total", map[string]string{"family": "describe", "field": "vcpu"}))
	})

	t.Run("native error meters result=error, is not propagated", func(t *testing.T) {
		virsh := virshResp("On", map[string]string{"UUID": "abc"})
		p := newShadowTestProvider(func(context.Context, string) (contracts.DescribeResponse, error) {
			return contracts.DescribeResponse{}, errors.New("go-libvirt boom")
		})

		before := shadowCounter(t, "virtrigaud_libvirt_shadow_compare_total", map[string]string{"family": "describe", "result": obsmetrics.ShadowResultError})
		// Must not panic or block; returns nothing.
		p.runShadowDescribe(context.Background(), "vm", virsh)
		after := shadowCounter(t, "virtrigaud_libvirt_shadow_compare_total", map[string]string{"family": "describe", "result": obsmetrics.ShadowResultError})
		assert.Equal(t, before+1, after)
	})
}

// TestMaybeShadowDescribePanicIsolation is the load-bearing safety property: a
// panic in the shadow (go-libvirt) path is recovered, metered, and NOT propagated
// — the caller of Describe is entirely unaffected.
func TestMaybeShadowDescribePanicIsolation(t *testing.T) {
	p := newShadowTestProvider(func(context.Context, string) (contracts.DescribeResponse, error) {
		panic("go-libvirt exploded")
	})

	before := shadowCounter(t, "virtrigaud_libvirt_shadow_compare_total", map[string]string{"family": "describe", "result": obsmetrics.ShadowResultPanic})

	// The caller returns normally (no panic escapes maybeShadowDescribe).
	assert.NotPanics(t, func() {
		p.maybeShadowDescribe(context.Background(), "vm", virshResp("On", map[string]string{"UUID": "abc"}))
		p.shadowWG.Wait()
	})

	after := shadowCounter(t, "virtrigaud_libvirt_shadow_compare_total", map[string]string{"family": "describe", "result": obsmetrics.ShadowResultPanic})
	assert.Equal(t, before+1, after, "panic must be metered")
}

// TestMaybeShadowDescribeOffIsNoOp verifies that with the describe family off (the
// default), the shadow path never runs — no goroutine, no native describe call.
func TestMaybeShadowDescribeOffIsNoOp(t *testing.T) {
	called := false
	cfg, _ := parseNativeConfig("") // empty => describe off
	p := &Provider{
		nativeCfg:     cfg,
		shadowSampler: &sampler{n: 1},
		describeNativeFn: func(context.Context, string) (contracts.DescribeResponse, error) {
			called = true
			return contracts.DescribeResponse{}, nil
		},
	}

	p.maybeShadowDescribe(context.Background(), "vm", virshResp("On", nil))
	p.shadowWG.Wait()
	assert.False(t, called, "shadow must not run when the family is off")
}

// TestMaybeShadowDescribeSamplingSkips verifies the sampling knob gates dispatch:
// with n=2, only every 2nd call shadows.
func TestMaybeShadowDescribeSamplingSkips(t *testing.T) {
	var calls atomic.Int64
	cfg, _ := parseNativeConfig("shadow:describe")
	p := &Provider{
		nativeCfg:     cfg,
		shadowSampler: &sampler{n: 2},
		describeNativeFn: func(context.Context, string) (contracts.DescribeResponse, error) {
			calls.Add(1)
			return nativeResp("On", map[string]string{}), nil
		},
	}

	for i := 0; i < 4; i++ {
		p.maybeShadowDescribe(context.Background(), "vm", virshResp("On", nil))
	}
	p.shadowWG.Wait()
	assert.Equal(t, int64(2), calls.Load(), "n=2 shadows 2 of 4 calls")
}

// ---- ADR-0008 PR 4c: the list family (ListVMs) ----

// listVM builds a contracts.VMInfo the way BOTH virsh's ListVMs and buildNativeList
// populate it — the list comparator sees both sides as the same typed shape (name,
// coarse power state, typed CPU/MemoryMiB, uuid in ProviderRaw), unlike describe
// where the two drivers key ProviderRaw differently.
func listVM(name, power, uuid string, cpu int32, memMiB int64) contracts.VMInfo {
	raw := map[string]string{}
	if uuid != "" {
		raw["uuid"] = uuid
	}
	return contracts.VMInfo{ID: name, Name: name, PowerState: power, CPU: cpu, MemoryMiB: memMiB, ProviderRaw: raw}
}

// TestCompareList covers the list comparator: equal sets, per-field divergence,
// canonicalization (whitespace/case), the "present on only one side is skipped"
// rule, membership (virsh has a VM native misses), the ONE-directional membership
// semantics (a native-extra domain is NOT flagged), and dedup of a field that
// diverges across several VMs.
func TestCompareList(t *testing.T) {
	tests := []struct {
		name     string
		virsh    []contracts.VMInfo
		native   []contracts.VMInfo
		wantDiff []string
	}{
		{
			name:     "identical set -> no divergence",
			virsh:    []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096), listVM("vm2", "Off", "def", 1, 2048)},
			native:   []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096), listVM("vm2", "Off", "def", 1, 2048)},
			wantDiff: nil,
		},
		{
			// Leading-zero normalization does not apply here (CPU/memory are typed
			// ints, not virsh-formatted strings); the string fields still normalize
			// for whitespace and case via canonLower.
			name:     "formatting-only differences canonicalize equal (whitespace, case)",
			virsh:    []contracts.VMInfo{listVM(" vm1 ", " on ", "ABC-DEF", 2, 4096)},
			native:   []contracts.VMInfo{listVM("vm1", "On", "abc-def", 2, 4096)},
			wantDiff: nil,
		},
		{
			name:     "power_state divergence",
			virsh:    []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096)},
			native:   []contracts.VMInfo{listVM("vm1", "Off", "abc", 2, 4096)},
			wantDiff: []string{"power_state"},
		},
		{
			name:     "uuid divergence",
			virsh:    []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096)},
			native:   []contracts.VMInfo{listVM("vm1", "On", "xyz", 2, 4096)},
			wantDiff: []string{"uuid"},
		},
		{
			name:     "vcpu and memory divergence",
			virsh:    []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096)},
			native:   []contracts.VMInfo{listVM("vm1", "On", "abc", 4, 8192)},
			wantDiff: []string{"vcpu", "memory_mib"},
		},
		{
			// virsh defaults a missing vcpu to 1 and an empty uuid is simply absent;
			// the native side here produced neither (cpu 0, memory 0, no uuid), so
			// those fields are skipped, not flagged — the list analogue of describe's
			// "field present on only one side is skipped".
			name:     "fields present on only one side are skipped, not divergent",
			virsh:    []contracts.VMInfo{listVM("vm1", "On", "abc", 1, 4096)},
			native:   []contracts.VMInfo{listVM("vm1", "On", "", 0, 0)},
			wantDiff: nil,
		},
		{
			name:     "membership: virsh has a VM native misses",
			virsh:    []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096), listVM("vm2", "Off", "def", 1, 2048)},
			native:   []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096)},
			wantDiff: []string{membershipField},
		},
		{
			// The one-directional rule: virsh skips domains whose dumpxml/parse fails
			// (#285), so a native-extra domain is expected and benign — never flagged.
			name:     "native-extra domain is NOT flagged",
			virsh:    []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096)},
			native:   []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096), listVM("vm2-extra", "On", "def", 1, 2048)},
			wantDiff: nil,
		},
		{
			name:     "membership is deduped across several missing VMs",
			virsh:    []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096), listVM("vm2", "On", "def", 1, 2048), listVM("vm3", "On", "ghi", 1, 1024)},
			native:   []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096)},
			wantDiff: []string{membershipField},
		},
		{
			name:     "a field diverging on several VMs is reported once",
			virsh:    []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096), listVM("vm2", "On", "def", 1, 2048)},
			native:   []contracts.VMInfo{listVM("vm1", "Off", "abc", 2, 4096), listVM("vm2", "Off", "def", 1, 2048)},
			wantDiff: []string{"power_state"},
		},
		{
			name:     "empty on both sides -> no divergence",
			virsh:    nil,
			native:   nil,
			wantDiff: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compareList(tt.virsh, tt.native)
			assert.ElementsMatch(t, tt.wantDiff, got)
		})
	}
}

// newShadowListTestProvider builds a Provider wired for shadow-compare of the list
// family with a scripted native-list function — no live libvirtd, no registry.
func newShadowListTestProvider(fn func(ctx context.Context) ([]contracts.VMInfo, error)) *Provider {
	cfg, _ := parseNativeConfig("shadow:list")
	return &Provider{
		nativeCfg:     cfg,
		shadowSampler: &sampler{n: 1},
		listNativeFn:  fn,
	}
}

// TestRunShadowListMeters verifies the synchronous core meters the right
// compare-run result and per-field divergences for the list family.
func TestRunShadowListMeters(t *testing.T) {
	t.Run("equal meters result=equal, no divergence", func(t *testing.T) {
		virsh := []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096)}
		native := []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096)}
		p := newShadowListTestProvider(func(context.Context) ([]contracts.VMInfo, error) { return native, nil })

		before := shadowCounter(t, "virtrigaud_libvirt_shadow_compare_total", map[string]string{"family": "list", "result": obsmetrics.ShadowResultEqual})
		p.runShadowList(context.Background(), virsh)
		after := shadowCounter(t, "virtrigaud_libvirt_shadow_compare_total", map[string]string{"family": "list", "result": obsmetrics.ShadowResultEqual})
		assert.Equal(t, before+1, after)
	})

	t.Run("divergent meters result=divergent + per-field divergence", func(t *testing.T) {
		virsh := []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096)}
		native := []contracts.VMInfo{listVM("vm1", "Off", "abc", 4, 4096)}
		p := newShadowListTestProvider(func(context.Context) ([]contracts.VMInfo, error) { return native, nil })

		divBefore := shadowCounter(t, "virtrigaud_libvirt_shadow_compare_total", map[string]string{"family": "list", "result": obsmetrics.ShadowResultDivergent})
		psBefore := shadowCounter(t, "virtrigaud_libvirt_shadow_divergence_total", map[string]string{"family": "list", "field": "power_state"})
		vcpuBefore := shadowCounter(t, "virtrigaud_libvirt_shadow_divergence_total", map[string]string{"family": "list", "field": "vcpu"})

		p.runShadowList(context.Background(), virsh)

		assert.Equal(t, divBefore+1, shadowCounter(t, "virtrigaud_libvirt_shadow_compare_total", map[string]string{"family": "list", "result": obsmetrics.ShadowResultDivergent}))
		assert.Equal(t, psBefore+1, shadowCounter(t, "virtrigaud_libvirt_shadow_divergence_total", map[string]string{"family": "list", "field": "power_state"}))
		assert.Equal(t, vcpuBefore+1, shadowCounter(t, "virtrigaud_libvirt_shadow_divergence_total", map[string]string{"family": "list", "field": "vcpu"}))
	})

	t.Run("membership divergence meters the membership field", func(t *testing.T) {
		virsh := []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096), listVM("vm2", "On", "def", 1, 2048)}
		native := []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096)} // native missed vm2
		p := newShadowListTestProvider(func(context.Context) ([]contracts.VMInfo, error) { return native, nil })

		memBefore := shadowCounter(t, "virtrigaud_libvirt_shadow_divergence_total", map[string]string{"family": "list", "field": membershipField})
		p.runShadowList(context.Background(), virsh)
		memAfter := shadowCounter(t, "virtrigaud_libvirt_shadow_divergence_total", map[string]string{"family": "list", "field": membershipField})
		assert.Equal(t, memBefore+1, memAfter)
	})

	t.Run("native error meters result=error, is not propagated", func(t *testing.T) {
		virsh := []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096)}
		p := newShadowListTestProvider(func(context.Context) ([]contracts.VMInfo, error) {
			return nil, errors.New("go-libvirt list boom")
		})

		before := shadowCounter(t, "virtrigaud_libvirt_shadow_compare_total", map[string]string{"family": "list", "result": obsmetrics.ShadowResultError})
		// Must not panic or block; returns nothing.
		p.runShadowList(context.Background(), virsh)
		after := shadowCounter(t, "virtrigaud_libvirt_shadow_compare_total", map[string]string{"family": "list", "result": obsmetrics.ShadowResultError})
		assert.Equal(t, before+1, after)
	})
}

// TestMaybeShadowListPanicIsolation is the load-bearing safety property for the list
// family: a panic in the shadow (go-libvirt) path is recovered, metered, and NOT
// propagated — the caller of ListVMs is entirely unaffected. It exercises the shared
// runDetachedShadow wrapper for family=list.
func TestMaybeShadowListPanicIsolation(t *testing.T) {
	p := newShadowListTestProvider(func(context.Context) ([]contracts.VMInfo, error) {
		panic("go-libvirt list exploded")
	})

	before := shadowCounter(t, "virtrigaud_libvirt_shadow_compare_total", map[string]string{"family": "list", "result": obsmetrics.ShadowResultPanic})

	assert.NotPanics(t, func() {
		p.maybeShadowList(context.Background(), []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096)})
		p.shadowWG.Wait()
	})

	after := shadowCounter(t, "virtrigaud_libvirt_shadow_compare_total", map[string]string{"family": "list", "result": obsmetrics.ShadowResultPanic})
	assert.Equal(t, before+1, after, "panic must be metered")
}

// TestMaybeShadowListOffIsNoOp verifies that with the list family off (the default),
// the shadow path never runs — no goroutine, no native list call.
func TestMaybeShadowListOffIsNoOp(t *testing.T) {
	called := false
	cfg, _ := parseNativeConfig("") // empty => list off
	p := &Provider{
		nativeCfg:     cfg,
		shadowSampler: &sampler{n: 1},
		listNativeFn: func(context.Context) ([]contracts.VMInfo, error) {
			called = true
			return nil, nil
		},
	}

	p.maybeShadowList(context.Background(), []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096)})
	p.shadowWG.Wait()
	assert.False(t, called, "shadow must not run when the list family is off")
}

// TestMaybeShadowListSamplingSkips verifies the sampling knob gates list dispatch:
// with n=2, only every 2nd call shadows.
func TestMaybeShadowListSamplingSkips(t *testing.T) {
	var calls atomic.Int64
	cfg, _ := parseNativeConfig("shadow:list")
	p := &Provider{
		nativeCfg:     cfg,
		shadowSampler: &sampler{n: 2},
		listNativeFn: func(context.Context) ([]contracts.VMInfo, error) {
			calls.Add(1)
			return []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096)}, nil
		},
	}

	for i := 0; i < 4; i++ {
		p.maybeShadowList(context.Background(), []contracts.VMInfo{listVM("vm1", "On", "abc", 2, 4096)})
	}
	p.shadowWG.Wait()
	assert.Equal(t, int64(2), calls.Load(), "n=2 shadows 2 of 4 calls")
}
