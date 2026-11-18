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
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ErrHostEnrolmentSecretAccess is returned by CheckHostEnrolmentSecretAccess
// when the manager may read Secrets it must not be able to read while it runs
// per-host agent enrolment (Secrets in every namespace, or the host-agent
// subordinate CA key Secret), or may grant itself such access.
var ErrHostEnrolmentSecretAccess = errors.New("the manager may read, or grant itself access to, Secrets that would expose the host-agent subordinate CA key")

// Resource names and API groups of the enrolment access reviews.
const (
	secretsResource             = "secrets"
	serviceAccountsResource     = "serviceaccounts"
	tokenSubresource            = "token"
	rbacAPIGroup                = "rbac.authorization.k8s.io"
	rolesResource               = "roles"
	clusterRolesResource        = "clusterroles"
	roleBindingsResource        = "rolebindings"
	clusterRoleBindingsResource = "clusterrolebindings"
	appsAPIGroup                = "apps"
	deploymentsResource         = "deployments"
	podsResource                = "pods"
	verbCreate                  = "create"
)

// secretDataVerbs are the Secret verbs whose responses carry Secret data:
// get, list and watch, and update, patch, delete and deletecollection, which
// return the object (or the deleted objects).
var secretDataVerbs = []string{"get", "list", "watch", "update", "patch", "delete", "deletecollection"}

// namedSecretDataVerbs are the verbs reviewed on the named sub-CA key
// Secret. list and watch are included: a resourceNames rule allows a list or
// watch with a metadata.name field selector.
var namedSecretDataVerbs = []string{"get", "list", "watch", "update", "patch", "delete"}

// namespaceSecretDataVerbs are the verbs reviewed on every Secret of the
// sub-CA key namespace (a named review already covers get).
var namespaceSecretDataVerbs = []string{"list", "watch", "update", "patch", "delete", "deletecollection"}

// accessReview is one access the manager must not hold.
type accessReview struct {
	// attrs are the reviewed resource attributes.
	attrs authorizationv1.ResourceAttributes
	// desc names the access in a refusal ("list secrets in all namespaces").
	desc string
}

// newAccessReview builds a review and its description.
func newAccessReview(group, resource, subresource, verb, namespace, name string) accessReview {
	what := resource
	if group != "" {
		what = resource + "." + group
	}
	if subresource != "" {
		what += "/" + subresource
	}
	where := "in all namespaces"
	switch {
	case name != "":
		where = "named " + namespace + "/" + name
	case namespace != "":
		where = "in namespace " + namespace
	}
	return accessReview{
		attrs: authorizationv1.ResourceAttributes{Group: group, Resource: resource, Subresource: subresource, Verb: verb,
			Namespace: namespace, Name: name},
		desc: verb + " " + what + " " + where,
	}
}

// hostEnrolmentSecretReviews lists the accesses that refuse enrolment:
//
//   - every verb that returns Secret data, on Secrets in all namespaces;
//   - with a declared sub-CA key Secret: those verbs on that Secret (get,
//     list, watch, update, patch, delete) and on every Secret of its
//     namespace (list, watch, update, patch, delete, deletecollection);
//   - escalation primitives that would let the manager grant itself any of
//     the above: impersonate on users, groups and serviceaccounts; create on
//     serviceaccounts/token and on rolebindings (in all namespaces and in the
//     key namespace) and on clusterrolebindings; bind and escalate on roles
//     and clusterroles (in all namespaces and in the key namespace).
func hostEnrolmentSecretReviews(cfg *HostEnrolmentConfig) []accessReview {
	var reviews []accessReview
	scopes := []string{""}
	for _, verb := range secretDataVerbs {
		reviews = append(reviews, newAccessReview("", secretsResource, "", verb, "", ""))
	}
	if key := cfg.CAKeySecret; key != nil {
		scopes = append(scopes, key.Namespace)
		for _, verb := range namedSecretDataVerbs {
			reviews = append(reviews, newAccessReview("", secretsResource, "", verb, key.Namespace, key.Name))
		}
		for _, verb := range namespaceSecretDataVerbs {
			reviews = append(reviews, newAccessReview("", secretsResource, "", verb, key.Namespace, ""))
		}
	}
	for _, resource := range []string{"users", "groups", serviceAccountsResource} {
		reviews = append(reviews, newAccessReview("", resource, "", "impersonate", "", ""))
	}
	for _, ns := range scopes {
		reviews = append(reviews,
			newAccessReview("", serviceAccountsResource, tokenSubresource, verbCreate, ns, ""),
			newAccessReview(rbacAPIGroup, roleBindingsResource, "", verbCreate, ns, ""))
	}
	reviews = append(reviews, newAccessReview(rbacAPIGroup, clusterRoleBindingsResource, "", verbCreate, "", ""))
	for _, ns := range scopes {
		for _, resource := range []string{rolesResource, clusterRolesResource} {
			for _, verb := range []string{"bind", "escalate"} {
				reviews = append(reviews, newAccessReview(rbacAPIGroup, resource, "", verb, ns, ""))
			}
		}
	}
	return reviews
}

