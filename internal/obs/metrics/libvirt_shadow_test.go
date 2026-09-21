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

package metrics

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// counterValue returns the current value of the metric family `name` with exactly
// the given labels, or 0 if no such series exists yet.
func counterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	return sampleValue(t, name, labels, func(m *dto.Metric) float64 { return m.GetCounter().GetValue() })
}

// gaugeValue returns the current value of the gauge family `name` with exactly the
// given labels, or 0 if no such series exists yet.
func gaugeValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	return sampleValue(t, name, labels, func(m *dto.Metric) float64 { return m.GetGauge().GetValue() })
}

func sampleValue(t *testing.T, name string, labels map[string]string, get func(*dto.Metric) float64) float64 {
	t.Helper()
	families, err := GetRegistry().Gather()
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
			if labelsEqual(got, labels) {
				return get(m)
			}
		}
	}
	return 0
}

func labelsEqual(a, b map[string]string) bool {
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

// TestLibvirtShadowMetricsRegistered verifies the three ADR-0008 D4/D5 metrics are
// registered in controller-runtime's Registry (the one served at /metrics), so the
// D5 soak can actually read them — the same "created but not exposed" canary the
// package's other metrics have.
func TestLibvirtShadowMetricsRegistered(t *testing.T) {
	RecordShadowDivergence("describe", "power_state")
	RecordShadowCompare("describe", ShadowResultEqual)
	SetLibvirtActiveDriver("describe", DriverShadow)

	families, err := GetRegistry().Gather()
	require.NoError(t, err)
	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}

	for _, name := range []string{
		"virtrigaud_libvirt_shadow_divergence_total",
		"virtrigaud_libvirt_shadow_compare_total",
		"virtrigaud_libvirt_active_driver",
	} {
		assert.True(t, names[name], "expected metric %q registered in controller-runtime's Registry", name)
	}
}

// TestRecordShadowDivergenceIncrements verifies the divergence counter increments
// per {family,field} — the exact series the D5 flip-gate is measured on.
func TestRecordShadowDivergenceIncrements(t *testing.T) {
	labels := map[string]string{"family": "describe", "field": "memory_mib"}
	before := counterValue(t, "virtrigaud_libvirt_shadow_divergence_total", labels)

	RecordShadowDivergence("describe", "memory_mib")
	RecordShadowDivergence("describe", "memory_mib")

	after := counterValue(t, "virtrigaud_libvirt_shadow_divergence_total", labels)
	assert.Equal(t, before+2, after, "two divergences should add 2 to the {describe,memory_mib} series")
}

// TestRecordShadowCompareIncrements verifies the compare-run counter increments per
// {family,result}, the denominator/liveness signal for the soak.
func TestRecordShadowCompareIncrements(t *testing.T) {
	labels := map[string]string{"family": "describe", "result": ShadowResultDivergent}
	before := counterValue(t, "virtrigaud_libvirt_shadow_compare_total", labels)

	RecordShadowCompare("describe", ShadowResultDivergent)

	after := counterValue(t, "virtrigaud_libvirt_shadow_compare_total", labels)
	assert.Equal(t, before+1, after)
}

// TestSetLibvirtActiveDriverIsExclusive verifies the active-driver gauge sets the
// chosen driver to 1 and every other driver for the same family to 0, so the audit
// signal reports exactly one active driver per family.
func TestSetLibvirtActiveDriverIsExclusive(t *testing.T) {
	SetLibvirtActiveDriver("describe", DriverShadow)

	assert.Equal(t, float64(1), gaugeValue(t, "virtrigaud_libvirt_active_driver", map[string]string{"family": "describe", "driver": DriverShadow}))
	assert.Equal(t, float64(0), gaugeValue(t, "virtrigaud_libvirt_active_driver", map[string]string{"family": "describe", "driver": DriverVirsh}))
	assert.Equal(t, float64(0), gaugeValue(t, "virtrigaud_libvirt_active_driver", map[string]string{"family": "describe", "driver": DriverNative}))

	// Switching drivers must flip the exclusivity, not accumulate.
	SetLibvirtActiveDriver("describe", DriverVirsh)
	assert.Equal(t, float64(1), gaugeValue(t, "virtrigaud_libvirt_active_driver", map[string]string{"family": "describe", "driver": DriverVirsh}))
	assert.Equal(t, float64(0), gaugeValue(t, "virtrigaud_libvirt_active_driver", map[string]string{"family": "describe", "driver": DriverShadow}))
}
