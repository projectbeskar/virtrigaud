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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// Cross-namespace references to a Provider, VMClass or VMImage
// (spec.consumerNamespaceSelector).
//
// A reference such as VirtualMachine spec.providerRef may name another
// namespace. The manager holds cluster-wide RBAC, so without a check anyone who
// can create a VirtualMachine could drive another namespace's Provider — its
// hypervisor credentials — create VMs from its VMImages (reading their data)
// or size them with its VMClasses. A reference from the referenced object's
// OWN namespace is always allowed. A reference from any other namespace is
// allowed only when the referenced object's spec.consumerNamespaceSelector is
// set and matches the referencing namespace's labels:
//
//   - nil (unset): same namespace only — the default, deny;
//   - {} (empty): every namespace;
//   - otherwise: the namespaces whose labels match (kubernetes.io/metadata.name
//     names one explicitly).
//
// A refused reference is a *ConsumerNotAllowedError: no provider is resolved
// or called through it, and the object that holds it reports Ready=False with
// reason ConsumerNotAllowed. A cross-namespace object that does not exist is
// refused exactly like one that does not select the namespace, so a refusal
// never tells a tenant whether another namespace's object exists.
//
// Whoever can label a Namespace can widen what it may use: Namespaces are
// cluster-scoped, so a tenant who can only write inside its namespace cannot
// grant itself anything, but a self-service platform that lets namespace
// owners label their own Namespace hands them that power (the same caveat as
// the AllowedSourceNamespacesAnnotation target grant).

// Referenced kinds and the field that grants a cross-namespace reference.
const (
	consumerKindProvider = "Provider"
	consumerKindVMClass  = "VMClass"
	consumerKindVMImage  = "VMImage"

	// consumerNamespaceSelectorField is the field of the referenced object
	// that grants other namespaces access; refusals name it.
	consumerNamespaceSelectorField = "spec.consumerNamespaceSelector"
)

// errReasonConsumerNotAllowed is the metrics reason recorded when a
// cross-namespace Provider, VMClass or VMImage reference is refused.
const errReasonConsumerNotAllowed = "consumer-not-allowed"

// consumerNotAllowedRetryInterval re-checks an object refused for an
// ungranted cross-namespace reference. It is a safety net: the controllers
// watch Namespace label changes and the referenced objects' selectors, so a
// grant takes effect within seconds; only whoever can set the selector (or
// label the namespace) can lift the refusal, so a fast requeue would just
// hot-loop.
const consumerNotAllowedRetryInterval = 5 * time.Minute

// errConsumerNotAllowed is the sentinel every ConsumerNotAllowedError matches
// (errors.Is).
var errConsumerNotAllowed = errors.New("cross-namespace reference is not allowed")

// ConsumerNotAllowedError reports that a namespace may not use a Provider,
// VMClass or VMImage in another namespace: the object's
// spec.consumerNamespaceSelector is unset or does not select it (or the object
// does not exist). Its message names only the referenced object and the field
// that would grant access — never the selector or whether the object exists.
type ConsumerNotAllowedError struct {
	// Kind is the referenced kind: Provider, VMClass or VMImage.
	Kind string
	// Namespace and Name identify the referenced object.
	Namespace, Name string
	// ConsumerNamespace is the referencing namespace. It is not part of the
	// message.
	ConsumerNamespace string
}

// Error implements error.
func (e *ConsumerNotAllowedError) Error() string {
	return fmt.Sprintf("%s %s/%s may not be used from this namespace: a reference from another namespace is allowed "+
		"only when the %s's %s selects the referencing namespace; no provider call is made",
		e.Kind, e.Namespace, e.Name, e.Kind, consumerNamespaceSelectorField)
}

// Is makes errors.Is(err, errConsumerNotAllowed) true for every
// ConsumerNotAllowedError.
func (e *ConsumerNotAllowedError) Is(target error) bool { return target == errConsumerNotAllowed }

