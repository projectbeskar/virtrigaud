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
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// hostEnrolmentNamespacedSecrets are the chart values of the namespaced
// Secret grant that per-host agent enrolment requires.
var hostEnrolmentNamespacedSecrets = []string{"--set", "rbac.secrets.scope=namespaced",
	"--set", "rbac.secrets.providerNamespaces={virt-a}", "--set", "rbac.secrets.workloadNamespaces={tenant-1}"}

// hostEnrolmentCAKey is the chart value naming the sub-CA key Secret, which a
// cert-manager signer requires.
var hostEnrolmentCAKey = []string{"--set", "experimental.hostEnrolment.caKeySecret=cert-manager/host-agent-ca"}

// assertHostEnrolmentRBAC checks a rendered set of enrolment RBAC objects:
// approve on exact signers only, CertificateRequest access in one namespace
// through a Role, no Secret access and no wildcard anywhere.
func assertHostEnrolmentRBAC(t *testing.T, docs [][]byte, wantNS string, wantSigners []string) {
	t.Helper()
	var sawClusterRole, sawRole bool
	for _, doc := range docs {
		u := &unstructured.Unstructured{}
		require.NoError(t, yaml.Unmarshal(doc, &u.Object))
		switch u.GetKind() {
		case "ClusterRole":
			sawClusterRole = true
			cr := &rbacv1.ClusterRole{}
			require.NoError(t, yaml.Unmarshal(doc, cr))
			require.Len(t, cr.Rules, 3, "the ClusterRole grants three rules")
			r := cr.Rules[0]
			assert.Equal(t, []string{"cert-manager.io"}, r.APIGroups)
			assert.Equal(t, []string{"signers"}, r.Resources)
			assert.Equal(t, []string{"approve"}, r.Verbs)
			assert.Equal(t, wantSigners, r.ResourceNames, "approve is limited to the named signers")
			assert.Equal(t, []string{"authorization.k8s.io"}, cr.Rules[1].APIGroups)
			assert.Equal(t, []string{"subjectaccessreviews"}, cr.Rules[1].Resources)
			assert.Equal(t, []string{"create"}, cr.Rules[1].Verbs)
			assert.Equal(t, []string{"admissionregistration.k8s.io"}, cr.Rules[2].APIGroups)
			assert.ElementsMatch(t, []string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"}, cr.Rules[2].Resources)
			assert.ElementsMatch(t, []string{"get", "list"}, cr.Rules[2].Verbs)
			for _, rule := range cr.Rules {
				assert.NotContains(t, rule.Resources, "secrets", "no cluster-wide Secret access")
			}
		case "Role":
			sawRole = true
			role := &rbacv1.Role{}
			require.NoError(t, yaml.Unmarshal(doc, role))
			assert.Equal(t, wantNS, role.Namespace)
			require.Len(t, role.Rules, 3)
			assert.Equal(t, []string{"certificaterequests"}, role.Rules[0].Resources)
			assert.ElementsMatch(t, []string{"create", "delete", "get", "list", "watch"}, role.Rules[0].Verbs,
				"create and delete for the manager's own renewal requests; no update of the spec")
			assert.Equal(t, []string{"certificaterequests/status"}, role.Rules[1].Resources)
			assert.Equal(t, []string{"update"}, role.Rules[1].Verbs)
			assert.Equal(t, []string{"secrets"}, role.Rules[2].Resources, "pins Secrets, in the enrolment namespace only")
			assert.ElementsMatch(t, []string{"create", "delete", "get", "list", "update", "watch"}, role.Rules[2].Verbs)
		case "RoleBinding":
			assert.Equal(t, wantNS, u.GetNamespace())
		}
		// No wildcard in any object.
		for _, r := range rulesOf(t, doc) {
			for _, list := range [][]string{r.APIGroups, r.Resources, r.Verbs, r.ResourceNames} {
				assert.NotContains(t, list, "*")
			}
		}
	}
	assert.True(t, sawClusterRole && sawRole)
}

// rulesOf returns the rules of a Role or ClusterRole document (nil for
// other kinds).
func rulesOf(t *testing.T, doc []byte) []rbacv1.PolicyRule {
	t.Helper()
	var r struct {
		Rules []rbacv1.PolicyRule `json:"rules"`
	}
	require.NoError(t, yaml.Unmarshal(doc, &r))
	return r.Rules
}

// splitYAMLDocs splits a multi-document YAML stream.
func splitYAMLDocs(t *testing.T, raw []byte) [][]byte {
	t.Helper()
	var docs [][]byte
	rd := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		doc, err := rd.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if len(bytes.TrimSpace(doc)) > 0 && !bytes.Equal(bytes.TrimSpace(doc), []byte("---")) {
			u := map[string]any{}
			require.NoError(t, yaml.Unmarshal(doc, &u))
			if len(u) > 0 {
				docs = append(docs, doc)
			}
		}
	}
	return docs
}

