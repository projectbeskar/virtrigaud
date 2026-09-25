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
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// The grant watches (withConsumerGrantWatches) re-drive only the objects a
// grant change can affect, found through one cache field index per kind so a
// change never lists every object cluster-wide:
//
//   - VirtualMachine: indexed by every Provider, VMClass and VMImage it
//     references from another namespace, and — when it has such a reference or
//     is refused — by its namespace.
//   - VMClone, VMMigration, VMSnapshot: indexed only while refused
//     (Ready=False/ConsumerNotAllowed), by the object named in the refusal and
//     by the namespaces whose labels decide it (its own and, for a clone or
//     migration, its target namespace). In-flight objects re-check the grants
//     at every step anyway, so only refused ones need re-driving.
//
// A tenant editing the selector of its own VMClass therefore re-drives only
// what references that VMClass; an unrelated change re-drives nothing.

// consumerGrantIndex is the cache field index the grant watches look up.
const consumerGrantIndex = "virtrigaud.io/consumer-grant"

// consumerNotAllowedPhrase separates the referenced object from the rest of a
// ConsumerNotAllowedError message; parseConsumerNotAllowedMessage relies on it.
const consumerNotAllowedPhrase = " may not be used from this namespace: "

// consumerRefIndexValue is the index value for a reference to the kind object
// at key.
func consumerRefIndexValue(kind string, key types.NamespacedName) string {
	return "ref:" + kind + "/" + key.Namespace + "/" + key.Name
}

// consumerNamespaceIndexValue is the index value for objects whose grants
// depend on namespace's labels.
func consumerNamespaceIndexValue(namespace string) string {
	return "ns:" + namespace
}

// changedObjectIndexValue is the index value of a Provider, VMClass or VMImage
// whose selector changed; ok is false for any other object.
func changedObjectIndexValue(obj client.Object) (string, bool) {
	kind, _, ok := consumerSelectorOf(obj)
	if !ok {
		return "", false
	}
	return consumerRefIndexValue(kind, client.ObjectKeyFromObject(obj)), true
}

// parseConsumerNotAllowedMessage recovers the referenced kind and key from a
// ConsumerNotAllowedError message ("<Kind> <namespace>/<name> may not be used
// from this namespace: ..."); ok is false for any other text.
func parseConsumerNotAllowedMessage(msg string) (kind string, key types.NamespacedName, ok bool) {
	head, _, found := strings.Cut(msg, consumerNotAllowedPhrase)
	if !found {
		return "", types.NamespacedName{}, false
	}
	kind, ref, found := strings.Cut(head, " ")
	if !found {
		return "", types.NamespacedName{}, false
	}
	switch kind {
	case consumerKindProvider, consumerKindVMClass, consumerKindVMImage:
	default:
		return "", types.NamespacedName{}, false
	}
	ns, name, found := strings.Cut(ref, "/")
	if !found || ns == "" || name == "" || strings.ContainsAny(ref, " \t") || strings.Contains(name, "/") {
		return "", types.NamespacedName{}, false
	}
	return kind, types.NamespacedName{Namespace: ns, Name: name}, true
}

// refusalIndexValues is the index of an object refused with
// ConsumerNotAllowed: the object named in its Ready condition, and each of
// consumerNamespaces. It is empty while the object is not refused.
func refusalIndexValues(conditions []metav1.Condition, consumerNamespaces ...string) []string {
	ready := meta.FindStatusCondition(conditions, k8s.ConditionReady)
	if ready == nil || ready.Reason != k8s.ReasonConsumerNotAllowed {
		return nil
	}
	var out []string
	if kind, key, ok := parseConsumerNotAllowedMessage(ready.Message); ok {
		out = append(out, consumerRefIndexValue(kind, key))
	}
	seen := map[string]bool{}
	for _, ns := range consumerNamespaces {
		if ns != "" && !seen[ns] {
			seen[ns] = true
			out = append(out, consumerNamespaceIndexValue(ns))
		}
	}
	return out
}

// vmConsumerGrantIndexValues indexes a VirtualMachine by each Provider,
// VMClass and VMImage it references from another namespace and, when it has
// one or is refused, by its own namespace.
func vmConsumerGrantIndexValues(obj client.Object) []string {
	vm, ok := obj.(*infravirtrigaudiov1beta1.VirtualMachine)
	if !ok {
		return nil
	}
	var out []string
	for _, ref := range vmConsumerRefs(vm) {
		if ref.set && ref.key.Namespace != vm.Namespace {
			out = append(out, consumerRefIndexValue(ref.kind, ref.key))
		}
	}
	if len(out) > 0 || consumerRefused(vm.Status.Conditions) {
		out = append(out, consumerNamespaceIndexValue(vm.Namespace))
	}
	return out
}

// cloneConsumerGrantIndexValues indexes a refused VMClone (see
// refusalIndexValues): its own and its target namespace decide its grants.
func cloneConsumerGrantIndexValues(obj client.Object) []string {
	clone, ok := obj.(*infravirtrigaudiov1beta1.VMClone)
	if !ok {
		return nil
	}
	return refusalIndexValues(clone.Status.Conditions, clone.Namespace, cloneTargetNamespace(clone))
}

// migrationConsumerGrantIndexValues indexes a refused VMMigration (see
// refusalIndexValues): its own and its target namespace decide its grants.
func migrationConsumerGrantIndexValues(obj client.Object) []string {
	migration, ok := obj.(*infravirtrigaudiov1beta1.VMMigration)
	if !ok {
		return nil
	}
	return refusalIndexValues(migration.Status.Conditions, migration.Namespace, migrationTargetVMKey(migration).Namespace)
}

// snapshotConsumerGrantIndexValues indexes a refused VMSnapshot (see
// refusalIndexValues): its own namespace decides its grant.
func snapshotConsumerGrantIndexValues(obj client.Object) []string {
	snapshot, ok := obj.(*infravirtrigaudiov1beta1.VMSnapshot)
	if !ok {
		return nil
	}
	return refusalIndexValues(snapshot.Status.Conditions, snapshot.Namespace)
}

// indexConsumerGrants registers fn as obj's consumerGrantIndex in mgr's cache.
func indexConsumerGrants(mgr ctrl.Manager, obj client.Object, fn client.IndexerFunc) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), obj, consumerGrantIndex, fn); err != nil {
		return fmt.Errorf("index %T by %s: %w", obj, consumerGrantIndex, err)
	}
	return nil
}

// requestsForGrantChange lists, through the consumerGrantIndex, the objects of
// list's kind indexed under indexValue and returns a request for each one skip
// does not reject.
func requestsForGrantChange(
	ctx context.Context,
	c client.Reader,
	list client.ObjectList,
	indexValue string,
	skip func(client.Object) bool,
) []reconcile.Request {
	if err := c.List(ctx, list, client.MatchingFields{consumerGrantIndex: indexValue}); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list objects for a consumer grant change", "index", indexValue)
		return nil
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		log.FromContext(ctx).Error(err, "Failed to read the listed objects for a consumer grant change", "index", indexValue)
		return nil
	}
	var reqs []reconcile.Request
	for _, item := range items {
		obj, ok := item.(client.Object)
		if !ok || (skip != nil && skip(obj)) {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
	}
	return reqs
}
