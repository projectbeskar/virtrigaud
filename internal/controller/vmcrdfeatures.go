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
	"net/http"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// The provider binding has two halves (see vmboundprovider.go): the CRD rule
// that makes spec.providerRef immutable once a VM is bound, and the operator's
// status.boundProvider record. Both live in the installed VirtualMachine CRD,
// which is upgraded separately from the manager (the chart's CRD upgrade hook,
// or a GitOps tool). With an older CRD the rule is absent — a bound VM can be
// re-pointed — and the API server prunes status.boundProvider on every write,
// because the field is not in the schema: the record never persists, the
// backfill re-runs on every reconcile from the still-mutable reference, and the
// operator-side check can never fire. VMCRDFeatureChecker detects that state.
//
// The cross-namespace consumer grant (consumergrant.go) likewise needs
// spec.consumerNamespaceSelector in the installed Provider, VMClass and VMImage
// CRDs. With an older CRD the API server rejects or prunes the field, so no
// grant can be set and every cross-namespace reference is refused: safe, but a
// silent outage for shared objects. The checker verifies those CRDs too.
//
// VMImage prepare state is per Provider identity (virtualmachine_image_prepare.go):
// each status.providerStatus entry records the UID of the Provider it was
// recorded through and its own prepare task. With an older VMImage CRD the API
// server prunes both on every write, so no entry is ever trusted (every
// reconcile re-issues the prepare) and no asynchronous prepare task is ever
// polled. The checker verifies the VMImage CRD has both fields.
//
// Prepared-image artifact identity (ADR-0009 D8) adds two more status fields
// the manager relies on: VMImage status.providerStatus[].sourceDigest (the
// source an entry was prepared for; pruned, no entry could ever be matched to
// the current spec.source, so none would be trusted) and Provider
// status.reportedCapabilities.supportsImageArtifactIdentity (pruned, every
// import-capable Provider would look like one without identity support, and
// every import-style prepare would be held). The checker verifies both.

// Names of the CustomResourceDefinitions whose security features the manager
// verifies.
const (
	// VirtualMachineCRDName is the name of the VirtualMachine CRD.
	VirtualMachineCRDName = "virtualmachines.infra.virtrigaud.io"
	// ProviderCRDName is the name of the Provider CRD.
	ProviderCRDName = "providers.infra.virtrigaud.io"
	// VMClassCRDName is the name of the VMClass CRD.
	VMClassCRDName = "vmclasses.infra.virtrigaud.io"
	// VMImageCRDName is the name of the VMImage CRD.
	VMImageCRDName = "vmimages.infra.virtrigaud.io"
)

// providerRefImmutabilityRuleFragment identifies the spec.providerRef
// immutability rule among the CRD's root x-kubernetes-validations (see the
// XValidation marker on VirtualMachine in virtualmachine_types.go).
const providerRefImmutabilityRuleFragment = "self.spec.providerRef == oldSelf.spec.providerRef"

// errReasonCRDFeaturesMissing is the metrics reason recorded on each check that
// finds the installed VirtualMachine CRD without the provider-binding features.
const errReasonCRDFeaturesMissing = "vm-crd-security-features-missing"

// defaultVMCRDFeatureCheckInterval is how long a check result is reused. A
// readiness probe calls the check every few seconds; the CRD rarely changes.
const defaultVMCRDFeatureCheckInterval = time.Minute

// crdCheckTimeout bounds one read of the CRD.
const crdCheckTimeout = 10 * time.Second

// Feature names reported when missing.
const (
	crdFeatureBoundProvider  = "status.boundProvider"
	crdFeatureProviderRefCEL = "the spec.providerRef immutability rule (x-kubernetes-validations)"
	// crdFeatureConsumerSelector is checked on the Provider, VMClass and
	// VMImage CRDs.
	crdFeatureConsumerSelector = consumerNamespaceSelectorField
	// crdFeatureImageProviderUID and crdFeatureImageTaskRef are checked on the
	// VMImage CRD.
	crdFeatureImageProviderUID = "status.providerStatus[].providerUID"
	crdFeatureImageTaskRef     = "status.providerStatus[].taskRef"
	// crdFeatureImageSourceDigest is checked on the VMImage CRD (ADR-0009 D8).
	crdFeatureImageSourceDigest = "status.providerStatus[].sourceDigest"
	// crdFeatureProviderImageArtifactIdentity is checked on the Provider CRD
	// (ADR-0009 D8).
	crdFeatureProviderImageArtifactIdentity = "status.reportedCapabilities.supportsImageArtifactIdentity"
	// crdMissingPrefix prefixes the name of a checked CRD that does not exist.
	crdMissingPrefix = "the CustomResourceDefinition "
)

