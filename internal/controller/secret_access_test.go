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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestParseSecretCacheNamespaces(t *testing.T) {
	for in, want := range map[string][]string{
		"":                        nil,
		" , ":                     nil,
		"virt-a":                  {"virt-a"},
		" virt-b , virt-a,virt-b": {"virt-a", "virt-b"},
	} {
		got, err := ParseSecretCacheNamespaces(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"Virt", "a/b", "a_b", "-a"} {
		_, err := ParseSecretCacheNamespaces(bad)
		assert.ErrorContains(t, err, SecretCacheNamespacesFlagName, bad)
	}
}

func TestSecretCacheByObject(t *testing.T) {
	assert.Nil(t, SecretCacheByObject(nil), "no restriction: every namespace")

	byObject := SecretCacheByObject([]string{"virt-a", "virt-b"})
	require.Len(t, byObject, 1)
	for obj, opts := range byObject {
		assert.IsType(t, &corev1.Secret{}, obj, "only the Secret informer is restricted")
		assert.Len(t, opts.Namespaces, 2)
		assert.Contains(t, opts.Namespaces, "virt-a")
		assert.Contains(t, opts.Namespaces, "virt-b")
	}
}
