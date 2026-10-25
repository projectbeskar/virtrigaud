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
	"go/format"
	"os"
	"path/filepath"
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
	t.Run("without a checkout the replace block is commented out", func(t *testing.T) {
		dir := generate(t, Config{ProviderName: "demo", ProviderType: "generic"})
		gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		require.NoError(t, err)
		assert.Contains(t, string(gomod), "module provider-demo")
		assert.Contains(t, string(gomod), "github.com/projectbeskar/virtrigaud/sdk v0.1.0")
		assert.Contains(t, string(gomod), "github.com/projectbeskar/virtrigaud/proto v0.1.0")
		assert.Contains(t, string(gomod), "go "+defaultGoVersion+"\n")
		assert.NotContains(t, string(gomod), "\nreplace (")
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

func TestReadGoDirective(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	require.NoError(t, os.WriteFile(path, []byte("module m\n\ngo 1.24.2\n\nrequire x v1\n"), 0o600))
	assert.Equal(t, "1.24.2", readGoDirective(path))
	assert.Empty(t, readGoDirective(filepath.Join(dir, "absent")))
}

func TestGeneratedGoIsGofmtClean(t *testing.T) {
	for _, providerType := range []string{"generic", "vsphere", "libvirt", "firecracker", "qemu"} {
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
