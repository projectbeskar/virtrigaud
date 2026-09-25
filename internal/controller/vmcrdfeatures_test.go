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
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// These tests pin the VirtualMachine CRD feature check: the generated CRD has
// both provider-binding features, an older CRD (without either) is reported
// missing and fails readiness, an unreadable CRD is unknown and does not, and
// the manager is granted get on exactly that one CRD.

// generatedVMCRD loads the generated VirtualMachine CRD from config/crd/bases.
func generatedVMCRD(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "infra.virtrigaud.io_virtualmachines.yaml"))
	require.NoError(t, err)
	obj := map[string]any{}
	require.NoError(t, yaml.Unmarshal(raw, &obj))
	return &unstructured.Unstructured{Object: obj}
}

// v1beta1Schema returns the v1beta1 openAPIV3Schema map of crd, in place (the
// unstructured.Nested* getters return copies), so edits change crd.
func v1beta1Schema(t *testing.T, crd *unstructured.Unstructured) map[string]any {
	t.Helper()
	spec, ok := crd.Object["spec"].(map[string]any)
	require.True(t, ok)
	versions, ok := spec["versions"].([]any)
	require.True(t, ok)
	for _, v := range versions {
		version, ok := v.(map[string]any)
		require.True(t, ok)
		if version["name"] == "v1beta1" {
			s, ok := version["schema"].(map[string]any)
			require.True(t, ok)
			root, ok := s["openAPIV3Schema"].(map[string]any)
			require.True(t, ok)
			return root
		}
	}
	t.Fatal("no v1beta1 version")
	return nil
}

// olderVMCRD returns the generated CRD without the features named.
func olderVMCRD(t *testing.T, dropBoundProvider, dropRule bool) *unstructured.Unstructured {
	t.Helper()
	crd := generatedVMCRD(t)
	root := v1beta1Schema(t, crd)
	if dropBoundProvider {
		unstructured.RemoveNestedField(root, "properties", "status", "properties", "boundProvider")
	}
	if dropRule {
		delete(root, "x-kubernetes-validations")
	}
	return crd
}

func TestMissingVMCRDFeatures(t *testing.T) {
	missing, err := missingVMCRDFeatures(generatedVMCRD(t))
	require.NoError(t, err)
	assert.Empty(t, missing, "the generated CRD has both provider-binding features")

	missing, err = missingVMCRDFeatures(olderVMCRD(t, true, false))
	require.NoError(t, err)
	assert.Equal(t, []string{crdFeatureBoundProvider}, missing)

	missing, err = missingVMCRDFeatures(olderVMCRD(t, false, true))
	require.NoError(t, err)
	assert.Equal(t, []string{crdFeatureProviderRefCEL}, missing)

	missing, err = missingVMCRDFeatures(olderVMCRD(t, true, true))
	require.NoError(t, err)
	assert.Len(t, missing, 2)
}

// stubCRDReader answers Get for the VirtualMachine CRD with crd or err.
type stubCRDReader struct {
	client.Reader
	crd   *unstructured.Unstructured
	err   error
	calls int
}

func (s *stubCRDReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	s.calls++
	if key.Name != VirtualMachineCRDName {
		return errors.New("unexpected key " + key.String())
	}
	if s.err != nil {
		return s.err
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return errors.New("expected unstructured")
	}
	u.Object = s.crd.DeepCopy().Object
	return nil
}

func readyzErr(c *VMCRDFeatureChecker) error {
	return c.ReadyzCheck(httptest.NewRequest("GET", "/readyz", nil))
}

