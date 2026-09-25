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

// These tests pin the CRD security-feature check: the generated
// VirtualMachine CRD has both provider-binding features and the generated
// Provider, VMClass and VMImage CRDs have spec.consumerNamespaceSelector; an
// older CRD (without any of them) is reported missing and fails readiness, an
// unreadable CRD is unknown and does not, and the manager is granted get on
// exactly those four CRDs.

// generatedCRD loads a generated CRD (by its metadata.name) from
// config/crd/bases.
func generatedCRD(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	plural, group, ok := strings.Cut(name, ".")
	require.True(t, ok, name)
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", group+"_"+plural+".yaml"))
	require.NoError(t, err)
	obj := map[string]any{}
	require.NoError(t, yaml.Unmarshal(raw, &obj))
	return &unstructured.Unstructured{Object: obj}
}

// generatedVMCRD loads the generated VirtualMachine CRD from config/crd/bases.
func generatedVMCRD(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	return generatedCRD(t, VirtualMachineCRDName)
}

// olderConsumerCRD returns the generated CRD name without
// spec.consumerNamespaceSelector.
func olderConsumerCRD(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	crd := generatedCRD(t, name)
	unstructured.RemoveNestedField(v1beta1Schema(t, crd), "properties", "spec", "properties", "consumerNamespaceSelector")
	return crd
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
	assert.Equal(t, []string{crdFeatureProviderRefCEL, crdFeaturePendingSizeCEL}, missing)

	missing, err = missingVMCRDFeatures(olderVMCRD(t, true, true))
	require.NoError(t, err)
	assert.Len(t, missing, 3)

	// A CRD from before the scheduler-accuracy amendment: the providerRef rule
	// only.
	crd := generatedVMCRD(t)
	root := v1beta1Schema(t, crd)
	rules, ok := root["x-kubernetes-validations"].([]any)
	require.True(t, ok)
	var kept []any
	for _, r := range rules {
		rule, ok := r.(map[string]any)
		require.True(t, ok)
		if text, _ := rule["rule"].(string); !strings.Contains(text, pendingSizeImmutabilityRuleFragment) {
			kept = append(kept, r)
		}
	}
	require.Len(t, kept, len(rules)-1, "the generated CRD carries the pending-size rule")
	root["x-kubernetes-validations"] = kept
	missing, err = missingVMCRDFeatures(crd)
	require.NoError(t, err)
	assert.Equal(t, []string{crdFeaturePendingSizeCEL}, missing)

	// A CRD without status.placement.pendingResources (review N3).
	crd = generatedVMCRD(t)
	unstructured.RemoveNestedField(v1beta1Schema(t, crd), "properties", "status", "properties", "placement", "properties", "pendingResources")
	missing, err = missingVMCRDFeatures(crd)
	require.NoError(t, err)
	assert.Equal(t, []string{crdFeaturePendingResources}, missing)
}

func TestMissingConsumerSelector(t *testing.T) {
	for _, name := range []string{ProviderCRDName, VMClassCRDName, VMImageCRDName} {
		missing, err := missingConsumerSelector(generatedCRD(t, name))
		require.NoError(t, err)
		assert.Empty(t, missing, "the generated %s CRD has spec.consumerNamespaceSelector", name)

		missing, err = missingConsumerSelector(olderConsumerCRD(t, name))
		require.NoError(t, err)
		assert.Equal(t, []string{crdFeatureConsumerSelector}, missing, name)
	}
}

// olderVMImageCRD returns the generated VMImage CRD without the named fields
// of a status.providerStatus entry.
func olderVMImageCRD(t *testing.T, fields ...string) *unstructured.Unstructured {
	t.Helper()
	crd := generatedCRD(t, VMImageCRDName)
	for _, f := range fields {
		unstructured.RemoveNestedField(v1beta1Schema(t, crd),
			"properties", "status", "properties", "providerStatus", "additionalProperties", "properties", f)
	}
	return crd
}

