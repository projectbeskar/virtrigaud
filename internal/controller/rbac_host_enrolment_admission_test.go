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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/projectbeskar/virtrigaud/internal/agent/enrol"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
)

// hostEnrolmentAdmissionDir holds the optional admission samples of the
// enrolment RBAC.
var hostEnrolmentAdmissionDir = filepath.Join("..", "..", "config", "rbac", "host-enrolment", "admission")

// readAdmissionSample returns the documents of one admission sample.
func readAdmissionSample(t *testing.T, name string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(hostEnrolmentAdmissionDir, name)) // #nosec G304 -- a repository file
	require.NoError(t, err)
	return splitYAMLDocs(t, raw)
}

// TestManagerCertificateRequestPolicySample: the deny-only admission sample
// is a ValidatingAdmissionPolicy (strictly decoded) that fails closed,
// matches only CREATE of certificaterequests by the manager's
// ServiceAccount, and is bound with the Deny action only. Its expressions
// are exercised against a real API server in the enrolment envtest specs.
func TestManagerCertificateRequestPolicySample(t *testing.T) {
	docs := readAdmissionSample(t, "manager-certificaterequests.yaml")
	require.Len(t, docs, 2)
	vap := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	require.NoError(t, yaml.UnmarshalStrict(docs[0], vap))
	require.NotNil(t, vap.Spec.FailurePolicy)
	assert.Equal(t, admissionregistrationv1.Fail, *vap.Spec.FailurePolicy)
	require.Len(t, vap.Spec.MatchConstraints.ResourceRules, 1)
	rule := vap.Spec.MatchConstraints.ResourceRules[0]
	assert.Equal(t, []admissionregistrationv1.OperationType{admissionregistrationv1.Create}, rule.Operations)
	assert.Equal(t, []string{"certificaterequests"}, rule.Resources)
	assert.Equal(t, []string{"cert-manager.io"}, rule.APIGroups)
	require.Len(t, vap.Spec.MatchConditions, 1)
	assert.Contains(t, vap.Spec.MatchConditions[0].Expression,
		"'system:serviceaccount:virtrigaud-system:virtrigaud-controller-manager'")
	var all string
	for _, v := range vap.Spec.Validations {
		all += v.Expression + "\n"
		assert.NotEmpty(t, v.Message)
	}
	for _, want := range []string{"host-agent-pki", "host-agent-ca", "isCA", "'digital signature'", "'server auth'",
		"duration('720h')", certrequestRenewalLabel, certrequestHostAnnotation} {
		assert.Contains(t, all, want)
	}
	bind := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	require.NoError(t, yaml.UnmarshalStrict(docs[1], bind))
	assert.Equal(t, vap.Name, bind.Spec.PolicyName)
	assert.Equal(t, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}, bind.Spec.ValidationActions)
}

// The label and annotation names the policy checks.
const (
	certrequestRenewalLabel   = "enrolment.virtrigaud.io/renewal"
	certrequestHostAnnotation = "enrolment.virtrigaud.io/host"
)

// wildcardMatch reports whether s matches pattern, in which "*" stands for
// any run of characters (approver-policy's wildcard).
func wildcardMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// TestApproverPolicySampleAllowsAgentRequests: the approver-policy sample
// allows exactly what an agent's certificate request carries: the subject
// common name (the host's name, which every agent request sets), the host
// URI and the urn:virtrigaud: record URIs, no DNS, IP or e-mail name; the
// agent usages, no CA, ECDSA P-256, at most 30 days; and it is bound, for
// "use", to the manager's ServiceAccount only, in the enrolment namespace.
func TestApproverPolicySampleAllowsAgentRequests(t *testing.T) {
	docs := readAdmissionSample(t, "approver-policy.yaml")
	require.Len(t, docs, 3)
	p := &unstructured.Unstructured{}
	require.NoError(t, yaml.Unmarshal(docs[0], &p.Object))
	assert.Equal(t, "policy.cert-manager.io/v1alpha1", p.GetAPIVersion())
	assert.Equal(t, "CertificateRequestPolicy", p.GetKind())
	cn, _, _ := unstructured.NestedString(p.Object, "spec", "allowed", "commonName", "value")
	uris, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "allowed", "uris", "values")
	usages, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "allowed", "usages")
	isCA, found, _ := unstructured.NestedBool(p.Object, "spec", "allowed", "isCA")
	maxDuration, _, _ := unstructured.NestedString(p.Object, "spec", "constraints", "maxDuration")
	for _, k := range []string{"dnsNames", "ipAddresses", "emailAddresses", "subject"} {
		_, has, _ := unstructured.NestedFieldNoCopy(p.Object, "spec", "allowed", k)
		assert.False(t, has, "spec.allowed.%s must not be set: the agent's requests carry none", k)
	}
	assert.True(t, found)
	assert.False(t, isCA)
	assert.ElementsMatch(t, []string{hostidentity.UsageDigitalSignature, hostidentity.UsageServerAuth}, usages)
	assert.Equal(t, "720h", maxDuration)

	// An agent's real request (the code that builds it) satisfies the
	// allowed names.
	hostURI, err := hostidentity.HostURI("example.test", "infra", "kvm-01")
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	rec := hostidentity.Record{EnrolmentID: bytes16(0xab), ProviderUID: "0f5c2a70-3a4e-4c3c-9a7e-5d1b2c3d4e5f",
		InstallID: bytes16(0xcd), Mode: hostidentity.ModeIdentity}
	csrPEM, err := enrol.BuildCSR(key, hostidentity.RequestURIs(hostURI, rec))
	require.NoError(t, err)
	csr, err := hostidentity.ParseCSR(csrPEM)
	require.NoError(t, err)
	require.NotEmpty(t, csr.Subject.CommonName, "agent requests carry a common name")
	assert.True(t, wildcardMatch(cn, csr.Subject.CommonName), "common name %q not allowed by %q", csr.Subject.CommonName, cn)
	require.NotEmpty(t, csr.URIs)
	for _, u := range csr.URIs {
		ok := false
		for _, pat := range uris {
			ok = ok || wildcardMatch(pat, u.String())
		}
		assert.True(t, ok, "URI %s is not allowed by %v", u, uris)
	}
	assert.Empty(t, csr.DNSNames)
	assert.Empty(t, csr.IPAddresses)
	assert.Empty(t, csr.EmailAddresses)

	role := &rbacv1.Role{}
	require.NoError(t, yaml.UnmarshalStrict(docs[1], role))
	require.Len(t, role.Rules, 1)
	assert.Equal(t, []string{"use"}, role.Rules[0].Verbs)
	assert.Equal(t, []string{p.GetName()}, role.Rules[0].ResourceNames)
	binding := &rbacv1.RoleBinding{}
	require.NoError(t, yaml.UnmarshalStrict(docs[2], binding))
	assert.Equal(t, role.Namespace, binding.Namespace)
	require.Len(t, binding.Subjects, 1)
	assert.Equal(t, rbacv1.Subject{Kind: "ServiceAccount", Name: "virtrigaud-controller-manager", Namespace: "virtrigaud-system"}, binding.Subjects[0])
}

