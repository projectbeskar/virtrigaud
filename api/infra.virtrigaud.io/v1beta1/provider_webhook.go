/*
Copyright 2025.

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

package v1beta1

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// topologyClusterAllowedTypes is the allow-list of ProviderType values for which
// spec.topology="cluster" is meaningful and therefore permitted (ADR-0007 D2/D9).
//
// The clustered topology makes VirtRigaud itself the cluster manager, so it is
// only valid for hypervisors that have NO native cluster manager. Today that is
// libvirt/KVM only. vSphere (vCenter) and Proxmox (pve-cluster) already own
// inventory/scheduling/migration, and firecracker/qemu are single-host provider
// types — none may declare topology=cluster.
//
// EXTENSION POINT: allowing another hypervisor type to use topology="cluster"
// is a single edit here (add its ProviderType); nothing else in this file
// hard-codes libvirt.
var topologyClusterAllowedTypes = map[ProviderType]struct{}{
	ProviderTypeLibvirt: {},
}

// +kubebuilder:webhook:path=/validate-infra-virtrigaud-io-v1beta1-provider,mutating=false,failurePolicy=fail,sideEffects=None,groups=infra.virtrigaud.io,resources=providers,verbs=create;update,versions=v1beta1,name=vprovider.kb.io,admissionReviewVersions=v1

// ProviderCustomValidator is the admission-time validating webhook for Provider
// (ADR-0007 D2). It enforces the topology×type rule: spec.topology="cluster" is
// permitted only on the provider types in topologyClusterAllowedTypes. It never
// mutates the object and never emits admission warnings.
//
// It is a stateless helper, not an API object, so it is excluded from DeepCopy
// generation.
// +kubebuilder:object:generate=false
type ProviderCustomValidator struct{}

// Compile-time assertion that ProviderCustomValidator satisfies the
// controller-runtime CustomValidator contract.
var _ webhook.CustomValidator = &ProviderCustomValidator{}

// SetupProviderWebhookWithManager registers the Provider validating webhook with
// the manager's webhook server. Callers gate this on webhook serving being
// configured (certs available); see cmd/manager/main.go.
func SetupProviderWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&Provider{}).
		WithValidator(&ProviderCustomValidator{}).
		Complete()
}

// providerForceDeleteAnnotation is the escape hatch that releases a Provider
// being deleted at once, even while objects still use it (ADR-0010 D5). It is
// the same key the controllers read (virtrigaud.io/force-delete).
const providerForceDeleteAnnotation = "virtrigaud.io/force-delete"

// forceDeleteOnLiveProviderWarning is the admission warning returned when
// providerForceDeleteAnnotation is set on a Provider that is not being deleted.
const forceDeleteOnLiveProviderWarning = providerForceDeleteAnnotation + "=true is set on a Provider that is not being deleted: " +
	"deleting it will release it at once, even while VirtualMachines still use it, leaving their hypervisor VMs unmanaged. " +
	"Remove the annotation unless that is intended."

// forceDeleteWarnings returns forceDeleteOnLiveProviderWarning when provider is
// live (not being deleted) and carries providerForceDeleteAnnotation=true. It
// is a warning, not a denial: setting it ahead of a planned deletion is legal.
func forceDeleteWarnings(provider *Provider) admission.Warnings {
	if provider.Annotations[providerForceDeleteAnnotation] == "true" && provider.DeletionTimestamp.IsZero() {
		return admission.Warnings{forceDeleteOnLiveProviderWarning}
	}
	return nil
}

// ValidateCreate validates a Provider on creation. It enforces the ADR-0007 D2
// topology×type rule, the /tmp size-limit and the disk-size limit annotations,
// and warns when the force-delete annotation is set (forceDeleteWarnings).
func (v *ProviderCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	provider, ok := obj.(*Provider)
	if !ok {
		return nil, fmt.Errorf("expected a Provider object but got %T", obj)
	}
	warnings := forceDeleteWarnings(provider)
	if err := validateTmpSizeLimitAnnotation(nil, provider); err != nil {
		return warnings, err
	}
	if err := validateMaxDiskSizeAnnotation(nil, provider); err != nil {
		return warnings, err
	}
	return warnings, validateProviderTopology(provider)
}

// ValidateUpdate validates a Provider on update. It rejects any change of
// spec.type or spec.topology (both immutable; the CRD enforces the same rules
// with CEL transition rules), and re-checks the D2 topology×type rule against
// the new object. It returns no admission warnings.
func (v *ProviderCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	provider, ok := newObj.(*Provider)
	if !ok {
		return nil, fmt.Errorf("expected a Provider object but got %T", newObj)
	}
	old, ok := oldObj.(*Provider)
	if !ok {
		return nil, fmt.Errorf("expected a Provider object but got %T", oldObj)
	}
	warnings := forceDeleteWarnings(provider)
	if err := validateTypeUnchanged(old, provider); err != nil {
		return warnings, err
	}
	if err := validateTopologyUnchanged(old, provider); err != nil {
		return warnings, err
	}
	if err := validateTmpSizeLimitAnnotation(old, provider); err != nil {
		return warnings, err
	}
	if err := validateMaxDiskSizeAnnotation(old, provider); err != nil {
		return warnings, err
	}
	// An update that leaves the spec as it is (a finalizer, a label, an
	// annotation) cannot make the spec invalid. Admitting it means a Provider
	// that predates the topology rule and breaks it can still take and lose
	// its in-use finalizer, and so can still be deleted (ADR-0010 D7).
	if equality.Semantic.DeepEqual(old.Spec, provider.Spec) {
		return warnings, nil
	}
	return warnings, validateProviderTopology(provider)
}

// validateTypeUnchanged rejects an update that changes spec.type (the CRD
// enforces the same rule with a CEL transition rule). The controllers decide
// type-specific safety rules from the type — a libvirt linked clone into
// another namespace is refused — and a Provider's VMs were created by the
// provider implementation of its type, so a Provider can never be re-typed.
func validateTypeUnchanged(old, updated *Provider) error {
	if old.Spec.Type == updated.Spec.Type {
		return nil
	}
	fieldErr := field.Invalid(
		field.NewPath("spec").Child("type"),
		updated.Spec.Type,
		fmt.Sprintf("spec.type is immutable (was %q); create a new Provider to change it", old.Spec.Type),
	)
	return apierrors.NewInvalid(
		GroupVersion.WithKind("Provider").GroupKind(),
		updated.Name,
		field.ErrorList{fieldErr},
	)
}

// effectiveTopology is the topology a Provider actually has: the empty string
// (an object created before the field existed, or a client that omits it) is
// the default, "single" (ADR-0007 D9).
func effectiveTopology(p *Provider) string {
	if p.Spec.Topology == "" {
		return ProviderTopologySingle
	}
	return p.Spec.Topology
}

// validateTopologyUnchanged rejects an update that changes spec.topology
// (ADR-0007 Addendum A). VMs record their placement under one topology and
// every per-VM call is routed and owner-checked by it: flipping cluster→single
// would send a placed VM's delete down the un-owner-checked single-host path
// (where it could destroy another tenant's same-named VM), and single→cluster
// would strand existing VMs without a host binding. "" and "single" are the
// same topology, so an ordinary update of a Provider created before the field
// existed is never rejected.
func validateTopologyUnchanged(old, updated *Provider) error {
	if effectiveTopology(old) == effectiveTopology(updated) {
		return nil
	}
	fieldErr := field.Invalid(
		field.NewPath("spec").Child("topology"),
		updated.Spec.Topology,
		fmt.Sprintf("spec.topology is immutable (was %q); create a new Provider to change it", effectiveTopology(old)),
	)
	return apierrors.NewInvalid(
		GroupVersion.WithKind("Provider").GroupKind(),
		updated.Name,
		field.ErrorList{fieldErr},
	)
}

// validateTmpSizeLimitAnnotation rejects a ProviderTmpSizeLimitAnnotation
// value that ParseProviderTmpSizeLimit does not accept. On an update (old not
// nil) only a value that is new or changed is checked, so a Provider that
// already carries a bad value can still take and lose its finalizers.
func validateTmpSizeLimitAnnotation(old, updated *Provider) error {
	value, ok := updated.Annotations[ProviderTmpSizeLimitAnnotation]
	if !ok {
		return nil
	}
	if old != nil {
		if prev, had := old.Annotations[ProviderTmpSizeLimitAnnotation]; had && prev == value {
			return nil
		}
	}
	if _, err := ParseProviderTmpSizeLimit(value); err != nil {
		return apierrors.NewInvalid(
			GroupVersion.WithKind("Provider").GroupKind(),
			updated.Name,
			field.ErrorList{field.Invalid(
				field.NewPath("metadata").Child("annotations").Key(ProviderTmpSizeLimitAnnotation), value, err.Error())},
		)
	}
	return nil
}

// validateMaxDiskSizeAnnotation rejects a ProviderMaxDiskSizeAnnotation value
// that ParseMaxDiskSize does not accept. On an update (old not nil) only a
// value that is new or changed is checked, so a Provider that already carries
// a bad value can still take and lose its finalizers. (The controllers fail
// closed on a bad value: nothing is created or grown through the Provider.)
func validateMaxDiskSizeAnnotation(old, updated *Provider) error {
	value, ok := updated.Annotations[ProviderMaxDiskSizeAnnotation]
	if !ok {
		return nil
	}
	if old != nil {
		if prev, had := old.Annotations[ProviderMaxDiskSizeAnnotation]; had && prev == value {
			return nil
		}
	}
	if _, err := ParseMaxDiskSize(value); err != nil {
		return apierrors.NewInvalid(
			GroupVersion.WithKind("Provider").GroupKind(),
			updated.Name,
			field.ErrorList{field.Invalid(
				field.NewPath("metadata").Child("annotations").Key(ProviderMaxDiskSizeAnnotation), value, err.Error())},
		)
	}
	return nil
}

// ValidateDelete is a no-op: deletion is always allowed. It returns no admission
// warnings.
func (v *ProviderCustomValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validateProviderTopology enforces ADR-0007 D2/D9: spec.topology="cluster" is
// only permitted on provider types in topologyClusterAllowedTypes. Any other
// topology value — "single" or the empty string, which the apiserver defaults to
// "single" (D9) — is allowed for every type. The returned error is an
// apierrors.NewInvalid so the apiserver surfaces a clean, field-scoped message
// pointing at spec.topology.
func validateProviderTopology(provider *Provider) error {
	// Only "cluster" is constrained. "single" and "" (defaulted to "single",
	// ADR-0007 D9) are valid for all provider types, so today's providers are
	// byte-for-byte unaffected.
	if provider.Spec.Topology != ProviderTopologyCluster {
		return nil
	}
	if _, ok := topologyClusterAllowedTypes[provider.Spec.Type]; ok {
		return nil
	}

	detail := fmt.Sprintf(
		"topology %q is only supported on provider type(s) %s — hypervisors with no native cluster manager (ADR-0007 D2); type %q has its own cluster manager, so use topology %q",
		ProviderTopologyCluster,
		allowedClusterTypeNames(),
		provider.Spec.Type,
		ProviderTopologySingle,
	)
	fieldErr := field.Invalid(
		field.NewPath("spec").Child("topology"),
		provider.Spec.Topology,
		detail,
	)
	return apierrors.NewInvalid(
		GroupVersion.WithKind("Provider").GroupKind(),
		provider.Name,
		field.ErrorList{fieldErr},
	)
}

// allowedClusterTypeNames renders the topology=cluster allow-list as a stable,
// human-readable, quoted, comma-separated string for error messages (e.g.
// `"libvirt"`). Sorted so the message is deterministic as the set grows.
func allowedClusterTypeNames() string {
	names := make([]string, 0, len(topologyClusterAllowedTypes))
	for t := range topologyClusterAllowedTypes {
		names = append(names, strconv.Quote(string(t)))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
