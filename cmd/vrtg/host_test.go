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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/cmd/internal/envtestutil"
	"github.com/projectbeskar/virtrigaud/internal/controller"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

const (
	testIssuerNS    = "host-agent-pki"
	testAdminGroup  = "virtrigaud:host-enrolment-admins"
	testTrustDomain = "cluster.test"
)

// TestHostEnrollOfflineWritesAManifestFromFilesOnly: with --manifest-out the
// command reads only the CSR file and writes the CertificateRequest to a
// file; it never contacts a cluster (the kubeconfig does not exist).
func TestHostEnrollOfflineWritesAManifestFromFilesOnly(t *testing.T) {
	dir := t.TempDir()
	csr := hostidentitytest.NewCSR(t, hostidentitytest.NewKey(t), hostidentitytest.CSROptions{URIs: []string{"spiffe://cluster.test/virtrigaud/host/infra/host-a"}})
	csrFile := filepath.Join(dir, "host-a.csr")
	require.NoError(t, os.WriteFile(csrFile, csr, 0o600))
	out := filepath.Join(dir, "request.yaml")

	stdout, err := runVrtg(t, filepath.Join(dir, "no-kubeconfig"), "-n", "infra", "host", "enroll", "--host", "host-a",
		"--csr", csrFile, "--issuer-namespace", testIssuerNS, "--issuer", "Issuer/host-agent-ca", "--manifest-out", out)
	require.NoError(t, err)
	assert.Contains(t, stdout, out)

	data, err := os.ReadFile(out)
	require.NoError(t, err)
	obj := map[string]any{}
	require.NoError(t, yaml.Unmarshal(data, &obj))
	cr := &unstructured.Unstructured{Object: obj}
	assert.Equal(t, certrequest.GVK, cr.GroupVersionKind())
	assert.Equal(t, testIssuerNS, cr.GetNamespace())
	ns, name, ok := certrequest.HostOf(cr)
	assert.True(t, ok)
	assert.Equal(t, "infra/host-a", ns+"/"+name)
	got, err := certrequest.CSR(cr)
	require.NoError(t, err)
	assert.Equal(t, csr, got)
	in, err := certrequest.RequestOf(cr)
	require.NoError(t, err)
	assert.Equal(t, hostidentity.MaxLifetime, in.Duration)
	assert.ElementsMatch(t, []string{hostidentity.UsageDigitalSignature, hostidentity.UsageServerAuth}, in.Usages)
	assert.Empty(t, in.Username, "the requester is recorded by cert-manager's webhook, never by the client")

	// A CSR whose signature does not verify is refused before anything is
	// written.
	bad := hostidentitytest.NewCSR(t, hostidentitytest.NewKey(t), hostidentitytest.CSROptions{BreakSignature: true})
	require.NoError(t, os.WriteFile(csrFile, bad, 0o600))
	_, err = runVrtg(t, filepath.Join(dir, "no-kubeconfig"), "host", "enroll", "--host", "host-a", "--csr", csrFile,
		"--issuer-namespace", testIssuerNS, "--issuer", "Issuer/host-agent-ca", "--manifest-out", filepath.Join(dir, "bad.yaml"))
	require.Error(t, err)
	assert.NoFileExists(t, filepath.Join(dir, "bad.yaml"))

	_, err = runVrtg(t, filepath.Join(dir, "no-kubeconfig"), "host", "enroll", "--host", "host-a", "--csr", csrFile,
		"--issuer-namespace", testIssuerNS, "--issuer", "Issuer/host-agent-ca", "--duration", "800h", "--manifest-out", out)
	assert.Error(t, err, "a lifetime over thirty days is refused locally")
}

