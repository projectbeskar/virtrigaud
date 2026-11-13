/*
Copyright 2026.

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

package inventory

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCollectorExecutesNothing is the guard against the collector ever running
// a program found on the host: the package's own sources import nothing that
// starts a process and call nothing that does. A component binary is read as
// data, never run (no version flag, no package tool, no self-test).
func TestCollectorExecutesNothing(t *testing.T) {
	forbiddenImports := map[string]bool{"os/exec": true, "plugin": true, "C": true, "net": true, "net/http": true}
	forbiddenCalls := map[string]bool{
		"os.StartProcess": true, "os.FindProcess": true, "syscall.Exec": true, "syscall.ForkExec": true, "syscall.StartProcess": true,
		"unix.Exec": true, "unix.Execve": true, "unix.Fexecve": true, "unix.Execveat": true, "unix.Syscall": true, "unix.RawSyscall": true,
		"unix.Syscall6": true, "syscall.Syscall": true, "syscall.RawSyscall": true, "syscall.Syscall6": true, "syscall.RawSyscall6": true, "unix.RawSyscall6": true, "unix.ForkExec": true, "unix.Clone3": true,
	}
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		require.NoError(t, err)
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			assert.False(t, forbiddenImports[path], "%s imports %s", name, path)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok {
					assert.False(t, forbiddenCalls[id.Name+"."+sel.Sel.Name], "%s uses %s.%s", name, id.Name, sel.Sel.Name)
				}
			}
			return true
		})
	}
	require.Greater(t, checked, 5)
}

// TestCollectorHasNoCgo checks that the collector and its in-module
// dependencies are pure Go: nothing links a C parser, and the package builds
// with cgo off.
func TestCollectorHasNoCgo(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}|{{len .CgoFiles}}|{{len .CFiles}}", "github.com/projectbeskar/virtrigaud/internal/agent/inventory")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "|")
		require.Len(t, parts, 3, line)
		if strings.HasPrefix(parts[0], "github.com/projectbeskar/virtrigaud/") || strings.HasPrefix(parts[0], "github.com/fsnotify/") ||
			strings.HasPrefix(parts[0], "golang.org/x/sys/") || parts[0] == "debug/elf" {
			assert.Equal(t, "0", parts[1], "%s has cgo files", parts[0])
			assert.Equal(t, "0", parts[2], "%s has C files", parts[0])
		}
	}
	build := exec.Command("go", "build", "github.com/projectbeskar/virtrigaud/internal/agent/inventory")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	bout, err := build.CombinedOutput()
	require.NoError(t, err, string(bout))
}

func FuzzParseManifest(f *testing.F) {
	cat := map[string]*CatalogEntry{}
	for _, e := range DefaultCatalog() {
		cat[e.Name] = &e
	}
	for _, seed := range []string{
		manifestJSON("cloud-hypervisor", "1.2.3", ""),
		manifestJSON("cloud-hypervisor", "1.2.3", `"artifact":{"name":"a.raw","sha256":"`+digestOf(nil)+`"},"files":{"cloud-hypervisor":"`+digestOf(nil)+`"}`),
		`{}`, `[]`, `null`, `{"schemaVersion":1}`, `{"schemaVersion":1,"component":"x"`, "\x00\xff", strings.Repeat("[", 5000),
		manifestJSON("cloud-hypervisor", strings.Repeat("9", 70), ""),
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		p := parseManifest(data, cat)
		if p.component != "" {
			if cat[p.component] == nil {
				t.Fatalf("component %q is not in the catalog", p.component)
			}
		}
		if p.reason != "" {
			if p.reason != ReasonTooLarge && len(p.decl.version) != 0 {
				t.Fatalf("an invalid manifest reports a declaration: %+v", p.decl)
			}
			return
		}
		d := p.decl
		if cat[d.component] == nil || !versionRE.MatchString(d.version) || (d.source != "" && !sourceRE.MatchString(d.source)) {
			t.Fatalf("a valid manifest broke a field rule: %+v", d)
		}
		if d.artifactName != "" && !fileNameRE.MatchString(d.artifactName) {
			t.Fatalf("artifact name %q", d.artifactName)
		}
		for name, dg := range d.fileDigests {
			if !catalogHasFile(cat[d.component], name) || len(dg) != 32 {
				t.Fatalf("file digest %q", name)
			}
		}
		if len(d.artifactDigest) != 0 && len(d.artifactDigest) != 32 {
			t.Fatalf("artifact digest")
		}
	})
}

func FuzzELFPackageVersion(f *testing.F) {
	f.Add(buildELF(`{"version":"1.2.3"}`))
	f.Add(buildELF(`{"version":"bad version"}`))
	f.Add(buildELF(`{"version":"1"}`)[:80])
	f.Add([]byte("\x7fELF"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		v, ok := elfPackageVersion(bytes.NewReader(data), int64(len(data)))
		if ok && !versionRE.MatchString(v) {
			t.Fatalf("version %q breaks the version rule", v)
		}
	})
}

func FuzzPackageDB(f *testing.F) {
	f.Add("Package: tool\nStatus: install ok installed\nVersion: 1.0\n\n", "tool")
	f.Add("P:tool\nV:1.0\n\n", "tool")
	f.Add(strings.Repeat("x", maxPackageDBLine+1), "tool")
	f.Fuzz(func(t *testing.T, content, pkg string) {
		v, ok := packageDBVersion(strings.NewReader(content), pkg)
		if ok && !versionRE.MatchString(v) {
			t.Fatalf("version %q breaks the version rule", v)
		}
	})
}

func FuzzExtensionRelease(f *testing.F) {
	f.Add([]byte("SYSEXT_VERSION_ID=1.0\n"))
	f.Add([]byte("SYSEXT_IMAGE_VERSION=\"x\"\n=\n=="))
	f.Fuzz(func(t *testing.T, data []byte) {
		v, ok := extensionReleaseVersion(data)
		if ok && !versionRE.MatchString(v) {
			t.Fatalf("version %q breaks the version rule", v)
		}
	})
}

func FuzzVersionRange(f *testing.F) {
	f.Add(">=1.0.0, <2", "1.5")
	f.Add("=", "")
	f.Add(strings.Repeat(">=9999999999999999999.", 5), "1")
	f.Fuzz(func(t *testing.T, rng, version string) {
		if r, err := parseRange(rng); err == nil {
			_ = r.contains(version)
		}
	})
}
