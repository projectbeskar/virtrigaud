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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// devGateProvider is a minimal clustered Provider of type pt.
func devGateProvider(pt infravirtrigaudiov1beta1.ProviderType) *infravirtrigaudiov1beta1.Provider {
	return &infravirtrigaudiov1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", Generation: 3},
		Spec: infravirtrigaudiov1beta1.ProviderSpec{
			Type:     pt,
			Topology: infravirtrigaudiov1beta1.ProviderTopologyCluster,
		},
		Status: infravirtrigaudiov1beta1.ProviderStatus{Healthy: true},
	}
}

// A cloudhypervisor Provider whose type the gate does not enable (always the
// case in a release build) is reported not ready, and its runtime is not
// reconciled: reconcileRuntimeStatus returns before the runtime is touched
// (it would otherwise fail on the missing spec.runtime).
func TestProviderController_DisabledDevProviderTypeIsNotRun(t *testing.T) {
	r := &ProviderReconciler{}
	provider := devGateProvider(infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor)

	result, deployment, err := r.reconcileRuntimeStatus(context.Background(), provider)
	if err != nil || deployment != nil || result.RequeueAfter != 0 {
		t.Fatalf("expected a quiet refusal, got result=%+v deployment=%v err=%v", result, deployment, err)
	}
	if provider.Status.Healthy {
		t.Error("a refused Provider must not be Healthy")
	}
	for _, condType := range []string{"ProviderRuntimeReady", providerConditionAvailable} {
		cond := k8s.GetCondition(provider.Status.Conditions, condType)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonDevProviderTypeNotEnabled ||
			cond.ObservedGeneration != provider.Generation {
			t.Errorf("%s = %+v; want False/%s at generation %d", condType, cond, reasonDevProviderTypeNotEnabled, provider.Generation)
		}
	}
}

// With the gate enabling the type, and for every other type, the refusal
// does not apply.
func TestProviderController_DevGateLetsEnabledAndOtherTypesThrough(t *testing.T) {
	gated := &ProviderReconciler{EnabledDevProviderTypes: map[infravirtrigaudiov1beta1.ProviderType]struct{}{
		infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor: {},
	}}
	if gated.refuseDisabledDevProviderType(devGateProvider(infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor)) {
		t.Error("an enabled development-only type must not be refused")
	}
	plain := &ProviderReconciler{}
	if plain.refuseDisabledDevProviderType(devGateProvider(infravirtrigaudiov1beta1.ProviderTypeLibvirt)) {
		t.Error("libvirt must never be refused by the development gate")
	}
}
