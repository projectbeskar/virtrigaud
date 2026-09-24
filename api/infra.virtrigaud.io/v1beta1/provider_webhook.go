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
// EXTENSION POINT: when the "cloudhypervisor" ProviderType is added (ADR-0007 P4),
// add ProviderTypeCloudHypervisor here — that is the single edit needed to widen
// the allow-list; nothing else in this file hard-codes libvirt.
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

// ValidateCreate validates a Provider on creation. It enforces the ADR-0007 D2
// topology×type rule and returns no admission warnings.
func (v *ProviderCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	provider, ok := obj.(*Provider)
	if !ok {
		return nil, fmt.Errorf("expected a Provider object but got %T", obj)
	}
	return nil, validateProviderTopology(provider)
}

// ValidateUpdate validates a Provider on update. It rejects any change of
// spec.topology (it is immutable, ADR-0007 Addendum A — the CRD enforces the
// same rule with a CEL transition rule), and re-checks the D2 topology×type
// rule against the new object. It returns no admission warnings.
func (v *ProviderCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	provider, ok := newObj.(*Provider)
	if !ok {
		return nil, fmt.Errorf("expected a Provider object but got %T", newObj)
	}
	old, ok := oldObj.(*Provider)
	if !ok {
		return nil, fmt.Errorf("expected a Provider object but got %T", oldObj)
	}
	if err := validateTopologyUnchanged(old, provider); err != nil {
		return nil, err
	}
	return nil, validateProviderTopology(provider)
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
