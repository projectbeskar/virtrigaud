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
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// These tests pin the manager's Secret grant (docs/manager-secret-access.md):
// the cluster-wide default generated from the kubebuilder markers and rendered
// by the chart, and the opt-in rbac.secrets.scope=namespaced mode, where no
// ClusterRole grants Secrets and each listed namespace gets its own Role.

// secretRBACAllVerbs is the manager's full Secret verb set: get, list, watch
// (cache), create, update (host-inventory and operation-intent Secrets) and
// delete (operation-intent Secrets). Never patch or deletecollection.
var secretRBACAllVerbs = []string{"create", "delete", "get", "list", "update", "watch"}

func TestRBAC_SecretMarkers(t *testing.T) {
	for file, markers := range map[string][]string{
		"virtualmachine_controller.go": {
			`// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch`,
			`// +kubebuilder:rbac:groups="",resources=secrets,verbs=delete`,
		},
		"provider_hostinventory.go": {`// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update`},
		"vmmigration_controller.go": {`//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch`},
	} {
		src, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, m := range markers {
			assert.Contains(t, string(src), m, file)
		}
	}
}

func TestRBAC_GeneratedRoleSecretVerbs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	require.NoError(t, err)
	role := &rbacv1.ClusterRole{}
	require.NoError(t, utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096).Decode(role))
	assert.Equal(t, [][]string{secretRBACAllVerbs}, secretRuleVerbs(role.Rules),
		"one generated rule covers Secrets, with exactly the manager's verbs")
}

// secretRuleVerbs returns the sorted verbs of every rule that covers Secrets.
func secretRuleVerbs(rules []rbacv1.PolicyRule) [][]string {
	var out [][]string
	for _, r := range rules {
		coreGroup := slices.Contains(r.APIGroups, "") || slices.Contains(r.APIGroups, "*")
		if coreGroup && (slices.Contains(r.Resources, "secrets") || slices.Contains(r.Resources, "*")) {
			v := slices.Clone(r.Verbs)
			slices.Sort(v)
			out = append(out, v)
		}
	}
	return out
}

// secretRBACRender runs helm template on the chart with args; it skips the
// test when helm is not installed.
func secretRBACRender(t *testing.T, args ...string) (string, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not installed")
	}
	all := append([]string{"template", "rel", filepath.Join("..", "..", "charts", "virtrigaud"), "--namespace", "virtrigaud-system"}, args...)
	out, err := exec.Command(helm, all...).CombinedOutput() // #nosec G204 -- test-controlled arguments
	return string(out), err
}

// secretRBACT is the part of *testing.T (or GinkgoT) secretRBACDocs uses.
type secretRBACT interface {
	require.TestingT
	Helper()
}

// secretRBACDocs splits a rendered multi-document stream into its non-empty
// objects.
func secretRBACDocs(t secretRBACT, raw string) []*unstructured.Unstructured {
	t.Helper()
	var docs []*unstructured.Unstructured
	rd := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(raw)))
	for {
		doc, err := rd.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		u := &unstructured.Unstructured{}
		require.NoError(t, yaml.Unmarshal(doc, &u.Object))
		if len(u.Object) > 0 {
			docs = append(docs, u)
		}
	}
	return docs
}

// renderedSecretGrants maps "Kind namespace/name" of every rendered Role or
// ClusterRole that covers Secrets to its Secret rules' verbs, and returns the
// manager container's args.
func renderedSecretGrants(t *testing.T, raw string) (grants map[string][][]string, managerArgs []string) {
	t.Helper()
	grants = map[string][][]string{}
	for _, u := range secretRBACDocs(t, raw) {
		doc, err := yaml.Marshal(u.Object)
		require.NoError(t, err)
		switch u.GetKind() {
		case "Role", "ClusterRole":
			var r struct {
				Rules []rbacv1.PolicyRule `json:"rules"`
			}
			require.NoError(t, yaml.Unmarshal(doc, &r))
			for _, rule := range r.Rules {
				if slices.Contains(rule.Resources, "secrets") {
					assert.Empty(t, rule.ResourceNames, "no resourceNames: Secret names are not fixed")
				}
			}
			if v := secretRuleVerbs(r.Rules); len(v) > 0 {
				grants[u.GetKind()+" "+u.GetNamespace()+"/"+u.GetName()] = v
			}
		case "Deployment":
			d := &appsv1.Deployment{}
			require.NoError(t, yaml.Unmarshal(doc, d))
			for _, c := range d.Spec.Template.Spec.Containers {
				if c.Name == "manager" {
					managerArgs = c.Args
				}
			}
		}
	}
	return grants, managerArgs
}

