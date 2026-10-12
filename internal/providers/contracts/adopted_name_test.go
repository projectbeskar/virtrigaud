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

package contracts

import (
	"strings"
	"testing"
)

func TestSanitizeVMName(t *testing.T) {
	for in, want := range map[string]string{
		"Web_01.Prod":   "web-01-prod",
		"my vm":         "my-vm",
		"--edge--":      "edge",
		"日本":            "",
		"a!b@c#":        "abc",
		"_leading.dot.": "leading-dot",
	} {
		if got := SanitizeVMName(in); got != want {
			t.Errorf("SanitizeVMName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("a", 62) + "_b"
	if got := SanitizeVMName(long); got != strings.Repeat("a", 62) {
		t.Errorf("a 64-char name is cut to 63 and trimmed: got %q", got)
	}
}

func TestAdoptedVMName(t *testing.T) {
	if got := AdoptedVMName("Web_01", "100"); got != "web-01" {
		t.Errorf("got %q", got)
	}
	if got := AdoptedVMName("日本", "100"); got != "vm-100" {
		t.Errorf("fallback: got %q", got)
	}
}
