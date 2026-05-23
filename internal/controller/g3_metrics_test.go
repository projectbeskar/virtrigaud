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

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// newG3MetricsScheme registers v1beta1 types needed by the G3 reconciler
// tests. Helpers (counterSample, labelsMatch) come from
// virtualmachine_metrics_test.go in the same package.
func newG3MetricsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, infravirtrigaudiov1beta1.AddToScheme(sch))
	return sch
}

// TestVMMigrationReconcile_NoDoubleCountOnError is the regression canary
// for issue #105. The pre-fix implementation used:
//
//	defer timer.Finish(metrics.OutcomeSuccess)  // arg captured immediately
//
// + explicit `timer.Finish(metrics.OutcomeError)` mid-function. Errored
// reconciles recorded TWO samples (one error + one deferred success).
//
// This test forces an error path (VMMigration not in cache + Get failure
// simulated by passing a request for an object that doesn't exist with
// no client error — fake.Client returns nil for IsNotFound which is the
// success-early-return path; for a true error we'd need a custom client
// wrapper). Instead, we exercise the success-early-return (IsNotFound)
// path which the fix also touches: it must record exactly ONE sample.
// The "exactly one" assertion catches any future refactor that
// re-introduces double-counting.
func TestVMMigrationReconcile_NoDoubleCountOnSuccessfulNotFound(t *testing.T) {
	sch := newG3MetricsScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &VMMigrationReconciler{Client: cli, Scheme: sch}

	successLabels := map[string]string{
		"kind":    "VMMigration",
		"outcome": metrics.OutcomeSuccess,
	}
	before := counterSample(t, "virtrigaud_manager_reconcile_total", successLabels)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "ghost-migration", Namespace: "default"},
	})
	require.NoError(t, err)

	after := counterSample(t, "virtrigaud_manager_reconcile_total", successLabels)
	assert.Equal(t, before+1, after,
		"VMMigration reconcile of non-existent CR should record EXACTLY ONE sample (no double-count). Pre-#105 fix this would record 2.")
}

// TestVMSnapshotReconcile_NoDoubleCountOnSuccessfulNotFound — same shape
// as the VMMigration test above. Issue #105 affected both files.
func TestVMSnapshotReconcile_NoDoubleCountOnSuccessfulNotFound(t *testing.T) {
	sch := newG3MetricsScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &VMSnapshotReconciler{Client: cli, Scheme: sch}

	successLabels := map[string]string{
		"kind":    "VMSnapshot",
		"outcome": metrics.OutcomeSuccess,
	}
	before := counterSample(t, "virtrigaud_manager_reconcile_total", successLabels)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "ghost-snapshot", Namespace: "default"},
	})
	require.NoError(t, err)

	after := counterSample(t, "virtrigaud_manager_reconcile_total", successLabels)
	assert.Equal(t, before+1, after,
		"VMSnapshot reconcile of non-existent CR should record EXACTLY ONE sample (no double-count). Pre-#105 fix this would record 2.")
}

// TestG3_StubReconcilersEmitTimer asserts that the 3 stub reconcilers
// (VMClass, VMImage, VMNetworkAttachment) all wire the timer correctly
// and emit outcome=success on their no-op reconcile path.
func TestG3_StubReconcilersEmitTimer(t *testing.T) {
	tests := []struct {
		name string
		kind string
		mk   func(cli *fake.ClientBuilder, sch *runtime.Scheme) reconcileFn
	}{
		{
			name: "VMClass",
			kind: "VMClass",
			mk: func(cli *fake.ClientBuilder, sch *runtime.Scheme) reconcileFn {
				r := &VMClassReconciler{Client: cli.Build(), Scheme: sch}
				return r.Reconcile
			},
		},
		{
			name: "VMImage",
			kind: "VMImage",
			mk: func(cli *fake.ClientBuilder, sch *runtime.Scheme) reconcileFn {
				r := &VMImageReconciler{Client: cli.Build(), Scheme: sch}
				return r.Reconcile
			},
		},
		{
			name: "VMNetworkAttachment",
			kind: "VMNetworkAttachment",
			mk: func(cli *fake.ClientBuilder, sch *runtime.Scheme) reconcileFn {
				r := &VMNetworkAttachmentReconciler{Client: cli.Build(), Scheme: sch}
				return r.Reconcile
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sch := newG3MetricsScheme(t)
			cli := fake.NewClientBuilder().WithScheme(sch)
			reconcile := tt.mk(cli, sch)

			labels := map[string]string{
				"kind":    tt.kind,
				"outcome": metrics.OutcomeSuccess,
			}
			before := counterSample(t, "virtrigaud_manager_reconcile_total", labels)

			_, err := reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "anything", Namespace: "default"},
			})
			require.NoError(t, err)

			after := counterSample(t, "virtrigaud_manager_reconcile_total", labels)
			assert.Equal(t, before+1, after,
				"%s stub reconciler should emit exactly one outcome=success sample per call", tt.kind)
		})
	}
}

// TestVMAdoptionReconcile_NotFoundIsSuccessOutcome — VMAdoption follows
// the same shape as G1/G2: IsNotFound → outcome=success.
func TestVMAdoptionReconcile_NotFoundIsSuccessOutcome(t *testing.T) {
	sch := newG3MetricsScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &VMAdoptionReconciler{Client: cli, Scheme: sch}

	successLabels := map[string]string{
		"kind":    "VMAdoption",
		"outcome": metrics.OutcomeSuccess,
	}
	before := counterSample(t, "virtrigaud_manager_reconcile_total", successLabels)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "ghost-provider", Namespace: "default"},
	})
	require.NoError(t, err)

	after := counterSample(t, "virtrigaud_manager_reconcile_total", successLabels)
	assert.Equal(t, before+1, after,
		"VMAdoption reconcile of non-existent Provider should increment outcome=success counter by 1")
}

// reconcileFn is a tiny adapter so the stub-table test above can hold
// reconcile methods of different concrete reconciler types.
type reconcileFn func(context.Context, ctrl.Request) (ctrl.Result, error)
