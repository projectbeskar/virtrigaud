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
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// newProviderSAScheme registers the types needed by the provider
// ServiceAccount-hardening tests: v1beta1 (Provider) + corev1
// (ServiceAccount/Service) + appsv1 (Deployment) + rbacv1 (RoleBinding/
// ClusterRoleBinding, so the "no bindings" assertion can List them).
func newProviderSAScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, infravirtrigaudiov1beta1.AddToScheme(sch))
	require.NoError(t, corev1.AddToScheme(sch))
	require.NoError(t, appsv1.AddToScheme(sch))
	require.NoError(t, rbacv1.AddToScheme(sch))
	return sch
}

// TestProvider_DedicatedServiceAccount_NoTokenNoBindings is the security
// regression guard for the provider-pod identity fix. It proves that a
// reconciled remote provider:
//
//  1. runs its pods under a DEDICATED ServiceAccount (not empty, not the
//     namespace "default", not the manager's SA);
//  2. sets AutomountServiceAccountToken=false on the pod spec, so no
//     Kubernetes API token is projected into the provider pod;
//  3. creates that ServiceAccount as an owner-referenced child object that
//     also pins AutomountServiceAccountToken=false; and
//  4. grants the ServiceAccount NO RBAC — there is no RoleBinding or
//     ClusterRoleBinding anywhere, so a compromised provider pod cannot use
//     its identity to read cluster Secrets.
func TestProvider_DedicatedServiceAccount_NoTokenNoBindings(t *testing.T) {
	sch := newProviderSAScheme(t)
	// tls.enabled=false so the reconcile proceeds all the way to creating the
	// Deployment (the loud-failure nil-TLS path would stop before that).
	prov := providerWithRuntime("sa-hardening",
		&infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false})
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "sa-hardening", Namespace: "default"},
	})
	require.NoError(t, err)

	const wantSAName = "virtrigaud-provider-default-sa-hardening"

	// --- Deployment pod spec ---------------------------------------------
	dep := &appsv1.Deployment{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: wantSAName, Namespace: "default"}, dep))

	podSpec := dep.Spec.Template.Spec
	assert.Equal(t, wantSAName, podSpec.ServiceAccountName,
		"provider pod must run under its own dedicated ServiceAccount")
	assert.NotEqual(t, "default", podSpec.ServiceAccountName,
		"provider pod must NOT run under the namespace default ServiceAccount")

	require.NotNil(t, podSpec.AutomountServiceAccountToken,
		"AutomountServiceAccountToken must be set explicitly, not left nil (nil defaults to true)")
	assert.False(t, *podSpec.AutomountServiceAccountToken,
		"provider pod must NOT have a Kubernetes API token projected into it")

	// --- The ServiceAccount object itself --------------------------------
	sa := &corev1.ServiceAccount{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: wantSAName, Namespace: "default"}, sa),
		"a dedicated ServiceAccount must be created for the provider")

	// Owner-referenced to the Provider CR so it is garbage-collected with it.
	owner := metav1.GetControllerOf(sa)
	require.NotNil(t, owner, "the ServiceAccount must be owner-referenced to its Provider")
	assert.Equal(t, "Provider", owner.Kind)
	assert.Equal(t, "sa-hardening", owner.Name)

	// Defence-in-depth: automount is also pinned false on the SA object.
	require.NotNil(t, sa.AutomountServiceAccountToken,
		"the ServiceAccount should pin AutomountServiceAccountToken explicitly")
	assert.False(t, *sa.AutomountServiceAccountToken,
		"the ServiceAccount must default its pods to no projected token")

	// --- No RBAC granted to the identity ---------------------------------
	// The whole point of the fix: this identity must be powerless. The
	// controller creates neither a RoleBinding nor a ClusterRoleBinding, so
	// the cluster contains none referencing the provider SA.
	rbList := &rbacv1.RoleBindingList{}
	require.NoError(t, cli.List(context.Background(), rbList))
	for _, rb := range rbList.Items {
		for _, s := range rb.Subjects {
			assert.NotEqualf(t, wantSAName, s.Name,
				"provider ServiceAccount must have NO RoleBinding (found %q)", rb.Name)
		}
	}

	crbList := &rbacv1.ClusterRoleBindingList{}
	require.NoError(t, cli.List(context.Background(), crbList))
	for _, crb := range crbList.Items {
		for _, s := range crb.Subjects {
			assert.NotEqualf(t, wantSAName, s.Name,
				"provider ServiceAccount must have NO ClusterRoleBinding (found %q)", crb.Name)
		}
	}
}

// TestProvider_ServiceAccountDeletedOnCleanup proves the finalizer/deletion
// path removes the dedicated ServiceAccount, so the identity does not outlive
// the Provider it belonged to.
func TestProvider_ServiceAccountDeletedOnCleanup(t *testing.T) {
	sch := newProviderSAScheme(t)
	prov := providerWithRuntime("sa-cleanup",
		&infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false})
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	// First reconcile creates the child resources including the SA.
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "sa-cleanup", Namespace: "default"},
	})
	require.NoError(t, err)

	const wantSAName = "virtrigaud-provider-default-sa-cleanup"
	sa := &corev1.ServiceAccount{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: wantSAName, Namespace: "default"}, sa))

	// Run the deletion cleanup and confirm the SA is gone.
	require.NoError(t, r.cleanupRemoteRuntime(context.Background(), prov))

	getErr := cli.Get(context.Background(),
		types.NamespacedName{Name: wantSAName, Namespace: "default"}, &corev1.ServiceAccount{})
	require.Error(t, getErr, "the dedicated ServiceAccount must be deleted on cleanup")
}
