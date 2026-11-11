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
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

// TestSecretCacheNamespacesWithEnrolment: a restricted Secret cache gains
// the enrolment namespace when enrolment is enabled, so the pins Secret
// watch sees events; an unrestricted cache or no enrolment is unchanged.
func TestSecretCacheNamespacesWithEnrolment(t *testing.T) {
	enrol := &HostEnrolmentConfig{Namespace: "agents"}
	assert.Equal(t, []string{"agents", "team-a", "team-b"},
		SecretCacheNamespacesWithEnrolment([]string{"team-a", "team-b"}, enrol))
	assert.Equal(t, []string{"agents", "team-a"},
		SecretCacheNamespacesWithEnrolment([]string{"agents", "team-a"}, enrol), "already present")
	assert.Nil(t, SecretCacheNamespacesWithEnrolment(nil, enrol), "unrestricted cache stays unrestricted")
	assert.Equal(t, []string{"team-a"}, SecretCacheNamespacesWithEnrolment([]string{"team-a"}, nil), "no enrolment")

	in := []string{"team-a"}
	_ = SecretCacheNamespacesWithEnrolment(in, enrol)
	assert.Equal(t, []string{"team-a"}, in, "the input is not modified")
}

// TestMergeCacheByObject: the Secret and CertificateRequest cache options
// are combined, and nothing configured stays nil (every namespace).
func TestMergeCacheByObject(t *testing.T) {
	assert.Nil(t, MergeCacheByObject(nil, nil))

	secrets := SecretCacheByObject([]string{"agents", "team-a"})
	requests := HostEnrolmentCacheOptions(&HostEnrolmentConfig{Namespace: "agents"})
	merged := MergeCacheByObject(secrets, requests)
	assert.Len(t, merged, 2)
	for obj, by := range merged {
		switch obj.(type) {
		case *corev1.Secret:
			assert.Equal(t, map[string]cache.Config{"agents": {}, "team-a": {}}, by.Namespaces)
		default:
			assert.Equal(t, map[string]cache.Config{"agents": {}}, by.Namespaces)
		}
	}
	assert.Equal(t, secrets, MergeCacheByObject(secrets, nil))
}
