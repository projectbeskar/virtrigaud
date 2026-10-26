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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// A Provider the Provider controller has not reconciled yet has no
// status.runtime. Reconciling a VM against it used to dereference the nil
// pointer in a log line and panic the reconcile.
func TestReconcileVM_ProviderWithoutRuntimeStatusDoesNotPanic(t *testing.T) {
	for name, resolver := range map[string]*stubResolver{
		"resolve fails":    {err: errors.New("remote provider runtime not configured")},
		"resolve succeeds": {provider: &rejectingCreateProvider{}},
	} {
		t.Run(name, func(t *testing.T) {
			const ns = "team-rt"
			prov, class := providerAndClass(ns)
			prov.Status.Runtime = nil
			vm := creatableVM(ns)
			r := newTestReconciler(coverageTestScheme(t), resolver, prov, class, vm)

			var (
				res struct{ after time.Duration }
				err error
			)
			require.NotPanics(t, func() {
				out, rerr := r.reconcileVM(context.Background(), vm)
				res.after, err = out.RequeueAfter, rerr
			})
			require.NoError(t, err)
			assert.Positive(t, res.after)
			if resolver.err != nil {
				ready := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
				require.NotNil(t, ready)
				assert.Equal(t, k8s.ReasonProviderError, ready.Reason)
				assert.Equal(t, providerResolveFailedMessage, ready.Message)
			}
		})
	}
}

func TestProviderRuntimeState(t *testing.T) {
	phase, endpoint := providerRuntimeState(nil)
	assert.Empty(t, phase)
	assert.Empty(t, endpoint)
	phase, endpoint = providerRuntimeState(&infravirtrigaudiov1beta1.Provider{})
	assert.Empty(t, phase)
	assert.Empty(t, endpoint)
	phase, endpoint = providerRuntimeState(&infravirtrigaudiov1beta1.Provider{Status: infravirtrigaudiov1beta1.ProviderStatus{
		Runtime: &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"},
	}})
	assert.Equal(t, "Running", phase)
	assert.Equal(t, "provider:9443", endpoint)
}
