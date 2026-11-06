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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// managerSubprocessSignerKeyEnv carries the --dev-operation-signer-key-file
// value the signer-flag tests' child process starts main() with.
const managerSubprocessSignerKeyEnv = "VIRTRIGAUD_TEST_DEV_SIGNER_KEY_FILE"

// TestDevSignerKeyFileFlag_ReleaseBuildRefusesAtStartup: a release build (no
// virtrigaud_dev tag) has no operation-grant signer, so setting
// --dev-operation-signer-key-file stops the manager at once (exit 1, before
// any cluster connection), whether or not the file exists, and never reads
// it.
func TestDevSignerKeyFileFlag_ReleaseBuildRefusesAtStartup(t *testing.T) {
	if os.Getenv(managerSubprocessEnv) == "1" {
		os.Args = []string{"manager", "--dev-operation-signer-key-file=" + os.Getenv(managerSubprocessSignerKeyEnv)}
		main()
		return
	}
	existing := filepath.Join(t.TempDir(), "signer.pem")
	require.NoError(t, os.WriteFile(existing, []byte("not read"), 0o600))
	for name, path := range map[string]string{"existing file": existing, "missing file": filepath.Join(t.TempDir(), "none.pem")} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestDevSignerKeyFileFlag_ReleaseBuildRefusesAtStartup$") // #nosec G204 -- re-runs this test binary
			cmd.Env = append(os.Environ(), managerSubprocessEnv+"=1", managerSubprocessSignerKeyEnv+"="+path,
				"KUBECONFIG="+t.TempDir()+"/none", "KUBERNETES_SERVICE_HOST=", "HOME="+t.TempDir())
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "the manager must exit: %s", out)
			assert.Equal(t, 1, exitErr.ExitCode())
			assert.Contains(t, string(out), "invalid --dev-operation-signer-key-file")
			assert.Contains(t, string(out), "no operation-grant signer is compiled into this build")
		})
	}
}
