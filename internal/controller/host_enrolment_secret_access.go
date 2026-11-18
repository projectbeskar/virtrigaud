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

// ErrHostEnrolmentRulesIncomplete is returned by
// CheckHostEnrolmentSecretAccess when the API server cannot list the
// manager's rules completely (an authorizer that cannot enumerate rules, such
// as a webhook, or a rule that could not be resolved). A name-limited grant
// could then go unseen, so enrolment refuses.
var ErrHostEnrolmentRulesIncomplete = errors.New("the manager's RBAC rules could not be listed completely")

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
	execSubresource             = "exec"
	attachSubresource           = "attach"
	ephemeralSubresource        = "ephemeralcontainers"
	authenticationAPIGroup      = "authentication.k8s.io"
	uidsResource                = "uids"
	userExtrasResource          = "userextras"
	usersResource               = "users"
	groupsResource              = "groups"
	verbCreate                  = "create"
	verbGet                     = "get"
	verbUpdate                  = "update"
	verbPatch                   = "patch"
	verbImpersonate             = "impersonate"
	verbBind                    = "bind"
	verbEscalate                = "escalate"
	ruleWildcard                = "*"
)

// secretDataVerbs are the Secret verbs reviewed. get, list, watch, update,
// patch and deletecollection return Secret data. A plain delete returns none,
// but deleting the sub-CA key is destructive, so it is reviewed too.
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

// bindingWriteVerbs are the verbs that change who a role binding applies to.
var bindingWriteVerbs = []string{verbCreate, verbUpdate, verbPatch}

// podExecVerbs are the verbs of pods/exec and pods/attach: create, and get,
// which authorizes a WebSocket connection on API servers before Kubernetes
// 1.31.
var podExecVerbs = []string{verbGet, verbCreate}

// ephemeralContainerVerbs are the verbs that add an ephemeral container to a
// running pod.
var ephemeralContainerVerbs = []string{verbUpdate, verbPatch}

