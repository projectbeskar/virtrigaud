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
	"slices"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The live usage check of a Provider being released (ADR-0010). The in-use
// finalizer decides "nothing uses this Provider" from the informer cache,
// which can lag: a VirtualMachine's create marker, or a clone or migration
// that just recorded its use, may not be in the cache yet. Right before the
// finalizer is removed, the same usage is computed again from the API server
// (liveIndexedReader); anything found there holds the Provider. It runs once
// per Provider release, so whole-kind live Lists (read in pages of
// liveListPageSize) are affordable.

// liveIndexedReader is a client.Reader over an uncached reader (the API
// server) that also answers a List selected by one of the manager's cache
// field indexes (placementProviderIndex, providerInUseIndex): the API server
// cannot select custom resources by those fields, so it lists the kind in
// every namespace and filters the items with the index's own extractor.
// Every other read goes to Reader unchanged.
type liveIndexedReader struct {
	client.Reader
}

// liveIndexExtractors are the extractors of the cache field indexes
// liveIndexedReader can answer, by field.
var liveIndexExtractors = map[string]func(client.Object) []string{
	placementProviderIndex: placementProviderIndexValue,
	providerInUseIndex:     providerInUseIndexValues,
}

// List implements client.Reader.
func (l liveIndexedReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	lo := &client.ListOptions{}
	lo.ApplyOptions(opts)
	if lo.FieldSelector == nil || lo.FieldSelector.Empty() {
		return l.Reader.List(ctx, list, opts...)
	}
	reqs := lo.FieldSelector.Requirements()
	if len(reqs) != 1 || (reqs[0].Operator != selection.Equals && reqs[0].Operator != selection.DoubleEquals) {
		return fmt.Errorf("live list: unsupported field selector %q", lo.FieldSelector.String())
	}
	extract, ok := liveIndexExtractors[reqs[0].Field]
	if !ok {
		return fmt.Errorf("live list: no index extractor for field %q", reqs[0].Field)
	}
	value := reqs[0].Value
	lo.FieldSelector = nil
	lo.UnsafeDisableDeepCopy = nil
	lo.Limit = liveListPageSize
	lo.Continue = ""
	var kept []runtime.Object
	for {
		if err := l.Reader.List(ctx, list, lo); err != nil {
			return err
		}
		items, err := apimeta.ExtractList(list)
		if err != nil {
			return fmt.Errorf("live list: extract items: %w", err)
		}
		for _, item := range items {
			obj, ok := item.(client.Object)
			if ok && slices.Contains(extract(obj), value) {
				// A copy: the next page is decoded into the same list.
				kept = append(kept, item.DeepCopyObject())
			}
		}
		next := list.GetContinue()
		if next == "" {
			break
		}
		lo.Continue = next
	}
	if err := apimeta.SetList(list, kept); err != nil {
		return fmt.Errorf("live list: set items: %w", err)
	}
	list.SetContinue("")
	return nil
}

// liveListPageSize is the page size of liveIndexedReader's live Lists, so a
// cluster with many objects of a kind is read in bounded pages.
const liveListPageSize = 500
