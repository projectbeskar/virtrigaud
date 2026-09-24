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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// The cross-namespace grant check reads (and watches) cluster-scoped
// Namespaces. These tests pin that the manager is granted exactly
// get;list;watch on namespaces — in the kubebuilder markers, the generated
// config/rbac role and BOTH chart RBAC templates (cluster and namespace scope)
// — and nothing that writes Namespaces.

const namespacesRBACMarker = `//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch`

var readOnlyVerbs = []string{"get", "list", "watch"}

func TestRBAC_NamespaceMarkersPresent(t *testing.T) {
	for _, f := range []string{"vmclone_controller.go", "vmmigration_controller.go"} {
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		assert.Contains(t, string(src), namespacesRBACMarker, f)
	}
}

func TestRBAC_GeneratedRoleGrantsNamespacesReadOnly(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	require.NoError(t, err)
	role := &rbacv1.ClusterRole{}
	require.NoError(t, utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096).Decode(role))

	var verbs [][]string
	for _, rule := range role.Rules {
		if slices.Contains(rule.APIGroups, "") && slices.Contains(rule.Resources, "namespaces") {
			v := slices.Clone(rule.Verbs)
			slices.Sort(v)
			verbs = append(verbs, v)
		}
	}
	require.Len(t, verbs, 1, "exactly one rule covers namespaces")
	assert.Equal(t, readOnlyVerbs, verbs[0])
}

// chartRuleVerbs returns, for every rule in a Helm RBAC template that lists
// resource, the rule's verbs. The template is not plain YAML (it has Go
// template directives), so rules are split on "- apiGroups:" at column 0.
func chartRuleVerbs(t *testing.T, template, resource string) [][]string {
	t.Helper()
	var out [][]string
	for _, block := range strings.Split(template, "\n- apiGroups:")[1:] {
		var resources, verbs []string
		section := ""
		for _, line := range strings.Split(block, "\n") {
			trimmed := strings.TrimSpace(line)
			switch {
			case trimmed == "resources:":
				section = "resources"
			case trimmed == "verbs:":
				section = "verbs"
			case strings.HasPrefix(line, "  - "):
				item := strings.TrimPrefix(line, "  - ")
				if section == "resources" {
					resources = append(resources, item)
				} else if section == "verbs" {
					verbs = append(verbs, item)
				}
			case !strings.HasPrefix(line, "  "):
				section = "" // end of this rule (comment, directive, next doc)
			}
		}
		if slices.Contains(resources, resource) {
			slices.Sort(verbs)
			out = append(out, verbs)
		}
	}
	return out
}

func TestRBAC_ChartGrantsNamespacesReadOnlyInBothScopes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "virtrigaud", "templates", "manager-rbac.yaml"))
	require.NoError(t, err)
	tpl := string(raw)

	clusterIdx := strings.Index(tpl, "\nkind: ClusterRole\n")
	roleIdx := strings.Index(tpl, "\nkind: Role\n")
	require.Positive(t, clusterIdx, "ClusterRole template present")
	require.Positive(t, roleIdx, "Role template present")
	require.Less(t, clusterIdx, roleIdx)

	for name, section := range map[string]string{
		"ClusterRole (rbac.scope=cluster)": tpl[clusterIdx:roleIdx],
		"Role (rbac.scope=namespace)":      tpl[roleIdx:],
	} {
		verbs := chartRuleVerbs(t, section, "namespaces")
		require.Len(t, verbs, 1, "%s: exactly one rule covers namespaces", name)
		assert.Equal(t, readOnlyVerbs, verbs[0], "%s: namespaces is read-only", name)
	}
}
