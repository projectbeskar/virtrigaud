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
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

// runProviderCLI runs the vrtg-provider command tree in-process.
func runProviderCLI(t *testing.T, args ...string) error {
	t.Helper()
	cmd := newRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	return cmd.Execute()
}

// repoCheckout returns the VirtRigaud checkout the tests run in.
func repoCheckout(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	checkout := findCheckout(wd)
	require.NotEmpty(t, checkout, "the tests must run inside the VirtRigaud repository")
	return checkout
}

// useLocalModuleCache makes every go command the test starts (directly or
// through generate and verify) resolve modules from the local module cache
// only, so the result does not depend on network access.
//
// The cache holds what the main module needed to build: the packages a
// generated project imports are built by the main module too. That is why the
// tests do not run "go mod tidy" in the project: tidy also loads the tests of
// every dependency, whose modules the main module never downloads, so it
// fails on a cache that is complete for building. "-mod=mod" lets the first
// build add the requirements it needs to go.mod and go.sum instead.
func useLocalModuleCache(t *testing.T) {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMODCACHE").Output()
	require.NoError(t, err)
	download := filepath.Join(strings.TrimSpace(string(out)), "cache", "download")
	_, err = os.Stat(download)
	require.NoError(t, err, "the module cache must exist: the main module has just been built")

	t.Setenv("GOPROXY", "file://"+filepath.ToSlash(download))
	t.Setenv("GONOSUMDB", "*")
	t.Setenv("GOFLAGS", "-mod=mod")
}

// goCmd returns a go command run in dir.
func goCmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	return cmd
}

// run runs cmd and fails the test with its output when it fails.
func run(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s: %s", strings.Join(cmd.Args, " "), out)
}

