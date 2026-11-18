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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/projectbeskar/virtrigaud/internal/devgate"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

// ssarAnswer answers one SelfSubjectAccessReview in a fake client.
type ssarAnswer func(a authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error)

// ssarKey names a review as "namespace:verb:group/resource[/subresource][:name]".
func ssarKey(a authorizationv1.ResourceAttributes) string {
	k := a.Namespace + ":" + a.Verb + ":" + a.Group + "/" + a.Resource
	if a.Subresource != "" {
		k += "/" + a.Subresource
	}
	if a.Name != "" {
		k += ":" + a.Name
	}
	return k
}

// ssarClient returns a client whose SelfSubjectAccessReview creates are
// answered by answer, and the list of reviews it was asked (ssarKey). Its
// rules review reports no rule.
func ssarClient(t *testing.T, answer ssarAnswer) (client.Client, *[]string) {
	t.Helper()
	return rulesClient(t, answer, authorizationv1.SubjectRulesReviewStatus{})
}

// rulesClient is ssarClient with the status its SelfSubjectRulesReview
// answers.
func rulesClient(t *testing.T, answer ssarAnswer, rules authorizationv1.SubjectRulesReviewStatus) (client.Client, *[]string) {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, authorizationv1.AddToScheme(s))
	var asked []string
	c := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			if rr, ok := obj.(*authorizationv1.SelfSubjectRulesReview); ok {
				rr.Status = rules
				return nil
			}
			r, ok := obj.(*authorizationv1.SelfSubjectAccessReview)
			require.True(t, ok, "only access and rules reviews are created")
			require.NotNil(t, r.Spec.ResourceAttributes)
			a := *r.Spec.ResourceAttributes
			asked = append(asked, ssarKey(a))
			st, err := answer(a)
			r.Status = st
			return err
		},
	}).Build()
	return c, &asked
}

// denyAll denies every review.
func denyAll(authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
	return authorizationv1.SubjectAccessReviewStatus{}, nil
}

// allowOnly allows exactly the review with the given key.
func allowOnly(key string) ssarAnswer {
	return func(a authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
		return authorizationv1.SubjectAccessReviewStatus{Allowed: ssarKey(a) == key}, nil
	}
}