// ErrVMCRDSecurityFeaturesMissing is returned (wrapped) by the readiness check
// while an installed CRD verifiably lacks a security feature: the
// VirtualMachine provider binding, the consumer grant's selector on the
// Provider, VMClass or VMImage CRD, the per-Provider prepare state
// (status.providerStatus[].providerUID, taskRef and sourceDigest) of the
// VMImage CRD, or the Provider CRD's
// status.reportedCapabilities.supportsImageArtifactIdentity.
var ErrVMCRDSecurityFeaturesMissing = errors.New("the installed CRDs are missing security features this manager requires")

// securityCRDs are the CRDs the checker reads, each with the function that
// lists the features it lacks.
var securityCRDs = []struct {
	name    string
	missing func(*unstructured.Unstructured) ([]string, error)
}{
	{VirtualMachineCRDName, missingVMCRDFeatures},
	{ProviderCRDName, missingProviderCRDFeatures},
	{VMClassCRDName, missingConsumerSelector},
	{VMImageCRDName, missingVMImageCRDFeatures},
}

// crdGVK is the CustomResourceDefinition kind, read as unstructured so the
// manager needs no apiextensions client.
var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,resourceNames=virtualmachines.infra.virtrigaud.io;providers.infra.virtrigaud.io;vmclasses.infra.virtrigaud.io;vmimages.infra.virtrigaud.io,verbs=get

// v1beta1SchemaRoot returns the v1beta1 openAPIV3Schema of crd.
func v1beta1SchemaRoot(crd *unstructured.Unstructured) (map[string]any, error) {
	versions, found, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	if err != nil || !found {
		return nil, fmt.Errorf("read spec.versions of CRD %s: found=%t: %w", crd.GetName(), found, err)
	}
	var schemaRoot map[string]any
	for _, v := range versions {
		version, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := version["name"].(string); name != infravirtrigaudiov1beta1.GroupVersion.Version {
			continue
		}
		root, found, err := unstructured.NestedMap(version, "schema", "openAPIV3Schema")
		if err != nil || !found {
			return nil, fmt.Errorf("read the %s schema of CRD %s: found=%t: %w", infravirtrigaudiov1beta1.GroupVersion.Version, crd.GetName(), found, err)
		}
		schemaRoot = root
	}
	if schemaRoot == nil {
		return nil, fmt.Errorf("CRD %s serves no %s version", crd.GetName(), infravirtrigaudiov1beta1.GroupVersion.Version)
	}
	return schemaRoot, nil
}

// missingConsumerSelector reports spec.consumerNamespaceSelector as missing
// when crd's v1beta1 schema (a Provider, VMClass or VMImage CRD) lacks it.
func missingConsumerSelector(crd *unstructured.Unstructured) ([]string, error) {
	schemaRoot, err := v1beta1SchemaRoot(crd)
	if err != nil {
		return nil, err
	}
	if _, found, _ := unstructured.NestedMap(schemaRoot, "properties", "spec", "properties", "consumerNamespaceSelector"); !found {
		return []string{crdFeatureConsumerSelector}, nil
	}
	return nil, nil
}

// missingProviderCRDFeatures returns the features the Provider CRD crd lacks in
// its v1beta1 schema: spec.consumerNamespaceSelector and
// status.reportedCapabilities.supportsImageArtifactIdentity (nil when it has
// both).
func missingProviderCRDFeatures(crd *unstructured.Unstructured) ([]string, error) {
	missing, err := missingConsumerSelector(crd)
	if err != nil {
		return nil, err
	}
	schemaRoot, err := v1beta1SchemaRoot(crd)
	if err != nil {
		return nil, err
	}
	if _, found, _ := unstructured.NestedMap(schemaRoot, "properties", "status", "properties",
		"reportedCapabilities", "properties", "supportsImageArtifactIdentity"); !found {
		missing = append(missing, crdFeatureProviderImageArtifactIdentity)
	}
	return missing, nil
}

// missingVMImageCRDFeatures returns the features the VMImage CRD crd lacks in
// its v1beta1 schema: spec.consumerNamespaceSelector, and the providerUID,
// taskRef and sourceDigest of a status.providerStatus entry (nil when it has
// them all).
func missingVMImageCRDFeatures(crd *unstructured.Unstructured) ([]string, error) {
	missing, err := missingConsumerSelector(crd)
	if err != nil {
		return nil, err
	}
	schemaRoot, err := v1beta1SchemaRoot(crd)
	if err != nil {
		return nil, err
	}
	entry := []string{"properties", "status", "properties", "providerStatus", "additionalProperties", "properties"}
	for _, f := range []struct{ field, feature string }{
		{"providerUID", crdFeatureImageProviderUID},
		{"taskRef", crdFeatureImageTaskRef},
		{"sourceDigest", crdFeatureImageSourceDigest},
	} {
		if _, found, _ := unstructured.NestedMap(schemaRoot, append(entry, f.field)...); !found {
			missing = append(missing, f.feature)
		}
	}
	return missing, nil
}

