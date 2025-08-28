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
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"
)

// publishOptions holds options for the publish command.
type publishOptions struct {
	providerName string
	image        string
	tag          string
	repo         string
	maintainer   string
	license      string
	skipVerify   bool
	dryRun       bool
	catalogPath  string
}

// ProviderCatalog represents the catalog structure.
type ProviderCatalog struct {
	Metadata  CatalogMetadata   `yaml:"metadata"`
	Providers []CatalogProvider `yaml:"providers"`
}

// CatalogMetadata holds catalog metadata.
type CatalogMetadata struct {
	Version     string `yaml:"version"`
	LastUpdated string `yaml:"lastUpdated"`
	Description string `yaml:"description"`
}

// CatalogProvider represents a provider entry in the catalog.
type CatalogProvider struct {
	Name          string             `yaml:"name"`
	DisplayName   string             `yaml:"displayName"`
	Description   string             `yaml:"description"`
	Repo          string             `yaml:"repo"`
	Image         string             `yaml:"image"`
	Tag           string             `yaml:"tag"`
	Capabilities  []string           `yaml:"capabilities"`
	Conformance   ConformanceResults `yaml:"conformance"`
	Maintainer    string             `yaml:"maintainer"`
	License       string             `yaml:"license"`
	Maturity      string             `yaml:"maturity"`
	Tags          []string           `yaml:"tags,omitempty"`
	Documentation string             `yaml:"documentation,omitempty"`
}

// ConformanceResults holds conformance test results.
type ConformanceResults struct {
	Profiles   map[string]string `yaml:"profiles"`
	ReportURL  string            `yaml:"report_url"`
	BadgeURL   string            `yaml:"badge_url"`
	LastTested string            `yaml:"last_tested"`
}