func TestCheckHostEnrolmentSecretAccess(t *testing.T) {
	ctx := context.Background()
	key := &types.NamespacedName{Namespace: "cert-manager", Name: "host-agent-ca"}
	withKey := &HostEnrolmentConfig{Namespace: "host-agent-pki", CAKeySecret: key}
	withoutKey := &HostEnrolmentConfig{Namespace: "host-agent-pki"}

	// The reviews that must be asked: every verb returning Secret data, in
	// all namespaces, on the key and in its namespace, and the escalation
	// primitives in all namespaces and in the key namespace.
	var want []string
	for _, v := range []string{"get", "list", "watch", "update", "patch", "delete", "deletecollection"} {
		want = append(want, ":"+v+":/secrets")
	}
	var wantKey []string
	for _, v := range []string{"get", "list", "watch", "update", "patch", "delete"} {
		wantKey = append(wantKey, "cert-manager:"+v+":/secrets:host-agent-ca")
	}
	for _, v := range []string{"list", "watch", "update", "patch", "delete", "deletecollection"} {
		wantKey = append(wantKey, "cert-manager:"+v+":/secrets")
	}
	wantKey = append(wantKey, "cert-manager:get:/pods/exec", "cert-manager:create:/pods/exec",
		"cert-manager:get:/pods/attach", "cert-manager:create:/pods/attach",
		"cert-manager:update:/pods/ephemeralcontainers", "cert-manager:patch:/pods/ephemeralcontainers")
	escalation := func(scopes ...string) []string {
		out := []string{":impersonate:/users", ":impersonate:/groups", ":create:rbac.authorization.k8s.io/clusterrolebindings",
			":update:rbac.authorization.k8s.io/clusterrolebindings", ":patch:rbac.authorization.k8s.io/clusterrolebindings"}
		for _, ns := range scopes {
			out = append(out, ns+":impersonate:/serviceaccounts", ns+":create:/serviceaccounts/token",
				ns+":create:rbac.authorization.k8s.io/rolebindings", ns+":update:rbac.authorization.k8s.io/rolebindings",
				ns+":patch:rbac.authorization.k8s.io/rolebindings")
			for _, res := range []string{"roles", "clusterroles"} {
				out = append(out, ns+":bind:rbac.authorization.k8s.io/"+res, ns+":escalate:rbac.authorization.k8s.io/"+res)
			}
		}
		return out
	}

	t.Run("enrolment disabled: no review", func(t *testing.T) {
		c, asked := ssarClient(t, denyAll)
		require.NoError(t, CheckHostEnrolmentSecretAccess(ctx, c, nil))
		assert.Empty(t, *asked)
	})

	t.Run("everything denied passes", func(t *testing.T) {
		c, asked := ssarClient(t, denyAll)
		require.NoError(t, CheckHostEnrolmentSecretAccess(ctx, c, withKey))
		assert.ElementsMatch(t, append(append(append([]string{}, want...), wantKey...), escalation("", "cert-manager")...), *asked)

		c, asked = ssarClient(t, denyAll)
		require.NoError(t, CheckHostEnrolmentSecretAccess(ctx, c, withoutKey))
		assert.ElementsMatch(t, append(append([]string{}, want...), escalation("")...), *asked,
			"without a key Secret (external PKI only) the all-namespaces reviews remain")
	})

	t.Run("enrolment-namespace Secret access is not a refusal", func(t *testing.T) {
		// The pins Secrets: the enrolment Role grants the manager full
		// Secret access in the enrolment namespace, which is intended.
		c, _ := ssarClient(t, func(a authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
			return authorizationv1.SubjectAccessReviewStatus{Allowed: a.Namespace == "host-agent-pki"}, nil
		})
		require.NoError(t, CheckHostEnrolmentSecretAccess(ctx, c, withKey))
	})

	t.Run("each allowed access refuses on its own", func(t *testing.T) {
		for _, r := range hostEnrolmentSecretReviews(withKey) {
			k := ssarKey(r.attrs)
			t.Run(k, func(t *testing.T) {
				c, asked := ssarClient(t, allowOnly(k))
				err := CheckHostEnrolmentSecretAccess(ctx, c, withKey)
				require.ErrorIs(t, err, ErrHostEnrolmentSecretAccess)
				assert.Contains(t, err.Error(), "it may "+r.desc+";")
				assert.Contains(t, err.Error(), "rbac.secrets.scope=namespaced", "the refusal says how to fix it")
				assert.Len(t, *asked, len(want)+len(wantKey)+len(escalation("", "cert-manager")),
					"every review is asked, so the refusal lists every allowed access")
			})
		}
	})

	for name, tc := range map[string]struct {
		allow string
		want  string
	}{
		"cluster-wide get":                                 {":get:/secrets", "get secrets in all namespaces"},
		"cluster-wide deletecollection":                    {":deletecollection:/secrets", "deletecollection secrets in all namespaces"},
		"the exact key Secret":                             {"cert-manager:get:/secrets:host-agent-ca", "get secrets named cert-manager/host-agent-ca"},
		"list of the key Secret by name":                   {"cert-manager:list:/secrets:host-agent-ca", "list secrets named cert-manager/host-agent-ca"},
		"watch of the key Secret by name":                  {"cert-manager:watch:/secrets:host-agent-ca", "watch secrets named cert-manager/host-agent-ca"},
		"update of the key Secret":                         {"cert-manager:update:/secrets:host-agent-ca", "update secrets named cert-manager/host-agent-ca"},
		"watch in the key namespace":                       {"cert-manager:watch:/secrets", "watch secrets in namespace cert-manager"},
		"impersonate ServiceAccounts":                      {":impersonate:/serviceaccounts", "impersonate serviceaccounts in all namespaces"},
		"tokens in the key namespace":                      {"cert-manager:create:/serviceaccounts/token", "create serviceaccounts/token in namespace cert-manager"},
		"RoleBindings in the key namespace":                {"cert-manager:create:rbac.authorization.k8s.io/rolebindings", "create rolebindings.rbac.authorization.k8s.io in namespace cert-manager"},
		"impersonate ServiceAccounts in the key namespace": {"cert-manager:impersonate:/serviceaccounts", "impersonate serviceaccounts in namespace cert-manager"},
		"patch RoleBindings in the key namespace":          {"cert-manager:patch:rbac.authorization.k8s.io/rolebindings", "patch rolebindings.rbac.authorization.k8s.io in namespace cert-manager"},
		"update ClusterRoleBindings":                       {":update:rbac.authorization.k8s.io/clusterrolebindings", "update clusterrolebindings.rbac.authorization.k8s.io in all namespaces"},
		"exec in the key namespace":                        {"cert-manager:create:/pods/exec", "create pods/exec in namespace cert-manager"},
		"attach in the key namespace":                      {"cert-manager:create:/pods/attach", "create pods/attach in namespace cert-manager"},
		"ephemeral containers in the key namespace":        {"cert-manager:patch:/pods/ephemeralcontainers", "patch pods/ephemeralcontainers in namespace cert-manager"},
		"ClusterRoleBindings":                              {":create:rbac.authorization.k8s.io/clusterrolebindings", "create clusterrolebindings.rbac.authorization.k8s.io in all namespaces"},
		"escalate ClusterRoles":                            {":escalate:rbac.authorization.k8s.io/clusterroles", "escalate clusterroles.rbac.authorization.k8s.io in all namespaces"},
		"bind ClusterRoles in the key namespace":           {"cert-manager:bind:rbac.authorization.k8s.io/clusterroles", "bind clusterroles.rbac.authorization.k8s.io in namespace cert-manager"},
	} {
		t.Run("refuses: "+name, func(t *testing.T) {
			c, _ := ssarClient(t, allowOnly(tc.allow))
			err := CheckHostEnrolmentSecretAccess(ctx, c, withKey)
			require.ErrorIs(t, err, ErrHostEnrolmentSecretAccess)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	t.Run("refuses: everything (the default cluster grant)", func(t *testing.T) {
		c, _ := ssarClient(t, func(authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
			return authorizationv1.SubjectAccessReviewStatus{Allowed: true}, nil
		})
		err := CheckHostEnrolmentSecretAccess(ctx, c, withKey)
		require.ErrorIs(t, err, ErrHostEnrolmentSecretAccess)
		assert.Contains(t, err.Error(), "get secrets in all namespaces, list secrets in all namespaces, watch secrets in all namespaces")
	})

	t.Run("a review error refuses", func(t *testing.T) {
		boom := errors.New("connection refused")
		for _, r := range hostEnrolmentSecretReviews(withKey) {
			failOn := ssarKey(r.attrs)
			t.Run(failOn, func(t *testing.T) {
				c, _ := ssarClient(t, func(a authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
					if ssarKey(a) == failOn {
						return authorizationv1.SubjectAccessReviewStatus{}, boom
					}
					return authorizationv1.SubjectAccessReviewStatus{}, nil
				})
				err := CheckHostEnrolmentSecretAccess(ctx, c, withKey)
				require.ErrorIs(t, err, boom)
				assert.Contains(t, err.Error(), "check whether the manager may "+r.desc)
			})
		}
	})

	t.Run("an evaluation error refuses", func(t *testing.T) {
		c, _ := ssarClient(t, func(a authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
			if a.Namespace == key.Namespace && a.Name == key.Name {
				return authorizationv1.SubjectAccessReviewStatus{EvaluationError: "webhook authorizer unavailable"}, nil
			}
			return authorizationv1.SubjectAccessReviewStatus{}, nil
		})
		err := CheckHostEnrolmentSecretAccess(ctx, c, withKey)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "could not evaluate it: webhook authorizer unavailable")
		assert.Contains(t, err.Error(), "get secrets named cert-manager/host-agent-ca")
	})
}

// TestCheckHostEnrolmentSecretAccess_Rules: a rule the access reviews cannot
// match (limited by resourceNames) refuses through the rules review, with
// every wildcard RBAC accepts, and rules that cannot be listed completely
// refuse too.
func TestCheckHostEnrolmentSecretAccess_Rules(t *testing.T) {
	ctx := context.Background()
	key := &types.NamespacedName{Namespace: "cert-manager", Name: "host-agent-ca"}
	withKey := &HostEnrolmentConfig{Namespace: "host-agent-pki", CAKeySecret: key}
	noKey := &HostEnrolmentConfig{Namespace: "host-agent-pki"}
	rule := func(verb, group, resource string, names ...string) authorizationv1.ResourceRule {
		return authorizationv1.ResourceRule{Verbs: []string{verb}, APIGroups: []string{group}, Resources: []string{resource}, ResourceNames: names}
	}
	const rbacGroup = "rbac.authorization.k8s.io"

	for name, tc := range map[string]struct {
		rule authorizationv1.ResourceRule
		want string
	}{
		"impersonate one group":    {rule("impersonate", "", "groups", "system:masters"), "impersonate groups through an RBAC rule limited to system:masters"},
		"impersonate one user":     {rule("impersonate", "", "users", "admin"), "impersonate users through an RBAC rule limited to admin"},
		"impersonate one SA":       {rule("impersonate", "", "serviceaccounts", "cert-manager"), "impersonate serviceaccounts through"},
		"impersonate a uid":        {rule("impersonate", "authentication.k8s.io", "uids", "1"), "impersonate uids.authentication.k8s.io through"},
		"impersonate a user extra": {rule("impersonate", "authentication.k8s.io", "userextras/scopes"), "impersonate userextras.authentication.k8s.io through"},
		"token of one SA":          {rule("create", "", "serviceaccounts/token", "cert-manager"), "create serviceaccounts/token through an RBAC rule limited to cert-manager"},
		"token through */token":    {rule("create", "", "*/token"), "create serviceaccounts/token through"},
		"bind cluster-admin":       {rule("bind", rbacGroup, "clusterroles", "cluster-admin"), "bind clusterroles.rbac.authorization.k8s.io through an RBAC rule limited to cluster-admin"},
		"escalate one Role":        {rule("escalate", rbacGroup, "roles", "r"), "escalate roles.rbac.authorization.k8s.io through"},
		"update one RoleBinding":   {rule("update", rbacGroup, "rolebindings", "rb"), "update rolebindings.rbac.authorization.k8s.io through"},
		"patch one ClusterBinding": {rule("patch", rbacGroup, "clusterrolebindings", "crb"), "patch clusterrolebindings.rbac.authorization.k8s.io through"},
		"create a named binding":   {rule("create", rbacGroup, "rolebindings", "rb"), "create rolebindings.rbac.authorization.k8s.io through"},
		"read the key by name":     {rule("get", "", "secrets", "host-agent-ca"), "get secrets through an RBAC rule limited to host-agent-ca"},
		"create Secrets":           {rule("create", "", "secrets"), "create secrets through"},
		"exec in one pod":          {rule("create", "", "pods/exec", "cainjector"), "create pods/exec through an RBAC rule limited to cainjector"},
		"exec over a WebSocket":    {rule("get", "", "pods/exec", "cainjector"), "get pods/exec through"},
		"attach to one pod":        {rule("create", "", "pods/attach", "cainjector"), "create pods/attach through"},
		"ephemeral container":      {rule("patch", "", "pods/ephemeralcontainers", "cainjector"), "patch pods/ephemeralcontainers through"},
		"wildcard verb":            {rule("*", "", "serviceaccounts/token", "x"), "create serviceaccounts/token through"},
		"wildcard group":           {rule("impersonate", "*", "users", "x"), "impersonate users through"},
		"wildcard resource":        {rule("bind", rbacGroup, "*", "x"), "bind roles.rbac.authorization.k8s.io through"},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := rulesClient(t, denyAll, authorizationv1.SubjectRulesReviewStatus{ResourceRules: []authorizationv1.ResourceRule{tc.rule}})
			err := CheckHostEnrolmentSecretAccess(ctx, c, withKey)
			require.ErrorIs(t, err, ErrHostEnrolmentSecretAccess)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	t.Run("rules that grant nothing forbidden pass", func(t *testing.T) {
		c, _ := rulesClient(t, denyAll, authorizationv1.SubjectRulesReviewStatus{ResourceRules: []authorizationv1.ResourceRule{
			rule("get", "", "pods"), rule("create", "apps", "deployments"), rule("create", "", "serviceaccounts"),
			rule("get", rbacGroup, "rolebindings"), rule("impersonate", "example.test", "users"),
			rule("create", "cert-manager.io", "certificaterequests"), rule("approve", "cert-manager.io", "signers", "a/b"),
		}})
		require.NoError(t, CheckHostEnrolmentSecretAccess(ctx, c, withKey))
	})

	t.Run("no key declared: Secret access in the enrolment namespace is intended", func(t *testing.T) {
		secrets := authorizationv1.SubjectRulesReviewStatus{ResourceRules: []authorizationv1.ResourceRule{rule("get", "", "secrets")}}
		c, _ := rulesClient(t, denyAll, secrets)
		require.NoError(t, CheckHostEnrolmentSecretAccess(ctx, c, noKey))

		escalating := authorizationv1.SubjectRulesReviewStatus{ResourceRules: []authorizationv1.ResourceRule{rule("impersonate", "", "groups", "system:masters")}}
		c, _ = rulesClient(t, denyAll, escalating)
		require.ErrorIs(t, CheckHostEnrolmentSecretAccess(ctx, c, noKey), ErrHostEnrolmentSecretAccess)
	})

	t.Run("rules that cannot be listed completely refuse", func(t *testing.T) {
		for name, st := range map[string]authorizationv1.SubjectRulesReviewStatus{
			"incomplete":       {Incomplete: true},
			"evaluation error": {EvaluationError: "clusterrole not found"},
		} {
			t.Run(name, func(t *testing.T) {
				c, _ := rulesClient(t, denyAll, st)
				err := CheckHostEnrolmentSecretAccess(ctx, c, withKey)
				require.ErrorIs(t, err, ErrHostEnrolmentRulesIncomplete)
				assert.Contains(t, err.Error(), "could not be listed completely")
			})
		}
	})

	t.Run("a rules review error refuses", func(t *testing.T) {
		boom := errors.New("connection refused")
		s := runtime.NewScheme()
		require.NoError(t, authorizationv1.AddToScheme(s))
		c := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				if _, ok := obj.(*authorizationv1.SelfSubjectRulesReview); ok {
					return boom
				}
				return nil
			},
		}).Build()
		require.ErrorIs(t, CheckHostEnrolmentSecretAccess(ctx, c, withKey), boom)
	})
}

