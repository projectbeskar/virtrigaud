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
	"flag"
	"io"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/controller"
)

// managerSubprocessCAKeySecretEnv carries the
// --dev-host-enrolment-ca-key-secret value the CA key Secret flag tests'
// child process starts main() with.
const managerSubprocessCAKeySecretEnv = "VIRTRIGAUD_TEST_DEV_CA_KEY_SECRET"

// TestDevHostEnrolmentCAKeySecretFlag_DevBuildRegistersIt: a development
// build defines --dev-host-enrolment-ca-key-secret and stores its value in
// the enrolment flags.
func TestDevHostEnrolmentCAKeySecretFlag_DevBuildRegistersIt(t *testing.T) {
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var f controller.HostEnrolmentFlags
	registerDevHostEnrolmentCAKeySecretFlag(fs, &f)
	require.NotNil(t, fs.Lookup(controller.DevHostEnrolmentCAKeySecretFlagName))
	require.NoError(t, fs.Parse([]string{"--" + controller.DevHostEnrolmentCAKeySecretFlagName + "=cert-manager/host-agent-ca"}))
	assert.Equal(t, "cert-manager/host-agent-ca", f.CAKeySecret)
}

// TestDevHostEnrolmentCAKeySecretFlag_DevBuildRefusesItAlone: in a
// development build the flag is an enrolment flag, so setting it without
// the rest of the enrolment flags stops the manager at startup (exit 1,
// before any cluster connection), as does an invalid value.
func TestDevHostEnrolmentCAKeySecretFlag_DevBuildRefusesItAlone(t *testing.T) {
	if os.Getenv(managerSubprocessEnv) == "1" {
		os.Args = []string{"manager", "--" + controller.DevHostEnrolmentCAKeySecretFlagName + "=" + os.Getenv(managerSubprocessCAKeySecretEnv)}
		main()
		return
	}
	for _, value := range []string{"cert-manager/host-agent-ca", "not-a-namespaced-name"} {
		t.Run(value, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestDevHostEnrolmentCAKeySecretFlag_DevBuildRefusesItAlone$") // #nosec G204 -- re-runs this test binary
			cmd.Env = append(os.Environ(), managerSubprocessEnv+"=1", managerSubprocessCAKeySecretEnv+"="+value,
				"KUBECONFIG="+t.TempDir()+"/none", "KUBERNETES_SERVICE_HOST=", "HOME="+t.TempDir())
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "the manager must exit: %s", out)
			assert.Equal(t, 1, exitErr.ExitCode())
			assert.Contains(t, string(out), "invalid per-host agent enrolment flags")
		})
	}
}
