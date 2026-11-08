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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/devgate"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

func TestParseHostEnrolmentConfig(t *testing.T) {
	cfg, err := ParseHostEnrolmentConfig(HostEnrolmentFlags{}, nil)
	require.NoError(t, err)
	assert.Nil(t, cfg, "no flag: enrolment disabled")

	ca := hostidentitytest.NewCA(t)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, ca.PEM, 0o600))
	clusterIssuer := HostEnrolmentFlags{
		Namespace: "host-agent-pki", Issuer: "ClusterIssuer/host-agent-ca", AdminGroup: "enrolment-admins",
		TrustDomain: "cluster.test",
	}
	full := HostEnrolmentFlags{
		Namespace: "host-agent-pki", Issuer: "Issuer/host-agent-ca", AdminGroup: "enrolment-admins",
		TrustDomain: "cluster.test", CABundleFile: caFile,
	}
	installID := bytes.Repeat([]byte{1}, 16)

	if !devgate.Compiled {
		_, err := ParseHostEnrolmentConfig(full, installID)
		require.ErrorIs(t, err, devgate.ErrNotCompiled, "a release build refuses every enrolment flag")
		return
	}
	cfg, err = ParseHostEnrolmentConfig(full, installID)
	require.NoError(t, err)
	assert.Equal(t, "cert-manager.io/Issuer/host-agent-ca", cfg.CertManagerIssuer.String())
	clusterIssuer.CABundleFile = caFile
	cfg, err = ParseHostEnrolmentConfig(clusterIssuer, installID)
	require.NoError(t, err, "a cert-manager ClusterIssuer is accepted (and recommended)")
	assert.Equal(t, certrequest.IssuerRef{Group: certrequest.Group, Kind: "ClusterIssuer", Name: "host-agent-ca"}, *cfg.CertManagerIssuer)
	assert.Nil(t, cfg.ExternalPKIIssuer)

	_, err = ParseHostEnrolmentConfig(full, nil)
	assert.Error(t, err, "the install id (development signer) is required")
	for name, mutate := range map[string]func(*HostEnrolmentFlags){
		"no namespace":                           func(f *HostEnrolmentFlags) { f.Namespace = "" },
		"no admin group":                         func(f *HostEnrolmentFlags) { f.AdminGroup = "" },
		"bad trust domain":                       func(f *HostEnrolmentFlags) { f.TrustDomain = "Not A Domain" },
		"no issuer":                              func(f *HostEnrolmentFlags) { f.Issuer = "" },
		"bad issuer":                             func(f *HostEnrolmentFlags) { f.Issuer = "just-a-name" },
		"external PKI in the cert-manager group": func(f *HostEnrolmentFlags) { f.ExternalPKIIssuer = "cert-manager.io/Issuer/external" },
		"external PKI without a group":           func(f *HostEnrolmentFlags) { f.ExternalPKIIssuer = "Issuer/external" },
		"no CA bundle":                           func(f *HostEnrolmentFlags) { f.CABundleFile = "" },
		"missing CA bundle":                      func(f *HostEnrolmentFlags) { f.CABundleFile = filepath.Join(t.TempDir(), "none") },
		"leaf as CA bundle":                      func(f *HostEnrolmentFlags) { f.CABundleFile = writeLeaf(t, ca) },
		"external PKI alone":                     nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := full
			if mutate == nil {
				f.Issuer, f.ExternalPKIIssuer = "", "external-pki.example.test/ExternalIssuer/enterprise"
				_, err := ParseHostEnrolmentConfig(f, installID)
				assert.NoError(t, err)
				return
			}
			mutate(&f)
			_, err := ParseHostEnrolmentConfig(f, installID)
			assert.Error(t, err)
		})
	}
}

