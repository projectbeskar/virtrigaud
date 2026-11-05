//go:build virtrigaud_dev

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
// TestDevProviderTypesFlag_DevBuildEnablesAtStartup's child process starts
// main() with.
const managerSubprocessDevTypesEnv = "VIRTRIGAUD_TEST_DEV_PROVIDER_TYPES"

// TestDevProviderTypesFlag_DevBuildEnablesAtStartup: a development build
// (virtrigaud_dev tag) accepts --enable-dev-provider-types=cloudhypervisor and
// logs a warning; the child then stops only because it finds no cluster.
func TestDevProviderTypesFlag_DevBuildEnablesAtStartup(t *testing.T) {
	if os.Getenv(managerSubprocessEnv) == "1" {
		os.Args = []string{"manager", "--enable-dev-provider-types=" + os.Getenv(managerSubprocessDevTypesEnv)}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDevProviderTypesFlag_DevBuildEnablesAtStartup$") // #nosec G204 -- re-runs this test binary
	// No usable kubeconfig: main() fails to find a cluster after the gate.
	cmd.Env = append(os.Environ(), managerSubprocessEnv+"=1", managerSubprocessDevTypesEnv+"=cloudhypervisor",
		"KUBECONFIG="+t.TempDir()+"/none", "KUBERNETES_SERVICE_HOST=", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "the manager must exit for want of a cluster: %s", out)
	assert.Contains(t, string(out), "development-only provider types are enabled")
	assert.NotContains(t, string(out), "invalid --enable-dev-provider-types")
}
