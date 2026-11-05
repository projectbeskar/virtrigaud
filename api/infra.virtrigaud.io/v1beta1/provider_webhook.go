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
// only valid for hypervisors that have NO native cluster manager: libvirt/KVM,
// and the development-only cloudhypervisor type. vSphere (vCenter) and Proxmox
// (pve-cluster) already own inventory/scheduling/migration, and
// firecracker/qemu are single-host provider types — none may declare
// topology=cluster.
//
// EXTENSION POINT: allowing another hypervisor type to use topology="cluster"
// is a single edit here (add its ProviderType); nothing else in this file
// hard-codes libvirt.
var topologyClusterAllowedTypes = map[ProviderType]struct{}{
	ProviderTypeLibvirt:         {},
	ProviderTypeCloudHypervisor: {},
}

// topologyClusterRequiredTypes are the ProviderType values that are valid ONLY
// with spec.topology="cluster". Cloud Hypervisor has no native cluster manager
// and runs one agent per host, so a single-host Cloud Hypervisor Provider is
// not a supported shape. Every entry must also be in
// topologyClusterAllowedTypes.
var topologyClusterRequiredTypes = map[ProviderType]struct{}{
	ProviderTypeCloudHypervisor: {},
}

// DevProviderTypesFlagName is the name of the manager flag that enables
// development-only provider types (IsDevOnlyProviderType). It is honoured only
// by a manager built with the virtrigaud_dev build tag; a release build
// refuses to start when it is set.
const DevProviderTypesFlagName = "enable-dev-provider-types"

// Admission operations named in the development-gate log line.
const (
	admissionOperationCreate = "create"
	admissionOperationUpdate = "update"
)

// devProviderTypeAdmittedWarning is the admission warning returned on every
// admission of a development-only provider type that the gate enables.
const devProviderTypeAdmittedWarning = "provider type %q is development-only: it is admitted only because this manager " +
	"was built with the development gate and started with --" + DevProviderTypesFlagName + "; it is not available in " +
	"release builds and must not be used for production workloads"

// devProviderTypeMetadataOnlyWarning is the admission warning returned when a
// metadata-only update of a Provider of a development-only type that the gate
// does not enable is admitted (so the Provider can still be deleted).
const devProviderTypeMetadataOnlyWarning = "provider type %q is development-only and is not available in this build: " +
	"only updates that leave spec unchanged (finalizers, labels, annotations) are admitted, so the Provider can be deleted"

// +kubebuilder:webhook:path=/validate-infra-virtrigaud-io-v1beta1-provider,mutating=false,failurePolicy=fail,sideEffects=None,groups=infra.virtrigaud.io,resources=providers,verbs=create;update,versions=v1beta1,name=vprovider.kb.io,admissionReviewVersions=v1

// ProviderCustomValidator is the admission-time validating webhook for Provider
// (ADR-0007 D2). It enforces the topology×type rule: spec.topology="cluster" is
// permitted only on the provider types in topologyClusterAllowedTypes, and
// required on those in topologyClusterRequiredTypes. A development-only
// provider type (IsDevOnlyProviderType) is refused unless
// EnabledDevProviderTypes names it; when it does, every admission of it is
// allowed with a warning and a log line. It never mutates the object.
//
// It is a helper, not an API object, so it is excluded from DeepCopy
// generation.
// +kubebuilder:object:generate=false
type ProviderCustomValidator struct {
	// EnabledDevProviderTypes are the development-only provider types the
	// manager's development gate enables (--enable-dev-provider-types). It is
	// always empty in a release build, which has no gate compiled in. Read
	// only; nil enables none.
	EnabledDevProviderTypes map[ProviderType]struct{}
}

// Compile-time assertion that ProviderCustomValidator satisfies the
// controller-runtime CustomValidator contract.
var _ webhook.CustomValidator = &ProviderCustomValidator{}

// SetupProviderWebhookWithManager registers the Provider validating webhook with
// the manager's webhook server. enabledDevProviderTypes are the
// development-only provider types the manager's development gate enables (nil
// in a release build). Callers gate this on webhook serving being configured
// (certs available); see cmd/manager/main.go.
func SetupProviderWebhookWithManager(mgr ctrl.Manager, enabledDevProviderTypes map[ProviderType]struct{}) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&Provider{}).
		WithValidator(&ProviderCustomValidator{EnabledDevProviderTypes: enabledDevProviderTypes}).
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

