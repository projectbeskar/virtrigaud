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
	"fmt"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// The Spike 1A test seam (docs/performance/spike-1a-scheduler-baseline.md).
//
// In production the VirtualMachine controller reads committed placements
// through the manager's informer cache: controller-runtime's CacheReader over
// a client-go ThreadSafeStore indexer. A real informer needs an API server, and
// envtest cannot hold 100,000 VirtualMachines in a benchmark loop. So the
// benchmarks read through informerStore below:
//
//   - the store IS client-go's indexer (toolscache.NewIndexer), the same type
//     an informer keeps its objects in, with the namespace index and the
//     placement-provider field index registered exactly as controller-runtime's
//     informer cache registers them (indexByField: a namespaced key and an
//     "__all_namespaces" key per value);
//   - Get and List repeat controller-runtime v0.20.4 CacheReader.Get/List
//     (pkg/cache/internal/cache_reader.go) — GetByKey and DeepCopy for Get;
//     ByIndex, the UnsafeDisableDeepCopy branch and apimeta.SetList for List —
//     because that package is internal and cannot be imported.
//
// What it leaves out: informerCache's per-call informer lookup by GVK (a map
// read) and the watch goroutine that applies updates (the harness applies them
// with Update instead). TestSchedBenchSeamMatchesFakeClient checks that the
// seam returns what controller-runtime's fake client returns.

const (
	// fieldIndexPrefix and allNamespacesKey are controller-runtime's field
	// index naming (cache/internal: FieldIndexName, KeyToNamespacedKey).
	fieldIndexPrefix = "field:"
	allNamespacesKey = "__all_namespaces"
)

// informerStore holds VirtualMachines and VMClasses in client-go indexers.
type informerStore struct {
	vms     toolscache.Indexer
	classes toolscache.Indexer
}

// newInformerStore returns an empty store with the indexes the manager's
// cache registers for VirtualMachines (namespace, placement provider).
func newInformerStore() *informerStore {
	return &informerStore{
		vms: toolscache.NewIndexer(toolscache.MetaNamespaceKeyFunc, toolscache.Indexers{
			toolscache.NamespaceIndex:                 toolscache.MetaNamespaceIndexFunc,
			fieldIndexPrefix + placementProviderIndex: fieldIndexFunc(placementProviderIndexValue),
			fieldIndexPrefix + placementHostIndex:     fieldIndexFunc(placementHostIndexValue),
			fieldIndexPrefix + specProviderIndex:      fieldIndexFunc(specProviderIndexValue),
		}),
		classes: toolscache.NewIndexer(toolscache.MetaNamespaceKeyFunc, toolscache.Indexers{
			toolscache.NamespaceIndex: toolscache.MetaNamespaceIndexFunc,
		}),
	}
}

// fieldIndexFunc wraps extract as controller-runtime's indexByField does.
func fieldIndexFunc(extract client.IndexerFunc) toolscache.IndexFunc {
	return func(raw interface{}) ([]string, error) {
		obj, ok := raw.(client.Object)
		if !ok {
			return nil, fmt.Errorf("object of type %T is not an Object", raw)
		}
		ns := obj.GetNamespace()
		vals := extract(obj)
		out := make([]string, 0, 2*len(vals))
		for _, v := range vals {
			out = append(out, namespacedIndexKey(ns, v))
		}
		if ns != "" {
			for _, v := range vals {
				out = append(out, namespacedIndexKey("", v))
			}
		}
		return out, nil
	}
}

// namespacedIndexKey is controller-runtime's KeyToNamespacedKey.
func namespacedIndexKey(ns, v string) string {
	if ns != "" {
		return ns + "/" + v
	}
	return allNamespacesKey + "/" + v
}

// mustAdd adds obj to the store it belongs in.
func (s *informerStore) mustAdd(obj client.Object) {
	var err error
	switch obj.(type) {
	case *infravirtrigaudiov1beta1.VirtualMachine:
		err = s.vms.Add(obj)
	case *infravirtrigaudiov1beta1.VMClass:
		err = s.classes.Add(obj)
	default:
		err = fmt.Errorf("informerStore: unsupported type %T", obj)
	}
	if err != nil {
		panic(err)
	}
}