func TestCheckProviderProject(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{
			name:  "sdk dependency",
			files: map[string]string{"go.mod": "module p\n\nrequire github.com/projectbeskar/virtrigaud/sdk v0.1.0\n", "Makefile": "all:\n"},
		},
		{
			name:  "proto dependency only",
			files: map[string]string{"go.mod": "module p\n\nrequire (\n\tgithub.com/projectbeskar/virtrigaud/proto v0.1.0\n\tgithub.com/other/x v1.0.0\n)\n", "Makefile": "all:\n"},
		},
		{
			name:    "no go.mod",
			files:   map[string]string{"Makefile": "all:\n"},
			wantErr: "missing go.mod",
		},
		{
			name:    "no Makefile",
			files:   map[string]string{"go.mod": "module p\n\nrequire github.com/projectbeskar/virtrigaud/sdk v0.1.0\n"},
			wantErr: "missing Makefile",
		},
		{
			name:    "unrelated module",
			files:   map[string]string{"go.mod": "module p\n\nrequire github.com/other/x v1.0.0\n", "Makefile": "all:\n"},
			wantErr: "does not require",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tt.files {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
			}
			t.Chdir(dir)
			err := checkProviderProject()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestInitRejectsBadInput(t *testing.T) {
	out := t.TempDir()
	require.Error(t, runProviderCLI(t, "init", "Bad_Name", "-o", out))
	require.Error(t, runProviderCLI(t, "init", "ok-name", "-t", "nonsense", "-o", out))

	err := runProviderCLI(t, "init", "ok-name", "-o", out, "--virtrigaud-path", t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a VirtRigaud checkout")
}

// TestInitWithoutCheckoutLeavesReplaceCommented checks the go.mod of a project
// created outside any VirtRigaud checkout.
func TestInitWithoutCheckoutLeavesReplaceCommented(t *testing.T) {
	out := t.TempDir()
	require.NoError(t, runProviderCLI(t, "init", "lonely", "-o", out))

	data, err := os.ReadFile(filepath.Join(out, "providers", "lonely", "go.mod"))
	require.NoError(t, err)
	gomod := string(data)
	assert.Contains(t, gomod, "github.com/projectbeskar/virtrigaud/sdk")
	assert.NotContains(t, gomod, "\nreplace (", "no checkout is known, so no active replace")
	assert.Contains(t, gomod, "// replace (")
}

// TestInitProjectIsAcceptedBuildsAndRuns is the end-to-end check: a scaffolded
// project passes the project check, compiles, and generate, verify and publish
// run in it. Every provider type is built.
func TestInitProjectIsAcceptedBuildsAndRuns(t *testing.T) {
	checkout := repoCheckout(t)
	useLocalModuleCache(t)

	for _, providerType := range []string{"generic", "libvirt", "vsphere", "proxmox", "firecracker", "qemu"} {
		t.Run(providerType, func(t *testing.T) {
			out := t.TempDir()
			name := "demo-" + providerType
			require.NoError(t, runProviderCLI(t, "init", name, "-t", providerType, "-o", out,
				"--virtrigaud-path", checkout))
			project := filepath.Join(out, "providers", name)

			gomod, err := os.ReadFile(filepath.Join(project, "go.mod"))
			require.NoError(t, err)
			assert.Contains(t, string(gomod), "replace (")
			assert.Contains(t, string(gomod), checkout+"/sdk")

			readme, err := os.ReadFile(filepath.Join(project, "README.md"))
			require.NoError(t, err)
			assert.NotContains(t, string(readme), "'''", "README code fences must be rendered")

			// Build, vet and test the generated project. (The documented first
			// step for an author, go mod tidy, needs the network; see
			// useLocalModuleCache.)
			run(t, goCmd(project, "build", "./..."))
			run(t, goCmd(project, "vet", "./..."))
			run(t, goCmd(project, "test", "./..."))

			// vrtg-provider accepts the project it created.
			t.Chdir(project)
			require.NoError(t, checkProviderProject())

			if providerType != "generic" {
				return
			}
			require.NoError(t, runProviderCLI(t, "generate"))
			require.NoError(t, runProviderCLI(t, "generate", "--clean"))
			require.NoError(t, runProviderCLI(t, "verify", "--skip-conformance"))
			require.NoError(t, runProviderCLI(t, "verify"), "verify must not fail on the conformance step")

			for _, skipVerify := range []bool{false, true} {
				catalog := filepath.Join(out, "catalog.yaml")
				args := []string{"publish", "--image", "example.invalid/p", "--repo", "https://example.invalid/p",
					"--maintainer", "dev@example.invalid", "--catalog", catalog}
				if skipVerify {
					args = append(args, "--skip-verify")
				}
				require.NoError(t, runProviderCLI(t, args...))

				data, err := os.ReadFile(catalog)
				require.NoError(t, err)
				var cat ProviderCatalog
				require.NoError(t, yaml.Unmarshal(data, &cat))
				require.Len(t, cat.Providers, 1)
				assert.Equal(t, name, cat.Providers[0].Name)
				// Neither path may claim a conformance pass: the suite was not run.
				assert.Equal(t, "skip", cat.Providers[0].Conformance.Profiles["core"])
				assert.Contains(t, cat.Providers[0].Conformance.BadgeURL, "not%20run")
				assert.Empty(t, cat.Providers[0].Capabilities)
			}

			// A dry run writes nothing.
			dry := filepath.Join(out, "dry.yaml")
			require.NoError(t, runProviderCLI(t, "publish", "--dry-run", "--skip-verify", "--image", "example.invalid/p",
				"--repo", "https://example.invalid/p", "--maintainer", "dev@example.invalid", "--catalog", dry))
			assert.NoFileExists(t, dry)
		})
	}
}

func TestGenerateBadgeURL(t *testing.T) {
	tests := []struct {
		name     string
		profiles map[string]string
		want     string
	}{
		{"nothing ran", map[string]string{"core": "skip"}, "not%20run"},
		{"all pass", map[string]string{"core": "pass", "clone": "pass"}, "conformance-pass-green"},
		{"skipped profiles do not count against", map[string]string{"core": "pass", "clone": "skip"}, "conformance-pass-green"},
		{"partial", map[string]string{"a": "pass", "b": "pass", "c": "fail"}, "partial"},
		{"fail", map[string]string{"a": "fail", "b": "fail", "c": "pass"}, "conformance-fail-red"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Contains(t, generateBadgeURL(tt.profiles), tt.want)
		})
	}
}

// TestCleanRemovesOnlyGeneratedBindings checks that generate --clean removes
// the *.pb.go files under internal/rpc, keeps every other file, and does not
// follow a symbolic link out of internal/rpc.
func TestCleanRemovesOnlyGeneratedBindings(t *testing.T) {
	project := t.TempDir()
	outside := t.TempDir()
	rpc := filepath.Join(project, filepath.FromSlash(generatedRPCDir))
	require.NoError(t, os.MkdirAll(filepath.Join(rpc, "v1"), 0o750))

	write := func(path string) {
		require.NoError(t, os.WriteFile(path, []byte("package x\n"), 0o600))
	}
	generated := filepath.Join(rpc, "v1", "provider.pb.go")
	kept := filepath.Join(rpc, "v1", "doc.go")
	outsideBinding := filepath.Join(outside, "other.pb.go")
	write(generated)
	write(kept)
	write(outsideBinding)
	require.NoError(t, os.Symlink(outside, filepath.Join(rpc, "linked")))
	linkedBinding := filepath.Join(rpc, "v1", "linked.pb.go")
	require.NoError(t, os.Symlink(outsideBinding, linkedBinding))

	t.Chdir(project)
	require.NoError(t, cleanGeneratedFiles())

	assert.NoFileExists(t, generated)
	assert.FileExists(t, kept)
	assert.NoFileExists(t, linkedBinding, "the link itself is removed")
	assert.FileExists(t, outsideBinding, "clean must not follow a symbolic link out of "+generatedRPCDir)
}
