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
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

// errReader fails every Get with err.
type errReader struct{ err error }

func (e errReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return e.err
}

func (e errReader) List(context.Context, client.ObjectList, ...client.ListOption) error { return e.err }

func TestGetWorkloadSecret(t *testing.T) {
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "team-a", Name: "user-data"}
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	withSecret := fake.NewClientBuilder().WithScheme(s).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}, Data: map[string][]byte{"userdata": []byte("x")}}).Build()
	uncovered := errReader{errors.New("unable to get: team-a/user-data because of unknown namespace for the cache")}

	// The cache answers: the live reader is not used (default informer).
	got := &corev1.Secret{}
	require.NoError(t, getWorkloadSecret(ctx, withSecret, errReader{errors.New("live read")}, key, got))
	assert.Equal(t, []byte("x"), got.Data["userdata"])

	// The cache does not cover the namespace: read live (namespaced grant).
	got = &corev1.Secret{}
	require.NoError(t, getWorkloadSecret(ctx, uncovered, withSecret, key, got))
	assert.Equal(t, []byte("x"), got.Data["userdata"])

	// The live reader's error is returned; with no live reader, the cache's.
	forbidden := apierrors.NewForbidden(corev1.Resource("secrets"), key.Name, errors.New("denied"))
	assert.True(t, apierrors.IsForbidden(getWorkloadSecret(ctx, uncovered, errReader{forbidden}, key, &corev1.Secret{})))
	assert.ErrorContains(t, getWorkloadSecret(ctx, uncovered, nil, key, &corev1.Secret{}), "unknown namespace for the cache")
}

func TestCheckSecretCacheAccess(t *testing.T) {
	ctx := context.Background()
	s := runtime.NewScheme()
	require.NoError(t, authorizationv1.AddToScheme(s))
	// allowed answers each review: list and watch are allowed in virt-a only.
	var asked []string
	c := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			r, ok := obj.(*authorizationv1.SelfSubjectAccessReview)
			require.True(t, ok)
			a := r.Spec.ResourceAttributes
			asked = append(asked, a.Namespace+":"+a.Verb+":"+a.Resource)
			r.Status.Allowed = a.Namespace == "virt-a"
			return nil
		},
	}).Build()

	require.NoError(t, CheckSecretCacheAccess(ctx, c, nil))
	assert.Empty(t, asked, "no review for the default informer")

	require.NoError(t, CheckSecretCacheAccess(ctx, c, []string{"virt-a"}))
	assert.Equal(t, []string{"virt-a:list:secrets", "virt-a:watch:secrets"}, asked)

	err := CheckSecretCacheAccess(ctx, c, []string{"virt-a", "virt-b"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "virt-b:list, virt-b:watch")
	assert.NotContains(t, err.Error(), "virt-a:")
}
