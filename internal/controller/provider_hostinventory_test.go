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
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
)

// Fake (NOT real) SSH credential material used across the host-inventory
// credential tests. The distinctive tokens make the no-leak assertions precise:
// if any byte of these appears in a log line or event, the test fails.
var (
	testHostSSHKey     = []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nZZZ-SECRET-KEY-MATERIAL-DO-NOT-LOG-0001\ndeadbeefcafe==\n-----END OPENSSH PRIVATE KEY-----\n")
	testHostKnownHosts = []byte("host-a ssh-ed25519 AAAAC3-SECRET-KNOWN-HOSTS-DO-NOT-LOG-0002\n")
)

// credSecret builds an Opaque credential Secret in namespace "default" carrying
// the given data keys, mirroring the shape a Provider/Host credentialSecretRef
// points at.
func credSecret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

// sshCredData is the standard well-formed credential Secret payload (ssh key +
// known_hosts), matching the libvirt credential Secret keys the render mirrors.
func sshCredData() map[string][]byte {
	return map[string][]byte{
		"ssh-privatekey": testHostSSHKey,
		"known_hosts":    testHostKnownHosts,
	}
}

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
			ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: providerName},
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
	// that names this provider but lives in ANOTHER namespace — the last two
	// must be excluded from the render.
	hostB := hostCR("host-b", "libvirt-cluster", map[string]string{"net.virtrigaud.io/br-vlan100": "true"})
	hostA := hostCR("host-a", "libvirt-cluster", map[string]string{"storage.virtrigaud.io/pool-nfs01": "true"})
	otherProvider := hostCR("host-x", "some-other-provider", nil)
	crossNS := hostCR("host-y", "libvirt-cluster", nil)
	crossNS.Namespace = "elsewhere" // same provider NAME, different namespace

	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov, hostB, hostA, otherProvider, crossNS, credSecret("test-creds", sshCredData())).
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
	// Both hosts fall back to the Provider default credentialSecretRef
	// ("test-creds"), so each carries the inlined SSH key + known_hosts.
	assert.True(t, bytes.Equal(testHostSSHKey, inv.Hosts[0].Credentials.SSHPrivateKey),
		"host credentials must inline the SSH private key from the Provider default secret")
	assert.True(t, bytes.Equal(testHostKnownHosts, inv.Hosts[0].Credentials.KnownHosts),
		"host credentials must inline known_hosts from the Provider default secret")
	assert.True(t, bytes.Equal(testHostSSHKey, inv.Hosts[1].Credentials.SSHPrivateKey))

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
		WithObjects(prov, hostCR("host-a", "libvirt-cluster", nil), hostCR("host-b", "libvirt-cluster", nil),
			credSecret("test-creds", sshCredData())).
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
		WithObjects(prov, hostA, credSecret("test-creds", sshCredData())).
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
// its owning Provider, always in the Host's own namespace (same-namespace
// model: providerRef is a LocalObjectReference).
func TestProvidersForHost_MapsToProviderRef(t *testing.T) {
	r := &ProviderReconciler{}

	got := r.providersForHost(context.Background(), hostCR("host-a", "libvirt-cluster", nil))
	require.Len(t, got, 1)
	assert.Equal(t, types.NamespacedName{Namespace: "default", Name: "libvirt-cluster"}, got[0].NamespacedName)

	// A Host in another namespace maps to the same-named Provider in ITS OWN
	// namespace — never to the "default" Provider.
	h := hostCR("host-a", "libvirt-cluster", nil)
	h.Namespace = "elsewhere"
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
			ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "libvirt-cluster"},
		},
	}
	got := r.providersForHostPool(context.Background(), pool)
	require.Len(t, got, 1)
	assert.Equal(t, types.NamespacedName{Namespace: "default", Name: "libvirt-cluster"}, got[0].NamespacedName)

	// A non-HostPool object yields no requests.
	assert.Nil(t, r.providersForHostPool(context.Background(), &corev1.Secret{}))
}

// renderedInventory reads the host-inventory Secret and returns the full parsed
// Inventory (hosts + inlined credentials), for credential-level assertions.
func renderedInventory(t *testing.T, cli client.Client) hostsecret.Inventory {
	t.Helper()
	secret := &corev1.Secret{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: "libvirt-cluster-hosts", Namespace: "default"}, secret))
	inv, err := hostsecret.Unmarshal(secret.Data[hostsecret.SecretDataKey])
	require.NoError(t, err)
	return inv
}

