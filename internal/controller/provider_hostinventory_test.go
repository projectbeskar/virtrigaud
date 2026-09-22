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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
)

// clusterProvider builds a topology=cluster Provider with TLS explicitly
// disabled, so a full Reconcile proceeds past the TLS-posture gate all the way
// to rendering the host-inventory Secret and Deployment.
func clusterProvider(name string) *infravirtrigaudiov1beta1.Provider {
	p := providerWithRuntime(name, &infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false})
	p.UID = types.UID("uid-" + name)
	p.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeLibvirt
	p.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
	return p
}

// hostCR builds a Host CR in namespace "default" whose spec.providerRef names
// providerName (same-namespace ref).
func hostCR(name, providerName string, labels map[string]string) *infravirtrigaudiov1beta1.Host {
	return &infravirtrigaudiov1beta1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.HostSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: providerName},
			PoolRef:     infravirtrigaudiov1beta1.LocalObjectReference{Name: "pool-a"},
			Endpoint:    "qemu+ssh://virt@" + name + "/system",
			Labels:      labels,
			Schedulable: true,
		},
	}
}

// findVolume / findVolumeMount locate a named volume / mount in a Deployment's
// pod template, returning it and whether it was present.
func findVolume(dep *appsv1.Deployment, name string) (corev1.Volume, bool) {
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == name {
			return v, true
		}
	}
	return corev1.Volume{}, false
}

func findVolumeMount(dep *appsv1.Deployment, name string) (corev1.VolumeMount, bool) {
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return corev1.VolumeMount{}, false
	}
	for _, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == name {
			return m, true
		}
	}
	return corev1.VolumeMount{}, false
}

// TestProvider_ClusterTopology_RendersHostInventorySecret is the end-to-end
// happy path: a cluster-topology Provider with several Hosts renders exactly its
// own hosts into an owner-referenced Secret, and mounts that Secret read-only
// into the provider Deployment.
func TestProvider_ClusterTopology_RendersHostInventorySecret(t *testing.T) {
	sch := newProviderTLSScheme(t)
	prov := clusterProvider("libvirt-cluster")

	// Two hosts that belong to this provider (given out of order to prove
	// deterministic sorting), one that belongs to a DIFFERENT provider, and one
	// whose providerRef targets a different namespace — the last two must be
	// excluded from the render.
	hostB := hostCR("host-b", "libvirt-cluster", map[string]string{"net.virtrigaud.io/br-vlan100": "true"})
	hostA := hostCR("host-a", "libvirt-cluster", map[string]string{"storage.virtrigaud.io/pool-nfs01": "true"})
	otherProvider := hostCR("host-x", "some-other-provider", nil)
	crossNS := hostCR("host-y", "libvirt-cluster", nil)
	crossNS.Spec.ProviderRef.Namespace = "elsewhere" // resolves to ns "elsewhere" != provider ns

	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov, hostB, hostA, otherProvider, crossNS).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "libvirt-cluster", Namespace: "default"},
	})
	require.NoError(t, err)

	// --- The rendered Secret ---------------------------------------------
	secret := &corev1.Secret{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: "libvirt-cluster-hosts", Namespace: "default"}, secret),
		"a host-inventory Secret named <provider>-hosts must be rendered")

	assert.Equal(t, corev1.SecretTypeOpaque, secret.Type)

	// Owner-referenced to the Provider so it is GC'd with it.
	owner := metav1.GetControllerOf(secret)
	require.NotNil(t, owner, "the host-inventory Secret must be owner-referenced to its Provider")
	assert.Equal(t, "Provider", owner.Kind)
	assert.Equal(t, "libvirt-cluster", owner.Name)

	// Decode the inventory and assert only this provider's hosts, sorted, with
	// empty credentials.
	raw, ok := secret.Data[hostsecret.SecretDataKey]
	require.True(t, ok, "Secret must carry the %q key", hostsecret.SecretDataKey)
	inv, err := hostsecret.Unmarshal(raw)
	require.NoError(t, err)
	assert.Equal(t, hostsecret.SchemaVersion, inv.SchemaVersion)
	require.Len(t, inv.Hosts, 2, "only the two same-provider, same-namespace hosts are rendered")
	assert.Equal(t, "host-a", inv.Hosts[0].ID, "hosts must be sorted by id")
	assert.Equal(t, "host-b", inv.Hosts[1].ID)
	assert.Equal(t, "qemu+ssh://virt@host-a/system", inv.Hosts[0].Endpoint)
	assert.Equal(t, map[string]string{"storage.virtrigaud.io/pool-nfs01": "true"}, inv.Hosts[0].Labels)
	assert.Equal(t, hostsecret.Credentials{}, inv.Hosts[0].Credentials,
		"credentials must be empty in the structural PR — no credential material rendered")

	// --- The Deployment mount --------------------------------------------
	dep := &appsv1.Deployment{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: r.getDeploymentName(prov), Namespace: "default"}, dep))

	vol, ok := findVolume(dep, hostInventoryVolumeName)
	require.True(t, ok, "cluster provider Deployment must carry the host-inventory volume")
	require.NotNil(t, vol.Secret, "host-inventory volume must be Secret-backed")
	assert.Equal(t, "libvirt-cluster-hosts", vol.Secret.SecretName)

	mnt, ok := findVolumeMount(dep, hostInventoryVolumeName)
	require.True(t, ok, "cluster provider container must mount the host-inventory volume")
	assert.Equal(t, hostsecret.MountPath, mnt.MountPath)
	assert.True(t, mnt.ReadOnly, "the host-inventory mount must be read-only")
}

