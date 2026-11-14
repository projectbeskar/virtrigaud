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
	"slices"
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
// target builds or publishes this Provider, and it has no image.
func TestNotInReleaseArtefacts(t *testing.T) {
	for _, f := range releaseFiles {
		b, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "provider-cloudhypervisor") {
			t.Errorf("%s names provider-cloudhypervisor: it must not be built or published as a release artefact", f)
		}
	}
	matches, err := filepath.Glob("Dockerfile*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("cmd/provider-cloudhypervisor has a Dockerfile: %v", matches)
	}
}

func deps(t *testing.T, tags string) []string {
	t.Helper()
	args := []string{"list", "-deps", "-f", "{{.ImportPath}}"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "github.com/projectbeskar/virtrigaud/cmd/provider-cloudhypervisor")
	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	return strings.Fields(string(out))
}

// TestReleaseBuildLinksNoProvider: a release build links none of the
// Provider; a development build does. Neither links the testing package.
func TestReleaseBuildLinksNoProvider(t *testing.T) {
	const impl = "github.com/projectbeskar/virtrigaud/internal/providers/cloudhypervisor"
	release, dev := deps(t, ""), deps(t, "virtrigaud_dev")
	if slices.Contains(release, impl) {
		t.Error("a release build of provider-cloudhypervisor links the Provider")
	}
	if !slices.Contains(dev, impl) {
		t.Error("a development build of provider-cloudhypervisor does not link the Provider")
	}
	for name, list := range map[string][]string{"release": release, "development": dev} {
		if slices.Contains(list, "testing") {
			t.Errorf("a %s build of provider-cloudhypervisor links the testing package", name)
		}
	}
}
