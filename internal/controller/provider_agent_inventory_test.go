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
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/devgate"
)

// agentProvider is a clustered Provider for per-host agents.
func agentProvider(name string) *infravirtrigaudiov1beta1.Provider {
	p := providerWithRuntime(name, &infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false})
	p.UID = types.UID("0f5c2a70-3a4e-4c3c-9a7e-5d1b2c3d4e5f")
	p.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
	p.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
	return p
}

// TestAgentProviderReadsInventorySecret: only the clustered Provider for
// per-host agents, and only in a development build, gets a token and the
// arguments that make it follow its own inventory Secret; every other
// Provider keeps no token and no extra argument.
func TestAgentProviderReadsInventorySecret(t *testing.T) {
	ch := agentProvider("ch")
	assert.Equal(t, devgate.Compiled, providerReadsInventorySecret(ch))
	if devgate.Compiled {
		assert.Equal(t, []string{"--hosts-secret=ch-hosts", "--provider-uid=" + string(ch.UID)}, agentProviderArgs(ch))
	} else {
		assert.Nil(t, agentProviderArgs(ch), "a release manager renders nothing for it")
	}

	single := agentProvider("single")
	single.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologySingle
	libvirt := agentProvider("lv")
	libvirt.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeLibvirt
	for _, p := range []*infravirtrigaudiov1beta1.Provider{single, libvirt, providerWithRuntime("mock", nil)} {
		assert.False(t, providerReadsInventorySecret(p), p.Name)
		assert.Nil(t, agentProviderArgs(p), p.Name)
	}

	r := &ProviderReconciler{}
	c, err := r.buildProviderContainer(libvirt, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"--port=9443", "--health-port=8080"}, c.Args, "other Providers' arguments are unchanged")
}

// TestRBAC_AgentProviderInventoryChartTemplate: the chart grants each listed
// Provider's own ServiceAccount get and watch on its inventory Secret only,
// renders nothing by default, and refuses to render without the
// development-build statement or with a malformed entry.
func TestRBAC_AgentProviderInventoryChartTemplate(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not installed")
	}
	chart := filepath.Join("..", "..", "charts", "virtrigaud")
	off, err := exec.Command(helm, "template", "t", chart).CombinedOutput() // #nosec G204 -- test-controlled arguments
	require.NoError(t, err, string(off))
	assert.NotContains(t, string(off), "provider-inventory-watch", "off by default")

	on := []string{"--set", "experimental.agentProviderInventoryWatch.enabled=true",
		"--set", "experimental.agentProviderInventoryWatch.developmentBuild=true"}
	out, err := exec.Command(helm, append([]string{"template", "t", chart, // #nosec G204 -- test-controlled arguments
		"--show-only", "templates/agent-provider-inventory-rbac.yaml",
		"--set", "experimental.agentProviderInventoryWatch.providers[0].namespace=infra",
		"--set", "experimental.agentProviderInventoryWatch.providers[0].name=ch"}, on...)...).CombinedOutput()
	require.NoError(t, err, string(out))
	assertAgentProviderInventoryRBAC(t, splitYAMLDocs(t, out), "infra", "ch")

	for name, args := range map[string][]string{
		"no development build": {"--set", "experimental.agentProviderInventoryWatch.enabled=true",
			"--set", "experimental.agentProviderInventoryWatch.providers[0].namespace=infra",
			"--set", "experimental.agentProviderInventoryWatch.providers[0].name=ch"},
		"no provider":  on,
		"no namespace": append([]string{"--set", "experimental.agentProviderInventoryWatch.providers[0].name=ch"}, on...),
		"wildcard name": append([]string{"--set", "experimental.agentProviderInventoryWatch.providers[0].namespace=infra",
			"--set", "experimental.agentProviderInventoryWatch.providers[0].name=*"}, on...),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := exec.Command(helm, append([]string{"template", "t", chart}, args...)...).CombinedOutput() // #nosec G204 -- test-controlled arguments
			assert.Error(t, err, string(out))
		})
	}
}

// assertAgentProviderInventoryRBAC checks the rendered Role and RoleBinding
// of one Provider: get and watch on <name>-hosts only, bound to the
// Provider's own ServiceAccount, in its namespace.
func assertAgentProviderInventoryRBAC(t *testing.T, docs [][]byte, ns, name string) {
	t.Helper()
	sa := "virtrigaud-provider-" + ns + "-" + name
	var sawRole, sawBinding bool
	for _, doc := range docs {
		u := &unstructured.Unstructured{}
		require.NoError(t, yaml.Unmarshal(doc, &u.Object))
		assert.Equal(t, ns, u.GetNamespace())
		switch u.GetKind() {
		case "Role":
			sawRole = true
			role := &rbacv1.Role{}
			require.NoError(t, yaml.Unmarshal(doc, role))
			require.Len(t, role.Rules, 1)
			r := role.Rules[0]
			assert.Equal(t, []string{""}, r.APIGroups)
			assert.Equal(t, []string{"secrets"}, r.Resources)
			assert.Equal(t, []string{"get", "watch"}, r.Verbs, "no list: the watch names the Secret")
			assert.Equal(t, []string{name + "-hosts"}, r.ResourceNames)
		case "RoleBinding":
			sawBinding = true
			rb := &rbacv1.RoleBinding{}
			require.NoError(t, yaml.Unmarshal(doc, rb))
			assert.Equal(t, "Role", rb.RoleRef.Kind)
			require.Len(t, rb.Subjects, 1)
			assert.Equal(t, rbacv1.Subject{Kind: "ServiceAccount", Name: sa, Namespace: ns}, rb.Subjects[0])
		default:
			t.Errorf("unexpected kind %s", u.GetKind())
		}
	}
	assert.True(t, sawRole && sawBinding)
}
