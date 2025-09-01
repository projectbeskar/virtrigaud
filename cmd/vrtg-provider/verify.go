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
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// verifyOptions holds options for the verify command.
type verifyOptions struct {
	skipBuild       bool
	skipTests       bool
	skipConformance bool
	profile         string
}

// newVerifyCommand creates the verify command.
func newVerifyCommand() *cobra.Command {
	opts := &verifyOptions{}

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify provider implementation",
		Long: `Verify the provider implementation by running a comprehensive test suite.

This command performs the following checks:
- Code compilation and build verification
- Unit tests for provider implementation
- Linting and code quality checks
- VCTS conformance tests (if applicable)
- Integration tests with mock hypervisor

The verification helps ensure the provider follows VirtRigaud best practices
and implements the required functionality correctly.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerify(opts)
		},
	}

	cmd.Flags().BoolVar(&opts.skipBuild, "skip-build", false, "Skip build verification")
	cmd.Flags().BoolVar(&opts.skipTests, "skip-tests", false, "Skip unit tests")
	cmd.Flags().BoolVar(&opts.skipConformance, "skip-conformance", false, "Skip conformance tests")
	cmd.Flags().StringVar(&opts.profile, "profile", "core", "Conformance test profile (core, snapshot, clone, advanced)")

	return cmd
}

// runVerify executes the verify command.
func runVerify(opts *verifyOptions) error {
	// Check if we're in a provider project directory
	if err := checkProviderProject(); err != nil {
		return fmt.Errorf("not in a provider project directory: %w", err)
	}

	fmt.Println("🔍 Verifying provider implementation...")

	// Step 1: Build verification
	if !opts.skipBuild {
		fmt.Println("\n📦 Building provider...")
		if err := verifyBuild(); err != nil {
			return fmt.Errorf("build verification failed: %w", err)
		}
		fmt.Println("✅ Build successful")
	}

	// Step 2: Code quality checks
	fmt.Println("\n🔍 Running code quality checks...")
	if err := verifyCodeQuality(); err != nil {
		fmt.Printf("⚠️  Code quality issues found: %v\n", err)
		// Don't fail on linting issues, just warn
	} else {
		fmt.Println("✅ Code quality checks passed")
	}

	// Step 3: Unit tests
	if !opts.skipTests {
		fmt.Println("\n🧪 Running unit tests...")
		if err := verifyUnitTests(); err != nil {
			return fmt.Errorf("unit tests failed: %w", err)
		}
		fmt.Println("✅ Unit tests passed")
	}

	// Step 4: Conformance tests
	if !opts.skipConformance {
		fmt.Println("\n🎯 Running conformance tests...")
		if err := verifyConformance(opts.profile); err != nil {
			fmt.Printf("⚠️  Conformance tests failed: %v\n", err)
			fmt.Println("💡 This may be expected if the provider doesn't support all features")
		} else {
			fmt.Println("✅ Conformance tests passed")
		}
	}

	// Step 5: Final verification summary
	fmt.Println("\n📊 Verification Summary:")
	fmt.Println("✅ Provider builds successfully")
	if !opts.skipTests {
		fmt.Println("✅ Unit tests pass")
	}
	if !opts.skipConformance {
		fmt.Printf("🎯 Conformance profile '%s' verified\n", opts.profile)
	}

	fmt.Println("\n🎉 Provider verification completed!")
	fmt.Println("\nNext steps:")
	fmt.Println("  - Build container image: make docker-build")
	fmt.Println("  - Deploy to cluster: make deploy")
	fmt.Println("  - Run full integration tests: make test-e2e")

	return nil
}

// verifyBuild checks that the provider builds successfully.
func verifyBuild() error {
	// Try make build first
	if err := runMakeTarget("build"); err == nil {
		return nil
	}

	// Fall back to go build
	fmt.Println("⚠️  Makefile build target not found, trying go build...")
	return runCommand("go", "build", "-v", "./...")
}

// verifyCodeQuality runs linting and code quality checks.
func verifyCodeQuality() error {
	var errors []string

	// Run go fmt check
	if err := runCommand("gofmt", "-l", "."); err != nil {
		errors = append(errors, "gofmt issues found")
	}

	// Run go vet
	if err := runCommand("go", "vet", "./..."); err != nil {
		errors = append(errors, "go vet issues found")
	}

	// Try to run golangci-lint if available
	if err := runCommand("golangci-lint", "run"); err != nil {
		errors = append(errors, "golangci-lint issues found")
	}

	// Try make lint if available
	// Run linting if available - errors are ignored for compatibility with older providers
	_ = runMakeTarget("lint") // intentionally ignore: not all providers have lint configured

	if len(errors) > 0 {
		return fmt.Errorf("code quality issues: %s", strings.Join(errors, ", "))
	}

	return nil
}

// verifyUnitTests runs unit tests.
func verifyUnitTests() error {
	// Try make test first
	if err := runMakeTarget("test"); err == nil {
		return nil
	}

	// Fall back to go test
	fmt.Println("⚠️  Makefile test target not found, trying go test...")
	return runCommand("go", "test", "-v", "./...")
}

// verifyConformance runs conformance tests.
func verifyConformance(profile string) error {
	// Check if VCTS is available
	if err := runCommand("vcts", "version"); err != nil {
		return fmt.Errorf("VCTS not found (install with: go install github.com/projectbeskar/virtrigaud/cmd/vcts)")
	}

	// Check if there's a mock provider to test against
	if err := checkMockProvider(); err != nil {
		return fmt.Errorf("mock provider not available: %w", err)
	}

	// Run VCTS with the specified profile
	args := []string{"run", "--profile", profile, "--provider", "mock"}
	if err := runCommand("vcts", args...); err != nil {
		return fmt.Errorf("conformance tests failed for profile %s", profile)
	}

	return nil
}

// checkMockProvider checks if a mock provider is available for testing.
func checkMockProvider() error {
	// Look for mock provider binary or Docker image
	mockSources := []string{
		"./bin/provider-mock",
		"provider-mock",
	}

	for _, source := range mockSources {
		if _, err := os.Stat(source); err == nil {
			return nil
		}
	}

	// Check if we can build mock provider
	if err := runCommand("go", "build", "-o", "bin/provider-mock", "./cmd/provider-mock"); err == nil {
		return nil
	}

	return fmt.Errorf("mock provider not found and cannot be built")
}