func TestHostEnrolmentWorkloadAccess(t *testing.T) {
	ctx := context.Background()
	key := &types.NamespacedName{Namespace: "cert-manager", Name: "host-agent-ca"}
	cfg := &HostEnrolmentConfig{Namespace: "host-agent-pki", CAKeySecret: key}

	c, asked := ssarClient(t, denyAll)
	got, err := HostEnrolmentWorkloadAccess(ctx, c, cfg)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, []string{"cert-manager:create:apps/deployments", "cert-manager:update:apps/deployments",
		"cert-manager:patch:apps/deployments", "cert-manager:create:/pods"}, *asked)

	c, _ = ssarClient(t, allowOnly("cert-manager:create:apps/deployments"))
	got, err = HostEnrolmentWorkloadAccess(ctx, c, cfg)
	require.NoError(t, err)
	assert.Equal(t, []string{"create deployments.apps in namespace cert-manager"}, got)

	c, _ = ssarClient(t, allowOnly("cert-manager:patch:apps/deployments"))
	got, err = HostEnrolmentWorkloadAccess(ctx, c, cfg)
	require.NoError(t, err)
	assert.Equal(t, []string{"patch deployments.apps in namespace cert-manager"}, got)

	c, _ = ssarClient(t, allowOnly("cert-manager:create:/pods"))
	got, err = HostEnrolmentWorkloadAccess(ctx, c, cfg)
	require.NoError(t, err)
	assert.Equal(t, []string{"create pods in namespace cert-manager"}, got)

	c, asked = ssarClient(t, denyAll)
	got, err = HostEnrolmentWorkloadAccess(ctx, c, &HostEnrolmentConfig{Namespace: "host-agent-pki"})
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Empty(t, *asked, "no key Secret: nothing to check")

	boom := errors.New("connection refused")
	c, _ = ssarClient(t, func(authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
		return authorizationv1.SubjectAccessReviewStatus{}, boom
	})
	_, err = HostEnrolmentWorkloadAccess(ctx, c, cfg)
	require.ErrorIs(t, err, boom)
}

