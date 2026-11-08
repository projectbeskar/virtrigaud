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
	"slices"
	"sync/atomic"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
)

// ErrNoCertManagerAdmission is returned by CheckCertManagerAdmission when no
// admission webhook is registered that can stamp and protect the requester
// of an enrolment CertificateRequest.
var ErrNoCertManagerAdmission = errors.New("no fail-closed admission webhook covers CertificateRequest create and update in the enrolment namespace")

// certificateRequestsResource is the CertificateRequest resource name.
const certificateRequestsResource = "certificaterequests"

// CheckCertManagerAdmission fails closed unless the cluster registers, for
// CertificateRequests in the enrolment namespace, a mutating admission
// webhook on CREATE and a validating admission webhook on CREATE and UPDATE,
// each with failurePolicy Fail, no matchConditions (which could skip the
// request; they are not evaluated here) and selectors that match the
// namespace and an enrolment request. The requester fields (spec.username, spec.groups,
// spec.uid, spec.extra) are trustworthy only because cert-manager's webhooks
// overwrite them on create and keep them immutable; without such webhooks,
// whoever creates a request would choose them.
//
// This is a registration check: it cannot prove that the registered webhook
// is cert-manager's or that it is serving (a failurePolicy Fail webhook that
// is down makes creates fail, which is closed).
func CheckCertManagerAdmission(ctx context.Context, c client.Reader, namespace string) error {
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: namespace}, ns); err != nil {
		return fmt.Errorf("get enrolment namespace %s: %w", namespace, err)
	}
	objLabels := labels.Set{certrequest.LabelAgentEnrolment: "true"}

	mutating := &admissionregistrationv1.MutatingWebhookConfigurationList{}
	if err := c.List(ctx, mutating); err != nil {
		return fmt.Errorf("list mutating webhook configurations: %w", err)
	}
	var mutOK bool
	for _, cfg := range mutating.Items {
		for _, w := range cfg.Webhooks {
			if len(w.MatchConditions) == 0 && webhookCovers(w.Rules, w.FailurePolicy, w.NamespaceSelector, w.ObjectSelector, ns.Labels, objLabels,
				admissionregistrationv1.Create) {
				mutOK = true
			}
		}
	}
	validating := &admissionregistrationv1.ValidatingWebhookConfigurationList{}
	if err := c.List(ctx, validating); err != nil {
		return fmt.Errorf("list validating webhook configurations: %w", err)
	}
	var valOK bool
	for _, cfg := range validating.Items {
		for _, w := range cfg.Webhooks {
			if len(w.MatchConditions) == 0 && webhookCovers(w.Rules, w.FailurePolicy, w.NamespaceSelector, w.ObjectSelector, ns.Labels, objLabels,
				admissionregistrationv1.Create, admissionregistrationv1.Update) {
				valOK = true
			}
		}
	}
	switch {
	case !mutOK:
		return fmt.Errorf("%w (mutating, CREATE)", ErrNoCertManagerAdmission)
	case !valOK:
		return fmt.Errorf("%w (validating, CREATE and UPDATE)", ErrNoCertManagerAdmission)
	}
	return nil
}

// webhookCovers reports whether one webhook applies, fail-closed, to
// CertificateRequests with the given labels in a namespace with the given
// labels, for every listed operation.
func webhookCovers(rules []admissionregistrationv1.RuleWithOperations, policy *admissionregistrationv1.FailurePolicyType,
	nsSel, objSel *metav1.LabelSelector, nsLabels, objLabels labels.Set, ops ...admissionregistrationv1.OperationType) bool {
	// An unset failurePolicy defaults to Fail in admissionregistration/v1.
	if policy != nil && *policy != admissionregistrationv1.Fail {
		return false
	}
	if !selectorMatches(nsSel, nsLabels) || !selectorMatches(objSel, objLabels) {
		return false
	}
	for _, op := range ops {
		if !slices.ContainsFunc(rules, func(r admissionregistrationv1.RuleWithOperations) bool { return ruleCovers(r, op) }) {
			return false
		}
	}
	return true
}

// selectorMatches evaluates a label selector (nil or empty: everything).
func selectorMatches(sel *metav1.LabelSelector, set labels.Set) bool {
	if sel == nil {
		return true
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	return err == nil && s.Matches(set)
}

// ruleCovers reports whether a rule covers cert-manager.io/v1
// certificaterequests (namespaced) for the operation.
func ruleCovers(r admissionregistrationv1.RuleWithOperations, op admissionregistrationv1.OperationType) bool {
	anyOf := func(list []string, want string) bool {
		return slices.Contains(list, want) || slices.Contains(list, "*")
	}
	opOK := slices.Contains(r.Operations, op) || slices.Contains(r.Operations, admissionregistrationv1.OperationAll)
	resOK := slices.Contains(r.Resources, certificateRequestsResource) || slices.Contains(r.Resources, "*") || slices.Contains(r.Resources, "*/*")
	scopeOK := r.Scope == nil || *r.Scope == admissionregistrationv1.AllScopes || *r.Scope == admissionregistrationv1.NamespacedScope
	return opOK && resOK && scopeOK && anyOf(r.APIGroups, certrequest.Group) && anyOf(r.APIVersions, certrequest.Version)
}

// DefaultAdmissionRecheckInterval is how often AdmissionGate re-runs the
// admission check.
const DefaultAdmissionRecheckInterval = 3 * time.Minute

// AdmissionGate re-runs CheckCertManagerAdmission periodically; while the
// last check failed, the request reconciler approves nothing. It is a
// manager Runnable: Start runs until its context is cancelled.
type AdmissionGate struct {
	// Reader is a live API reader.
	Reader client.Reader
	// Namespace is the enrolment namespace.
	Namespace string
	// Interval between checks (zero: DefaultAdmissionRecheckInterval).
	Interval time.Duration
	ok       atomic.Bool
}

// Check runs the admission check once and records its outcome.
func (g *AdmissionGate) Check(ctx context.Context) error {
	err := CheckCertManagerAdmission(ctx, g.Reader, g.Namespace)
	was := g.ok.Swap(err == nil)
	if was && err != nil {
		log.FromContext(ctx).Error(err, "Per-host agent enrolment approvals are suspended")
	} else if !was && err == nil {
		log.FromContext(ctx).Info("Per-host agent enrolment approvals are resumed: the admission webhooks cover CertificateRequests again")
	}
	return err
}

// OK reports whether the last check passed.
func (g *AdmissionGate) OK() bool { return g != nil && g.ok.Load() }

// Start implements manager.Runnable: it re-checks every Interval until ctx
// is cancelled.
func (g *AdmissionGate) Start(ctx context.Context) error {
	interval := g.Interval
	if interval <= 0 {
		interval = DefaultAdmissionRecheckInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_ = g.Check(checkCtx) // the outcome is recorded and logged
			cancel()
		}
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: every
// replica keeps its own gate current.
func (g *AdmissionGate) NeedLeaderElection() bool { return false }
