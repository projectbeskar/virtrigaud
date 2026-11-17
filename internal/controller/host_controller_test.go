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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/devgate"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// newHostReconciler builds a HostReconciler backed by a fake client with the
// Host status subresource enabled, and the given fake provider resolver — the
// same seam the VM-controller provider-interaction tests use (stubResolver +
// stubProvider), so no real gRPC/provider is stood up.
func newHostReconciler(s *runtime.Scheme, resolver ProviderResolver, objs ...client.Object) *HostReconciler {
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementHostIndex, placementHostIndexValue).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Host{}).
		Build()
	return &HostReconciler{Client: fc, Scheme: s, RemoteResolver: resolver}
}

// hostReq builds a reconcile request for a Host in namespace "default" (where the
// clusterProvider / hostCR helpers place their objects).
func hostReq(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}
}

// getHost re-fetches a Host from the (fake) client.
func getHost(t *testing.T, c client.Client, name string) *infravirtrigaudiov1beta1.Host {
	t.Helper()
	h := &infravirtrigaudiov1beta1.Host{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, h))
	return h
}

// healthyHostInfo is a fully-populated, Ready HostInfo — the shape #318's real
// libvirt provider returns for a reachable host.
func healthyHostInfo(id string) contracts.HostInfo {
	return contracts.HostInfo{
		ID:                 id,
		Address:            "10.0.0.1:22",
		AllocatableCPU:     32,
		AllocatableMemMiB:  262144,
		AllocatableStorage: 1 << 42,
		Health:             contracts.HostHealthReady,
		CPUModel:           "EPYC-Milan",
		CPUFeatures:        []string{"avx2", "sse4.2"},
		MachineTypes:       []string{"pc-q35-8.2"},
		EmulatorVersion:    "/usr/bin/qemu-system-x86_64",
	}
}

func hostReadyCondition(t *testing.T, host *infravirtrigaudiov1beta1.Host) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(host.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, c, "Ready condition must be present")
	return c
}

// A healthy clustered provider populates Host.status from GetHostInfo, stamps the
// heartbeat, bumps ObservedGeneration, and sets Ready=True.
func TestHostReconciler_HealthyProvider_PopulatesStatus(t *testing.T) {
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-a")
	host := hostCR("host-alpha", "prov-a", map[string]string{"topology.virtrigaud.io/rack": "r7"})
	host.Generation = 7 // model an apiserver-assigned spec generation

	var gotHostID string
	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, id string) (contracts.HostInfo, error) {
		gotHostID = id
		return healthyHostInfo(id), nil
	}}
	r := newHostReconciler(s, &stubResolver{provider: stub}, prov, host)

	res, err := r.Reconcile(context.Background(), hostReq("host-alpha"))
	require.NoError(t, err)
	assert.Equal(t, hostHeartbeatInterval, res.RequeueAfter, "successful sync requeues at the heartbeat cadence")
	assert.Equal(t, "host-alpha", gotHostID, "GetHostInfo must be called with the Host CR name as the host id")

	got := getHost(t, r.Client, "host-alpha")
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthReady, got.Status.Health)

	require.NotNil(t, got.Status.AllocatableCPU)
	assert.Equal(t, int32(32), *got.Status.AllocatableCPU)
	require.NotNil(t, got.Status.AllocatableMemoryMiB)
	assert.Equal(t, int64(262144), *got.Status.AllocatableMemoryMiB)
	require.NotNil(t, got.Status.AllocatableStorageBytes)
	assert.Equal(t, int64(1<<42), *got.Status.AllocatableStorageBytes)
	assert.Equal(t, "EPYC-Milan", got.Status.CPUModel)
	assert.Equal(t, []string{"avx2", "sse4.2"}, got.Status.CPUFeatures)
	assert.Equal(t, []string{"pc-q35-8.2"}, got.Status.MachineTypes)
	assert.Equal(t, "/usr/bin/qemu-system-x86_64", got.Status.EmulatorVersion)

	require.NotNil(t, got.Status.LastHeartbeatTime, "a successful sync must stamp LastHeartbeatTime")
	assert.Equal(t, int64(7), got.Status.ObservedGeneration, "spec generation must be observed in status")
	assert.Equal(t, int32(0), got.Status.BoundVMs, "BoundVMs is deferred to the placement slice (D1)")

	ready := hostReadyCondition(t, got)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, reasonHostReady, ready.Reason)
	assert.Equal(t, int64(7), ready.ObservedGeneration, "the Ready condition carries ObservedGeneration")
}

