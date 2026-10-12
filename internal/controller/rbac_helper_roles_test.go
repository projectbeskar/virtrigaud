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

package controller

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// TestRBAC_HelperRolesUseTheAPIGroup pins that the admin/editor/viewer helper
// ClusterRoles in config/rbac grant the CRDs' real API group
// (infra.virtrigaud.io). They were scaffolded with a doubled group
// (infra.virtrigaud.io.infra.virtrigaud.io) that matches no resource, so they
// granted nothing.
func TestRBAC_HelperRolesUseTheAPIGroup(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "config", "rbac", "*_role.yaml"))
	require.NoError(t, err)
	checked := 0
	for _, f := range files {
		base := filepath.Base(f)
		if !strings.HasSuffix(base, "_admin_role.yaml") && !strings.HasSuffix(base, "_editor_role.yaml") &&
			!strings.HasSuffix(base, "_viewer_role.yaml") {
			continue
		}
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		role := &rbacv1.ClusterRole{}
		require.NoError(t, utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096).Decode(role), base)
		require.NotEmpty(t, role.Rules, base)
		for _, rule := range role.Rules {
			assert.Equal(t, []string{infravirtrigaudiov1beta1.GroupVersion.Group}, rule.APIGroups, base)
		}
		checked++
	}
	assert.Equal(t, 15, checked, "every helper role is checked")
}