// writeLeaf writes a non-CA certificate to a file and returns its path.
func writeLeaf(t *testing.T, ca *hostidentitytest.CA) string {
	t.Helper()
	csr := hostidentitytest.NewCSR(t, hostidentitytest.NewKey(t), hostidentitytest.CSROptions{URIs: []string{"spiffe://x/virtrigaud/host/a/b"}})
	p := filepath.Join(t.TempDir(), "leaf.pem")
	require.NoError(t, os.WriteFile(p, ca.Issue(t, csr, hostidentitytest.IssueOptions{}), 0o600))
	return p
}

// TestHostInventory_AgentHostsRenderPinnedAgentTLS: a Provider that runs
// per-host agents renders each enrolled, unrevoked host with agentTLS read
// from its pins Secret and no SSH material (development builds), and every
// other host as unroutable. A release build renders no agent host.
func TestHostInventory_AgentHostsRenderPinnedAgentTLS(t *testing.T) {
	ctx := context.Background()
	sch := newProviderTLSScheme(t)
	prov := clusterProvider("ch-cluster")
	prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor

	ca := hostidentitytest.NewCA(t)
	pin := strings.Repeat("ab", 32)
	mkHost := func(name, identity string, revoked bool) *infravirtrigaudiov1beta1.Host {
		h := hostCR(name, prov.Name, nil)
		h.UID = types.UID("uid-" + name)
		h.Spec.Endpoint = "grpc://" + name + ":7443"
		h.Spec.Identity = identity
		h.Spec.Revoked = revoked
		return h
	}
	enrolled := mkHost("host-enrolled", "serial-1", false)
	revoked := mkHost("host-revoked", "serial-2", true)
	pending := mkHost("host-pending", "serial-3", false)
	noIdentity := mkHost("host-noidentity", "", false)

	// The Provider's credential Secret carries SSH material; it must never
	// be copied into an agent host.
	cli := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(prov, enrolled, revoked, pending, noIdentity, credSecret("test-creds", sshCredData())).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).Build()
	cfg := &HostEnrolmentConfig{Namespace: "host-agent-pki", TrustDomain: "cluster.test", CABundle: ca.PEM}
	for _, h := range []*infravirtrigaudiov1beta1.Host{enrolled, revoked} {
		l := loadedEnrolment{state: hostEnrolmentState{HostUID: string(h.UID), Identity: h.Spec.Identity, Current: &currentEnrolment{
			Record:   enrolmentRecord{EnrolmentID: strings.Repeat("a1", 16), ProviderUID: "0b9a3f2e-5d1c-4f7a-9e6b-2c8d1a4b7e90", InstallID: strings.Repeat("42", 16)},
			KeyPins:  []string{pin},
			NotAfter: metav1.NewTime(time.Now().Add(time.Hour)),
		}}}
		require.NoError(t, saveHostEnrolment(ctx, cli, cfg, h, &l))
	}

	// Without the enrolment configuration nothing is rendered for agent hosts.
	r := &ProviderReconciler{Client: cli, Scheme: sch}
	require.NoError(t, r.reconcileHostInventorySecret(ctx, prov))
	r.HostEnrolment = cfg
	require.NoError(t, r.reconcileHostInventorySecret(ctx, prov))
	sec := &corev1.Secret{}
	require.NoError(t, cli.Get(ctx, types.NamespacedName{Namespace: prov.Namespace, Name: r.getHostInventorySecretName(prov)}, sec))
	inv, err := hostsecret.Unmarshal(sec.Data[hostsecret.SecretDataKey])
	require.NoError(t, err)

	if !devgate.Compiled {
		assert.Empty(t, inv.Hosts, "a release build renders no per-host agent host")
		assert.ElementsMatch(t, []string{"host-enrolled", "host-revoked", "host-pending", "host-noidentity"}, inv.UnroutableHostIDs)
		return
	}
	require.Len(t, inv.Hosts, 1)
	h := inv.Hosts[0]
	assert.Equal(t, "host-enrolled", h.ID)
	require.NotNil(t, h.Credentials.AgentTLS)
	assert.Equal(t, []string{pin}, h.Credentials.AgentTLS.KeyPins)
	assert.Equal(t, "spiffe://cluster.test/virtrigaud/host/default/host-enrolled", h.Credentials.AgentTLS.ExpectedSAN)
	assert.Equal(t, ca.PEM, h.Credentials.AgentTLS.CABundle)
	assert.Empty(t, h.Credentials.SSHPrivateKey, "an agent host never carries SSH material")
	assert.Empty(t, h.Credentials.KnownHosts)
	assert.ElementsMatch(t, []string{"host-revoked", "host-pending", "host-noidentity"}, inv.UnroutableHostIDs)
}

