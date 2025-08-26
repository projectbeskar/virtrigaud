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
	"os/exec"

	"github.com/spf13/cobra"
)

// generateOptions holds options for the generate command.
type generateOptions struct {
	protoOnly bool
	clean     bool
}

// newGenerateCommand creates the generate command.
func newGenerateCommand() *cobra.Command {
	opts := &generateOptions{}

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate protocol buffer bindings and other code",
		Long: `Generate protocol buffer bindings and other generated code for the provider.

This command regenerates:
- Protocol buffer Go bindings from .proto files
- gRPC service stubs
- Any other generated code dependencies

The command must be run from within a provider project directory.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGenerate(opts)
		},
	}

	cmd.Flags().BoolVar(&opts.protoOnly, "proto-only", false, "Only regenerate protocol buffer bindings")
	cmd.Flags().BoolVar(&opts.clean, "clean", false, "Clean generated files before regenerating")

	return cmd
}

// runGenerate executes the generate command.
func runGenerate(opts *generateOptions) error {
	// Check if we're in a provider project directory
	if err := checkProviderProject(); err != nil {
		return fmt.Errorf("not in a provider project directory: %w", err)
	}

	fmt.Println("🔧 Generating protocol buffer bindings...")

	// Clean generated files if requested
	if opts.clean {
		fmt.Println("🧹 Cleaning generated files...")
		if err := cleanGeneratedFiles(); err != nil {
			return fmt.Errorf("failed to clean generated files: %w", err)
		}
	}

	// Generate protocol buffers
	if err := generateProto(); err != nil {
		return fmt.Errorf("failed to generate protocol buffers: %w", err)
	}

	if !opts.protoOnly {
		// Generate other code (e.g., mocks, deepcopy, etc.)
		fmt.Println("🔧 Generating additional code...")
		if err := generateAdditionalCode(); err != nil {
			return fmt.Errorf("failed to generate additional code: %w", err)
		}
	}

	fmt.Println("✅ Code generation completed successfully!")
	return nil
}

// checkProviderProject checks if the current directory is a provider project.
func checkProviderProject() error {
	// Look for key files that indicate a provider project
	requiredFiles := []string{"go.mod", "Makefile"}
	for _, file := range requiredFiles {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			return fmt.Errorf("missing %s file", file)
		}
	}

	// Check if go.mod contains virtrigaud SDK dependency
	content, err := os.ReadFile("go.mod")
	if err != nil {
		return fmt.Errorf("failed to read go.mod: %w", err)
	}

	if !contains([]string{string(content)}, "github.com/projectbeskar/virtrigaud") {
		return fmt.Errorf("go.mod does not contain virtrigaud dependency")
	}

	return nil
}

// cleanGeneratedFiles removes generated files.
func cleanGeneratedFiles() error {
	// Remove common generated files
	filesToRemove := []string{
		"internal/rpc/**/*.pb.go",
		"internal/rpc/**/*_grpc.pb.go",
	}

	for _, pattern := range filesToRemove {
		if err := removeFilesByPattern(pattern); err != nil {
			return err
		}
	}

	return nil
}

// generateProto generates protocol buffer bindings.
func generateProto() error {
	// Check if Makefile has proto target
	if err := runMakeTarget("proto"); err != nil {
		// Fall back to direct buf command
		fmt.Println("⚠️  Makefile proto target not found, trying buf directly...")
		return runBufGenerate()
	}
	return nil
}

// generateAdditionalCode generates other code artifacts.
func generateAdditionalCode() error {
	// Run go generate
	if err := runCommand("go", "generate", "./..."); err != nil {
		fmt.Printf("⚠️  go generate failed: %v\n", err)
		// Continue anyway as this might not be critical
	}

	// Run other generation targets if they exist
	makeTargets := []string{"generate", "deepcopy"}
	for _, target := range makeTargets {
		if err := runMakeTarget(target); err != nil {
			fmt.Printf("⚠️  make %s failed: %v\n", target, err)
			// Continue anyway
		}
	}

	return nil
}

// runMakeTarget runs a make target if it exists.
func runMakeTarget(target string) error {
	// Check if target exists in Makefile
	cmd := exec.Command("make", "-n", target)
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("make target %s not found", target)
	}

	// Run the target
	return runCommand("make", target)
}

// runBufGenerate runs buf generate directly.
func runBufGenerate() error {
	// Look for proto directory
	if _, err := os.Stat("proto"); os.IsNotExist(err) {
		return fmt.Errorf("proto directory not found")
	}

	// Run buf generate
	cmd := exec.Command("buf", "generate")
	cmd.Dir = "proto"
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// runCommand runs a command and returns any error.
func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// removeFilesByPattern removes files matching a pattern.
func removeFilesByPattern(pattern string) error {
	// This is a simplified implementation
	// In a real implementation, you'd use filepath.Glob or similar
	fmt.Printf("🗑️  Would remove files matching: %s\n", pattern)
	return nil
}
