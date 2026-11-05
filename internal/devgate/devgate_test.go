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

package devgate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

func TestParse(t *testing.T) {
	ch := infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
	tests := []struct {
		value   string
		want    []infravirtrigaudiov1beta1.ProviderType
		wantErr bool
	}{
		{"", nil, false},
		{" , ,", nil, false},
		{"cloudhypervisor", []infravirtrigaudiov1beta1.ProviderType{ch}, false},
		{" cloudhypervisor , cloudhypervisor,", []infravirtrigaudiov1beta1.ProviderType{ch}, false},
		{"libvirt", nil, true},
		{"cloudhypervisor,vsphere", nil, true},
		{"CloudHypervisor", nil, true},
		{"firecracker", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			got, err := Parse(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q): expected an error, got %v", tc.value, got)
				}
				if !strings.Contains(err.Error(), "not a development-only provider type") {
					t.Fatalf("Parse(%q): unexpected error %v", tc.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.value, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Parse(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// An empty value enables none in every build, and an invalid value is a parse
// error in every build (before the build is consulted).
func TestResolve_EmptyAndInvalidInEveryBuild(t *testing.T) {
	enabled, err := Resolve("")
	if err != nil || len(enabled) != 0 {
		t.Fatalf("Resolve(\"\") = %v, %v; want an empty set", enabled, err)
	}
	if _, err := Resolve("vsphere"); err == nil || strings.Contains(err.Error(), "not compiled") {
		t.Fatalf("Resolve(\"vsphere\") = %v; want a parse error", err)
	}
}

func TestNames(t *testing.T) {
	if got := Names(map[infravirtrigaudiov1beta1.ProviderType]struct{}{
		infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor: {},
	}); got != "cloudhypervisor" {
		t.Fatalf("Names = %q", got)
	}
}

// No release build path may compile the gate in: no Makefile target,
// Dockerfile, goreleaser config or CI workflow names the build tag.
func TestNoReleaseBuildUsesTheDevTag(t *testing.T) {
	root := filepath.Join("..", "..")
	candidates := []string{filepath.Join(root, "Makefile")}
	for _, pattern := range []string{"Dockerfile*", ".goreleaser*", filepath.Join(".github", "workflows", "*"),
		filepath.Join("cmd", "*", "Dockerfile*"), filepath.Join("build", "*")} {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		candidates = append(candidates, matches...)
	}
	checked := 0
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		data, err := os.ReadFile(path) // #nosec G304 -- a repository file named by the globs above
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		checked++
		if strings.Contains(string(data), BuildTag) {
			t.Errorf("%s names the %s build tag: release builds must never compile the development gate in", path, BuildTag)
		}
	}
	if checked == 0 {
		t.Fatal("found no build files to check")
	}
}
