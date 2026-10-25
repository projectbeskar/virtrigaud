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

// Package conformance loads, validates and runs the VirtRigaud conformance
// test specifications (test/conformance/specs) for the vcts command. A run
// creates objects through the Kubernetes API in one namespace, waits for the
// operator and the Provider under test to act on them, checks their status,
// and deletes everything it created.
package conformance

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// Test and step result statuses.
const (
	StatusPassed  = "passed"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
)

// DefaultSpecDir is where the specifications are read from when
// Config.SpecDir is empty, relative to the working directory.
const DefaultSpecDir = "test/conformance/specs"

// Labels the runner puts on every object it creates. Cleanup finds objects
// created on the run's behalf (a clone's target VM) by the run and test
// labels.
const (
	// LabelRun holds the run id (${RUN}).
	LabelRun = "vcts.virtrigaud.io/run"
	// LabelTest holds the test name (${TEST}).
	LabelTest = "vcts.virtrigaud.io/test"
	// LabelManagedBy is the standard managed-by label; its value is
	// managedByValue.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	managedByValue = "vcts"
)

// Defaults for the timeouts a specification or Config leaves unset.
const (
	// DefaultTestTimeout bounds a test's steps when the test sets no timeout.
	DefaultTestTimeout = 15 * time.Minute
	// DefaultStepTimeout bounds a create, update, delete or validate step.
	DefaultStepTimeout = time.Minute
	// DefaultWaitTimeout bounds a wait step with neither waitFor.timeout nor
	// a step timeout.
	DefaultWaitTimeout = 5 * time.Minute
	// DefaultCleanupTimeout bounds the cleanup of one test.
	DefaultCleanupTimeout = 10 * time.Minute
	// DefaultPollInterval is how often a wait step reads the object.
	DefaultPollInterval = 2 * time.Second
)

// runIDLength is the number of characters of a generated run id.
const runIDLength = 6

// runIDAlphabet is what a generated run id is made of: it is valid in object
// names and label values.
const runIDAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// Config holds configuration for the conformance runner
type Config struct {
	// KubeClient reads and writes the objects. Its scheme must know the
	// VirtRigaud types (cmd/internal/cliclient builds one).
	KubeClient client.Client
	// Clientset is kept for callers that set it; the runner does not use it.
	Clientset kubernetes.Interface
	// Namespace is where every object is created and read.
	Namespace string
	// Provider is the name of the Provider under test, in Namespace.
	Provider string
	// OutputDir receives results.json, junit.xml and report.md.
	OutputDir string
	// SkipTests names tests to report as skipped without running them.
	SkipTests []string
	// Parallel is how many tests run at once (at least 1).
	Parallel int
	// Verbose prints each step's result to Out as it finishes.
	Verbose bool

	// SpecDir is the directory of *.yaml specifications (DefaultSpecDir when
	// empty).
	SpecDir string
	// Vars are the user variables (--set NAME=value) the specifications
	// refer to as ${NAME}.
	Vars map[string]string
	// Keep leaves every object the run created in place (no cleanup).
	Keep bool
	// CleanupTimeout bounds the cleanup of one test (DefaultCleanupTimeout
	// when zero).
	CleanupTimeout time.Duration
	// PollInterval is how often a wait step reads its object
	// (DefaultPollInterval when zero).
	PollInterval time.Duration
	// RunID is the run id (${RUN}); a random one is generated when empty.
	RunID string
	// Out receives progress output (nothing is printed when nil).
	Out io.Writer
}

