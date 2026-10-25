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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/cmd/internal/envtestutil"
)

const createOnlyConfig = `duration: 3s
concurrency: 2
providers: [lab-provider]
vmTemplate:
  classRef: small
  imageRef: ubuntu
  labels:
    team: perf
operations:
  create: 100
  delete: 0
  power: 0
  reconfigure: 0
  snapshot: 0
  clone: 0
  describe: 0
`

// writeConfig writes content to a config file in a new directory.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "loadtest.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestLoadConfig(t *testing.T) {
	t.Run("no file gives the defaults", func(t *testing.T) {
		cfg, err := loadConfig("")
		require.NoError(t, err)
		assert.Equal(t, getDefaultConfig(), cfg)
	})

	t.Run("a file overrides the defaults", func(t *testing.T) {
		cfg, err := loadConfig(writeConfig(t, createOnlyConfig))
		require.NoError(t, err)
		assert.Equal(t, 3*time.Second, cfg.Duration)
		assert.Equal(t, 2, cfg.Concurrency)
		assert.Equal(t, []string{"lab-provider"}, cfg.Providers)
		assert.Equal(t, "small", cfg.VMTemplate.ClassRef)
		assert.Equal(t, "perf", cfg.VMTemplate.Labels["team"])
		assert.InDelta(t, 100.0, cfg.Operations.Create, 0.001)
		assert.InDelta(t, 0.0, cfg.Operations.Describe, 0.001)
	})

	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "unknown key", content: "durration: 3s\n", wantErr: "durration"},
		{name: "a config in another format", content: "provider:\n  name: x\nscenarios: []\n", wantErr: "provider"},
		{name: "bad duration", content: "duration: soon\n", wantErr: "parse config"},
		{name: "zero duration", content: "duration: 0s\n", wantErr: "duration must be positive"},
		{name: "no workers", content: "concurrency: 0\n", wantErr: "concurrency"},
		{name: "negative weight", content: "operations:\n  create: -1\n", wantErr: "negative"},
		{
			name:    "all weights zero",
			content: "operations: {create: 0, delete: 0, power: 0, reconfigure: 0, snapshot: 0, clone: 0, describe: 0}\n",
			wantErr: "all be zero",
		},
		{name: "no providers", content: "providers: []\n", wantErr: "providers"},
		{name: "cpu range of one value", content: "vmTemplate: {classRef: c, imageRef: i, resources: {cpuRange: [2]}}\n", wantErr: "cpuRange must be [min, max]"},
		{name: "cpu range reversed", content: "vmTemplate: {classRef: c, imageRef: i, resources: {cpuRange: [4, 2]}}\n", wantErr: "cpuRange"},
		{name: "cpu range out of bounds", content: "vmTemplate: {classRef: c, imageRef: i, resources: {cpuRange: [0, 2]}}\n", wantErr: "1 <= min"},
		{name: "memory range too small", content: "vmTemplate: {classRef: c, imageRef: i, resources: {memoryRange: [64, 512]}}\n", wantErr: "memoryRange"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadConfig(writeConfig(t, tt.content))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}

	t.Run("missing file", func(t *testing.T) {
		_, err := loadConfig(filepath.Join(t.TempDir(), "absent.yaml"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read config")
	})
}

// runLoadgen runs the loadgen command tree in-process.
func runLoadgen(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), err
}

// listVMs returns the VirtualMachines in namespace ns.
func listVMs(t *testing.T, c client.Client, ns string) []infrav1beta1.VirtualMachine {
	t.Helper()
	list := &infrav1beta1.VirtualMachineList{}
	require.NoError(t, c.List(context.Background(), list, client.InNamespace(ns)))
	return list.Items
}

