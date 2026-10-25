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
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/projectbeskar/virtrigaud/internal/scaffold"
)

// initOptions holds options for the init command.
type initOptions struct {
	providerName string
	outputDir    string
	providerType string
	remote       bool
	force        bool

	// virtrigaudPath is the VirtRigaud checkout the generated go.mod resolves
	// the SDK and proto modules from; empty means detect it.
	virtrigaudPath string
}

// rootModuleLine is the module line of the VirtRigaud repository's go.mod.
const rootModuleLine = "module github.com/projectbeskar/virtrigaud"

// newInitCommand creates the init command.
func newInitCommand() *cobra.Command {
	opts := &initOptions{}

	cmd := &cobra.Command{
		Use:   "init <provider-name>",
		Short: "Initialize a new provider project",
		Long: `Initialize a new VirtRigaud provider project with scaffolded code.

This command creates a complete provider project structure including:
- Go module and main.go with gRPC server setup
- Dockerfile for containerization
- Makefile with build and test targets
- GitHub Actions CI workflow
- Example provider implementation
- Deployment manifests for Kubernetes

Examples:
  # Create a new vSphere-like provider
  vrtg-provider init myprovider --type vsphere

  # Create with remote runtime configuration
  vrtg-provider init myprovider --remote

  # Create under a specific directory (the project goes in <dir>/providers/myprovider)
  vrtg-provider init myprovider --output ./work

  # Resolve the SDK and proto modules from a VirtRigaud checkout elsewhere
  vrtg-provider init myprovider --virtrigaud-path ~/src/virtrigaud`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.providerName = args[0]
			return runInit(opts)
		},
	}

	cmd.Flags().StringVarP(&opts.outputDir, "output", "o", ".", "Output directory for the provider project")
	cmd.Flags().StringVarP(&opts.providerType, "type", "t", "generic", "Provider type (vsphere, libvirt, firecracker, qemu, generic)")
	cmd.Flags().BoolVar(&opts.remote, "remote", false, "Generate remote runtime configuration")
	cmd.Flags().BoolVar(&opts.force, "force", false, "Overwrite existing files")
	cmd.Flags().StringVar(&opts.virtrigaudPath, "virtrigaud-path", "",
		"VirtRigaud checkout to resolve the SDK and proto modules from (default: the checkout the project is created in, if any)")

	return cmd
}

// runInit executes the init command.
func runInit(opts *initOptions) error {
	// Validate provider name
	if !isValidProviderName(opts.providerName) {
		return fmt.Errorf("invalid provider name %q: must be lowercase alphanumeric with hyphens", opts.providerName)
	}

	// Validate provider type
	validTypes := []string{"vsphere", "libvirt", "firecracker", "qemu", "generic"}
	if !contains(validTypes, opts.providerType) {
		return fmt.Errorf("invalid provider type %q: must be one of %v", opts.providerType, validTypes)
	}

	// Create target directory
	targetDir := filepath.Join(opts.outputDir, "providers", opts.providerName)
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", targetDir, err)
	}

	// Check if directory already exists and has files
	if !opts.force {
		if err := checkEmptyDirectory(targetDir); err != nil {
			return fmt.Errorf("directory %s is not empty (use --force to overwrite): %w", targetDir, err)
		}
	}

	// Find the VirtRigaud checkout the SDK and proto modules come from
	checkout := opts.virtrigaudPath
	if checkout == "" {
		checkout = findCheckout(targetDir)
	} else if !isCheckout(checkout) {
		return fmt.Errorf("--virtrigaud-path %s is not a VirtRigaud checkout (no go.mod with %q)", checkout, rootModuleLine)
	}

	// Create scaffolder
	scaffolder := scaffold.New(scaffold.Config{
		ProviderName:   opts.providerName,
		ProviderType:   opts.providerType,
		TargetDir:      targetDir,
		Remote:         opts.remote,
		Force:          opts.force,
		VirtrigaudPath: checkout,
	})

	// Generate project structure
	if err := scaffolder.Generate(); err != nil {
		return fmt.Errorf("failed to generate provider scaffold: %w", err)
	}

	fmt.Printf("✅ Provider %q initialized successfully!\n", opts.providerName)
	fmt.Printf("📁 Project created in: %s\n", targetDir)
	fmt.Printf("\nNext steps:\n")
	fmt.Printf("  cd %s\n", targetDir)
	if checkout == "" {
		fmt.Printf("  # go.mod needs replace directives for the VirtRigaud modules (see the comment in it),\n")
		fmt.Printf("  # or re-run init with --virtrigaud-path <checkout>\n")
	}
	fmt.Printf("  go mod tidy\n")
	fmt.Printf("  make build\n")
	fmt.Printf("  make test\n")
	fmt.Printf("  vrtg-provider verify\n")

	return nil
}

// findCheckout returns the VirtRigaud checkout that contains dir, or "" when
// dir is not inside one.
func findCheckout(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		if isCheckout(abs) {
			return abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return ""
		}
		abs = parent
	}
}

// isCheckout reports whether dir holds the VirtRigaud repository's go.mod.
func isCheckout(dir string) bool {
	f, err := os.Open(filepath.Join(dir, "go.mod")) //nolint:gosec // dir is a path the user named or a parent of the output directory
	if err != nil {
		return false
	}
	defer f.Close() //nolint:errcheck // read-only file

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == rootModuleLine {
			return true
		}
	}
	return false
}

// isValidProviderName checks if the provider name is valid.
func isValidProviderName(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}

	// Must start and end with alphanumeric
	if !isAlphaNumeric(name[0]) || !isAlphaNumeric(name[len(name)-1]) {
		return false
	}

	// Can contain alphanumeric and hyphens
	for i, r := range name {
		if !isAlphaNumeric(name[i]) && r != '-' {
			return false
		}
	}

	return true
}

// isAlphaNumeric checks if a rune is alphanumeric.
func isAlphaNumeric(r byte) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

// contains checks if a slice contains a string.
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// checkEmptyDirectory checks if a directory is empty.
func checkEmptyDirectory(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Directory doesn't exist, that's fine
		}
		return err
	}

	if len(entries) > 0 {
		var files []string
		for _, entry := range entries {
			files = append(files, entry.Name())
		}
		return fmt.Errorf("contains files: %s", strings.Join(files, ", "))
	}

	return nil
}