// Runner executes conformance tests
type Runner struct {
	config Config
	tests  []TestSpec
	outMu  sync.Mutex
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
	Namespace string        `json:"namespace"`
	RunID     string        `json:"runID"`
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
	Name     string        `json:"name"`
	Status   string        `json:"status"` // passed, failed, skipped
	Duration time.Duration `json:"duration"`
	Error    string        `json:"error,omitempty"`
	// SkipReason says why a skipped test was not run.
	SkipReason string       `json:"skipReason,omitempty"`
	Steps      []StepResult `json:"steps"`
	// Cleanup holds the results of the specification's cleanup steps and of
	// the automatic deletion of the objects the test created.
	Cleanup []StepResult `json:"cleanup,omitempty"`
	// Kept lists the objects left in place because of --keep.
	Kept         []string `json:"kept,omitempty"`
	Capabilities []string `json:"capabilities"`
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

// specDir returns the directory the specifications are read from.
func (r *Runner) specDir() string {
	if r.config.SpecDir != "" {
		return r.config.SpecDir
	}
	return DefaultSpecDir
}

// RunID returns the run's id, generating it on first use.
func (r *Runner) RunID() (string, error) {
	if r.config.RunID != "" {
		return r.config.RunID, nil
	}
	id, err := newRunID()
	if err != nil {
		return "", err
	}
	r.config.RunID = id
	return id, nil
}

// newRunID returns a random run id of runIDLength characters.
func newRunID() (string, error) {
	b := make([]byte, runIDLength)
	max := big.NewInt(int64(len(runIDAlphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate run id: %w", err)
		}
		b[i] = runIDAlphabet[n.Int64()]
	}
	return string(b), nil
}

// Run executes all conformance tests. It returns an error, and runs nothing,
// when the specifications are invalid, the Provider cannot be read, or a test
// to run refers to a variable that is not set. A test that fails is reported
// in the Results, not as an error.
func (r *Runner) Run(ctx context.Context) (*Results, error) {
	startTime := time.Now()

	// Load test specifications
	if err := r.loadTests(); err != nil {
		return nil, fmt.Errorf("failed to load tests: %w", err)
	}
	if r.config.KubeClient == nil {
		return nil, errors.New("no Kubernetes client configured")
	}
	runID, err := r.RunID()
	if err != nil {
		return nil, err
	}

	// Get provider capabilities
	capabilities, err := r.getProviderCapabilities(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get provider capabilities: %w", err)
	}

	results := &Results{
		Provider:  r.config.Provider,
		Namespace: r.config.Namespace,
		RunID:     runID,
		Total:     len(r.tests),
		Timestamp: startTime,
		Tests:     make([]TestResult, len(r.tests)),
	}

	// Decide which tests run; the others are reported as skipped.
	var toRun []int
	for i, test := range r.tests {
		if reason := r.skipReason(test, capabilities); reason != "" {
			results.Tests[i] = TestResult{
				Name: test.Name, Status: StatusSkipped, SkipReason: reason, Capabilities: capabilities,
			}
			r.printf("SKIPPED %s: %s\n", test.Name, reason)
			continue
		}
		toRun = append(toRun, i)
	}
	if err := r.checkVars(toRun); err != nil {
		return nil, err
	}

	r.printf("Run %s: %d tests (%d to run) against Provider %s/%s\n",
		runID, len(r.tests), len(toRun), r.config.Namespace, r.config.Provider)

	parallel := r.config.Parallel
	if parallel < 1 {
		parallel = 1
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for _, i := range toRun {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			// Interrupted or out of time: the remaining tests are not started.
			results.Tests[i] = TestResult{
				Name: r.tests[i].Name, Status: StatusSkipped, Capabilities: capabilities,
				SkipReason: fmt.Sprintf("not run: %v", context.Cause(ctx)),
			}
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			results.Tests[i] = r.runTest(ctx, r.tests[i], capabilities)
			r.printTestResult(results.Tests[i])
		}(i)
	}
	wg.Wait()

	for _, result := range results.Tests {
		switch result.Status {
		case StatusPassed:
			results.Passed++
		case StatusFailed:
			results.Failed++
		case StatusSkipped:
			results.Skipped++
		}
	}
	results.Duration = time.Since(startTime)

	// Save results
	if err := r.saveResults(results); err != nil {
		return results, fmt.Errorf("failed to save results: %w", err)
	}

	return results, nil
}

// skipReason returns why test is not run, or "" when it runs.
func (r *Runner) skipReason(test TestSpec, capabilities []string) string {
	if r.shouldSkipTest(test.Name) {
		return "skipped by --skip"
	}
	if missing := missingCapabilities(test.RequiredCapabilities, capabilities); len(missing) > 0 {
		return fmt.Sprintf("Provider %s does not offer %s", r.config.Provider, strings.Join(missing, ", "))
	}
	return ""
}

// checkVars returns an error naming every variable the tests at idx refer
// to that is neither built in nor set.
func (r *Runner) checkVars(idx []int) error {
	missing := map[string][]string{}
	for _, i := range idx {
		for _, name := range ReferencedVars(r.tests[i]) {
			if IsBuiltinVar(name) {
				continue
			}
			if _, ok := r.config.Vars[name]; !ok {
				missing[name] = append(missing[name], r.tests[i].Name)
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	names := make([]string, 0, len(missing))
	for n := range missing {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("${%s} (used by %s; pass --set %s=<value>)", n, strings.Join(missing[n], ", "), n))
	}
	return fmt.Errorf("variables not set: %s", strings.Join(parts, "; "))
}

// ListTests returns all available tests
func (r *Runner) ListTests() ([]TestSpec, error) {
	if err := r.loadTests(); err != nil {
		return nil, err
	}
	return r.tests, nil
}

// loadTests loads and validates the test specifications from the spec
// directory. Test names must be unique across the files.
func (r *Runner) loadTests() error {
	specDir := r.specDir()
	specFiles, err := filepath.Glob(filepath.Join(specDir, "*.yaml"))
	if err != nil {
		return fmt.Errorf("failed to find test specs: %w", err)
	}
	if len(specFiles) == 0 {
		return fmt.Errorf("no test specification files (*.yaml) in %s", specDir)
	}
	sort.Strings(specFiles)

	validator := NewValidator()
	r.tests = []TestSpec{}
	seen := map[string]string{}
	for _, specFile := range specFiles {
		tests, err := validator.ValidateFile(specFile)
		if err != nil {
			return fmt.Errorf("invalid specification %s: %w", specFile, err)
		}
		for _, test := range tests {
			if other, dup := seen[test.Name]; dup {
				return fmt.Errorf("test %s is defined in both %s and %s", test.Name, other, specFile)
			}
			seen[test.Name] = specFile
		}
		r.tests = append(r.tests, tests...)
	}

	return nil
}

// getProviderCapabilities reads the Provider under test and returns the
// capabilities it offers (see ProviderCapabilities).
func (r *Runner) getProviderCapabilities(ctx context.Context) ([]string, error) {
	provider := &infrav1beta1.Provider{}
	key := client.ObjectKey{
		Namespace: r.config.Namespace,
		Name:      r.config.Provider,
	}

	if err := r.config.KubeClient.Get(ctx, key, provider); err != nil {
		return nil, fmt.Errorf("failed to get provider %s: %w", r.config.Provider, err)
	}
	if !provider.Status.Healthy {
		r.printf("Warning: Provider %s/%s does not report status.healthy=true; tests may time out\n",
			r.config.Namespace, r.config.Provider)
	}
	if provider.Status.ReportedCapabilities == nil {
		r.printf("Warning: Provider %s/%s has not reported its capabilities; only the core capabilities (%s) are assumed\n",
			r.config.Namespace, r.config.Provider, strings.Join(coreCapabilities, ", "))
	}

	return ProviderCapabilities(provider), nil
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

// testTimeout returns the timeout of test's steps.
func testTimeout(test TestSpec) time.Duration {
	if d, err := time.ParseDuration(test.Timeout); err == nil && d > 0 {
		return d
	}
	return DefaultTestTimeout
}

// runTest executes a single test: its steps, bounded by the test timeout,
// then — whatever happened, unless Config.Keep is set — its cleanup, which
// is not bounded by the test timeout nor stopped by ctx being cancelled.
func (r *Runner) runTest(ctx context.Context, test TestSpec, capabilities []string) TestResult {
	startTime := time.Now()

	result := TestResult{
		Name:         test.Name,
		Capabilities: capabilities,
		Steps:        make([]StepResult, 0, len(test.Steps)),
	}
	tr := r.newTestRun(test)

	testCtx, cancel := context.WithTimeout(ctx, testTimeout(test))
	defer cancel()

	failedStep := ""
	for _, step := range test.Steps {
		if failedStep != "" {
			result.Steps = append(result.Steps, StepResult{
				Name: step.Name, Status: StatusSkipped,
				Error: fmt.Sprintf("not run: step %s failed", failedStep),
			})
			continue
		}
		stepResult := tr.runStep(testCtx, step, false)
		result.Steps = append(result.Steps, stepResult)
		r.printStep(test.Name, "", stepResult)

		if stepResult.Status == StatusFailed && !step.Optional {
			result.Status = StatusFailed
			result.Error = fmt.Sprintf("step %s: %s", step.Name, stepResult.Error)
			failedStep = step.Name
		}
	}

	// If all steps passed, mark test as passed
	if result.Status == "" {
		result.Status = StatusPassed
	}

	if r.config.Keep {
		result.Kept = tr.trackedNames()
	} else {
		result.Cleanup = tr.cleanup(ctx)
		for _, c := range result.Cleanup {
			r.printStep(test.Name, "cleanup ", c)
			if c.Status == StatusFailed {
				if result.Status == StatusPassed {
					result.Status = StatusFailed
					result.Error = fmt.Sprintf("cleanup %s: %s", c.Name, c.Error)
				}
			}
		}
	}

	result.Duration = time.Since(startTime)
	return result
}

// printf writes progress output to Config.Out.
func (r *Runner) printf(format string, args ...interface{}) {
	if r.config.Out == nil {
		return
	}
	r.outMu.Lock()
	defer r.outMu.Unlock()
	_, _ = fmt.Fprintf(r.config.Out, format, args...)
}

// printStep prints a step result when Config.Verbose is set.
func (r *Runner) printStep(test, prefix string, s StepResult) {
	if !r.config.Verbose {
		return
	}
	line := fmt.Sprintf("  [%s] %s%s: %s (%v)", test, prefix, s.Name, s.Status, s.Duration.Round(time.Millisecond))
	if s.Error != "" {
		line += ": " + s.Error
	}
	r.printf("%s\n", line)
}

// printTestResult prints a test result.
func (r *Runner) printTestResult(result TestResult) {
	line := fmt.Sprintf("%s %s (%v)", strings.ToUpper(result.Status), result.Name, result.Duration.Round(time.Millisecond))
	if result.Error != "" {
		line += ": " + result.Error
	}
	r.printf("%s\n", line)
}

// saveResults saves test results to files
func (r *Runner) saveResults(results *Results) error {
	if r.config.OutputDir == "" {
		return nil
	}
	files := map[string]func(*Results) ([]byte, error){
		"results.json": jsonReport,
		"junit.xml":    junitReport,
		"report.md":    func(res *Results) ([]byte, error) { return []byte(markdownReport(res)), nil },
	}
	for name, render := range files {
		data, err := render(results)
		if err != nil {
			return fmt.Errorf("render %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(r.config.OutputDir, name), data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}