func TestMissingVMImageCRDFeatures(t *testing.T) {
	missing, err := missingVMImageCRDFeatures(generatedCRD(t, VMImageCRDName))
	require.NoError(t, err)
	assert.Empty(t, missing, "the generated VMImage CRD has every feature")

	missing, err = missingVMImageCRDFeatures(olderVMImageCRD(t, "providerUID", "taskRef"))
	require.NoError(t, err)
	assert.Equal(t, []string{crdFeatureImageProviderUID, crdFeatureImageTaskRef}, missing)

	missing, err = missingVMImageCRDFeatures(olderVMImageCRD(t, "taskRef"))
	require.NoError(t, err)
	assert.Equal(t, []string{crdFeatureImageTaskRef}, missing)

	missing, err = missingVMImageCRDFeatures(olderConsumerCRD(t, VMImageCRDName))
	require.NoError(t, err)
	assert.Equal(t, []string{crdFeatureConsumerSelector}, missing)

	// A VMImage CRD without the prepare-state fields fails readiness.
	c := NewVMCRDFeatureChecker(&stubCRDReader{
		crd:    generatedVMCRD(t),
		others: map[string]*unstructured.Unstructured{VMImageCRDName: olderVMImageCRD(t, "providerUID")},
		t:      t,
	})
	state, missing := c.Evaluate(context.Background())
	assert.Equal(t, metrics.CRDFeaturesMissing, state)
	assert.Equal(t, []string{VMImageCRDName + ": " + crdFeatureImageProviderUID}, missing)
	require.Error(t, readyzErr(c))
	assert.Contains(t, readyzErr(c).Error(), crdFeatureImageProviderUID)
}

// TestMissingVMImageCRDFeatures_SourceDigest pins ADR-0009 D8: a VMImage CRD
// without status.providerStatus[].sourceDigest (the #344 CRD) is reported
// missing and fails readiness.
func TestMissingVMImageCRDFeatures_SourceDigest(t *testing.T) {
	missing, err := missingVMImageCRDFeatures(olderVMImageCRD(t, "sourceDigest"))
	require.NoError(t, err)
	assert.Equal(t, []string{crdFeatureImageSourceDigest}, missing)

	c := NewVMCRDFeatureChecker(&stubCRDReader{
		crd:    generatedVMCRD(t),
		others: map[string]*unstructured.Unstructured{VMImageCRDName: olderVMImageCRD(t, "sourceDigest")},
		t:      t,
	})
	state, missing := c.Evaluate(context.Background())
	assert.Equal(t, metrics.CRDFeaturesMissing, state)
	assert.Equal(t, []string{VMImageCRDName + ": " + crdFeatureImageSourceDigest}, missing)
	require.Error(t, readyzErr(c))
	assert.Contains(t, readyzErr(c).Error(), crdFeatureImageSourceDigest)
}

// olderProviderCRD returns the generated Provider CRD without
// status.reportedCapabilities.supportsImageArtifactIdentity.
func olderProviderCRD(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	crd := generatedCRD(t, ProviderCRDName)
	unstructured.RemoveNestedField(v1beta1Schema(t, crd), "properties", "status", "properties",
		"reportedCapabilities", "properties", "supportsImageArtifactIdentity")
	return crd
}

// TestMissingProviderCRDFeatures pins ADR-0009 D8 on the Provider CRD: it needs
// spec.consumerNamespaceSelector and
// status.reportedCapabilities.supportsImageArtifactIdentity, and an older CRD
// without the capability field fails readiness.
func TestMissingProviderCRDFeatures(t *testing.T) {
	missing, err := missingProviderCRDFeatures(generatedCRD(t, ProviderCRDName))
	require.NoError(t, err)
	assert.Empty(t, missing, "the generated Provider CRD has every feature")

	missing, err = missingProviderCRDFeatures(olderProviderCRD(t))
	require.NoError(t, err)
	assert.Equal(t, []string{crdFeatureProviderImageArtifactIdentity}, missing)

	missing, err = missingProviderCRDFeatures(olderConsumerCRD(t, ProviderCRDName))
	require.NoError(t, err)
	assert.Equal(t, []string{crdFeatureConsumerSelector}, missing)

	c := NewVMCRDFeatureChecker(&stubCRDReader{
		crd:    generatedVMCRD(t),
		others: map[string]*unstructured.Unstructured{ProviderCRDName: olderProviderCRD(t)},
		t:      t,
	})
	state, missing := c.Evaluate(context.Background())
	assert.Equal(t, metrics.CRDFeaturesMissing, state)
	assert.Equal(t, []string{ProviderCRDName + ": " + crdFeatureProviderImageArtifactIdentity}, missing)
	require.Error(t, readyzErr(c))
	assert.Contains(t, readyzErr(c).Error(), crdFeatureProviderImageArtifactIdentity)
}

// stubCRDReader answers Get for the VirtualMachine CRD with crd or err, and
// for the Provider, VMClass and VMImage CRDs with others[name] (the generated
// CRD when unset) or otherErr.
type stubCRDReader struct {
	client.Reader
	crd      *unstructured.Unstructured
	err      error
	others   map[string]*unstructured.Unstructured
	otherErr error
	calls    int
	t        *testing.T
}

