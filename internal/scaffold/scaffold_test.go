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

package scaffold

import (
	"encoding/json"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// generate scaffolds a project into a new directory and returns its path.
func generate(t *testing.T, cfg Config) string {
	t.Helper()
	cfg.TargetDir = t.TempDir()
	require.NoError(t, New(cfg).Generate())
	return cfg.TargetDir
}

func TestGenerateDoesNotImportInternalPackages(t *testing.T) {
	dir := generate(t, Config{ProviderName: "demo", ProviderType: "generic"})

	// A generated project lives in its own module, which cannot import the
	// VirtRigaud repository's internal packages.
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		assert.NotContains(t, string(data), "virtrigaud/internal/", path)
		return nil
	})
	require.NoError(t, err)
}

func TestGoModTemplate(t *testing.T) {
	t.Run("a development build without a checkout requires placeholders and comments the replace block", func(t *testing.T) {
		dir := generate(t, Config{ProviderName: "demo", ProviderType: "generic"})
		gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		require.NoError(t, err)
		assert.Contains(t, string(gomod), "module provider-demo")
		assert.Contains(t, string(gomod), "github.com/projectbeskar/virtrigaud/sdk "+devModuleVersion+"\n")
		assert.Contains(t, string(gomod), "github.com/projectbeskar/virtrigaud/proto "+devModuleVersion+"\n")
		assert.Contains(t, string(gomod), "go "+defaultGoVersion+"\n")
		assert.NotContains(t, string(gomod), "\nreplace (")
		assert.Contains(t, string(gomod), "// replace (")
	})

	t.Run("a release pins the published modules and has no replace directives", func(t *testing.T) {
		dir := generate(t, Config{ProviderName: "demo", ProviderType: "generic", ModuleVersion: "v0.4.1-rc1"})
		gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		require.NoError(t, err)
		for _, module := range []string{"", "/proto", "/sdk"} {
			assert.Contains(t, string(gomod), "\tgithub.com/projectbeskar/virtrigaud"+module+" v0.4.1-rc1\n")
		}
		assert.NotContains(t, string(gomod), "replace")
		assert.NotContains(t, string(gomod), devModuleVersion+"\n")
		want := toolchainGoVersion()
		if want == "" {
			want = defaultGoVersion
		}
		assert.Contains(t, string(gomod), "go "+want+"\n")

		// The go command reads it as three requires and nothing else.
		cmd := exec.Command("go", "mod", "edit", "-json")
		cmd.Dir = dir
		out, err := cmd.Output()
		require.NoError(t, err)
		var parsed struct {
			Require []struct{ Path, Version string }
			Replace []json.RawMessage
		}
		require.NoError(t, json.Unmarshal(out, &parsed))
		assert.Empty(t, parsed.Replace)
		require.Len(t, parsed.Require, 3)
		for _, r := range parsed.Require {
			assert.Equal(t, "v0.4.1-rc1", r.Version, r.Path)
		}

		dockerfile, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
		require.NoError(t, err)
		assert.NotContains(t, string(dockerfile), "local checkout", "a pinned project builds from its own directory")
	})

	t.Run("a checkout overrides a release and keeps its version in the requires", func(t *testing.T) {
		checkout := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(checkout, "go.mod"),
			[]byte("module github.com/projectbeskar/virtrigaud\n\ngo 1.99.1\n"), 0o600))
		dir := generate(t, Config{
			ProviderName: "demo", ProviderType: "generic", ModuleVersion: "v0.4.1", VirtrigaudPath: checkout,
		})
		gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		require.NoError(t, err)
		assert.Contains(t, string(gomod), "github.com/projectbeskar/virtrigaud/sdk v0.4.1\n")
		assert.Contains(t, string(gomod), "\nreplace (")
		assert.Contains(t, string(gomod), "go 1.99.1\n")

		dockerfile, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
		require.NoError(t, err)
		assert.Contains(t, string(dockerfile), "local checkout")
	})

	t.Run("a checkout outside the project is replaced by absolute path and sets the go directive", func(t *testing.T) {
		checkout := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(checkout, "go.mod"),
			[]byte("module github.com/projectbeskar/virtrigaud\n\ngo 1.99.1\n"), 0o600))
		dir := generate(t, Config{ProviderName: "demo", ProviderType: "generic", VirtrigaudPath: checkout})
		gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		require.NoError(t, err)
		assert.Contains(t, string(gomod), "go 1.99.1\n")
		assert.Contains(t, string(gomod), "github.com/projectbeskar/virtrigaud/sdk => "+filepath.ToSlash(checkout)+"/sdk")
		assert.Contains(t, string(gomod), "github.com/projectbeskar/virtrigaud/proto => "+filepath.ToSlash(checkout)+"/proto")
	})

	t.Run("a project inside the checkout gets a relative path", func(t *testing.T) {
		checkout := t.TempDir()
		target := filepath.Join(checkout, "providers", "demo")
		require.NoError(t, New(Config{
			ProviderName: "demo", ProviderType: "generic", TargetDir: target, VirtrigaudPath: checkout,
		}).Generate())
		gomod, err := os.ReadFile(filepath.Join(target, "go.mod"))
		require.NoError(t, err)
		assert.Contains(t, string(gomod), "github.com/projectbeskar/virtrigaud/sdk => ../../sdk")
	})
}

