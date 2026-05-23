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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// newProviderMetricsScheme registers only the v1beta1 types needed by the
// Provider reconciler tests. Helper-functions counterSample / labelsMatch
// live in virtualmachine_metrics_test.go (same package).
func newProviderMetricsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, infravirtrigaudiov1beta1.AddToScheme(sch))
	return sch
}

// TestProviderReconcile_Metrics_NotFoundIsSuccessOutcome — reconciling a
// Provider that doesn't exist (IsNotFound) records the reconcile as
// outcome=success. No error to controller-runtime; no work needed.
func TestProviderReconcile_Metrics_NotFoundIsSuccessOutcome(t *testing.T) {
	sch := newProviderMetricsScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	wantLabels := map[string]string{
		"kind":    "Provider",
		"outcome": metrics.OutcomeSuccess,
	}
	before := counterSample(t, "virtrigaud_manager_reconcile_total", wantLabels)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "ghost-provider", Namespace: "default"},
	})

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result, "should return empty result for IsNotFound")

	after := counterSample(t, "virtrigaud_manager_reconcile_total", wantLabels)
	assert.Equal(t, before+1, after,
		"reconcile of non-existent Provider should increment outcome=success counter by 1")
}

// TestProviderReconcile_Metrics_MissingRuntimeIsErrorOutcome — Provider
// without a `spec.runtime` block fails fast with an error and records
// outcome=error + errors_total{reason="runtime-spec-invalid"}. This is
// the documented contract (runtime configuration is required).
func TestProviderReconcile_Metrics_MissingRuntimeIsErrorOutcome(t *testing.T) {
	sch := newProviderMetricsScheme(t)
	prov := &infravirtrigaudiov1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-provider",
			Namespace: "default",
		},
		Spec: infravirtrigaudiov1beta1.ProviderSpec{
			// Spec.Runtime is intentionally nil.
			Type:     "mock",
			Endpoint: "localhost:9090",
		},
	}
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	errorLabels := map[string]string{
		"kind":    "Provider",
		"outcome": metrics.OutcomeError,
	}
	invalidLabels := map[string]string{
		"reason":    errReasonRuntimeSpecInvalid,
		"component": metrics.ComponentManager,
	}
	beforeError := counterSample(t, "virtrigaud_manager_reconcile_total", errorLabels)
	beforeInvalid := counterSample(t, "virtrigaud_errors_total", invalidLabels)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "bad-provider", Namespace: "default"},
	})
	require.Error(t, err, "missing runtime spec should return an error")

	afterError := counterSample(t, "virtrigaud_manager_reconcile_total", errorLabels)
	afterInvalid := counterSample(t, "virtrigaud_errors_total", invalidLabels)

	assert.Equal(t, beforeError+1, afterError,
		"missing-runtime reconcile should increment outcome=error counter")
	assert.Equal(t, beforeInvalid+1, afterInvalid,
		"missing-runtime reconcile should increment errors_total{reason=runtime-spec-invalid}")
}

// TestProviderReconcile_Metrics_DurationHistogramFires — smoke that the
// histogram receives at least one observation under kind=Provider.
func TestProviderReconcile_Metrics_DurationHistogramFires(t *testing.T) {
	sch := newProviderMetricsScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "ghost", Namespace: "default"},
	})
	require.NoError(t, err)

	families, err := metrics.GetRegistry().Gather()
	require.NoError(t, err)

	var sampleCount uint64
	for _, f := range families {
		if f.GetName() != "virtrigaud_manager_reconcile_duration_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelsMatch(m.GetLabel(), map[string]string{"kind": "Provider"}) {
				if h := m.GetHistogram(); h != nil {
					sampleCount += h.GetSampleCount()
				}
			}
		}
	}
	assert.Greater(t, sampleCount, uint64(0),
		"virtrigaud_manager_reconcile_duration_seconds{kind=Provider} should have at least one observation")
}