// TestLoadgenAgainstAPIServer runs the tool against a real apiserver.
func TestLoadgenAgainstAPIServer(t *testing.T) {
	env := envtestutil.Start(t)
	ctx := context.Background()

	t.Run("run honours --config and creates VMs", func(t *testing.T) {
		outDir := t.TempDir()
		out, err := runLoadgen(t, "run", "--kubeconfig", env.KubeconfigPath, "-n", "default",
			"--config", writeConfig(t, createOnlyConfig), "--output-dir", outDir)
		require.NoError(t, err, out)
		// The values come from the file, not the defaults (5m, 2 workers, test-provider).
		assert.Contains(t, out, "Duration: 3s")
		assert.Contains(t, out, "Providers: [lab-provider]")

		vms := listVMs(t, env.Client, "default")
		require.NotEmpty(t, vms, "the run created no VMs")
		for _, vm := range vms {
			assert.Equal(t, "lab-provider", vm.Spec.ProviderRef.Name)
			assert.Equal(t, "small", vm.Spec.ClassRef.Name)
			require.NotNil(t, vm.Spec.ImageRef)
			assert.Equal(t, "ubuntu", vm.Spec.ImageRef.Name)
			assert.Equal(t, loadgenLabelValue, vm.Labels[loadgenLabelKey])
			assert.Equal(t, "perf", vm.Labels["team"])
		}

		csv, err := os.ReadFile(filepath.Join(outDir, "results.csv"))
		require.NoError(t, err)
		assert.Contains(t, string(csv), "create,lab-provider,loadgen-vm-")
		assert.Contains(t, string(csv), ",true,")
		assert.FileExists(t, filepath.Join(outDir, "summary.md"))
	})

	t.Run("a second run in the same namespace does not collide", func(t *testing.T) {
		before := len(listVMs(t, env.Client, "default"))
		_, err := runLoadgen(t, "run", "--kubeconfig", env.KubeconfigPath,
			"--config", writeConfig(t, createOnlyConfig), "--output-dir", t.TempDir())
		require.NoError(t, err)
		assert.Greater(t, len(listVMs(t, env.Client, "default")), before)
	})

	t.Run("dry run creates nothing", func(t *testing.T) {
		before := len(listVMs(t, env.Client, "default"))
		_, err := runLoadgen(t, "run", "--kubeconfig", env.KubeconfigPath, "--dry-run",
			"--config", writeConfig(t, createOnlyConfig), "--output-dir", t.TempDir())
		require.NoError(t, err)
		assert.Len(t, listVMs(t, env.Client, "default"), before)
	})

	t.Run("an unusable config fails before touching the cluster", func(t *testing.T) {
		before := len(listVMs(t, env.Client, "default"))
		_, err := runLoadgen(t, "run", "--kubeconfig", env.KubeconfigPath,
			"--config", writeConfig(t, "durration: 3s\n"), "--output-dir", t.TempDir())
		require.Error(t, err)
		assert.Len(t, listVMs(t, env.Client, "default"), before)
	})

	t.Run("delete removes only VMs the run created", func(t *testing.T) {
		const ns = "default"
		bystander := &infrav1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "bystander", Namespace: ns},
			Spec: infrav1beta1.VirtualMachineSpec{
				ProviderRef: infrav1beta1.ObjectRef{Name: "lab-provider"},
				ClassRef:    infrav1beta1.ObjectRef{Name: "small"},
				ImageRef:    &infrav1beta1.ObjectRef{Name: "ubuntu"},
			},
		}
		require.NoError(t, env.Client.Create(ctx, bystander))

		cfg := getDefaultConfig()
		cfg.Providers = []string{"lab-provider"}
		lg := &LoadGenerator{
			client:    env.Client,
			namespace: ns,
			config:    cfg,
			results:   make(chan Result, 10),
			opts:      &options{outputDir: t.TempDir()},
			runID:     "test",
		}

		// With nothing created by this run, delete fails and leaves the
		// bystander alone.
		lg.performDelete(ctx, "lab-provider", 0)
		res := <-lg.results
		assert.False(t, res.Success)
		require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "bystander"}, &infrav1beta1.VirtualMachine{}))

		lg.performCreate(ctx, "lab-provider", 7)
		created := <-lg.results
		require.True(t, created.Success, created.Error)
		key := types.NamespacedName{Namespace: ns, Name: created.VMName}
		require.NoError(t, env.Client.Get(ctx, key, &infrav1beta1.VirtualMachine{}))

		lg.performDelete(ctx, "lab-provider", 7)
		deleted := <-lg.results
		require.True(t, deleted.Success, deleted.Error)
		assert.Equal(t, created.VMName, deleted.VMName)
		err := env.Client.Get(ctx, key, &infrav1beta1.VirtualMachine{})
		require.Error(t, err)
		assert.True(t, client.IgnoreNotFound(err) == nil, "want not found, got %v", err)

		require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "bystander"}, &infrav1beta1.VirtualMachine{}))
	})
}
