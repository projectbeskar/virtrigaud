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

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// releaseFiles define what a release builds and publishes.
var releaseFiles = []string{
	"../../.github/workflows/release.yml",
	"../../.github/workflows/ci.yml",
	"../../Makefile",
}

// TestNotInReleaseArtefacts: no release workflow, CI image matrix or make
// target builds or publishes the agent.
func TestNotInReleaseArtefacts(t *testing.T) {
	for _, f := range releaseFiles {
		b, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "vrtg-agent") {
			t.Errorf("%s names vrtg-agent: the per-host agent must not be built or published as a release artefact", f)
		}
	}
	matches, err := filepath.Glob("Dockerfile*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("cmd/vrtg-agent has a Dockerfile: %v", matches)
	}
}

func deps(t *testing.T, tags string) []string {
	t.Helper()
	args := []string{"list", "-deps", "-f", "{{.ImportPath}}"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "github.com/projectbeskar/virtrigaud/cmd/vrtg-agent")
	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	return strings.Fields(string(out))
}

// TestReleaseBuildLinksNoEffectfulExecutor: a release build of the agent does
// not link the fake executor (or any development-only code of the agent
// configuration); a development build does.
func TestReleaseBuildLinksNoEffectfulExecutor(t *testing.T) {
	const fakePkg = "github.com/projectbeskar/virtrigaud/internal/agent/executor/fake"
	has := func(list []string, p string) bool {
		for _, d := range list {
			if d == p {
				return true
			}
		}
		return false
	}
	if has(deps(t, ""), fakePkg) {
		t.Fatal("a release build of vrtg-agent links the fake executor")
	}
	if !has(deps(t, "virtrigaud_dev"), fakePkg) {
		t.Fatal("a development build of vrtg-agent does not link the fake executor")
	}
}

// forbiddenImports are what the agent binary may not link.
var forbiddenImports = []string{
	"k8s.io/",
	"sigs.k8s.io/",
	"github.com/projectbeskar/virtrigaud/api/",
	"github.com/projectbeskar/virtrigaud/internal/controller",
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts",
}

// TestImportBoundary: neither build of the agent links Kubernetes, the API
// types, the controllers or the provider contract package.
func TestImportBoundary(t *testing.T) {
	for _, tags := range []string{"", "virtrigaud_dev"} {
		for _, d := range deps(t, tags) {
			for _, f := range forbiddenImports {
				if strings.HasPrefix(d, f) {
					t.Errorf("vrtg-agent (tags %q) links %s", tags, d)
				}
			}
		}
	}
}
