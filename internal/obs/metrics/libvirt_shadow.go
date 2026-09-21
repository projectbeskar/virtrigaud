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

import "github.com/prometheus/client_golang/prometheus"

// This file adds the ADR-0008 D4/D5 libvirt-driver-transition metrics. They are
// emitted by the libvirt PROVIDER process (where go-libvirt runs), and registered
// — like every other virtrigaud_* metric — with controller-runtime's Registry
// (see metrics.go's registerer). The provider serves that registry at /metrics via
// the SDK server's MetricsHandler, which is what lets the D5 production soak read
// the divergence signal.
var (
	// libvirtShadowDivergenceTotal is THE ADR-0008 D5 flip-gate metric. Each
	// increment is one SEMANTIC divergence (whitespace/field-order/formatting
	// excluded by the canonicalizing comparator) between the virsh
	// (authoritative) and go-libvirt (shadow) drivers for one field of one read
	// operation. PR 5 may not flip a read family to native until this reads 0
	// for that family across the full D5 window (>=14 days in prod, >=2 libvirtd
	// restarts + >=1 host reboot, per-VM-shape checklist complete).
	libvirtShadowDivergenceTotal = registerer.NewCounterVec(
		prometheus.CounterOpts{
			Name: "virtrigaud_libvirt_shadow_divergence_total",
			Help: "Total semantic divergences between the virsh (authoritative) and go-libvirt (shadow) libvirt drivers, by operation family and field. The ADR-0008 D5 flip-gate metric; PR 5 requires this at 0 across the soak window before flipping a read family to native.",
		},
		[]string{"family", "field"},
	)

	// libvirtShadowCompareTotal counts every shadow-compare RUN by result. It is
	// the denominator for the divergence rate and, just as importantly, the proof
	// that the harness is actually running: a divergence counter that stays 0
	// cannot by itself distinguish "0 divergences because all-equal" from "0
	// divergences because the shadow path never executed". A D5 dashboard reads
	// both series together.
	//
	// result is one of ShadowResult{Equal,Divergent,Error,Panic}. Error and Panic
	// are shadow-path failures that were recovered, metered, and NOT propagated to
	// the caller (the caller always received virsh's answer).
	libvirtShadowCompareTotal = registerer.NewCounterVec(
		prometheus.CounterOpts{
			Name: "virtrigaud_libvirt_shadow_compare_total",
			Help: "Total libvirt shadow-compare read runs by family and result (equal|divergent|error|panic). Denominator for the divergence rate and proof the shadow harness is running (ADR-0008 D5).",
		},
		[]string{"family", "result"},
	)

	// libvirtActiveDriver is the ADR-0008 D4 operator-visible audit signal: the
	// EFFECTIVE driver per operation family, exposed as a metric rather than a CRD
	// field so the transient transition flag never pollutes the stable v1beta1 API.
	// One series per (family, driver) is set to 1 for the effective driver and 0
	// for the others. The effective driver is derived from VIRTRIGAUD_LIBVIRT_NATIVE
	// after any build-gated downgrade (in PR 4b, a family configured native runs as
	// shadow, so this reads shadow — the metric reports what is actually happening,
	// not merely what was requested).
	libvirtActiveDriver = registerer.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "virtrigaud_libvirt_active_driver",
			Help: "Effective libvirt driver per operation family: 1 for the active driver (virsh|shadow|native), 0 otherwise. ADR-0008 D4 audit signal (read-only, non-API-committing).",
		},
		[]string{"family", "driver"},
	)
)

// Shadow-compare run results (the "result" label of
// virtrigaud_libvirt_shadow_compare_total).
const (
	// ShadowResultEqual means the canonicalizing comparator found no semantic
	// divergence between the two drivers' answers.
	ShadowResultEqual = "equal"
	// ShadowResultDivergent means at least one field diverged semantically; each
	// diverging field also increments virtrigaud_libvirt_shadow_divergence_total.
	ShadowResultDivergent = "divergent"
	// ShadowResultError means the shadow (go-libvirt) path returned an error that
	// was recovered, metered, and not propagated — the caller got virsh's answer.
	ShadowResultError = "error"
	// ShadowResultPanic means the shadow path panicked; the panic was recovered,
	// metered, and not propagated — the caller got virsh's answer.
	ShadowResultPanic = "panic"
)

// Effective libvirt drivers (the "driver" label of
// virtrigaud_libvirt_active_driver).
const (
	// DriverVirsh is the authoritative subprocess driver: virsh over SSH.
	DriverVirsh = "virsh"
	// DriverShadow means both drivers run but virsh's answer is returned
	// (go-libvirt is observed and metered only) — the ADR-0008 PR 4b read mode.
	DriverShadow = "shadow"
	// DriverNative means go-libvirt's answer is returned (ADR-0008 PR 5+); not
	// reachable for reads in PR 4b.
	DriverNative = "native"
)

// RecordShadowDivergence increments virtrigaud_libvirt_shadow_divergence_total for
// one diverging field of a shadowed read family (ADR-0008 D5).
func RecordShadowDivergence(family, field string) {
	libvirtShadowDivergenceTotal.WithLabelValues(family, field).Inc()
}

// RecordShadowCompare increments virtrigaud_libvirt_shadow_compare_total for one
// shadow-compare run. result must be one of the ShadowResult* constants.
func RecordShadowCompare(family, result string) {
	libvirtShadowCompareTotal.WithLabelValues(family, result).Inc()
}

// SetLibvirtActiveDriver publishes the effective driver for family on
// virtrigaud_libvirt_active_driver: the given driver's series is set to 1 and the
// other known drivers' series for the same family to 0, so the family always has
// exactly one active series. driver should be one of the Driver* constants.
func SetLibvirtActiveDriver(family, driver string) {
	for _, d := range []string{DriverVirsh, DriverShadow, DriverNative} {
		value := 0.0
		if d == driver {
			value = 1.0
		}
		libvirtActiveDriver.WithLabelValues(family, d).Set(value)
	}
}
