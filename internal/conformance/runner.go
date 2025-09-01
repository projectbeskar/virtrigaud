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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v2"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// Config holds configuration for the conformance runner
type Config struct {
	KubeClient client.Client
	Clientset  kubernetes.Interface
	Namespace  string
	Provider   string
	OutputDir  string
	SkipTests  []string
	Parallel   int
	Verbose    bool
}

// Runner executes conformance tests
type Runner struct {
	config Config
	tests  []TestSpec
}

// TestSpec defines a conformance test
type TestSpec struct {
	Name                 string            `yaml:"name"`
	Description          string            `yaml:"description"`
	RequiredCapabilities []string          `yaml:"requiredCapabilities"`
	Steps                []TestStep        `yaml:"steps"`
	Cleanup              []TestStep        `yaml:"cleanup"`
	Timeout              string            `yaml:"timeout"`
	Labels               map[string]string `yaml:"labels"`
}

// TestStep defines a single test step
type TestStep struct {
	Name        string                 `yaml:"name"`
	Type        string                 `yaml:"type"` // create, update, delete, wait, validate
	Resource    map[string]interface{} `yaml:"resource"`
	Validate    []Validation           `yaml:"validate"`
	WaitFor     *WaitCondition         `yaml:"waitFor"`
	Timeout     string                 `yaml:"timeout"`
	Optional    bool                   `yaml:"optional"`
	Description string                 `yaml:"description"`
}

// Validation defines validation criteria
type Validation struct {
	Path     string      `yaml:"path"`     // JSONPath expression
	Value    interface{} `yaml:"value"`    // Expected value
	Operator string      `yaml:"operator"` // eq, ne, gt, lt, contains, etc.
}

// WaitCondition defines what to wait for
type WaitCondition struct {
	Condition string `yaml:"condition"` // Ready, Deleted, etc.
	Timeout   string `yaml:"timeout"`
}

// Results holds test execution results
type Results struct {
	Provider  string        `json:"provider"`
	Total     int           `json:"total"`
	Passed    int           `json:"passed"`
	Failed    int           `json:"failed"`
	Skipped   int           `json:"skipped"`
	Duration  time.Duration `json:"duration"`
	Tests     []TestResult  `json:"tests"`
	Timestamp time.Time     `json:"timestamp"`
}

// TestResult holds individual test results
type TestResult struct {
	Name         string        `json:"name"`
	Status       string        `json:"status"` // passed, failed, skipped
	Duration     time.Duration `json:"duration"`
	Error        string        `json:"error,omitempty"`
	Steps        []StepResult  `json:"steps"`
	Capabilities []string      `json:"capabilities"`
}

// StepResult holds individual step results
type StepResult struct {
	Name     string        `json:"name"`
	Status   string        `json:"status"`
	Duration time.Duration `json:"duration"`
	Error    string        `json:"error,omitempty"`
}

// NewRunner creates a new conformance test runner
func NewRunner(config Config) *Runner {
	return &Runner{
		config: config,
	}
}

// Run executes all conformance tests
func (r *Runner) Run(ctx context.Context) (*Results, error) {
	startTime := time.Now()

	// Load test specifications
	if err := r.loadTests(); err != nil {
		return nil, fmt.Errorf("failed to load tests: %w", err)
	}

	// Get provider capabilities
	capabilities, err := r.getProviderCapabilities(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get provider capabilities: %w", err)
	}

	// Filter tests based on capabilities and skip list
	filteredTests := r.filterTests(capabilities)

	results := &Results{
		Provider:  r.config.Provider,
		Total:     len(filteredTests),
		Timestamp: startTime,
		Tests:     make([]TestResult, 0, len(filteredTests)),
	}

	// Execute tests
	for _, test := range filteredTests {
		if r.shouldSkipTest(test.Name) {
			results.Skipped++
			results.Tests = append(results.Tests, TestResult{
				Name:   test.Name,
				Status: "skipped",
			})
			continue
		}

		result := r.runTest(ctx, test, capabilities)
		results.Tests = append(results.Tests, result)

		switch result.Status {
		case "passed":
			results.Passed++
		case "failed":
			results.Failed++
		case "skipped":
			results.Skipped++
		}

		if r.config.Verbose {
			r.printTestResult(result)
		}
	}

	results.Duration = time.Since(startTime)

	// Save results
	if err := r.saveResults(results); err != nil {
		return results, fmt.Errorf("failed to save results: %w", err)
	}

	return results, nil
}

// ListTests returns all available tests
func (r *Runner) ListTests() ([]TestSpec, error) {
	if err := r.loadTests(); err != nil {
		return nil, err
	}
	return r.tests, nil
}

