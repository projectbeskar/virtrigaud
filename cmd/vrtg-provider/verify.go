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
	"errors"
	"fmt"
	"os/exec"
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
		Long: `Verify the provider implementation by running the project's own checks.

This command performs the following checks:
- Code compilation and build verification (the "build" make target, else go build)
- Formatting, go vet and, when installed, golangci-lint (reported, not fatal)
- Unit tests (the "test" make target, else go test)

It does not run the VCTS conformance suite: that needs the provider deployed in
a cluster. Deploy it, then run "vcts run --provider <name>".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVerify(opts)
		},
	}

	cmd.Flags().BoolVar(&opts.skipBuild, "skip-build", false, "Skip build verification")
	cmd.Flags().BoolVar(&opts.skipTests, "skip-tests", false, "Skip unit tests")
	cmd.Flags().BoolVar(&opts.skipConformance, "skip-conformance", false, "Skip the conformance reminder")
	cmd.Flags().StringVar(&opts.profile, "profile", "core", "Conformance test profile (core, snapshot, clone, advanced)")
	_ = cmd.Flags().MarkDeprecated("profile", "verify does not run conformance tests; use \"vcts run\" against a deployed provider")

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

	// Step 4: Final verification summary
	fmt.Println("\n📊 Verification Summary:")
	if !opts.skipBuild {
		fmt.Println("✅ Provider builds successfully")
	}
	if !opts.skipTests {
		fmt.Println("✅ Unit tests pass")
	}
	if !opts.skipConformance {
		fmt.Println("ℹ️  Conformance was not run: deploy the provider and run \"vcts run --provider <name>\"")
	}

	fmt.Println("\n🎉 Provider verification completed!")
	fmt.Println("\nNext steps:")
	fmt.Println("  - Build container image: make docker-build")
	fmt.Println("  - Deploy the provider and run: vcts run --provider <name>")

	return nil
}

// verifyBuild checks that the provider builds successfully.
func verifyBuild() error {
	// Prefer the project's own build target
	err := runMakeTarget("build")
	if err == nil || !errors.Is(err, errNoMakeTarget) {
		return err
	}

	// Fall back to go build
	fmt.Println("⚠️  Makefile build target not found, trying go build...")
	return runCommand("go", "build", "-v", "./...")
}

// verifyCodeQuality runs linting and code quality checks.
func verifyCodeQuality() error {
	var issues []string

	// Run the gofmt check: it lists the files that need formatting
	out, err := exec.Command("gofmt", "-l", ".").Output()
	switch {
	case err != nil:
		issues = append(issues, fmt.Sprintf("gofmt failed: %v", err))
	case strings.TrimSpace(string(out)) != "":
		issues = append(issues, "gofmt: files need formatting:\n"+strings.TrimSpace(string(out)))
	}

	// Run go vet
	if err := runCommand("go", "vet", "./..."); err != nil {
		issues = append(issues, "go vet issues found")
	}

	// Run golangci-lint when it is installed
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		fmt.Println("ℹ️  golangci-lint is not installed, skipping it")
	} else if err := runCommand("golangci-lint", "run"); err != nil {
		issues = append(issues, "golangci-lint issues found")
	}

	if len(issues) > 0 {
		return fmt.Errorf("code quality issues: %s", strings.Join(issues, ", "))
	}

	return nil
}

// verifyUnitTests runs unit tests.
func verifyUnitTests() error {
	// Prefer the project's own test target
	err := runMakeTarget("test")
	if err == nil || !errors.Is(err, errNoMakeTarget) {
		return err
	}

	// Fall back to go test
	fmt.Println("⚠️  Makefile test target not found, trying go test...")
	return runCommand("go", "test", "-v", "./...")
}
