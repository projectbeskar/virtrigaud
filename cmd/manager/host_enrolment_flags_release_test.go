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
	"flag"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/controller"
)

// TestDevHostEnrolmentCAKeySecretFlag_ReleaseBuildHasNoFlag: a release build
// does not define --dev-host-enrolment-ca-key-secret, so a release manager
// started with it stops at flag parsing (exit 2, before any cluster
// connection).
func TestDevHostEnrolmentCAKeySecretFlag_ReleaseBuildHasNoFlag(t *testing.T) {
	if os.Getenv(managerSubprocessEnv) == "1" {
		os.Args = []string{"manager", "--" + controller.DevHostEnrolmentCAKeySecretFlagName + "=cert-manager/host-agent-ca"}
		main()
		return
	}
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	var f controller.HostEnrolmentFlags
	registerDevHostEnrolmentCAKeySecretFlag(fs, &f)
	assert.Nil(t, fs.Lookup(controller.DevHostEnrolmentCAKeySecretFlagName), "a release build registers no such flag")

	cmd := exec.Command(os.Args[0], "-test.run=^TestDevHostEnrolmentCAKeySecretFlag_ReleaseBuildHasNoFlag$") // #nosec G204 -- re-runs this test binary
	cmd.Env = append(os.Environ(), managerSubprocessEnv+"=1",
		"KUBECONFIG="+t.TempDir()+"/none", "KUBERNETES_SERVICE_HOST=", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "the manager must exit: %s", out)
	assert.Equal(t, 2, exitErr.ExitCode())
	assert.Contains(t, string(out), "flag provided but not defined: -"+controller.DevHostEnrolmentCAKeySecretFlagName)
}
