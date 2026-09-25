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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// These tests pin the cross-namespace consumer grant
// (spec.consumerNamespaceSelector on Provider, VMClass and VMImage): the own
// namespace is always allowed, nil denies every other namespace, {} allows
// every namespace, and a selector is matched against the consumer Namespace's
// labels.

const (
	cgOwnerNS    = "infra"
	cgConsumerNS = "team-a"
)

// labeledNamespace returns a Namespace with the automatic
// kubernetes.io/metadata.name label plus extra labels.
func labeledNamespace(name string, extra map[string]string) *corev1.Namespace {
	l := map[string]string{corev1.LabelMetadataName: name}
	for k, v := range extra {
		l[k] = v
	}
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}}
}

// sharedWith returns a selector matching the given labels.
func sharedWith(l map[string]string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: l}
}

// sharedWithAll is the empty selector: every namespace.
func sharedWithAll() *metav1.LabelSelector { return &metav1.LabelSelector{} }

// sharedWithNamespace selects exactly one namespace by its automatic name label.
func sharedWithNamespace(name string) *metav1.LabelSelector {
	return sharedWith(map[string]string{corev1.LabelMetadataName: name})
}

func grantedProvider(ns, name string, sel *metav1.LabelSelector) *infravirtrigaudiov1beta1.Provider {
	p := withRuntime(singleProviderCR(name, ns))
	p.Spec.ConsumerNamespaceSelector = sel
	return p
}

func grantedClass(ns, name string, sel *metav1.LabelSelector) *infravirtrigaudiov1beta1.VMClass {
	c := smallVMClass(ns)
	c.Name = name
	c.Spec.ConsumerNamespaceSelector = sel
	return c
}

func grantedImage(ns, name string, sel *metav1.LabelSelector) *infravirtrigaudiov1beta1.VMImage {
	i := minimalVMImage(ns)
	i.Name = name
	i.Spec.ConsumerNamespaceSelector = sel
	return i
}

func cgClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(coverageTestScheme(t)).WithObjects(objs...).Build()
}

func TestConsumerAllowed(t *testing.T) {
	team := labeledNamespace(cgConsumerNS, map[string]string{"virtrigaud.io/tenant": "gold"})
	cases := map[string]struct {
		sel   *metav1.LabelSelector
		objNS string
		ns    *corev1.Namespace
		want  bool
	}{
		"own namespace is always allowed, even with no selector": {nil, cgConsumerNS, team, true},
		"nil selector denies another namespace":                  {nil, cgOwnerNS, team, false},
		"empty selector allows every namespace":                  {sharedWithAll(), cgOwnerNS, team, true},
		"empty selector allows without reading the namespace":    {sharedWithAll(), cgOwnerNS, nil, true},
		"matching label":                           {sharedWith(map[string]string{"virtrigaud.io/tenant": "gold"}), cgOwnerNS, team, true},
		"non-matching label value":                 {sharedWith(map[string]string{"virtrigaud.io/tenant": "silver"}), cgOwnerNS, team, false},
		"label absent":                             {sharedWith(map[string]string{"other": "x"}), cgOwnerNS, team, false},
		"explicit namespace name":                  {sharedWithNamespace(cgConsumerNS), cgOwnerNS, team, true},
		"another explicit namespace name":          {sharedWithNamespace("team-b"), cgOwnerNS, team, false},
		"matchExpressions In":                      {&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: corev1.LabelMetadataName, Operator: metav1.LabelSelectorOpIn, Values: []string{"team-z", cgConsumerNS}}}}, cgOwnerNS, team, true},
		"matchExpressions NotIn":                   {&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: corev1.LabelMetadataName, Operator: metav1.LabelSelectorOpNotIn, Values: []string{cgConsumerNS}}}}, cgOwnerNS, team, false},
		"consumer namespace missing is denied":     {sharedWithNamespace(cgConsumerNS), cgOwnerNS, nil, false},
		"a selector that does not parse is denied": {&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "k", Operator: "Bogus"}}}, cgOwnerNS, team, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var objs []client.Object
			if tc.ns != nil {
				objs = append(objs, tc.ns)
			}
			c := cgClient(t, objs...)
			for _, obj := range []client.Object{
				grantedProvider(tc.objNS, "p", tc.sel),
				grantedClass(tc.objNS, "c", tc.sel),
				grantedImage(tc.objNS, "i", tc.sel),
			} {
				got, err := consumerAllowed(context.Background(), c, obj, cgConsumerNS)
				require.NoError(t, err)
				assert.Equal(t, tc.want, got, "%T", obj)
			}
		})
	}
}

func TestConsumerAllowed_NamespaceReadErrorFailsClosed(t *testing.T) {
	boom := errors.New("apiserver unavailable")
	c := fake.NewClientBuilder().WithScheme(coverageTestScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Namespace); ok {
				return boom
			}
			return cl.Get(ctx, key, obj, opts...)
		}}).Build()
	allowed, err := consumerAllowed(context.Background(), c, grantedProvider(cgOwnerNS, "p", sharedWithNamespace(cgConsumerNS)), cgConsumerNS)
	require.ErrorIs(t, err, boom)
	assert.False(t, allowed)
	require.Error(t, checkConsumer(context.Background(), c, grantedProvider(cgOwnerNS, "p", sharedWithNamespace(cgConsumerNS)), cgConsumerNS))
}

func TestConsumerAllowed_UnknownKindIsDenied(t *testing.T) {
	allowed, err := consumerAllowed(context.Background(), cgClient(t), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: cgOwnerNS, Name: "x"}}, cgConsumerNS)
	require.NoError(t, err)
	assert.False(t, allowed)
}