// TestProvider_SingleTopology_RendersNothing proves the D9 invariant: a
// single-topology (default) Provider renders no Secret and its Deployment carries
// no host-inventory volume/mount, even when a Host references it.
func TestProvider_SingleTopology_RendersNothing(t *testing.T) {
	sch := newProviderTLSScheme(t)
	// Default topology: leave Spec.Topology unset ("") — the controller treats
	// "" identically to "single".
	prov := providerWithRuntime("single-prov", &infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false})
	host := hostCR("host-a", "single-prov", nil)

	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov, host).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "single-prov", Namespace: "default"},
	})
	require.NoError(t, err)

	// No host-inventory Secret.
	err = cli.Get(context.Background(),
		types.NamespacedName{Name: "single-prov-hosts", Namespace: "default"}, &corev1.Secret{})
	require.Error(t, err, "single-topology Provider must not render a host-inventory Secret")

	// No host-inventory volume/mount on the Deployment.
	dep := &appsv1.Deployment{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: r.getDeploymentName(prov), Namespace: "default"}, dep))
	_, ok := findVolume(dep, hostInventoryVolumeName)
	assert.False(t, ok, "single-topology Deployment must not carry the host-inventory volume")
	_, ok = findVolumeMount(dep, hostInventoryVolumeName)
	assert.False(t, ok, "single-topology container must not mount the host-inventory volume")
}

// TestProvider_HostInventory_Idempotent proves a re-render of an unchanged
// inventory does not write the Secret again (no ResourceVersion churn).
func TestProvider_HostInventory_Idempotent(t *testing.T) {
	sch := newProviderTLSScheme(t)
	prov := clusterProvider("libvirt-cluster")
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov, hostCR("host-a", "libvirt-cluster", nil), hostCR("host-b", "libvirt-cluster", nil)).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	require.NoError(t, r.reconcileHostInventorySecret(context.Background(), prov))
	first := &corev1.Secret{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: "libvirt-cluster-hosts", Namespace: "default"}, first))

	require.NoError(t, r.reconcileHostInventorySecret(context.Background(), prov))
	second := &corev1.Secret{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: "libvirt-cluster-hosts", Namespace: "default"}, second))

	assert.Equal(t, first.ResourceVersion, second.ResourceVersion,
		"re-rendering an unchanged inventory must not write the Secret again")
}

