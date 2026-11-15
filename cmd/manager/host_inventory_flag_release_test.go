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

// managerSubprocessHostInventoryFlagEnv carries the host inventory flag
// TestHostInventoryFlags_ReleaseBuildRefusesAtStartup's child process starts
// main() with.
const managerSubprocessHostInventoryFlagEnv = "VIRTRIGAUD_TEST_HOST_INVENTORY_FLAG"

// TestHostInventoryFlags_ReleaseBuildRefusesAtStartup: a release build refuses
// every host inventory flag at once, the insecure allow-list flag included,
// before any cluster connection.
func TestHostInventoryFlags_ReleaseBuildRefusesAtStartup(t *testing.T) {
	if os.Getenv(managerSubprocessEnv) == "1" {
		os.Args = []string{"manager", os.Getenv(managerSubprocessHostInventoryFlagEnv)}
		main()
		return
	}
	for _, flag := range []string{
		"--insecure-skip-component-allow-list",
		"--component-allow-list-file=/nonexistent",
		"--component-allow-list-sha256=" + "00000000000000000000000000000000000000000000000000000000000000ff",
		"--component-allow-list-signers-file=/nonexistent",
		"--host-inventory-max-age=10m",
		"--host-inventory-required-features=kvm",
		"--host-inventory-accepted-origins=read-only",
		"--host-inventory-accepted-tiers=pidfd-store",
	} {
		t.Run(flag, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestHostInventoryFlags_ReleaseBuildRefusesAtStartup$") // #nosec G204 -- re-runs this test binary
			cmd.Env = append(os.Environ(), managerSubprocessEnv+"=1", managerSubprocessHostInventoryFlagEnv+"="+flag,
				"KUBECONFIG="+t.TempDir()+"/none", "KUBERNETES_SERVICE_HOST=", "HOME="+t.TempDir())
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "the manager must exit: %s", out)
			assert.Equal(t, 1, exitErr.ExitCode())
			assert.Contains(t, string(out), "invalid host inventory flags")
			assert.Contains(t, string(out), "cannot be used with a release build")
		})
	}
}