// secretCacheArg returns the --secret-cache-namespaces value in args and
// whether it is there.
func secretCacheArg(args []string) (string, bool) {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--"+SecretCacheNamespacesFlagName+"="); ok {
			return v, true
		}
	}
	return "", false
}

func TestRBAC_ChartSecretGrantDefaultIsClusterWide(t *testing.T) {
	out, err := secretRBACRender(t)
	require.NoError(t, err, out)
	grants, args := renderedSecretGrants(t, out)
	assert.Equal(t, map[string][][]string{
		"ClusterRole /rel-virtrigaud-manager": {{"create", "get", "list", "update", "watch"}, {"delete"}},
	}, grants, "the default keeps the cluster-wide Secret grant")
	_, ok := secretCacheArg(args)
	assert.False(t, ok, "the default caches Secrets in every namespace")
	assert.NotContains(t, out, "manager-secrets")

	// An older values file without rbac.secrets (helm upgrade --reuse-values)
	// renders the same cluster-wide default.
	out, err = secretRBACRender(t, "--set", "rbac.secrets=null")
	require.NoError(t, err, out)
	reused, args := renderedSecretGrants(t, out)
	assert.Equal(t, grants, reused)
	_, ok = secretCacheArg(args)
	assert.False(t, ok)

	// The release-namespace Role (rbac.scope=namespace) keeps both rules too.
	out, err = secretRBACRender(t, "--set", "rbac.scope=namespace")
	require.NoError(t, err, out)
	grants, _ = renderedSecretGrants(t, out)
	assert.Equal(t, map[string][][]string{
		"Role virtrigaud-system/rel-virtrigaud-manager": {{"create", "get", "list", "update", "watch"}, {"delete"}},
	}, grants)
}

func TestRBAC_ChartSecretGrantNamespaced(t *testing.T) {
	for _, scope := range []string{"cluster", "namespace"} {
		t.Run("rbac.scope="+scope, func(t *testing.T) {
			out, err := secretRBACRender(t,
				"--set", "rbac.scope="+scope,
				"--set", "rbac.secrets.scope=namespaced",
				"--set", "rbac.secrets.providerNamespaces={virt-b,virt-a,virt-a}",
				"--set", "rbac.secrets.workloadNamespaces={tenant-1,virt-b}")
			require.NoError(t, err, out)
			grants, args := renderedSecretGrants(t, out)
			// No Secret rule outside the per-namespace Roles: the manager's
			// ClusterRole (or release Role) holds none, and nothing else does.
			assert.Equal(t, map[string][][]string{
				"Role virt-a/rel-virtrigaud-manager-secrets":        {secretRBACAllVerbs},
				"Role virt-b/rel-virtrigaud-manager-secrets":        {secretRBACAllVerbs},
				"Role tenant-1/rel-virtrigaud-manager-secrets-read": {{"get"}},
			}, grants, "virt-b is a Provider namespace, so it gets no second, read-only Role")

			v, ok := secretCacheArg(args)
			require.True(t, ok)
			assert.Equal(t, "virt-a,virt-b", v, "the informer covers the Provider namespaces only")

			bindings := 0
			for _, u := range secretRBACDocs(t, out) {
				if u.GetKind() != "RoleBinding" || !strings.HasPrefix(u.GetName(), "rel-virtrigaud-manager-secrets") {
					continue
				}
				bindings++
				doc, err := yaml.Marshal(u.Object)
				require.NoError(t, err)
				rb := &rbacv1.RoleBinding{}
				require.NoError(t, yaml.Unmarshal(doc, rb))
				assert.Equal(t, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: rb.Name}, rb.RoleRef)
				assert.Equal(t, []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "rel-virtrigaud", Namespace: "virtrigaud-system"}},
					rb.Subjects, "bound to the manager's ServiceAccount only")
			}
			assert.Equal(t, 3, bindings)
		})
	}
}