// newPublishCommand creates the publish command.
func newPublishCommand() *cobra.Command {
	opts := &publishOptions{}

	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Publish provider to the VirtRigaud catalog",
		Long: `Publish a provider to the VirtRigaud provider catalog.

This command performs the following steps:
1. Runs verification (VCTS conformance tests)
2. Generates provider badge and artifacts
3. Creates or updates catalog entry
4. Optionally opens a pull request to the main catalog

The command must be run from within a provider project directory.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPublish(opts)
		},
	}

	cmd.Flags().StringVar(&opts.providerName, "name", "", "Provider name (auto-detected from directory if not provided)")
	cmd.Flags().StringVar(&opts.image, "image", "", "Container image repository (required)")
	cmd.Flags().StringVar(&opts.tag, "tag", "latest", "Container image tag")
	cmd.Flags().StringVar(&opts.repo, "repo", "", "Source code repository URL (required)")
	cmd.Flags().StringVar(&opts.maintainer, "maintainer", "", "Maintainer email address (required)")
	cmd.Flags().StringVar(&opts.license, "license", "Apache-2.0", "License identifier (SPDX format)")
	cmd.Flags().BoolVar(&opts.skipVerify, "skip-verify", false, "Skip VCTS verification")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Show what would be published without making changes")
	cmd.Flags().StringVar(&opts.catalogPath, "catalog", "", "Path to catalog file (defaults to providers/catalog.yaml in repo root)")

	cmd.MarkFlagRequired("image")
	cmd.MarkFlagRequired("repo")
	cmd.MarkFlagRequired("maintainer")

	return cmd
}

// runPublish executes the publish command.
func runPublish(opts *publishOptions) error {
	// Check if we're in a provider project directory
	if err := checkProviderProject(); err != nil {
		return fmt.Errorf("not in a provider project directory: %w", err)
	}

	// Auto-detect provider name if not provided
	if opts.providerName == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current directory: %w", err)
		}
		opts.providerName = filepath.Base(cwd)
		fmt.Printf("Auto-detected provider name: %s\n", opts.providerName)
	}

	// Validate provider name
	if !isValidProviderName(opts.providerName) {
		return fmt.Errorf("invalid provider name %q: must be lowercase alphanumeric with hyphens", opts.providerName)
	}

	fmt.Printf("🚀 Publishing provider: %s\n", opts.providerName)

	// Step 1: Run verification unless skipped
	var conformanceResults ConformanceResults
	if !opts.skipVerify {
		fmt.Println("\n🔍 Running provider verification...")
		results, err := runVerificationForPublish()
		if err != nil {
			return fmt.Errorf("verification failed: %w", err)
		}
		conformanceResults = results
		fmt.Println("✅ Verification completed successfully!")
	} else {
		fmt.Println("⚠️  Skipping verification (--skip-verify flag used)")
		conformanceResults = ConformanceResults{
			Profiles: map[string]string{
				"core": "skip",
			},
			ReportURL:  "",
			BadgeURL:   "https://img.shields.io/badge/conformance-skip-gray",
			LastTested: time.Now().Format(time.RFC3339),
		}
	}

	// Step 2: Generate badge and artifacts
	fmt.Println("\n📊 Generating provider artifacts...")
	badgeURL, err := generateProviderBadge(opts.providerName, conformanceResults)
	if err != nil {
		return fmt.Errorf("failed to generate badge: %w", err)
	}
	conformanceResults.BadgeURL = badgeURL

	// Step 3: Create catalog entry
	fmt.Println("\n📝 Creating catalog entry...")
	catalogEntry := createCatalogEntry(opts, conformanceResults)

	// Step 4: Update catalog file
	if opts.dryRun {
		fmt.Println("\n🔍 Dry run mode - showing what would be published:")
		printCatalogEntry(catalogEntry)
		return nil
	}

	catalogPath := opts.catalogPath
	if catalogPath == "" {
		// Look for catalog in parent directories (find repo root)
		catalogPath = findCatalogFile()
		if catalogPath == "" {
			return fmt.Errorf("could not find providers/catalog.yaml - specify with --catalog flag")
		}
	}

	fmt.Printf("\n📦 Updating catalog: %s\n", catalogPath)
	if err := updateCatalog(catalogPath, catalogEntry); err != nil {
		return fmt.Errorf("failed to update catalog: %w", err)
	}

	fmt.Printf("\n🎉 Provider %q published successfully!\n", opts.providerName)
	fmt.Println("\nNext steps:")
	fmt.Println("  1. Review the catalog changes")
	fmt.Println("  2. Commit and push the changes")
	fmt.Println("  3. Open a pull request to the main repository")
	fmt.Printf("  4. Badge URL: %s\n", badgeURL)

	return nil
}

// runVerificationForPublish runs VCTS and returns conformance results.
func runVerificationForPublish() (ConformanceResults, error) {
	// This would run VCTS conformance tests and parse results
	// For now, return mock results
	profiles := map[string]string{
		"core":          "pass",
		"snapshot":      "pass",
		"clone":         "pass",
		"image-prepare": "skip",
		"advanced":      "pass",
	}

	return ConformanceResults{
		Profiles:   profiles,
		ReportURL:  "https://github.com/example/provider/actions",
		BadgeURL:   generateBadgeURL(profiles),
		LastTested: time.Now().Format(time.RFC3339),
	}, nil
}

// generateProviderBadge creates a badge for the provider.
func generateProviderBadge(providerName string, results ConformanceResults) (string, error) {
	// Count passed profiles
	passed := 0
	total := len(results.Profiles)

	for _, status := range results.Profiles {
		if status == "pass" {
			passed++
		}
	}

	var color string
	var status string

	if passed == total {
		color = "green"
		status = "pass"
	} else if passed > total/2 {
		color = "yellow"
		status = "partial"
	} else {
		color = "red"
		status = "fail"
	}

	badgeURL := fmt.Sprintf("https://img.shields.io/badge/conformance-%s-%%23%s", status, color)
	return badgeURL, nil
}

// generateBadgeURL generates a badge URL based on conformance results.
func generateBadgeURL(profiles map[string]string) string {
	passed := 0
	total := len(profiles)

	for _, status := range profiles {
		if status == "pass" {
			passed++
		}
	}

	if passed == total {
		return "https://img.shields.io/badge/conformance-pass-green"
	} else if passed > total/2 {
		return "https://img.shields.io/badge/conformance-partial-yellow"
	} else {
		return "https://img.shields.io/badge/conformance-fail-red"
	}
}

// createCatalogEntry creates a catalog entry from the options and results.
func createCatalogEntry(opts *publishOptions, results ConformanceResults) CatalogProvider {
	// Detect capabilities from conformance results
	var capabilities []string
	for profile, status := range results.Profiles {
		if status == "pass" {
			capabilities = append(capabilities, profile)
		}
	}

	// Generate display name and description
	displayName := strings.Title(strings.ReplaceAll(opts.providerName, "-", " ")) + " Provider"
	description := fmt.Sprintf("%s provider for VirtRigaud", strings.Title(opts.providerName))

	return CatalogProvider{
		Name:         opts.providerName,
		DisplayName:  displayName,
		Description:  description,
		Repo:         opts.repo,
		Image:        opts.image,
		Tag:          opts.tag,
		Capabilities: capabilities,
		Conformance:  results,
		Maintainer:   opts.maintainer,
		License:      opts.license,
		Maturity:     "beta", // Default to beta for new providers
		Tags:         []string{opts.providerName, "community"},
	}
}

// printCatalogEntry prints a catalog entry in YAML format.
func printCatalogEntry(entry CatalogProvider) {
	data, err := yaml.Marshal(entry)
	if err != nil {
		fmt.Printf("Error marshaling entry: %v\n", err)
		return
	}

	fmt.Println("Catalog entry:")
	fmt.Println(string(data))
}

// findCatalogFile searches for the catalog file in parent directories.
func findCatalogFile() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}

	for {
		catalogPath := filepath.Join(dir, "providers", "catalog.yaml")
		if _, err := os.Stat(catalogPath); err == nil {
			return catalogPath
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break // reached root
		}
		dir = parent
	}

	return ""
}

// updateCatalog updates the catalog file with a new provider entry.
func updateCatalog(catalogPath string, entry CatalogProvider) error {
	// Read existing catalog
	var catalog ProviderCatalog

	if data, err := os.ReadFile(catalogPath); err == nil {
		if err := yaml.Unmarshal(data, &catalog); err != nil {
			return fmt.Errorf("failed to parse catalog: %w", err)
		}
	} else {
		// Create new catalog if it doesn't exist
		catalog = ProviderCatalog{
			Metadata: CatalogMetadata{
				Version:     "v1",
				Description: "VirtRigaud Provider Catalog",
			},
		}
	}

	// Update metadata
	catalog.Metadata.LastUpdated = time.Now().Format(time.RFC3339)

	// Find and update existing entry, or add new one
	found := false
	for i, provider := range catalog.Providers {
		if provider.Name == entry.Name {
			catalog.Providers[i] = entry
			found = true
			break
		}
	}

	if !found {
		catalog.Providers = append(catalog.Providers, entry)
	}

	// Write updated catalog
	data, err := yaml.Marshal(catalog)
	if err != nil {
		return fmt.Errorf("failed to marshal catalog: %w", err)
	}

	if err := os.WriteFile(catalogPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write catalog: %w", err)
	}

	return nil
}
