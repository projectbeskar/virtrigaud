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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/devgate"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// agentHostWithExpiry is a per-host agent Host whose status mirror shows a
// pinned agent certificate expiring at at.
func agentHostWithExpiry(name string, at time.Time) *infravirtrigaudiov1beta1.Host {
	host := hostCR(name, "prov-a", nil)
	exp := metav1.NewTime(at)
	host.Status.Agent = &infravirtrigaudiov1beta1.HostAgentStatus{Mode: infravirtrigaudiov1beta1.HostAgentModeIdentity, CertificateExpiry: &exp}
	return host
}

// A Host whose agent certificate expired is deliberately not polled: its
// signed-answer and inventory conditions say so (existing reasons), instead
// of keeping a verdict from before the expiry.
func TestHostReconciler_ExpiredHostConditionsSayNotPolled(t *testing.T) {
	if !devgate.Compiled {
		t.Skip("per-host agents run in development builds only")
	}
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-a")
	prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
	host := agentHostWithExpiry("host-np", time.Now().Add(-time.Minute))
	host.Status.Conditions = []metav1.Condition{{
		Type: HostConditionAgentAnswer, Status: metav1.ConditionTrue, Reason: "Verified",
		Message: "an earlier answer", LastTransitionTime: metav1.Now(),
	}}
	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, id string) (contracts.HostInfo, error) {
		return healthyHostInfo(id), nil
	}}
	r := newHostReconciler(s, &stubResolver{provider: stub}, prov, host)
	_, err := r.Reconcile(context.Background(), hostReq("host-np"))
	require.NoError(t, err)

	got := getHost(t, r.Client, "host-np")
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthUnknown, got.Status.Health)
	inv := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified)
	require.NotNil(t, inv)
	assert.Equal(t, metav1.ConditionFalse, inv.Status)
	assert.Equal(t, infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer, inv.Reason)
	assert.Contains(t, inv.Message, "not polled")
	assert.Contains(t, inv.Message, infravirtrigaudiov1beta1.HostReasonAgentCertificateExpired)
	ans := meta.FindStatusCondition(got.Status.Conditions, HostConditionAgentAnswer)
	require.NotNil(t, ans)
	assert.Equal(t, metav1.ConditionFalse, ans.Status)
	assert.Equal(t, reasonHostAnswerMissing, ans.Reason)
	assert.Contains(t, ans.Message, "not polled")
}

// A revoked Host gets the same not-polled conditions.
func TestHostReconciler_RevokedHostConditionsSayNotPolled(t *testing.T) {
	if !devgate.Compiled {
		t.Skip("per-host agents run in development builds only")
	}
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-a")
	prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
	host := hostCR("host-rv", "prov-a", nil)
	host.Spec.Revoked = true
	r := newHostReconciler(s, &stubResolver{provider: &stubProvider{}}, prov, host)
	_, err := r.Reconcile(context.Background(), hostReq("host-rv"))
	require.NoError(t, err)
	got := getHost(t, r.Client, "host-rv")
	inv := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified)
	require.NotNil(t, inv)
	assert.Equal(t, infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer, inv.Reason)
	assert.Contains(t, inv.Message, "not polled")
	assert.Contains(t, inv.Message, infravirtrigaudiov1beta1.HostReasonAgentRevoked)
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, HostConditionAgentAnswer),
		"no signed-answer condition is invented for a Host that never had one")
}

// A certificate that expires before the next heartbeat brings the next check
// forward to just after the expiry, so Ready turns CertificateExpired then,
// not up to a heartbeat later; Ready is not False before the expiry.
func TestHostReconciler_NextCheckNoLaterThanTheCertificateExpiry(t *testing.T) {
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-a")
	prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
	host := agentHostWithExpiry("host-soon", time.Now().Add(20*time.Second))
	calls := 0
	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, id string) (contracts.HostInfo, error) {
		calls++
		return healthyHostInfo(id), nil
	}}
	r := newHostReconciler(s, &stubResolver{provider: stub}, prov, host)
	res, err := r.Reconcile(context.Background(), hostReq("host-soon"))
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "a certificate that is still valid is polled")
	ready := hostReadyCondition(t, getHost(t, r.Client, "host-soon"))
	assert.NotEqual(t, infravirtrigaudiov1beta1.HostReasonAgentCertificateExpired, ready.Reason,
		"not CertificateExpired before the expiry")
	if !devgate.Compiled {
		assert.Equal(t, hostHeartbeatInterval, res.RequeueAfter, "a release build does not run per-host agents")
		return
	}
	require.Greater(t, hostHeartbeatInterval, 25*time.Second, "the test assumes a heartbeat longer than the expiry distance")
	assert.Positive(t, res.RequeueAfter)
	assert.LessOrEqual(t, res.RequeueAfter, 21*time.Second, "rechecked right after the expiry")

	// A certificate valid for longer than a heartbeat keeps the heartbeat.
	far := agentHostWithExpiry("host-far", time.Now().Add(hostHeartbeatInterval+time.Hour))
	r = newHostReconciler(s, &stubResolver{provider: stub}, prov, far)
	res, err = r.Reconcile(context.Background(), hostReq("host-far"))
	require.NoError(t, err)
	assert.Equal(t, hostHeartbeatInterval, res.RequeueAfter)
}

