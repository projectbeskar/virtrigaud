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

// VirtualMachineCRDName is the name of the VirtualMachine CustomResourceDefinition.
const VirtualMachineCRDName = "virtualmachines.infra.virtrigaud.io"

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
)

// ErrVMCRDSecurityFeaturesMissing is returned (wrapped) by the readiness check
// while the installed VirtualMachine CRD lacks the provider-binding features.
var ErrVMCRDSecurityFeaturesMissing = errors.New("the installed VirtualMachine CRD is missing security features this manager requires")

// crdGVK is the CustomResourceDefinition kind, read as unstructured so the
// manager needs no apiextensions client.
var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,resourceNames=virtualmachines.infra.virtrigaud.io,verbs=get

// missingVMCRDFeatures returns the provider-binding features the
// VirtualMachine CRD crd lacks in its v1beta1 schema (nil when it has them all).
func missingVMCRDFeatures(crd *unstructured.Unstructured) ([]string, error) {
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

// VMCRDFeatureChecker verifies that the installed VirtualMachine CRD carries
// the provider-binding features (status.boundProvider and the spec.providerRef
// immutability rule). It is used once at startup and as a readiness check.
//
// States (published on virtrigaud_manager_vm_crd_security_features):
//   - verified: both features are present; ready.
//   - missing: the CRD is older than the manager (or absent). An error is
//     logged, virtrigaud_errors_total{reason="vm-crd-security-features-missing"}
//     counts each check, and readiness FAILS — see ReadyzCheck.
//   - unknown: the CRD cannot be read (RBAC forbids it, as with
//     rbac.scope=namespace, or the first read failed); a warning is logged and
//     readiness is not affected, since nothing was proven missing.
//
// A transient read error keeps the previous state. Results are reused for
// Interval.
type VMCRDFeatureChecker struct {
	// Reader reads the CRD; use an uncached reader (mgr.GetAPIReader()) so the
	// manager needs only get on the one CRD and no informer.
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

	logger := log.FromContext(ctx).WithValues("crd", VirtualMachineCRDName)
	state, missing, err := c.read(ctx)
	if err != nil && state == "" {
		// Transient: keep the previous state, or report unknown on the first read.
		logger.Error(err, "Could not read the VirtualMachine CRD to verify its security features; keeping the previous result")
		if c.state != "" {
			return c.state, c.missing
		}
		state = metrics.CRDFeaturesUnknown
	}

	if state != c.state {
		switch state {
		case metrics.CRDFeaturesVerified:
			logger.Info("The installed VirtualMachine CRD has the provider-binding security features")
		case metrics.CRDFeaturesMissing:
			logger.Error(ErrVMCRDSecurityFeaturesMissing, "Upgrade the CRDs: spec.providerRef of a bound VirtualMachine can still be changed and status.boundProvider is pruned on every write, so the provider-binding check cannot take effect; readiness fails until the CRD is upgraded",
				"missing", missing)
		case metrics.CRDFeaturesUnknown:
			logger.Info("WARNING: cannot verify the VirtualMachine CRD's security features (the CRD cannot be read); make sure the CRDs are upgraded with the manager",
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

// read fetches the CRD once. It returns a state when the answer is definite
// (verified, missing — including a CRD that does not exist — or unknown when
// RBAC forbids the read), or an empty state and the error when it is transient.
func (c *VMCRDFeatureChecker) read(ctx context.Context) (string, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, crdCheckTimeout)
	defer cancel()
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(crdGVK)
	err := c.Reader.Get(ctx, types.NamespacedName{Name: VirtualMachineCRDName}, crd)
	switch {
	case apierrors.IsNotFound(err):
		return metrics.CRDFeaturesMissing, []string{"the CustomResourceDefinition " + VirtualMachineCRDName}, nil
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		return metrics.CRDFeaturesUnknown, nil, fmt.Errorf("read CRD %s: %w", VirtualMachineCRDName, err)
	case err != nil:
		return "", nil, fmt.Errorf("read CRD %s: %w", VirtualMachineCRDName, err)
	}
	missing, err := missingVMCRDFeatures(crd)
	if err != nil {
		return metrics.CRDFeaturesMissing, []string{err.Error()}, nil
	}
	if len(missing) > 0 {
		return metrics.CRDFeaturesMissing, missing, nil
	}
	return metrics.CRDFeaturesVerified, nil, nil
}

// ReadyzCheck is a healthz.Checker for the manager's readiness endpoint. It
// fails while the installed VirtualMachine CRD verifiably lacks the
// provider-binding features.
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
