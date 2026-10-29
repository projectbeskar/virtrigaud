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
	"sync"

	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// placementProviderIndex is the cache field index of every VirtualMachine by
// its placement Provider (placementProviderKey, "namespace/name"). The
// committed-capacity reads — the scheduler's snapshot under the Provider's
// assume lock, the Host gauges and the Host in-use check — list only one
// Provider's VMs through it instead of every VM in the cluster.
const placementProviderIndex = "virtrigaud.io/placement-provider"

// placementProviderIndexValue is the placementProviderIndex extractor: every
// VirtualMachine is indexed, placed or not, because an assumption settles when
// its VM is no longer among the Provider's VMs.
func placementProviderIndexValue(obj client.Object) []string {
	vm, ok := obj.(*infravirtrigaudiov1beta1.VirtualMachine)
	if !ok {
		return nil
	}
	return []string{placementProviderKey(vm).String()}
}

// placementIndexRegistered records the field indexers (one per manager cache)
// placementProviderIndex is registered in. The VirtualMachine and Host
// controllers both use the index and both register it; the cache refuses a
// second registration of the same name.
var placementIndexRegistered sync.Map

// indexPlacementProvider registers placementProviderIndex in mgr's cache once.
func indexPlacementProvider(mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	if _, done := placementIndexRegistered.LoadOrStore(indexer, struct{}{}); done {
		return nil
	}
	if err := indexer.IndexField(context.Background(), &infravirtrigaudiov1beta1.VirtualMachine{},
		placementProviderIndex, placementProviderIndexValue); err != nil {
		placementIndexRegistered.Delete(indexer)
		return fmt.Errorf("index VirtualMachine by %s: %w", placementProviderIndex, err)
	}
	return nil
}

// listProviderVMs lists, through placementProviderIndex, the VirtualMachines
// whose placement Provider is provider. The items are shared with the cache
// (UnsafeDisableDeepCopy): callers must treat them as read-only.
func listProviderVMs(ctx context.Context, reader client.Reader, provider types.NamespacedName) ([]infravirtrigaudiov1beta1.VirtualMachine, error) {
	var vms infravirtrigaudiov1beta1.VirtualMachineList
	if err := reader.List(ctx, &vms,
		client.MatchingFields{placementProviderIndex: provider.String()}, client.UnsafeDisableDeepCopy); err != nil {
		return nil, fmt.Errorf("list VirtualMachines of Provider %s: %w", provider, err)
	}
	return vms.Items, nil
}

// providerVMIndex is implemented by a reader that can hand out a Provider's
// VirtualMachines straight from the informer's index (indexedReader).
type providerVMIndex interface {
	providerVMs(provider types.NamespacedName) ([]*infravirtrigaudiov1beta1.VirtualMachine, error)
}

// providerVMsFrom returns the VirtualMachines whose placement Provider is
// provider, as listProviderVMs does, but as pointers: straight from the
// informer's index when reader is an indexedReader (no list is built, nothing
// is copied), otherwise through listProviderVMs. Either way the objects are
// the cache's own (or a list's): callers must treat them as read-only and not
// keep them beyond the call.
//
// A direct read that finds no VM is confirmed through listProviderVMs: an
// index whose key naming no longer matches controller-runtime's would answer
// "none" for every Provider, and an empty Provider must never be taken as
// holding nothing on that word alone. A failed read is an error, never an
// empty result.
func providerVMsFrom(ctx context.Context, reader client.Reader, provider types.NamespacedName) ([]*infravirtrigaudiov1beta1.VirtualMachine, error) {
	if ix, ok := reader.(providerVMIndex); ok {
		vms, err := ix.providerVMs(provider)
		if err != nil || len(vms) > 0 {
			return vms, err
		}
	}
	vms, err := listProviderVMs(ctx, reader, provider)
	if err != nil {
		return nil, err
	}
	out := make([]*infravirtrigaudiov1beta1.VirtualMachine, len(vms))
	for i := range vms {
		out[i] = &vms[i]
	}
	return out, nil
}

// Controller-runtime's naming of a field index in an informer's indexer and
// of its cluster-wide key (sigs.k8s.io/controller-runtime/pkg/cache/internal:
// FieldIndexName, KeyToNamespacedKey). That package is internal, so the names
// are repeated here; TestIndexedReaderMatchesList checks them against a real
// informer cache.
const (
	crFieldIndexPrefix  = "field:"
	crAllNamespacesKey  = "__all_namespaces"
	placementIndexerErr = "read the placement-provider index of the VirtualMachine informer"
)

// indexedReader is a client.Reader whose placement scans read the
// VirtualMachine informer's indexer directly (providerVMIndex): the same
// ByIndex lookup controller-runtime's cache reader makes for a List with
// MatchingFields and UnsafeDisableDeepCopy, without the list it then builds
// (apimeta.SetList copies every object into the list's Items). Every other
// read goes to Reader.
type indexedReader struct {
	client.Reader
	vms toolscache.Indexer
}

// providerVMs implements providerVMIndex. ByIndex returns the objects the
// index holds at one instant, under the store's lock: one consistent
// snapshot, as for a List.
func (r indexedReader) providerVMs(provider types.NamespacedName) ([]*infravirtrigaudiov1beta1.VirtualMachine, error) {
	items, err := r.vms.ByIndex(crFieldIndexPrefix+placementProviderIndex, crAllNamespacesKey+"/"+provider.String())
	if err != nil {
		return nil, fmt.Errorf("%s for Provider %s: %w", placementIndexerErr, provider, err)
	}
	out := make([]*infravirtrigaudiov1beta1.VirtualMachine, 0, len(items))
	for _, item := range items {
		vm, ok := item.(*infravirtrigaudiov1beta1.VirtualMachine)
		if !ok {
			return nil, fmt.Errorf("%s for Provider %s: unexpected object %T", placementIndexerErr, provider, item)
		}
		out = append(out, vm)
	}
	return out, nil
}

// placementVMIndexer returns the indexer of mgr's VirtualMachine informer, for
// an indexedReader; nil, with nothing logged as an error, when the cache does
// not expose it (placement scans then List through the cache, as before).
func placementVMIndexer(mgr ctrl.Manager) toolscache.Indexer {
	inf, err := mgr.GetCache().GetInformer(context.Background(), &infravirtrigaudiov1beta1.VirtualMachine{})
	if err != nil {
		ctrl.Log.WithName("placement").Info("VirtualMachine informer not available for direct index reads; placement scans list through the cache",
			"error", err.Error())
		return nil
	}
	sii, ok := inf.(toolscache.SharedIndexInformer)
	if !ok {
		ctrl.Log.WithName("placement").Info("VirtualMachine informer does not expose its indexer; placement scans list through the cache",
			"type", fmt.Sprintf("%T", inf))
		return nil
	}
	return sii.GetIndexer()
}

// withPlacementIndex returns reader reading placement scans from vms when vms
// is set, reader itself otherwise.
func withPlacementIndex(reader client.Reader, vms toolscache.Indexer) client.Reader {
	if vms == nil {
		return reader
	}
	return indexedReader{Reader: reader, vms: vms}
}

// placementReader is the reader the VirtualMachine controller's placement
// scans use: the informer's indexer directly when SetupWithManager found it.
func (r *VirtualMachineReconciler) placementReader() client.Reader {
	return withPlacementIndex(r.Client, r.placementIndex)
}

// placementReader is the reader the VMClone controller's placement scan uses.
func (r *VMCloneReconciler) placementReader() client.Reader {
	return withPlacementIndex(r.Client, r.placementIndex)
}