// hostEnrolmentSecretReviews lists the accesses that refuse enrolment. The
// rules review (forbiddenRulesFor) also catches the name-limited rules these
// reviews cannot see:
//
//   - every reviewed Secret verb (secretDataVerbs), in all namespaces;
//   - with a declared sub-CA key Secret: those verbs on that Secret (get,
//     list, watch, update, patch, delete) and on every Secret of its
//     namespace (list, watch, update, patch, delete, deletecollection), and
//     exec, attach and ephemeral containers on pods in its namespace;
//   - escalation primitives that would let the manager grant itself any of
//     the above: impersonate on users and groups, and on serviceaccounts,
//     create on serviceaccounts/token, create, update and patch on
//     rolebindings, and bind and escalate on roles and clusterroles (each in
//     all namespaces and in the key namespace); create, update and patch on
//     clusterrolebindings.
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
		for _, sub := range []string{execSubresource, attachSubresource} {
			for _, verb := range podExecVerbs {
				reviews = append(reviews, newAccessReview("", podsResource, sub, verb, key.Namespace, ""))
			}
		}
		for _, verb := range ephemeralContainerVerbs {
			reviews = append(reviews, newAccessReview("", podsResource, ephemeralSubresource, verb, key.Namespace, ""))
		}
	}
	for _, resource := range []string{usersResource, groupsResource} {
		reviews = append(reviews, newAccessReview("", resource, "", verbImpersonate, "", ""))
	}
	for _, ns := range scopes {
		reviews = append(reviews,
			newAccessReview("", serviceAccountsResource, "", verbImpersonate, ns, ""),
			newAccessReview("", serviceAccountsResource, tokenSubresource, verbCreate, ns, ""))
		for _, verb := range bindingWriteVerbs {
			reviews = append(reviews, newAccessReview(rbacAPIGroup, roleBindingsResource, "", verb, ns, ""))
		}
	}
	for _, verb := range bindingWriteVerbs {
		reviews = append(reviews, newAccessReview(rbacAPIGroup, clusterRoleBindingsResource, "", verb, "", ""))
	}
	for _, ns := range scopes {
		for _, resource := range []string{rolesResource, clusterRolesResource} {
			for _, verb := range []string{verbBind, verbEscalate} {
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

// forbiddenRule is one access no rule of the manager may cover, whatever the
// rule's resourceNames.
type forbiddenRule struct {
	// group is the API group.
	group string
	// resource is the resource.
	resource string
	// subresource is the subresource, if any.
	subresource string
	// anySubresource matches the resource and each of its subresources
	// (userextras/<key>).
	anySubresource bool
	// verbs are the forbidden verbs.
	verbs []string
}

// covers reports whether a rule's resource entry covers the access, with
// the wildcards RBAC accepts: "*" and "*/<subresource>".
func (f forbiddenRule) covers(ruleResource string) bool {
	if ruleResource == ruleWildcard {
		return true
	}
	if f.anySubresource {
		return ruleResource == f.resource || strings.HasPrefix(ruleResource, f.resource+"/") || strings.HasPrefix(ruleResource, "*/")
	}
	if f.subresource == "" {
		return ruleResource == f.resource
	}
	return ruleResource == f.resource+"/"+f.subresource || ruleResource == "*/"+f.subresource
}

// name names the access in a refusal.
func (f forbiddenRule) name() string {
	what := f.resource
	if f.group != "" {
		what += "." + f.group
	}
	if f.subresource != "" {
		what += "/" + f.subresource
	}
	return what
}

// forbiddenRulesFor lists the accesses the rules review refuses. A review
// with an empty name does not match a rule limited by resourceNames, so
// these are looked for in the rules themselves. The Secret and pod entries
// apply only with a declared key Secret: without one the rules are those of
// the enrolment namespace, where the manager holds Secret access on purpose.
func forbiddenRulesFor(cfg *HostEnrolmentConfig) []forbiddenRule {
	forbidden := []forbiddenRule{
		{resource: usersResource, verbs: []string{verbImpersonate}},
		{resource: groupsResource, verbs: []string{verbImpersonate}},
		{resource: serviceAccountsResource, verbs: []string{verbImpersonate}},
		{group: authenticationAPIGroup, resource: uidsResource, verbs: []string{verbImpersonate}},
		{group: authenticationAPIGroup, resource: userExtrasResource, anySubresource: true, verbs: []string{verbImpersonate}},
		{resource: serviceAccountsResource, subresource: tokenSubresource, verbs: []string{verbCreate}},
		{group: rbacAPIGroup, resource: roleBindingsResource, verbs: bindingWriteVerbs},
		{group: rbacAPIGroup, resource: clusterRoleBindingsResource, verbs: bindingWriteVerbs},
		{group: rbacAPIGroup, resource: rolesResource, verbs: []string{verbBind, verbEscalate}},
		{group: rbacAPIGroup, resource: clusterRolesResource, verbs: []string{verbBind, verbEscalate}},
	}
	if cfg.CAKeySecret != nil {
		forbidden = append(forbidden,
			forbiddenRule{resource: secretsResource, verbs: append([]string{verbCreate}, secretDataVerbs...)},
			forbiddenRule{resource: podsResource, subresource: execSubresource, verbs: podExecVerbs},
			forbiddenRule{resource: podsResource, subresource: attachSubresource, verbs: podExecVerbs},
			forbiddenRule{resource: podsResource, subresource: ephemeralSubresource, verbs: ephemeralContainerVerbs})
	}
	return forbidden
}

// forbiddenInRules returns a description of each forbidden access the rules
// grant, once each.
func forbiddenInRules(rules []authorizationv1.ResourceRule, forbidden []forbiddenRule) []string {
	var found []string
	for _, rule := range rules {
		for _, f := range forbidden {
			if !slices.Contains(rule.APIGroups, f.group) && !slices.Contains(rule.APIGroups, ruleWildcard) {
				continue
			}
			if !slices.ContainsFunc(rule.Resources, f.covers) {
				continue
			}
			for _, verb := range f.verbs {
				if !slices.Contains(rule.Verbs, verb) && !slices.Contains(rule.Verbs, ruleWildcard) {
					continue
				}
				desc := verb + " " + f.name() + " through an RBAC rule"
				if len(rule.ResourceNames) > 0 {
					desc += " limited to " + strings.Join(rule.ResourceNames, " ")
				}
				if !slices.Contains(found, desc) {
					found = append(found, desc)
				}
			}
		}
	}
	return found
}

// reviewManagerRules lists the manager's rules with a
// SelfSubjectRulesReview, in the sub-CA key Secret's namespace or, with no
// key Secret declared, in the enrolment namespace (cluster-wide rules appear
// in either), and returns the forbidden accesses they grant. Rules that
// cannot be listed completely are an error (ErrHostEnrolmentRulesIncomplete).
func reviewManagerRules(ctx context.Context, c client.Client, cfg *HostEnrolmentConfig) ([]string, error) {
	namespace := cfg.Namespace
	if cfg.CAKeySecret != nil {
		namespace = cfg.CAKeySecret.Namespace
	}
	review := &authorizationv1.SelfSubjectRulesReview{Spec: authorizationv1.SelfSubjectRulesReviewSpec{Namespace: namespace}}
	if err := c.Create(ctx, review); err != nil {
		return nil, fmt.Errorf("list the manager's RBAC rules in namespace %s: %w", namespace, err)
	}
	if review.Status.Incomplete || review.Status.EvaluationError != "" {
		return nil, fmt.Errorf("%w in namespace %s (incomplete: %t, evaluation error: %q); per-host agent enrolment refuses to run "+
			"without them, because a grant limited to named objects could go unseen",
			ErrHostEnrolmentRulesIncomplete, namespace, review.Status.Incomplete, review.Status.EvaluationError)
	}
	return forbiddenInRules(review.Status.ResourceRules, forbiddenRulesFor(cfg)), nil
}

// CheckHostEnrolmentSecretAccess refuses per-host agent enrolment while the
// manager may read the host-agent subordinate CA's key, or grant itself that
// access, as far as its RBAC shows. It asks the API server, with
// SelfSubjectAccessReviews, for each access hostEnrolmentSecretReviews lists,
// and looks through the manager's rules (a SelfSubjectRulesReview) for the
// grants limited by resourceNames that those reviews cannot match. Any
// allowed access refuses (ErrHostEnrolmentSecretAccess). It fails closed: a
// review that cannot be created, a review the authorizer could not evaluate
// fully (status.evaluationError), and rules that cannot be listed completely
// (ErrHostEnrolmentRulesIncomplete) refuse as well.
//
// It does not see admission policies or the workload path (a pod that mounts
// the Secret); HostEnrolmentWorkloadAccess reports the latter.
//
// The reviews need create on selfsubjectaccessreviews and
// selfsubjectrulesreviews, which Kubernetes grants every authenticated user
// through the default system:basic-user ClusterRole; no rule is added for
// them. Nothing here reads a Secret, and a refusal names only verbs,
// resources, namespaces and object names.
func CheckHostEnrolmentSecretAccess(ctx context.Context, c client.Client, cfg *HostEnrolmentConfig) error {
	if cfg == nil {
		return nil
	}
	allowed, err := runAccessReviews(ctx, c, hostEnrolmentSecretReviews(cfg))
	if err != nil {
		return err
	}
	inRules, err := reviewManagerRules(ctx, c, cfg)
	if err != nil && len(allowed) == 0 {
		return err
	}
	// With an allowed access already found, the refusal names it even when
	// the rules could not be listed.
	allowed = append(allowed, inRules...)
	if len(allowed) > 0 {
		return fmt.Errorf("%w: it may %s; per-host agent enrolment needs the namespaced Secret grant "+
			"(Helm rbac.secrets.scope=namespaced, see docs/manager-secret-access.md), no Secret access in the sub-CA key's namespace "+
			"and no grant that lets the manager impersonate, mint tokens, change role bindings or enter pods in that namespace",
			ErrHostEnrolmentSecretAccess, strings.Join(allowed, ", "))
	}
	return nil
}

// HostEnrolmentWorkloadAccess reports whether the manager may create or
// change Deployments, or create Pods, in the sub-CA key Secret's namespace:
// such a workload could mount the key Secret, or run under an existing
// ServiceAccount there, with no Secret RBAC involved. It returns the allowed accesses (none when no
// key Secret is declared). The caller warns rather than refuses: an
// admission policy, which a review cannot see, may still forbid them.
func HostEnrolmentWorkloadAccess(ctx context.Context, c client.Client, cfg *HostEnrolmentConfig) ([]string, error) {
	if cfg == nil || cfg.CAKeySecret == nil {
		return nil, nil
	}
	ns := cfg.CAKeySecret.Namespace
	return runAccessReviews(ctx, c, []accessReview{
		newAccessReview(appsAPIGroup, deploymentsResource, "", verbCreate, ns, ""),
		newAccessReview(appsAPIGroup, deploymentsResource, "", verbUpdate, ns, ""),
		newAccessReview(appsAPIGroup, deploymentsResource, "", verbPatch, ns, ""),
		newAccessReview("", podsResource, "", verbCreate, ns, ""),
	})
}
