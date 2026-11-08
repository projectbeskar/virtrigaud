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
			require.Len(t, cr.Rules, 1, "the ClusterRole grants one rule")
			r := cr.Rules[0]
			assert.Equal(t, []string{"cert-manager.io"}, r.APIGroups)
			assert.Equal(t, []string{"signers"}, r.Resources)
			assert.Equal(t, []string{"approve"}, r.Verbs)
			assert.Equal(t, wantSigners, r.ResourceNames, "approve is limited to the named signers")
		case "Role":
			sawRole = true
			role := &rbacv1.Role{}
			require.NoError(t, yaml.Unmarshal(doc, role))
			assert.Equal(t, wantNS, role.Namespace)
			require.Len(t, role.Rules, 2)
			assert.Equal(t, []string{"certificaterequests"}, role.Rules[0].Resources)
			assert.ElementsMatch(t, []string{"create", "get", "list", "watch"}, role.Rules[0].Verbs)
			assert.Equal(t, []string{"certificaterequests/status"}, role.Rules[1].Resources)
			assert.Equal(t, []string{"update"}, role.Rules[1].Verbs)
		case "RoleBinding":
			assert.Equal(t, wantNS, u.GetNamespace())
		}
		// No wildcard and no Secret access in any object.
		for _, r := range rulesOf(t, doc) {
			for _, list := range [][]string{r.APIGroups, r.Resources, r.Verbs, r.ResourceNames} {
				assert.NotContains(t, list, "*")
			}
			assert.NotContains(t, r.Resources, "secrets")
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
	assertHostEnrolmentRBAC(t, splitYAMLDocs(t, raw), "host-agent-pki", []string{"issuers.cert-manager.io/host-agent-pki.host-agent-ca"})

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

	out, err := exec.Command(helm, "template", "t", chart, "--show-only", "templates/host-enrolment-rbac.yaml", // #nosec G204 -- test-controlled arguments
		"--set", "experimental.hostEnrolment.enabled=true",
		"--set", "experimental.hostEnrolment.namespace=host-agent-pki",
		"--set", "experimental.hostEnrolment.signerNames={issuers.cert-manager.io/host-agent-pki.host-agent-ca}").CombinedOutput()
	require.NoError(t, err, string(out))
	assertHostEnrolmentRBAC(t, splitYAMLDocs(t, out), "host-agent-pki", []string{"issuers.cert-manager.io/host-agent-pki.host-agent-ca"})

	for name, args := range map[string][]string{
		"wildcard signer": {"--set", "experimental.hostEnrolment.namespace=x", "--set", "experimental.hostEnrolment.signerNames={issuers.cert-manager.io/*}"},
		"no signer":       {"--set", "experimental.hostEnrolment.namespace=x"},
		"no namespace":    {"--set", "experimental.hostEnrolment.signerNames={issuers.cert-manager.io/x.y}"},
	} {
		t.Run(name, func(t *testing.T) {
			all := append([]string{"template", "t", chart, "--set", "experimental.hostEnrolment.enabled=true"}, args...)
			out, err := exec.Command(helm, all...).CombinedOutput() // #nosec G204 -- test-controlled arguments
			assert.Error(t, err, string(out))
		})
	}
}