// isConsumerNotAllowed reports whether err is, or wraps, a
// ConsumerNotAllowedError.
func isConsumerNotAllowed(err error) bool { return errors.Is(err, errConsumerNotAllowed) }

// consumerSelectorOf returns the kind and spec.consumerNamespaceSelector of a
// Provider, VMClass or VMImage. Any other type is a programming error and
// reports ok == false; callers fail closed.
func consumerSelectorOf(obj client.Object) (kind string, selector *metav1.LabelSelector, ok bool) {
	switch o := obj.(type) {
	case *infravirtrigaudiov1beta1.Provider:
		return consumerKindProvider, o.Spec.ConsumerNamespaceSelector, true
	case *infravirtrigaudiov1beta1.VMClass:
		return consumerKindVMClass, o.Spec.ConsumerNamespaceSelector, true
	case *infravirtrigaudiov1beta1.VMImage:
		return consumerKindVMImage, o.Spec.ConsumerNamespaceSelector, true
	}
	return "", nil, false
}

// consumerNotAllowed builds the refusal for obj (kind, key) as seen from
// consumerNamespace.
func consumerNotAllowed(kind string, key types.NamespacedName, consumerNamespace string) *ConsumerNotAllowedError {
	return &ConsumerNotAllowedError{Kind: kind, Namespace: key.Namespace, Name: key.Name, ConsumerNamespace: consumerNamespace}
}

// consumerAllowed reports whether consumerNamespace may use obj, a fetched
// Provider, VMClass or VMImage.
//
// The object's own namespace is always allowed and needs no read. Otherwise a
// nil selector refuses, an empty selector allows without a read, and any other
// selector is matched against the consumer Namespace object's labels, read
// through c. A selector that does not parse, and a consumer Namespace that does
// not exist, refuse. Any other read error is returned: the caller fails closed
// and retries.
func consumerAllowed(ctx context.Context, c client.Reader, obj client.Object, consumerNamespace string) (bool, error) {
	if obj.GetNamespace() == consumerNamespace {
		return true, nil
	}
	kind, sel, ok := consumerSelectorOf(obj)
	if !ok || sel == nil || consumerNamespace == "" {
		return false, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		log.FromContext(ctx).Info("Refusing a cross-namespace reference: the referenced object's consumer namespace selector does not parse",
			"kind", kind, "namespace", obj.GetNamespace(), "name", obj.GetName(), "error", err.Error())
		return false, nil
	}
	if selector.Empty() {
		return true, nil
	}
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKey{Name: consumerNamespace}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get consumer namespace %s: %w", consumerNamespace, err)
	}
	return selector.Matches(labels.Set(ns.Labels)), nil
}

// checkConsumer returns a *ConsumerNotAllowedError when consumerNamespace may
// not use obj (a fetched Provider, VMClass or VMImage), a read error when that
// cannot be decided, and nil when it may. A nil obj is not checked.
func checkConsumer(ctx context.Context, c client.Reader, obj client.Object, consumerNamespace string) error {
	if obj == nil {
		return nil
	}
	allowed, err := consumerAllowed(ctx, c, obj, consumerNamespace)
	if err != nil {
		return err
	}
	if !allowed {
		kind, _, _ := consumerSelectorOf(obj)
		return consumerNotAllowed(kind, client.ObjectKeyFromObject(obj), consumerNamespace)
	}
	return nil
}

// getForConsumer reads the Provider, VMClass or VMImage at key into obj and
// checks that consumerNamespace may use it (checkConsumer). A cross-namespace
// object that does not exist is refused with the same *ConsumerNotAllowedError
// as one that exists but does not select consumerNamespace (no existence
// oracle); in the consumer's own namespace the NotFound error is returned
// wrapped, so callers keep their not-found handling.
func getForConsumer(ctx context.Context, c client.Reader, key types.NamespacedName, obj client.Object, consumerNamespace string) error {
	kind, _, ok := consumerSelectorOf(obj)
	if !ok {
		return fmt.Errorf("get %T %s: not a Provider, VMClass or VMImage", obj, key)
	}
	if err := c.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) && key.Namespace != consumerNamespace {
			return consumerNotAllowed(kind, key, consumerNamespace)
		}
		return fmt.Errorf("get %s %s: %w", kind, key, err)
	}
	return checkConsumer(ctx, c, obj, consumerNamespace)
}

