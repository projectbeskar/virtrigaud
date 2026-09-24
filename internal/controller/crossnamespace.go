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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// Cross-namespace targets (VMClone / VMMigration spec.target.namespace).
//
// The manager holds cluster-wide RBAC, so a VMClone or VMMigration that names
// another namespace as its target would otherwise let anyone who can create
// one in namespace A make the manager create a VirtualMachine — with labels
// and annotations of their choosing — in namespace B, and (for a migration)
// land a disk named for B's VM on the hypervisor. A different target namespace
// is therefore denied by default and allowed only when the TARGET namespace
// opts in by listing the source namespace in AllowedSourceNamespacesAnnotation.

// Cross-namespace grant constants.
const (
	// AllowedSourceNamespacesAnnotation is the Namespace annotation that lets
	// a VMClone or VMMigration in ANOTHER namespace create a VirtualMachine in
	// the annotated namespace. Its value is a comma-separated list of exact
	// namespace names; spaces around an entry are ignored and there are no
	// wildcards. Whoever can update the target Namespace can grant: Namespaces
	// are cluster-scoped, so a tenant who can only write objects inside its own
	// namespace cannot grant itself access to another one — but self-service
	// platforms may let namespace owners annotate their own Namespace. Grants
	// match namespace NAMES, so a namespace deleted and recreated under the same
	// name keeps every grant that lists it.
	AllowedSourceNamespacesAnnotation = "infra.virtrigaud.io/allowed-source-namespaces"

	// ReasonTargetNamespaceNotAllowed is the condition reason a VMClone or
	// VMMigration records while its spec.target.namespace differs from its own
	// namespace and the target namespace does not list its namespace in
	// AllowedSourceNamespacesAnnotation. Nothing is created, bound, written or
	// deleted in the target namespace while it holds.
	ReasonTargetNamespaceNotAllowed = "TargetNamespaceNotAllowed"

	// crossNamespaceRecheckInterval is how often a refused VMClone or
	// VMMigration re-checks its target namespace's grant. It is a safety net:
	// the Namespace watch re-drives a refused object as soon as the annotation
	// changes, so the interval is deliberately slow — only whoever can update
	// the target Namespace can lift the refusal, and a fast requeue would just
	// hot-loop.
	crossNamespaceRecheckInterval = 5 * time.Minute
)

// targetNamespaceAllowed reports whether an object in sourceNamespace may
// create, bind or write objects in targetNamespace.
//
// The own namespace (or an empty target, which defaults to it) is always
// allowed and needs no API read. Any other namespace is allowed only when its
// Namespace object lists sourceNamespace in AllowedSourceNamespacesAnnotation.
// A target namespace that does not exist is refused exactly like one without
// the grant, so a refusal never tells a tenant whether a namespace exists. Any
// other read error is returned: the caller fails closed and retries.
func targetNamespaceAllowed(ctx context.Context, c client.Reader, sourceNamespace, targetNamespace string) (bool, error) {
	if targetNamespace == "" || targetNamespace == sourceNamespace {
		return true, nil
	}
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKey{Name: targetNamespace}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get target namespace %s: %w", targetNamespace, err)
	}
	return namespaceGrantsSource(ns, sourceNamespace), nil
}

// namespaceGrantsSource reports whether ns lists sourceNamespace in its
// AllowedSourceNamespacesAnnotation. Entries are compared exactly after
// trimming surrounding spaces; empty entries never match and "*" is not a
// wildcard.
func namespaceGrantsSource(ns *corev1.Namespace, sourceNamespace string) bool {
	if ns == nil || sourceNamespace == "" {
		return false
	}
	raw, ok := ns.Annotations[AllowedSourceNamespacesAnnotation]
	if !ok {
		return false
	}
	for _, entry := range strings.Split(raw, ",") {
		if strings.TrimSpace(entry) == sourceNamespace {
			return true
		}
	}
	return false
}

// targetNamespaceNotAllowedMessage is the condition / event message for a
// refused cross-namespace target. It names only the two namespaces and the
// annotation that would grant access.
func targetNamespaceNotAllowedMessage(sourceNamespace, targetNamespace string) string {
	return fmt.Sprintf("namespace %q may not create VirtualMachines in namespace %q: "+
		"someone who can update namespace %q must add %q to its %s annotation",
		sourceNamespace, targetNamespace, targetNamespace, sourceNamespace, AllowedSourceNamespacesAnnotation)
}

// allowedSourceNamespacesChanged is the predicate for the Namespace watch the
// VMClone and VMMigration controllers use to re-drive refused objects: it
// passes a Namespace that is created with the grant annotation, and an update
// that changes the annotation's value (grant, change or revoke). Deleting a
// namespace needs no re-drive: the next check simply finds no grant.
func allowedSourceNamespacesChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			_, ok := e.Object.GetAnnotations()[AllowedSourceNamespacesAnnotation]
			return ok
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldVal, oldOK := e.ObjectOld.GetAnnotations()[AllowedSourceNamespacesAnnotation]
			newVal, newOK := e.ObjectNew.GetAnnotations()[AllowedSourceNamespacesAnnotation]
			return oldOK != newOK || oldVal != newVal
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