func TestCheckConsumer_ErrorNamesOnlyTheObjectAndField(t *testing.T) {
	sel := sharedWith(map[string]string{"secret-tenant-label": "gold"})
	c := cgClient(t, labeledNamespace(cgConsumerNS, nil))
	for _, obj := range []client.Object{
		grantedProvider(cgOwnerNS, "shared", sel),
		grantedClass(cgOwnerNS, "shared", sel),
		grantedImage(cgOwnerNS, "shared", sel),
	} {
		err := checkConsumer(context.Background(), c, obj, cgConsumerNS)
		require.Error(t, err)
		require.True(t, isConsumerNotAllowed(err))
		var cna *ConsumerNotAllowedError
		require.ErrorAs(t, err, &cna)
		kind, _, _ := consumerSelectorOf(obj)
		assert.Equal(t, kind, cna.Kind)
		assert.Contains(t, err.Error(), kind+" "+cgOwnerNS+"/shared")
		assert.Contains(t, err.Error(), consumerNamespaceSelectorField)
		assert.NotContains(t, err.Error(), "secret-tenant-label", "the selector is never revealed")
		assert.NotContains(t, err.Error(), cgConsumerNS, "the message names only the referenced object and the field")
	}
	require.NoError(t, checkConsumer(context.Background(), c, nil, cgConsumerNS), "nil object is not checked")
}

func TestGetForConsumer(t *testing.T) {
	ctx := context.Background()
	c := cgClient(t, labeledNamespace(cgConsumerNS, nil),
		grantedProvider(cgOwnerNS, "shared", sharedWithNamespace(cgConsumerNS)),
		grantedProvider(cgOwnerNS, "private", nil),
	)

	p := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, getForConsumer(ctx, c, types.NamespacedName{Namespace: cgOwnerNS, Name: "shared"}, p, cgConsumerNS))
	assert.Equal(t, "shared", p.Name, "the object is read into obj")

	err := getForConsumer(ctx, c, types.NamespacedName{Namespace: cgOwnerNS, Name: "private"}, &infravirtrigaudiov1beta1.Provider{}, cgConsumerNS)
	require.True(t, isConsumerNotAllowed(err))

	missing := getForConsumer(ctx, c, types.NamespacedName{Namespace: cgOwnerNS, Name: "nope"}, &infravirtrigaudiov1beta1.Provider{}, cgConsumerNS)
	require.True(t, isConsumerNotAllowed(missing), "a missing cross-namespace object is refused like an ungranted one")
	assert.Equal(t, err.Error(), (&ConsumerNotAllowedError{Kind: consumerKindProvider, Namespace: cgOwnerNS, Name: "private"}).Error())
	assert.Equal(t, missing.Error(), (&ConsumerNotAllowedError{Kind: consumerKindProvider, Namespace: cgOwnerNS, Name: "nope"}).Error(),
		"no existence oracle: same wording for a missing object")

	own := getForConsumer(ctx, c, types.NamespacedName{Namespace: cgConsumerNS, Name: "nope"}, &infravirtrigaudiov1beta1.Provider{}, cgConsumerNS)
	require.Error(t, own)
	assert.False(t, isConsumerNotAllowed(own))
	assert.True(t, apierrors.IsNotFound(own), "a missing object in the own namespace keeps its NotFound")

	require.Error(t, getForConsumer(ctx, c, types.NamespacedName{Namespace: cgOwnerNS, Name: "x"}, &corev1.ConfigMap{}, cgConsumerNS),
		"only Provider, VMClass and VMImage are resolvable")
}

func TestCheckVMConsumerRefs(t *testing.T) {
	ctx := context.Background()
	vmWith := func(provNS, classNS, imageNS string) *infravirtrigaudiov1beta1.VirtualMachine {
		vm := baseVM(cgConsumerNS)
		vm.Spec.ProviderRef = infravirtrigaudiov1beta1.ObjectRef{Name: "p", Namespace: provNS}
		vm.Spec.ClassRef = infravirtrigaudiov1beta1.ObjectRef{Name: "c", Namespace: classNS}
		vm.Spec.ImageRef = &infravirtrigaudiov1beta1.ObjectRef{Name: "i", Namespace: imageNS}
		return vm
	}
	objs := func(pSel, cSel, iSel *metav1.LabelSelector) []client.Object {
		return []client.Object{
			labeledNamespace(cgConsumerNS, nil),
			grantedProvider(cgOwnerNS, "p", pSel), grantedClass(cgOwnerNS, "c", cSel), grantedImage(cgOwnerNS, "i", iSel),
		}
	}
	all := sharedWithAll()

	require.NoError(t, checkVMConsumerRefs(ctx, cgClient(t), vmWith("", "", "")),
		"same-namespace references are allowed and not even read")
	require.NoError(t, checkVMConsumerRefs(ctx, cgClient(t, objs(all, all, all)...), vmWith(cgOwnerNS, cgOwnerNS, cgOwnerNS)))

	for name, tc := range map[string]struct {
		objs []client.Object
		kind string
	}{
		"provider": {objs(nil, all, all), consumerKindProvider},
		"class":    {objs(all, nil, all), consumerKindVMClass},
		"image":    {objs(all, all, nil), consumerKindVMImage},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkVMConsumerRefs(ctx, cgClient(t, tc.objs...), vmWith(cgOwnerNS, cgOwnerNS, cgOwnerNS))
			var cna *ConsumerNotAllowedError
			require.ErrorAs(t, err, &cna)
			assert.Equal(t, tc.kind, cna.Kind)
		})
	}

	vm := vmWith(cgOwnerNS, "", "")
	vm.Spec.ClassRef = infravirtrigaudiov1beta1.ObjectRef{}
	vm.Spec.ImageRef = nil
	require.NoError(t, checkVMConsumerRefs(ctx, cgClient(t, objs(all, nil, nil)...), vm), "unset class/image references are skipped")
}
