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
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v2"
)

// Validator validates conformance test specifications
type Validator struct {
	validStepTypes  map[string]bool
	validOperators  map[string]bool
	validConditions map[string]bool
	namePattern     *regexp.Regexp
}

// NewValidator creates a new test specification validator
func NewValidator() *Validator {
	return &Validator{
		validStepTypes: map[string]bool{
			"create":   true,
			"update":   true,
			"delete":   true,
			"wait":     true,
			"validate": true,
		},
		validOperators: map[string]bool{
			"eq":       true,
			"ne":       true,
			"gt":       true,
			"lt":       true,
			"gte":      true,
			"lte":      true,
			"contains": true,
			"matches":  true,
			"exists":   true,
		},
		validConditions: map[string]bool{
			"Ready":       true,
			"Available":   true,
			"Running":     true,
			"Stopped":     true,
			"Deleted":     true,
			"Failed":      true,
			"Provisioned": true,
		},
		namePattern: regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`),
	}
}

// ValidateFile validates a test specification file
func (v *Validator) ValidateFile(filename string) ([]TestSpec, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	var tests []TestSpec
	if err := yaml.Unmarshal(data, &tests); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	for i, test := range tests {
		if err := v.ValidateTest(test); err != nil {
			return nil, fmt.Errorf("test %d (%s): %w", i, test.Name, err)
		}
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
	if test.Timeout != "" {
		if _, err := time.ParseDuration(test.Timeout); err != nil {
			return fmt.Errorf("invalid timeout format: %w", err)
		}
	}

	// Validate required capabilities
	for _, capability := range test.RequiredCapabilities {
		if capability == "" {
			return fmt.Errorf("empty capability in requiredCapabilities")
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
	if step.Timeout != "" {
		if _, err := time.ParseDuration(step.Timeout); err != nil {
			return fmt.Errorf("invalid timeout format: %w", err)
		}
	}

	// Type-specific validation
	switch step.Type {
	case "create", "update", "delete", "validate":
		if step.Resource == nil {
			return fmt.Errorf("resource is required for %s step", step.Type)
		}
		if err := v.ValidateResource(step.Resource); err != nil {
			return fmt.Errorf("invalid resource: %w", err)
		}

	case "wait":
		if step.WaitFor == nil {
			return fmt.Errorf("waitFor is required for wait step")
		}
		if err := v.ValidateWaitCondition(*step.WaitFor); err != nil {
			return fmt.Errorf("invalid wait condition: %w", err)
		}
	}

	// Validate validations
	for i, validation := range step.Validate {
		if err := v.ValidateValidation(validation); err != nil {
			return fmt.Errorf("validation %d: %w", i, err)
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

	if _, ok := apiVersion.(string); !ok {
		return fmt.Errorf("apiVersion must be a string")
	}

	kind, ok := resource["kind"]
	if !ok {
		return fmt.Errorf("kind is required")
	}

	if _, ok := kind.(string); !ok {
		return fmt.Errorf("kind must be a string")
	}

	// Validate metadata
	metadata, ok := resource["metadata"]
	if ok {
		if metaMap, ok := metadata.(map[string]interface{}); ok {
			if name, exists := metaMap["name"]; exists {
				if nameStr, ok := name.(string); ok {
					if !v.namePattern.MatchString(nameStr) {
						return fmt.Errorf("resource name must match pattern %s", v.namePattern.String())
					}
				}
			}
		}
	}

	return nil
}

// ValidateWaitCondition validates a wait condition
func (v *Validator) ValidateWaitCondition(condition WaitCondition) error {
	if condition.Condition == "" {
		return fmt.Errorf("condition is required")
	}

	if !v.validConditions[condition.Condition] {
		return fmt.Errorf("invalid condition: %s", condition.Condition)
	}

	if condition.Timeout != "" {
		if _, err := time.ParseDuration(condition.Timeout); err != nil {
			return fmt.Errorf("invalid timeout format: %w", err)
		}
	}

	return nil
}

// ValidateValidation validates a validation specification
func (v *Validator) ValidateValidation(validation Validation) error {
	if validation.Path == "" {
		return fmt.Errorf("path is required")
	}

	// Validate JSONPath format (basic check)
	if validation.Path[0] != '.' && validation.Path[0] != '$' {
		return fmt.Errorf("path must start with '.' or '$'")
	}

	if validation.Operator == "" {
		validation.Operator = "eq" // default operator
	}

	if !v.validOperators[validation.Operator] {
		return fmt.Errorf("invalid operator: %s", validation.Operator)
	}

	// Some operators don't require a value
	if validation.Operator != "exists" && validation.Value == nil {
		return fmt.Errorf("value is required for operator %s", validation.Operator)
	}

	return nil
}

// GetValidStepTypes returns valid step types
func (v *Validator) GetValidStepTypes() []string {
	types := make([]string, 0, len(v.validStepTypes))
	for stepType := range v.validStepTypes {
		types = append(types, stepType)
	}
	return types
}

// GetValidOperators returns valid validation operators
func (v *Validator) GetValidOperators() []string {
	operators := make([]string, 0, len(v.validOperators))
	for operator := range v.validOperators {
		operators = append(operators, operator)
	}
	return operators
}

// GetValidConditions returns valid wait conditions
func (v *Validator) GetValidConditions() []string {
	conditions := make([]string, 0, len(v.validConditions))
	for condition := range v.validConditions {
		conditions = append(conditions, condition)
	}
	return conditions
}