func (s *stubCRDReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	s.calls++
	var crd *unstructured.Unstructured
	switch key.Name {
	case VirtualMachineCRDName:
		if s.err != nil {
			return s.err
		}
		crd = s.crd
	case ProviderCRDName, VMClassCRDName, VMImageCRDName:
		if s.otherErr != nil {
			return s.otherErr
		}
		crd = s.others[key.Name]
		if crd == nil {
			crd = generatedCRD(s.t, key.Name)
		}
	default:
		return errors.New("unexpected key " + key.String())
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return errors.New("expected unstructured")
	}
	u.Object = crd.DeepCopy().Object
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
		"older Provider CRD": {&stubCRDReader{crd: generatedVMCRD(t),
			others: map[string]*unstructured.Unstructured{ProviderCRDName: olderConsumerCRD(t, ProviderCRDName)}}, metrics.CRDFeaturesMissing, false},
		"older VMClass CRD": {&stubCRDReader{crd: generatedVMCRD(t),
			others: map[string]*unstructured.Unstructured{VMClassCRDName: olderConsumerCRD(t, VMClassCRDName)}}, metrics.CRDFeaturesMissing, false},
		"older VMImage CRD": {&stubCRDReader{crd: generatedVMCRD(t),
			others: map[string]*unstructured.Unstructured{VMImageCRDName: olderConsumerCRD(t, VMImageCRDName)}}, metrics.CRDFeaturesMissing, false},
		"consumer CRDs forbidden only": {&stubCRDReader{crd: generatedVMCRD(t),
			otherErr: apierrors.NewForbidden(crdGR, ProviderCRDName, errors.New("no"))}, metrics.CRDFeaturesUnknown, true},
		"missing wins over forbidden": {&stubCRDReader{crd: olderVMCRD(t, true, false),
			otherErr: apierrors.NewForbidden(crdGR, ProviderCRDName, errors.New("no"))}, metrics.CRDFeaturesMissing, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tc.reader.t = t
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

	t.Run("the missing consumer selector is named with its CRD", func(t *testing.T) {
		c := NewVMCRDFeatureChecker(&stubCRDReader{t: t, crd: generatedVMCRD(t),
			others: map[string]*unstructured.Unstructured{VMImageCRDName: olderConsumerCRD(t, VMImageCRDName)}})
		_, missing := c.Evaluate(context.Background())
		assert.Equal(t, []string{VMImageCRDName + ": " + crdFeatureConsumerSelector}, missing)
		assert.Contains(t, readyzErr(c).Error(), VMImageCRDName)
	})
}

func TestVMCRDFeatureChecker_CachesAndKeepsStateOnTransientErrors(t *testing.T) {
	now := time.Unix(1_000, 0)
	reader := &stubCRDReader{t: t, crd: olderVMCRD(t, true, false)}
	c := NewVMCRDFeatureChecker(reader)
	c.now = func() time.Time { return now }
	perCheck := len(securityCRDs)

	require.Error(t, readyzErr(c))
	require.Error(t, readyzErr(c))
	assert.Equal(t, perCheck, reader.calls, "the result is reused within the interval")

	// A transient read error keeps the previous (missing) state.
	reader.err = errors.New("etcd timeout")
	now = now.Add(2 * time.Minute)
	require.Error(t, readyzErr(c))
	assert.Equal(t, perCheck+1, reader.calls, "the check stops at the first transient error")

	// Once the CRD is upgraded, readiness recovers at the next check.
	reader.err = nil
	reader.crd = generatedVMCRD(t)
	now = now.Add(2 * time.Minute)
	require.NoError(t, readyzErr(c))
	state, _ := c.Evaluate(context.Background())
	assert.Equal(t, metrics.CRDFeaturesVerified, state)
}

func TestRBAC_CRDReadIsGetOnTheFourCheckedCRDs(t *testing.T) {
	checked := []string{VirtualMachineCRDName, ProviderCRDName, VMClassCRDName, VMImageCRDName}
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
	assert.ElementsMatch(t, checked, rules[0].ResourceNames)

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
	for _, name := range checked {
		assert.Contains(t, tpl[clusterIdx:roleIdx], "\n  - "+name+"\n", "the ClusterRole names %s", name)
	}
	assert.Contains(t, tpl[clusterIdx:roleIdx], "  resourceNames:\n  - "+VirtualMachineCRDName)
	assert.Empty(t, chartRuleVerbs(t, tpl[roleIdx:], "customresourcedefinitions"),
		"a namespaced Role cannot grant a cluster-scoped resource")
}
