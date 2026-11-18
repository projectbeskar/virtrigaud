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
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/cmd/internal/envtestutil"
	"github.com/projectbeskar/virtrigaud/internal/controller"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	"github.com/projectbeskar/virtrigaud/internal/operation"
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

// managerSubprocessArgsEnv carries the newline-separated arguments the
// enrolment startup-check test's child process starts main() with.
const managerSubprocessArgsEnv = "VIRTRIGAUD_TEST_MANAGER_ARGS"

// TestDevHostEnrolment_RefusesWhenSecretsReadableClusterWide: a development
// manager with valid enrolment flags, connected to a real API server as a
// user who may read every Secret (system:masters), refuses to run per-host
// agent enrolment at startup (exit 1) and names the accesses it found, and
// never the contents of a Secret.
func TestDevHostEnrolment_RefusesWhenSecretsReadableClusterWide(t *testing.T) {
	if os.Getenv(managerSubprocessEnv) == "1" {
		os.Args = append([]string{"manager"}, strings.Split(os.Getenv(managerSubprocessArgsEnv), "\n")...)
		main()
		return
	}
	env := envtestutil.Start(t, filepath.Join("test", "crds", "cert-manager"))
	dir := t.TempDir()
	signer := filepath.Join(dir, "signer.pem")
	data, err := operation.GenerateDevSignerKey()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(signer, data, 0o600))
	ca := hostidentitytest.NewCA(t)
	bundle := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(bundle, ca.PEM, 0o600))
	args := []string{
		"--metrics-bind-address=0", "--health-probe-bind-address=0",
		"--dev-operation-signer-key-file=" + signer,
		"--" + controller.DevHostEnrolmentNamespaceFlagName + "=host-agent-pki",
		"--" + controller.DevHostEnrolmentIssuerFlagName + "=ClusterIssuer/host-agent-ca",
		"--" + controller.DevHostEnrolmentAdminGroupFlagName + "=enrolment-admins",
		"--" + controller.DevHostEnrolmentTrustDomainFlagName + "=cluster.test",
		"--" + controller.DevHostEnrolmentCABundleFileFlagName + "=" + bundle,
		"--" + controller.DevHostEnrolmentCAKeySecretFlagName + "=cert-manager/host-agent-ca",
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDevHostEnrolment_RefusesWhenSecretsReadableClusterWide$") // #nosec G204 -- re-runs this test binary
	cmd.Env = append(os.Environ(), managerSubprocessEnv+"=1", managerSubprocessArgsEnv+"="+strings.Join(args, "\n"),
		"KUBECONFIG="+env.KubeconfigPath, "KUBERNETES_SERVICE_HOST=", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "the manager must exit: %s", out)
	assert.Equal(t, 1, exitErr.ExitCode(), "%s", out)
	assert.Contains(t, string(out), "refusing to run per-host agent enrolment")
	assert.Contains(t, string(out), "get secrets in all namespaces")
	assert.Contains(t, string(out), "get secrets named cert-manager/host-agent-ca")
	assert.NotContains(t, string(out), "admission webhook", "the Secret check runs before the admission check")
}
