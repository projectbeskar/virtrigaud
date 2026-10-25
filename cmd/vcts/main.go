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

// Package main provides vcts, the VirtRigaud Conformance Test Suite CLI.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/projectbeskar/virtrigaud/cmd/internal/cliclient"
	"github.com/projectbeskar/virtrigaud/internal/conformance"
)

// defaultSpecDir is the directory 'vcts validate' reads when none is given.
const defaultSpecDir = "test/conformance/specs"

// errValidationFailed is returned by 'vcts validate' when a specification is
// invalid, so the process exits non-zero.
var errValidationFailed = errors.New("validation failed")

// errTestsFailed is returned by 'vcts run' when a conformance test fails.
var errTestsFailed = errors.New("conformance tests failed")

// options holds the values of vcts's flags.
type options struct {
	kubeconfig string
	namespace  string
	provider   string
	outputDir  string
	skipTests  []string
	timeout    time.Duration
	parallel   int
	verbose    bool
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// newRootCmd builds the vcts command tree. Every call returns an independent
// tree with its own flag values, so it can be run repeatedly in one process.
func newRootCmd() *cobra.Command {
	opts := &options{}

	rootCmd := &cobra.Command{
		Use:   "vcts",
		Short: "Virtrigaud Conformance Test Suite",
		Long: `VCTS (Virtrigaud Conformance Test Suite) runs standardized tests against
virtrigaud providers to verify compliance with the provider contract.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.PersistentFlags().StringVar(&opts.kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	rootCmd.PersistentFlags().StringVarP(&opts.namespace, "namespace", "n", "default", "Kubernetes namespace")
	rootCmd.PersistentFlags().BoolVarP(&opts.verbose, "verbose", "v", false, "Verbose output")

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run conformance tests",
		Long:  "Run conformance tests against a virtrigaud provider",
		Args:  cobra.NoArgs,
		RunE:  opts.runConformanceTests,
	}

	runCmd.Flags().StringVarP(&opts.provider, "provider", "p", "", "Provider name to test (required)")
	runCmd.Flags().StringVarP(&opts.outputDir, "output-dir", "o", "./conformance-results", "Output directory for test results")
	runCmd.Flags().StringSliceVar(&opts.skipTests, "skip", []string{}, "List of test names to skip")
	runCmd.Flags().DurationVar(&opts.timeout, "timeout", 30*time.Minute, "Test timeout")
	runCmd.Flags().IntVar(&opts.parallel, "parallel", 1, "Number of parallel test executions")
	_ = runCmd.MarkFlagRequired("provider")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List available tests",
		Long:  "List all available conformance tests",
		Args:  cobra.NoArgs,
		RunE:  listTests,
	}

	validateCmd := &cobra.Command{
		Use:   "validate [spec-dir]",
		Short: "Validate test specifications",
		Long: `Validate conformance test specifications for correctness.

Every *.yaml file in spec-dir (default ` + defaultSpecDir + `) is validated. The
command exits non-zero when a file cannot be read or holds an invalid test.`,
		Args: cobra.MaximumNArgs(1),
		RunE: validateTests,
	}

	rootCmd.AddCommand(runCmd, listCmd, validateCmd)

	return rootCmd
}

func (o *options) runConformanceTests(cmd *cobra.Command, _ []string) error {
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, o.timeout)
	defer cancel()

	// Create Kubernetes clients
	cfg, err := cliclient.RESTConfig(o.kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	k8sClient, err := cliclient.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes clientset: %w", err)
	}

	// Create output directory
	if err := os.MkdirAll(o.outputDir, 0o755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Create conformance runner
	runner := conformance.NewRunner(conformance.Config{
		KubeClient: k8sClient,
		Clientset:  clientset,
		Namespace:  o.namespace,
		Provider:   o.provider,
		OutputDir:  o.outputDir,
		SkipTests:  o.skipTests,
		Parallel:   o.parallel,
		Verbose:    o.verbose,
	})

	// Run tests
	results, err := runner.Run(ctx)
	if err != nil {
		return fmt.Errorf("failed to run conformance tests: %w", err)
	}

	// Print summary
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "\nConformance Test Results:\n")
	fmt.Fprintf(out, "Provider: %s\n", o.provider)
	fmt.Fprintf(out, "Total Tests: %d\n", results.Total)
	fmt.Fprintf(out, "Passed: %d\n", results.Passed)
	fmt.Fprintf(out, "Failed: %d\n", results.Failed)
	fmt.Fprintf(out, "Skipped: %d\n", results.Skipped)
	fmt.Fprintf(out, "Duration: %v\n", results.Duration)
	fmt.Fprintf(out, "\nResults saved to: %s\n", o.outputDir)

	if results.Failed > 0 {
		return errTestsFailed
	}

	return nil
}

func listTests(cmd *cobra.Command, _ []string) error {
	runner := conformance.NewRunner(conformance.Config{})
	tests, err := runner.ListTests()
	if err != nil {
		return fmt.Errorf("failed to list tests: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Available Conformance Tests:\n\n")
	for _, test := range tests {
		fmt.Fprintf(out, "  %s\n", test.Name)
		fmt.Fprintf(out, "    Description: %s\n", test.Description)
		fmt.Fprintf(out, "    Required Capabilities: %v\n", test.RequiredCapabilities)
		fmt.Fprintf(out, "\n")
	}

	return nil
}

func validateTests(cmd *cobra.Command, args []string) error {
	specDir := defaultSpecDir
	if len(args) > 0 {
		specDir = args[0]
	}

	// Find all test spec files
	specFiles, err := filepath.Glob(filepath.Join(specDir, "*.yaml"))
	if err != nil {
		return fmt.Errorf("failed to find test specs: %w", err)
	}

	if len(specFiles) == 0 {
		return fmt.Errorf("no test specification files found in %s", specDir)
	}

	out := cmd.OutOrStdout()
	validator := conformance.NewValidator()

	totalTests := 0
	invalidFiles := 0

	for _, specFile := range specFiles {
		fmt.Fprintf(out, "Validating %s...\n", specFile)

		tests, err := validator.ValidateFile(specFile)
		if err != nil {
			fmt.Fprintf(out, "  ERROR: %v\n", err)
			invalidFiles++
			continue
		}

		totalTests += len(tests)

		for _, test := range tests {
			fmt.Fprintf(out, "  ✓ %s\n", test.Name)
		}
	}

	fmt.Fprintf(out, "\nValidation Results:\n")
	fmt.Fprintf(out, "Files: %d\n", len(specFiles))
	fmt.Fprintf(out, "Valid Tests: %d\n", totalTests)
	fmt.Fprintf(out, "Invalid Files: %d\n", invalidFiles)

	if invalidFiles > 0 {
		return fmt.Errorf("%w: %d of %d specification files are invalid", errValidationFailed, invalidFiles, len(specFiles))
	}

	fmt.Fprintf(out, "All tests are valid!\n")
	return nil
}
