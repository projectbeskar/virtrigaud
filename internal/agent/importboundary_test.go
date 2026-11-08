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

package agent_test

import (
	"os/exec"
	"strings"
	"testing"
)

// forbiddenImports are the packages nothing under internal/agent may link,
// directly or transitively: the agent runs on a host, outside the cluster,
// and must not pull in Kubernetes, controller-runtime, the API types, the
// controllers or the provider contract package (which depends on them).
var forbiddenImports = []string{
	"k8s.io/",
	"sigs.k8s.io/",
	"github.com/projectbeskar/virtrigaud/api/",
	"github.com/projectbeskar/virtrigaud/internal/controller",
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts",
}

func TestImportBoundary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "github.com/projectbeskar/virtrigaud/internal/agent/...").CombinedOutput()
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
				t.Errorf("internal/agent links %s", d)
			}
		}
	}
	// The store must actually be under test: its own packages are listed.
	for _, want := range []string{"internal/agent/journal", "internal/agent/opstore", "internal/agent/executor", "go.etcd.io/bbolt"} {
		found := false
		for _, d := range deps {
			if strings.HasSuffix(d, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s not in the dependency list", want)
		}
	}
}