func TestRBAC_ChartSecretGrantNamespacedEnrolmentNamespaceIsCacheOnly(t *testing.T) {
	// The per-host agent enrolment namespace joins the Secret informer (its
	// pins Secrets are watched through the cache), but the Secret grant adds
	// nothing there: the enrolment feature's own Role is the only one.
	enrolment := []string{"--set", "experimental.hostEnrolment.enabled=true",
		"--set", "experimental.hostEnrolment.developmentBuild=true",
		"--set", "experimental.hostEnrolment.signerNames={issuers.cert-manager.io/agent-pki.host-ca}"}
	out, err := secretRBACRender(t, append([]string{"--set", "rbac.secrets.scope=namespaced",
		"--set", "rbac.secrets.providerNamespaces={virt-a}",
		"--set", "experimental.hostEnrolment.namespace=agent-pki"}, enrolment...)...)
	require.NoError(t, err, out)
	grants, args := renderedSecretGrants(t, out)
	for name := range grants {
		if strings.Contains(name, "agent-pki/") {
			assert.Equal(t, "Role agent-pki/rel-virtrigaud-host-enrolment", name, "only the enrolment feature's own Role grants Secrets in the enrolment namespace")
		}
	}
	assert.Contains(t, grants, "Role agent-pki/rel-virtrigaud-host-enrolment")
	assert.Contains(t, grants, "Role virt-a/rel-virtrigaud-manager-secrets")
	v, ok := secretCacheArg(args)
	require.True(t, ok)
	assert.Equal(t, "agent-pki,virt-a", v)

	out, err = secretRBACRender(t, append([]string{"--set", "rbac.secrets.scope=namespaced",
		"--set", "rbac.secrets.providerNamespaces={virt-a}",
		"--set", "experimental.hostEnrolment.namespace=Agent_PKI"}, enrolment...)...)
	require.Error(t, err, out)
	assert.Contains(t, out, "experimental.hostEnrolment.namespace")
}

func TestRBAC_ChartSecretGrantNamespacedWithoutRBAC(t *testing.T) {
	// rbac.create=false: the administrator manages RBAC, the manager is still
	// told to restrict its Secret informer.
	out, err := secretRBACRender(t, "--set", "rbac.create=false", "--set", "rbac.secrets.scope=namespaced",
		"--set", "rbac.secrets.providerNamespaces={virt-a}")
	require.NoError(t, err, out)
	grants, args := renderedSecretGrants(t, out)
	assert.Empty(t, grants)
	v, ok := secretCacheArg(args)
	assert.True(t, ok)
	assert.Equal(t, "virt-a", v)
}

func TestRBAC_ChartSecretGrantRefusesInvalidValues(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"unknown scope": {[]string{"--set", "rbac.secrets.scope=everywhere"}, "rbac.secrets.scope"},
		"no provider namespaces": {[]string{"--set", "rbac.secrets.scope=namespaced",
			"--set", "rbac.secrets.workloadNamespaces={tenant-1}"}, "rbac.secrets.providerNamespaces must list"},
		"invalid provider namespace": {[]string{"--set", "rbac.secrets.scope=namespaced",
			"--set", "rbac.secrets.providerNamespaces={Virt_A}"}, "not a namespace name"},
		"invalid workload namespace": {[]string{"--set", "rbac.secrets.scope=namespaced",
			"--set", "rbac.secrets.providerNamespaces={virt-a}", "--set", "rbac.secrets.workloadNamespaces={a/b}"}, "not a namespace name"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := secretRBACRender(t, tc.args...)
			require.Error(t, err, out)
			assert.Contains(t, out, tc.want)
		})
	}
}