func TestRBAC_HostEnrolmentKustomizeManifests(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "host-enrolment", "role.yaml"))
	require.NoError(t, err)
	assertHostEnrolmentRBAC(t, splitYAMLDocs(t, raw), "host-agent-pki", []string{"clusterissuers.cert-manager.io/host-agent-ca"})

	// Not part of the default RBAC set.
	k, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "kustomization.yaml"))
	require.NoError(t, err)
	assert.NotContains(t, string(k), "host-enrolment")
}

func TestRBAC_HostEnrolmentChartTemplate(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not installed")
	}
	chart := filepath.Join("..", "..", "charts", "virtrigaud")

	off, err := exec.Command(helm, "template", "t", chart).CombinedOutput() // #nosec G204 -- test-controlled arguments
	require.NoError(t, err, string(off))
	assert.NotContains(t, string(off), "host-enrolment", "off by default")

	out, err := exec.Command(helm, append([]string{"template", "t", chart, "--show-only", "templates/host-enrolment-rbac.yaml", // #nosec G204 -- test-controlled arguments
		"--set", "experimental.hostEnrolment.enabled=true",
		"--set", "experimental.hostEnrolment.developmentBuild=true",
		"--set", "experimental.hostEnrolment.namespace=host-agent-pki",
		"--set", "experimental.hostEnrolment.signerNames={clusterissuers.cert-manager.io/host-agent-ca}"},
		append(hostEnrolmentCAKey, hostEnrolmentNamespacedSecrets...)...)...).CombinedOutput()
	require.NoError(t, err, string(out))
	assertHostEnrolmentRBAC(t, splitYAMLDocs(t, out), "host-agent-pki", []string{"clusterissuers.cert-manager.io/host-agent-ca"})

	noDev, err := exec.Command(helm, "template", "t", chart, // #nosec G204 -- test-controlled arguments
		"--set", "rbac.secrets.scope=namespaced", "--set", "rbac.secrets.providerNamespaces={virt-a}",
		"--set", "experimental.hostEnrolment.enabled=true",
		"--set", "experimental.hostEnrolment.namespace=host-agent-pki",
		"--set", "experimental.hostEnrolment.signerNames={issuers.cert-manager.io/host-agent-pki.host-agent-ca}").CombinedOutput()
	require.Error(t, err, "rendering needs the explicit development-build statement")
	assert.Contains(t, string(noDev), "developmentBuild")

	for name, args := range map[string][]string{
		"wildcard signer": {"--set", "experimental.hostEnrolment.namespace=x", "--set", "experimental.hostEnrolment.signerNames={issuers.cert-manager.io/*}"},
		"no signer":       {"--set", "experimental.hostEnrolment.namespace=x"},
		"no namespace":    {"--set", "experimental.hostEnrolment.signerNames={issuers.cert-manager.io/x.y}"},
	} {
		t.Run(name, func(t *testing.T) {
			all := append(append(append([]string{"template", "t", chart, "--set", "experimental.hostEnrolment.enabled=true",
				"--set", "experimental.hostEnrolment.developmentBuild=true"}, hostEnrolmentCAKey...), hostEnrolmentNamespacedSecrets...), args...)
			out, err := exec.Command(helm, all...).CombinedOutput() // #nosec G204 -- test-controlled arguments
			assert.Error(t, err, string(out))
		})
	}
}