// TestHostEnrollFileHandling: the CSR must be a bounded regular file, and
// an output is never written through a symbolic link and always ends up
// mode 0600.
func TestHostEnrollFileHandling(t *testing.T) {
	dir := t.TempDir()
	offline := filepath.Join(dir, "no-kubeconfig")
	args := func(csr, out string) []string {
		return []string{"host", "enroll", "--host", "host-a", "--csr", csr, "--issuer-namespace", testIssuerNS,
			"--issuer", "Issuer/host-agent-ca", "--manifest-out", out}
	}
	if _, err := os.Stat("/dev/zero"); err == nil {
		_, err := runVrtg(t, offline, args("/dev/zero", filepath.Join(dir, "x.yaml"))...)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a regular file")
	}
	big := filepath.Join(dir, "big.csr")
	require.NoError(t, os.WriteFile(big, bytes.Repeat([]byte("a"), maxEnrolFileSize+1), 0o600))
	_, err := runVrtg(t, offline, args(big, filepath.Join(dir, "x.yaml"))...)
	require.Error(t, err)

	csrFile := filepath.Join(dir, "host-a.csr")
	require.NoError(t, os.WriteFile(csrFile, hostidentitytest.NewCSR(t, hostidentitytest.NewKey(t), hostidentitytest.CSROptions{}), 0o600))
	victim := filepath.Join(dir, "victim")
	require.NoError(t, os.WriteFile(victim, []byte("keep"), 0o600))
	link := filepath.Join(dir, "link.yaml")
	require.NoError(t, os.Symlink(victim, link))
	_, err = runVrtg(t, offline, args(csrFile, link)...)
	require.Error(t, err, "a symbolic link is never written through")
	data, err := os.ReadFile(victim)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(data))

	existing := filepath.Join(dir, "existing.yaml")
	require.NoError(t, os.WriteFile(existing, []byte("old"), 0o644)) // #nosec G306 -- the test checks the mode is tightened
	_, err = runVrtg(t, offline, args(csrFile, existing)...)
	require.NoError(t, err)
	st, err := os.Stat(existing)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
}