// runAccessReviews asks each review as a SelfSubjectAccessReview and returns
// the descriptions of the allowed ones. A review that cannot be created, or
// that the authorizer could not fully evaluate, is an error.
func runAccessReviews(ctx context.Context, c client.Client, reviews []accessReview) ([]string, error) {
	logger := log.FromContext(ctx)
	var allowed []string
	for _, r := range reviews {
		attrs := r.attrs
		review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attrs}}
		if err := c.Create(ctx, review); err != nil {
			return nil, fmt.Errorf("check whether the manager may %s: %w", r.desc, err)
		}
		switch {
		case review.Status.Allowed:
			allowed = append(allowed, r.desc)
		case review.Status.EvaluationError != "":
			return nil, fmt.Errorf("check whether the manager may %s: the authorizer could not evaluate it: %s",
				r.desc, review.Status.EvaluationError)
		default:
			logger.V(1).Info("Access denied, as per-host agent enrolment requires", "access", r.desc)
		}
	}
	return allowed, nil
}

// CheckHostEnrolmentSecretAccess refuses per-host agent enrolment while the
// manager may read the host-agent subordinate CA's key, or grant itself that
// access, as far as its RBAC shows: it asks the API server, with
// SelfSubjectAccessReviews, for each access hostEnrolmentSecretReviews lists.
// Any allowed access refuses (ErrHostEnrolmentSecretAccess). It fails closed:
// a review that cannot be created, or that the authorizer could not evaluate
// fully (status.evaluationError), refuses as well.
//
// It does not see admission policies or the workload path (a pod that mounts
// the Secret); HostEnrolmentWorkloadAccess reports the latter.
//
// The reviews need create on selfsubjectaccessreviews, which Kubernetes
// grants every authenticated user through the default system:basic-user
// ClusterRole; no rule is added for it. Nothing here reads a Secret, and a
// refusal names only verbs, resources, namespaces and the declared Secret
// name.
func CheckHostEnrolmentSecretAccess(ctx context.Context, c client.Client, cfg *HostEnrolmentConfig) error {
	if cfg == nil {
		return nil
	}
	allowed, err := runAccessReviews(ctx, c, hostEnrolmentSecretReviews(cfg))
	if err != nil {
		return err
	}
	if len(allowed) > 0 {
		return fmt.Errorf("%w: it may %s; per-host agent enrolment needs the namespaced Secret grant "+
			"(Helm rbac.secrets.scope=namespaced, see docs/manager-secret-access.md), no Secret access in the sub-CA key's namespace "+
			"and no grant that lets the manager impersonate, mint tokens or bind roles",
			ErrHostEnrolmentSecretAccess, strings.Join(allowed, ", "))
	}
	return nil
}

// HostEnrolmentWorkloadAccess reports whether the manager may create
// Deployments or Pods in the sub-CA key Secret's namespace: such a workload
// could mount the key Secret, or run under an existing ServiceAccount there,
// with no Secret RBAC involved. It returns the allowed accesses (none when no
// key Secret is declared). The caller warns rather than refuses: an
// admission policy, which a review cannot see, may still forbid them.
func HostEnrolmentWorkloadAccess(ctx context.Context, c client.Client, cfg *HostEnrolmentConfig) ([]string, error) {
	if cfg == nil || cfg.CAKeySecret == nil {
		return nil, nil
	}
	ns := cfg.CAKeySecret.Namespace
	return runAccessReviews(ctx, c, []accessReview{
		newAccessReview(appsAPIGroup, deploymentsResource, "", verbCreate, ns, ""),
		newAccessReview("", podsResource, "", verbCreate, ns, ""),
	})
}