// missingVMCRDFeatures returns the provider-binding features the
// VirtualMachine CRD crd lacks in its v1beta1 schema (nil when it has them all).
func missingVMCRDFeatures(crd *unstructured.Unstructured) ([]string, error) {
	schemaRoot, err := v1beta1SchemaRoot(crd)
	if err != nil {
		return nil, err
	}

	var missing []string
	if _, found, _ := unstructured.NestedMap(schemaRoot, "properties", "status", "properties", "boundProvider"); !found {
		missing = append(missing, crdFeatureBoundProvider)
	}
	hasRule := false
	rules, _, _ := unstructured.NestedSlice(schemaRoot, "x-kubernetes-validations")
	for _, r := range rules {
		rule, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if text, _ := rule["rule"].(string); strings.Contains(text, providerRefImmutabilityRuleFragment) {
			hasRule = true
			break
		}
	}
	if !hasRule {
		missing = append(missing, crdFeatureProviderRefCEL)
	}
	return missing, nil
}

// VMCRDFeatureChecker verifies that the installed CRDs carry the security
// features this manager relies on: the VirtualMachine CRD's provider-binding
// features (status.boundProvider and the spec.providerRef immutability rule),
// spec.consumerNamespaceSelector in the Provider, VMClass and VMImage CRDs (the
// cross-namespace consumer grant), the VMImage CRD's per-Provider prepare
// state (status.providerStatus[].providerUID, taskRef and sourceDigest), and
// the Provider CRD's status.reportedCapabilities.supportsImageArtifactIdentity
// (ADR-0009). It is used once at startup and as a readiness check.
//
// States (published on virtrigaud_manager_vm_crd_security_features):
//   - verified: every CRD has its features; ready.
//   - missing: at least one CRD is older than the manager (or absent). An
//     error is logged, virtrigaud_errors_total{reason="vm-crd-security-features-missing"}
//     counts each check, and readiness FAILS — see ReadyzCheck.
//   - unknown: nothing is proven missing but at least one CRD cannot be read
//     (RBAC forbids it, as with rbac.scope=namespace, or the first read
//     failed); a warning is logged and readiness is not affected.
//
// A transient read error keeps the previous state. Results are reused for
// Interval.
type VMCRDFeatureChecker struct {
	// Reader reads the CRDs; use an uncached reader (mgr.GetAPIReader()) so the
	// manager needs only get on those four CRDs and no informer.
	Reader client.Reader
	// Interval is how long a result is reused (default one minute).
	Interval time.Duration

	mu      sync.Mutex
	checked time.Time
	state   string
	missing []string
	now     func() time.Time
}

// NewVMCRDFeatureChecker returns a checker reading through reader.
func NewVMCRDFeatureChecker(reader client.Reader) *VMCRDFeatureChecker {
	return &VMCRDFeatureChecker{Reader: reader, Interval: defaultVMCRDFeatureCheckInterval}
}

// Evaluate returns the current state and, when missing, the missing features,
// re-reading the CRD when the last result is older than Interval.
func (c *VMCRDFeatureChecker) Evaluate(ctx context.Context) (string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	interval := c.Interval
	if interval <= 0 {
		interval = defaultVMCRDFeatureCheckInterval
	}
	if c.state != "" && now().Sub(c.checked) < interval {
		return c.state, c.missing
	}
	c.checked = now()

	logger := log.FromContext(ctx)
	state, missing, err := c.read(ctx)
	if err != nil && state == "" {
		// Transient: keep the previous state, or report unknown on the first read.
		logger.Error(err, "Could not read the CRDs to verify their security features; keeping the previous result")
		if c.state != "" {
			return c.state, c.missing
		}
		state = metrics.CRDFeaturesUnknown
	}

	if state != c.state {
		switch state {
		case metrics.CRDFeaturesVerified:
			logger.Info("The installed CRDs have the security features this manager relies on")
		case metrics.CRDFeaturesMissing:
			logger.Error(ErrVMCRDSecurityFeaturesMissing, "Upgrade the CRDs: without the VirtualMachine provider-binding features a bound VM's spec.providerRef can still be changed and status.boundProvider is pruned; without spec.consumerNamespaceSelector no cross-namespace grant can be set and every cross-namespace reference is refused; without VMImage status.providerStatus[].providerUID and taskRef no prepared image is trusted and no asynchronous prepare is tracked; without VMImage status.providerStatus[].sourceDigest and Provider status.reportedCapabilities.supportsImageArtifactIdentity no prepared image can be matched to its source and import-style prepares are held. Readiness fails until the CRDs are upgraded",
				"missing", missing)
		case metrics.CRDFeaturesUnknown:
			logger.Info("WARNING: cannot verify the CRDs' security features (a CRD cannot be read); make sure the CRDs are upgraded with the manager",
				"error", fmt.Sprint(err))
		}
	}
	if state == metrics.CRDFeaturesMissing {
		metrics.RecordError(errReasonCRDFeaturesMissing, metrics.ComponentManager)
	}
	metrics.SetVMCRDSecurityFeatures(state)
	c.state, c.missing = state, missing
	return state, missing
}