// ValidateCreate validates a Provider on creation. It refuses a
// development-only provider type the development gate does not enable, and
// enforces the ADR-0007 D2 topology×type rule, the /tmp size-limit and the
// disk-size limit annotations. It warns when the force-delete annotation is
// set (forceDeleteWarnings) and on every admission of a development-only type.
func (v *ProviderCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	provider, ok := obj.(*Provider)
	if !ok {
		return nil, fmt.Errorf("expected a Provider object but got %T", obj)
	}
	warnings := forceDeleteWarnings(provider)
	devWarnings, err := v.validateDevProviderType(ctx, provider, admissionOperationCreate, true)
	warnings = append(warnings, devWarnings...)
	if err != nil {
		return warnings, err
	}
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
// with CEL transition rules), refuses a spec change of a development-only
// provider type the development gate does not enable, and re-checks the D2
// topology×type rule against the new object. It warns as ValidateCreate does.
func (v *ProviderCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
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
	specChanged := !equality.Semantic.DeepEqual(old.Spec, provider.Spec)
	devWarnings, err := v.validateDevProviderType(ctx, provider, admissionOperationUpdate, specChanged)
	warnings = append(warnings, devWarnings...)
	if err != nil {
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
	if !specChanged {
		return warnings, nil
	}
	return warnings, validateProviderTopology(provider)
}

// validateDevProviderType applies the development gate to provider
// (IsDevOnlyProviderType). A type the gate enables is admitted with
// devProviderTypeAdmittedWarning and a log line, on every admission. A type it
// does not enable is refused on a create or a spec change (specChanged); an
// update that leaves the spec as it is is admitted with
// devProviderTypeMetadataOnlyWarning, so a Provider created while the gate was
// on can still take and lose its finalizers and be deleted. Any other type is
// not affected.
func (v *ProviderCustomValidator) validateDevProviderType(ctx context.Context, provider *Provider,
	operation string, specChanged bool) (admission.Warnings, error) {
	t := provider.Spec.Type
	if !IsDevOnlyProviderType(t) {
		return nil, nil
	}
	if _, enabled := v.EnabledDevProviderTypes[t]; enabled {
		ctrl.LoggerFrom(ctx).Info("WARNING: admitting a development-only provider type; not for production use",
			"provider", provider.Namespace+"/"+provider.Name, "type", string(t), "operation", operation)
		return admission.Warnings{fmt.Sprintf(devProviderTypeAdmittedWarning, t)}, nil
	}
	if !specChanged {
		return admission.Warnings{fmt.Sprintf(devProviderTypeMetadataOnlyWarning, t)}, nil
	}
	fieldErr := field.Invalid(
		field.NewPath("spec").Child("type"),
		t,
		fmt.Sprintf("provider type %q is development-only and is not available in this build: only a development build "+
			"of the manager started with --%s=%s admits it", t, DevProviderTypesFlagName, t),
	)
	return nil, apierrors.NewInvalid(
		GroupVersion.WithKind("Provider").GroupKind(),
		provider.Name,
		field.ErrorList{fieldErr},
	)
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
// only permitted on provider types in topologyClusterAllowedTypes, and the
// types in topologyClusterRequiredTypes permit nothing else. Any other
// topology value — "single" or the empty string, which the apiserver defaults to
// "single" (D9) — is allowed for every other type. The returned error is an
// apierrors.NewInvalid so the apiserver surfaces a clean, field-scoped message
// pointing at spec.topology.
func validateProviderTopology(provider *Provider) error {
	if _, required := topologyClusterRequiredTypes[provider.Spec.Type]; required &&
		effectiveTopology(provider) != ProviderTopologyCluster {
		fieldErr := field.Invalid(
			field.NewPath("spec").Child("topology"),
			provider.Spec.Topology,
			fmt.Sprintf("provider type %q requires topology %q: it has no native cluster manager and runs one agent per host, "+
				"so a single-host Provider of this type is not supported", provider.Spec.Type, ProviderTopologyCluster),
		)
		return apierrors.NewInvalid(
			GroupVersion.WithKind("Provider").GroupKind(),
			provider.Name,
			field.ErrorList{fieldErr},
		)
	}
	// Otherwise only "cluster" is constrained. "single" and "" (defaulted to
	// "single", ADR-0007 D9) are valid for the other provider types, so
	// today's providers are byte-for-byte unaffected.
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
