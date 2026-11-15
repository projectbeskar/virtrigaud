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
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

// deploymentFor renders the Provider's Deployment with a fake client and
// returns its pod spec.
func deploymentFor(t *testing.T, r *ProviderReconciler, p *infravirtrigaudiov1beta1.Provider) corev1.PodSpec {
	t.Helper()
	sch := newProviderSAScheme(t)
	r.Client = fake.NewClientBuilder().WithScheme(sch).WithObjects(p.DeepCopy()).Build()
	r.Scheme = sch
	dep, err := r.reconcileDeployment(context.Background(), p, r.getServiceAccountName(p))
	require.NoError(t, err)
	return dep.Spec.Template.Spec
}

// hasInventoryVolume reports whether the pod mounts the inventory Secret.
func hasInventoryVolume(spec corev1.PodSpec) bool {
	for _, v := range spec.Volumes {
		if v.Name == hostInventoryVolumeName {
			return true
		}
	}
	for _, c := range spec.Containers {
		for _, m := range c.VolumeMounts {
			if m.Name == hostInventoryVolumeName {
				return true
			}
		}
	}
	return false
}

// TestAgentProviderReadsInventorySecret: only the clustered Provider for
// per-host agents, only in a development build and only with the manager's
// --dev-agent-provider-inventory-watch, gets a ServiceAccount token, the
// --hosts-secret and --provider-uid arguments and no inventory volume;
// without the flag it keeps the mounted inventory and no token, and every
// other Provider is unchanged.
func TestAgentProviderReadsInventorySecret(t *testing.T) {
	ch := agentProvider("ch")
	on := &ProviderReconciler{AgentProviderInventoryWatch: map[types.NamespacedName]struct{}{
		{Namespace: "default", Name: "ch"}: {}, {Namespace: "default", Name: "single"}: {},
		{Namespace: "default", Name: "lv"}: {}, {Namespace: "default", Name: "mock"}: {}}}
	off := &ProviderReconciler{}
	other := &ProviderReconciler{AgentProviderInventoryWatch: map[types.NamespacedName]struct{}{{Namespace: "default", Name: "other"}: {}}}

	spec := deploymentFor(t, on, ch)
	require.NotNil(t, spec.AutomountServiceAccountToken)
	assert.Equal(t, devgate.Compiled, *spec.AutomountServiceAccountToken, "token only in a development build")
	if devgate.Compiled {
		assert.Subset(t, spec.Containers[0].Args, []string{"--hosts-secret=ch-hosts", "--provider-uid=" + string(ch.UID)})
		assert.False(t, hasInventoryVolume(spec), "no copy of the inventory on disk when it is read through the API")
	} else {
		assert.Equal(t, []string{"--port=9443", "--health-port=8080"}, spec.Containers[0].Args, "a release manager renders nothing for it")
		assert.True(t, hasInventoryVolume(spec))
	}

	// Not listed in the manager flag (no chart grant for it), with the flag
	// empty or naming another Provider: no token, no arguments, the
	// inventory stays mounted.
	for _, r := range []*ProviderReconciler{off, other} {
		spec = deploymentFor(t, r, ch)
		require.NotNil(t, spec.AutomountServiceAccountToken)
		assert.False(t, *spec.AutomountServiceAccountToken)
		assert.Equal(t, []string{"--port=9443", "--health-port=8080"}, spec.Containers[0].Args)
		assert.True(t, hasInventoryVolume(spec))
	}

	single := agentProvider("single")
	single.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologySingle
	libvirt := agentProvider("lv")
	libvirt.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeLibvirt
	for _, p := range []*infravirtrigaudiov1beta1.Provider{single, libvirt, providerWithRuntime("mock", &infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false})} {
		spec := deploymentFor(t, on, p)
		require.NotNil(t, spec.AutomountServiceAccountToken, p.Name)
		assert.False(t, *spec.AutomountServiceAccountToken, p.Name)
		assert.Equal(t, []string{"--port=9443", "--health-port=8080"}, spec.Containers[0].Args, p.Name)
	}
	assert.True(t, hasInventoryVolume(deploymentFor(t, on, libvirt)), "clustered libvirt keeps its mounted inventory")
}

// TestParseAgentProviderInventoryWatch: the manager flag lists
// namespace/name entries; a release build refuses any entry.
func TestParseAgentProviderInventoryWatch(t *testing.T) {
	got, err := ParseAgentProviderInventoryWatch("")
	require.NoError(t, err)
	assert.Empty(t, got)
	got, err = ParseAgentProviderInventoryWatch(" infra/ch, b/ch2 ,")
	if !devgate.Compiled {
		require.Error(t, err)
		return
	}
	require.NoError(t, err)
	assert.Equal(t, map[types.NamespacedName]struct{}{{Namespace: "infra", Name: "ch"}: {}, {Namespace: "b", Name: "ch2"}: {}}, got)
	for _, bad := range []string{"ch", "infra/", "/ch", "Infra/ch", "infra/c h", "a/b/c"} {
		_, err := ParseAgentProviderInventoryWatch(bad)
		assert.Error(t, err, bad)
	}
}