func TestReadmeCodeFences(t *testing.T) {
	dir := generate(t, Config{ProviderName: "demo", ProviderType: "generic"})
	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	require.NoError(t, err)
	assert.NotContains(t, string(readme), "'''")
	assert.Contains(t, string(readme), "```bash")
}

func TestIsReleaseVersion(t *testing.T) {
	for version, want := range map[string]bool{
		"v0.4.1":                  true,
		"v0.4.1-rc1":              true,
		"v0.4.1-rc.2":             true,
		"v1.0.0-beta3":            true,
		"v1.2.3-alpha1":           true,
		"":                        false,
		"dev":                     false,
		"0.4.1":                   false,
		"v0.4":                    false,
		"v0.04.1":                 false,
		"v2.0.0":                  false, // would need a /v2 module path
		"v0.4.1+meta":             false,
		"v0.4.1-rc01":             false,
		"v0.4.0-3-gabc1234":       false, // git describe
		"v0.4.0-3-gabc1234-dirty": false,
		"v0.4.1-preview1":         false,
	} {
		assert.Equal(t, want, IsReleaseVersion(version), version)
	}
}

func TestToolchainGoVersion(t *testing.T) {
	v := toolchainGoVersion()
	if v == "" {
		t.Skipf("toolchain %s is not a Go release", runtime.Version())
	}
	assert.Equal(t, "go"+v, runtime.Version())
	assert.Regexp(t, goVersionPattern, v)
}

func TestReadGoDirective(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	require.NoError(t, os.WriteFile(path, []byte("module m\n\ngo 1.24.2\n\nrequire x v1\n"), 0o600))
	assert.Equal(t, "1.24.2", readGoDirective(path))
	assert.Empty(t, readGoDirective(filepath.Join(dir, "absent")))
}

func TestGeneratedGoIsGofmtClean(t *testing.T) {
	for _, providerType := range []string{"generic", "vsphere", "libvirt", "proxmox", "firecracker", "qemu"} {
		t.Run(providerType, func(t *testing.T) {
			dir := generate(t, Config{ProviderName: "demo", ProviderType: providerType, Remote: true})
			err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
				require.NoError(t, err)
				if d.IsDir() || !strings.HasSuffix(path, ".go") {
					return nil
				}
				data, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				formatted, fmtErr := format.Source(data)
				require.NoError(t, fmtErr, path)
				assert.Equal(t, string(formatted), string(data), "%s is not gofmt-clean", path)
				return nil
			})
			require.NoError(t, err)
		})
	}
}

// TestProxmoxScaffoldAdvertisesNoOptionalCapability checks that a fresh
// proxmox scaffold, whose every operation is unimplemented, declares only the
// core profile and Proxmox's disk and network types.
func TestProxmoxScaffoldAdvertisesNoOptionalCapability(t *testing.T) {
	dir := generate(t, Config{ProviderName: "demo", ProviderType: "proxmox", Remote: true})

	for _, file := range []string{"internal/provider/provider.go", "internal/provider/capabilities.go"} {
		data, err := os.ReadFile(filepath.Join(dir, file))
		require.NoError(t, err)
		src := string(data)
		for _, optional := range []string{".Snapshots()", ".MemorySnapshots()", ".LinkedClones()", ".OnlineReconfigure()", ".OnlineDiskExpansion()", ".ImageImport()", ".DiskExport(", ".DiskImport(", ".VSphere()", ".Libvirt()"} {
			assert.NotContains(t, src, optional, file)
		}
	}

	caps, err := os.ReadFile(filepath.Join(dir, "internal/provider/capabilities.go"))
	require.NoError(t, err)
	assert.Contains(t, string(caps), `DiskTypes("raw", "qcow2")`)
	assert.Contains(t, string(caps), `NetworkTypes("bridge", "vlan")`)
}

// TestProxmoxScaffoldDocumentsConfiguration checks that the README and the
// remote Deployment of a proxmox scaffold carry the Proxmox settings, and that
// no secret value is written into either.
func TestProxmoxScaffoldDocumentsConfiguration(t *testing.T) {
	dir := generate(t, Config{ProviderName: "demo", ProviderType: "proxmox", Remote: true})

	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	require.NoError(t, err)
	assert.Contains(t, string(readme), "Proxmox VE API integration")
	assert.Contains(t, string(readme), "PVE_TOKEN_SECRET")
	assert.NotContains(t, string(readme), "vSphere")

	deployment, err := os.ReadFile(filepath.Join(dir, "config/deployment.yaml"))
	require.NoError(t, err)
	for _, env := range []string{"PVE_ENDPOINT", "PVE_TOKEN_ID", "PVE_TOKEN_SECRET"} {
		assert.Contains(t, string(deployment), "- name: "+env)
	}
	assert.Equal(t, 3, strings.Count(string(deployment), "secretKeyRef:"), "credentials come from a Secret")
	assert.NotContains(t, string(deployment), "VSPHERE")
}
