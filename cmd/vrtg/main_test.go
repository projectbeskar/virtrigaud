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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/cmd/internal/envtestutil"
)

// runVrtg runs the vrtg command tree in-process against the kubeconfig and
// returns its standard output.
func runVrtg(t *testing.T, kubeconfig string, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"--kubeconfig", kubeconfig}, args...))
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), err
}

// TestVrtgAgainstAPIServer runs the read commands against a real apiserver.
func TestVrtgAgainstAPIServer(t *testing.T) {
	env := envtestutil.Start(t)
	ctx := context.Background()

	for _, ns := range []string{"vrtg-a", "vrtg-b"} {
		require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	}
	require.NoError(t, env.Client.Create(ctx, &infrav1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "lab-provider", Namespace: "vrtg-a"},
		Spec: infrav1beta1.ProviderSpec{
			Type:     infrav1beta1.ProviderTypeLibvirt,
			Endpoint: "qemu+tcp://libvirt.example.com/system",

			CredentialSecretRef: infrav1beta1.ObjectRef{Name: "libvirt-creds"},
			Runtime: &infrav1beta1.ProviderRuntimeSpec{
				Mode:  infrav1beta1.RuntimeModeRemote,
				Image: "example.invalid/provider-libvirt:test",
			},
		},
	}))
	require.NoError(t, env.Client.Create(ctx, &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "vrtg-a"},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "lab-provider"},
			ClassRef:    infrav1beta1.ObjectRef{Name: "small"},
			ImageRef:    &infrav1beta1.ObjectRef{Name: "ubuntu"},
		},
	}))
	// A VM without spec.imageRef (it boots from an imported disk) must not
	// crash the table output.
	require.NoError(t, env.Client.Create(ctx, &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "adopted-1", Namespace: "vrtg-b"},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef:  infrav1beta1.ObjectRef{Name: "lab-provider", Namespace: "vrtg-a"},
			ClassRef:     infrav1beta1.ObjectRef{Name: "small"},
			ImportedDisk: &infrav1beta1.ImportedDiskRef{DiskID: "disk-1"},
		},
	}))

	t.Run("vm list shows every namespace by default", func(t *testing.T) {
		out, err := runVrtg(t, env.KubeconfigPath, "vm", "list")
		require.NoError(t, err)
		assert.Contains(t, out, "NAMESPACE")
		assert.Contains(t, out, "web-1")
		assert.Contains(t, out, "adopted-1")
		assert.Contains(t, out, "lab-provider")
	})

	t.Run("vm list honours an explicit namespace", func(t *testing.T) {
		out, err := runVrtg(t, env.KubeconfigPath, "vm", "list", "-n", "vrtg-a")
		require.NoError(t, err)
		assert.Contains(t, out, "web-1")
		assert.NotContains(t, out, "adopted-1")
	})

	t.Run("vm describe", func(t *testing.T) {
		out, err := runVrtg(t, env.KubeconfigPath, "vm", "describe", "web-1", "-n", "vrtg-a")
		require.NoError(t, err)
		assert.Contains(t, out, "Name: web-1")
		assert.Contains(t, out, "Provider: lab-provider")
		assert.Contains(t, out, "Image: ubuntu")
	})

	t.Run("vm describe of a missing VM fails", func(t *testing.T) {
		_, err := runVrtg(t, env.KubeconfigPath, "vm", "describe", "nope", "-n", "vrtg-a")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("vm console-url", func(t *testing.T) {
		out, err := runVrtg(t, env.KubeconfigPath, "vm", "console-url", "web-1", "-n", "vrtg-a")
		require.NoError(t, err)
		assert.Contains(t, out, "No console URL available for VM web-1")
	})

	t.Run("vm events", func(t *testing.T) {
		out, err := runVrtg(t, env.KubeconfigPath, "vm", "events", "web-1", "-n", "vrtg-a")
		require.NoError(t, err)
		assert.Contains(t, out, "LAST SEEN")
	})

	t.Run("vm list as json carries apiVersion and kind", func(t *testing.T) {
		out, err := runVrtg(t, env.KubeconfigPath, "vm", "list", "-n", "vrtg-a", "-o", "json")
		require.NoError(t, err)
		var list struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Items      []struct {
				Kind     string `json:"kind"`
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"items"`
		}
		assert.NotContains(t, out, "managedFields")
		require.NoError(t, json.Unmarshal([]byte(out), &list), out)
		assert.Equal(t, "infra.virtrigaud.io/v1beta1", list.APIVersion)
		assert.Equal(t, "VirtualMachineList", list.Kind)
		require.Len(t, list.Items, 1)
		assert.Equal(t, "VirtualMachine", list.Items[0].Kind)
		assert.Equal(t, "web-1", list.Items[0].Metadata.Name)
	})

	t.Run("vm describe as yaml", func(t *testing.T) {
		out, err := runVrtg(t, env.KubeconfigPath, "vm", "describe", "web-1", "-n", "vrtg-a", "-o", "yaml")
		require.NoError(t, err)
		var vm infrav1beta1.VirtualMachine
		require.NoError(t, yaml.Unmarshal([]byte(out), &vm), out)
		assert.Equal(t, "web-1", vm.Name)
		assert.Equal(t, "VirtualMachine", vm.Kind)
		assert.Equal(t, "lab-provider", vm.Spec.ProviderRef.Name)
	})

	t.Run("provider list", func(t *testing.T) {
		out, err := runVrtg(t, env.KubeconfigPath, "provider", "list")
		require.NoError(t, err)
		assert.Contains(t, out, "lab-provider")
		assert.Contains(t, out, "libvirt")
		assert.Contains(t, out, "qemu+tcp://libvirt.example.com/system")
	})

	t.Run("provider status", func(t *testing.T) {
		out, err := runVrtg(t, env.KubeconfigPath, "provider", "status", "lab-provider", "-n", "vrtg-a")
		require.NoError(t, err)
		assert.Contains(t, out, "Provider: lab-provider")
		assert.Contains(t, out, "Healthy: false")
		assert.NotContains(t, out, "Status: Available", "status must come from the object, not be hard-coded")
	})

	t.Run("provider list as json", func(t *testing.T) {
		out, err := runVrtg(t, env.KubeconfigPath, "provider", "list", "-o", "json")
		require.NoError(t, err)
		assert.Contains(t, out, `"kind": "ProviderList"`)
	})

	t.Run("unsupported output format fails", func(t *testing.T) {
		_, err := runVrtg(t, env.KubeconfigPath, "provider", "list", "-o", "xml")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported output format")
	})
}

// TestVrtgNotImplementedFails checks that a subcommand without an
// implementation reports that instead of succeeding.
func TestVrtgNotImplementedFails(t *testing.T) {
	tests := []struct {
		args []string
	}{
		{[]string{"snapshot", "create", "vm", "snap"}},
		{[]string{"snapshot", "list"}},
		{[]string{"clone", "run", "a", "b"}},
		{[]string{"conformance", "run", "p"}},
		{[]string{"diag", "bundle"}},
		{[]string{"provider", "logs", "p"}},
		{[]string{"init"}},
	}
	for _, tt := range tests {
		t.Run(tt.args[0], func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			require.ErrorIs(t, err, errNotImplemented)
		})
	}
}

// TestVrtgBadKubeconfig checks that --kubeconfig is honoured and a bad path
// is an error rather than silently falling back to another cluster.
func TestVrtgBadKubeconfig(t *testing.T) {
	_, err := runVrtg(t, "/nonexistent/kubeconfig", "vm", "list")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/nonexistent/kubeconfig")
}
