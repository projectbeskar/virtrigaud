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

// Package envtestutil starts a real kube-apiserver (controller-runtime
// envtest) with the VirtRigaud CRDs installed, for the command-line tools'
// tests. Only test code imports it.
package envtestutil

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/projectbeskar/virtrigaud/cmd/internal/cliclient"
)

// Env is a running envtest apiserver.
type Env struct {
	// Config is the admin connection settings of the apiserver.
	Config *rest.Config
	// KubeconfigPath is a kubeconfig file, in the test's temporary directory,
	// that connects to the apiserver as an administrator.
	KubeconfigPath string
	// Client is a client that knows the VirtRigaud API types.
	Client client.Client
}

// Start starts an apiserver with the CRDs from config/crd/bases and registers
// its shutdown with t.Cleanup. It skips the test when no envtest binaries are
// available (KUBEBUILDER_ASSETS unset and no bin/k8s/* in the repository).
func Start(t testing.TB) *Env {
	t.Helper()

	root := repoRoot(t)
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		dir := firstAssetsDir(filepath.Join(root, "bin", "k8s"))
		if dir == "" {
			t.Skip("envtest binaries not found: set KUBEBUILDER_ASSETS or run `make setup-envtest`")
		}
		env.BinaryAssetsDirectory = dir
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

	user, err := env.AddUser(envtest.User{Name: "vrtg-test-admin", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatalf("add envtest admin user: %v", err)
	}
	kubeconfig, err := user.KubeConfig()
	if err != nil {
		t.Fatalf("render envtest kubeconfig: %v", err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, kubeconfig, 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	c, err := cliclient.NewClient(cfg)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	return &Env{Config: cfg, KubeconfigPath: path, Client: c}
}

// repoRoot returns the repository root, found by walking up from the working
// directory to the directory holding go.mod.
func repoRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repository root (go.mod) not found above %s", dir)
		}
		dir = parent
	}
}

// firstAssetsDir returns the first sub-directory of base, or "" when there is
// none.
func firstAssetsDir(base string) string {
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(base, entry.Name())
		}
	}
	return ""
}
