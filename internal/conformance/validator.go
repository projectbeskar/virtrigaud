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
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// Step types (TestStep.Type).
const (
	StepCreate   = "create"
	StepUpdate   = "update"
	StepDelete   = "delete"
	StepWait     = "wait"
	StepValidate = "validate"
)

// kindProvider is the Provider kind: a specification may read a Provider
// (wait, validate) but never create, change or delete one.
const kindProvider = "Provider"

// kindVirtualMachine is the VirtualMachine kind.
const kindVirtualMachine = "VirtualMachine"

// Validator validates conformance test specifications
type Validator struct {
	validStepTypes  map[string]bool
	validOperators  map[string]bool
	validConditions map[string]bool
	knownKinds      map[string]bool
	namePattern     *regexp.Regexp
}

// NewValidator creates a new test specification validator
func NewValidator() *Validator {
	return &Validator{
		validStepTypes: map[string]bool{
			StepCreate: true, StepUpdate: true, StepDelete: true, StepWait: true, StepValidate: true,
		},
		validOperators: map[string]bool{
			OpEq: true, OpNe: true, OpGt: true, OpLt: true, OpGte: true, OpLte: true,
			OpContains: true, OpMatches: true, OpExists: true,
		},
		validConditions: map[string]bool{
			CondReady: true, CondAvailable: true, CondRunning: true, CondStopped: true,
			CondDeleted: true, CondFailed: true, CondProvisioned: true,
		},
		knownKinds:  infraKinds(),
		namePattern: regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`),
	}
}

// infraKinds returns the kinds of the infra.virtrigaud.io/v1beta1 API group
// (list kinds left out).
func infraKinds() map[string]bool {
	scheme := runtime.NewScheme()
	kinds := map[string]bool{}
	if err := infrav1beta1.AddToScheme(scheme); err != nil {
		return kinds
	}
	pkg := reflect.TypeOf(infrav1beta1.VirtualMachine{}).PkgPath()
	for kind, t := range scheme.KnownTypes(infrav1beta1.GroupVersion) {
		if t.PkgPath() == pkg && !strings.HasSuffix(kind, "List") {
			kinds[kind] = true
		}
	}
	return kinds
}

// decodeSpecs decodes a specification file: a YAML list of tests. A key the
// format does not define is an error, so a misspelt key is not ignored.
func decodeSpecs(data []byte) ([]TestSpec, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var tests []TestSpec
	if err := dec.Decode(&tests); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}
	return tests, nil
}

// ValidateFile validates a test specification file
func (v *Validator) ValidateFile(filename string) ([]TestSpec, error) {
	data, err := os.ReadFile(filename) // #nosec G304 -- the operator names the specification files to read
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	tests, err := decodeSpecs(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	if len(tests) == 0 {
		return nil, errors.New("no tests in file")
	}

	seen := map[string]bool{}
	for i, test := range tests {
		if err := v.ValidateTest(test); err != nil {
			return nil, fmt.Errorf("test %d (%s): %w", i, test.Name, err)
		}
		if seen[test.Name] {
			return nil, fmt.Errorf("test %d (%s): duplicate test name", i, test.Name)
		}
		seen[test.Name] = true
	}

	return tests, nil
}

// ValidateTest validates a single test specification
func (v *Validator) ValidateTest(test TestSpec) error {
	// Validate test name
	if test.Name == "" {
		return fmt.Errorf("test name is required")
	}

	if !v.namePattern.MatchString(test.Name) {
		return fmt.Errorf("test name must match pattern %s", v.namePattern.String())
	}

	// Validate description
	if test.Description == "" {
		return fmt.Errorf("test description is required")
	}

	// Validate timeout
	if err := validDuration(test.Timeout); err != nil {
		return err
	}

	// Validate required capabilities
	for _, capability := range test.RequiredCapabilities {
		if capability == "" {
			return fmt.Errorf("empty capability in requiredCapabilities")
		}
		if !isKnownCapability(capability) {
			return fmt.Errorf("unknown capability %q in requiredCapabilities (known: %s)",
				capability, strings.Join(KnownCapabilities(), ", "))
		}
	}

	// Validate steps
	if len(test.Steps) == 0 {
		return fmt.Errorf("at least one step is required")
	}

	for i, step := range test.Steps {
		if err := v.ValidateStep(step); err != nil {
			return fmt.Errorf("step %d (%s): %w", i, step.Name, err)
		}
	}

	// Validate cleanup steps
	for i, step := range test.Cleanup {
		if err := v.ValidateStep(step); err != nil {
			return fmt.Errorf("cleanup step %d (%s): %w", i, step.Name, err)
		}
	}

	return nil
}

// validDuration checks a timeout string; empty means the default.
func validDuration(s string) error {
	if s == "" {
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid timeout format: %w", err)
	}
	if d <= 0 {
		return fmt.Errorf("timeout must be positive, got %s", s)
	}
	return nil
}

// ValidateStep validates a single test step
func (v *Validator) ValidateStep(step TestStep) error {
	// Validate step name
	if step.Name == "" {
		return fmt.Errorf("step name is required")
	}

	// Validate step type
	if step.Type == "" {
		return fmt.Errorf("step type is required")
	}

	if !v.validStepTypes[step.Type] {
		return fmt.Errorf("invalid step type: %s", step.Type)
	}

	// Validate timeout
	if err := validDuration(step.Timeout); err != nil {
		return err
	}

	// Every variable reference must be well-formed.
	for _, val := range stepStrings(step) {
		if err := walkStrings(val, checkPlaceholders); err != nil {
			return err
		}
	}

	// Every step names the object it acts on or reads.
	if step.Resource == nil {
		return fmt.Errorf("resource is required for %s step", step.Type)
	}
	if err := v.validateStepResource(step); err != nil {
		return fmt.Errorf("invalid resource: %w", err)
	}

	// Type-specific validation
	switch step.Type {
	case StepWait:
		if step.WaitFor == nil && len(step.Validate) == 0 {
			return fmt.Errorf("a wait step needs waitFor, validate, or both")
		}
		if step.WaitFor != nil {
			if err := v.validateWaitFor(*step.WaitFor, len(step.Validate) > 0, kindOf(step.Resource)); err != nil {
				return fmt.Errorf("invalid wait condition: %w", err)
			}
		}
	case StepValidate:
		if len(step.Validate) == 0 {
			return fmt.Errorf("a validate step needs at least one entry in validate")
		}
	case StepDelete:
		if len(step.Validate) > 0 {
			return fmt.Errorf("a delete step cannot have validate entries: use a wait step with condition Deleted")
		}
	}
	if step.WaitFor != nil && step.Type != StepWait {
		return fmt.Errorf("waitFor is only allowed on a wait step")
	}

	// Validate validations
	for i, validation := range step.Validate {
		if err := v.ValidateValidation(validation); err != nil {
			return fmt.Errorf("validation %d: %w", i, err)
		}
	}

	return nil
}

// kindOf returns the resource's kind, or "".
func kindOf(resource map[string]interface{}) string {
	kind, _ := resource["kind"].(string)
	return kind
}

// validateStepResource checks the resource of a step: it must name an
// infra.virtrigaud.io/v1beta1 object without a namespace (the runner uses
// --namespace); a created object's name must contain ${RUN}; a Provider is
// read-only.
func (v *Validator) validateStepResource(step TestStep) error {
	if err := v.ValidateResource(step.Resource); err != nil {
		return err
	}
	kind := kindOf(step.Resource)
	if kind == kindProvider && (step.Type == StepCreate || step.Type == StepUpdate || step.Type == StepDelete) {
		return fmt.Errorf("a %s step cannot act on a Provider: the suite tests the Provider given with --provider", step.Type)
	}
	if step.Type == StepCreate {
		name, _ := nestedString(step.Resource, "metadata", "name")
		if !strings.Contains(name, "${"+VarRun+"}") {
			return fmt.Errorf("metadata.name %q of a created object must contain ${%s}, so every run uses new names", name, VarRun)
		}
	}
	return nil
}

// ValidateResource validates a resource specification
func (v *Validator) ValidateResource(resource map[string]interface{}) error {
	// Check for required fields
	apiVersion, ok := resource["apiVersion"]
	if !ok {
		return fmt.Errorf("apiVersion is required")
	}

	apiVersionStr, ok := apiVersion.(string)
	if !ok {
		return fmt.Errorf("apiVersion must be a string")
	}
	if apiVersionStr != infrav1beta1.GroupVersion.String() {
		return fmt.Errorf("apiVersion must be %s", infrav1beta1.GroupVersion.String())
	}

	kind, ok := resource["kind"]
	if !ok {
		return fmt.Errorf("kind is required")
	}

	kindStr, ok := kind.(string)
	if !ok {
		return fmt.Errorf("kind must be a string")
	}
	if !v.knownKinds[kindStr] {
		return fmt.Errorf("unknown kind %q for %s", kindStr, apiVersionStr)
	}

	// Validate metadata
	metaMap, ok := resource["metadata"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("metadata.name is required")
	}
	if _, set := metaMap["namespace"]; set {
		return fmt.Errorf("metadata.namespace must not be set: the runner uses the namespace given with --namespace")
	}
	nameStr, ok := metaMap["name"].(string)
	if !ok || nameStr == "" {
		return fmt.Errorf("metadata.name is required")
	}
	sample := varPattern.ReplaceAllString(nameStr, sampleValue)
	if !v.namePattern.MatchString(sample) {
		return fmt.Errorf("resource name must match pattern %s", v.namePattern.String())
	}

	return nil
}

// nestedString returns the string at the path of keys in m.
func nestedString(m map[string]interface{}, keys ...string) (string, bool) {
	var cur interface{} = m
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return "", false
		}
		cur = mm[k]
	}
	s, ok := cur.(string)
	return s, ok
}

// validateWaitFor validates the waitFor of a wait step on an object of
// kind. The condition may be empty when the step has validate entries.
func (v *Validator) validateWaitFor(condition WaitCondition, hasValidate bool, kind string) error {
	if err := validDuration(condition.Timeout); err != nil {
		return err
	}
	if condition.Condition == "" {
		if hasValidate {
			return nil
		}
		return fmt.Errorf("condition is required")
	}
	if !v.validConditions[condition.Condition] {
		return fmt.Errorf("invalid condition: %s", condition.Condition)
	}
	if vmOnlyConditions[condition.Condition] && kind != kindVirtualMachine {
		return fmt.Errorf("condition %s applies to a VirtualMachine only, not %s", condition.Condition, kind)
	}
	if condition.Condition == CondDeleted && hasValidate {
		return fmt.Errorf("condition Deleted cannot be combined with validate entries")
	}
	return nil
}

// ValidateWaitCondition validates a wait condition
func (v *Validator) ValidateWaitCondition(condition WaitCondition) error {
	return v.validateWaitFor(condition, false, kindVirtualMachine)
}

// ValidateValidation validates a validation specification
func (v *Validator) ValidateValidation(validation Validation) error {
	if validation.Path == "" {
		return fmt.Errorf("path is required")
	}

	if _, err := compilePath(validation.Path); err != nil {
		return err
	}

	operator := validation.Operator
	if operator == "" {
		operator = OpEq // default operator
	}

	if !v.validOperators[operator] {
		return fmt.Errorf("invalid operator: %s", operator)
	}

	switch operator {
	case OpExists:
		if validation.Value != nil {
			if _, ok := validation.Value.(bool); !ok {
				return fmt.Errorf("value of operator exists must be true or false (or omitted)")
			}
		}
	case OpGt, OpLt, OpGte, OpLte:
		if s, ok := validation.Value.(string); ok && varPattern.MatchString(s) {
			return nil
		}
		if _, ok := toNumber(validation.Value); !ok {
			return fmt.Errorf("value of operator %s must be a number or a quantity", operator)
		}
	case OpMatches:
		s, ok := validation.Value.(string)
		if !ok {
			return fmt.Errorf("value of operator matches must be a regular expression string")
		}
		if !varPattern.MatchString(s) {
			if _, err := regexp.Compile(s); err != nil {
				return fmt.Errorf("invalid regular expression: %w", err)
			}
		}
	default:
		if validation.Value == nil {
			return fmt.Errorf("value is required for operator %s", operator)
		}
	}

	return nil
}

// GetValidStepTypes returns valid step types
func (v *Validator) GetValidStepTypes() []string {
	return sortedKeys(v.validStepTypes)
}

// GetValidOperators returns valid validation operators
func (v *Validator) GetValidOperators() []string {
	return sortedKeys(v.validOperators)
}

// GetValidConditions returns valid wait conditions
func (v *Validator) GetValidConditions() []string {
	return sortedKeys(v.validConditions)
}

// sortedKeys returns the keys of m, sorted.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// gvkOf returns the GroupVersionKind of a validated resource.
func gvkOf(resource map[string]interface{}) schema.GroupVersionKind {
	apiVersion, _ := resource["apiVersion"].(string)
	return schema.FromAPIVersionAndKind(apiVersion, kindOf(resource))
}
