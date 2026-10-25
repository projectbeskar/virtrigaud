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

	"github.com/projectbeskar/virtrigaud/internal/scaffold"
	internalversion "github.com/projectbeskar/virtrigaud/internal/version"
)

// version is the release the binary was built from; the release workflow sets
// it with -ldflags "-X main.version=...". When it is not set the build's
// internal/version string is reported.
var version string

// releaseVersion returns the VirtRigaud release this binary was built from,
// or "" for a development build (no version set, or one that is not a
// release, such as git describe output).
func releaseVersion() string {
	if scaffold.IsReleaseVersion(version) {
		return version
	}
	return ""
}

// versionString returns the version to report.
func versionString() string {
	if version != "" {
		return version
	}
	return internalversion.String()
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// newRootCmd builds the vrtg-provider command tree. Every call returns an
// independent tree with its own flag values, so it can be run repeatedly in
// one process.
func newRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "vrtg-provider",
		Short: "VirtRigaud Provider Development CLI",
		Long: `vrtg-provider is a command-line tool for developing VirtRigaud providers.

It provides scaffolding for new providers, code generation, and verification tools
to help developers build reliable and conformant provider implementations.`,
		Version:       versionString(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Global flags
	var verbose bool
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose output")

	// Add subcommands
	rootCmd.AddCommand(
		newInitCommand(),
		newGenerateCommand(),
		newVerifyCommand(),
		newPublishCommand(),
		newVersionCommand(),
	)

	return rootCmd
}

// newVersionCommand creates the version command.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Printf("vrtg-provider version: %s\n", versionString())
			fmt.Printf("Git SHA: %s\n", internalversion.GitSHA)
		},
	}
}