// A NotReady health enum maps to HostHealthNotReady + Ready=False, and the sync
// still stamps the heartbeat (the provider WAS reachable).
func TestHostReconciler_NotReadyHealth_MapsThrough(t *testing.T) {
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-a")
	host := hostCR("host-nr", "prov-a", nil)

	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, id string) (contracts.HostInfo, error) {
		return contracts.HostInfo{ID: id, Health: contracts.HostHealthNotReady}, nil
	}}
	r := newHostReconciler(s, &stubResolver{provider: stub}, prov, host)

	res, err := r.Reconcile(context.Background(), hostReq("host-nr"))
	require.NoError(t, err)
	assert.Equal(t, hostHeartbeatInterval, res.RequeueAfter)

	got := getHost(t, r.Client, "host-nr")
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthNotReady, got.Status.Health)
	require.NotNil(t, got.Status.LastHeartbeatTime)
	ready := hostReadyCondition(t, got)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, reasonHostNotReady, ready.Reason)
}

// A provider/GetHostInfo transient error must NOT crash-loop: health goes to
// Unknown (never Ready), Ready=False/ProviderUnavailable, no heartbeat stamped,
// and the reconcile requeues on the shorter backoff with no returned error.
func TestHostReconciler_GetHostInfoError_MarksNotReady(t *testing.T) {
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-a")
	host := hostCR("host-alpha", "prov-a", nil)

	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, _ string) (contracts.HostInfo, error) {
		return contracts.HostInfo{}, contracts.NewRetryableError("getHostInfo: connection refused", nil)
	}}
	r := newHostReconciler(s, &stubResolver{provider: stub}, prov, host)

	res, err := r.Reconcile(context.Background(), hostReq("host-alpha"))
	require.NoError(t, err, "an unreachable provider must not be a hard reconcile error")
	assert.Equal(t, hostSyncBackoffInterval, res.RequeueAfter, "a transient error requeues on the shorter backoff")

	got := getHost(t, r.Client, "host-alpha")
	assert.NotEqual(t, infravirtrigaudiov1beta1.HostHealthReady, got.Status.Health)
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthUnknown, got.Status.Health)
	assert.Nil(t, got.Status.LastHeartbeatTime, "a failed sync must not stamp a heartbeat")
	ready := hostReadyCondition(t, got)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, reasonProviderUnavailable, ready.Reason)
	assert.Equal(t, got.Generation, ready.ObservedGeneration)
}

// GetHostInfo returning a NotSupported error (the typed form the transport client
// now maps gRPC Unimplemented to) means the referenced provider does not front an
// inventory: Health=Unknown + ProviderNotClustered, not a crash-loop.
func TestHostReconciler_GetHostInfoUnimplemented_ProviderNotClustered(t *testing.T) {
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-a")
	host := hostCR("host-alpha", "prov-a", nil)

	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, _ string) (contracts.HostInfo, error) {
		return contracts.HostInfo{}, contracts.NewNotSupportedError("getHostInfo: not a clustered provider")
	}}
	r := newHostReconciler(s, &stubResolver{provider: stub}, prov, host)

	res, err := r.Reconcile(context.Background(), hostReq("host-alpha"))
	require.NoError(t, err)
	assert.Equal(t, hostHeartbeatInterval, res.RequeueAfter)

	got := getHost(t, r.Client, "host-alpha")
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthUnknown, got.Status.Health)
	ready := hostReadyCondition(t, got)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, reasonProviderNotClustered, ready.Reason)
}

