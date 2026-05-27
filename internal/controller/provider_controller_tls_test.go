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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// newProviderTLSScheme registers the types needed by the Provider TLS
// reconciler tests: v1beta1 (Provider) + corev1 (Secret/Service) +
// appsv1 (Deployment, for assert-no-Deployment).
func newProviderTLSScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, infravirtrigaudiov1beta1.AddToScheme(sch))
	require.NoError(t, corev1.AddToScheme(sch))
	require.NoError(t, appsv1.AddToScheme(sch))
	return sch
}

// providerWithRuntime builds a baseline Provider CR with a runtime
// block. tlsSpec is attached as spec.runtime.service.tls; pass nil to
// model the loud-failure case.
func providerWithRuntime(name string, tlsSpec *infravirtrigaudiov1beta1.ProviderTLSSpec) *infravirtrigaudiov1beta1.Provider {
	// Always provide a Service (matching default schema behaviour). The
	// TLS-nil case models a Provider whose spec was upgraded from v0.3.6
	// without a tls block — Service may exist or may not; either way
	// the tls-nil branch is what we're exercising.
	service := &infravirtrigaudiov1beta1.ProviderServiceSpec{
		Port: 9443,
		TLS:  tlsSpec,
	}
	return &infravirtrigaudiov1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.ProviderSpec{
			Type:                "mock",
			Endpoint:            "tcp://mock.example.com:9090",
			CredentialSecretRef: infravirtrigaudiov1beta1.ObjectRef{Name: "test-creds"},
			Runtime: &infravirtrigaudiov1beta1.ProviderRuntimeSpec{
				Mode:    infravirtrigaudiov1beta1.RuntimeModeRemote,
				Image:   "virtrigaud/provider-mock:test",
				Service: service,
			},
		},
	}
}

// getConditionByType returns the Condition with the given type from
// the Provider's latest status, or nil if not found.
func getConditionByType(t *testing.T, conds []metav1.Condition, condType string) *metav1.Condition {
	t.Helper()
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

// TestProvider_TLSBlockMissing_LoudFailure — nil tls block must set
// TLSConfigured=False/TLSBlockMissing, must NOT create a Deployment,
// and must not return an error to controller-runtime (controller-
// runtime treats requeue-with-no-error as "try again later").
func TestProvider_TLSBlockMissing_LoudFailure(t *testing.T) {
	sch := newProviderTLSScheme(t)
	prov := providerWithRuntime("tls-missing", nil)
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "tls-missing", Namespace: "default"},
	})
	// The reconcile flow returns an inner ctrl.Result with RequeueAfter
	// set but no error (we don't want CR-level error escalation in this
	// case — the Condition is the operator-facing signal).
	require.NoError(t, err)
	assert.NotZero(t, res.RequeueAfter, "loud-failure case should requeue, not return immediately")

	// Re-fetch the Provider; assert Condition is set.
	latest := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: "tls-missing", Namespace: "default"}, latest))

	c := getConditionByType(t, latest.Status.Conditions, providerConditionTLSConfigured)
	require.NotNil(t, c, "TLSConfigured Condition must be set when tls block is missing")
	assert.Equal(t, metav1.ConditionFalse, c.Status, "TLSConfigured must be False")
	assert.Equal(t, providerReasonTLSBlockMissing, c.Reason, "Reason must be TLSBlockMissing")
	assert.Contains(t, c.Message, "v0.3.7",
		"Message should reference v0.3.7 so the operator knows the upgrade context")

	// No Deployment must have been created.
	depList := &appsv1.DeploymentList{}
	require.NoError(t, cli.List(context.Background(), depList))
	assert.Empty(t, depList.Items, "no Deployment should be created when TLS posture is undecided")
}

// TestProvider_TLSEnabledFalse_ProceedsWithoutVolume — explicit
// plaintext opt-out. Provider reconciles, Deployment is created, and
// the pod template carries NO `provider-tls` volume / volume mount.
func TestProvider_TLSEnabledFalse_ProceedsWithoutVolume(t *testing.T) {
	sch := newProviderTLSScheme(t)
	prov := providerWithRuntime("tls-disabled",
		&infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false})
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "tls-disabled", Namespace: "default"},
	})
	require.NoError(t, err)

	// Deployment should exist.
	dep := &appsv1.Deployment{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{
			Name:      "virtrigaud-provider-default-tls-disabled",
			Namespace: "default",
		}, dep))

	// Volume named providerTLSVolumeName should NOT be present.
	for _, v := range dep.Spec.Template.Spec.Volumes {
		assert.NotEqual(t, providerTLSVolumeName, v.Name,
			"plaintext-opt-out Deployment must NOT include the provider-tls volume")
	}
	// Same for the mount on the container.
	require.NotEmpty(t, dep.Spec.Template.Spec.Containers)
	for _, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
		assert.NotEqual(t, providerTLSVolumeName, m.Name,
			"plaintext-opt-out container must NOT mount the provider-tls volume")
	}

	// Condition surfaces the opt-out.
	latest := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: "tls-disabled", Namespace: "default"}, latest))
	c := getConditionByType(t, latest.Status.Conditions, providerConditionTLSConfigured)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, providerReasonTLSDisabled, c.Reason)
}