func TestVMCRDFeatureChecker_States(t *testing.T) {
	crdGR := schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}
	cases := map[string]struct {
		reader    *stubCRDReader
		wantState string
		wantReady bool
	}{
		"verified":                   {&stubCRDReader{crd: generatedVMCRD(t)}, metrics.CRDFeaturesVerified, true},
		"older CRD":                  {&stubCRDReader{crd: olderVMCRD(t, true, true)}, metrics.CRDFeaturesMissing, false},
		"older CRD, rule only":       {&stubCRDReader{crd: olderVMCRD(t, false, true)}, metrics.CRDFeaturesMissing, false},
		"CRD absent":                 {&stubCRDReader{err: apierrors.NewNotFound(crdGR, VirtualMachineCRDName)}, metrics.CRDFeaturesMissing, false},
		"forbidden (namespace RBAC)": {&stubCRDReader{err: apierrors.NewForbidden(crdGR, VirtualMachineCRDName, errors.New("no"))}, metrics.CRDFeaturesUnknown, true},
		"transient on first read":    {&stubCRDReader{err: errors.New("connection refused")}, metrics.CRDFeaturesUnknown, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := NewVMCRDFeatureChecker(tc.reader)
			state, _ := c.Evaluate(context.Background())
			assert.Equal(t, tc.wantState, state)
			err := readyzErr(c)
			if tc.wantReady {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrVMCRDSecurityFeaturesMissing)
		})
	}
}

func TestVMCRDFeatureChecker_CachesAndKeepsStateOnTransientErrors(t *testing.T) {
	now := time.Unix(1_000, 0)
	reader := &stubCRDReader{crd: olderVMCRD(t, true, false)}
	c := NewVMCRDFeatureChecker(reader)
	c.now = func() time.Time { return now }

	require.Error(t, readyzErr(c))
	require.Error(t, readyzErr(c))
	assert.Equal(t, 1, reader.calls, "the result is reused within the interval")

	// A transient read error keeps the previous (missing) state.
	reader.err = errors.New("etcd timeout")
	now = now.Add(2 * time.Minute)
	require.Error(t, readyzErr(c))
	assert.Equal(t, 2, reader.calls)

	// Once the CRD is upgraded, readiness recovers at the next check.
	reader.err = nil
	reader.crd = generatedVMCRD(t)
	now = now.Add(2 * time.Minute)
	require.NoError(t, readyzErr(c))
	state, _ := c.Evaluate(context.Background())
	assert.Equal(t, metrics.CRDFeaturesVerified, state)
}

func TestRBAC_VMCRDReadIsGetOnOneCRD(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	require.NoError(t, err)
	role := &rbacv1.ClusterRole{}
	require.NoError(t, utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096).Decode(role))
	var rules []rbacv1.PolicyRule
	for _, rule := range role.Rules {
		if slices.Contains(rule.Resources, "customresourcedefinitions") {
			rules = append(rules, rule)
		}
	}
	require.Len(t, rules, 1)
	assert.Equal(t, []string{"get"}, rules[0].Verbs)
	assert.Equal(t, []string{VirtualMachineCRDName}, rules[0].ResourceNames)

	chart, err := os.ReadFile(filepath.Join("..", "..", "charts", "virtrigaud", "templates", "manager-rbac.yaml"))
	require.NoError(t, err)
	tpl := string(chart)
	clusterIdx := strings.Index(tpl, "\nkind: ClusterRole\n")
	roleIdx := strings.Index(tpl, "\nkind: Role\n")
	require.Positive(t, clusterIdx)
	require.Less(t, clusterIdx, roleIdx)
	clusterVerbs := chartRuleVerbs(t, tpl[clusterIdx:roleIdx], "customresourcedefinitions")
	require.Len(t, clusterVerbs, 1, "the ClusterRole grants the CRD read")
	assert.Equal(t, []string{"get"}, clusterVerbs[0])
	assert.Contains(t, tpl[clusterIdx:roleIdx], "  resourceNames:\n  - "+VirtualMachineCRDName)
	assert.Empty(t, chartRuleVerbs(t, tpl[roleIdx:], "customresourcedefinitions"),
		"a namespaced Role cannot grant a cluster-scoped resource")
}