// A Host referencing a single-topology Provider is short-circuited BEFORE the RPC
// (a single-host provider cannot answer GetHostInfo): Health=Unknown +
// ProviderNotClustered, and GetHostInfo is never called.
func TestHostReconciler_SingleTopologyProvider_NotClustered(t *testing.T) {
	s := coverageTestScheme(t)
	// providerWithRuntime leaves spec.topology unset ("" == single).
	prov := providerWithRuntime("prov-single", &infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false})
	host := hostCR("host-alpha", "prov-single", nil)

	called := false
	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, _ string) (contracts.HostInfo, error) {
		called = true
		return healthyHostInfo("host-alpha"), nil
	}}
	r := newHostReconciler(s, &stubResolver{provider: stub}, prov, host)

	res, err := r.Reconcile(context.Background(), hostReq("host-alpha"))
	require.NoError(t, err)
	assert.Equal(t, hostHeartbeatInterval, res.RequeueAfter)
	assert.False(t, called, "GetHostInfo must NOT be called for a non-clustered provider")

	got := getHost(t, r.Client, "host-alpha")
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthUnknown, got.Status.Health)
	ready := hostReadyCondition(t, got)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, reasonProviderNotClustered, ready.Reason)
}

// A missing Provider is recorded as ProviderUnavailable and requeued on the
// backoff — never a hard error.
func TestHostReconciler_ProviderMissing_Unavailable(t *testing.T) {
	s := coverageTestScheme(t)
	host := hostCR("host-alpha", "ghost-provider", nil) // no such Provider object

	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, _ string) (contracts.HostInfo, error) {
		t.Fatalf("GetHostInfo must not be called when the Provider is missing")
		return contracts.HostInfo{}, nil
	}}
	r := newHostReconciler(s, &stubResolver{provider: stub}, host)

	res, err := r.Reconcile(context.Background(), hostReq("host-alpha"))
	require.NoError(t, err)
	assert.Equal(t, hostSyncBackoffInterval, res.RequeueAfter)

	got := getHost(t, r.Client, "host-alpha")
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthUnknown, got.Status.Health)
	ready := hostReadyCondition(t, got)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, reasonProviderUnavailable, ready.Reason)
}

// TestHostReconciler_ResolvesProviderInHostNamespaceOnly pins the same-namespace
// model for the inventory-sync path: a Host in namespace "tenant" whose
// providerRef names "prov-a" must NOT resolve the "prov-a" Provider that exists
// only in "default". It is recorded as ProviderUnavailable and GetHostInfo is
// never driven against the other namespace's provider.
func TestHostReconciler_ResolvesProviderInHostNamespaceOnly(t *testing.T) {
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-a") // namespace "default"
	host := hostCR("host-alpha", "prov-a", nil)
	host.Namespace = "tenant"

	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, _ string) (contracts.HostInfo, error) {
		t.Fatalf("GetHostInfo must not be called against a Provider in another namespace")
		return contracts.HostInfo{}, nil
	}}
	r := newHostReconciler(s, &stubResolver{provider: stub}, prov, host)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "host-alpha"},
	})
	require.NoError(t, err)
	assert.Equal(t, hostSyncBackoffInterval, res.RequeueAfter)

	got := &infravirtrigaudiov1beta1.Host{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "host-alpha"}, got))
	ready := hostReadyCondition(t, got)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, reasonProviderUnavailable, ready.Reason)
}

// The sync is status-only (spec is never mutated) and idempotent: a second
// reconcile leaves the durable fields stable and the spec byte-identical.
func TestHostReconciler_StatusOnlyAndIdempotent(t *testing.T) {
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-a")
	host := hostCR("host-alpha", "prov-a", map[string]string{"zone": "z1"})
	host.Generation = 3
	wantSpec := *host.Spec.DeepCopy()

	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, id string) (contracts.HostInfo, error) {
		return healthyHostInfo(id), nil
	}}
	r := newHostReconciler(s, &stubResolver{provider: stub}, prov, host)

	_, err := r.Reconcile(context.Background(), hostReq("host-alpha"))
	require.NoError(t, err)
	first := getHost(t, r.Client, "host-alpha")
	assert.Equal(t, wantSpec, first.Spec, "spec must be untouched after reconcile")

	_, err = r.Reconcile(context.Background(), hostReq("host-alpha"))
	require.NoError(t, err)
	second := getHost(t, r.Client, "host-alpha")

	assert.Equal(t, wantSpec, second.Spec, "spec must stay untouched on re-reconcile")
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthReady, second.Status.Health)
	assert.Equal(t, int64(3), first.Status.ObservedGeneration)
	assert.Equal(t, first.Status.ObservedGeneration, second.Status.ObservedGeneration)
	assert.Equal(t, reasonHostReady, hostReadyCondition(t, second).Reason)
}

