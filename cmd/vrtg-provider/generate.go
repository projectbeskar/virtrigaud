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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// virtrigaudModulePrefix is the module path every VirtRigaud module (the
// repository itself, its SDK and its proto bindings) starts with. A provider
// project's go.mod requires at least one of them.
const virtrigaudModulePrefix = "github.com/projectbeskar/virtrigaud"

// generatedRPCDir is the directory of a provider project that holds generated
// protocol buffer bindings, if it has any.
const generatedRPCDir = "internal/rpc"

// errNoMakeTarget is returned by runMakeTarget when the project's Makefile
// does not define the target.
var errNoMakeTarget = errors.New("make target not found")

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
- Protocol buffer Go bindings from .proto files (the "proto" make target or
  "buf generate" in proto/; a project that has neither has nothing to generate)
- Any other generated code ("go generate ./..." and the "generate" and
  "deepcopy" make targets, when the project defines them)

The command must be run from within a provider project directory.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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

// checkProviderProject checks if the current directory is a provider project:
// it has a go.mod that requires a VirtRigaud module (the repository, its SDK
// or its proto bindings) and a Makefile.
func checkProviderProject() error {
	// Look for key files that indicate a provider project
	requiredFiles := []string{"go.mod", "Makefile"}
	for _, file := range requiredFiles {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			return fmt.Errorf("missing %s file", file)
		}
	}

	// Check if go.mod requires a VirtRigaud module
	content, err := os.ReadFile("go.mod")
	if err != nil {
		return fmt.Errorf("failed to read go.mod: %w", err)
	}

	if !strings.Contains(string(content), virtrigaudModulePrefix) {
		return fmt.Errorf("go.mod does not require a %s module", virtrigaudModulePrefix)
	}

	return nil
}

// cleanGeneratedFiles removes the generated protocol buffer bindings
// (*.pb.go) under internal/rpc.
func cleanGeneratedFiles() error {
	if _, err := os.Stat(generatedRPCDir); os.IsNotExist(err) {
		fmt.Printf("ℹ️  No %s directory, nothing to clean\n", generatedRPCDir)
		return nil
	}

	// Walk and remove through an os.Root so a symbolic link in the tree
	// cannot make the removal reach outside generatedRPCDir.
	root, err := os.OpenRoot(generatedRPCDir)
	if err != nil {
		return fmt.Errorf("open %s: %w", generatedRPCDir, err)
	}
	defer func() { _ = root.Close() }()

	return fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".pb.go") {
			return nil
		}
		name := filepath.Join(generatedRPCDir, filepath.FromSlash(path))
		fmt.Printf("🗑️  Removing %s\n", name)
		if err := root.Remove(path); err != nil {
			return fmt.Errorf("remove %s: %w", name, err)
		}
		return nil
	})
}

// generateProto generates protocol buffer bindings. A provider project that
// defines no "proto" make target and has no proto/ directory imports its
// bindings from the VirtRigaud proto module, so there is nothing to generate.
func generateProto() error {
	err := runMakeTarget("proto")
	if err == nil {
		return nil
	}
	if !errors.Is(err, errNoMakeTarget) {
		return err
	}

	// Fall back to direct buf command
	if _, statErr := os.Stat("proto"); os.IsNotExist(statErr) {
		fmt.Println("ℹ️  No proto make target and no proto/ directory: nothing to generate " +
			"(the project uses the bindings of the VirtRigaud proto module)")
		return nil
	}
	fmt.Println("⚠️  Makefile proto target not found, trying buf directly...")
	return runBufGenerate()
}

// generateAdditionalCode generates other code artifacts: go generate and the
// "generate" and "deepcopy" make targets the project defines.
func generateAdditionalCode() error {
	if err := runCommand("go", "generate", "./..."); err != nil {
		return fmt.Errorf("go generate: %w", err)
	}

	for _, target := range []string{"generate", "deepcopy"} {
		if err := runMakeTarget(target); err != nil {
			if errors.Is(err, errNoMakeTarget) {
				continue
			}
			return err
		}
	}

	return nil
}

// hasMakeTarget reports whether the Makefile in the current directory defines
// target.
func hasMakeTarget(target string) bool {
	data, err := os.ReadFile("Makefile")
	if err != nil {
		return false
	}
	return regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(target) + `\s*:`).Match(data)
}

// runMakeTarget runs a make target of the project's Makefile. It returns an
// error wrapping errNoMakeTarget when the Makefile does not define the target.
func runMakeTarget(target string) error {
	if !hasMakeTarget(target) {
		return fmt.Errorf("%w: %s", errNoMakeTarget, target)
	}

	if err := runCommand("make", target); err != nil {
		return fmt.Errorf("make %s: %w", target, err)
	}
	return nil
}

// runBufGenerate runs buf generate directly.
func runBufGenerate() error {
	// Run buf generate
	cmd := exec.Command("buf", "generate")
	cmd.Dir = "proto"
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("buf generate: %w", err)
	}
	return nil
}

// runCommand runs a command and returns any error.
func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...) // #nosec G204 -- every caller passes a fixed go, make or golangci-lint argv; make targets come from a built-in list
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