// TestHostEnrolAndRevokeAgainstAPIServer runs the offline flow end to end
// against a real apiserver: the manager's enrolment reconcilers draw the
// record and decide the request, the test plays the agent (key and CSR
// file), cert-manager (requester fields, signing) and an administrator
// (files only, then kubectl create), and the CLI drives the external-PKI
// path and revocation.
func TestHostEnrolAndRevokeAgainstAPIServer(t *testing.T) {
	env := envtestutil.Start(t, filepath.Join("test", "crds", "cert-manager"))
	ctx := context.Background()
	for _, ns := range []string{"infra", testIssuerNS} {
		require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	}
	prov := &infrav1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "infra"},
		Spec: infrav1beta1.ProviderSpec{
			Type: infrav1beta1.ProviderTypeCloudHypervisor, Topology: infrav1beta1.ProviderTopologyCluster,
			Endpoint: "grpc://ch:9443", CredentialSecretRef: infrav1beta1.ObjectRef{Name: "creds"},
			Runtime: &infrav1beta1.ProviderRuntimeSpec{Mode: infrav1beta1.RuntimeModeRemote, Image: "virtrigaud/provider-ch:test"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, prov))
	for _, name := range []string{"host-a", "host-b"} {
		require.NoError(t, env.Client.Create(ctx, &infrav1beta1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "infra"},
			Spec: infrav1beta1.HostSpec{
				ProviderRef: infrav1beta1.LocalObjectReference{Name: "ch"}, PoolRef: infrav1beta1.LocalObjectReference{Name: "pool"},
				Endpoint: "grpc://" + name + ":7443", Identity: "serial-" + name, Schedulable: true,
			},
		}))
	}

	ca := hostidentitytest.NewCA(t)
	cm := certrequest.IssuerRef{Group: certrequest.Group, Kind: "Issuer", Name: "host-agent-ca"}
	ext := certrequest.IssuerRef{Group: "external-pki.example.test", Kind: "ExternalIssuer", Name: "enterprise"}
	cfg := &controller.HostEnrolmentConfig{
		Namespace: testIssuerNS, CertManagerIssuer: &cm, ExternalPKIIssuer: &ext, AdminGroup: testAdminGroup,
		TrustDomain: testTrustDomain, CABundle: ca.PEM, InstallID: bytes.Repeat([]byte{7}, 16),
	}
	hostR := &controller.HostEnrolmentReconciler{Client: env.Client, Config: cfg}
	reqR := &controller.HostEnrolmentRequestReconciler{Client: env.Client, Config: cfg}
	// The administrator "alice" may enrol the Hosts in "infra".
	require.NoError(t, env.Client.Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "host-enrolers", Namespace: "infra"},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{infrav1beta1.GroupVersion.Group}, Resources: []string{"hosts"},
			Verbs: []string{infrav1beta1.HostEnrolVerb}}},
	}))
	require.NoError(t, env.Client.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "host-enrolers", Namespace: "infra"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "host-enrolers"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "alice"}},
	}))
	getHost := func(name string) *infrav1beta1.Host {
		_, err := hostR.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "infra", Name: name}})
		require.NoError(t, err)
		h := &infrav1beta1.Host{}
		require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "infra", Name: name}, h))
		return h
	}
	// createAsAdmin creates a manifest file as an administrator would
	// (kubectl create -f), with the requester fields cert-manager's webhook
	// records.
	createAsAdmin := func(path string) *unstructured.Unstructured {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		obj := map[string]any{}
		require.NoError(t, yaml.Unmarshal(data, &obj))
		cr := &unstructured.Unstructured{Object: obj}
		spec, ok := cr.Object["spec"].(map[string]any)
		require.True(t, ok)
		spec["username"] = "alice"
		spec["groups"] = []any{"system:authenticated", testAdminGroup}
		require.NoError(t, env.Client.Create(ctx, cr))
		return cr
	}
	reconcileCR := func(cr *unstructured.Unstructured) *unstructured.Unstructured {
		_, err := reqR.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
		require.NoError(t, err)
		got := certrequest.NewObject()
		require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(cr), got))
		return got
	}
	dir := t.TempDir()
	offline := filepath.Join(dir, "no-kubeconfig")

	// cert-manager path, offline: the agent's CSR file and the request
	// manifest are files only.
	hostA := getHost("host-a")
	keyA := hostidentitytest.NewKey(t)
	csrA := hostidentitytest.NewCSR(t, keyA, hostidentitytest.CSROptions{URIs: hostA.Status.Agent.RequestURIs})
	csrFile := filepath.Join(dir, "host-a.csr")
	require.NoError(t, os.WriteFile(csrFile, csrA, 0o600))
	manifest := filepath.Join(dir, "host-a-request.yaml")
	_, err := runVrtg(t, offline, "-n", "infra", "host", "enroll", "--host", "host-a", "--csr", csrFile,
		"--issuer-namespace", testIssuerNS, "--issuer", "Issuer/host-agent-ca", "--manifest-out", manifest)
	require.NoError(t, err)
	cr := reconcileCR(createAsAdmin(manifest))
	require.True(t, certrequest.IsTrue(cr, certrequest.ConditionApproved), "the manager approves a well-formed offline request")
	require.NoError(t, certrequest.SetCertificate(cr, ca.Issue(t, csrA, hostidentitytest.IssueOptions{}), ca.PEM))
	require.NoError(t, certrequest.SetCondition(cr, certrequest.ConditionReady, certrequest.StatusTrue, certrequest.ReasonIssued, "issued", time.Now()))
	require.NoError(t, env.Client.Status().Update(ctx, cr))
	reconcileCR(cr)
	pinA, err := hostidentity.SPKIPin(&keyA.PublicKey)
	require.NoError(t, err)
	assert.Equal(t, []string{pinA}, getHost("host-a").Status.Agent.KeyPins)

	// The online submit refuses a CSR that does not carry the Host's
	// request URIs (here: host-a's CSR for host-b).
	_, err = runVrtg(t, env.KubeconfigPath, "-n", "infra", "host", "enroll", "--host", "host-b", "--csr", csrFile,
		"--issuer-namespace", testIssuerNS, "--issuer", "external-pki.example.test/ExternalIssuer/enterprise")
	require.Error(t, err)

	// External-PKI path through the CLI: submit, bundle, return.
	hostB := getHost("host-b")
	keyB := hostidentitytest.NewKey(t)
	csrB := hostidentitytest.NewCSR(t, keyB, hostidentitytest.CSROptions{URIs: hostB.Status.Agent.RequestURIs})
	csrFileB := filepath.Join(dir, "host-b.csr")
	require.NoError(t, os.WriteFile(csrFileB, csrB, 0o600))
	manifestB := filepath.Join(dir, "host-b-request.yaml")
	_, err = runVrtg(t, offline, "-n", "infra", "host", "enroll", "--host", "host-b", "--csr", csrFileB,
		"--issuer-namespace", testIssuerNS, "--issuer", "external-pki.example.test/ExternalIssuer/enterprise", "--manifest-out", manifestB)
	require.NoError(t, err)
	crB := createAsAdmin(manifestB)

	bundleFile := filepath.Join(dir, "bundle.json")
	_, err = runVrtg(t, env.KubeconfigPath, "-n", "infra", "host", "enroll", "--host", "host-b", "--issuer-namespace", testIssuerNS,
		"--request", crB.GetName(), "--bundle-out", bundleFile)
	require.Error(t, err, "no bundle for a request the manager has not approved")

	crB = reconcileCR(crB)
	require.True(t, certrequest.IsTrue(crB, certrequest.ConditionApproved))
	_, err = runVrtg(t, env.KubeconfigPath, "-n", "infra", "host", "enroll", "--host", "host-b", "--issuer-namespace", testIssuerNS,
		"--request", crB.GetName(), "--bundle-out", bundleFile)
	require.NoError(t, err)
	var bundle certrequest.Bundle
	data, err := os.ReadFile(bundleFile)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &bundle))
	assert.Equal(t, string(csrB), bundle.CSR)
	assert.ElementsMatch(t, hostB.Status.Agent.RequestURIs, bundle.SubjectAltNameURIs)
	assert.Equal(t, "720h0m0s", bundle.MaxLifetime)
	assert.False(t, bundle.CA)
	assert.Equal(t, []string{hostidentity.UsageServerAuth}, bundle.ExtendedKeyUsages)
	assert.Equal(t, []string{hostidentity.UsageDigitalSignature}, bundle.KeyUsages)
	assert.ElementsMatch(t, []string{hostidentity.UsageCertSign, hostidentity.UsageCRLSign}, bundle.ForbiddenKeyUsages)

	certFile := filepath.Join(dir, "host-b.pem")
	require.NoError(t, os.WriteFile(certFile, ca.Issue(t, csrB, hostidentitytest.IssueOptions{}), 0o600))
	_, err = runVrtg(t, env.KubeconfigPath, "-n", "infra", "host", "enroll", "--host", "host-b", "--issuer-namespace", testIssuerNS,
		"--request", crB.GetName(), "--certificate", certFile)
	require.NoError(t, err)
	crB = reconcileCR(crB)
	assert.True(t, certrequest.IsTrue(crB, certrequest.ConditionReady))
	pinB, err := hostidentity.SPKIPin(&keyB.PublicKey)
	require.NoError(t, err)
	assert.Equal(t, []string{pinB}, getHost("host-b").Status.Agent.KeyPins)

	// Revocation.
	stdout, err := runVrtg(t, env.KubeconfigPath, "-n", "infra", "host", "revoke", "--host", "host-a")
	require.NoError(t, err)
	assert.Contains(t, stdout, "revoked")
	h := getHost("host-a")
	assert.True(t, h.Spec.Revoked)
	assert.Equal(t, infrav1beta1.HostReasonAgentRevoked,
		findCondition(h.Status.Conditions, infrav1beta1.HostConditionAgentEnrolled).Reason)
}

// findCondition returns the condition of a type, or an empty one.
func findCondition(conds []metav1.Condition, t string) metav1.Condition {
	for _, c := range conds {
		if c.Type == t {
			return c
		}
	}
	return metav1.Condition{}
}
