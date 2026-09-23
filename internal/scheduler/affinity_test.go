/*
Copyright 2025.

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

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

func TestEvalSelectorRequirement(t *testing.T) {
	vmLabels := map[string]string{"app": "db", "tier": "backend"}
	tests := []struct {
		name string
		req  v1beta1.VMSelectorRequirement
		want bool
	}{
		{"In match", v1beta1.VMSelectorRequirement{Key: "app", Operator: v1beta1.VMSelectorOpIn, Values: []string{"db", "cache"}}, true},
		{"In no match", v1beta1.VMSelectorRequirement{Key: "app", Operator: v1beta1.VMSelectorOpIn, Values: []string{"web"}}, false},
		{"In absent key", v1beta1.VMSelectorRequirement{Key: "missing", Operator: v1beta1.VMSelectorOpIn, Values: []string{"x"}}, false},
		{"NotIn match (value not listed)", v1beta1.VMSelectorRequirement{Key: "app", Operator: v1beta1.VMSelectorOpNotIn, Values: []string{"web"}}, true},
		{"NotIn no match (value listed)", v1beta1.VMSelectorRequirement{Key: "app", Operator: v1beta1.VMSelectorOpNotIn, Values: []string{"db"}}, false},
		{"NotIn absent key is satisfied", v1beta1.VMSelectorRequirement{Key: "missing", Operator: v1beta1.VMSelectorOpNotIn, Values: []string{"x"}}, true},
		{"Exists present", v1beta1.VMSelectorRequirement{Key: "tier", Operator: v1beta1.VMSelectorOpExists}, true},
		{"Exists absent", v1beta1.VMSelectorRequirement{Key: "missing", Operator: v1beta1.VMSelectorOpExists}, false},
		{"DoesNotExist absent", v1beta1.VMSelectorRequirement{Key: "missing", Operator: v1beta1.VMSelectorOpDoesNotExist}, true},
		{"DoesNotExist present", v1beta1.VMSelectorRequirement{Key: "app", Operator: v1beta1.VMSelectorOpDoesNotExist}, false},
		{"unknown operator fails closed", v1beta1.VMSelectorRequirement{Key: "app", Operator: "Bogus"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, evalSelectorRequirement(tt.req, vmLabels))
		})
	}
}

func TestTermMatchesVM(t *testing.T) {
	vmLabels := map[string]string{"app": "db", "tier": "backend"}

	t.Run("metav1 label selector matches", func(t *testing.T) {
		ok, err := termMatchesVM(v1beta1.VMAffinityTerm{LabelSelector: selEq("app", "db")}, vmLabels)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("metav1 label selector does not match", func(t *testing.T) {
		ok, err := termMatchesVM(v1beta1.VMAffinityTerm{LabelSelector: selEq("app", "web")}, vmLabels)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("empty term matches everything", func(t *testing.T) {
		ok, err := termMatchesVM(v1beta1.VMAffinityTerm{}, vmLabels)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("label selector AND matchExpressions both must hold", func(t *testing.T) {
		term := v1beta1.VMAffinityTerm{
			LabelSelector: selEq("app", "db"),
			MatchExpressions: []v1beta1.VMSelectorRequirement{
				{Key: "tier", Operator: v1beta1.VMSelectorOpIn, Values: []string{"backend"}},
			},
		}
		ok, err := termMatchesVM(term, vmLabels)
		require.NoError(t, err)
		assert.True(t, ok)

		// Flip the matchExpression so the AND fails even though LabelSelector holds.
		term.MatchExpressions[0].Values = []string{"frontend"}
		ok, err = termMatchesVM(term, vmLabels)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("malformed label selector errors", func(t *testing.T) {
		term := v1beta1.VMAffinityTerm{LabelSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: nil}, // In requires values
			},
		}}
		_, err := termMatchesVM(term, vmLabels)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "selector")
	})
}

func TestParseOvercommit(t *testing.T) {
	t.Run("nil defaults to 1.0/1.0", func(t *testing.T) {
		cpu, mem, err := parseOvercommit(nil)
		require.NoError(t, err)
		assert.Equal(t, 1.0, cpu)
		assert.Equal(t, 1.0, mem)
	})
	t.Run("empty strings default to 1.0", func(t *testing.T) {
		cpu, mem, err := parseOvercommit(&v1beta1.OvercommitRatios{})
		require.NoError(t, err)
		assert.Equal(t, 1.0, cpu)
		assert.Equal(t, 1.0, mem)
	})
	t.Run("parsed values", func(t *testing.T) {
		cpu, mem, err := parseOvercommit(&v1beta1.OvercommitRatios{CPU: "4.0", Memory: "1.5"})
		require.NoError(t, err)
		assert.Equal(t, 4.0, cpu)
		assert.Equal(t, 1.5, mem)
	})
	t.Run("malformed cpu errors", func(t *testing.T) {
		_, _, err := parseOvercommit(&v1beta1.OvercommitRatios{CPU: "x"})
		require.Error(t, err)
	})
	t.Run("non-positive memory errors", func(t *testing.T) {
		_, _, err := parseOvercommit(&v1beta1.OvercommitRatios{Memory: "-1"})
		require.Error(t, err)
	})
}
