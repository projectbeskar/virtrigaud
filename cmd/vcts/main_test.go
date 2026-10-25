/*
Copyright 2025.

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
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/cmd/internal/envtestutil"
)

// helperEnv makes the test binary behave as the vcts binary: TestMain runs
// main() instead of the tests when it is set, so a test can check the real
// process exit code.
const helperEnv = "VCTS_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

const validSpec = `- name: vm-create-delete
  description: Test basic VM creation and deletion
  steps:
    - name: create-vmclass
      type: create
      resource:
        apiVersion: infra.virtrigaud.io/v1beta1
        kind: VMClass
        metadata:
          name: vcts-${RUN}-class
`

// invalidSpec has an upper-case test name, which the validator rejects.
const invalidSpec = `- name: Not_A_Valid_Name
  description: x
  steps:
    - name: s
      type: create
`

const malformedSpec = "this: is: not: [valid yaml\n"

// writeSpecs writes name->content files into a new directory.
func writeSpecs(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	return dir
}

// runVcts runs the vcts command tree in-process.
func runVcts(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), err
}

func TestValidateReportsInvalidSpecs(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		wantErr bool
		wantOut string
	}{
		{name: "valid", files: map[string]string{"a.yaml": validSpec}, wantOut: "All tests are valid!"},
		{name: "invalid test name", files: map[string]string{"a.yaml": invalidSpec}, wantErr: true, wantOut: "ERROR"},
		{name: "malformed yaml", files: map[string]string{"a.yaml": malformedSpec}, wantErr: true, wantOut: "ERROR"},
		{
			name:    "one bad file among good ones",
			files:   map[string]string{"a.yaml": validSpec, "b.yaml": invalidSpec},
			wantErr: true,
			wantOut: "Invalid Files: 1",
		},
		{name: "no spec files", files: map[string]string{"readme.txt": "x"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runVcts(t, "validate", writeSpecs(t, tt.files))
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Contains(t, out, tt.wantOut)
			if tt.wantErr && tt.wantOut != "" {
				assert.NotContains(t, out, "All tests are valid!")
			}
		})
	}
}

func TestValidateErrorIsErrValidationFailed(t *testing.T) {
	_, err := runVcts(t, "validate", writeSpecs(t, map[string]string{"a.yaml": invalidSpec}))
	require.True(t, errors.Is(err, errValidationFailed), "got %v", err)
}

// TestValidateExitCode runs the real binary entry point and checks the exit
// status: 0 for a valid spec directory, non-zero for an invalid one.
func TestValidateExitCode(t *testing.T) {
	run := func(dir string) (int, string) {
		cmd := exec.Command(os.Args[0], "validate", dir)
		cmd.Env = append(os.Environ(), helperEnv+"=1")
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), out.String()
		}
		require.NoError(t, err)
		return 0, out.String()
	}

	code, out := run(writeSpecs(t, map[string]string{"a.yaml": validSpec}))
	assert.Equal(t, 0, code, out)

	code, out = run(writeSpecs(t, map[string]string{"a.yaml": invalidSpec}))
	assert.Equal(t, 1, code, out)
	assert.Contains(t, out, "validation failed")
}

// TestValidateRepositorySpecs checks the specs CI validates stay valid.
func TestValidateRepositorySpecs(t *testing.T) {
	out, err := runVcts(t, "validate", filepath.Join("..", "..", "test", "conformance", "specs"))
	require.NoError(t, err, out)
}

// TestRunAgainstAPIServer runs 'vcts run' against a real apiserver. The point
// is the client path: the Provider must be read through a scheme that knows the
// VirtRigaud types.
func TestRunAgainstAPIServer(t *testing.T) {
	env := envtestutil.Start(t)
	ctx := context.Background()

	require.NoError(t, env.Client.Create(ctx, &infrav1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "conf-provider", Namespace: "default"},
		Spec: infrav1beta1.ProviderSpec{
			Type:                infrav1beta1.ProviderTypeLibvirt,
			Endpoint:            "qemu+tcp://libvirt.example.com/system",
			CredentialSecretRef: infrav1beta1.ObjectRef{Name: "libvirt-creds"},
			Runtime: &infrav1beta1.ProviderRuntimeSpec{
				Mode:  infrav1beta1.RuntimeModeRemote,
				Image: "example.invalid/provider-libvirt:test",
			},
		},
	}))

	// The runner reads test/conformance/specs relative to the working directory.
	t.Chdir(filepath.Join("..", ".."))

	t.Run("missing provider is a not-found error, not a scheme error", func(t *testing.T) {
		_, err := runVcts(t, "run", "--kubeconfig", env.KubeconfigPath, "-p", "absent",
			"-o", t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
		assert.NotContains(t, err.Error(), "no kind is registered")
	})

	t.Run("existing provider is read; the shipped suite needs --set IMAGE", func(t *testing.T) {
		_, err := runVcts(t, "run", "--kubeconfig", env.KubeconfigPath, "-p", "conf-provider",
			"-o", t.TempDir())
		// The run got past reading the Provider and stops before creating
		// anything because the shipped specifications need ${IMAGE}.
		require.Error(t, err)
		assert.Contains(t, err.Error(), "pass --set IMAGE=<value>")
	})

	t.Run("--set rejects a built-in variable and a malformed entry", func(t *testing.T) {
		_, err := runVcts(t, "run", "--kubeconfig", env.KubeconfigPath, "-p", "conf-provider", "--set", "RUN=x")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot be overridden")
		_, err = runVcts(t, "run", "--kubeconfig", env.KubeconfigPath, "-p", "conf-provider", "--set", "IMAGE")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "want NAME=value")
	})

	t.Run("list", func(t *testing.T) {
		out, err := runVcts(t, "list")
		require.NoError(t, err)
		assert.Contains(t, out, "vm-create-delete")
		assert.Contains(t, out, "Variables (--set): IMAGE")
	})
}