// A reconcile for an already-deleted Host is a no-op success (no finalizer, no
// external cleanup).
func TestHostReconciler_HostGone_NoError(t *testing.T) {
	s := coverageTestScheme(t)
	r := newHostReconciler(s, &stubResolver{provider: &stubProvider{}})

	res, err := r.Reconcile(context.Background(), hostReq("does-not-exist"))
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)
	assert.False(t, res.Requeue)
}

// A revoked Host of a clustered Provider for per-host agents is not polled:
// GetHostInfo is never called, Ready=False says Revoked, and it is rechecked
// at the heartbeat cadence (no error, no short backoff, no log per poll).
// A revoked libvirt Host (spec.revoked does not apply to it) is still polled.
func TestHostReconciler_RevokedAgentHost_NotPolled(t *testing.T) {
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-a")
	prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
	host := hostCR("host-rev", "prov-a", nil)
	host.Spec.Revoked = true

	calls := 0
	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, id string) (contracts.HostInfo, error) {
		calls++
		return healthyHostInfo(id), nil
	}}
	r := newHostReconciler(s, &stubResolver{provider: stub}, prov, host)
	if !devgate.Compiled {
		_, err := r.Reconcile(context.Background(), hostReq("host-rev"))
		require.NoError(t, err)
		assert.Equal(t, 1, calls, "a release build does not read spec.revoked")
		return
	}
	for i := 0; i < 2; i++ {
		res, err := r.Reconcile(context.Background(), hostReq("host-rev"))
		require.NoError(t, err)
		assert.Equal(t, hostHeartbeatInterval, res.RequeueAfter)
	}
	assert.Zero(t, calls, "a revoked agent Host is never polled")
	got := getHost(t, r.Client, "host-rev")
	ready := hostReadyCondition(t, got)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, infravirtrigaudiov1beta1.HostReasonAgentRevoked, ready.Reason)
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthUnknown, got.Status.Health)

	libvirtProv := clusterProvider("prov-b")
	lvHost := hostCR("host-lv", "prov-b", nil)
	lvHost.Spec.Revoked = true
	r = newHostReconciler(s, &stubResolver{provider: stub}, libvirtProv, lvHost)
	_, err := r.Reconcile(context.Background(), hostReq("host-lv"))
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "spec.revoked does not apply to a libvirt Host")
}

// A per-host agent Host whose pinned certificate has expired is not polled:
// Ready=False/CertificateExpired, rechecked at the heartbeat cadence without
// a call; once a re-key pins a new certificate it is polled again.
func TestHostReconciler_ExpiredAgentCertificate_NotPolledUntilRekeyed(t *testing.T) {
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-a")
	prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
	host := hostCR("host-exp", "prov-a", nil)
	past := metav1.NewTime(time.Now().Add(-time.Minute))
	host.Status.Agent = &infravirtrigaudiov1beta1.HostAgentStatus{Mode: infravirtrigaudiov1beta1.HostAgentModeIdentity, CertificateExpiry: &past}
	calls := 0
	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, id string) (contracts.HostInfo, error) {
		calls++
		return healthyHostInfo(id), nil
	}}
	r := newHostReconciler(s, &stubResolver{provider: stub}, prov, host)
	res, err := r.Reconcile(context.Background(), hostReq("host-exp"))
	require.NoError(t, err)
	if !devgate.Compiled {
		assert.Equal(t, 1, calls, "a release build does not run per-host agents")
		return
	}
	assert.Equal(t, hostHeartbeatInterval, res.RequeueAfter)
	assert.Zero(t, calls, "an expired agent Host is not polled")
	got := getHost(t, r.Client, "host-exp")
	ready := hostReadyCondition(t, got)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, infravirtrigaudiov1beta1.HostReasonAgentCertificateExpired, ready.Reason)

	// A re-key pins a new certificate: the next reconcile polls again.
	future := metav1.NewTime(time.Now().Add(time.Hour))
	got.Status.Agent.CertificateExpiry = &future
	require.NoError(t, r.Status().Update(context.Background(), got))
	_, err = r.Reconcile(context.Background(), hostReq("host-exp"))
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "polled again after the re-key")
}
