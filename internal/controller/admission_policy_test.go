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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/yaml"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// reservedKeysPolicyPath is the kustomize copy of the reserved-keys
// ValidatingAdmissionPolicy; the chart renders the same CEL
// (hack/verify-admission-policy-render.sh).
var reservedKeysPolicyPath = filepath.Join("..", "..", "config", "admission-policy", "reserved_keys_policy.yaml")

// The classification of every virtrigaud.io key the manager writes on a
// VirtualMachine (docs/admission-policy.md). A new manager-owned key must be
// added here AND to the policy; a key users set must never be reserved.
var (
	managerOwnedVMAnnotations = []string{
		migrationProvenanceAnnotation, migrationTargetClaimAnnotation, migratedFromAnnotation,
		importedDiskIDAnnotation, diskChecksumAnnotation, migrationCompletedAnnotation, migrationCompletedAtAnnotation,
		CloneAnnotationClone, CloneAnnotationCloneUID, CloneAnnotationClonedFrom,
		AdoptedHostAnnotation, AdoptedIDAnnotation,
	}
	managerOwnedVMLabels = reservedTargetLabels
	userSetVMKeys        = []string{
		infrav1beta1.VirtualMachineOrphanOnDeleteAnnotation, forceDeleteAnnotation, createOutcomeAnnotation,
		infrav1beta1.VirtualMachinePlacementUIDAnnotation, infrav1beta1.ProviderAllowConsumerOrphanOnDeleteAnnotation,
	}
)

// loadReservedKeysPolicy decodes the policy and binding from the kustomize
// copy.
func loadReservedKeysPolicy(t interface{ Fatalf(string, ...any) }) (*admissionregistrationv1.ValidatingAdmissionPolicy,
	*admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	raw, err := os.ReadFile(reservedKeysPolicyPath)
	if err != nil {
		t.Fatalf("read %s: %v", reservedKeysPolicyPath, err)
	}
	var policy *admissionregistrationv1.ValidatingAdmissionPolicy
	var binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		var head struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatalf("decode %s: %v", reservedKeysPolicyPath, err)
		}
		switch head.Kind {
		case "ValidatingAdmissionPolicy":
			policy = &admissionregistrationv1.ValidatingAdmissionPolicy{}
			err = yaml.UnmarshalStrict([]byte(doc), policy)
		case "ValidatingAdmissionPolicyBinding":
			binding = &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
			err = yaml.UnmarshalStrict([]byte(doc), binding)
		}
		if err != nil {
			t.Fatalf("decode %s: %v", head.Kind, err)
		}
	}
	if policy == nil || binding == nil {
		t.Fatalf("%s must hold a ValidatingAdmissionPolicy and its binding", reservedKeysPolicyPath)
	}
	return policy, binding
}

// policyVariableKeys returns the quoted keys of the policy variable name.
func policyVariableKeys(t *testing.T, policy *admissionregistrationv1.ValidatingAdmissionPolicy, name string) []string {
	t.Helper()
	for _, v := range policy.Spec.Variables {
		if v.Name == name {
			var keys []string
			for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(v.Expression, -1) {
				keys = append(keys, m[1])
			}
			sort.Strings(keys)
			return keys
		}
	}
	t.Fatalf("policy has no variable %q", name)
	return nil
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestReservedKeysPolicy_MatchesTheKeyInventory: the policy reserves exactly
// the manager-owned keys and none a user sets.
func TestReservedKeysPolicy_MatchesTheKeyInventory(t *testing.T) {
	policy, binding := loadReservedKeysPolicy(t)
	assert.Equal(t, sorted(managerOwnedVMAnnotations), policyVariableKeys(t, policy, "reservedAnnotations"))
	assert.Equal(t, sorted(managerOwnedVMLabels), policyVariableKeys(t, policy, "reservedLabels"))
	reserved := append(policyVariableKeys(t, policy, "reservedAnnotations"), policyVariableKeys(t, policy, "reservedLabels")...)
	for _, k := range userSetVMKeys {
		assert.NotContains(t, reserved, k, "users set %s themselves", k)
	}

	require.NotNil(t, policy.Spec.FailurePolicy)
	assert.Equal(t, admissionregistrationv1.Fail, *policy.Spec.FailurePolicy)
	assert.Equal(t, policy.Name, binding.Spec.PolicyName)
	assert.Equal(t, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}, binding.Spec.ValidationActions)
	require.Len(t, policy.Spec.MatchConstraints.ResourceRules, 1)
	rule := policy.Spec.MatchConstraints.ResourceRules[0]
	assert.Equal(t, []string{"virtualmachines"}, rule.Resources, "status is not matched: metadata changes there are ignored")
	assert.ElementsMatch(t, []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
		rule.Operations)
}