// drainEvents non-blockingly collects everything a FakeRecorder has buffered.
func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestProvider_HostInventory_PerHostCredentialSecretRef proves per-host
// credential resolution: a Host with its own spec.credentialSecretRef uses that
// Secret, while a Host without one falls back to the Provider's default
// credentialSecretRef — each host inlining its OWN source material.
func TestProvider_HostInventory_PerHostCredentialSecretRef(t *testing.T) {
	sch := newProviderTLSScheme(t)
	prov := clusterProvider("libvirt-cluster") // default credentialSecretRef: test-creds

	hostAKey := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nHOST-A-OWN-KEY-0003\n-----END OPENSSH PRIVATE KEY-----\n")

	// host-a overrides with its own secret; host-b falls back to the default.
	hostA := hostCR("host-a", "libvirt-cluster", nil)
	hostA.Spec.CredentialSecretRef = &infravirtrigaudiov1beta1.LocalObjectReference{Name: "host-a-creds"}
	hostB := hostCR("host-b", "libvirt-cluster", nil)

	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov, hostA, hostB,
			credSecret("test-creds", sshCredData()),
			credSecret("host-a-creds", map[string][]byte{"ssh-privatekey": hostAKey})).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	require.NoError(t, r.reconcileHostInventorySecret(context.Background(), prov))

	inv := renderedInventory(t, cli)
	require.Len(t, inv.Hosts, 2)
	// host-a uses its own key (and carries no known_hosts, which its secret omits).
	assert.True(t, bytes.Equal(hostAKey, inv.Hosts[0].Credentials.SSHPrivateKey),
		"host-a must inline its own credentialSecretRef key")
	assert.Nil(t, inv.Hosts[0].Credentials.KnownHosts,
		"host-a's secret has no known_hosts, so the field must be omitted")
	// host-b falls back to the Provider default.
	assert.True(t, bytes.Equal(testHostSSHKey, inv.Hosts[1].Credentials.SSHPrivateKey),
		"host-b must fall back to the Provider default key")
	assert.True(t, bytes.Equal(testHostKnownHosts, inv.Hosts[1].Credentials.KnownHosts))

	// All hosts resolved -> HostCredentialsReady=True.
	cond := getConditionByType(t, prov.Status.Conditions, conditionHostCredentialsReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, reasonCredentialsResolved, cond.Reason)
}

// TestProvider_HostInventory_MissingCredentialSecret_SkipsHost proves a host
// whose credential Secret is missing is SKIPPED from the render while its
// siblings still render, and the skip is surfaced via a non-secret condition and
// a Warning event that name the host id + reason but no credential value.
func TestProvider_HostInventory_MissingCredentialSecret_SkipsHost(t *testing.T) {
	sch := newProviderTLSScheme(t)
	prov := clusterProvider("libvirt-cluster")

	hostA := hostCR("host-a", "libvirt-cluster", nil) // falls back to test-creds (present)
	hostB := hostCR("host-b", "libvirt-cluster", nil)
	hostB.Spec.CredentialSecretRef = &infravirtrigaudiov1beta1.LocalObjectReference{Name: "missing-creds"} // absent

	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov, hostA, hostB, credSecret("test-creds", sshCredData())).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	rec := record.NewFakeRecorder(10)
	r := &ProviderReconciler{Client: cli, Scheme: sch, Recorder: rec}

	require.NoError(t, r.reconcileHostInventorySecret(context.Background(), prov),
		"one host's missing credentials must NOT fail the whole reconcile")

	// Only host-a rendered; host-b was dropped.
	assert.Equal(t, []string{"host-a"}, renderedHostIDs(t, cli))

	// Condition surfaces the skip, names host-b, and leaks no key material.
	cond := getConditionByType(t, prov.Status.Conditions, conditionHostCredentialsReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonCredentialsUnresolved, cond.Reason)
	assert.Contains(t, cond.Message, "host-b")
	assert.Contains(t, cond.Message, "not found")
	assert.NotContains(t, cond.Message, "host-a", "a rendered host must not appear in the skip message")
	assertNoCredentialLeak(t, cond.Message)

	// A Warning event was emitted naming the same, and no key material.
	events := drainEvents(rec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "Warning")
	assert.Contains(t, events[0], eventReasonCredentialsUnresolved)
	assert.Contains(t, events[0], "host-b")
	assertNoCredentialLeak(t, events[0])
}