// TestLoadHostEnrolment_RefusesAForeignSecret: a Secret with a pins Secret
// name that VirtRigaud does not manage is never read as pins nor
// overwritten.
func TestLoadHostEnrolment_RefusesAForeignSecret(t *testing.T) {
	ctx := context.Background()
	sch := newProviderTLSScheme(t)
	h := hostCR("host-a", "ch", nil)
	h.UID = "uid-host-a"
	h.Spec.Identity = "serial-1"
	cfg := &HostEnrolmentConfig{Namespace: "host-agent-pki", TrustDomain: "cluster.test"}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: hostEnrolmentSecretName("default", "host-a"), Namespace: cfg.Namespace},
		Data: map[string][]byte{hostEnrolmentStateKey: []byte(`{}`)}}
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(h, foreign).Build()
	_, err := loadHostEnrolment(ctx, cli, cfg, h)
	require.ErrorIs(t, err, errForeignPinsSecret)
	_, reason := resolveHostAgentTLS(ctx, cli, cfg, h, time.Now())
	assert.NotEmpty(t, reason)
	require.NoError(t, deleteHostEnrolment(ctx, cli, cli, cfg, "default", "host-a", ""))
	require.NoError(t, cli.Get(ctx, client.ObjectKeyFromObject(foreign), &corev1.Secret{}), "a foreign Secret is never deleted")
}

// TestHostEnrolmentSecretName_IsBoundedAndPerHost: the pins Secret name is
// bounded for any Host name and differs per namespace/name.
func TestHostEnrolmentSecretName_IsBoundedAndPerHost(t *testing.T) {
	long := strings.Repeat("a", 253)
	assert.LessOrEqual(t, len(hostEnrolmentSecretName(long, long)), 63)
	assert.NotEqual(t, hostEnrolmentSecretName("a", "b-c"), hostEnrolmentSecretName("a-b", "c"))
	assert.NotEqual(t, hostEnrolmentSecretName("ns", "a"), hostEnrolmentSecretName("ns", "b"))
}