// loadTests loads test specifications from files
func (r *Runner) loadTests() error {
	specDir := "test/conformance/specs"
	specFiles, err := filepath.Glob(filepath.Join(specDir, "*.yaml"))
	if err != nil {
		return fmt.Errorf("failed to find test specs: %w", err)
	}

	r.tests = []TestSpec{}
	for _, specFile := range specFiles {
		data, err := os.ReadFile(specFile)
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", specFile, err)
		}

		var tests []TestSpec
		if err := yaml.Unmarshal(data, &tests); err != nil {
			return fmt.Errorf("failed to parse %s: %w", specFile, err)
		}

		r.tests = append(r.tests, tests...)
	}

	return nil
}

// getProviderCapabilities retrieves provider capabilities
func (r *Runner) getProviderCapabilities(ctx context.Context) ([]string, error) {
	// Get the provider resource
	provider := &infrav1beta1.Provider{}
	key := client.ObjectKey{
		Namespace: r.config.Namespace,
		Name:      r.config.Provider,
	}

	if err := r.config.KubeClient.Get(ctx, key, provider); err != nil {
		return nil, fmt.Errorf("failed to get provider %s: %w", r.config.Provider, err)
	}

	// Extract capabilities from provider status
	// This would be populated by the provider's GetCapabilities RPC
	capabilities := []string{}
	// For now, return basic capabilities based on provider type
	if string(provider.Spec.Type) == "vsphere" {
		capabilities = append(capabilities, "vm-create", "vm-delete", "vm-power", "vm-reconfigure", "vm-snapshot")
	} else if string(provider.Spec.Type) == "libvirt" {
		capabilities = append(capabilities, "vm-create", "vm-delete", "vm-power")
	}

	return capabilities, nil
}

// filterTests filters tests based on provider capabilities
func (r *Runner) filterTests(capabilities []string) []TestSpec {
	filtered := []TestSpec{}
	capabilitySet := make(map[string]bool)
	for _, cap := range capabilities {
		capabilitySet[cap] = true
	}

	for _, test := range r.tests {
		// Check if provider has required capabilities
		hasAllCapabilities := true
		for _, required := range test.RequiredCapabilities {
			if !capabilitySet[required] {
				hasAllCapabilities = false
				break
			}
		}

		if hasAllCapabilities {
			filtered = append(filtered, test)
		}
	}

	return filtered
}

// shouldSkipTest checks if a test should be skipped
func (r *Runner) shouldSkipTest(testName string) bool {
	for _, skip := range r.config.SkipTests {
		if skip == testName {
			return true
		}
	}
	return false
}

// runTest executes a single test
func (r *Runner) runTest(ctx context.Context, test TestSpec, capabilities []string) TestResult {
	startTime := time.Now()

	result := TestResult{
		Name:         test.Name,
		Capabilities: capabilities,
		Steps:        make([]StepResult, 0, len(test.Steps)),
	}

	// Parse test timeout
	timeout := 5 * time.Minute
	if test.Timeout != "" {
		if d, err := time.ParseDuration(test.Timeout); err == nil {
			timeout = d
		}
	}

	testCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Execute test steps
	for _, step := range test.Steps {
		stepResult := r.runStep(testCtx, step)
		result.Steps = append(result.Steps, stepResult)

		if stepResult.Status == "failed" && !step.Optional {
			result.Status = "failed"
			result.Error = stepResult.Error
			break
		}
	}

	// If all steps passed, mark test as passed
	if result.Status == "" {
		result.Status = "passed"
	}

	result.Duration = time.Since(startTime)

	// Run cleanup steps
	r.runCleanup(ctx, test.Cleanup)

	return result
}

// runStep executes a single test step
func (r *Runner) runStep(ctx context.Context, step TestStep) StepResult {
	startTime := time.Now()

	result := StepResult{
		Name: step.Name,
	}

	// Parse step timeout
	timeout := 30 * time.Second
	if step.Timeout != "" {
		if d, err := time.ParseDuration(step.Timeout); err == nil {
			timeout = d
		}
	}

	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Execute step based on type
	var err error
	switch step.Type {
	case "create":
		err = r.createResource(stepCtx, step.Resource)
	case "update":
		err = r.updateResource(stepCtx, step.Resource)
	case "delete":
		err = r.deleteResource(stepCtx, step.Resource)
	case "wait":
		err = r.waitForCondition(stepCtx, step.WaitFor)
	case "validate":
		err = r.validateResource(stepCtx, step.Resource, step.Validate)
	default:
		err = fmt.Errorf("unknown step type: %s", step.Type)
	}

	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
	} else {
		result.Status = "passed"
	}

	result.Duration = time.Since(startTime)
	return result
}

