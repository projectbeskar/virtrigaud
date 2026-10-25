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

package cliclient

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

func TestNewSchemeKnowsBuiltInAndVirtRigaudTypes(t *testing.T) {
	scheme, err := NewScheme()
	require.NoError(t, err)

	for _, obj := range []runtime.Object{
		&infrav1beta1.VirtualMachine{},
		&infrav1beta1.Provider{},
		&corev1.Namespace{},
	} {
		kinds, _, err := scheme.ObjectKinds(obj)
		require.NoError(t, err, "%T", obj)
		assert.NotEmpty(t, kinds, "%T", obj)
	}
}

func TestRESTConfigExplicitPath(t *testing.T) {
	t.Run("loads the named file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "kubeconfig")
		require.NoError(t, os.WriteFile(path, []byte(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: x
  context: {cluster: c, user: u}
current-context: x
users:
- name: u
  user: {token: t}
`), 0o600))
		cfg, err := RESTConfig(path)
		require.NoError(t, err)
		assert.Equal(t, "https://127.0.0.1:6443", cfg.Host)
	})

	t.Run("a missing file is an error, not a fallback", func(t *testing.T) {
		_, err := RESTConfig(filepath.Join(t.TempDir(), "absent"))
		require.Error(t, err)
	})
}