// TestCheckCertManagerAdmission: enrolment refuses to run unless fail-closed
// mutating (create) and validating (create, update) webhooks cover
// CertificateRequests in the enrolment namespace.
func TestCheckCertManagerAdmission(t *testing.T) {
	ctx := context.Background()
	sch := newProviderTLSScheme(t)
	require.NoError(t, admissionregistrationv1.AddToScheme(sch))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "host-agent-pki", Labels: map[string]string{"team": "pki"}}}
	fail, ignore := admissionregistrationv1.Fail, admissionregistrationv1.Ignore
	rule := func(ops ...admissionregistrationv1.OperationType) []admissionregistrationv1.RuleWithOperations {
		return []admissionregistrationv1.RuleWithOperations{{Operations: ops, Rule: admissionregistrationv1.Rule{
			APIGroups: []string{"cert-manager.io", "acme.cert-manager.io"}, APIVersions: []string{"v1"}, Resources: []string{"*/*"}}}}
	}
	mut := func(policy *admissionregistrationv1.FailurePolicyType, sel *metav1.LabelSelector) *admissionregistrationv1.MutatingWebhookConfiguration {
		return &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "cert-manager-webhook"},
			Webhooks: []admissionregistrationv1.MutatingWebhook{{Name: "webhook.cert-manager.io", FailurePolicy: policy, NamespaceSelector: sel,
				Rules: rule(admissionregistrationv1.Create)}}}
	}
	val := func(ops ...admissionregistrationv1.OperationType) *admissionregistrationv1.ValidatingWebhookConfiguration {
		return &admissionregistrationv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "cert-manager-webhook"},
			Webhooks: []admissionregistrationv1.ValidatingWebhook{{Name: "webhook.cert-manager.io", FailurePolicy: &fail,
				NamespaceSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: "cert-manager.io/disable-validation", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"true"}}}},
				Rules: rule(ops...)}}}
	}
	withMatchCondition := func(m *admissionregistrationv1.MutatingWebhookConfiguration) *admissionregistrationv1.MutatingWebhookConfiguration {
		m.Webhooks[0].MatchConditions = []admissionregistrationv1.MatchCondition{{Name: "skip-some", Expression: "true"}}
		return m
	}
	for name, tc := range map[string]struct {
		objs []client.Object
		ok   bool
	}{
		"cert-manager's webhooks": {objs: []client.Object{mut(nil, nil), val(admissionregistrationv1.Create, admissionregistrationv1.Update)}, ok: true},
		"mutating webhook with match conditions": {objs: []client.Object{withMatchCondition(mut(&fail, nil)),
			val(admissionregistrationv1.Create, admissionregistrationv1.Update)}},
		"no webhook at all":           {},
		"mutating webhook only":       {objs: []client.Object{mut(&fail, nil)}},
		"validating without update":   {objs: []client.Object{mut(&fail, nil), val(admissionregistrationv1.Create)}},
		"mutating webhook fails open": {objs: []client.Object{mut(&ignore, nil), val(admissionregistrationv1.Create, admissionregistrationv1.Update)}},
		"namespace excluded by selector": {objs: []client.Object{mut(&fail, &metav1.LabelSelector{MatchLabels: map[string]string{"team": "other"}}),
			val(admissionregistrationv1.Create, admissionregistrationv1.Update)}},
	} {
		t.Run(name, func(t *testing.T) {
			cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(append([]client.Object{ns}, tc.objs...)...).Build()
			err := CheckCertManagerAdmission(ctx, cli, ns.Name)
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, ErrNoCertManagerAdmission)
			}
		})
	}
}

// TestAdmissionGate: the gate follows the last check, and Start returns when
// its context is cancelled.
func TestAdmissionGate(t *testing.T) {
	ctx := context.Background()
	sch := newProviderTLSScheme(t)
	require.NoError(t, admissionregistrationv1.AddToScheme(sch))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "host-agent-pki"}}
	rules := []admissionregistrationv1.RuleWithOperations{{Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
		Rule: admissionregistrationv1.Rule{APIGroups: []string{"cert-manager.io"}, APIVersions: []string{"v1"}, Resources: []string{"*/*"}}}}
	mut := &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "m"},
		Webhooks: []admissionregistrationv1.MutatingWebhook{{Name: "m.cert-manager.io", Rules: rules}}}
	val := &admissionregistrationv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "v"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{Name: "v.cert-manager.io", Rules: rules}}}
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(ns, mut, val).Build()

	var nilGate *AdmissionGate
	assert.False(t, nilGate.OK())
	g := &AdmissionGate{Reader: cli, Namespace: ns.Name, Interval: time.Millisecond}
	assert.False(t, g.OK(), "closed until a check passes")
	require.NoError(t, g.Check(ctx))
	assert.True(t, g.OK())
	require.NoError(t, cli.Delete(ctx, val))
	require.ErrorIs(t, g.Check(ctx), ErrNoCertManagerAdmission)
	assert.False(t, g.OK(), "a failed re-check suspends approvals")

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- g.Start(runCtx) }()
	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after its context was cancelled")
	}
	assert.False(t, g.NeedLeaderElection())
}
