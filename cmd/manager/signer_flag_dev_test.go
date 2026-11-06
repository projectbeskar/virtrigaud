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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/operation"
)

// managerSubprocessSignerKeyEnv carries the --dev-operation-signer-key-file
// value the signer-flag tests' child process starts main() with.
const managerSubprocessSignerKeyEnv = "VIRTRIGAUD_TEST_DEV_SIGNER_KEY_FILE"

// TestDevSignerKeyFileFlag_DevBuild: a development build loads a valid key
// file and logs a warning (the child then stops only because it finds no
// cluster), and refuses a malformed or missing one at startup without
// quoting the file.
func TestDevSignerKeyFileFlag_DevBuild(t *testing.T) {
	if os.Getenv(managerSubprocessEnv) == "1" {
		os.Args = []string{"manager", "--dev-operation-signer-key-file=" + os.Getenv(managerSubprocessSignerKeyEnv)}
		main()
		return
	}
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.pem")
	data, err := operation.GenerateDevSignerKey()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(valid, data, 0o600))
	malformed := filepath.Join(dir, "malformed.pem")
	require.NoError(t, os.WriteFile(malformed, []byte("secret-garbage"), 0o600))

	run := func(t *testing.T, path string) string {
		cmd := exec.Command(os.Args[0], "-test.run=^TestDevSignerKeyFileFlag_DevBuild$") // #nosec G204 -- re-runs this test binary
		cmd.Env = append(os.Environ(), managerSubprocessEnv+"=1", managerSubprocessSignerKeyEnv+"="+path,
			"KUBECONFIG="+t.TempDir()+"/none", "KUBERNETES_SERVICE_HOST=", "HOME="+t.TempDir())
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr, "the manager must exit: %s", out)
		return string(out)
	}
	t.Run("valid key file", func(t *testing.T) {
		out := run(t, valid)
		assert.Contains(t, out, "a development operation-grant signer is loaded")
		assert.NotContains(t, out, "invalid --dev-operation-signer-key-file")
		assert.NotContains(t, out, "PRIVATE KEY")
	})
	for name, path := range map[string]string{"malformed key file": malformed, "missing key file": filepath.Join(dir, "none.pem")} {
		t.Run(name, func(t *testing.T) {
			out := run(t, path)
			assert.Contains(t, out, "invalid --dev-operation-signer-key-file")
			assert.NotContains(t, out, "secret-garbage")
		})
	}
}
