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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the Host in-use finalizer (ADR-0007 Addendum A, A1): a Host
// cannot be deleted while any VirtualMachine of its Provider names it in
// status.placement.host or status.placement.pendingHost.

func vmPlacedOn(ns, name, providerName, providerNS string, placement infravirtrigaudiov1beta1.PlacementStatus) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: providerName, Namespace: providerNS},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "c"},
		},
	}
	vm.Status.Placement = &placement
	return vm
}

func healthyHostStub() *stubProvider {
	return &stubProvider{GetHostInfoFn: func(_ context.Context, id string) (contracts.HostInfo, error) {
		return healthyHostInfo(id), nil
	}}
}

func TestHostReconciler_AddsInUseFinalizer(t *testing.T) {
	r := newHostReconciler(coverageTestScheme(t), &stubResolver{provider: healthyHostStub()},
		clusterProvider("prov-a"), hostCR("host-alpha", "prov-a", nil))

	_, err := r.Reconcile(context.Background(), hostReq("host-alpha"))
	require.NoError(t, err)
	assert.Contains(t, getHost(t, r.Client, "host-alpha").Finalizers, infravirtrigaudiov1beta1.HostInUseFinalizer)
}

func TestHostReconciler_InUseFinalizerBlocksDeletion(t *testing.T) {
	cases := map[string]*infravirtrigaudiov1beta1.VirtualMachine{
		"bound":                 vmPlacedOn("default", "web", "prov-a", "", infravirtrigaudiov1beta1.PlacementStatus{Host: "host-alpha"}),
		"create pending":        vmPlacedOn("default", "db", "prov-a", "", infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-alpha"}),
		"VM in other namespace": vmPlacedOn("team-b", "api", "prov-a", "default", infravirtrigaudiov1beta1.PlacementStatus{Host: "host-alpha"}),
	}
	for name, user := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			host := hostCR("host-alpha", "prov-a", nil)
			host.Finalizers = []string{infravirtrigaudiov1beta1.HostInUseFinalizer}
			r := newHostReconciler(coverageTestScheme(t), &stubResolver{provider: healthyHostStub()},
				clusterProvider("prov-a"), host, user)

			require.NoError(t, r.Delete(ctx, getHost(t, r.Client, "host-alpha")))
			res, err := r.Reconcile(ctx, hostReq("host-alpha"))
			require.NoError(t, err)
			assert.Equal(t, hostInUseRetryInterval, res.RequeueAfter, "a blocked deletion re-checks")

			got := getHost(t, r.Client, "host-alpha")
			assert.Contains(t, got.Finalizers, infravirtrigaudiov1beta1.HostInUseFinalizer, "the Host must not be deleted while in use")
			ready := hostReadyCondition(t, got)
			assert.Equal(t, reasonHostInUse, ready.Reason)
			assert.Contains(t, ready.Message, user.Namespace+"/"+user.Name)

			// The VM goes away → the next reconcile releases the finalizer.
			require.NoError(t, r.Delete(ctx, user))
			_, err = r.Reconcile(ctx, hostReq("host-alpha"))
			require.NoError(t, err)
			getErr := r.Get(ctx, types.NamespacedName{Namespace: "default", Name: "host-alpha"}, &infravirtrigaudiov1beta1.Host{})
			assert.True(t, apierrors.IsNotFound(getErr), "the Host is released once no VM uses it")
		})
	}
}

// TestHostReconciler_InUseIgnoresOtherProvidersAndNamespaces proves the
// same-namespace model (#330): a VM bound to a same-named Host of ANOTHER
// Provider, or of a Provider in another namespace, does not block this Host.
func TestHostReconciler_InUseIgnoresOtherProvidersAndNamespaces(t *testing.T) {
	ctx := context.Background()
	host := hostCR("host-alpha", "prov-a", nil)
	host.Finalizers = []string{infravirtrigaudiov1beta1.HostInUseFinalizer}
	unrelated := []*infravirtrigaudiov1beta1.VirtualMachine{
		vmPlacedOn("default", "other-provider", "prov-b", "", infravirtrigaudiov1beta1.PlacementStatus{Host: "host-alpha"}),
		vmPlacedOn("team-b", "other-ns-provider", "prov-a", "", infravirtrigaudiov1beta1.PlacementStatus{Host: "host-alpha"}),
		vmPlacedOn("default", "other-host", "prov-a", "", infravirtrigaudiov1beta1.PlacementStatus{Host: "host-beta"}),
	}
	r := newHostReconciler(coverageTestScheme(t), &stubResolver{provider: healthyHostStub()},
		clusterProvider("prov-a"), host, unrelated[0], unrelated[1], unrelated[2])

	require.NoError(t, r.Delete(ctx, getHost(t, r.Client, "host-alpha")))
	_, err := r.Reconcile(ctx, hostReq("host-alpha"))
	require.NoError(t, err)
	getErr := r.Get(ctx, types.NamespacedName{Namespace: "default", Name: "host-alpha"}, &infravirtrigaudiov1beta1.Host{})
	assert.True(t, apierrors.IsNotFound(getErr), "no VM of THIS Host uses it, so deletion proceeds")
}
