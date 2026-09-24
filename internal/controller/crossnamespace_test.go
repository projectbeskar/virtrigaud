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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// grantNamespace returns a Namespace named name whose grant annotation is
// value. A nil value leaves the annotation off entirely.
func grantNamespace(name string, value *string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if value != nil {
		ns.Annotations = map[string]string{AllowedSourceNamespacesAnnotation: *value}
	}
	return ns
}

func strPtr(s string) *string { return &s }

func TestNamespaceGrantsSource(t *testing.T) {
	cases := map[string]struct {
		value  *string
		source string
		want   bool
	}{
		"no annotation":                     {nil, "team-a", false},
		"empty annotation":                  {strPtr(""), "team-a", false},
		"exact single entry":                {strPtr("team-a"), "team-a", true},
		"one of several":                    {strPtr("team-x,team-a,team-y"), "team-a", true},
		"spaces around entries":             {strPtr("  team-x ,  team-a  "), "team-a", true},
		"tabs around entries":               {strPtr("team-x,\tteam-a\t"), "team-a", true},
		"other namespaces only":             {strPtr("team-b,team-c"), "team-a", false},
		"prefix is not a match":             {strPtr("team"), "team-a", false},
		"superstring is not a match":        {strPtr("team-a-dev"), "team-a", false},
		"case differs":                      {strPtr("Team-A"), "team-a", false},
		"wildcard is not supported":         {strPtr("*"), "team-a", false},
		"glob is not supported":             {strPtr("team-*"), "team-a", false},
		"semicolon is not a separator":      {strPtr("team-x;team-a"), "team-a", false},
		"empty entries are ignored":         {strPtr(",,team-a,,"), "team-a", true},
		"empty source never matches":        {strPtr(",,"), "", false},
		"empty source vs populated list":    {strPtr("team-a"), "", false},
		"internal whitespace is not a trim": {strPtr("team -a"), "team-a", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, namespaceGrantsSource(grantNamespace("team-b", tc.value), tc.source))
		})
	}
	assert.False(t, namespaceGrantsSource(nil, "team-a"), "a nil namespace grants nothing")
}

func crossNSTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func TestTargetNamespaceAllowed(t *testing.T) {
	ctx := context.Background()

	// countingReader counts Namespace reads so the same-namespace fast path is
	// proven to need no API read.
	reads := 0
	c := fake.NewClientBuilder().WithScheme(crossNSTestScheme(t)).
		WithObjects(
			grantNamespace("team-b", strPtr("team-a")),
			grantNamespace("team-c", strPtr("team-x")),
			grantNamespace("team-d", nil),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				reads++
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	for _, target := range []string{"", "team-a"} {
		ok, err := targetNamespaceAllowed(ctx, c, "team-a", target)
		require.NoError(t, err)
		assert.True(t, ok, "own namespace %q is always allowed", target)
	}
	assert.Zero(t, reads, "the own namespace needs no Namespace read")

	cases := map[string]bool{
		"team-b":  true,  // lists team-a
		"team-c":  false, // lists another namespace only
		"team-d":  false, // no annotation
		"missing": false, // does not exist: same answer as no grant
	}
	for target, want := range cases {
		ok, err := targetNamespaceAllowed(ctx, c, "team-a", target)
		require.NoError(t, err, target)
		assert.Equal(t, want, ok, target)
	}
}

func TestTargetNamespaceAllowed_ReadErrorFailsClosed(t *testing.T) {
	boom := errors.New("apiserver unavailable")
	c := fake.NewClientBuilder().WithScheme(crossNSTestScheme(t)).
		WithObjects(grantNamespace("team-b", strPtr("team-a"))).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return boom
			},
		}).
		Build()
	ok, err := targetNamespaceAllowed(context.Background(), c, "team-a", "team-b")
	require.Error(t, err)
	assert.ErrorIs(t, err, boom, "the read error is wrapped, not swallowed")
	assert.False(t, ok, "a read error never allows")
}

func TestTargetNamespaceNotAllowedMessage(t *testing.T) {
	msg := targetNamespaceNotAllowedMessage("team-a", "team-b")
	assert.Contains(t, msg, `"team-a"`)
	assert.Contains(t, msg, `"team-b"`)
	assert.Contains(t, msg, AllowedSourceNamespacesAnnotation)
}

func TestAllowedSourceNamespacesChangedPredicate(t *testing.T) {
	p := allowedSourceNamespacesChanged()
	with := func(v string) *corev1.Namespace { return grantNamespace("team-b", strPtr(v)) }
	without := grantNamespace("team-b", nil)

	assert.True(t, p.Create(event.CreateEvent{Object: with("team-a")}), "created with a grant")
	assert.False(t, p.Create(event.CreateEvent{Object: without}), "created without a grant")

	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: without, ObjectNew: with("team-a")}), "grant added")
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: with("team-a"), ObjectNew: without}), "grant removed")
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: with("team-a"), ObjectNew: with("team-x")}), "grant changed")
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: without, ObjectNew: with("")}), "empty grant added")
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: with("team-a"), ObjectNew: with("team-a")}), "unchanged")

	other := with("team-a")
	other.Labels = map[string]string{"x": "y"}
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: with("team-a"), ObjectNew: other}), "unrelated change")

	assert.False(t, p.Delete(event.DeleteEvent{Object: with("team-a")}))
	assert.False(t, p.Generic(event.GenericEvent{Object: with("team-a")}))
}
