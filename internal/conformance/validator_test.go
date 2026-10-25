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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// specWithStep wraps one step (YAML, indented for the steps list) in a test.
func specWithStep(step string) string {
	return "- name: t\n  description: d\n  steps:\n" + step
}

const vmResource = `      resource:
        apiVersion: infra.virtrigaud.io/v1beta1
        kind: VirtualMachine
        metadata:
          name: vcts-${RUN}-vm
`

func validateSpec(t *testing.T, content string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	_, err := NewValidator().ValidateFile(path)
	return err
}

func TestValidateFile(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		wantErr string
	}{
		{name: "create", spec: specWithStep("    - name: s\n      type: create\n" + vmResource)},
		{name: "wait with condition", spec: specWithStep("    - name: s\n      type: wait\n" + vmResource + "      waitFor:\n        condition: Ready\n")},
		{name: "wait with validate only", spec: specWithStep("    - name: s\n      type: wait\n" + vmResource +
			"      validate:\n        - path: .status.id\n          operator: exists\n")},
		{name: "wait without condition or validate", spec: specWithStep("    - name: s\n      type: wait\n" + vmResource), wantErr: "needs waitFor, validate"},
		{name: "wait without resource", spec: specWithStep("    - name: s\n      type: wait\n      waitFor:\n        condition: Ready\n"), wantErr: "resource is required"},
		{name: "unknown condition", spec: specWithStep("    - name: s\n      type: wait\n" + vmResource + "      waitFor:\n        condition: Happy\n"), wantErr: "invalid condition"},
		{
			name: "vm-only condition on another kind",
			spec: specWithStep("    - name: s\n      type: wait\n      resource:\n        apiVersion: infra.virtrigaud.io/v1beta1\n" +
				"        kind: VMSnapshot\n        metadata:\n          name: x\n      waitFor:\n        condition: Running\n"),
			wantErr: "VirtualMachine only",
		},
		{name: "Deleted with validate", spec: specWithStep("    - name: s\n      type: wait\n" + vmResource +
			"      waitFor:\n        condition: Deleted\n      validate:\n        - path: .status.id\n          operator: exists\n"), wantErr: "cannot be combined"},
		{name: "validate without entries", spec: specWithStep("    - name: s\n      type: validate\n" + vmResource), wantErr: "at least one entry"},
		{name: "delete with validate", spec: specWithStep("    - name: s\n      type: delete\n" + vmResource +
			"      validate:\n        - path: .status.id\n          operator: exists\n"), wantErr: "delete step cannot have validate"},
		{name: "waitFor on create", spec: specWithStep("    - name: s\n      type: create\n" + vmResource + "      waitFor:\n        condition: Ready\n"), wantErr: "only allowed on a wait step"},
		{
			name: "created name without RUN",
			spec: specWithStep("    - name: s\n      type: create\n      resource:\n        apiVersion: infra.virtrigaud.io/v1beta1\n" +
				"        kind: VMClass\n        metadata:\n          name: fixed\n"),
			wantErr: "must contain ${RUN}",
		},
		{
			name: "namespace set",
			spec: specWithStep("    - name: s\n      type: validate\n      resource:\n        apiVersion: infra.virtrigaud.io/v1beta1\n" +
				"        kind: VirtualMachine\n        metadata:\n          name: x\n          namespace: default\n" +
				"      validate:\n        - path: .status.id\n          operator: exists\n"),
			wantErr: "metadata.namespace must not be set",
		},
		{
			name: "another API group",
			spec: specWithStep("    - name: s\n      type: create\n      resource:\n        apiVersion: v1\n        kind: Secret\n" +
				"        metadata:\n          name: vcts-${RUN}-s\n"),
			wantErr: "apiVersion must be infra.virtrigaud.io/v1beta1",
		},
		{
			name: "unknown kind",
			spec: specWithStep("    - name: s\n      type: create\n      resource:\n        apiVersion: infra.virtrigaud.io/v1beta1\n" +
				"        kind: Gadget\n        metadata:\n          name: vcts-${RUN}-g\n"),
			wantErr: "unknown kind",
		},
		{
			name: "create a Provider",
			spec: specWithStep("    - name: s\n      type: create\n      resource:\n        apiVersion: infra.virtrigaud.io/v1beta1\n" +
				"        kind: Provider\n        metadata:\n          name: vcts-${RUN}-p\n"),
			wantErr: "cannot act on a Provider",
		},
		{
			name: "validate a Provider",
			spec: specWithStep("    - name: s\n      type: validate\n      resource:\n        apiVersion: infra.virtrigaud.io/v1beta1\n" +
				"        kind: Provider\n        metadata:\n          name: ${PROVIDER}\n" +
				"      validate:\n        - path: .status.healthy\n          value: true\n"),
		},
		{name: "malformed variable", spec: specWithStep("    - name: s\n      type: create\n      resource:\n        apiVersion: infra.virtrigaud.io/v1beta1\n" +
			"        kind: VMClass\n        metadata:\n          name: vcts-${RUN}-c\n        spec:\n          x: ${lower}\n"), wantErr: "malformed variable"},
		{name: "unknown capability", spec: "- name: t\n  description: d\n  requiredCapabilities: [vm-teleport]\n  steps:\n    - name: s\n      type: create\n" + vmResource, wantErr: "unknown capability"},
		{name: "unknown key", spec: "- name: t\n  description: d\n  stpes: []\n", wantErr: "stpes"},
		{name: "duplicate test", spec: strings.Repeat(specWithStep("    - name: s\n      type: create\n"+vmResource), 2), wantErr: "duplicate test name"},
		{name: "empty file", spec: "", wantErr: "no tests"},
		{name: "negative timeout", spec: "- name: t\n  description: d\n  timeout: -1m\n  steps:\n    - name: s\n      type: create\n" + vmResource, wantErr: "must be positive"},
		{name: "bad path", spec: specWithStep("    - name: s\n      type: validate\n" + vmResource + "      validate:\n        - path: status.id\n          operator: exists\n"), wantErr: "must start with"},
		{name: "gt needs a number", spec: specWithStep("    - name: s\n      type: validate\n" + vmResource + "      validate:\n        - path: .status.id\n          operator: gt\n          value: abc\n"), wantErr: "number or a quantity"},
		{name: "exists value must be bool", spec: specWithStep("    - name: s\n      type: validate\n" + vmResource + "      validate:\n        - path: .status.id\n          operator: exists\n          value: yes-please\n"), wantErr: "true or false"},
		{name: "bad regexp", spec: specWithStep("    - name: s\n      type: validate\n" + vmResource + "      validate:\n        - path: .status.id\n          operator: matches\n          value: \"(\"\n"), wantErr: "regular expression"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSpec(t, tt.spec)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestShippedSpecs checks the repository's specifications are valid and use
// only built-in variables plus IMAGE.
func TestShippedSpecs(t *testing.T) {
	r := NewRunner(Config{SpecDir: filepath.Join("..", "..", "test", "conformance", "specs")})
	tests, err := r.ListTests()
	require.NoError(t, err)
	require.NotEmpty(t, tests)
	for _, test := range tests {
		for _, name := range ReferencedVars(test) {
			if !IsBuiltinVar(name) {
				assert.Equal(t, "IMAGE", name, "test %s", test.Name)
			}
		}
	}
}

func TestVars(t *testing.T) {
	vars := map[string]string{"RUN": "abc123", "IMAGE": "ubuntu"}
	missing := map[string]bool{}
	got := substitute(map[string]interface{}{
		"name": "vcts-${RUN}-vm",
		"list": []interface{}{"${IMAGE}", 3, "${UNSET}"},
	}, vars, missing)
	assert.Equal(t, map[string]interface{}{
		"name": "vcts-abc123-vm",
		"list": []interface{}{"ubuntu", 3, "${UNSET}"},
	}, got)
	assert.Equal(t, map[string]bool{"UNSET": true}, missing)

	assert.NoError(t, checkPlaceholders("a-${RUN}-${X_1}"))
	assert.Error(t, checkPlaceholders("a-${run}"))
	assert.Error(t, checkPlaceholders("a-${RUN"))

	test := TestSpec{Steps: []TestStep{{
		Resource: map[string]interface{}{"metadata": map[string]interface{}{"name": "vcts-${RUN}-${TEST}"}},
		Validate: []Validation{{Value: "${IMAGE}"}},
	}}, Cleanup: []TestStep{{Resource: map[string]interface{}{"x": "${OTHER}"}}}}
	assert.Equal(t, []string{"IMAGE", "OTHER", "RUN", "TEST"}, ReferencedVars(test))
}

func TestProviderCapabilities(t *testing.T) {
	p := &infrav1beta1.Provider{}
	assert.Equal(t, []string{CapVMClone, CapVMCreate, CapVMDelete, CapVMPower, CapVMReconfigure}, ProviderCapabilities(p))

	p.Status.ReportedCapabilities = &infrav1beta1.ReportedCapabilities{SupportsSnapshots: true, SupportsLinkedClones: true}
	caps := ProviderCapabilities(p)
	assert.Contains(t, caps, CapVMSnapshot)
	assert.Contains(t, caps, CapVMCloneLinked)
	assert.NotContains(t, caps, CapVMSnapshotMemory)
	assert.NotContains(t, caps, CapVMReconfigureOnline)

	assert.Equal(t, []string{CapVMSnapshotMemory}, missingCapabilities([]string{CapVMCreate, CapVMSnapshotMemory}, caps))
	for _, c := range KnownCapabilities() {
		assert.True(t, isKnownCapability(c))
	}
	assert.False(t, isKnownCapability("vm-teleport"))
}

func TestReports(t *testing.T) {
	results := &Results{
		Provider: "p", Namespace: "ns", RunID: "abc123", Total: 3, Passed: 1, Failed: 1, Skipped: 1,
		Tests: []TestResult{
			{Name: "a", Status: StatusPassed, Steps: []StepResult{{Name: "s1", Status: StatusPassed}}},
			{Name: "b", Status: StatusFailed, Error: `step s: got "On" <want> & | more`,
				Steps:   []StepResult{{Name: "s", Status: StatusFailed, Error: `got "On"`}, {Name: "s2", Status: StatusSkipped}},
				Cleanup: []StepResult{{Name: "delete VirtualMachine/x", Status: StatusPassed}}},
			{Name: "c", Status: StatusSkipped, SkipReason: "Provider p does not offer vm-snapshot"},
		},
	}

	junit, err := junitReport(results)
	require.NoError(t, err)
	s := string(junit)
	assert.Contains(t, s, `<testsuite name="virtrigaud-conformance" tests="3" failures="1" skipped="1"`)
	assert.Contains(t, s, `message="step s: got &#34;On&#34; &lt;want&gt; &amp; | more"`)
	assert.Contains(t, s, `<skipped message="Provider p does not offer vm-snapshot">`)
	assert.Contains(t, s, "cleanup delete VirtualMachine/x: passed")

	md := markdownReport(results)
	assert.Contains(t, md, "- **Run ID**: abc123")
	assert.Contains(t, md, `\| more`)
	assert.Contains(t, md, "| c | skipped |")
	assert.Contains(t, md, "| cleanup: delete VirtualMachine/x | passed |")

	js, err := jsonReport(results)
	require.NoError(t, err)
	assert.Contains(t, string(js), `"skipReason": "Provider p does not offer vm-snapshot"`)
}