// bytes16 returns 16 bytes of b.
func bytes16(b byte) []byte {
	out := make([]byte, 16)
	for i := range out {
		out[i] = b
	}
	return out
}

// TestProviderIdentityApproverPolicySample: the approver-policy sample for
// the Provider identity certificate allows the documented Certificate's URI
// and nothing broader than that Provider's identity, the client usages, no
// CA, ECDSA P-256 and at most 30 days, for the Provider identity issuer in
// the Provider's namespace, bound for "use" to cert-manager's
// ServiceAccount only.
func TestProviderIdentityApproverPolicySample(t *testing.T) {
	docs := readAdmissionSample(t, "provider-identity-approver-policy.yaml")
	require.Len(t, docs, 3)
	p := &unstructured.Unstructured{}
	require.NoError(t, yaml.Unmarshal(docs[0], &p.Object))
	assert.Equal(t, "CertificateRequestPolicy", p.GetKind())
	uris, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "allowed", "uris", "values")
	usages, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "allowed", "usages")
	maxDuration, _, _ := unstructured.NestedString(p.Object, "spec", "constraints", "maxDuration")
	issuer, _, _ := unstructured.NestedString(p.Object, "spec", "selector", "issuerRef", "name")
	for _, k := range []string{"dnsNames", "ipAddresses", "emailAddresses", "commonName", "subject"} {
		_, has, _ := unstructured.NestedFieldNoCopy(p.Object, "spec", "allowed", k)
		assert.False(t, has, "spec.allowed.%s must not be set", k)
	}
	require.Len(t, uris, 1)
	assert.True(t, wildcardMatch(uris[0], "spiffe://example.test/virtrigaud/provider/infra/ch/0f5c2a70-3a4e-4c3c-9a7e-5d1b2c3d4e5f"))
	assert.False(t, wildcardMatch(uris[0], "spiffe://example.test/virtrigaud/provider/infra/other/0f5c2a70"), "another Provider")
	assert.False(t, wildcardMatch(uris[0], "spiffe://example.test/virtrigaud/host/infra/kvm-01"), "a host identity")
	assert.ElementsMatch(t, []string{"digital signature", "client auth"}, usages)
	assert.Equal(t, "720h", maxDuration)
	assert.Equal(t, "provider-identity-ca", issuer)

	// The documented Certificate is the shape the policy allows.
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "clustered-agent-provider-development.md"))
	require.NoError(t, err)
	assert.Contains(t, string(doc), "spiffe://example.test/virtrigaud/provider/infra/ch/<Provider UID>")
	assert.Contains(t, string(doc), `usages: ["digital signature", "client auth"]`)

	binding := &rbacv1.RoleBinding{}
	require.NoError(t, yaml.UnmarshalStrict(docs[2], binding))
	require.Len(t, binding.Subjects, 1)
	assert.Equal(t, rbacv1.Subject{Kind: "ServiceAccount", Name: "cert-manager", Namespace: "cert-manager"}, binding.Subjects[0])
	role := &rbacv1.Role{}
	require.NoError(t, yaml.UnmarshalStrict(docs[1], role))
	assert.Equal(t, []string{p.GetName()}, role.Rules[0].ResourceNames)
	assert.Equal(t, []string{"use"}, role.Rules[0].Verbs)
}
