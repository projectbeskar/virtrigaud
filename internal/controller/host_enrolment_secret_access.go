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
// per-host agent enrolment: Secrets in every namespace, or the declared
// host-agent subordinate CA key Secret.
var ErrHostEnrolmentSecretAccess = errors.New("the manager may read Secrets that would expose the host-agent subordinate CA key")

// secretsResource is the core Secret resource name.
const secretsResource = "secrets"

// clusterWideSecretReadVerbs are the Secret verbs, in all namespaces, that
// any of which would let the manager read the sub-CA key wherever it lives
// (watch returns whole objects, like list).
var clusterWideSecretReadVerbs = []string{"get", "list", "watch"}

// caKeyNamespaceReadVerbs are the namespace-wide Secret verbs that would
// return the sub-CA key Secret with the others of its namespace.
var caKeyNamespaceReadVerbs = []string{"list", "watch"}

// secretReadReview is one Secret access the manager must not hold.
type secretReadReview struct {
	// attrs are the reviewed resource attributes.
	attrs authorizationv1.ResourceAttributes
	// desc names the access in a refusal ("list Secrets in all namespaces").
	desc string
}

// hostEnrolmentSecretReviews lists the Secret accesses that refuse
// enrolment: get, list and watch on Secrets in all namespaces and, with a
// declared sub-CA key Secret, get on it and list and watch in its namespace.
func hostEnrolmentSecretReviews(cfg *HostEnrolmentConfig) []secretReadReview {
	var reviews []secretReadReview
	for _, verb := range clusterWideSecretReadVerbs {
		reviews = append(reviews, secretReadReview{
			attrs: authorizationv1.ResourceAttributes{Verb: verb, Resource: secretsResource},
			desc:  verb + " Secrets in all namespaces",
		})
	}
	if key := cfg.CAKeySecret; key != nil {
		reviews = append(reviews, secretReadReview{
			attrs: authorizationv1.ResourceAttributes{Namespace: key.Namespace, Verb: "get", Resource: secretsResource, Name: key.Name},
			desc:  "get the sub-CA key Secret " + key.String(),
		})
		for _, verb := range caKeyNamespaceReadVerbs {
			reviews = append(reviews, secretReadReview{
				attrs: authorizationv1.ResourceAttributes{Namespace: key.Namespace, Verb: verb, Resource: secretsResource},
				desc:  verb + " Secrets in the sub-CA key namespace " + key.Namespace,
			})
		}
	}
	return reviews
}

// CheckHostEnrolmentSecretAccess refuses per-host agent enrolment while the
// manager may read the host-agent subordinate CA's key: it asks the API
// server, with SelfSubjectAccessReviews, whether the manager may get, list or
// watch Secrets in all namespaces and, when cfg.CAKeySecret is declared, get
// that Secret or list or watch Secrets in its namespace. Any allowed access
// refuses (ErrHostEnrolmentSecretAccess). It fails closed: a review that
// cannot be created, or that the authorizer could not evaluate fully
// (status.evaluationError), refuses as well.
//
// The reviews need create on selfsubjectaccessreviews, which Kubernetes
// grants every authenticated user through the default system:basic-user
// ClusterRole; no rule is added for it. Nothing here reads a Secret, and a
// refusal names only verbs, namespaces and the declared Secret name.
func CheckHostEnrolmentSecretAccess(ctx context.Context, c client.Client, cfg *HostEnrolmentConfig) error {
	if cfg == nil {
		return nil
	}
	logger := log.FromContext(ctx)
	var allowed []string
	for _, r := range hostEnrolmentSecretReviews(cfg) {
		attrs := r.attrs
		review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attrs}}
		if err := c.Create(ctx, review); err != nil {
			return fmt.Errorf("check whether the manager may %s: %w", r.desc, err)
		}
		switch {
		case review.Status.Allowed:
			allowed = append(allowed, r.desc)
		case review.Status.EvaluationError != "":
			return fmt.Errorf("check whether the manager may %s: the authorizer could not evaluate it: %s",
				r.desc, review.Status.EvaluationError)
		default:
			logger.V(1).Info("Secret access denied, as per-host agent enrolment requires", "access", r.desc)
		}
	}
	if len(allowed) > 0 {
		return fmt.Errorf("%w: it may %s; per-host agent enrolment needs the namespaced Secret grant "+
			"(Helm rbac.secrets.scope=namespaced, see docs/manager-secret-access.md) and no Secret access in the sub-CA key's namespace",
			ErrHostEnrolmentSecretAccess, strings.Join(allowed, ", "))
	}
	return nil
}