// TestProvider_HostInventory_AddRemoveHost proves a Host add and a Host remove
// both update the rendered Secret.
func TestProvider_HostInventory_AddRemoveHost(t *testing.T) {
	sch := newProviderTLSScheme(t)
	prov := clusterProvider("libvirt-cluster")
	hostA := hostCR("host-a", "libvirt-cluster", nil)
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov, hostA).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	// Initial render: one host.
	require.NoError(t, r.reconcileHostInventorySecret(context.Background(), prov))
	assert.Equal(t, []string{"host-a"}, renderedHostIDs(t, cli))

	// Add a host -> two hosts.
	require.NoError(t, cli.Create(context.Background(), hostCR("host-b", "libvirt-cluster", nil)))
	require.NoError(t, r.reconcileHostInventorySecret(context.Background(), prov))
	assert.Equal(t, []string{"host-a", "host-b"}, renderedHostIDs(t, cli))

	// Remove host-a -> one host.
	require.NoError(t, cli.Delete(context.Background(), hostA))
	require.NoError(t, r.reconcileHostInventorySecret(context.Background(), prov))
	assert.Equal(t, []string{"host-b"}, renderedHostIDs(t, cli))
}

// renderedHostIDs reads the host-inventory Secret and returns the host IDs it
// carries, in rendered order.
func renderedHostIDs(t *testing.T, cli client.Client) []string {
	t.Helper()
	secret := &corev1.Secret{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: "libvirt-cluster-hosts", Namespace: "default"}, secret))
	inv, err := hostsecret.Unmarshal(secret.Data[hostsecret.SecretDataKey])
	require.NoError(t, err)
	ids := make([]string, 0, len(inv.Hosts))
	for _, h := range inv.Hosts {
		ids = append(ids, h.ID)
	}
	return ids
}

// TestProvidersForHost_MapsToProviderRef proves a Host event enqueues exactly
// its owning Provider, resolving the ref namespace from the Host when unset and
// honoring an explicit ref namespace when set.
func TestProvidersForHost_MapsToProviderRef(t *testing.T) {
	r := &ProviderReconciler{}

	// Same-namespace ref: resolves to the Host's namespace.
	got := r.providersForHost(context.Background(), hostCR("host-a", "libvirt-cluster", nil))
	require.Len(t, got, 1)
	assert.Equal(t, types.NamespacedName{Namespace: "default", Name: "libvirt-cluster"}, got[0].NamespacedName)

	// Explicit cross-namespace ref: resolves to the ref's namespace.
	h := hostCR("host-a", "libvirt-cluster", nil)
	h.Spec.ProviderRef.Namespace = "elsewhere"
	got = r.providersForHost(context.Background(), h)
	require.Len(t, got, 1)
	assert.Equal(t, types.NamespacedName{Namespace: "elsewhere", Name: "libvirt-cluster"}, got[0].NamespacedName)

	// A non-Host object yields no requests.
	assert.Nil(t, r.providersForHost(context.Background(), &corev1.Secret{}))
}

// TestProvidersForHostPool_MapsToProviderRef proves a HostPool event enqueues
// its owning Provider.
func TestProvidersForHostPool_MapsToProviderRef(t *testing.T) {
	r := &ProviderReconciler{}
	pool := &infravirtrigaudiov1beta1.HostPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.HostPoolSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "libvirt-cluster"},
		},
	}
	got := r.providersForHostPool(context.Background(), pool)
	require.Len(t, got, 1)
	assert.Equal(t, types.NamespacedName{Namespace: "default", Name: "libvirt-cluster"}, got[0].NamespacedName)

	// A non-HostPool object yields no requests.
	assert.Nil(t, r.providersForHostPool(context.Background(), &corev1.Secret{}))
}