// TestAdmissionGate_SecretAccess: the gate re-runs the Secret access check
// after the admission check; while it fails, approvals are suspended.
func TestAdmissionGate_SecretAccess(t *testing.T) {
	ctx := context.Background()
	sch := newProviderTLSScheme(t)
	require.NoError(t, admissionregistrationv1.AddToScheme(sch))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "host-agent-pki"}}
	rules := []admissionregistrationv1.RuleWithOperations{{Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
		Rule: admissionregistrationv1.Rule{APIGroups: []string{"cert-manager.io"}, APIVersions: []string{"v1"}, Resources: []string{"*/*"}}}}
	mut := &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "m"},
		Webhooks: []admissionregistrationv1.MutatingWebhook{{Name: "m.cert-manager.io", Rules: rules}}}
	val := &admissionregistrationv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "v"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{Name: "v.cert-manager.io", Rules: rules}}}
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(ns, mut, val).Build()

	var secretErr error
	calls := 0
	g := &AdmissionGate{Reader: cli, Namespace: ns.Name, SecretAccess: func(context.Context) error { calls++; return secretErr }}
	require.NoError(t, g.Check(ctx))
	assert.True(t, g.OK())
	assert.Equal(t, 1, calls)

	secretErr = fmt.Errorf("%w: it may list secrets in all namespaces", ErrHostEnrolmentSecretAccess)
	require.ErrorIs(t, g.Check(ctx), ErrHostEnrolmentSecretAccess)
	assert.False(t, g.OK(), "a Secret grant added after startup suspends approvals")

	secretErr = nil
	require.NoError(t, g.Check(ctx))
	assert.True(t, g.OK(), "approvals resume once the grant is gone")

	// Only a recovery is logged as resumed, never the first check to pass.
	var logged []string
	logCtx := logr.NewContext(ctx, funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{}))
	fresh := &AdmissionGate{Reader: cli, Namespace: ns.Name, SecretAccess: func(context.Context) error { return secretErr }}
	require.NoError(t, fresh.Check(logCtx))
	assert.Empty(t, logged, "the first check to pass is no recovery")
	secretErr = ErrHostEnrolmentSecretAccess
	require.Error(t, fresh.Check(logCtx))
	secretErr = nil
	require.NoError(t, fresh.Check(logCtx))
	require.NotEmpty(t, logged)
	assert.Contains(t, logged[len(logged)-1], "approvals are resumed")
	assert.NotContains(t, logged[len(logged)-1], "sub-CA key", "no key Secret need be declared")

	// Start checks at once, before the first tick.
	started := &AdmissionGate{Reader: cli, Namespace: ns.Name, Interval: time.Hour}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- started.Start(runCtx) }()
	require.Eventually(t, started.OK, 10*time.Second, 10*time.Millisecond, "Start checks without waiting for the interval")
	cancel()
	require.NoError(t, <-done)

	// A failing admission check suspends approvals without asking the
	// Secret reviews.
	require.NoError(t, cli.Delete(ctx, val))
	calls = 0
	require.ErrorIs(t, g.Check(ctx), ErrNoCertManagerAdmission)
	assert.False(t, g.OK())
	assert.Zero(t, calls)
}