// A change to a pins Secret enqueues the Host it names (a re-key re-evaluates
// Ready at once); a Secret elsewhere or with a malformed annotation does not.
func TestHostOfPinsSecret(t *testing.T) {
	m := hostOfPinsSecret("host-agent-pki")
	sec := func(ns, ann string) client.Object {
		o := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "pins"}}
		if ann != "" {
			o.Annotations = map[string]string{certrequest.AnnotationHost: ann}
		}
		return o
	}
	got := m(context.Background(), sec("host-agent-pki", "tenant-a/host-1"))
	require.Len(t, got, 1)
	assert.Equal(t, types.NamespacedName{Namespace: "tenant-a", Name: "host-1"}, got[0].NamespacedName)
	for _, o := range []client.Object{
		sec("elsewhere", "tenant-a/host-1"),
		sec("host-agent-pki", ""),
		sec("host-agent-pki", "host-1"),
		sec("host-agent-pki", "/host-1"),
		sec("host-agent-pki", "tenant-a/"),
		sec("host-agent-pki", "tenant-a/x/y"),
	} {
		assert.Empty(t, m(context.Background(), o), o.GetNamespace()+" "+o.GetAnnotations()[certrequest.AnnotationHost])
	}
}

// conflictErr is the optimistic-lock conflict the API server returns for a
// stale resourceVersion.
func conflictErr(kind, name string) error {
	return apierrors.NewConflict(schema.GroupResource{Group: infravirtrigaudiov1beta1.GroupVersion.Group, Resource: kind}, name, assert.AnError)
}

// A status update that loses an optimistic-lock race is retried quietly: a
// requeue, no error.
func TestHostReconciler_StatusConflictRequeuesQuietly(t *testing.T) {
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-a")
	host := hostCR("host-c", "prov-a", nil)
	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, id string) (contracts.HostInfo, error) {
		return healthyHostInfo(id), nil
	}}
	fc := fake.NewClientBuilder().WithScheme(s).WithObjects(prov, host).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementHostIndex, placementHostIndexValue).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Host{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ ...client.SubResourceUpdateOption) error {
				return conflictErr("hosts", obj.GetName())
			},
		}).Build()
	r := &HostReconciler{Client: fc, Scheme: s, RemoteResolver: &stubResolver{provider: stub}}
	res, err := r.Reconcile(context.Background(), hostReq("host-c"))
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{Requeue: true}, res) //nolint:staticcheck // Requeue is what the controller returns
}

// Removing the in-use finalizer from a Provider that changed since it was read
// is retried quietly: a requeue, no error, the Provider kept.
func TestProviderInUse_FinalizerRemovalConflictRequeuesQuietly(t *testing.T) {
	ctx := context.Background()
	sch := newProviderSAScheme(t)
	conflict := false
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(inUseProvider()).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*infravirtrigaudiov1beta1.Provider); ok && conflict {
					return conflictErr("providers", obj.GetName())
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch, Recorder: record.NewFakeRecorder(8)}
	deleteInUseProvider(t, r)
	got := getInUseProvider(t, r)
	got.Annotations = map[string]string{forceDeleteAnnotation: "true"}
	require.NoError(t, r.Update(ctx, got))

	conflict = true
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: inUseProviderKey})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{Requeue: true}, res) //nolint:staticcheck // Requeue is what the controller returns
	assert.False(t, providerGone(t, r))

	conflict = false
	reconcileProvider(t, r)
	assert.True(t, providerGone(t, r), "released on the retry")
}