// runCleanup runs cleanup steps
func (r *Runner) runCleanup(ctx context.Context, cleanup []TestStep) {
	for _, step := range cleanup {
		r.runStep(ctx, step)
	}
}

// createResource creates a Kubernetes resource
func (r *Runner) createResource(ctx context.Context, resource map[string]interface{}) error {
	// Convert to unstructured object and create via client
	// This is a simplified implementation
	return fmt.Errorf("createResource not implemented")
}

// updateResource updates a Kubernetes resource
func (r *Runner) updateResource(ctx context.Context, resource map[string]interface{}) error {
	return fmt.Errorf("updateResource not implemented")
}

// deleteResource deletes a Kubernetes resource
func (r *Runner) deleteResource(ctx context.Context, resource map[string]interface{}) error {
	return fmt.Errorf("deleteResource not implemented")
}

// waitForCondition waits for a specific condition
func (r *Runner) waitForCondition(ctx context.Context, condition *WaitCondition) error {
	return fmt.Errorf("waitForCondition not implemented")
}

// validateResource validates a resource against criteria
func (r *Runner) validateResource(ctx context.Context, resource map[string]interface{}, validations []Validation) error {
	return fmt.Errorf("validateResource not implemented")
}

// saveResults saves test results to files
func (r *Runner) saveResults(results *Results) error {
	// Save JSON results
	jsonFile := filepath.Join(r.config.OutputDir, "results.json")
	jsonData, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal JSON results: %w", err)
	}

	if err := os.WriteFile(jsonFile, jsonData, 0o644); err != nil {
		return fmt.Errorf("failed to write JSON results: %w", err)
	}

	// Save JUnit XML results
	junitFile := filepath.Join(r.config.OutputDir, "junit.xml")
	junitData := r.generateJUnitXML(results)
	if err := os.WriteFile(junitFile, []byte(junitData), 0o644); err != nil {
		return fmt.Errorf("failed to write JUnit results: %w", err)
	}

	// Save Markdown report
	markdownFile := filepath.Join(r.config.OutputDir, "report.md")
	markdownData := r.generateMarkdownReport(results)
	if err := os.WriteFile(markdownFile, []byte(markdownData), 0o644); err != nil {
		return fmt.Errorf("failed to write Markdown report: %w", err)
	}

	return nil
}

// generateJUnitXML generates JUnit XML format results
func (r *Runner) generateJUnitXML(results *Results) string {
	xml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="virtrigaud-conformance" tests="%d" failures="%d" skipped="%d" time="%.2f">
`, results.Total, results.Failed, results.Skipped, results.Duration.Seconds())

	for _, test := range results.Tests {
		xml += fmt.Sprintf(`  <testcase name="%s" time="%.2f"`, test.Name, test.Duration.Seconds())

		switch test.Status {
		case "failed":
			xml += fmt.Sprintf(`>
    <failure message="%s">%s</failure>
  </testcase>
`, test.Error, test.Error)
		case "skipped":
			xml += `>
    <skipped/>
  </testcase>
`
		default:
			xml += "/>\n"
		}
	}

	xml += "</testsuite>\n"
	return xml
}

// generateMarkdownReport generates a Markdown report
func (r *Runner) generateMarkdownReport(results *Results) string {
	report := fmt.Sprintf(`# Virtrigaud Conformance Test Report

## Summary

- **Provider**: %s
- **Total Tests**: %d
- **Passed**: %d
- **Failed**: %d
- **Skipped**: %d
- **Duration**: %v
- **Timestamp**: %s

## Test Results

| Test Name | Status | Duration | Error |
|-----------|--------|----------|-------|
`, results.Provider, results.Total, results.Passed, results.Failed, results.Skipped,
		results.Duration, results.Timestamp.Format(time.RFC3339))

	for _, test := range results.Tests {
		status := test.Status
		switch test.Status {
		case "passed":
			status = "✅ Passed"
		case "failed":
			status = "❌ Failed"
		case "skipped":
			status = "⏭️ Skipped"
		}

		error := test.Error
		if error == "" {
			error = "-"
		}

		report += fmt.Sprintf("| %s | %s | %v | %s |\n",
			test.Name, status, test.Duration, error)
	}

	return report
}

// printTestResult prints a test result to stdout
func (r *Runner) printTestResult(result TestResult) {
	status := "✅"
	switch result.Status {
	case "failed":
		status = "❌"
	case "skipped":
		status = "⏭️"
	}

	fmt.Printf("%s %s (%v)\n", status, result.Name, result.Duration)
	if result.Error != "" {
		fmt.Printf("   Error: %s\n", result.Error)
	}
}