func TestParseHostEnrolmentConfig_CAKeySecret(t *testing.T) {
	ca := hostidentitytest.NewCA(t)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, ca.PEM, 0o600))
	base := HostEnrolmentFlags{
		Namespace: "host-agent-pki", Issuer: "ClusterIssuer/host-agent-ca", AdminGroup: "enrolment-admins",
		TrustDomain: "cluster.test", CABundleFile: caFile,
	}
	installID := bytes.Repeat([]byte{1}, 16)

	if !devgate.Compiled {
		// The flag is not registered in a release build; the field alone
		// still counts as an enrolment flag and is refused.
		_, err := ParseHostEnrolmentConfig(HostEnrolmentFlags{CAKeySecret: "cert-manager/host-agent-ca"}, installID)
		require.ErrorIs(t, err, devgate.ErrNotCompiled)
		return
	}

	_, err := ParseHostEnrolmentConfig(base, installID)
	require.Error(t, err, "required with a cert-manager issuer")
	assert.Contains(t, err.Error(), "--"+DevHostEnrolmentCAKeySecretFlagName+" is required")

	extOnly := base
	extOnly.Issuer, extOnly.ExternalPKIIssuer = "", "external-pki.example.test/ExternalIssuer/enterprise"
	cfg, err := ParseHostEnrolmentConfig(extOnly, installID)
	require.NoError(t, err, "with an external PKI alone, declaring a key kept in the cluster is the operator's responsibility")
	assert.Nil(t, cfg.CAKeySecret)
	extOnly.CAKeySecret = "pki/enterprise-key"
	cfg, err = ParseHostEnrolmentConfig(extOnly, installID)
	require.NoError(t, err)
	assert.Equal(t, &types.NamespacedName{Namespace: "pki", Name: "enterprise-key"}, cfg.CAKeySecret, "checked when declared")

	f := base
	f.CAKeySecret = "cert-manager/host-agent.ca-key"
	cfg, err = ParseHostEnrolmentConfig(f, installID)
	require.NoError(t, err)
	assert.Equal(t, &types.NamespacedName{Namespace: "cert-manager", Name: "host-agent.ca-key"}, cfg.CAKeySecret)

	_, err = ParseHostEnrolmentConfig(HostEnrolmentFlags{CAKeySecret: "cert-manager/host-agent-ca"}, installID)
	require.Error(t, err, "the flag alone does not enable enrolment")
	assert.Contains(t, err.Error(), "--"+DevHostEnrolmentNamespaceFlagName)

	for value, want := range map[string]string{
		"host-agent-ca":                 "must be <namespace>/<name>",
		"cert-manager/a/b":              "must be <namespace>/<name>",
		"/host-agent-ca":                `namespace ""`,
		"cert-manager/":                 `the Secret name ""`,
		"Cert_Manager/host-agent-ca":    `namespace "Cert_Manager"`,
		"cert-manager/Host_Agent_CA":    `the Secret name "Host_Agent_CA"`,
		"host-agent-pki/host-agent-ca":  "must not be in the enrolment namespace host-agent-pki",
		"cert-manager.io/host-agent-ca": `namespace "cert-manager.io"`,
	} {
		t.Run(value, func(t *testing.T) {
			f := base
			f.CAKeySecret = value
			_, err := ParseHostEnrolmentConfig(f, installID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--"+DevHostEnrolmentCAKeySecretFlagName)
			assert.Contains(t, err.Error(), want)
		})
	}
}
