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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// histogramSampleCount returns the cumulative sample count for the
// given histogram metric family matching the supplied labels. Returns
// 0 when no matching sample exists (so before/after subtraction yields
// the increment, matching the counterSample helper pattern in this
// package).
func histogramSampleCount(t *testing.T, family string, want map[string]string) uint64 {
	t.Helper()
	families, err := metrics.GetRegistry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != family {
			continue
		}
		var total uint64
		for _, m := range f.GetMetric() {
			if !labelsMatch(m.GetLabel(), want) {
				continue
			}
			if h := m.GetHistogram(); h != nil {
				total += h.GetSampleCount()
			}
		}
		return total
	}
	return 0
}

// histogramSampleSum returns the cumulative observed-value sum across
// all samples matching the supplied labels. Used to assert that the
// recorded duration was non-zero (and roughly within the expected
// range) without flaky time-based equality.
func histogramSampleSum(t *testing.T, family string, want map[string]string) float64 {
	t.Helper()
	families, err := metrics.GetRegistry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != family {
			continue
		}
		var total float64
		for _, m := range f.GetMetric() {
			if !labelsMatch(m.GetLabel(), want) {
				continue
			}
			if h := m.GetHistogram(); h != nil {
				total += h.GetSampleSum()
			}
		}
		return total
	}
	return 0
}

// _ keeps the dto import live for histogram callers above (the helper
// uses h.GetSampleCount which is on *dto.Histogram).
var _ = (*dto.Histogram)(nil)

// TestRecordIPDiscoveryIfFirstSeen_NoIPsToNoIPs pins gate path 1:
// VM had no IPs before AND no IPs in this Describe → MUST NOT record.
// This is the common "still waiting for DHCP" case.
//
// G7.2 / #127.
func TestRecordIPDiscoveryIfFirstSeen_NoIPsToNoIPs(t *testing.T) {
	const providerType = "g72-no-to-no"
	labels := map[string]string{"provider_type": providerType}

	beforeCount := histogramSampleCount(t, "virtrigaud_ip_discovery_duration_seconds", labels)
	beforeSum := histogramSampleSum(t, "virtrigaud_ip_discovery_duration_seconds", labels)

	creationTime := metav1.NewTime(time.Now().Add(-30 * time.Second))
	recordIPDiscoveryIfFirstSeen(nil, nil, creationTime, providerType)
	recordIPDiscoveryIfFirstSeen([]string{}, []string{}, creationTime, providerType)

	afterCount := histogramSampleCount(t, "virtrigaud_ip_discovery_duration_seconds", labels)
	afterSum := histogramSampleSum(t, "virtrigaud_ip_discovery_duration_seconds", labels)

	assert.Equal(t, beforeCount, afterCount,
		"sample count must not change when neither side has IPs")
	assert.Equal(t, beforeSum, afterSum,
		"sample sum must not change when neither side has IPs")
}

// TestRecordIPDiscoveryIfFirstSeen_FirstIPDiscovered pins gate path 2
// (the load-bearing success path): VM had no IPs before AND has IPs
// now → MUST record exactly one sample with the correct
// provider_type label, and the observed duration must be positive
// (≈ time.Since(creationTime)).
//
// G7.2 / #127.
func TestRecordIPDiscoveryIfFirstSeen_FirstIPDiscovered(t *testing.T) {
	const providerType = "g72-first-ip"
	labels := map[string]string{"provider_type": providerType}

	beforeCount := histogramSampleCount(t, "virtrigaud_ip_discovery_duration_seconds", labels)
	beforeSum := histogramSampleSum(t, "virtrigaud_ip_discovery_duration_seconds", labels)

	creationTime := metav1.NewTime(time.Now().Add(-5 * time.Second))
	recordIPDiscoveryIfFirstSeen(nil, []string{"10.0.0.42"}, creationTime, providerType)

	afterCount := histogramSampleCount(t, "virtrigaud_ip_discovery_duration_seconds", labels)
	afterSum := histogramSampleSum(t, "virtrigaud_ip_discovery_duration_seconds", labels)

	assert.Equal(t, beforeCount+1, afterCount,
		"first-IP transition must increment the histogram sample count by exactly 1")

	delta := afterSum - beforeSum
	// Expect roughly 5 seconds; allow a generous window to absorb scheduling
	// jitter on slow CI machines without flakiness.
	assert.Greater(t, delta, 4.0,
		"observed duration must be at least the elapsed wall-clock time since creationTime (got %.3fs)", delta)
	assert.Less(t, delta, 60.0,
		"observed duration must not be wildly large (got %.3fs)", delta)
}

// TestRecordIPDiscoveryIfFirstSeen_AlreadyHadIPs pins gate path 3
// (the idempotency-after-restart contract): VM had IPs before → MUST
// NOT re-record, even though the Describe still returns IPs. Catches
// the regression class where a manager restart re-fires the metric for
// every existing VM at reconcile-resume time.
//
// G7.2 / #127.
func TestRecordIPDiscoveryIfFirstSeen_AlreadyHadIPs(t *testing.T) {
	const providerType = "g72-idempotent"
	labels := map[string]string{"provider_type": providerType}

	beforeCount := histogramSampleCount(t, "virtrigaud_ip_discovery_duration_seconds", labels)

	creationTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	// VM has been observed with IPs in a prior reconcile (or after a
	// restart, persisted in etcd). Describe still returns IPs.
	recordIPDiscoveryIfFirstSeen([]string{"10.0.0.42"}, []string{"10.0.0.42", "fe80::1"}, creationTime, providerType)
	recordIPDiscoveryIfFirstSeen([]string{"10.0.0.42"}, []string{"10.0.0.42"}, creationTime, providerType)

	afterCount := histogramSampleCount(t, "virtrigaud_ip_discovery_duration_seconds", labels)

	assert.Equal(t, beforeCount, afterCount,
		"sample count must NOT increase when VM already had IPs (idempotency across reconciles + restarts)")
}

// TestRecordIPDiscoveryIfFirstSeen_ZeroCreationTimeIsSkipped pins gate
// path 4 (defensive): CreationTimestamp zero → skip. CRs fetched via
// the API server always have a non-zero CreationTimestamp, but this
// defensive branch prevents the helper from emitting nonsensical
// durations like time.Since(epoch) = decades.
//
// G7.2 / #127.
func TestRecordIPDiscoveryIfFirstSeen_ZeroCreationTimeIsSkipped(t *testing.T) {
	const providerType = "g72-zero-time"
	labels := map[string]string{"provider_type": providerType}

	beforeCount := histogramSampleCount(t, "virtrigaud_ip_discovery_duration_seconds", labels)

	var zeroTime metav1.Time // explicitly zero
	require.True(t, zeroTime.IsZero(), "test precondition: zero-value metav1.Time must IsZero()")
	recordIPDiscoveryIfFirstSeen(nil, []string{"10.0.0.42"}, zeroTime, providerType)

	afterCount := histogramSampleCount(t, "virtrigaud_ip_discovery_duration_seconds", labels)

	assert.Equal(t, beforeCount, afterCount,
		"sample count must NOT increase when CreationTimestamp is zero")
}
