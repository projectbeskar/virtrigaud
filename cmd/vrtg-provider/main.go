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

// Package main provides the vrtg-provider CLI tool for scaffolding and managing VirtRigaud providers.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/projectbeskar/virtrigaud/internal/version"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "vrtg-provider",
		Short: "VirtRigaud Provider Development CLI",
		Long: `vrtg-provider is a command-line tool for developing VirtRigaud providers.

It provides scaffolding for new providers, code generation, and verification tools
to help developers build reliable and conformant provider implementations.`,
		Version: version.String(),
	}

	// Global flags
	var verbose bool
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose output")

	// Add subcommands
	rootCmd.AddCommand(
		newInitCommand(),
		newGenerateCommand(),
		newVerifyCommand(),
		newVersionCommand(),
	)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// newVersionCommand creates the version command.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("vrtg-provider version: %s\n", version.String())
			fmt.Printf("Git SHA: %s\n", version.GitSHA)
		},
	}
}