// TestRBAC_HostEnrolmentChartNeedsNamespacedSecretGrant: the enrolment RBAC
// renders only with the namespaced Secret grant, never with the enrolment
// namespace or the declared sub-CA key Secret's namespace among the
// namespaces that grant the manager Secret access.
func TestRBAC_HostEnrolmentChartNeedsNamespacedSecretGrant(t *testing.T) {
	enrolment := []string{"--set", "experimental.hostEnrolment.enabled=true",
		"--set", "experimental.hostEnrolment.developmentBuild=true",
		"--set", "experimental.hostEnrolment.namespace=host-agent-pki",
		"--set", "experimental.hostEnrolment.signerNames={clusterissuers.cert-manager.io/host-agent-ca}",
		"--set", "experimental.hostEnrolment.caKeySecret=cert-manager/host-agent-ca"}
	render := func(t *testing.T, args ...string) (string, error) {
		t.Helper()
		return secretRBACRender(t, append(append([]string{}, enrolment...), args...)...)
	}

	t.Run("accepted", func(t *testing.T) {
		for name, args := range map[string][]string{
			"namespaced grant": hostEnrolmentNamespacedSecrets,
			"unrelated additional rule": append([]string{"--set-json",
				`rbac.additionalRules=[{"apiGroups":[""],"resources":["configmaps"],"verbs":["get"]}]`}, hostEnrolmentNamespacedSecrets...),
			"external issuer in a cert-manager.io sub-group, no sub-CA key": append([]string{"--set", "experimental.hostEnrolment.caKeySecret=",
				"--set", "experimental.hostEnrolment.signerNames={awspcaclusterissuers.awspca.cert-manager.io/ca}"},
				hostEnrolmentNamespacedSecrets...),
			"external PKI signer only, no sub-CA key": append([]string{"--set", "experimental.hostEnrolment.caKeySecret=",
				"--set", "experimental.hostEnrolment.signerNames={externalissuers.pki.example.test/host-agent-pki.enterprise}"},
				hostEnrolmentNamespacedSecrets...),
			"namespaced grant with a sub-CA key":       append([]string{"--set", "experimental.hostEnrolment.caKeySecret=cert-manager/host-agent-ca"}, hostEnrolmentNamespacedSecrets...),
			"sub-CA key name with dots":                append([]string{"--set", "experimental.hostEnrolment.caKeySecret=cert-manager/host-agent.ca-key"}, hostEnrolmentNamespacedSecrets...),
			"namespaced grant, release-namespace RBAC": append([]string{"--set", "rbac.scope=namespace"}, hostEnrolmentNamespacedSecrets...),
		} {
			t.Run(name, func(t *testing.T) {
				out, err := render(t, args...)
				require.NoError(t, err, out)
				grants, _ := renderedSecretGrants(t, out)
				for g := range grants {
					assert.NotContains(t, g, "cert-manager/", "no Secret grant in the sub-CA key namespace")
					assert.NotContains(t, g, "ClusterRole", "no cluster-wide Secret grant")
				}
				assert.Contains(t, grants, "Role host-agent-pki/rel-virtrigaud-host-enrolment")
			})
		}
	})

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"default cluster scope": {nil, "needs rbac.secrets.scope=namespaced"},
		"explicit cluster scope": {[]string{"--set", "rbac.secrets.scope=cluster"},
			"the sub-CA key must not be readable by the manager"},
		"enrolment namespace is a Provider namespace": {[]string{"--set", "rbac.secrets.scope=namespaced",
			"--set", "rbac.secrets.providerNamespaces={virt-a,host-agent-pki}"},
			`experimental.hostEnrolment.namespace "host-agent-pki" must not be listed`},
		"enrolment namespace is a workload namespace": {[]string{"--set", "rbac.secrets.scope=namespaced",
			"--set", "rbac.secrets.providerNamespaces={virt-a}", "--set", "rbac.secrets.workloadNamespaces={host-agent-pki}"},
			`experimental.hostEnrolment.namespace "host-agent-pki" must not be listed`},
		"sub-CA key in the enrolment namespace": {append([]string{"--set", "experimental.hostEnrolment.caKeySecret=host-agent-pki/ca-key"},
			hostEnrolmentNamespacedSecrets...), "must not be in the enrolment namespace"},
		"sub-CA key in a Provider namespace": {append([]string{"--set", "experimental.hostEnrolment.caKeySecret=virt-a/ca-key"},
			hostEnrolmentNamespacedSecrets...), `namespace "virt-a" is listed`},
		"sub-CA key in a workload namespace": {append([]string{"--set", "experimental.hostEnrolment.caKeySecret=tenant-1/ca-key"},
			hostEnrolmentNamespacedSecrets...), `namespace "tenant-1" is listed`},
		"no sub-CA key with a cert-manager signer": {append([]string{"--set", "experimental.hostEnrolment.caKeySecret="},
			hostEnrolmentNamespacedSecrets...), "experimental.hostEnrolment.caKeySecret is required with a cert-manager signer"},
		"no sub-CA key with a namespaced cert-manager issuer": {append([]string{"--set", "experimental.hostEnrolment.caKeySecret=",
			"--set", "experimental.hostEnrolment.signerNames={issuers.cert-manager.io/host-agent-pki.ca}"},
			hostEnrolmentNamespacedSecrets...), "experimental.hostEnrolment.caKeySecret is required"},
		"additional rule on Secrets": {append([]string{"--set-json",
			`rbac.additionalRules=[{"apiGroups":[""],"resources":["configmaps"],"verbs":["get"]},{"apiGroups":[""],"resources":["secrets"],"verbs":["get"]}]`},
			hostEnrolmentNamespacedSecrets...), "rbac.additionalRules[1] grants Secret access"},
		"additional rule on every core resource": {append([]string{"--set-json",
			`rbac.additionalRules=[{"apiGroups":[""],"resources":["*"],"verbs":["list"]}]`},
			hostEnrolmentNamespacedSecrets...), "rbac.additionalRules[0] grants Secret access"},
		"additional rule on every group": {append([]string{"--set-json",
			`rbac.additionalRules=[{"apiGroups":["*"],"resources":["secrets"],"verbs":["watch"]}]`},
			hostEnrolmentNamespacedSecrets...), "rbac.additionalRules[0] grants Secret access"},
		"additional rule with a null resource": {append([]string{"--set-json",
			`rbac.additionalRules=[{"apiGroups":[""],"resources":[null],"verbs":["get"]}]`},
			hostEnrolmentNamespacedSecrets...), "rbac.additionalRules[0]: apiGroups and resources must hold only strings"},
		"additional rule with a null group": {append([]string{"--set-json",
			`rbac.additionalRules=[{"apiGroups":[null],"resources":["secrets"],"verbs":["get"]}]`},
			hostEnrolmentNamespacedSecrets...), "rbac.additionalRules[0]: apiGroups and resources must hold only strings"},
		"additional rule with a numeric resource": {append([]string{"--set-json",
			`rbac.additionalRules=[{"apiGroups":[""],"resources":[1],"verbs":["get"]}]`},
			hostEnrolmentNamespacedSecrets...), "rbac.additionalRules[0]: apiGroups and resources must hold only strings"},
		"additional rule with a string for resources": {append([]string{"--set-json",
			`rbac.additionalRules=[{"apiGroups":[""],"resources":"secrets","verbs":["get"]}]`},
			hostEnrolmentNamespacedSecrets...), "rbac.additionalRules[0]: apiGroups and resources must be lists"},
		"additional rule that is not a rule": {append([]string{"--set-json", `rbac.additionalRules=["secrets"]`},
			hostEnrolmentNamespacedSecrets...), "rbac.additionalRules[0] must be a rule"},
		"bare cert-manager.io signer": {append([]string{"--set", "experimental.hostEnrolment.signerNames={cert-manager.io/ca}"},
			hostEnrolmentNamespacedSecrets...), "names no resource"},
		"signer in upper case": {append([]string{"--set", "experimental.hostEnrolment.caKeySecret=",
			"--set", "experimental.hostEnrolment.signerNames={ClusterIssuers.Cert-Manager.io/host-agent-ca}"},
			hostEnrolmentNamespacedSecrets...), "must start with a lowercase"},
		"signer with a trailing dot": {append([]string{"--set", "experimental.hostEnrolment.caKeySecret=",
			"--set", "experimental.hostEnrolment.signerNames={clusterissuers.cert-manager.io./host-agent-ca}"},
			hostEnrolmentNamespacedSecrets...), "must start with a lowercase"},
		"no sub-CA key with another cert-manager.io signer": {append([]string{"--set", "experimental.hostEnrolment.caKeySecret=",
			"--set", "experimental.hostEnrolment.signerNames={otherissuers.cert-manager.io/host-agent-ca}"},
			hostEnrolmentNamespacedSecrets...), "experimental.hostEnrolment.caKeySecret is required"},
		"sub-CA key without a namespace": {append([]string{"--set", "experimental.hostEnrolment.caKeySecret=ca-key"},
			hostEnrolmentNamespacedSecrets...), "must be <namespace>/<name>"},
		"sub-CA key with an extra segment": {append([]string{"--set", "experimental.hostEnrolment.caKeySecret=a/b/c"},
			hostEnrolmentNamespacedSecrets...), "must be <namespace>/<name>"},
		"sub-CA key with an invalid namespace": {append([]string{"--set", "experimental.hostEnrolment.caKeySecret=Cert_Manager/ca"},
			hostEnrolmentNamespacedSecrets...), "must be <namespace>/<name>"},
		"sub-CA key with an empty name": {append([]string{"--set", "experimental.hostEnrolment.caKeySecret=cert-manager/"},
			hostEnrolmentNamespacedSecrets...), "must be <namespace>/<name>"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := render(t, tc.args...)
			require.Error(t, err, out)
			assert.Contains(t, out, tc.want)
		})
	}

	// With rbac.create=false the chart renders no enrolment RBAC and checks
	// nothing: the manager's startup check holds whoever grants it RBAC.
	out, err := render(t, "--set", "rbac.create=false")
	require.NoError(t, err, out)
	assert.NotContains(t, out, "host-enrolment")
}