// vmClassKey is the VMClass vm's spec.classRef resolves to (an empty namespace
// means the VM's own), and whether it names one at all.
func vmClassKey(vm *infravirtrigaudiov1beta1.VirtualMachine) (types.NamespacedName, bool) {
	return objectRefKey(vm.Spec.ClassRef, vm.Namespace)
}

// vmImageKey is the VMImage vm's spec.imageRef resolves to, and whether it
// names one at all.
func vmImageKey(vm *infravirtrigaudiov1beta1.VirtualMachine) (types.NamespacedName, bool) {
	if vm.Spec.ImageRef == nil {
		return types.NamespacedName{}, false
	}
	return objectRefKey(*vm.Spec.ImageRef, vm.Namespace)
}

// objectRefKey resolves ref against defaultNamespace; ok is false when ref
// names nothing.
func objectRefKey(ref infravirtrigaudiov1beta1.ObjectRef, defaultNamespace string) (types.NamespacedName, bool) {
	if ref.Name == "" {
		return types.NamespacedName{}, false
	}
	key := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
	if key.Namespace == "" {
		key.Namespace = defaultNamespace
	}
	return key, true
}

// checkVMConsumerRefs checks that vm's namespace may use every Provider,
// VMClass and VMImage vm's spec references, reading each through c; it returns
// the first *ConsumerNotAllowedError, or a read error (fail closed).
// References in vm's own namespace are always allowed and are not read (a
// missing one is the VirtualMachine controller's to report). It is the check
// for a VirtualMachine the caller is about to create or bind but that has not
// been through the VirtualMachine controller's own dependency resolution: a
// VMClone or VMMigration target (whose references are pinned to the clone's
// or migration's namespace), and an adoption bind. Refusing there keeps a
// clone, migration or adoption from producing a VM the VirtualMachine
// controller would refuse to manage.
func checkVMConsumerRefs(ctx context.Context, c client.Reader, vm *infravirtrigaudiov1beta1.VirtualMachine) error {
	classKey, hasClass := vmClassKey(vm)
	imageKey, hasImage := vmImageKey(vm)
	refs := []consumerRef{
		{obj: &infravirtrigaudiov1beta1.Provider{}, key: vmProviderKey(vm), set: vm.Spec.ProviderRef.Name != ""},
		{obj: &infravirtrigaudiov1beta1.VMClass{}, key: classKey, set: hasClass},
		{obj: &infravirtrigaudiov1beta1.VMImage{}, key: imageKey, set: hasImage},
	}
	for _, ref := range refs {
		if !ref.set || ref.key.Namespace == vm.Namespace {
			continue
		}
		if err := getForConsumer(ctx, c, ref.key, ref.obj, vm.Namespace); err != nil {
			return err
		}
	}
	return nil
}

// consumerRef is one Provider / VMClass / VMImage reference of a
// VirtualMachine: an empty object of the referenced kind, the key it resolves
// to, and whether the reference is set at all.
type consumerRef struct {
	obj client.Object
	key types.NamespacedName
	set bool
}

// getVMProvider reads the Provider vm's spec.providerRef names and checks that
// vm's namespace may use it. It is how a controller acting on an existing VM
// (VMSnapshot, VMClone source) resolves the VM's Provider; the caller still
// addresses the VM through vmRefFor, which enforces the bound Provider.
func getVMProvider(ctx context.Context, c client.Reader, vm *infravirtrigaudiov1beta1.VirtualMachine) (*infravirtrigaudiov1beta1.Provider, error) {
	provider := &infravirtrigaudiov1beta1.Provider{}
	if err := getForConsumer(ctx, c, vmProviderKey(vm), provider, vm.Namespace); err != nil {
		return nil, err
	}
	return provider, nil
}