// TestProvider_TLSEnabledTrue_DeploymentHasVolume — enabled=true with
// a referenced Secret. Deployment created with the provider-tls volume
// mounted at providerTLSMountPath (/etc/virtrigaud/tls).
//
// Note: this test does NOT need the Secret to actually exist in the
// fake client — the reconciler builds the Deployment from the spec's
// SecretRef.Name; consumption of the Secret happens at gRPC dial time
// inside Resolver.buildTLSConfig (covered by resolver_test.go).
func TestProvider_TLSEnabledTrue_DeploymentHasVolume(t *testing.T) {
	sch := newProviderTLSScheme(t)
	prov := providerWithRuntime("tls-enabled",
		&infravirtrigaudiov1beta1.ProviderTLSSpec{
			Enabled:   true,
			SecretRef: &corev1.LocalObjectReference{Name: "provider-tls-secret"},
		})
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "tls-enabled", Namespace: "default"},
	})
	require.NoError(t, err)

	// Deployment exists.
	dep := &appsv1.Deployment{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{
			Name:      "virtrigaud-provider-default-tls-enabled",
			Namespace: "default",
		}, dep))

	// Find the provider-tls volume.
	var found *corev1.Volume
	for i := range dep.Spec.Template.Spec.Volumes {
		if dep.Spec.Template.Spec.Volumes[i].Name == providerTLSVolumeName {
			found = &dep.Spec.Template.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, found, "Deployment must carry the provider-tls volume when tls.enabled=true")
	require.NotNil(t, found.Secret, "provider-tls volume must be a Secret volume")
	assert.Equal(t, "provider-tls-secret", found.Secret.SecretName,
		"Secret name on the volume must come from spec.runtime.service.tls.secretRef")

	// Find the VolumeMount on the container.
	require.NotEmpty(t, dep.Spec.Template.Spec.Containers)
	var mount *corev1.VolumeMount
	for i := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
		if dep.Spec.Template.Spec.Containers[0].VolumeMounts[i].Name == providerTLSVolumeName {
			mount = &dep.Spec.Template.Spec.Containers[0].VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, mount, "container must mount the provider-tls volume")
	assert.Equal(t, providerTLSMountPath, mount.MountPath,
		"mount path must be the canonical /etc/virtrigaud/tls")
	assert.True(t, mount.ReadOnly, "TLS material must be mounted read-only")

	// TLS_ENABLED env var on the container must be "true".
	var tlsEnabledEnv string
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "TLS_ENABLED" {
			tlsEnabledEnv = e.Value
			break
		}
	}
	assert.Equal(t, "true", tlsEnabledEnv,
		"TLS_ENABLED env var must reflect spec.runtime.service.tls.enabled")

	// Condition surfaces the enabled state.
	latest := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: "tls-enabled", Namespace: "default"}, latest))
	c := getConditionByType(t, latest.Status.Conditions, providerConditionTLSConfigured)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status, "TLSConfigured should be True when enabled with secretRef")
	assert.Equal(t, providerReasonTLSEnabled, c.Reason)
}

// TestProvider_TLSEnabledTrue_NoSecretRef_SurfacesCondition — enabled=true
// but secretRef is empty. Controller refuses to deploy and surfaces the
// SecretRefMissing reason on the Condition.
func TestProvider_TLSEnabledTrue_NoSecretRef_SurfacesCondition(t *testing.T) {
	sch := newProviderTLSScheme(t)
	prov := providerWithRuntime("tls-no-secret",
		&infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: true})
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "tls-no-secret", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.NotZero(t, res.RequeueAfter, "should requeue while waiting for operator action")

	latest := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: "tls-no-secret", Namespace: "default"}, latest))
	c := getConditionByType(t, latest.Status.Conditions, providerConditionTLSConfigured)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, providerReasonTLSSecretRefEmpty, c.Reason)

	// No Deployment should be created.
	dep := &appsv1.Deployment{}
	getErr := cli.Get(context.Background(),
		types.NamespacedName{
			Name:      "virtrigaud-provider-default-tls-no-secret",
			Namespace: "default",
		}, dep)
	require.Error(t, getErr)
	assert.True(t, apierrors.IsNotFound(getErr),
		"no Deployment should exist when secretRef is missing")
}