// storeClient is a client.Client whose reads go to an informerStore the way
// the manager's client reads go to its cache. It implements only Get and
// List; any other call reaches the nil embedded Client and panics, which
// proves the benchmarked path makes no API call.
type storeClient struct {
	client.Client
	store *informerStore
}

// Get is CacheReader.Get: GetByKey, NotFound, DeepCopy, reflect assignment.
func (c *storeClient) Get(_ context.Context, key client.ObjectKey, out client.Object, _ ...client.GetOption) error {
	var idx toolscache.Indexer
	var kind string
	switch out.(type) {
	case *infravirtrigaudiov1beta1.VirtualMachine:
		idx, kind = c.store.vms, "VirtualMachine"
	case *infravirtrigaudiov1beta1.VMClass:
		idx, kind = c.store.classes, "VMClass"
	default:
		return fmt.Errorf("storeClient: unsupported type %T", out)
	}
	storeKey := key.Name
	if key.Namespace != "" {
		storeKey = key.Namespace + "/" + key.Name
	}
	obj, exists, err := idx.GetByKey(storeKey)
	if err != nil {
		return err
	}
	if !exists {
		return apierrors.NewNotFound(schema.GroupResource{Group: infravirtrigaudiov1beta1.GroupVersion.Group, Resource: kind}, key.Name)
	}
	ro, ok := obj.(runtime.Object)
	if !ok {
		return fmt.Errorf("cache contained %T, which is not an Object", obj)
	}
	cp := ro.DeepCopyObject()
	outVal := reflect.ValueOf(out)
	cpVal := reflect.ValueOf(cp)
	if !cpVal.Type().AssignableTo(outVal.Type()) {
		return fmt.Errorf("cache had type %s, but %s was asked for", cpVal.Type(), outVal.Type())
	}
	reflect.Indirect(outVal).Set(reflect.Indirect(cpVal))
	return nil
}

// List is CacheReader.List for VirtualMachineList: by the first field
// requirement's index (byIndexes), else by namespace, else everything; deep
// copied unless UnsafeDisableDeepCopy; written with apimeta.SetList.
func (c *storeClient) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*infravirtrigaudiov1beta1.VirtualMachineList); !ok {
		return fmt.Errorf("storeClient: unsupported list type %T", list)
	}
	listOpts := client.ListOptions{}
	listOpts.ApplyOptions(opts)
	var objs []interface{}
	var err error
	switch {
	case listOpts.FieldSelector != nil:
		reqs := listOpts.FieldSelector.Requirements()
		if len(reqs) != 1 {
			return fmt.Errorf("storeClient: %d field requirements; the seam supports one", len(reqs))
		}
		objs, err = c.store.vms.ByIndex(fieldIndexPrefix+reqs[0].Field, namespacedIndexKey(listOpts.Namespace, reqs[0].Value))
	case listOpts.Namespace != "":
		objs, err = c.store.vms.ByIndex(toolscache.NamespaceIndex, listOpts.Namespace)
	default:
		objs = c.store.vms.List()
	}
	if err != nil {
		return err
	}
	unsafe := listOpts.UnsafeDisableDeepCopy != nil && *listOpts.UnsafeDisableDeepCopy
	runtimeObjs := make([]runtime.Object, 0, len(objs))
	for _, item := range objs {
		obj, ok := item.(runtime.Object)
		if !ok {
			return fmt.Errorf("cache contained %T, which is not an Object", item)
		}
		if listOpts.LabelSelector != nil {
			m, err := apimeta.Accessor(obj)
			if err != nil {
				return err
			}
			if !listOpts.LabelSelector.Matches(labels.Set(m.GetLabels())) {
				continue
			}
		}
		if !unsafe {
			obj = obj.DeepCopyObject()
		}
		runtimeObjs = append(runtimeObjs, obj)
	}
	return apimeta.SetList(list, runtimeObjs)
}

// providerVMs makes storeClient an indexed reader (providerVMIndex), reading
// the placement-provider index as indexedReader reads a real informer's.
func (c *storeClient) providerVMs(provider types.NamespacedName) ([]*infravirtrigaudiov1beta1.VirtualMachine, error) {
	return indexedReader{vms: c.store.vms}.providerVMs(provider)
}