// TestRBAC_AgentProviderInventoryChartTemplate: the chart grants each listed
// Provider's own ServiceAccount get and watch on its inventory Secret only,
// renders nothing by default, and refuses to render without the
// development-build statement or with a malformed entry.
func TestRBAC_AgentProviderInventoryChartTemplate(t *testing.T) {
	helm := requireHelm(t)
	chart := filepath.Join("..", "..", "charts", "virtrigaud")
	off, err := exec.Command(helm, "template", "t", chart).CombinedOutput() // #nosec G204 -- test-controlled arguments
	require.NoError(t, err, string(off))
	assert.NotContains(t, string(off), "provider-inventory-watch", "off by default")
	assert.NotContains(t, string(off), "--dev-agent-provider-inventory-watch", "the manager flag is off by default")

	on := []string{"--set", "experimental.agentProviderInventoryWatch.enabled=true",
		"--set", "experimental.agentProviderInventoryWatch.developmentBuild=true"}
	out, err := exec.Command(helm, append([]string{"template", "t", chart, // #nosec G204 -- test-controlled arguments
		"--show-only", "templates/agent-provider-inventory-rbac.yaml",
		"--set", "experimental.agentProviderInventoryWatch.providers[0].namespace=infra",
		"--set", "experimental.agentProviderInventoryWatch.providers[0].name=ch"}, on...)...).CombinedOutput()
	require.NoError(t, err, string(out))
	assertAgentProviderInventoryRBAC(t, splitYAMLDocs(t, out), "infra", "ch")
	golden, err := os.ReadFile(agentProviderInventoryGolden)
	require.NoError(t, err)
	assert.Equal(t, withoutLabels(t, splitYAMLDocs(t, golden)), withoutLabels(t, splitYAMLDocs(t, out)),
		"the chart renders the reviewed objects (%s; labels aside)", agentProviderInventoryGolden)

	// The manager is told the grant exists, so that it renders the
	// Provider's Deployment to read the Secret through the API.
	mgr, err := exec.Command(helm, append([]string{"template", "t", chart, // #nosec G204 -- test-controlled arguments
		"--show-only", "templates/manager-deployment.yaml",
		"--set", "experimental.agentProviderInventoryWatch.providers[0].namespace=infra",
		"--set", "experimental.agentProviderInventoryWatch.providers[0].name=ch"}, on...)...).CombinedOutput()
	require.NoError(t, err, string(mgr))
	assert.Contains(t, string(mgr), "- --dev-agent-provider-inventory-watch=infra/ch\n")
	two, err := exec.Command(helm, append([]string{"template", "t", chart, // #nosec G204 -- test-controlled arguments
		"--show-only", "templates/manager-deployment.yaml",
		"--set", "experimental.agentProviderInventoryWatch.providers[0].namespace=infra",
		"--set", "experimental.agentProviderInventoryWatch.providers[0].name=ch",
		"--set", "experimental.agentProviderInventoryWatch.providers[1].namespace=b",
		"--set", "experimental.agentProviderInventoryWatch.providers[1].name=ch2"}, on...)...).CombinedOutput()
	require.NoError(t, err, string(two))
	assert.Contains(t, string(two), "- --dev-agent-provider-inventory-watch=infra/ch,b/ch2\n", "the manager gets exactly the listed Providers")

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

// agentProviderInventoryGolden is the reviewed rendering of the Provider
// inventory watch RBAC for Provider infra/ch.
var agentProviderInventoryGolden = filepath.Join("testdata", "agent-provider-inventory-rbac.golden.yaml")

// requireHelm returns the helm binary; without one the test is skipped,
// except in CI, where it fails.
func requireHelm(t *testing.T) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI")
		}
		t.Skip("helm not installed")
	}
	return helm
}

// withoutLabels returns docs decoded, without metadata.labels (they carry
// the chart version).
func withoutLabels(t *testing.T, docs [][]byte) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(docs))
	for _, d := range docs {
		u := &unstructured.Unstructured{}
		require.NoError(t, yaml.Unmarshal(d, &u.Object))
		u.SetLabels(nil)
		out = append(out, u.Object)
	}
	return out
}

// TestRBAC_AgentProviderInventoryGolden checks the reviewed rendering
// without helm: get and watch on the inventory Secret only, bound to the
// Provider's own ServiceAccount.
func TestRBAC_AgentProviderInventoryGolden(t *testing.T) {
	raw, err := os.ReadFile(agentProviderInventoryGolden)
	require.NoError(t, err)
	docs := splitYAMLDocs(t, raw)
	require.Len(t, docs, 2)
	assertAgentProviderInventoryRBAC(t, docs, "infra", "ch")
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
