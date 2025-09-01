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

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/projectbeskar/virtrigaud/internal/conformance"
)

var (
	kubeconfig string
	namespace  string
	provider   string
	outputDir  string
	skipTests  []string
	timeout    time.Duration
	parallel   int
	verbose    bool
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "vcts",
		Short: "Virtrigaud Conformance Test Suite",
		Long: `VCTS (Virtrigaud Conformance Test Suite) runs standardized tests against 
virtrigaud providers to verify compliance with the provider contract.`,
	}

	rootCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Verbose output")

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run conformance tests",
		Long:  "Run conformance tests against a virtrigaud provider",
		RunE:  runConformanceTests,
	}

	runCmd.Flags().StringVarP(&provider, "provider", "p", "", "Provider name to test (required)")
	runCmd.Flags().StringVarP(&outputDir, "output-dir", "o", "./conformance-results", "Output directory for test results")
	runCmd.Flags().StringSliceVar(&skipTests, "skip", []string{}, "List of test names to skip")
	runCmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "Test timeout")
	runCmd.Flags().IntVar(&parallel, "parallel", 1, "Number of parallel test executions")
	_ = runCmd.MarkFlagRequired("provider")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List available tests",
		Long:  "List all available conformance tests",
		RunE:  listTests,
	}

	validateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate test specifications",
		Long:  "Validate conformance test specifications for correctness",
		RunE:  validateTests,
	}

	rootCmd.AddCommand(runCmd, listCmd, validateCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runConformanceTests(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Create Kubernetes clients
	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	k8sClient, err := client.New(cfg, client.Options{})
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes clientset: %w", err)
	}

	// Create output directory
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Create conformance runner
	runner := conformance.NewRunner(conformance.Config{
		KubeClient: k8sClient,
		Clientset:  clientset,
		Namespace:  namespace,
		Provider:   provider,
		OutputDir:  outputDir,
		SkipTests:  skipTests,
		Parallel:   parallel,
		Verbose:    verbose,
	})

	// Run tests
	results, err := runner.Run(ctx)
	if err != nil {
		return fmt.Errorf("failed to run conformance tests: %w", err)
	}

	// Print summary
	fmt.Printf("\nConformance Test Results:\n")
	fmt.Printf("Provider: %s\n", provider)
	fmt.Printf("Total Tests: %d\n", results.Total)
	fmt.Printf("Passed: %d\n", results.Passed)
	fmt.Printf("Failed: %d\n", results.Failed)
	fmt.Printf("Skipped: %d\n", results.Skipped)
	fmt.Printf("Duration: %v\n", results.Duration)
	fmt.Printf("\nResults saved to: %s\n", outputDir)

	if results.Failed > 0 {
		return fmt.Errorf("conformance tests failed")
	}

	return nil
}

func listTests(cmd *cobra.Command, args []string) error {
	runner := conformance.NewRunner(conformance.Config{})
	tests, err := runner.ListTests()
	if err != nil {
		return fmt.Errorf("failed to list tests: %w", err)
	}

	fmt.Printf("Available Conformance Tests:\n\n")
	for _, test := range tests {
		fmt.Printf("  %s\n", test.Name)
		fmt.Printf("    Description: %s\n", test.Description)
		fmt.Printf("    Required Capabilities: %v\n", test.RequiredCapabilities)
		fmt.Printf("\n")
	}

	return nil
}

func validateTests(cmd *cobra.Command, args []string) error {
	specDir := "test/conformance/specs"
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

	validator := conformance.NewValidator()

	totalTests := 0
	validTests := 0

	for _, specFile := range specFiles {
		fmt.Printf("Validating %s...\n", specFile)

		tests, err := validator.ValidateFile(specFile)
		if err != nil {
			fmt.Printf("  ERROR: %v\n", err)
			continue
		}

		totalTests += len(tests)
		validTests += len(tests)

		for _, test := range tests {
			fmt.Printf("  ✓ %s\n", test.Name)
		}
	}

	fmt.Printf("\nValidation Results:\n")
	fmt.Printf("Total Tests: %d\n", totalTests)
	fmt.Printf("Valid Tests: %d\n", validTests)
	fmt.Printf("Invalid Tests: %d\n", totalTests-validTests)

	if validTests != totalTests {
		return fmt.Errorf("validation failed")
	}

	fmt.Printf("All tests are valid!\n")
	return nil
}
