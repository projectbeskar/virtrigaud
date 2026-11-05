//go:build !virtrigaud_dev

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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// managerSubprocessDevTypesEnv carries the --enable-dev-provider-types value
// TestDevProviderTypesFlag_ReleaseBuildRefusesAtStartup's child process
// starts main() with.
const managerSubprocessDevTypesEnv = "VIRTRIGAUD_TEST_DEV_PROVIDER_TYPES"

// TestDevProviderTypesFlag_ReleaseBuildRefusesAtStartup: a release build (no
// virtrigaud_dev tag) has no development gate, so setting
// --enable-dev-provider-types stops the manager at once (exit 1, before any
// cluster connection) with an error that says the gate is not compiled in;
// an invalid value is refused the same way.
func TestDevProviderTypesFlag_ReleaseBuildRefusesAtStartup(t *testing.T) {
	if os.Getenv(managerSubprocessEnv) == "1" {
		os.Args = []string{"manager", "--enable-dev-provider-types=" + os.Getenv(managerSubprocessDevTypesEnv)}
		main()
		return
	}
	for value, want := range map[string]string{
		"cloudhypervisor":  "not compiled into this build",
		" cloudhypervisor": "not compiled into this build",
		"vsphere":          "not a development-only provider type",
	} {
		t.Run(value, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestDevProviderTypesFlag_ReleaseBuildRefusesAtStartup$") // #nosec G204 -- re-runs this test binary
			// No usable kubeconfig: should the value be accepted by mistake,
			// main() fails to find a cluster instead of reaching a real one.
			cmd.Env = append(os.Environ(), managerSubprocessEnv+"=1", managerSubprocessDevTypesEnv+"="+value,
				"KUBECONFIG="+t.TempDir()+"/none", "KUBERNETES_SERVICE_HOST=", "HOME="+t.TempDir())
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "the manager must exit: %s", out)
			assert.Equal(t, 1, exitErr.ExitCode())
			assert.Contains(t, string(out), "invalid --enable-dev-provider-types")
			assert.Contains(t, string(out), want)
		})
	}
}