// TestProvider_HostInventory_MalformedCredentialSecret_SkipsHost proves a host
// whose credential Secret EXISTS but lacks the SSH private key is treated the
// same as missing: skipped, siblings render, surfaced without leaking material.
func TestProvider_HostInventory_MalformedCredentialSecret_SkipsHost(t *testing.T) {
	sch := newProviderTLSScheme(t)
	prov := clusterProvider("libvirt-cluster")

	hostA := hostCR("host-a", "libvirt-cluster", nil) // test-creds (well-formed)
	hostB := hostCR("host-b", "libvirt-cluster", nil)
	hostB.Spec.CredentialSecretRef = &infravirtrigaudiov1beta1.LocalObjectReference{Name: "malformed-creds"}

	// malformed-creds exists but carries only known_hosts (no ssh-privatekey) and
	// a whitespace-only key must not count as present either.
	malformed := credSecret("malformed-creds", map[string][]byte{
		"known_hosts":    testHostKnownHosts,
		"ssh-privatekey": []byte("   \n"),
	})

	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov, hostA, hostB, credSecret("test-creds", sshCredData()), malformed).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	require.NoError(t, r.reconcileHostInventorySecret(context.Background(), prov))

	assert.Equal(t, []string{"host-a"}, renderedHostIDs(t, cli))
	cond := getConditionByType(t, prov.Status.Conditions, conditionHostCredentialsReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "host-b")
	assert.Contains(t, cond.Message, "ssh-privatekey")
	assertNoCredentialLeak(t, cond.Message)
}

// TestProvider_HostInventory_NoLeak is the core security assertion: across a
// render that inlines real credential material AND skips a host, NO byte of the
// key or known_hosts material (raw or base64) may appear in any captured log
// line or event. The material must reach ONLY the rendered Secret.
func TestProvider_HostInventory_NoLeak(t *testing.T) {
	sch := newProviderTLSScheme(t)
	prov := clusterProvider("libvirt-cluster")

	hostA := hostCR("host-a", "libvirt-cluster", nil) // renders with real material
	hostB := hostCR("host-b", "libvirt-cluster", nil)
	hostB.Spec.CredentialSecretRef = &infravirtrigaudiov1beta1.LocalObjectReference{Name: "missing-creds"} // skipped

	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov, hostA, hostB, credSecret("test-creds", sshCredData())).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	rec := record.NewFakeRecorder(10)
	r := &ProviderReconciler{Client: cli, Scheme: sch, Recorder: rec}

	// Capture ALL controller logs via a funcr sink injected through the log ctx.
	var logbuf bytes.Buffer
	logger := funcr.New(func(prefix, args string) {
		logbuf.WriteString(prefix)
		logbuf.WriteString(args)
		logbuf.WriteByte('\n')
	}, funcr.Options{Verbosity: 10})
	ctx := ctrllog.IntoContext(context.Background(), logr.New(logger.GetSink()))

	require.NoError(t, r.reconcileHostInventorySecret(ctx, prov))

	// Sanity: the material DID flow into the rendered Secret (base64 in hosts.json)
	// — so we are genuinely testing a path that carried it, not a no-op.
	secret := &corev1.Secret{}
	require.NoError(t, cli.Get(ctx,
		types.NamespacedName{Name: "libvirt-cluster-hosts", Namespace: "default"}, secret))
	keyB64 := base64.StdEncoding.EncodeToString(testHostSSHKey)
	assert.Contains(t, string(secret.Data[hostsecret.SecretDataKey]), keyB64,
		"the rendered Secret must carry the inlined key (base64) — otherwise the leak test is vacuous")

	// The material must appear in NEITHER logs NOR events, raw or base64.
	assertNoCredentialLeak(t, logbuf.String())
	for _, e := range drainEvents(rec) {
		assertNoCredentialLeak(t, e)
	}
	// And the skip signal for host-b is still present in the logs (proving we
	// captured the surfacing path, not an empty buffer).
	assert.Contains(t, logbuf.String(), "host-b")
}

// assertNoCredentialLeak fails if s contains any credential material — the raw
// key/known_hosts bytes or their base64 (JSON) encodings.
func assertNoCredentialLeak(t *testing.T, s string) {
	t.Helper()
	for _, secret := range [][]byte{testHostSSHKey, testHostKnownHosts} {
		assert.NotContains(t, s, string(secret), "raw credential material leaked")
		assert.NotContains(t, s, base64.StdEncoding.EncodeToString(secret), "base64 credential material leaked")
		// A distinctive interior token, in case of any partial/transformed emission.
		assert.False(t, strings.Contains(s, "DO-NOT-LOG"), "a credential token leaked")
	}
}
