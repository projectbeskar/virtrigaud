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

package conformance

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// vmObject is an observed VirtualMachine as the API returns it (JSON types).
func vmObject() map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "infra.virtrigaud.io/v1beta1",
		"kind":       "VirtualMachine",
		"metadata": map[string]interface{}{
			"name":       "vm",
			"generation": int64(3),
			"labels":     map[string]interface{}{"vcts.virtrigaud.io/run": "abc123"},
		},
		"spec": map[string]interface{}{"powerState": "On"},
		"status": map[string]interface{}{
			"id":               "vm-42",
			"powerState":       "On",
			"phase":            "Running",
			"ips":              []interface{}{"192.0.2.10", "2001:db8::1"},
			"currentResources": map[string]interface{}{"cpu": int64(2), "memoryMiB": int64(2048)},
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True", "reason": "ReconcileSuccess", "observedGeneration": int64(3)},
				map[string]interface{}{"type": "Reconfiguring", "status": "False", "reason": "ReconcileSuccess"},
			},
		},
	}
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name    string
		v       Validation
		wantErr string
	}{
		{name: "eq string", v: Validation{Path: ".status.powerState", Operator: OpEq, Value: "On"}},
		{name: "default operator is eq", v: Validation{Path: ".status.powerState", Value: "On"}},
		{name: "eq mismatch", v: Validation{Path: ".status.powerState", Operator: OpEq, Value: "Off"}, wantErr: `got "On", want eq "Off"`},
		{name: "eq number from YAML int", v: Validation{Path: ".status.currentResources.cpu", Value: 2}},
		{name: "eq number written as string", v: Validation{Path: ".status.currentResources.cpu", Value: "2"}},
		{name: "ne", v: Validation{Path: ".status.phase", Operator: OpNe, Value: "Failed"}},
		{name: "ne mismatch", v: Validation{Path: ".status.phase", Operator: OpNe, Value: "Running"}, wantErr: "want ne"},
		{name: "gt", v: Validation{Path: ".status.currentResources.memoryMiB", Operator: OpGt, Value: 1024}},
		{name: "gte", v: Validation{Path: ".status.currentResources.cpu", Operator: OpGte, Value: 2}},
		{name: "lt fails", v: Validation{Path: ".status.currentResources.cpu", Operator: OpLt, Value: 2}, wantErr: "want lt"},
		{name: "lte", v: Validation{Path: ".status.currentResources.cpu", Operator: OpLte, Value: 2}},
		{name: "gt on a string", v: Validation{Path: ".status.phase", Operator: OpGt, Value: 1}, wantErr: "not a number"},
		{name: "contains list element", v: Validation{Path: ".status.ips", Operator: OpContains, Value: "192.0.2.10"}},
		{name: "contains wildcard results", v: Validation{Path: ".status.ips[*]", Operator: OpContains, Value: "2001:db8::1"}},
		{name: "contains substring", v: Validation{Path: ".status.id", Operator: OpContains, Value: "vm-"}},
		{name: "contains missing", v: Validation{Path: ".status.ips", Operator: OpContains, Value: "198.51.100.1"}, wantErr: "does not contain"},
		{name: "matches", v: Validation{Path: ".status.id", Operator: OpMatches, Value: "^vm-[0-9]+$"}},
		{name: "matches mismatch", v: Validation{Path: ".status.id", Operator: OpMatches, Value: "^x"}, wantErr: "want matches"},
		{name: "exists", v: Validation{Path: ".status.id", Operator: OpExists}},
		{name: "exists true", v: Validation{Path: ".status.id", Operator: OpExists, Value: true}},
		{name: "exists missing", v: Validation{Path: ".status.consoleURL", Operator: OpExists}, wantErr: "not set"},
		{name: "exists false", v: Validation{Path: ".status.consoleURL", Operator: OpExists, Value: false}},
		{name: "exists false but set", v: Validation{Path: ".status.id", Operator: OpExists, Value: false}, wantErr: "want it not set"},
		{name: "missing field for eq", v: Validation{Path: ".status.message", Value: "x"}, wantErr: "not set"},
		{name: "filter expression", v: Validation{Path: `.status.conditions[?(@.type=="Ready")].status`, Value: "True"}},
		{name: "dollar root", v: Validation{Path: "$.status.phase", Value: "Running"}},
		{name: "escaped label key", v: Validation{Path: `.metadata.labels.vcts\.virtrigaud\.io/run`, Value: "abc123"}},
		{name: "several values for eq", v: Validation{Path: ".status.ips[*]", Value: "192.0.2.10"}, wantErr: "exactly one"},
		{name: "bad path", v: Validation{Path: "status.id", Value: "x"}, wantErr: "must start with"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := evaluate(vmObject(), tt.v)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestConditionMet(t *testing.T) {
	obj := &unstructured.Unstructured{Object: vmObject()}

	ok, _ := conditionMet(obj, CondReady)
	assert.True(t, ok)
	ok, desc := conditionMet(obj, CondAvailable)
	assert.False(t, ok)
	assert.Contains(t, desc, "no Available condition")
	ok, _ = conditionMet(obj, CondRunning)
	assert.True(t, ok)
	ok, desc = conditionMet(obj, CondStopped)
	assert.False(t, ok)
	assert.Contains(t, desc, `status.powerState="On"`)
	ok, _ = conditionMet(obj, CondProvisioned)
	assert.True(t, ok)
	ok, _ = conditionMet(obj, CondFailed)
	assert.False(t, ok)
	ok, _ = conditionMet(obj, CondDeleted)
	assert.False(t, ok)

	t.Run("a Ready condition older than the generation is not met", func(t *testing.T) {
		stale := &unstructured.Unstructured{Object: vmObject()}
		stale.SetGeneration(4)
		ok, desc := conditionMet(stale, CondReady)
		assert.False(t, ok)
		assert.Contains(t, desc, "observedGeneration 3 < generation 4")
	})

	t.Run("failed phase", func(t *testing.T) {
		failed := &unstructured.Unstructured{Object: vmObject()}
		require.NoError(t, unstructured.SetNestedField(failed.Object, "Failed", "status", "phase"))
		require.NoError(t, unstructured.SetNestedField(failed.Object, "create refused", "status", "message"))
		ok, _ := conditionMet(failed, CondFailed)
		assert.True(t, ok)
		assert.Contains(t, failedPhase(failed), `message="create refused"`)
		assert.Empty(t, failedPhase(obj))
	})
}
