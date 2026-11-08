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

package grantverify_test

import (
	"os/exec"
	"strings"
	"testing"
)

// forbiddenImports are what the verifier may not link, directly or
// transitively: it is shared with a per-host agent that runs outside the
// cluster and must not carry Kubernetes, controller-runtime, the API types or
// the provider contract package.
var forbiddenImports = []string{
	"k8s.io/",
	"sigs.k8s.io/",
	"github.com/projectbeskar/virtrigaud/api/",
	"github.com/projectbeskar/virtrigaud/internal/controller",
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts",
}

// TestImportBoundary: the non-test dependency closure of grantverify holds
// none of forbiddenImports.
func TestImportBoundary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}",
		"github.com/projectbeskar/virtrigaud/internal/operation/grantverify").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	deps := strings.Fields(string(out))
	if len(deps) < 10 {
		t.Fatalf("go list returned too few packages: %v", deps)
	}
	for _, d := range deps {
		for _, f := range forbiddenImports {
			if strings.HasPrefix(d, f) {
				t.Errorf("grantverify links %s", d)
			}
		}
	}
}