// read fetches every CRD in securityCRDs once. It returns a state when the
// answer is definite — missing if any CRD verifiably lacks a feature
// (including a CRD that does not exist), else unknown if RBAC forbids reading
// any of them, else verified — or an empty state and the error when a read
// failed transiently.
func (c *VMCRDFeatureChecker) read(ctx context.Context) (string, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, crdCheckTimeout)
	defer cancel()
	var missing []string
	var unreadable error
	for _, check := range securityCRDs {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(crdGVK)
		err := c.Reader.Get(ctx, types.NamespacedName{Name: check.name}, crd)
		switch {
		case apierrors.IsNotFound(err):
			missing = append(missing, crdMissingPrefix+check.name)
			continue
		case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
			if unreadable == nil {
				unreadable = fmt.Errorf("read CRD %s: %w", check.name, err)
			}
			continue
		case err != nil:
			return "", nil, fmt.Errorf("read CRD %s: %w", check.name, err)
		}
		lacks, err := check.missing(crd)
		if err != nil {
			missing = append(missing, err.Error())
			continue
		}
		for _, feature := range lacks {
			missing = append(missing, check.name+": "+feature)
		}
	}
	switch {
	case len(missing) > 0:
		return metrics.CRDFeaturesMissing, missing, nil
	case unreadable != nil:
		return metrics.CRDFeaturesUnknown, nil, unreadable
	}
	return metrics.CRDFeaturesVerified, nil, nil
}

// VMImagePrepareStateMissing implements VMImageCRDFeatureReporter: it is true
// while the checker's (cached) result is missing and names the VMImage CRD's
// per-Provider prepare-state fields, or the VMImage CRD itself. An unknown
// state (the CRD cannot be read) is not missing.
func (c *VMCRDFeatureChecker) VMImagePrepareStateMissing(ctx context.Context) bool {
	state, missing := c.Evaluate(ctx)
	if state != metrics.CRDFeaturesMissing {
		return false
	}
	for _, m := range missing {
		switch m {
		case VMImageCRDName + ": " + crdFeatureImageProviderUID,
			VMImageCRDName + ": " + crdFeatureImageTaskRef,
			crdMissingPrefix + VMImageCRDName:
			return true
		}
	}
	return false
}

// ReadyzCheck is a healthz.Checker for the manager's readiness endpoint. It
// fails while an installed CRD verifiably lacks a security feature (the
// VirtualMachine provider binding, spec.consumerNamespaceSelector on the
// Provider, VMClass or VMImage CRD, or a status field listed on
// VMCRDFeatureChecker).
//
// Failing readiness is deliberate: a manager running against an old CRD looks
// healthy while the spec.providerRef protection is silently off. Failing its
// readiness keeps that visible (the pod is not Ready, `helm upgrade --wait`
// fails, and a rolling update keeps the previous manager — which runs against
// the same CRD with no weaker protection — until the CRD is upgraded) without
// stopping VM management: controllers keep running under leader election, as
// before. An unreadable CRD (unknown) does not fail readiness, so installs that
// cannot read CRDs (rbac.scope=namespace) keep working; the gauge and a
// warning report it.
func (c *VMCRDFeatureChecker) ReadyzCheck(req *http.Request) error {
	state, missing := c.Evaluate(req.Context())
	if state == metrics.CRDFeaturesMissing {
		return fmt.Errorf("%w: %s (upgrade the CRDs, e.g. with the chart's crdUpgrade hook)",
			ErrVMCRDSecurityFeaturesMissing, strings.Join(missing, "; "))
	}
	return nil
}
